# Unreal Agent 共通開発キット

同じarchiveをasp/devloreで使います。Linux amd64、glibc @MIN_GLIBC@以上、
Python 3・Bash・Gitが必要。CGO/tree-sitter/AST、4 binaryを含むローカルsnapshot
（@REVISION@）。正式Releaseではありません。認証情報を含みません。

1. Windowsホストから同じarchiveとchecksumを転送します。`sbx`の利用可否は
   ホスト依存です。利用可能なら以下を実行します。

```powershell
sbx cp .\@ARCHIVE@ asp:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
sbx cp .\@ARCHIVE@ devlore:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 devlore:/tmp/@ARCHIVE@.sha256
```

2. 各SandboxのBashで同じarchiveを存在確認・検証し、新規private先へ展開します。

```bash
cd /tmp
archive='@ARCHIVE@'
test -f "$archive" && test -f "$archive.sha256" && sha256sum -c "$archive.sha256" &&
kit=$(mktemp -d "$HOME/unreal-common.XXXXXXXX") &&
tar -xzf "$archive" -C "$kit" &&
cd "$kit/@KIT_ROOT@"
```

通常Codexの場合は `bash development.sh`。Docker OAuthの場合のみ
`bash development.sh --docker-sandbox-oauth` を明示し、検証済みHTTPS_PROXYを
保ってください。不正proxyは直接通信へfallbackしません。
Claude Teamは各環境のmanaged policyを監査したうえで、
`bash development.sh --policy-audited --trust-managed-policy`。
両方の認証経路を使う場合は3つのopt-inを同時指定できます。
未監査ならtrustを付けず、Claude Teamは利用可能と偽りません。
外部CLIが認証を所有し、installerは資格情報を読み込み/コピー/変更しません。

既知の旧Pilot wrapperを移行する場合のみ、上の導入コマンドへ
`--replace-pilot-launcher` を加えます。exact owned wrapperだけをprivate backup
付きで置換し、無関係なunrealは拒否します。旧config・Session・kitは残します。
installerは親shellのalias/functionを消せません。新しいBashを開き
`type -a unreal`、必要なら `"$HOME/.local/bin/unreal"` で確認してください。

```bash
cd ~/workspace/your-existing-project
unreal development
# Ctrl+Dでdetach後、同じrepoで
unreal development
```

workspaceは起動したGit worktreeのroot。repoごとに新しい共通namespaceの
config/state/socketを分離し、同名Sessionを混同しません。最初だけ既存の
provider chooserを使い、起動後 `/model` でProvider→Model→Effortを選びます。
1 providerならProvider段を省略。model/effortの事前指定は不要です。
通常bootstrapのRead Onlyと人間のAsk/Full Access確認を使います。
Full AccessはOS sandboxではなくambient権限です。Git公開は別の承認が必要。

runtimeは既存の通常launcherがprivate atomic作成します。
`~/.config/unreal-agent-common/repos/r-*/native/unreal-agent/runtime.json`、
stateは `~/.local/state/unreal-agent-common/r-*/`。
`unreal --workspace-info` でrepo/config/state/socket、`unreal --readme` でこの説明。
`bash doctor.sh` はchecksum、arch/glibc、4 binaryを非推論検査します。
資格情報の存在は診断のみで、認証済みと宣言しません。

異版Hostはbuild/protocol identityで拒否し、自動停止しません。
旧履歴を保った別起動は `unreal --instance repair-v2 development`。
古い同名Hostを明示停止して更新する場合は、その専用PID/config/socketを
利用者が確認してください。Session security/config identityは緩和しません。
復帰は従来の `codex` / `claude` コマンド。既存PATH・CLI設定・認証・Gate環境を
変更せず、新規共通wrapperだけを使わなくできます。

## 合成workspaceでの受入（実機は別途承認後）

`unreal --acceptance-fixture repair` は新規Git fixture（約5KiBのsample.txt）を
作ってそのworkspaceで起動します。実プロジェクトを探索・変更しません。
最初に `/model`、`/permissions` を確認し、人間が必要な権限を明示承認します。

送るプロンプト：
「sample.txtをreadし、受信したwhole-file revisionで1行だけeditしてください。
再readし、Bashで行数とgit diffを検証。結果を観測してからFinalを返してください。
commit/pushは禁止です。」

canonical read/edit/Bash receipts、実ファイル、TPS/scroll、detach/attach履歴を
確認します。秘匿stub・拒否・未知状態を成功と推測しないこと。
通常のRead Only試験、Ask拒否/Allow Once、Full取消、専用Host再起動後のRead Only
も確認してください。この手順自体は実推論を自動送信しません。
