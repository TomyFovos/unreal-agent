# @SANDBOX@ 日常開発用 Unreal Agent

Linux amd64 / CGO / tree-sitter、4 binary、revision `@REVISION@`。
既存のCodex/Claude CLI・認証・Gate D/E・Sessionは削除・移行しません。
導入時は診断と認証preflightだけで、Hostや推論を開始しません。
必要環境: Bash、Python 3.11以上、Git、glibc @MIN_GLIBC@以上。

1. Windowsから転送（このフォルダをカレントにする）。ホストの`sbx`利用可否は未確認です。

   ```powershell
   sbx cp @ARCHIVE@ @SANDBOX@:/tmp/@ARCHIVE@
   sbx cp @ARCHIVE@.sha256 @SANDBOX@:/tmp/@ARCHIVE@.sha256
   ```

2. Sandbox Bashで一括インストール。

   ```bash
   set -eu
   cd /tmp
   sha256sum -c @ARCHIVE@.sha256
   daily_unpack=$(mktemp -d /tmp/unreal-daily.XXXXXXXX)
   tar -xzf @ARCHIVE@ -C "$daily_unpack"
   bash "$daily_unpack/@KIT_ROOT@/daily.sh" install --model @MODEL@ --effort high --replace-pilot-launcher@OPT_IN@
   ```

`--replace-pilot-launcher`は検証済みの既存日常wrapper、または旧devlore
launcher-v1 wrapperの更新を明示承認するオプションです。任意の既存`unreal`、
symlink、alias、functionは置換しません。既存コマンドの競合は停止して案内します。
旧wrapperと日常launcher設定の更新前内容は、専用configの`backups/`に保持します。
旧Pilotバイナリ・設定・historyも残します。現在のBashに旧`unreal` functionがある場合は、
導入後に新しいBashを開いてください。既存PATHの内容は維持し、必要な場合だけ
`~/.local/bin`を認識するガード付き設定を既存Bash startupに追記します。

devloreの`--policy-audited --trust-managed-policy`は既存の管理policy監査と明示trustを
確認した人間だけが指定します。管理policyは外部の管理境界として維持し、
Unrealがその安全性を検証したとは主張しません。Claude未導入・未ログイン・未監査は停止します。
aspは既存Docker OAuthと許可済みHTTPS_PROXYを必須とし、直接通信やAPI keyへfallbackしません。
認証ファイルはパス参照のみ。wrapperは認証内容を読み取らず、変更・複製しません。

## 起動とworkspace確認

新しいBashを開き、**自分で選んだGit repo**へ移動してください。
`~/workspace`自体がGit repoである必要はありません。

```bash
command -v unreal
cd ~/workspace/my-project
unreal --workspace-info development
unreal development
```

`unreal SESSION_NAME`でSession名を指定し、`unreal`だけなら`default`です。起動前は短いQuick Startと明示Enterのみ。
全文は`unreal --readme`で参照できます。非対話環境はEnter待ちをしません。
起動自体はユーザーメッセージを投入せず、モデル推論もしません。
repo外・bare repo・Git workspace override・private storage内のrepoは停止し、
対象repoへcdするよう案内します。サブディレクトリやsymlinkからも実Git rootへ解決します。

rootのcanonical pathのSHA-256からrepo識別子を作り、sandbox別に保存先を分離します。
同名Sessionでも別repoなら別Host・別historyとなります。cloneやGit worktreeも別workspaceです。
path短縮による衝突は完全hashとrootのbinding照合でfail-closedにします。
repoを移動すると別namespaceになります。旧historyを変更・移行はしません。

| 保存先 | 内容 |
| --- | --- |
| `~/.config/unreal-agent-dev/@SANDBOX@/launcher.json` | 認証参照・初期モデル・kit参照 |
| `~/.config/unreal-agent-dev/@SANDBOX@/repos/r-<hash>/` | repo固有runtimeとimmutable binding |
| `~/.local/state/unreal-agent-dev/@SANDBOX@/r-<hash>/` | Session、Host log、run/host.sock |
| `~/.local/opt/unreal-agent-pilot/@SANDBOX@/` | checksum付きの別世代binary |

`--workspace-info`は選択rootとconfig/state/socketを表示し、Host・推論は開始しません。
新規repoのprivate設定はこの時点でも安全に作成します。既存repo設定のモデル・権限・denyは
更新で書き換えません。新しい初期モデル設定は新規repoへだけ適用します。
同じrepoで`/model`変更後にattach/resumeしてもcanonical RuntimeSelectionを維持します。
稼働中Hostは設定とSessionを保有し、kit更新で自動停止しません。更新binaryの適用に
Host停止が必要な場合は、そのrepoのsocket/PID/serve引数を確認してSIGTERMしてください。
認証・config・Sessionディレクトリは削除しません。

## Permission / TUI

初期値は **Read Only**。`/permissions`でAskのAllow Once / Deny、Full Accessの
人間による別確認、Read Onlyへの取消を使用できます。Full Accessを自動有効化しません。
Host再起動後は昇格失効・再承認。上位denyや子権限の制限は既存契約を維持します。

Full AccessはOS sandboxではありません。Bashはambient accessで実行され、認証情報・
共有マウント・ネットワークへ技術的にアクセス可能です。ネイティブfile toolの
immutable workspace境界とは別物です。Full AccessをGit push、PR、merge、tag、Releaseの
運用承認とみなしてはいけません。モデルへ渡す指示でも明示しておいてください。

`/model`でprovider内のmodel・effortを選択。ClaudeはStructured Bridgeのみ。
MCP・side-effecting built-in tool・provider fallbackは使用しません。
mouse wheel、PgUp/PgDn、Shift+dragによるterminal側選択、入力欄下のTPS/usageは既存のまま。
不明なTPS/usageは`—`。Ctrl+DはdetachだけでHost・historyを保ちます。

## 最初の開発確認

小さな変更内容は人間が選びます。勝手にAIdeaや他repoをテスト変更しません。
最初は次のread-only指示を送れます。

```text
このSessionのworkspaceをUnrealのread/glob/grepだけで調べてください。
AGENTS.md等、README、構成、該当テストを確認し、変更候補を報告してください。
まだ編集・Bash・commit・pushをしないでください。
```

Git statusにもBashの権限が必要です。Askを選んで個別承認、またはFull Accessを
人間が明示確認した後、次の指示の`<小さな変更>`を実際の依頼へ置き換えます。

```text
まずgit status --shortを実行し、既存変更を保護してください。
このrepoで <小さな変更> だけ実装してください。Unreal write/edit/readを使い、
対応するunit test/buildをBashで実行、最後にgit diffを確認してください。
観測したreceiptとテスト結果だけを報告。commit/push/PR/merge/tag/Releaseは禁止。
```

Ctrl+Dの後、同じrepoで`unreal development`を再実行し、会話と変更が残っていることを確認。
Operation/receiptは`/view orchestration`、初期workspaceとruntimeは`/analyze Overview`で確認。
必要に応じて`/export`を使用。Permission拒否は実際のdenied receiptで確認し、
モデル文章だけの拒否を合格証拠にしません。

Codex CLIはそのまま残ります。問題があればUnrealをdetachし、従来の`codex`で作業を継続。
これらの設定はSandbox固有です。WSL fakeテストは実機認証・通信・推論のPASSではありません。
