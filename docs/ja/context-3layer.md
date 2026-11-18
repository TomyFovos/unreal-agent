# 文脈の3層

Provider 非依存の Host API で生記録を読む L3 と、現在の状態を扱う L2 を提供します。3層設計の進行は [Issue #41](https://github.com/TomyFovos/unreal-agent/issues/41) にあります。

## 正本を読む

`record show LOCATOR` は locator が指す記録を読みます。`record find TEXT` は Session と配下の子の確定記録を検索します。アーカイブ済みの子も対象です。ネイティブツール名は `RecordShow` と `RecordFind` です。

- `rec:<session>/<sequence>` は改行を含む JSONL の Item をそのまま返します。接尾辞でユーザー・Peer・統括の原文、ツール呼び出し、子の Finish、保存した成果物を指定できます。子の Finish には handle が付き、`summary`、`tests`、`changed-files`、`blockers`、記録されている `next` を指定できます。
- `rec:<session>/output/<operation>/<stream>` は終了した Operation の出力を指します。シェルの標準出力・標準エラーは既存の capture ファイルから読みます。Remote Job は表示用の値と原文が異なる場合に元のバイト列を保持します。原文のない旧記録は取得不能を明示します。独立した Operation チェックポイントにも JSONL 上の locator があります。
- `Session.RecordArtifact` は明示的に指定されたファイルの版を保存します。locator はパスと SHA-256 を持ちます。保存された版はワークスペースで上書き・削除した後も読めます。
- `?bytes=<開始>-<終了>` は 0 始まりの半開区間です。`?line=<行番号>` は 1 始まりで改行も含みます。呼び出し元がページのバイト数を指定し、明示された継続 locator をたどります。0 は選択範囲の全体を返します。
- 検索は記録の種類、handle、時刻範囲（開始を含み、終了を除く）、大文字・小文字を区別する部分文字列で絞れます。抜粋は一致箇所、先頭行、または構造化された記録の字句を原文のまま返します。時刻で絞るには記録時刻が必要です。

`record show` は選んだバイト列をそのまま返します。`--json` または `--page-bytes` を指定すると、テキストか base64 と、続きがある場合の locator を JSON で返します。`record find` は一致した記録を JSON で返し、`--kind`、`--handle`、`--from`、`--before` を使えます。時刻は RFC3339 形式です。

ファイルを参照する際は、その時点の Session の読取り Permission に従います。Session 履歴には既存の正本・アーカイブの読取り経路を使います。入力の受領記録と観測値は出典の locator を持ちます。ターン途中で渡した入力の locator は継続記録に残り、到達確認は既存の input watermark と応答で行います。

## 現在の状態を再生する

L2 の13項目は `session`、`requests`、`rules`、`plan`、`decisions`、`children`、`approvals`、`waiting`、`questions_to_user`、`artifacts`、`issues`、`recent_events`、`metrics` です。各項目は出典の locator を持ちます。進行中の本人の依頼は原文全文を保持し、混載入力は alias と出典を保持します。終了した子は受入待ちの間、現在の状態に載ります。処理済みの項目は件数と locator に集約し、子の件数は template・model・終わり方で集計します。成果物は現在の子と、最後の意味上の区切り以降に統括が書いたパスを列挙します。

`Session.AppendContextEvent` は明示的な `context_state_event` を会話と同じ JSONL に追記します。`Session.ContextState` は独立したスナップショットを返します。reducer は同じイベント ID を一度だけ適用し、追記を再試行すると既存の locator が返ります。復元チェックポイントは状態と適用済み ID を保存し、正本の全再生とチェックポイント以降の再生は同じ状態のバイト列を生成します。fork では元のイベントを証跡として引き継ぎ、現在の状態は新しい Session のイベントから作ります。

書き込み時と再生時に、同じ入力・応答・承認記録・Operation チェックポイントから機械イベントを作ります。子の開始には記録済みの brief を含めます。進捗と観測した振り分け規則の ID は直近の出来事に入ります。終了した子に Finish があれば、その locator が付きます。旧記録に Finish が埋め込まれている場合は、チェックポイントの locator から読めます。承認の依頼と結果、Operation とモデルの待ち、子から転送された質問も現在の状態を更新します。ユーザー入力には原文の locator が付きます。行頭の `【要確認】` はユーザーへの質問を開きます。次の入力との対応付けは統括が明示します。

成果物のパスは Finish の changed files、成功した書き込みの受領記録、明示的なファイル保存から取り込み、出典を付けます。内容は保存した版または通常のファイル読取りツールで取得できます。

未対応のイベント版・種類は正本に保持し、reducer は認識できる更新を適用します。モデルは `state show` または `StateShow` を呼んで L2 を取得します。自動投影は PR5 で扱います。

## 統括の判断を記録する

Responses 系はネイティブツールを使います。Claude Code の統括は、Session のシェルから `unreal-agent subagent` と同じ Host の実行ファイルとソケットを使います。Read Only でも使えます。証拠の読取りには Session の通常の Permission が適用されます。

| `unreal-agent` に続ける CLI | ネイティブツール | 操作 |
| --- | --- | --- |
| `state show` | `StateShow` | 未回答の質問 ID と locator を含む現在の状態を読む。 |
| `state decide TEXT [--why TEXT]` | `StateDecide` | 決定、任意の根拠、退けた案、関連 ID を記録する。 |
| `state plan ID --text TEXT [--status STATUS]` | `StatePlan` | 手順を追加・更新する。順序、担当、依存先、決定 ID を指定できる。 |
| `state rule add EXCERPT --source LOCATOR` | `StateRuleAdd` | 指定した本人発言から原文の引用を登録する。 |
| `state accept HANDLE` または `state reject HANDLE [--why TEXT]` | `StateAccept` または `StateReject` | 自分の子の受入・差し戻しを記録する。理由は任意。 |
| `state question TEXT [--id ID] [--source LOCATOR]` | `StateQuestion` | 質問を手記録する。ユーザーには本文で質問する。既存の統括発言も出典に指定できる。 |
| `state answer ID --source LOCATOR` | `StateAnswer` | 未回答の質問に本人の回答を結び付け、待ちを解消する。 |

`state question answer` も使えます。テキストに `-` を渡すと標準入力から原文を読みます。各コマンドの `--help` でフラグを確認できます。

`state accept`・`state reject` と `subagent review HANDLE accept|reject [--reason TEXT]` は同じ正本にレビューを記録します。L2 はそのレビューから子の状態を更新し、`/analyze` はレビュー済みの子を分母として差し戻し率を計算します。子の実行結果は独立した記録です。
