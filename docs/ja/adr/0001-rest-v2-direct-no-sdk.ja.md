# ADR-0001: BigQuery REST API v2 を直接呼ぶ。BigQuery SDK は使わない

| 項目 | 内容 |
|-------|-------|
| ステータス | **採択** |
| 日付 | 2026-09-06 |
| 拘束対象 | bigquery-mcp |
| 決定者 | nlink-jp maintainers |
| 契機 | RFP §2「外部依存」と §3.1。利用者の当初の言い方は「BigQuery SDK で自前実装」だった。設計にする前に依存グラフを測る必要があった |

## 背景

サーバーが必要とする REST 呼び出しは 7 つ（dry run 用の `jobs.insert`、`jobs.query`、`jobs.getQueryResults`、`datasets.list`、`datasets.get`、`tables.list`、`tables.get`。Phase 2 で `jobs.get` と `jobs.cancel`）と、資格情報の供給源が 1 つ（Application Default Credentials）である。

2026-09-05、Go 1.27 の空モジュールで計測:

| import | build list のモジュール数 | リンクされるパッケージ数 |
|---|---|---|
| `cloud.google.com/go/bigquery` | 236 | 330 |
| `golang.org/x/oauth2/google` のみ | 3 | 27 |

SDK は `google.golang.org/grpc`、`google.golang.org/protobuf`、`google.golang.org/api`、`cloud.google.com/go` のコア一式を、7 つのエンドポイントに HTTPS で JSON を送るだけのサーバーに引き込む。組織のサプライチェーン方針は標準ライブラリ・一次 SDK・REST 直叩きを許し、その中で要件を満たす最小のものを選ぶ。

REST 面は安定しており、Discovery ドキュメント（`https://bigquery.googleapis.com/$discovery/rest?version=v2`）に完全に記述されている。本サーバーが必要とする少数のリクエスト/レスポンス構造体はそこから手書きする。組織は Splunk で同じことを既に行った（splunk-mcp の REST クライアントは splunk-cli からの移植）。

## 決定

1. `internal/bq` は `net/http` で `https://bigquery.googleapis.com/bigquery/v2` を叩く手書きクライアントとする。本サーバーが読み書きするフィールドだけを持ち、未知のレスポンスフィールドは無視する。
2. 認証は `golang.org/x/oauth2/google.DefaultTokenSource`、スコープは `https://www.googleapis.com/auth/bigquery`。利用者 ADC ではスコープは `gcloud auth application-default login` 時に固定（cloud-platform）され要求は無視される。サービスアカウント ADC ではトークンが狭まる。ADC ファイルを本コードが列挙・解析することはない。ライブラリが ADC の定める 1 パスを読むだけである。
3. `cloud.google.com/go/bigquery` と `google.golang.org/api` は依存にせず、推移的依存にもさせない。`go.mod` がそのテストである。

## 帰結

- 直接のサードパーティモジュールは 3 つ（cobra、toml、oauth2）、間接が 3 つ（pflag、mousetrap、compute/metadata）。フリートの他の MCP サーバー + 資格情報ライブラリ。
- 新しい REST フィールド（新しい統計ブロック、新しいジョブオプション）は SDK の更新ではなくここの構造体編集になる。Discovery ドキュメントが参照先で、使用フィールドは `docs/ja/architecture.ja.md` に列挙する。
- 再試行・ページング・エラー写像は本サーバー自身のコード（ADR-0004）になる。RFP がそれらを置きたい場所はまさにそこである — それが製品だから。
- ADC の読取はプロセス内で起きる。EDR から見える足跡は SDK が行うのと同じファイル open であり、別の足跡を望む操作者には Phase 2 で `[auth] token_command` を提供する。

## 検討した代替案

- **BigQuery Go SDK。** 上の計測により棄却: 7 呼び出しに 236 モジュール。しかも SDK が持ち込む再試行とページングの挙動は、本サーバーが自分で制御しなければならないものそのもの。
- **`google.golang.org/api/bigquery/v2`**（Discovery 生成クライアント）。cloud SDK より小さいが `google.golang.org/api` とそのトランスポート一式を持ち込む。生成される型は、必要な一部分を手書きするのと同じ形である。
- **`gcloud auth print-access-token` へのトークン委譲**を唯一の資格情報経路にする案。既定としては棄却: 更新のたびに Python プロセスを起動し、ネットワークの無いサンドボックス内では失敗する（gem-agent の read レーンで観測）。Phase 2 の `token_command` オプションとして残す。

## 参照

- RFP §2 外部依存、§3.1、§5
- BigQuery Discovery ドキュメント v2（使用フィールドは `docs/ja/architecture.ja.md`）
- splunk-mcp `internal/client`（フリートの REST 直叩きの先例）
