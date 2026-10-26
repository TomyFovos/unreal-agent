# インストール手順

Unreal Agent は、Claude（公式 Claude Code）と Codex を同じ `/model` から選んで使える
コーディングエージェントです。Claude を設計者、GPT（Codex）を実装者として、
Claude から GPT のサブエージェントを動かせます。

## 必要なもの

- Linux x86_64（WSL2、Docker Sandboxes の Sandbox を含む）。`git`、`python3`、`tar`、`sha256sum`
- **Claude:** Claude Code CLI（`claude`）にサブスクリプションでログイン済みであること
  （`claude auth login`）。API キーは使いません。
- **Codex:** 次のどちらか
  - Codex CLI にログイン済み（`codex login`。`~/.codex/auth.json` を使います）
  - Docker Sandboxes の Codex（ホストの OAuth）。Windows 側で
    `sbx secret set openai --oauth` を済ませておきます。

片方だけでも動きます。使えない方は `/model` に出ません。

## 1. ダウンロード

[Releases](https://github.com/TomyFovos/unreal-agent/releases) から
`unreal-agent-linux-amd64-<版>.tar.gz` と `.sha256` を同じディレクトリに置きます。

Linux で直接取得する場合（`<版>` は Release のタグに付いた短いリビジョンです）:

```bash
v=v0.3.0; r=<版>
cd /tmp
curl -fLO "https://github.com/TomyFovos/unreal-agent/releases/download/$v/unreal-agent-linux-amd64-$r.tar.gz"
curl -fLO "https://github.com/TomyFovos/unreal-agent/releases/download/$v/unreal-agent-linux-amd64-$r.tar.gz.sha256"
```

Docker Sandboxes へ Windows からコピーする場合（PowerShell。`devlore` は Sandbox 名）:

```powershell
sbx cp .\unreal-agent-linux-amd64-<版>.tar.gz devlore:/tmp/
sbx cp .\unreal-agent-linux-amd64-<版>.tar.gz.sha256 devlore:/tmp/
sbx exec -it devlore bash
```

## 2. インストール

```bash
cd /tmp && archive=unreal-agent-linux-amd64-<版>.tar.gz &&
sha256sum -c "$archive.sha256" &&
kit=$(mktemp -d "$HOME/unreal-common.XXXXXXXX") &&
tar -xzf "$archive" -C "$kit" &&
cd "$kit/unreal-agent-linux-amd64-<版>" &&
bash development.sh --configure-providers --replace-pilot-launcher
```

Docker Sandboxes の Codex（ホストの OAuth）を使う場合は、最後に
`--docker-sandbox-oauth` を付けます。成功すると `PASS: common unreal installed` と、
使えるプロバイダー（`claude: available` など）が表示されます。

`unreal` は `~/.local/bin` に入ります。同じ手順で上書きすれば更新です。
以前の Host と Session は残り、新しい版は新しいインスタンスとして起動します。

## 3. 起動

Git の作業ツリーで実行します（引数は Session 名）。

```bash
cd /path/to/your/repo
"$HOME/.local/bin/unreal" development
```

## 主な操作

| 操作 | 内容 |
|---|---|
| `/model` | プロバイダー、モデル、effort を選ぶ（Claude で始まります） |
| `/permissions` | Read Only（初期）、Ask、Full Access を選ぶ。Read Only でも読むだけのコマンド（git log など）は実行できます |
| `/stop` | サブエージェントだけ、メインだけ、両方、Session を止める |
| `/view` | Chat、Orchestration、Split（初期。120桁以上） |
| `/copy`、`/export` | 会話をクリップボードへコピー、Markdown へ出力 |
| Ctrl+D | 抜ける（Session は動き続けます） |

Claude に「Codex に調べさせて」「実装統括に実装させて」と頼むと、GPT の作業者や
実装統括（gpt-6.1-sol ultra）をサブエージェントとして起動します。
子の承認は親のエージェントが行います。

## 注意

- Full Access は OS の隔離ではありません。Git の push などは別に承認してください。
- 詳細: [Claude の経路](claude-route.md)、[権限](permissions.md)、
  [Docker Sandboxes の Codex OAuth](docker-sandbox-oauth.md)
