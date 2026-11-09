# プロバイダ設定 (Providers)

**言語:** [English](../providers.md) | 日本語

Unreal Agent は1つのインストール環境と共通の `/model` メニューで **Claude だけ・ChatGPT（Codex）だけ・両方**の構成に対応しています。どちらも既存のサブスクリプションログインを使用し、プロバイダの自動切替や API キーへのフォールバックは行いません。

---

## モデルの選択と切り替え (`/model`)

1. TUI で `/model` を入力し、プロバイダ（`Claude` または `Codex`）を選択します。
2. 利用可能なモデルを選び、続けて推論の深さ（effort）を選択します。
3. 後からログインした場合や外部 CLI を更新した場合は、`/model refresh` で認証状態とモデル一覧を再検出し、`/model` で選択します。インストールのやり直しは不要です。既存 Session のプロバイダは自分で選び直すまで維持されます。

> **補足:**
> - 初回起動時は、両方使える場合は **Claude**、片方のみの場合は使える側のプロバイダが初期選択されます（環境変数 `UNREAL_HARNESS_LLM_PROVIDER` / `UNREAL_HARNESS_LLM_MODEL` で明示指定も可能）。
> - 未ログインのプロバイダもメニュー内に理由と準備手順付きで表示されます。
> - 両方未準備でも起動し、最初の入力にログイン手順を案内します。`/login` でも手順を確認できます。
> - 明示的に無効化したプロバイダはログインだけでは有効になりません。Codex は `bash development.sh --configure-providers --codex-auto`、Claude は `bash development.sh --configure-providers` で再び有効にします。
> - モデル一覧に表示されていても、実際に推論できるかはご自身のサブスクリプション契約プランに依存します。

---

## Claude（Claude Code CLI 連携）

### セットアップ手順

1. Unreal を実行する Linux 環境に公式の `claude` CLI をインストールします。
2. 別の Bash シェルで `claude` を起動し、Claude Code 内で `/login` を実行して Claude サブスクリプションでログインします。
3. Unreal に戻り、`/model refresh` を実行します。

### 仕組み

- **プロバイダ ID:** `claude`
- **会話とツールの管理:** Claude Code 自身が会話履歴と内蔵ツールを管理します。Unreal は `claude` CLI を起動してプロンプトを渡し、実行されたツールの結果を `claude_code` Operation として記録します。
- **権限と Bash フォールバック:** Claude Code の内蔵ツール呼び出しはすべて Unreal の承認フックを通過します。Claude Code 自身が Bash コマンドの実行を辞退した場合は、Unreal のネイティブ Bash 実行器が同じ権限モードのもとで引き継いで実行し、結果を Claude に返します。
- **モデル一覧:** Claude Code の初期化レスポンスから自動取得されます。
- **会話圧縮と指示書:** [コンテキストと指示書](context.md) を参照してください。

---

## Codex（通常のサブスクリプション経路）

### セットアップ手順

1. 公式の `codex` CLI をインストールし、Bash で `codex login` を実行して ChatGPT サブスクリプションでログインします。
2. 初回導入時などにモデル一覧が未作成と表示された場合は、一度 `codex` CLI を開いてローカルの `models_cache.json` を生成させます。
3. Unreal を起動するか、既存セッションで `/model refresh` を実行します。

### 仕組み

- **プロバイダ ID:** `openai-codex`
- **実行パイプライン:** Unreal 内蔵の Codex アダプタを通じてリクエストを送り、Unreal のネイティブファイルツール・AST ツール・Bash を実行します。
- **CLI の有無:** 選択した認証とモデル一覧が有効な間は、Codex CLI がなくても推論できます。ログインと認証の更新には公式 CLI を使います。
- **認証情報の管理:** OAuth トークンの保存と更新は外部の `codex` CLI が担います。Unreal は認証ファイルを読み取り専用で参照し、独自のキーストアへトークンをコピーしません。
- **認証ファイルの探索順序:**
  1. `--codex-auth-file` フラグ（ソースビルド版ランチャー）
  2. `OPENAI_CODEX_AUTH_FILE` 環境変数
  3. `$CODEX_HOME/auth.json`
  4. `$HOME/.codex/auth.json`
  ※ 通常のプライベートファイルである必要があり、同じフォルダ内の `models_cache.json` からモデル一覧を読み取ります。

- **モデル一覧の取得規則:** 通常 Codex のモデル探索は、選択した認証ファイルと同じディレクトリの `models_cache.json` の読み取りのみを行います（`/model refresh` 時にも CLI を起動して列挙することはしません）。モデル一覧の取得と認証・推論権限の検査は独立しています。

---

<a id="docker-sandboxes-codex-oauth"></a>
## Docker Sandboxes Codex OAuth

既存の **Docker Sandbox** 内で Unreal を動かす場合、Docker の認証フォワードプロキシを経由してホスト側の OpenAI OAuth ログインを安全に利用できます。

### セットアップ手順

1. **Docker ホスト側:** Sandbox ツールで OpenAI OAuth ログインを設定します。
   ```bash
   sbx secret set openai --oauth
   ```
   詳しくは [Docker 公式の Codex セットアップ手順](https://docs.docker.com/ai/sandboxes/agents/codex/) を参照してください。
2. **Sandbox 内:** [インストール手順](install.md) に従って共通キットを導入します。既定の自動判定（`--codex-auto`）でも認識されますが、明示的に指定する場合は `bash development.sh` に `--docker-sandbox-oauth` を追加します。
3. Unreal を起動し、`/model` で **Codex** を選択します。

### 検証条件とモデルキャッシュの仕組み

- **環境と設定の検査:** `SBX_CRED_OPENAI_MODE=oauth`、非秘匿の認証マーカーファイル、`.codex/config.toml` の `sandboxd` プロバイダ設定、および Codex エンドポイント URL を検査します（設定内の `forced_login_method = "api"` はプロキシ経路用の指定であり、API キー課金にはなりません）。
- **フォワードプロキシ:** 環境変数 `HTTPS_PROXY`（または `https_proxy`）に `http://host.docker.internal:3128` または `http://gateway.docker.internal:3128` が設定されている必要があります（大文字・小文字で異なる値が設定されている場合は拒否されます）。プロキシが通信時にホストの認証情報を付与するため、Unreal がホストのトークンを読み取ったり証明書を導入したりすることはありません。
- **モデル一覧の取得とキャッシュ:**
  - まず `models_cache.json` を確認し、存在しない場合は `codex app-server` のメタデータ取得（`model/list`）のみを実行します。取得結果は `~/.cache/unreal-agent/codex-cli-catalog.json` に保存され、24時間再利用されます。
  - `models_cache.json` がない状態で `/model refresh` を実行すると、保存済み CLI カタログのキャッシュを迂回して再取得します。プロキシ経路を検証した後、取得が失敗・遅延した場合は最後の成功済みカタログを保持します。
  - 一覧取得は推論の認証ではなく、直接通信・別の認証源・API キー・他プロバイダへのフォールバックは行いません。

プロキシ経路が有効でモデル一覧を取得済みなら、Codex CLI がなくても利用できます。

---

<a id="delegated-gpt-work"></a>
## Delegated GPT work（GPT サブエージェントへの作業委譲）

Codex が設定済み、または自動検出が有効で、Permission Modes と `Maximum` が設定されており、明示的な `Subagents` 設定で既定値が置き換えられていない場合、後からのログインにも備えて以下の GPT テンプレートを準備します。各テンプレートのモデルと推論の深さ（effort）は親セッションの選択モデルとは独立して固定されています。

Claude の統括が GPT の子を案内するのは、その Session で Codex の認証とモデル一覧が利用できる場合だけです。利用できなければ Claude Code の Agent（`haiku` / `sonnet`）を使います。GPT の統括は GPT の子を使い続け、Codex が利用できない場合はログインを案内します。Claude の子には自動で切り替えません。

| テンプレート / 役割 | モデル・Effort | 主な用途 |
| --- | --- | --- |
| `lead` | `gpt-6.1-sol` (`max`) | 実装作業の進行・統括（配下に `worker-*` や `collector` を起動可能） |
| `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`, `worker-max` | `gpt-6.1-sol`（各 effort） | コード実装および担当範囲のテスト実行 |
| `collector` | `gpt-6-luna` (`medium`) | 検索、ファイル・ログからの値抽出、URL/API 確認、CI 状態確認、読み取り専用の要約 |

### 委譲の動き

1. Claude に「サブエージェントに委譲して」「lead に実装を渡して」と指示します。
2. Claude は `~/.local/bin/unreal-agent subagent` コマンドでサブエージェントを起動してタスクを渡し、いったん自分のターンを終了します。
3. サブエージェントが完了または連絡を送ると、親セッションが自動的に再開して結果を処理します。
4. サブエージェントの権限モードは親セッションの選択モード（および上限ポリシー）に従い、承認要求は親エージェントが判断します。詳しくは [権限管理](permissions.md) を参照してください。

> **環境変数の継承:** 子は Host の環境（`PATH`、`HOME`、ツール設定など）を継承し、除去するのは `OPENAI_CODEX_ACCESS_TOKEN` と `OPENAI_CODEX_ACCOUNT_ID` のみです。環境隔離を保証するものではありません。
