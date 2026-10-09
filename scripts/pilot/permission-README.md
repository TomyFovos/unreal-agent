# @SANDBOX@ — Permission Mode Gate D/E

Linux amd64 / GOAMD64=v1 / CGO / tree-sitter の新snapshotです。OS sandboxを追加する
ものではありません。実Sandboxの操作・承認・推論・最終受入はユーザーが担当します。
既存Codex/Claude CLI、PATH、認証、default config/Host、旧Session、旧Pilot配置は
変更しません。新しいprivate配置へ導入し、同じキットの再実行は検証後に再利用します。
過去の6284e49 binaryにはPermission Modeがないため、この新snapshotを使用します。

入力欄の直下にprovider/model・effort・TPS・Input/Output usageを常時表示します。
`(api)`はClaude CLIのAPI時間と報告token数からの速度、`~...(est)`は報告token数と
Unrealのgeneration時計による推定です。tool/承認待ちを計測へ混ぜません。
token数を本文から推定せず、不明は`—`（ASCII表示では`-`）。完了値は保持し、
新generationの統計が未報告なら不明表示へ戻ります。狭い画面では項目を省略します。

## 転送と一括導入

Windowsでarchiveと`.sha256`のあるフォルダから実行してください。
`sbx`のホスト上の利用可否は未確認です。既存転送先がある場合は別パスを選んでください。

```text
sbx cp ./@ARCHIVE@ @SANDBOX@:/tmp/@ARCHIVE@
sbx cp ./@ARCHIVE@.sha256 @SANDBOX@:/tmp/@ARCHIVE@.sha256
```

Sandbox Bashで実行します。modelはGate Cで確認済みの選択に合わせて変更可能です。
devloreのtrust flagsはその環境の管理policy監査済みという明示opt-inです。

```sh
set -eu
cd /tmp
sha256sum -c @ARCHIVE@.sha256
permission_unpack=$(mktemp -d /tmp/unreal-permission.XXXXXXXX)
tar -xzf @ARCHIVE@ -C "$permission_unpack"
bash "$permission_unpack/@KIT_ROOT@/gate.sh" setup --model @MODEL@ --effort high@OPT_IN@
```

導入にPython3、Bash、Git、standard coreutils/ldd/tarが必要です。OS packagesは
インストールしません。glibc @MIN_GLIBC@以上をdoctorで検査します。
aspは既存Docker host-managed ChatGPT OAuth/HTTPS proxyをnative preflightで検査。
API key/account/tokenを取得・捏造・コピーしません。devloreは既存Team CLI authを
restricted preflightで確認し、Structured Bridge/managedPolicyMode=trustを維持。
MCPや他providerへfallbackしません。未認証・未監査はfail-closedです。

setupは実推論を送りません。表示された`LAUNCH:`の専用`unreal`のパスを使用します。
起動直前にこのREADMEを表示し、対話端末では**Enterまで待機**します。Ctrl+C/EOFは
TUIを起動せず終了します。非対話環境の自動試験は待機しません。
`~/.local/bin/unreal`やshell設定は置換しません。configは0600、各専用directoryは0700。
state/workspaceは専用の新しい一時Git repoであり、既存`~/workspace`は使用しません。
証拠保持のためworkspace/stateは永続で、削除しません。

以下の変数で全操作を実行できます（再接続時にも同じパス）。

```sh
permission_gate="$HOME/.local/state/unreal-agent-pilot/@SANDBOX@/permissions-@REVISION_SHORT@"
permission_package="$HOME/.local/opt/unreal-agent-pilot/@SANDBOX@/@REVISION_SHORT@-$(sha256sum "$permission_unpack/@KIT_ROOT@/SHA256SUMS" | cut -c1-16)"
# 後から手順だけ再表示（Host起動・認証probe・推論なし）
bash "$permission_package/gate.sh" readme
```

## Gate D — 履歴とHost lifetime

1. `"$permission_gate/unreal" gate-d`で新Sessionを開く。headerがRead Only、
   `/model`に該当providerのモデル/effort、`/view orchestration`が使えることを確認。
2. 別Bashで`bash "$permission_package/gate.sh" prompt d`を表示し、TUIへ1回送信。
   正確なFinalを待ち、Ctrl+Dでdetach。Hostは生存します。
3. 同じ`"$permission_gate/unreal" gate-d`で再接続し履歴を確認してdetach。
4. 次を実行し、同じSessionを開き直して、再送せずにdetachして確認。

```sh
bash "$permission_package/gate.sh" verify d
bash "$permission_package/gate.sh" checkpoint gate-d
bash "$permission_package/gate.sh" stop
"$permission_gate/unreal" gate-d
# 履歴確認後Ctrl+D
bash "$permission_package/gate.sh" resume-check gate-d
```

stopはUnix peer UID/PID、binary、serve引数、専用config/store/socket/authパス、
process start timeを照合し、pidfdでそのHostだけへSIGTERMします。照合できなければ
停止しません。再起動は通常launcherがgateway readiness/lock/stale socketを処理します。
PASSはcanonical履歴prefix/terminal receipts不変、Operation重複0、Read Only再初期化。

## Gate E — 実際のOperationと人間承認

`"$permission_gate/unreal" gate-e`を開きます。新SessionはRead Onlyです。
以下の各promptを`bash "$permission_package/gate.sh" prompt STAGE`で表示し、順にTUIへ
1回だけ送信してFinalを待ちます。各段階でCtrl+Dして`verify STAGE`を実行できます。
再接続は同じ専用unrealで、Full Accessは同じHost内でのみ継続します。

| STAGE | 人間の操作 / PASS |
| --- | --- |
| read | read/grep/globの実receiptと結果。 |
| deny | Read OnlyのwriteとBashを実際に要求。Unreal typed denial、出力ファイル0。モデルの文章だけではFAIL。 |
| ask | 先に`/permissions`→Ask for Approval。最初のwriteをAllow Once、次のwriteをDeny。once.txtだけ生成。 |
| work | `/permissions`→Full Accessを明示Confirm。sample.txtをnative write→read→edit→read→Bashによる内容検証。 |
| full | 先に`/permissions`→Full Access→別画面の警告を読み明示Confirm。Bash failing test→edit/write→passing testのcanonical receiptsと成果物。 |

Full Access/Bashは**OS sandboxではありません**。認証情報・filesystem・network・
environment・shared mountsへambient accessでき、任意command/Git pushも技術的には可能。
試験promptは専用repo内だけを指示しますが、Bashをrepo内へOS隔離する設定ではありません。
このアクセスを受け入れる場合だけ確認してください。80x24未満では承認不可です。
初期baselineはread/grep/globのみで、設定のMaximumは承認ではありません。

**native write/editはSessionのworkspaceに限定**されます。`sample.txt`など相対pathを
使い、別の`/tmp` repoを作らないでください。Bashで外のdirectoryを作れてもnative toolの
workspaceにはなりません。新規writeの`expected:{exists:false}`は正規形式で、digestは不要。
親directoryは存在する必要があります。editではreadが返したwhole-file revisionを使用。
失敗receiptの`reason=workspace_boundary`、`parent_missing`、`invalid_revision`、
`revision_conflict`等で原因を確認できます。`code=invalid`だけでPermission拒否とは扱いません。

長い応答は会話領域上のマウスホイール、PgUp/PgDnで読めます。Ctrl+Home/Endで先頭/末尾、
入力が空ならHome/Endと上下キーも履歴操作です。入力中のHome/Endはcomposer操作を維持。
Splitの右ペイン上のホイールはOrchestrationをスクロールします。コピー機能は不要です。
端末のテキスト選択が必要ならShiftを押してドラッグしてください（端末のmouse override）。

```sh
# STAGEごとのFinal後、detachして実行
bash "$permission_package/gate.sh" verify read
bash "$permission_package/gate.sh" verify deny
bash "$permission_package/gate.sh" verify ask
bash "$permission_package/gate.sh" verify work
bash "$permission_package/gate.sh" verify full
# Full Access中にdetach/reattachしてmodeが保持されることも確認
bash "$permission_package/gate.sh" checkpoint gate-e
bash "$permission_package/gate.sh" stop
"$permission_gate/unreal" gate-e
# Read Onlyと既存履歴を確認し、推論を送らずdetach
bash "$permission_package/gate.sh" resume-check gate-e
"$permission_gate/unreal" gate-e
# prompt restartを1回送信し、拒否receipt/Finalを待ってdetach
bash "$permission_package/gate.sh" verify restart
```

Full Accessの取消は`/permissions`→Read Only。未承認中のdetach/timeout/cancelも
fail-closedです。再起動時の未完Operationはexpired/canceledとなり、勝手に再実行しません。
過去の許可操作や履歴は消えません。副作用のrollbackは保証しません。

verifyはcanonical decision/Operation、test exit code、生成物を検査し、本文・credential・
tool argsを表示しません。完了済みの全Operation IDは一意でなければFAILです。
full promptは`python3 -m unittest -v`を正確に使用します。別commandやLLMの拒否だけを
成功へ読み替えません。失敗時は専用Sessionの`/analyze Errors`を確認し、証拠を保持。

## 復帰

Ctrl+Dでdetachし、必要なら上記stopで専用Hostだけを停止。既存の`codex`/`claude`、
既存通常unrealをそのまま使用できます。認証・旧state・新試験証拠の削除/移行は不要です。
このキットはpush/PR/Releaseを行いません。WSLのfake試験はSandbox実機のPASSとは別です。
