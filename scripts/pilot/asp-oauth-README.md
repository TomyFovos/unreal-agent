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
   sha256sum -c @ARCHIVE@.sha256 && asp_proxy_work=$(mktemp -d /tmp/unreal-asp-oauth.XXXXXXXX) && tar -xzf @ARCHIVE@ -C "$asp_proxy_work" && source "$asp_proxy_work/@KIT_ROOT@/activate.sh" --model gpt-6.1-sol --effort medium --instance oauth-inference-v1 && unreal asp-inference
   ```

`SBX_CRED_OPENAI_MODE=oauth`、既存`~/workspace`、kitの`auth.json`と`config.toml`が
必要です。認証ファイルの配置や値をスクリプトが修正することはありません。
Dockerが既に設定した`HTTPS_PROXY`（または`https_proxy`）も必要です。
認証注入に必要なforward proxyだけを使い、transparent/direct経路へfallbackしません。
許可されるproxyはDockerのhost/gateway内部名・3128番のHTTP listenerだけです。
欠落・別host/port・userinfo・上下ケースの不一致はnative preflightで安全に停止します。
URL値、header、credential、レスポンス本文は診断へ出しません。既存proxy設定や
CA/trust storeをこのキットが変更することもありません。
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

この更新では旧binaryのHostを再利用しないよう、明示的な新instance
`oauth-inference-v1`を使用します。旧`oauth-v1`と`oauth-catalog-v1`のHost・config・Session Storeは停止・変更せず
保持します。新instanceには旧Session履歴を移行しません。
config=`~/.config/unreal-agent-pilot/asp/oauth-inference-v1/runtime.json`（0600）、
state=`~/.local/state/unreal-agent-pilot/asp/oauth-inference-v1/`、socket=`state/run/host.sock`。
初期権限はworkspace内read/grep/globのみ。edit/write/Bash/childは許可しません。
`--instance <別名>`は新しい設定用で、旧設定やSessionを移行・上書きしません。

`unreal asp-inference`はcreate/attach/resumeを行い、起動だけでは推論を送りません。
TUIで`/model`、`/view orchestration`を確認できます。Ctrl-Dでdetachした後は
同じshellで`unreal asp-inference`により再接続します。
`/model`は既存models_cache.jsonを優先し、欠落・形式不一致ならCodex CLIの
metadata-only `model/list`を利用します。既存Codex CLIがPATHに必要です。
推論・thread作成・tool実行は行わず、API keyやproviderへfallbackしません。
一覧はCLIのbundled/cache metadataの場合があり、各modelの契約上の利用権を保証しません。
取得失敗時はsafe reasonを表示し現在選択を維持します。`/model refresh`で再取得できます。
短い実推論を1件だけ試す場合は、ユーザー自身が`unreal --smoke-only`を実行します。
既知Finalのcanonical保存とOperations=0を検査し、再送・自動fallbackしません。
Sandbox接続・実推論の実機確認は未実施です。
最初のTUI依頼は`Reply with exactly: CODEX_PROXY_OK`程度に限定してください。
正常なFinalが履歴に1件保存され、Operationは0件になることを確認します。
proxyの401/403は既存の`unauthenticated`/`forbidden`となり、自動再認証・再送はしません。

復帰は`unset -f unreal`の後に従来のCodex起動操作を使用します。
default Hostは変更しません。Pilot Hostを止める場合は当該socket/configを使用する
専用プロセスだけを停止し、state/sessionsや認証は削除しません。
