# インストール (Install)

**言語:** [English](../install.md) | 日本語

Unreal Agent の共通キットは **Linux amd64**（WSL および Linux Docker Sandbox を含む）に対応しており、1つのキットで **Claude だけ・ChatGPT（Codex）だけ・両方**の構成を利用できます。

## 動作要件

- **OS / アーキテクチャ:** Linux amd64（最低 glibc バージョンは `KIT.json` に記録され、インストール時に `doctor.sh` が自動検査します）
- **必要なコマンド:** `bash`, `python3`, `git`, `tar`, `sha256sum`
- **プロバイダのログイン:** Claude は公式 `claude` CLI、ChatGPT は `codex login` を使用します。Codex は有効な認証とモデル一覧があれば CLI なしでも推論でき、Docker Sandbox のホスト管理 OAuth も利用できます。

---

## ステップ 1: プロバイダへのログイン準備

Unreal を動かす Linux 環境内で、利用したいプロバイダにサブスクリプションでログインします。

- **Claude を使う場合:** 公式の Claude Code CLI をインストールし、Bash で `claude` を起動して `/login` を実行します（Claude サブスクリプションでログイン）。
- **Codex を使う場合:** 公式の Codex CLI をインストールし、Bash で `codex login` を実行します（ChatGPT サブスクリプションでログイン）。
- **Docker Sandbox のホスト管理 Codex OAuth を使う場合:** ホスト側で OAuth ログインを準備します。詳しくは [プロバイダ設定 — Docker Sandboxes Codex OAuth](providers.md#docker-sandboxes-codex-oauth) を参照してください。

> **補足:** Unreal の通常利用では API キーは使用しません。Unreal 内の `/login` はログイン手順を表示します。両方未準備でもインストールは成功（終了コード `0`）し、起動後の最初の入力にログインを案内します。後からログインしたら `/model refresh` を実行し、`/model` で選択します。インストールのやり直しは不要です。明示的に無効化した場合は、Codex は `bash development.sh --configure-providers --codex-auto`、Claude は `bash development.sh --configure-providers` で再び有効にします。

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

3. インストール完了後に表示される Claude / Codex の利用可能状態を確認します。
   新規導入時は両方使えれば Claude、片方ならそのプロバイダで始まります。Claude は Codex が使えれば GPT の子、使えなければ Claude Code の Agent（`haiku` / `sonnet`）を使います。GPT の統括は常に GPT の子を使い、Codex が利用できない場合はログインを案内します。
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
