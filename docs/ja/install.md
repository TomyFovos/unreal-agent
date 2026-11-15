# インストール (Install)

**言語:** [English](../install.md) | 日本語

Unreal Agent の共通キットは **Linux amd64**（WSL および Linux Docker Sandbox を含む）に対応します。1つのキットで Claude・ChatGPT のサブスクログインと、任意の OpenAI・Anthropic API キー接続を使えます。

## 動作要件

- **OS / アーキテクチャ:** Linux amd64（最低 glibc バージョンは `KIT.json` に記録され、インストール時に `doctor.sh` が自動検査します）
- **必要なコマンド:** `bash`, `python3`, `git`, `tar`, `sha256sum`
- **接続の準備:** サブスクのログイン、または明示的に選んだ API キーを使います。Claude はどちらの認証でも公式 `claude` CLI が必要です。OpenAI API に Codex のサブスクログインは不要です。ChatGPT は `codex login` を使い、有効な認証とモデル一覧があれば CLI なしでも推論できます。Docker Sandbox のホスト管理 OAuth も利用できます。

---

## ステップ 1: プロバイダへのログイン準備

サブスク接続では、Unreal を動かす Linux 環境内で利用したいプロバイダにログインします。API キーで使う場合は、この下の任意登録へ進んでください。サブスクのログインは不要です。

- **Claude を使う場合:** 公式の Claude Code CLI をインストールし、Bash で `claude` を起動して `/login` を実行します（Claude サブスクリプションでログイン）。
- **Codex を使う場合:** 公式の Codex CLI をインストールし、Bash で `codex login` を実行します（ChatGPT サブスクリプションでログイン）。
- **Docker Sandbox のホスト管理 Codex OAuth を使う場合:** ホスト側で OAuth ログインを準備します。詳しくは [プロバイダ設定 — Docker Sandboxes Codex OAuth](providers.md#docker-sandboxes-codex-oauth) を参照してください。

> **補足:** Unreal 内の `/login` はサブスクのログイン手順を表示します。両方のサブスクが未準備でもインストールは成功（終了コード `0`）します。後からログインしたら `/model refresh` を実行し、接続を選びます。インストールのやり直しは不要です。明示的に無効化したサブスク接続は、Codex は `bash development.sh --configure-providers --codex-auto`、Claude は `bash development.sh --configure-providers` で再び有効にします。

### 任意: API キー接続を登録する

キーを入れたファイルを **0600** にし、インストール時に片方または両方のフラグを追加します。

```bash
chmod 600 /absolute/openai.key /absolute/anthropic.key
bash development.sh --openai-api-key-file /absolute/openai.key --anthropic-api-key-file /absolute/anthropic.key
```

設定に保存するのはファイルパスだけで、キーの値は保存しません。0600 でないファイルは拒否します。環境変数を使う場合は、`--openai-api-key-env NAME` または `--anthropic-api-key-env NAME` で変数名を明示します。

**API はサブスクとは別課金です。** 環境変数 `OPENAI_API_KEY` や `ANTHROPIC_API_KEY` があるだけでは使いません。登録だけでは API を選択せず、起動後に `/model` の `OpenAI API（API キー・別課金）` または `Claude（API キー・別課金）` を選びます。API しか登録されていない場合も同じです。Session は選択を維持し、サブスクの認証切れでも API キーへ自動では切り替えません。初期接続の明示指定や `runtime.json` の参照先は [プロバイダ設定](providers.md#任意の-api-キー接続) を参照してください。

---

## ステップ 2: 共通キットのダウンロードとインストール

1. [GitHub Releases](https://github.com/TomyFovos/unreal-agent/releases) から `unreal-agent-linux-amd64-REV.tar.gz` と `unreal-agent-linux-amd64-REV.tar.gz.sha256` の2ファイルを同じフォルダにダウンロードします（`REV` は7文字のコミットハッシュ）。
2. Bash で以下のブロックを貼り付けて実行し、プロンプトが出たらダウンロードした `.tar.gz` ファイルのフルパスを入力します（展開前にチェックサムを自動検証します）。

   ```bash
   set -e
   read -r -e -p 'Full path to the kit .tar.gz: ' unreal_archive
   unreal_archive=$(realpath -- "$unreal_archive")
   cd -- "$(dirname -- "$unreal_archive")"
   sha256sum -c "$(basename -- "$unreal_archive").sha256"
   unreal_unpack=$(mktemp -d /tmp/unreal-common.XXXXXXXX)
   tar -xzf "$unreal_archive" -C "$unreal_unpack"
   cd -- "$unreal_unpack/$(basename -- "$unreal_archive" .tar.gz)"
   bash doctor.sh
   bash development.sh --configure-providers --replace-pilot-launcher
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```

3. インストール完了後に表示される接続状態を確認します。
   新規導入時は利用可能なサブスクを優先し、両方使えれば Claude、片方ならそのサブスクで始まります。API 接続は `/model` で明示的に選びます。Claude は Codex が使えれば GPT の子、使えなければ Claude Code の Agent（`haiku` / `sonnet`）を使います。OpenAI API を統括に選ぶと組み込みの GPT の子も API を使い、他の統括では Codex を維持します。認証が使えなくても別接続へ切り替えません。
4. 開発したいプロジェクトのディレクトリへ移動し、名前付きセッションを起動します。

   ```bash
   unreal development
   ```

   新規作成の確認が表示されたら `y` を入力します。

### インストールされるもの

- **実行コマンド:** `~/.local/bin/unreal` および `~/.local/bin/unreal-agent`
- **シェル設定:** `~/.config/unreal-agent-common/path.sh`（新しい Bash シェル起動時に読み込まれ、`PATH` の追加と `ucd` 関数を提供します）
- **ワークスペースの基準ディレクトリ:** Git 管理下では Git ワークツリーのルート、Git 外では現在のディレクトリが自動的にワークスペースになります。

---

## Docker Sandbox への転送とインストール（Windows から）

Windows 上で既存の Linux Docker Sandbox を使っている場合は、以下の手順でキットを転送して導入できます。

1. **Windows PowerShell から Sandbox へファイルを転送する:**
   `.tar.gz` と `.tar.gz.sha256` を同じフォルダに置いた状態で、**Windows PowerShell** で以下を実行します。

   ```powershell
   $unrealArchive = Get-Item -LiteralPath (Read-Host 'Full path to the kit .tar.gz')
   $unrealSandbox = Read-Host 'Existing Sandbox name'
   sbx cp $unrealArchive.FullName "${unrealSandbox}:/tmp/$($unrealArchive.Name)"
   if ($LASTEXITCODE -ne 0) { throw 'Archive transfer failed' }
   sbx cp "$($unrealArchive.FullName).sha256" "${unrealSandbox}:/tmp/$($unrealArchive.Name).sha256"
   if ($LASTEXITCODE -ne 0) { throw 'Checksum transfer failed' }
   sbx exec -it $unrealSandbox bash
   ```

2. **Sandbox 内の Bash でインストールを実行する:**

   ```bash
   set -e
   cd /tmp
   read -r -p 'Transferred kit filename (.tar.gz): ' unreal_archive
   sha256sum -c "$unreal_archive.sha256"
   unreal_unpack=$(mktemp -d /tmp/unreal-common.XXXXXXXX)
   tar -xzf "$unreal_archive" -C "$unreal_unpack"
   cd -- "$unreal_unpack/${unreal_archive%.tar.gz}"
   bash doctor.sh
   bash development.sh --configure-providers --replace-pilot-launcher
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```

3. Sandbox 内のプロジェクトディレクトリに移動し、`unreal development` を起動します。

> **参考:** 転送には Docker 公式の [`sbx cp`](https://docs.docker.com/reference/cli/sbx/cp/) と [`sbx exec`](https://docs.docker.com/reference/cli/sbx/exec/) を使用します（既存の Sandbox が対象であり、インストーラが Sandbox 自体を新規作成することはありません）。

---

## アップデート手順

1. 新しいバージョンの `.tar.gz` と `.sha256` をダウンロードします。
2. [ステップ 2](#ステップ-2-共通キットのダウンロードとインストール) と同じコマンドブロックを実行します。
3. 既存のセッションを通常通り `unreal <セッション名>`（例: `unreal development`）で開きます。

アップデート後にセッションを開くと、ラッパーが新しいビルドを自動検出してバックグラウンドの Host を再起動し、**これまでの会話履歴と設定をそのまま引き継ぎます**。古いバージョンは「現在」「1つ前」「実行中プロセスが使用中のもの」を残して自動的に整理されます。

### インストーラ (`development.sh`) のオプションフラグ

セットアップ時や更新時にプロバイダ構成を明示したい場合は、`bash development.sh` に以下のオプションを渡せます（導入後の `unreal` コマンドではなくインストール時に指定します）。

| オプション | 内容 |
| --- | --- |
| `--codex-auto` | 通常の Codex ログインと Docker Sandbox OAuth を自動判別（初回インストール時の既定値） |
| `--external-codex` | 外部 Codex ログインを明示選択（`OPENAI_CODEX_AUTH_FILE`、`$CODEX_HOME/auth.json`、`~/.codex/auth.json` の順） |
| `--docker-sandbox-oauth` | Docker Sandbox のホスト管理 OAuth プロキシ経路を明示選択 |
| `--disable-codex` | Codex プロバイダを無効化 |
| `--disable-claude` | Claude プロバイダを無効化 |
| `--claude-binary <path>` | `claude` 実行ファイルのパスを明示指定 |
| `--openai-api-key-file <path>` | 0600 のキーファイルで任意の OpenAI API 接続を登録（別課金） |
| `--anthropic-api-key-file <path>` | 0600 のキーファイルで任意の Claude API 接続を登録（別課金） |
| `--openai-api-key-env <NAME>` | OpenAI キーを読む環境変数名を明示指定 |
| `--anthropic-api-key-env <NAME>` | Anthropic キーを読む環境変数名を明示指定 |
