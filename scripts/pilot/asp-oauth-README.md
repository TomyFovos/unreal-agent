# asp / Docker host-owned ChatGPT OAuth 更新キット

基準 `@REVISION@`、Linux amd64 / glibc @MIN_GLIBC@以上、CGO/tree-sitter有効の
4 binaryを含みます。旧6284e49インストール、Codex CLI、認証、PATH、default設定、
既存Sessionを上書きしません。devloreには使用しません。正式Releaseではありません。

1. Windowsで配布ファイルのあるフォルダからaspへコピーします。
   ホストのCLI利用可否は未確認です。対応する`sbx cp`がない場合は停止します。
   コピー先に同名ファイルがある場合は新しい専用ディレクトリを使います。

   ```text
   sbx cp ./@ARCHIVE@ asp:/tmp/@ARCHIVE@
   sbx cp ./@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
   ```

2. aspのbashで次を実行します。modelは明示的なoperator選択です。

   ```sh
   cd /tmp
   sha256sum -c @ARCHIVE@.sha256 && work=$(mktemp -d /tmp/unreal-asp-oauth.XXXXXXXX) && tar -xzf @ARCHIVE@ -C "$work" && source "$work/@KIT_ROOT@/activate.sh" --model gpt-6.1-sol --effort medium && unreal asp-oauth
   ```

`SBX_CRED_OPENAI_MODE=oauth`、既存`~/workspace`、kitの`auth.json`と`config.toml`が
必要です。認証ファイルの配置や値をスクリプトが修正することはありません。
`OPENAI_API_KEY=proxy-managed`はAPIキーではなくkit markerとして検証されます。
`requires_openai_auth=false`のためaccount IDは取得・捏造・送信しません。
モード不一致、設定変更、未認証、接続不良では安全に停止します。token抽出、
API-key fallback、provider fallback、permission緩和、パッケージ自動導入はありません。

インストール先は`~/.local/opt/unreal-agent-pilot/asp/@REVISION_SHORT@-<content-id>/`。
同一キットの再実行は検証後に再利用し、変更済み配置は上書きしません。
Python3、bash、標準coreutils/tar/lddが必要で、ビルド環境は不要です。
`unreal`は明示的にsourceした**現在のshellだけ**でPilotラッパーになります。
shell設定ファイル・PATHを変更せず、内部では同梱native `unreal`を起動します。
既存の同名shell functionは上書きせず停止します。

config=`~/.config/unreal-agent-pilot/asp/oauth-v1/runtime.json`（0600）、
state=`~/.local/state/unreal-agent-pilot/asp/oauth-v1/`、socket=`state/run/host.sock`。
初期権限はworkspace内read/grep/globのみ。edit/write/Bash/childは許可しません。
`--instance <別名>`は新しい設定用で、旧設定やSessionを移行・上書きしません。

`unreal asp-oauth`はcreate/attach/resumeを行い、起動だけでは推論を送りません。
TUIで`/model`、`/view orchestration`を確認できます。Ctrl-Dでdetachした後は
同じshellで`unreal asp-oauth`により再接続します。
短い実推論を1件だけ試す場合は、ユーザー自身が`unreal --smoke-only`を実行します。
既知Finalのcanonical保存とOperations=0を検査し、再送・自動fallbackしません。
Sandbox接続・実推論の実機確認は未実施です。

復帰は`unset -f unreal`の後に従来のCodex起動操作を使用します。
default Hostは変更しません。Pilot Hostを止める場合は当該socket/configを使用する
専用プロセスだけを停止し、state/sessionsや認証は削除しません。
