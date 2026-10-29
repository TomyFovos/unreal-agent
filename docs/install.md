# インストール手順

Unreal Agent は、Claude（公式 Claude Code）と Codex を同じ `/model` から選んで使える
コーディングエージェントです。**Codex か Claude のどちらか一方が使えれば始められます。**
Claude を設計者、GPT（Codex）を実装者として、
Claude から GPT のサブエージェントを動かせます。

## 必要なもの

- Linux x86_64（WSL2、Docker Sandboxes の Sandbox を含む）。`git`、`python3`、`tar`、`sha256sum`
- **Claude:** Claude Code CLI（`claude`）にサブスクリプションでログイン済みであること
  （`claude` を起動し、その中で `/login`）。API キーは使いません。
- **Codex:** 次のどちらか
  - Codex CLI にログイン済み（`codex login`。`~/.codex/auth.json` を使います）
  - Docker Sandboxes の Codex（ホストの OAuth）。Windows 側で
    `sbx secret set openai --oauth` を済ませておきます。

片方だけでも動きます。使えない方も `/model` に状態と準備手順が表示されます。
CLI がなければ、公式の Codex CLI / Claude Code を入手してください。
Unreal 内の `/login` で手順を確認できます。API キーの登録は行いません。

## 1. ダウンロード

[Releases](https://github.com/TomyFovos/unreal-agent/releases) から
`unreal-agent-linux-amd64-<版>.tar.gz` と `.sha256` を同じディレクトリに置きます。

Linux で直接取得する場合（`<版>` は Release のタグに付いた短いリビジョンです）:

```bash
v=v1.0.0; r=<版>
cd /tmp
curl -fLO "https://github.com/TomyFovos/unreal-agent/releases/download/$v/unreal-agent-linux-amd64-$r.tar.gz"
curl -fLO "https://github.com/TomyFovos/unreal-agent/releases/download/$v/unreal-agent-linux-amd64-$r.tar.gz.sha256"
```

Docker Sandboxes へ Windows からコピーする場合（PowerShell。`<sandbox>` は Sandbox 名）:

```powershell
sbx cp .\unreal-agent-linux-amd64-<版>.tar.gz <sandbox>:/tmp/
sbx cp .\unreal-agent-linux-amd64-<版>.tar.gz.sha256 <sandbox>:/tmp/
sbx exec -it devlore bash
```

## 2. インストール

```bash
cd /tmp && archive=unreal-agent-linux-amd64-<版>.tar.gz &&
sha256sum -c "$archive.sha256" &&
kit=$(mktemp -d /tmp/unreal-common.XXXXXXXX) &&
tar -xzf "$archive" -C "$kit" &&
cd "$kit/unreal-agent-linux-amd64-<版>" &&
bash development.sh --configure-providers --replace-pilot-launcher
```

Docker Sandboxes の Codex（ホストの OAuth）を使う場合は、最後に
`--docker-sandbox-oauth` を付けます。「インストール完了」と接続先ごとの利用状態が表示され、
使えない側にだけログイン方法が出ます。両方使えない場合は、どちらかにログインしてから
`unreal` を起動してください。

`unreal` は `~/.local/bin` に入ります。同じ手順で上書きすれば更新です。
更新後に起動すると、動いていた Host を新しい版で起動し直し、Session と設定はそのまま引き継ぎます。
インストール時には今回の版と直前の版を残して古い版を削除し、削除した版と解放容量を1行で表示します。
実行中のプロセスが使う版も残します。キットの展開先は `/tmp` の一時ディレクトリです。

## 3. 起動

作業したいフォルダで実行します（引数は Session 名）。Git の作業ツリーの中ならその最上位、
それ以外ならそのフォルダが作業範囲になります（Codex と同じ）。

```bash
cd /path/to/your/repo
"$HOME/.local/bin/unreal" development
```

## 主な操作

| 操作 | 内容 |
|---|---|
| `/model` | プロバイダー、モデル、effort を選ぶ（両方使える初回は Claude、一方だけなら使える側で開始） |
| `/login` | サブスクでのログイン方法を案内 |
| `/model refresh` | 外部 CLI のログインとモデル一覧を再検出。未登録の接続先も追加 |
| `/permissions` | Read Only（初期）、Ask、Full Access を選ぶ。Read Only でも読むだけのコマンド（git log など）は実行できます |
| `/stop` | サブエージェントだけ、メインだけ、両方、Session を止める |
| `/view` | Chat、Orchestration、Split（初期。120桁以上） |
| `/copy`、`/export` | 会話をクリップボードへコピー、Markdown へ出力 |
| Ctrl+D | 抜ける（Session は動き続けます） |

Claude に「Codex に調べさせて」「実装統括に実装させて」と頼むと、GPT の作業者や
実装統括（gpt-6.1-sol ultra）をサブエージェントとして起動します。
子の承認は親のエージェントが行います。

ログイン後は `/model refresh`、または `/model` で使えない側を選ぶと再検出します。
開き直す必要はありません。接続先が使えなくなった場合は案内に従ってログインし直すか、
停止中でも `/model` で切り替えて `/resume` できます。

## 入力のコツ

- **改行:** Ctrl+Enter（Shift+Enter、Alt+Enter、Ctrl+J も可）。どの端末でも使える方法として、行末に `\` を入れて Enter。
- **貼り付け:** 数行以上は `[Pasted text #1 +N lines]` と表示され、送信時に全文が送られます。

## ファイルを Sandbox に渡す（Windows）

`scripts/windows/sandbox-drop.cmd` を、Sandbox 名に合わせて `devlore.cmd` のように名前を変え、
`shell:sendto`（右クリックの「送る」メニュー）に置くと、ファイルを右クリック →「送る」→ Sandbox 名で送れます。
ファイルをその `.cmd` にドラッグ＆ドロップすると、Sandbox の `/home/agent/drop/` にコピーされ、
パスがクリップボードに入るので Unreal に貼り付けます。先に送っておけば、端末へ直接ドラッグした
Windows のパスも Sandbox 内のパスに読み替えます。Claude は Read Only でも `~/drop` のファイルを読めます。

## 注意

- Full Access は OS の隔離ではありません。
- 詳細: [Claude の経路](claude-route.md)、[権限](permissions.md)、
  [Docker Sandboxes の Codex OAuth](docker-sandbox-oauth.md)
