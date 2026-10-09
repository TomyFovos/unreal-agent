# devlore / 恒久 Unreal launcher

launcher revision: `@REVISION@`。既存の `6284e49` Linux amd64 / CGO Unreal Agent
インストールを再利用します。binary・認証を含まない小さな更新キットです。
Claude CLI・認証・設定、旧smoke Session、default Host、aspを変更しません。
WSLのfake検証はSandboxの実機・認証・推論PASSを意味しません。

1. Windowsでarchiveと`.sha256`のあるフォルダからdevloreへ転送します。
   ホストの`sbx`利用可否は未確認です。同名ファイルがある場合は別の専用場所へ転送してください。

   ```text
   sbx cp ./@ARCHIVE@ devlore:/tmp/@ARCHIVE@
   sbx cp ./@ARCHIVE@.sha256 devlore:/tmp/@ARCHIVE@.sha256
   ```

2. devlore Bashで一括インストールします。managed policyの監査・trust受入を
   確認した環境だけで、下記の2つの明示フラグを使用してください。
   初期モデル/effortはoperator選択で、起動後に`/model`で変更できます。

   ```sh
   (set -eu; cd /tmp; sha256sum -c @ARCHIVE@.sha256; devlore_install_work=$(mktemp -d /tmp/unreal-devlore.XXXXXXXX); tar -xzf @ARCHIVE@ -C "$devlore_install_work"; bash "$devlore_install_work/@ROOT@/install.sh" --policy-audited --trust-managed-policy --model claude-opus-5-5 --effort high)
   ```

インストール後は**新しいBashを開き**、`unreal` または `unreal my-session` を実行します。
毎回の`source`や`pilot.sh`は不要です。`command -v unreal`で確認できます。
`unreal`は新専用Storeの`default`、名前付きはそのSessionをcreate/attach/resumeします。
`/model`でClaude catalogとeffort、`/view orchestration`で状態を確認し、Ctrl-Dでdetach。
同じコマンドで再接続できます。インストール・起動だけでは推論を送りません。

`~/workspace`、既存の6284e49 Pilotインストール、Python3、Bash、既存doctorの
依存コマンドが必要です。複数の既存配置があれば `--kit-directory <既存配置>` を
installコマンドへ追加します。既存binaryの所在は旧キットと同じ探索規則です。
CLI欠落・未ログイン・未監査なら停止します。login、認証コピー、OSパッケージ変更、
API-key/provider/MCP fallbackは行いません。

`~/.local/bin/unreal`をuser-owned 0700で導入します。既存同名command・alias・functionを
置き換えたりshadowingしたりせず停止します。同じキット・設定なら冪等に再利用し、
変更されたcommand/configは上書きしません。PATH不足時は専用`path.sh`を作成し、
`.bashrc`とBashが実際に読むlogin fileへguarded source行だけを追記します。
既存内容・login fileの優先順位は保持し、system PATH/profileは変更しません。

config: `~/.config/unreal-agent-pilot/devlore/launcher-v1/runtime.json`（0600）
state: `~/.local/state/unreal-agent-pilot/devlore/launcher-v1/`（0700）
socket: `state/run/host.sock`

旧`devlore/v1`のhistory・config・Hostは保持し、移行・再利用しません。
新しいSessionで検証してください。初期Permissionはworkspace内read/grep/globのみ。
write/edit/Bash/childおよびunrestricted権限は追加しません。
Claude初期provider、`managedPolicyMode=trust`、`ToolBridge.Mode=structured`を明示します。
これはoperatorの監査済み宣言であり、組織policyの自動監査・不変性保証ではありません。
Unreal adapterのrestricted environment、empty MCP、side-effecting built-in無効を維持します。

従来の `claude` 操作はそのまま利用できます。Pilotを停止する場合はこの新socketに
紐づくHostだけを識別して停止し、Session・認証を削除しません。既存commandとの競合、
特殊なBash startup、binary/config変更は安全に停止するため、勝手な置換で回避しません。
