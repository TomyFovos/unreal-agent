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
   新規導入時は利用可能なサブスクを優先し、両方使えれば Claude、片方ならそのサブスクで始まります。API 接続は `/model` で明示的に選びます。委譲には Unreal の子 Session を使います。メインエージェントは利用者の指示で `unreal-agent subagent model show/set` を使い、`all` または `lead`・`worker`・`collector` ごとに接続先、モデル、effort を設定・参照できます。保存済み設定のない役割では、OpenAI API の親は GPT の子に同じ API 接続を使い、他の親は Codex のサブスクを使います。選択した接続が利用できない場合は復旧手順を案内します。
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

### 子 Session の同時実行上限

`bash development.sh --max-active-children 10` で Session ツリーごとの上限を設定します。共通ランチャーは更新後も設定を保持し、起動時に workspace の `runtime.json` へ `MaxActiveChildren` を書き込みます。既定値と明示的な `0` は上限なしです。独自の `--config` を使う場合は、そのファイルに設定します。

runtime ファイルを再読込する Host では、子の受付時と空き枠を待つ間にこの設定を読み直します。それ以外の Host では起動時の値を使います。値を下げると、稼働中の子は継続し、新たな子は空きができるまで待機します。旧版の Host には新しいビルドでの再起動が必要です。

## Mac: どのフォルダからでも `unreal NAME`

macOS では、この Git チェックアウトでリビジョンをインストールします（`REV` は必須で、完全または短縮のコミットハッシュ、あるいはタグやブランチなど Git がコミットに解決できるリビジョンを指定できます）。

```bash
make mac-install REV=<commit>
```

ビルドは分離した worktree で行われ、`~/.local/opt/unreal-agent/<rev>` に配置されます。`current` リンクがそこへ切り替わり、`~/.local/bin/unreal` が書き込まれます。新しい版、それが置き換えた版、実行中のプロセスが使っている版は残し、それ以外の版は削除します。

`~/.local/bin` が `PATH` に無い場合は、表示された1行を `~/.zshrc` に追加してシェルを開き直します。

```bash
export PATH="$HOME/.local/bin:$PATH"
```

あとはどのフォルダからでも実行できます。

```bash
unreal NAME             # そのフォルダで Session NAME を開く（どのフォルダにも無ければ作る）
unreal                  # 全フォルダの Session を一覧し、番号で開く
unreal --where NAME     # Session NAME があるフォルダを表示する
```

（`unreal-agent folders ...` がラッパー無しの同じサブコマンドです。）

フォルダは、カレントディレクトリを含む Git worktree のルート（Git の外ならそのディレクトリ）で、シンボリックリンクは解決されます。フォルダごとに1つの Host が動き、設定 `~/.config/unreal-agent/folders/<ID>/runtime.json` と状態 `~/.local/state/unreal-agent/folders/<ID>/` を持ちます（`<ID>` は `r-` + フォルダパスの SHA-256 の先頭16桁の16進）。既定フォルダ（`~/.config/unreal-agent/runtime.json` の `Runtime.Workspace`）はそのファイルと `~/.local/state/unreal-agent` をそのまま使い、追加フラグ無しで起動します。Claude のログイン、OpenCode Go の API キー、`unreal-agent subagent model` の選択は全フォルダで共有されます。Claude のサンドボックスはその Host の socket だけを許可するので、サブエージェント CLI も動きます。

以前のリビジョンに戻すには `make mac-install REV=<older commit>` を実行します。

入れ直しや戻しでは、動いている Host は替わりません。`unreal NAME` はその Host につながり、Host は起動したときの版のまま動きます。切り替えるには、すべての Session を切り離し（`Ctrl+D`）、`pkill -f 'unreal-agent serve'` で Host を止めてから `unreal NAME` を実行します。Host は入れた版で起動し直します。会話と設定はそのまま残ります。動いている子は、Host が止まると取り消されます。

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

Sandbox が UTC の場合は、手順 2 の `development.sh` に `--time-zone Asia/Tokyo` を追加します。日本以外では現地の IANA タイムゾーン名を指定します。設定は更新後も引き継がれ、起動シェルで `TZ` を指定するとそちらが優先されます。

> **参考:** 転送には Docker 公式の [`sbx cp`](https://docs.docker.com/reference/cli/sbx/cp/) と [`sbx exec`](https://docs.docker.com/reference/cli/sbx/exec/) を使用します（既存の Sandbox が対象であり、インストーラが Sandbox 自体を新規作成することはありません）。

---

## アップデート手順

1. 新しいバージョンの `.tar.gz` と `.sha256` をダウンロードします。
2. [ステップ 2](#ステップ-2-共通キットのダウンロードとインストール) と同じコマンドブロックを実行します。
3. 既存のセッションを通常通り `unreal <セッション名>`（例: `unreal development`）で開きます。

アップデート後に Session を開くと、workspace の Host がアイドルなら新しいビルドで再起動し、**これまでの会話履歴と設定を引き継ぎます**。いずれかの Session でモデルターン、操作、子エージェントが動いていれば、更新を知らせて稼働中の Host と同じビルドの TUI で接続します。Host がアイドルになってから Session を開き直すと更新が適用されます。

すぐ切り替えるには `unreal development --restart-host` を実行します。作業を中断する前に、直接の子エージェント、その他の操作、稼働中の Session の件数を表示して確認を求めます。子から起動した下位のエージェントにも影響します。この機能より前の Host は workspace 全体のアイドル状態を確認できないため、確認付きの明示再起動で切り替えます。

インストーラは現在の版、1つ前の版、実行中のプロセスが使っている版を保持します。

### インストーラ (`development.sh`) のオプションフラグ

セットアップ時や更新時にプロバイダ構成を明示したい場合は、`bash development.sh` に以下のオプションを渡せます（導入後の `unreal` コマンドではなくインストール時に指定します）。

| オプション | 内容 |
| --- | --- |
| `--time-zone <IANA-zone>` | Unreal の表示に使うタイムゾーンを保存する。日本の場合は `Asia/Tokyo` |
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

### OpenCode Go のキーと配布時の選択

有効版は `bash development.sh --opencode-go-api-key-file /absolute/go.key`（0600 のファイル）、または `--opencode-go-api-key-env MY_GO_KEY`（環境変数の名前）も受け付けます。キーを画面に出さず管理ストアへ保存し、利用時には `/model` で OpenCode Go を明示選択します。

保存先は TUI の `/login` と同じ `~/.local/state/unreal-agent/credentials` です。
`XDG_STATE_HOME` が設定されていれば `$XDG_STATE_HOME/unreal-agent/credentials` を使います。
共通ランチャーはこの親ディレクトリを `--shared-state-directory` で渡し、同じ環境の全ワークスペース・instance で
キーと `subagent-model.json` のロール初期値を共有し、モデル選択は Session ごとに保存します。Mac のランチャーも同じ保存先です。
旧リポジトリ別モデル設定は共有ファイルがないときに起動時に移行し、元ファイルを残します。
共有済みの選択は上書きしません。Session・ログの保存先は従来どおりです。[詳細](../opencode-go.md#サブエージェントのモデルを選ぶ)。

`KIT.json` の `opencode_go_enabled` が `false` の配布物は、どちらのキー指定も「この配布物では OpenCode Go は使えません」と案内して拒否します。`unreal-agent-auth` でのキー登録も拒否し、環境変数や設定ファイルでは有効に戻せません。以前の Session は選択を保持して開き、`/model` でほかの接続を選ぶよう案内します。実行ファイル自体の判定は `unreal-agent --version` で確認できます。変更にはソースからの再ビルドが必要です。[配布手順](distribution.md#opencode-go-を無効にした配布物)を参照してください。
