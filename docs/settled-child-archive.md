# Settled child archive（2026-11-18）

ユーザー決定: 終了した子の記録は削除しない。settled な子のファイルを live の Session directory と別の場所にまとめ、起動時の restore/reconcile と通常の子一覧を軽くする。期間・サイズによる上限は設けない。本番 Session・認証情報は作業・計測に使っていない。

## settled の判定

`harness/host/child_archive.go` は、親の writer 所有下で canonical items / Operations から以下を確認する。

1. `subagent` v1 の `start` Operation が completed / failed / canceled。
2. `harness/subagent/reporting.Index` が親への終了報告の到達を確認済み。子の Finish や terminal checkpoint だけでは判定しない。旧束ね通知・show/wait receipt・terminal tool receipt の照合は d82b00c の Index をそのまま利用する。
3. 親に未回答の質問・承認がない。`message` の canonical peer input は質問として扱い、`progress` は区別する。回答は後続の親の send intent と子に記録された `send:<operation-id>` receipt で照合する。親の completed checkpoint だけでは回答到達を推測しない。承認は start handle の Approvals と、対応する成功済み approve Operation から判定する。
4. 実際の移動直前に子の writer lease を取得できる。lease がある場合は移動せず、実行終了後に再確認する。

子 ID の導出は `harness/subagent/identity` に共有し、通常の Subagent と archive で別の identity 規則を持たせない。

## 保存形式と中断回復

例えば live が `<state>/sessions` の場合:

```text
<state>/sessions/
  main.session.jsonl
  main.session.jsonl.lock
  .archive.lock
<state>/sessions-archive/
  index/<child-id>.json
  pending/<child-id>.json              # 移動中だけ存在
  <parent-id>/<child-id>/
    <child-id>.session.jsonl
    <child-id>.session.jsonl.lock
    operations/<child-id>/...          # 存在する場合
    attachments/<child-id>/...         # 存在する場合
```

index は child ID・parent ID・start Operation ID・canonical ended time・最終 status・archived-at・original path・移動対象の相対パスを持つ小さな JSON。original path は記録時の情報であり、状態ディレクトリ全体を移した後の参照先を固定しない。

`localfile.ArchiveChild` は directory layout の exclusive flock と子の writer lease を取得する。`AcquireWriter` は同じ layout の shared flock の下で実際の lock inode を取得し、移動中に live 側へ別 inode を作らない。

子単位の移動は以下の journaled transaction とする。

1. pending index を一時ファイルから rename し、file/directory を sync。
2. 子専用の lock・operation/attachment directory を rename。
3. record を最後に rename。record は常に live / archive のどちらか一か所にあり、内容をコピー・再エンコード・切り詰めしない。不完全な末尾もそのまま保存する。
4. pending index を completed index へ rename し、両 directory を sync。

各ファイルの rename は atomic。子全体の途中状態は pending index で表現し、reader は live の inode を開くか、index から archive の inode を開く。既に開いた descriptor は rename 後も有効。次の `Host.New` は pending index だけを列挙して未完の rename を再開する。completed archive の本文を列挙しない。回復時にも子の writer lease を尊重する。

## 実行タイミング

- Host 起動時: live の stopped Sessions の reconciliation 後に settled な子を移動。開いている親自身は Coordinator の `AfterRestore` hook 後に処理する。
- runtime: canonical item / Operation の更新を channel でまとめて処理する。報告が到達し、子が終了した時点で移動する。lease の解放待ちだけを 1 秒間隔で再確認する。この間隔は保存期間の条件ではない。
- 停止中の親でも、起動時に他の条件を満たして lease 解放だけを待つ場合は Host 所有の maintenance が再確認する。停止中の親 runtime は起動しない。
- 終了時: runtime と Operation manager の drain / 最終 checkpoint 後に再確認する。

## reader の調査と対応

`.session.jsonl`、`sessionPath`、`ListSessions`、`localfile.New`、`ReadChild` を Go と収集スクリプトで検索した。

| 経路 | 対応 |
|---|---|
| localfile Inspect / ReadSnapshot / Items / Operations / Resume / Fork の親参照 | 共通の `OpenRecord` / `readRecord` が live → index を解決 |
| OpenRestoreView / RecoveryOperations | 同じ record resolver を利用 |
| ListSessions、Host stopped-session reconciliation | live のファイルだけを列挙 |
| subagent RetainOperation | completed archive は index だけで復帰判定。本文や writer lease を再取得しない |
| subagent watchRecovered / viewer read cache | `StatSession` が解決済み inode の identity / size / mtime を返す |
| ReadChild / ReadChildSnapshot、viewer ParentReader / gateway の child history | localfile resolver を透過利用。親子の canonical ownership 検証を維持 |
| TUI の通常の子一覧 / topology | 親の canonical Operations を利用。ended entries を残し、非選択の ended child 本文を polling しない |
| TUI `/analyze` | 明示要求時に direct children を `PrepareAnalysis` で観測。archive の usage / Finish / history も集計できる |
| `/export` / child history | canonical child reader が解決。同じ記録の公開用 projection を使用 |
| CLI subagent show / wait / list --all | 親の canonical start handle / Finish を利用。archive 後も同じ handle で参照 |
| agentrunner drainParent / aggregateChildren / endRemainingChildren | localfile / ReadChild を通じて解決 |
| Harbor launch / product export / offline costs | `session_files.session_paths` が live と index/pending を統合。収集コピーでは従来の flat な Session コピーにし、各子を一度だけ集計 |
| scripts/common_development.py の Session discovery | 元から main のみ（child-* を除外）。変更不要 |

operation の stdout/stderr の inline result は record に保持し、子専用の完全な capture ファイルも上記の operations directory ごと保存する。workspace・shared state・別 Session のファイルを移動しない。

## 検証

- settled（completed / failed / canceled）が archive される。
- pending、未配信、live executor、未回答の質問・承認は archive されない。
- 親の send が completed でも、子の receipt がなければ未回答のまま。子の receipt があれば親の最終 checkpoint が failed でも到達済みとして扱う。
- index 保存直後と、lock・operations・attachments・record の各 rename 境界を再現。回復前後に record が一か所だけで読め、bytes が同一であること、反復回復・writer exclusion を確認。
- 不完全な末尾を含む canonical bytes、他 Session / shared file、index metadata、state root 全体の移動を確認。
- 親の archive 前後・繰り返し restore の canonical public projections を比較。新しい ownership と各起動固有の host_start telemetry は比較から分離。
- 6 子（成功・失敗・取消各 2）の fixture で `/analyze` JSON と viewer rows が archive 前後で同一。child conversation export も export 時刻以外が同一。
- 実 gateway を使い archive 済み handle の show / wait / list --all と cached child history を確認。
- archived transcript が利用できない fixture でも RetainOperation が index だけで復帰判定できる。
- Harbor の live / completed / pending と子・孫の offline cost 集計を確認。

## 起動計測

baseline は d82b00c を `/tmp` の隔離ディレクトリに `git archive` で展開し、同じ fixture / benchmark のみを追加。実モデル・API は利用していない。

環境: WSL2 Linux 6.18.58.1、Intel Core Ultra 7 355、Go 1.27.1 linux/amd64、CPU 0,1、nice 19。2,000 settled children、各本文 128 KiB。両版とも同じ warmup / command を使い、3 回の中央値を比較する。warmup は一回限りの archive 移行を含み、通常の起動計測から分離する。起動の終点は最後の child executor の復帰確認（実 `subagent.Manager.RetainOperation`）の完了。

```bash
GORACE=atexit_sleep_ms=0 nice -n 19 taskset -c 0,1 go test -p 1 ./harness/host -run '^$' -bench '^BenchmarkSettledChildStartup$' -benchtime=1x -count=3 -timeout 10m
```

| 指標 | d82b00c | archive 後 |
|---|---:|---:|
| Host start（executor 復帰確認まで） | 1,040 ms | 271.2 ms |
| coordinator_restore（親の history replay） | 35.09 ms | 44.95 ms |
| executor_restore（子の executor 確認の合計） | 694.7 ms | 28.50 ms |
| reconcile_stopped | 148.2 ms | 0.1042 ms |
| live Session records | 2,001 | 1 |
| live directory のファイル bytes（論理サイズ） | 269,470,656 B | 4,024,801 B |

初回起動（移行を含む warmup）は 18,385 / 17,821 / 17,751 ms、中央値 17.821 秒。以後の start-ms は 257.5 / 271.2 / 279.5 ms、baseline は 1,041 / 1,032 / 1,040 ms。親自身の history replay は archive の対象外で、今回の計測でも短縮していない。この synthetic state は本番 Japan Harness の 5.9 GB を複製したものではなく、本番の秒数を予測する計測ではない。

最終の `make build` / `make prepush` は成功。prepush は Go vet / 全 Go tests / scripts Python / Harbor lint・format・tests を含む。scripts Python は 146 件中 9 件 SKIP（PASS に含めない）、Harbor は 63 件で SKIP なし。

full race は最後に以下を **1 回だけ** 実行し、終了コード 0、FAIL 0。56 package PASS、4,495 test/subtest PASS。テストなしの 10 package と 11 test SKIP は PASS に含めない。SKIP の内訳は live Claude 3・OpenCode Go 4・Ollama 1、snapshot 2、installed CLI metadata 1。JSON イベント上の実行時間は 266.206 秒。

```bash
GORACE=atexit_sleep_ms=0 nice -n 19 taskset -c 0,1 go test -race -json -count=1 -p 1 ./...
```
