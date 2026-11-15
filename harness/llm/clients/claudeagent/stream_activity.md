# E3b: Claude の応答待ちの観測

`claude_stream` activity は、CLI が公開する stream と system フレームの
受信状況を記録する。本文、推論、tool の引数、エラー本文は保存しない。
停止、時間制限、再試行、Provider の切り替えは追加しない。

## 記録の読み方

共通の `llm.ActivityRecord` は変更せず、`Reason` に JSON を入れる。
`ID` は `<TurnID>/stream/<連番>`。同じ CLI プロセス内の各 `message_start`
で連番を進める。`parent_tool_use_id` がある native Agent の stream は
独立して集計し、その ID も JSON に残す。

| `Reason.event` | 記録 |
|---|---|
| `message_start` | 開始時刻、model、分かる場合の `input_tokens`（cache read/write を含む）。 |
| `progress` | 開始以来の `delta_count`、可視の推論と本文の概算トークン数、`last_delta_at`、届いていれば provider の出力・推論トークン数。 |
| `no_delta` | 前のサンプル以降 delta がない。JSON はこの event と、native Agent なら親 tool ID だけ。 |
| `message_stop` | 最終の累計値と、直前の `message_delta.delta.stop_reason`（分かる場合）。 |
| `api_retry` | 種類別の受信回数、CLI の `attempt`、`max_retries`、`retry_delay_ms`、分かる場合の HTTP status、`no_response` の待機時間。 |
| `api_error` | 種類別の受信回数と、分かる場合の `error.status`。 |
| `error` | stream 内のエラーの type と、その type の受信回数。 |

E4 以降は、content block の開始と delta の種類から分かる場合に
`output_phase`（`thinking` / `text`）も記録する。推論や本文の内容は記録しない。
メインの `max_tokens` / `max_output_tokens` による `message_stop` は、
native Agent と分けて実行内で数える。CLI の回復処理が失敗して実行が終わった
場合だけ、この回数・分かる上限値・phase を失敗理由に使う。
観測できなかった回数や phase は不明とし、環境変数による上限変更は案内しない。
effort の変更・再試行・失敗の配送は行わず、既存の経路を維持する。

費用の正は `claude_usage`（SDK の実行全体の usage）。`provider_usage` は同じ
合計のモデル別内訳であり、重ねて加算しない。完全な stream の usage で native
Agent のモデルを分け、実行全体からその分を引いた残りをメインモデルに割り当てる。
回復処理で破棄された出力を含め、`<synthetic>` のモデルは除く。
共通の Session 記録を読む側では、`ToolContinuation` で同じ実行につながる
途中の応答 usage も、この実行全体の合計に置き換える。

途中のサンプルは名前付き定数 `streamActivityInterval = 60 * time.Second`。
stdout の読み取りとは別の ticker を使うため、CLI が何も書かなくても、
開始済みで未完了の stream には `no_delta` が残る。`StartedAt` は stream の
開始、途中の `EndedAt` はサンプルの時刻であり、モデルの完了を表さない。
`message_stop` 後はサンプルを出さず、CLI の終了時には ticker を停止して回収する。

`delta_count` は `content_block_delta` と `message_delta` の合計。
概算は、届いた `thinking_delta.thinking` / `text_delta.text` の UTF-8 byte 数を
それぞれ累計し、4で割って切り上げる。本文そのものは保持しない。
その種類の可視 byte がまだ届いていなければ概算欄を省略する。
非公開の推論や未受信部分は測れず、tokenizer による正確な値でもない。
概算には `_estimate` を付け、`ActivityRecord.Usage` や TPS に加算しない。

回数は各 stream の開始でリセットする。`message_start` より前や完了後の
system フレームは `ID = TurnID` で記録し、要求への対応を推測しない。
再試行で次の `message_start` が届けば、新しい ID で数え直す。
前の stream の `message_stop` が欠けても、完了記録を捏造しない。

## 実 CLI で確認した範囲（2026-11-14）

[install.py](install.py) が固定している Claude Code **2.1.322** の公式 Linux
artifact と Node を、新しい一時ディレクトリに integrity 検証付きで導入した。
空の `CLAUDE_CONFIG_DIR` を指定し、`--version` と `--help` だけを実行した。
認証ファイルと実 Session は参照せず、モデル推論は実行していない。

- 実行した CLI の `--version` は `2.1.322 (Claude Code)`。
- `--help` は、`--include-partial-messages` が到着した partial chunk を含め、
  `--print` と `--output-format=stream-json` の場合に使えることを説明している。
  stream の一定間隔や、推論 delta の発生頻度は記載していない。
- 公式配布バイナリ内の schema を識別子に絞って確認した。
  `system/api_retry` には `attempt`、`max_retries`、`retry_delay_ms`、
  nullable な `error_status` がある。任意の `no_response` は、first-byte の
  待機時間内に response header が届かない場合の追加情報として定義されている。
  その `waited_ms` / `retry_wait_ms` は数値として記録する。
  `system/api_error` には `error.status` と、保存対象外の message / formatted /
  connection などがある。

これらは実 CLI の help と配布物内の型定義の確認であり、再試行を起こした
実測ではない。推論中に `stream_event` が何秒ごとに出るか、再試行の前後で
どの event がどの順番で出るかは**未確認**。今回参照した既存資料には、これを
確定できる実 stream の時系列記録はない。偽 CLI の試験を、その根拠にはしない。

`no_delta` は「この adapter に delta が届かなかった」という観測であり、
通信断やモデル停止の断定ではない。`message_start` 自体がまだ届かない区間の
要求開始も特定できない。次の本番では、`native_tool` の終了、`message_start`、
`progress` / `no_delta`、`api_retry` / `api_error` の時刻を照合する。

## 試験

`stream_activity_test.go` は既存の偽 CLI を実プロセスとして起動する。
連続 delta、途中から stdout が無音になる応答、開始前・開始後の再試行と
エラーの3ケースを検証する。サンプルを確認するまで偽 CLI を終了させないので、
フレーム到着時だけの記録では通らない。テストだけで間隔を短縮する。
別途、native Agent とメインの分離、次の要求での累計リセット、
`message_stop` 後の記録終了、未知の token 数の省略も確認する。
