# Unreal Agent 共通開発キット

asp/devloreの両方で同じarchive、同じセットアップを使います。Linux amd64、
glibc @MIN_GLIBC@以上、Python 3・Bash・Gitが必要です。CGO/tree-sitter/ASTと
4 binaryを含むローカルsnapshot（@REVISION@）。正式Releaseではありません。
認証情報を同梱せず、外部Codex/Claude CLIが認証を所有します。

Windowsの転送手段は未確認です。`sbx`が使える場合、同じ2ファイルを転送します。

```powershell
sbx cp .\@ARCHIVE@ asp:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
sbx cp .\@ARCHIVE@ devlore:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 devlore:/tmp/@ARCHIVE@.sha256
```

各SandboxのBashで同じ手順を実行します。

```bash
cd /tmp
archive='@ARCHIVE@'
test -f "$archive" && test -f "$archive.sha256" && sha256sum -c "$archive.sha256" &&
kit=$(mktemp -d "$HOME/unreal-common.XXXXXXXX") &&
tar -xzf "$archive" -C "$kit" &&
cd "$kit/@KIT_ROOT@" &&
bash development.sh --configure-providers --replace-pilot-launcher
```

Claudeは、このSandboxにインストールしてClaudeサブスクでログイン済みの公式
Claude Codeで動きます（API key・MCP・Modは使いません）。事前に確認します。

```bash
claude --version && claude auth status
```

未ログインなら`claude`を起動して`/login`してください。Claude Codeはtool
（Read/Edit/Bash等）を自分で実行しますが、承認が必要な操作は毎回Unrealに
問い合わせ、`/permissions`のmode（Read Only/Ask/Full Access）で決まります。
組織の許可リストのコマンドとworkspace内の読み取りはClaude Code自身が許可します。
組織ルールでClaude Codeが拒否したコマンド（git push等）は、Unrealが自分の
シェルでUnrealのmodeに従って実行し、結果をClaudeに返します。
セットアップに質問と起動前のEnter待ちはありません。

CodexはGo native resolverが初回だけ決定的に発見します。Docker OAuthは既存
source/TOML routing/OAuth mode/固定HTTPS forward proxyをすべて検証できた場合
だけ選びます。Sandbox名、hostname、proxy文字列だけでは選びません。
意図したDocker sourceの不一致や曖昧なroutingは当該Providerだけunavailableに
し、別credentialへfallbackしません。External方式も既存認証とcatalogを検証
します。ClaudeはClaude CLIのサブスクのログインとモデル一覧をnativeで確認します。

設定時のnative preflightは認証確認とcatalog取得だけです。推論、thread、
tool、Operationは実行しません。CLIのinstall/login、credentialのコピーや
設定変更は行いません。両方が未利用ならSession起動は拒否し、両方の不足理由を
表示します。API key、別auth route、別providerへの自動fallbackはありません。

既存の明示ルートは保持します。特別なルート変更には
`--docker-sandbox-oauth` / `--external-codex` / `--codex-auto`、意図した無効化には
`--disable-codex` / `--disable-claude`を任意指定できます。旧Claude経路の
`--trust-managed-policy` / `--reject-managed-policy` / `--policy-audited`は互換の
ため受け付けますが、Claude Code経路には影響しません。
別HOMEのlauncher bindingのコピーは拒否します。native standaloneの初期値は
`reject`のままです。利用不可のProviderを一覧から隠しません。

既知の旧wrapperだけをchecksumと所有権で確認し、private backup付きで置換
します。無関係な`unreal`は上書きしません。新しいBashを開き、`type -a unreal`、
必要なら`"$HOME/.local/bin/unreal"`を使います。現在のshellのalias/functionは
installerでは消せません。

```bash
cd ~/workspace/your-existing-project
unreal development
# Ctrl+Dでdetach後、同じrepoから再接続
unreal development
```

Git worktree rootをworkspaceとし、repo/instanceごとにconfig/state/socketを
分離します。両方利用可能なら初回だけ既存のinitial provider chooserを使います。
`/model`には対応する2社と利用状態を表示し、利用可能なProvider→Model→Effortを
選びます。未利用側にはCLI/login、trust、proxy、catalog等の固定された不足理由を
表示し、Host側でも選択を拒否します。モデル名やeffortを導入時に指定する必要は
ありません。非共通の明示`DiscoverProviders=false`設定は維持されます。

通常の初期権限はRead Only。`/permissions`で人間がAsk/Full Accessを明示承認
します。Full AccessはOS sandboxではなくambient権限です。Git公開の承認は別です。
ClaudeのtoolはClaude Codeが実行し、承認はUnrealのmode、記録はUnrealのSession
（claude_code receipt）です。

## 既存workspaceへの更新

セットアップで保存した希望設定と、workspaceに適用済みの設定/buildを区別します。
変更後、そのworkspaceで次に`unreal NAME`を実行すると、旧Host/履歴を保持し
**別instanceで新規Sessionを開始する**ことを短く説明します。人間が`y`で確認した
場合だけ新しいconfig/state/socketへ切り替え、以後は同じ通常コマンドで再接続
します。名前だけ変えてもprovider設定は更新されません。同じ承認済み設定の
再実行・再インストールでは質問せずinstanceを増やしません。実際のprofile変更
への非対話起動や確認拒否では旧利用先を保持します。可用性の変化とschema v1/v2
からv3への同一意味の移行だけでは新instanceを作りません。

旧runtime.jsonとSession security identityは書き換えません。Runtime/Providers内の
Claude設定がLauncherより優先していた場合も、古い設定へ書き戻さず新規bootstrapで
一貫した値を作ります。現在のconfigのhash/有効設定が変わった場合も検出します。

`unreal --workspace-info`で現在と旧config/state/socketを確認できます。
旧履歴へは`unreal --instance default NAME`、または表示された旧instance名で
明示再接続します。build identityが違う場合は保存した旧kitのbinaryを使い、
異版Hostを自動停止しません。`--instance`や`--config`の明示指定は自動bindingの
対象外です。既存Host/Session/認証/Gate環境を削除・移行しません。

configは`~/.config/unreal-agent-common/repos/r-*/native/unreal-agent/runtime.json`、
stateは`~/.local/state/unreal-agent-common/r-*/`。private 0700/0600とatomic
publish/lockを維持します。`unreal --readme`でこの説明、`bash doctor.sh`で4 binary・
CGO・arch/glibcを非推論検査します。認証の復帰は従来の`codex`/`claude`コマンドです。

## 新規合成workspaceでの受入

`unreal --acceptance-fixture repair`は200行のsample.txt（中央行だけ
UNREAL_REPAIR_BEFORE）とAGENTS.mdを含む新規Git fixtureを作ります。
実プロジェクトを探索・変更しません。最初に`/model`、`/permissions`を確認し、
人間が必要な権限を承認して次を送信します。

```text
sample.txtをnative readで200行全体読み、受信した本文とwhole-file revisionだけで
中央のUNREAL_REPAIR_BEFOREをUNREAL_REPAIR_AFTERへnative editしてください。
再readし、200行と他の行が保持されたことを観測してください。
Bashは/bin/sh -cです。POSIX sh構文で以下を検証してください：
set -eu
[ "$(wc -l < sample.txt)" -eq 200 ]
grep -q '^UNREAL_REPAIR_AFTER$' sample.txt
! grep -q '^UNREAL_REPAIR_BEFORE$' sample.txt
git diff -- sample.txt
実際のexit codeとcanonical receiptを観測してからFinalを返してください。
非0、拒否、秘匿stubを成功にせず、成功時だけUNREAL_ACCEPTANCE_OKと記載してください。
commit/push、shellによる編集、provider切替は禁止です。
```

通常引用文を今回の実行結果にしません。現在のcanonical receiptから受け取った
本文/revision/exitは観測値として確認します。read/edit/Bash receipts、実ファイル、
TPS/scroll、detach/attach履歴を独立に確認してください。
この手順は推論を自動送信しません。実機受入は合成検証とは別です。
