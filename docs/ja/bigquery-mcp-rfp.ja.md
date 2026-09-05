# RFP: bigquery-mcp

> Generated: 2026-09-06
> Status: Approved (2026-09-06)

## 1. Problem Statement

エージェント（Claude Code / gem-agent / Claude Desktop）が企業内の BigQuery DWH をエージェンティックに分析するとき、公式リモート MCP サーバー（`bigquery.googleapis.com/mcp`）には三つの不足がある。

1. **保護が弱い。** dry run による予算プレビューも bytes-billed 上限も無く、読取専用の強制は SQL 文字列の検査に頼る。
2. **エラーが行動に結びつかない。** 2026-09-05 のセッション 4d6bb685 では、引数が正しいのに「Required parameter is missing: query」という文字列が一過性で返り、原因も再試行可否も示されないままエージェントがランタイムの調査に脱線した。
3. **stdio クライアントからは mcp-bridge を挟む。** トークン更新とホップが増え、障害点が一つ増える。

bigquery-mcp は、利用者自身の ADC（Application Default Credentials）でローカルに stdio 動作し、予算ゲートと読取専用の判定を実行経路の内側に固定し、BigQuery API のエラー reason を `{code, message, retryable, details}` に写像して返す、**保護重視の BigQuery 専用 MCP サーバー**である。SQL の生成は呼び出し側エージェントに任せ、LLM は呼ばない。

**対象ユーザー**: gcloud と BigQuery の IAM を既に持つ人。自分自身（nlink-jp の DWH）と、組織外の企業内アナリスト。準備は `gcloud auth application-default login`、既存の IAM ロール、config への billing プロジェクト ID 記入の三つで、per-user の OAuth クライアント発行や API 有効化は要らない。gcloud を持たない利用者は対象外とする。

## 2. Functional Specification

### Commands / API Surface

単一バイナリ + サブコマンド。

| サブコマンド | 動作 |
|---|---|
| `serve`（既定） | MCP サーバーを stdio で起動 |
| `doctor` | ADC の有無 → billing プロジェクトへの到達 → `SELECT 1` の dry run による IAM 確認、の順に検査し、欠けている段を名指しする |
| `version` | 版数表示 |

MCP ツール（v1、6 本）:

| ツール | 引数 | 動作 |
|---|---|---|
| `list_datasets` | `project?` | データセット一覧。`project` 省略時は config の billing プロジェクト |
| `list_tables` | `dataset`, `project?` | テーブル・ビュー一覧（種別付き） |
| `describe_table` | `dataset`, `table`, `project?` | スキーマ（ネスト含む）、パーティション列・種別・粒度、`require_filter`、クラスタ列、行数、サイズ、パーティション期限、作成/更新/期限時刻、location、ビューなら SQL |
| `dry_run` | `query`, `params?` | バイト数（生と課金推定）、精度、statementType、参照テーブルとルーチン、未宣言パラメータ、結果スキーマ、location、ゲート判定（`allowed`、`denied_by`）、バイト比較による警告（パーティションテーブルの全走査、不正確な推定） |
| `query` | `query`, `params?`, `max_rows?` | **必ず内部で dry run** → ゲート（statementType が `SELECT`、許可リスト内、予算内）→ `jobs.query` 実行 → ページング → キャップまで返却 |
| `get_usage` | なし | リファレンスとエラー回復表（nlink-jp MCP 標準） |

メタデータ系 3 ツールの `project` は探索専用で、課金は発生しない。境界は IAM。

Phase 2 で追加するツール:

| ツール | 動作 |
|---|---|
| `start_query` / `check_job` / `get_results` / `cancel_job` | `jobs.insert` → `jobs.get` ポーリング → `getQueryResults` の非同期系。3 分を超える長いクエリ用 |

### Input / Output

**`query` の結果**（JSON）:

```json
{
  "rows": [ {"email": "...", "cnt": 12} ],
  "schema": [ {"name": "email", "type": "STRING", "mode": "NULLABLE"} ],
  "total_rows": 4813,
  "returned_rows": 1000,
  "truncated": true,
  "truncated_by": "max_rows",
  "bytes_processed": 123456789,
  "bytes_billed": 130023424,
  "cache_hit": false,
  "job_id": "...",
  "location": "US",
  "slot_ms": 1234
}
```

- 行は **列名をキーにしたオブジェクト**。公式サーバーの `{"f":[{"v":…}]}` 形式は採らない。ネストは JSON の入れ子、`REPEATED` は配列、`BYTES` は base64、`TIMESTAMP` は RFC 3339、`NUMERIC` 系は文字列。
- 全件をレスポンスとして返す。サーバー側で workspace にファイルを落とす方式は持たない（ファイル化はエージェントランタイムの責務。gem-agent は ADR-0058 の intake が work dir へ退避する）。
- キャップは二つ。`max_rows`（呼び出し引数。config に既定と上限）と `[results] max_bytes`（レスポンスのバイト予算）。どちらかに当たったら行を切り、`truncated: true`、`truncated_by`、`total_rows` を必ず計上する。黙って落とす経路は無い。

**`dry_run` の結果**:

```json
{
  "bytes_processed": 123456789,
  "statement_type": "SELECT",
  "referenced_tables": ["proj.dataset.table"],
  "schema": [ ... ],
  "within_budget": true,
  "budget_bytes": 10737418240,
  "warnings": ["table proj.dataset.events is partitioned by event_date but the query has no filter on it"]
}
```

**エラー契約**（`isError: true` の text content に JSON）:

```json
{"code": "budget_exceeded", "message": "...", "retryable": false,
 "details": {"reason": "...", "location": "...", "job_id": "...", "bytes_processed": 0, "budget_bytes": 0}}
```

| code | 由来 | retryable |
|---|---|---|
| `invalid_query` | BigQuery `invalidQuery`（構文・存在しない列・SQL 長 1 MB 超など。`details.location` に位置） | no |
| `statement_not_allowed` | dry run の statementType が `SELECT` 以外 | no |
| `dataset_not_allowed` | 参照テーブルが `[access] datasets` の外 | no |
| `budget_exceeded` | dry run のバイト数が `[budget] max_bytes_billed` 超（実行前に止める） | no |
| `access_denied` | BigQuery `accessDenied`（IAM 不足。必要ロールを message に書く） | no |
| `not_found` | `notFound`（プロジェクト・データセット・テーブル） | no |
| `rate_limited` | `rateLimitExceeded` / `quotaExceeded` のうち一時的なもの | yes |
| `backend_error` | `backendError` / `internalError` | yes |
| `timeout` | `jobTimeoutMs` 超過、または `getQueryResults` の待機超過 | no |
| `auth_error` | ADC が無い・期限切れ・更新失敗（`gcloud auth application-default login` を案内） | no |
| `invalid_arguments` | 引数の型・欠落・`max_rows` 上限超 | no |

`retryable: true` のエラーはサーバー内でジッタ付きに 1 回だけ再試行し、それでも失敗したときにモデルへ返す。上の表は RFP 時点のスケッチであり、**拘束力を持つ表は ADR-0004 §2**。コード・`get_usage`・テストが共有する（2026-09-06 の設計レビューで `cancelled`、`duplicate`、`upstream_error`、カーネル上限の reason を追加）。

### Configuration

sectioned TOML。既定パス `~/.config/bigquery-mcp/config.toml`。解決順は `--config` → `$BIGQUERY_MCP_CONFIG` → 既定パス。

```toml
[project]
id = "billing-project"          # ジョブが走り課金されるプロジェクト（必須）
# location = "US"               # 省略時は BigQuery がデータセットから決める

[budget]
max_bytes_billed = "10GiB"       # dry run ゲートの閾値。実行時の maximumBytesBilled にもそのまま渡す
job_timeout      = "3m"          # jobTimeoutMs

[access]
# datasets = ["proj.dataset", "proj2.*"]   # 指定時のみ jobs.insert(dryRun) の referencedTables で強制

[results]
default_max_rows = 1000          # 呼び出しが max_rows を省略したときの値
hard_max_rows    = 50000         # 呼び出しが指定できる上限
max_bytes        = "1MiB"        # レスポンス全体のバイト予算

[logging]
# log_file    = ""               # 省略時は stderr
# log_level   = "info"
# log_queries = false            # SQL 本文を残すオプトイン。PII 混入に注意
```

**1 インスタンス = 1 billing プロジェクト。** 複数の billing プロジェクトを使うときは config を分け、クライアント側に別名で登録する（`bigquery-prod` / `bigquery-sandbox`）。config に秘密情報は無い（資格情報は ADC）。プロジェクト ID は環境固有値なのでリポジトリには書かない。

### External Dependencies

- **BigQuery REST API v2**: `jobs.insert`（dryRun）、`jobs.query`、`jobs.getQueryResults`、`datasets.list`、`datasets.get`、`tables.list`、`tables.get`。Phase 2 で `jobs.get`、`jobs.cancel`。
- **認証**: ADC。`golang.org/x/oauth2/google`（build list 3 モジュール・リンク 27 パッケージ）。トークンは自動更新。
- **不採用**: `cloud.google.com/go/bigquery`（build list 236 モジュール・リンク 330 パッケージ、gRPC と protobuf を引き込む）。必要な構造体は Discovery ドキュメントから手書きする。
- TOML パーサは data-toolbox-mcp と同じものを scaffold 時に固定する。

## 3. Design Decisions

1. **Go、REST v2 直叩き、SDK 不採用。** 依存グラフの実測（236 対 3 モジュール）が根拠。組織のサプライチェーン方針（stdlib / 一次 SDK / REST 直叩き）の範囲内で最小を選ぶ。骨格は data-toolbox-mcp の transport / jsonrpc / mcpserver / toolerr / logging を移植し、REST とジョブ層の先例は splunk-mcp。
2. **保護は `query` の内側に固定する。** dry run → ゲート → 実行の順は省略できない。モデルが `dry_run` を呼ばなくても予算判定は必ず走る。
3. **文の分類は BigQuery 自身の statementType で行う。** 正規表現で SQL を分類しない。`SCRIPT`、`EXPORT_DATA`、DML、DDL はすべて `SELECT` 以外として落ちる。
4. **恒久的な境界は IAM。** `jobUser` + `dataViewer` だけを持つ利用者は、サーバーの判定を抜けても書けない。サーバーの判定は二重目の防御であり、唯一の防御ではない。
5. **カーネル側上限は `maximumBytesBilled` と `jobTimeoutMs`。** dry run の予算判定は UX。`maximumBytesBilled` はオンデマンド課金でのみ効く（Editions では bytes billed が 0）ことを README に明記する。
6. **ジョブにラベル `bigquery-mcp` を付ける。** 課金側で切り分けられるようにする。
7. **再試行は retryable に限り 1 回。** それ以上はモデルに `retryable: true` を返して委ねる。透過的な再試行の繰り返しはサーバーの状態を隠す。
8. **サーバー側スピルは持たない。** 全件をレスポンスで返し、ファイル化はエージェントランタイムが自動で扱う。サーバーに残すのは明示キャップと省略の計上だけ。
9. **エラー契約 `{code, message, retryable, details}`。** details に BigQuery の reason、location、job_id、dry run のバイト数と予算値を入れ、モデルが「何を直せば通るか」を読めるようにする。
10. **SQL 本文のログはオプトイン**（`log_queries = true`）。ログレベルとは独立。既定ではジョブ ID・statementType・バイト数・所要時間・エラー code のみ。
11. **ADC のスコープには触れない。** ADC は機体上の全ツールが共有する 1 ファイルで、狭めると gem-agent 等の Vertex 利用が壊れる。権限縮小は IAM とサーバー内判定で行い、README には「ADC は共有物なので狭めない」と注記する。サービスアカウント経路だけはサーバーが `https://www.googleapis.com/auth/bigquery` を要求する。
12. **1 インスタンス = 1 billing プロジェクト。** プロファイル機構や呼び出しごとの billing 切替は採らない。モデルが billing 先と予算を選べる構造は予算ゲートを弱める。データの横断は BigQuery が元々許すので、メタデータ系ツールの `project` 引数と IAM で足りる。
13. **補完関係。** gem-agent とは ADR-0058 の intake が受け口。大きな結果は work dir に退避され、data-toolbox-mcp の `load_from_work` で二次分析へ渡せる。mcp-tactics スキルに BigQuery の導線を追加する（追随義務）。gem-query（NL→SQL）とは別物。
14. **対象外。** NL→SQL、書込全般（DML / DDL / EXPORT。Phase 2 で一時データセット限定の保護付き書込を再検討）、公式リモート MCP の前段プロキシ化、HTTP/SSE トランスポート、複数 DB 対応、Legacy SQL、セッション、BI Engine、予約スロット操作、コミュニティ製 BigQuery MCP との比較。

## 4. Development Plan

### Phase 0: 設計文書化（scaffold 直後、コードより先）

- ADR-0001: REST v2 直叩き + oauth2/google、SDK 不採用（計測値付き）
- ADR-0002: 予算ゲートと文分類は `query` の内側、statementType と IAM で判定
- ADR-0003: サーバー側スピルを持たない。明示キャップと省略計上
- ADR-0004: エラー契約と 1 回再試行
- architecture.md
- 設計確定の独立検証パス（サブエージェント）

### Phase 1: Core + tests

- `_wip/bigquery-mcp/` に Go scaffold（CONVENTIONS のテンプレート、`make build` → `dist/`）。サブコマンド `serve` / `doctor` / `version`
- data-toolbox-mcp から transport / jsonrpc / mcpserver / toolerr / logging を移植
- `internal/bq`: ADC、REST クライアント、エラー写像、`location` の引き回し
- `internal/tools`: 6 ツール。`query` の内側に dry run → ゲート → 実行 → ページング → キャップ
- `doctor`
- テスト: `httptest` の偽 BigQuery でゲート・キャップ・エラー写像・再試行・location を固定。live E2E は opt-in（`-tags live`）で GWS 監査エクスポートのデータセットを使い、プロジェクト ID は環境変数から取る

レビュー単位: 偽サーバーのテスト一式と live E2E の結果で独立に評価できる。

### Phase 2: Features

- 非同期系 `start_query` / `check_job` / `get_results` / `cancel_job`
- `[auth] token_command`（EDR 可視面を変えたい環境向け。mcp-bridge の tokenCommand と同型）
- 一時データセット限定の保護付き書込（要再検討）
- フルスキャン注意の精緻化（パーティション列 × WHERE の有無）

### Phase 3: Release

- README en/ja、AGENTS.md、CHANGELOG、`docs/{en,ja}/reference/client-setup.md`（Claude Desktop / Claude Code / gem-agent）
- 署名・notarize・5 プラットフォーム zip、tap formula、util-series submodule、org profile
- mcp-tactics スキルに BigQuery の導線を追加
- knowledge への還元、check-org 全緑
- リリース前の独立検証パス

## 5. Required API Scopes / Permissions

**OAuth スコープ**（Discovery ドキュメントより）:

| メソッド | 受け付けるスコープ |
|---|---|
| `jobs.query` / `jobs.get` / `jobs.getQueryResults` / `datasets.*` / `tables.*` | `bigquery`, `cloud-platform`, `cloud-platform.read-only` |
| `jobs.insert` | `bigquery`, `cloud-platform` |
| `jobs.cancel` | `bigquery`, `cloud-platform` |

`bigquery.readonly` はどのメソッドにも載っていない。`cloud-platform.read-only` は `jobs.query` は実行できるが、dry run と実行の両方が使う `jobs.insert` は実行できないため、読取専用スコープの資格情報では本サーバー経由のクエリはできない。利用者 ADC の既定 `cloud-platform` で全経路が通る。サービスアカウント経路では `https://www.googleapis.com/auth/bigquery` を要求する。

**IAM ロール**:

| 対象 | ロール | 理由 |
|---|---|---|
| billing プロジェクト | `roles/bigquery.jobUser` | `bigquery.jobs.create`。自分のジョブの取得と取消はこれで足りる |
| データプロジェクト / データセット | `roles/bigquery.dataViewer` | `datasets.get`、`tables.list`、`tables.get`、`tables.getData` |
| 探索のみ許すプロジェクト | `roles/bigquery.metadataViewer` | 一覧とスキーマのみ |

**不要**: `dataEditor` 以上（付けないことが読取専用の恒久境界）、`serviceusage.services.use`、`x-goog-user-project` ヘッダ。API は billing プロジェクトで BigQuery API が有効なら十分。

## 6. Series Placement

Series: **util-series**
Reason: splunk-mcp、data-toolbox-mcp、pcap-analyzer-mcp と同じ「解析基盤 MCP」の列。利用者認証の対話 CLI（cli-series）でも Slack 自動化（chatops-series）でもない。

## 7. External Platform Constraints

| 制約 | 値 | 設計への反映 |
|---|---|---|
| 結果ページのサイズ | 1 ページ 10〜20 MB（quotas ページは 20 MB、`maxResults` リファレンスは 10 MB） | `maxResults` でページングし、`max_rows` / `max_bytes`（どちらより遥かに小さい）に達するまで `getQueryResults` を回す |
| 未解決 SQL の長さ | 1 MB | 超過は `invalid_query` の details に載せる |
| オンデマンド日次クエリ量 | 既定 200 TiB/プロジェクト | 予算ゲートの上位にあるプロジェクト側カスタム quota を README で案内 |
| 待機中の対話クエリ | 1,000/プロジェクト | 超過は `rate_limited`（retryable） |
| `jobs.get` | 1,000 req/s/プロジェクト | Phase 2 のポーリングは指数バックオフ |
| `maximumBytesBilled` | オンデマンド課金のみ有効 | Editions 環境では予算ゲートは dry run の推定値に依存すると明記 |
| 最小課金単位 | テーブルあたり 10 MB | `bytes_billed` が 10 MB 刻みで返る旨を注記 |
| ジョブの location | データセットの所在で決まる | `jobReference.location` を保持し `getQueryResults` に必ず渡す。リージョン跨ぎの参照はエラー |
| `referencedTables` | `jobs.query` の応答には無い | 許可リスト指定時のみ `jobs.insert(dryRun)` を使う |
| ADC のトークン | 1 時間 | oauth2 が自動更新 |
| MCP クライアントのツール結果上限 | Claude Desktop / Claude Code で数百 KB 級の応答が拒否された実績 | `max_rows` に加えて `max_bytes` のバイト予算を持つ |

Phase 0 で再確認して ADR に固定する値: 同時実行の対話クエリ数（100/プロジェクト）、クエリ実行時間上限（6 時間）、ユーザー毎の API レート（100 req/s、同時 300）。

---

## Discussion Log

- **問題定義**: 利用者が ADC 採用と保護三点（予算ゲート・読取専用・エラー契約）の v1 投入を承認。非同期ジョブは Phase 2 へ。マルチプロジェクトの扱いを検討事項として提起。
- **公式サーバーの位置づけ**: 「ベータだから」という当初の仮説は release notes（2025-12-10 Preview、2026-03-17 から標準提供）で弱いと判明。自前化の価値は、障害の起きた MCP フロントエンド層と mcp-bridge のホップが消えること、エラー reason を契約に写像できること、に置き直した。
- **マルチプロジェクト**: BigQuery は billing プロジェクトとデータプロジェクトが分かれ、横断は IAM で元々可能。案 A（1 インスタンス = 1 billing プロジェクト）、案 B（プロファイル + `profile` 引数）、案 C（呼び出しごと `project_id`）を比較し、モデルに billing 先と予算を選ばせない案 A を採用。メタデータ系ツールの `project` 任意引数は承認。
- **結果の扱い**: 当初 splunk-mcp と同じ workspace スピル方式を提案したが、利用者から「その方式は廃止の方向。すべてレスポンスとして流し、ファイル化はランタイムが自動で扱う」と方針提示。サーバー側は明示キャップ（`max_rows`）と省略計上のみに変更。knowledge の mcp-server-design.md に根拠が記録済み。
- **設計判断**: SDK 不採用（236 対 3 モジュールの実測）、書込は v1 完全対象外、SQL ログはオプトイン（利用者指示。ログレベル連動ではなく独立スイッチ）。
- **開発計画**: 6 ツール全部を Phase 1 に、live E2E は GWS 監査エクスポートのデータセット、Phase 0 で ADR 4 本を先に書く、を承認。
- **スコープ**: `--scopes` で ADC を狭める強化案は利用者が却下。ADC は機体共有で gem-agent の Vertex 利用が壊れるため。README には逆の注記を入れる。
- **シリーズと制約**: util-series、`max_bytes` のバイト予算追加を承認。
