# @SANDBOX@ / @PROVIDER@ Pilot

既存`6284e49` Linux amd64 / CGO Pilotインストールを再利用します。
binary、Codex/Claude設定、認証、PATH、既存default Host/Session Storeを変更しません。
Sandbox内の実行はユーザーが担当します。WSL検証はSandboxの実機PASSではありません。

Windowsでこのarchiveと隣の`.sha256`のあるフォルダからコピーしてください。
ホストの`sbx`の利用可否は未確認です。既存ファイルがあるコピー先は別の専用場所に変更してください。

```text
sbx cp ./@ARCHIVE@ @SANDBOX@:/tmp/@ARCHIVE@
sbx cp ./@ARCHIVE@.sha256 @SANDBOX@:/tmp/@ARCHIVE@.sha256
```

Sandbox内では次を1回実行します。model/effortは明示的なoperator選択です。
モデルの利用可否は実providerで確認するまで未保証です。

```sh
(set -eu; cd /tmp; sha256sum -c @ARCHIVE@.sha256; work=$(mktemp -d /tmp/unreal-provider.XXXXXXXX); tar -xzf @ARCHIVE@ -C "$work"; bash "$work/@ROOT@/pilot.sh" --model @MODEL@ --effort @EFFORT@)
```

`~/workspace`は既存の実ディレクトリが必須です。ファイルは作成・変更しません。
Python 3、bash、sha256sumと既存Pilot doctorの依存コマンドが必要です。
Pilotが未導入/複数配置の場合は停止します。既存キットを先に導入するか、
`--kit-directory <既存インストール先>`を明示してください。binaryを自動再導入しません。

@AUTH_INSTRUCTIONS@

専用配置はconfig=`~/.config/unreal-agent-pilot/@SANDBOX@/v1/runtime.json`、
state=`~/.local/state/unreal-agent-pilot/@SANDBOX@/v1/`、socket=`state/run/host.sock`です。
新しいprofileは0700/config0600でatomicに作成し、既存configは同じ内容なら再利用します。
変更されたconfigは上書きせず、変更を意図する場合だけ新しい`--instance <名前>`を指定します。
`Launcher.DiscoverProviders=false`でproviderは1つに固定し、API key/MCP/他provider fallbackはありません。
Permissionはread/grep/globとworkspace ReadRootsのみ。write/edit/Bash/childは許可しません。
Codexの限定NetworkOriginsはprovider transport用です。Claudeの隔離CLI transportの許可を
tool processの許可と混同しません。filesystem/network unrestrictedやProcessMode unrestrictedは使用しません。

前提を満たすと専用Hostを起動/互換確認して再利用し、短いFinal-only smokeを1件送ります。
成功条件は既知の応答とcanonical履歴、Operations=0です。本文・IDs・秘密情報をdiagnosticsへ出しません。
providerが拒否/タイムアウトした場合も自動再送・自動fallbackはせず、専用履歴を保持して停止します。
管理者設定、認証のlogin/refresh、ネットワーク設定、OSパッケージはスクリプトで変更しません。
CLI自身が認証/managed設定を管理する境界は保持します。

smoke成功後はnative TUIへattachします。Ctrl-Dでdetachしても専用Hostは生存します。
再接続時は同じscript/model/effortに`--attach-only`を追加すれば新しいinferenceを送信しません。
`--preflight-only`は専用configの生成/検査までで、Host/inferenceを起動しません。
`--smoke-only`はsmoke終了後にTUIへ入らず終了します。
失敗時は`--attach-only`で`/analyze Errors`を確認し、既存Codex/Claude CLIの操作へ復帰できます。
Hostの終了が必要なら、このprofileの`host.log`とsocketを使うPilotプロセスだけを識別して停止し、
state/sessionsと既存認証を削除しないでください。既存/不一致Hostを自動停止しません。
