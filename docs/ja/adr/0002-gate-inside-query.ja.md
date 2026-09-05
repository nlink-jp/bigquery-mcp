# ADR-0002: ゲートは `query` の内側にある。BigQuery の statementType と IAM が判定する

| 項目 | 内容 |
|-------|-------|
| ステータス | **採択** |
| 日付 | 2026-09-06 |
| 拘束対象 | bigquery-mcp |
| 決定者 | nlink-jp maintainers |
| 契機 | RFP §1（保護重視）と §3.2–3.5。公式リモート MCP の `execute_sql_readonly` は文字列検査で「SELECT に制限」し、予算は提供しない |

## 背景

v1 要件として保護三点が固定された: 予算ゲート、読取専用実行、エラー契約（ADR-0004）。本記録は前二者がどこに置かれ、何が判定するかを決める。

Discovery ドキュメントの二つの事実が設計を形作る:

- `jobs.query` は `dryRun`・`maximumBytesBilled`・`jobTimeoutMs`・`labels` を受け付け、応答に `statementType` と `totalBytesProcessed` を持つ。**`referencedTables` は持たない。**
- `jobs.insert` に `configuration.dryRun = true` を付けると Job が即座に返り、`statistics.query` に `statementType`・`totalBytesProcessed`・`referencedTables`・`schema` が乗る。

`maximumBytesBilled` は、課金バイト数がそれを超えるときジョブを「課金なしで」失敗させる — オンデマンド課金のもとで。Editions では課金バイト数は 0 で上限は無効化される。

SQL への正規表現による文分類は無限領域である（コメント、文字列リテラル、スクリプト、`EXECUTE IMMEDIATE`、先頭が `SELECT` の複文スクリプト）。BigQuery は文を自ら分類し `statementType` として報告する。複文スクリプトは `SCRIPT` と報告される。

検査を飛ばせるモデルはいずれ飛ばす — 任意の `dry_run` ツールは契機の無い能力である（組織に記録された教訓）。したがってゲートは「先に呼ぶよう頼む別ツール」であってはならない。

## 決定

1. **`query` は常に先に dry run する。** dry run → ゲート → 実行の順序はツールの内側にあり、それを飛ばす引数は無い。`dry_run` はモデル自身のプレビュー用の別ツールとして存在し、同じコードを走らせる。
2. **ゲートの検査は 3 つ、この順で、それぞれ固有のエラーコードを持つ:**
   - dry run の `statementType` が厳密に `SELECT` でなければ `statement_not_allowed`。`SCRIPT`、`EXPORT_DATA`、全 DML/DDL 型、`ASSERT`、`CALL` はこの一つの比較で全て拒否される。SQL 文字列は検査しない。
   - `[access] datasets` が設定され、参照テーブルの `project.dataset` がどの項目（`project.dataset` 完全一致または `project.*`）にも一致しなければ `dataset_not_allowed`。この検査には `referencedTables` が要るので、**許可リストが設定されているときの dry run は `jobs.insert` を通る**。許可リストが無ければ `jobs.query` を通り、往復が 1 つ少ない。
   - `totalBytesProcessed` が `[budget] max_bytes_billed` を超えれば `budget_exceeded`。何も実行されておらず、エラーは推定値と予算を運ぶ。
3. **実行はカーネル側上限を伴う。** `maximumBytesBilled` に同じ `max_bytes_billed`、`jobTimeoutMs` に `[budget] job_timeout` を設定し、ラベル `bigquery-mcp: true` を付ける。真の課金バイト数が推定を超えるクエリは本サーバーではなく BigQuery が止める。
4. **IAM が恒久的な境界。** README の準備手順は billing プロジェクトに `roles/bigquery.jobUser`、データに `roles/bigquery.dataViewer` を付与し、書けるものは何も付けない。サーバーの文検査は二重目であって唯一ではなく、README にそう書く。
5. **コストの形には拒否でなく警告。** `describe_table` や dry run で参照テーブルがパーティション化されているのに SQL がパーティション列のフィルタに触れていなければ、`dry_run` と `query` は警告文を添える。これは助言であり、規則は予算である。

## 帰結

- 1 インスタンス 1 billing プロジェクト（RFP §3.12）が、予算をモデルの選択でなく設定値にしている。
- Editions 課金のもとではゲートは dry run の推定値のみ。README に明記し、`dry_run` はいずれにせよ `bytes_processed` を報告する。
- 正当な非 SELECT の読取 — `ASSERT`、読取専用プロシージャへの `CALL`、`DECLARE` を含むスクリプト — は v1 で拒否される。意図して広げる場所は Phase 2 の保護付き書込モードである。
- 2 つの dry run 経路（`jobs.query` と `jobs.insert`）は偽サーバーのテストで走らせ、live テストは実プロジェクトで両方を走らせる。

## 検討した代替案

- **SQL への正規表現やパーサ**で書込を検出する。棄却: 無限領域であり、BigQuery が自身の分類を既に公開している。
- **IAM だけ**でサーバー検査なし。棄却: 操作者はサーバーが行使すべきより広いロールを持ちうる（通常持っている）し、安定したコードつきの拒否こそがモデルを探索でなく停止へ向かわせる。
- **プロンプトが推奨する任意の第一歩としての `dry_run`。** 棄却: 契機の無い能力は発火しない。予算はモデルの記憶に依存してはならない。
- **dry run を常に `jobs.insert` で。** 許可リスト無しの場合について棄却: 読まないフィールドのためにクエリごとに 1 リクエスト増える。

## 参照

- RFP §3.2–3.5、§7（`maximumBytesBilled` はオンデマンドのみ; `referencedTables` は `jobs.query` に無い）
- BigQuery Discovery v2: `QueryRequest`、`QueryResponse`、`JobStatistics2.statementType`
- 組織ナレッジ: 契機の無い能力は発火しない; 文字列規則より有限領域
