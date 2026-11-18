# アーキテクチャ (Architecture)

**言語:** [English](../architecture.md) | 日本語

Unreal Agent は、**会話履歴とツール実行の所有権を持つバックグラウンドプロセス（Host）** と、**ターミナル描画を行うクライアント（TUI）** を明確に分離した設計になっています。

---

## 1. Host と Session の所有権

[Host](../../harness/host/host.go) は稼働中の各 Session に対して1つの Coordinator を管理し、単一書き込みプロセス（single-writer）の安全性を保証します。

- **プロセス間書き込みロック:** Session の新規作成や再開の前に、プロセス間で共有される書き込みロックを取得します。
- **正規ストアと投影（Projection）:** ディスク上の Session ストアが唯一の情報源（Single Source of Truth）です。TUI の表示状態やモデル入力は正規履歴から作られる投影です。プロバイダが所有する会話は関連ランタイム状態であり、Claude Code は別の会話記録を所有し、通常は `--resume` で継続します。
- **Session のライフサイクル。** ランタイムファクトリは Session ごとにコンテキストビルダー、プロバイダクライアント、ツール群、Operation Manager を生成します。TUI の切断（`Ctrl+D`）や単一ターンの中断後も Session は稼働します。親のモデル呼び出しが失敗するとそのターンが終了し、子の実行・Operation・承認待ちは継続します。明示的な Session 停止（`/stop session`）や Host のシャットダウンでは、ハンドラの終了を待ってから所有権を解放します。
- **べき等な送信と Receipt（受領証）:** すべての入力には現在の Host 世代（generation）と安定した `Inbox` ID が付与されます。Receipt は「入力がストアへ永続化されたこと」を意味します（要求された副作用の完了ではありません）。同じ ID での再送（`/retry`）は元の Receipt を返し、異なる内容や古い世代のリクエストは拒否されます。
- **停止の復元とサブエージェントの扱い。** 未完了の停止要求は Operation のディスパッチ前に復元されます。親 Session の再開時、書き込みロックを保持している子は元の実行を継続します。実行主体を失った子はキャンセルとして記録し、終了通知を親に1件届けます。

---

## 2. Operation・Registry・実行パイプライン

ツール実行は、純粋な引数検証・計画作成（**Translator**）と、非同期 I/O 実行（**Executor**）に分かれています。

1. **Tool Registry ([`harness/tool/registry.go`](../../harness/tool/registry.go)):** 有効なツール定義と Translator（`Bash`, `ViewImage`, `SkillUse`, ネイティブファイル操作, AST ツール, 観測用 `claude_code` ツールなど）を管理します。
2. **Translator:** モデルのツール呼び出し引数を検証し、不変（immutable）な Operation 計画を生成します。Translator 自身はファイルやネットワークへの副作用を一切行いません。
3. **Coordinator と Operation Manager:** Coordinator がモデル応答と Operation 計画を正規履歴へ記録し、Operation Manager が実行とキャンセルを管理します。Executor は型付きの結果を返し、正規 Receipt が継続やリカバリの基準になります。
4. **Claude Code の観測 Receipt (`claude_code`):** Claude Code が自身の内蔵ツールを実行した場合、Unreal はその結果を「観測された Receipt」として記録します。これは外部 CLI がすでに実行した事実の記録であり、Unreal が同じツールをもう一度実行する権限にはなりません。

### 権限の強制境界

権限チェックは具体的な実行境界で行われます。
- 固定ポリシー構成では `host.Options.Policy` が不変の権限を与え、`nil` は全拒否となります。
- 人間向けの権限モード（`/permissions`）が有効な場合、Host ポリシーは読み取り専用のベースラインと上限ポリシーを定め、実効権限はユーザーの選択モード（`Read Only` / `Ask for Approval` / `Full Access`）と上限の共通部分で決まります。
- 子セッションにも親のポリシー制限がそのまま適用されます。プロンプトの文章やモデルの出力によって権限が増えることはありません。詳しくは [権限管理](permissions.md) と [ツールとファイル変更](tools.md) を参照してください。

---

## 3. プロバイダ・コンテキスト・外部プロトコル統合

[ランタイム構成](../../cmd/internal/agentrunner/runtime.go) は、同じ Host の背後に2つのサブスクリプション経路を統合しています。

- **Codex (`openai-codex`):** Unreal 内蔵の Responses アダプタとツール実行パイプラインを使用します。
- **Claude (`claude`):** Claude Code CLI を起動し、会話管理と内蔵ツールは CLI に任せつつ、承認判定を Unreal が担います。CLI が実行を辞退した Bash コマンドは Unreal の Bash 実行器で処理します。
- **外部所有の認証情報:** ランタイム設定には秘匿情報を含まない選択状態のみを記録し、認証トークンは外部 CLI（または Docker Sandbox プロキシ）が所有します。エラー時に別プロバイダへ自動切替することはありません。
- **LSP / DAP の分離:** 言語サーバー（`harness/lsp`）およびデバッガ（`harness/dap`）は独立したライフサイクルを持ちます。LSP によるファイル編集は共通の Mutation サービスを経由し、モデル推論や TUI 描画、認可ロジックとは分離されています。

---

## 4. TUI とエクスポート

ターミナル画面（TUI）は Unix ドメインソケット経由で接続する Host ゲートウェイクライアントです。
- 履歴と稼働中の Operation を **Chat**、**Orchestration**、**Split** の各ビューへ投影します。
- 通信ギャップや世代変更を検知すると自動的に再同期します。
- `/analyze`、`/copy`、`/export` は画面の表示行ではなく Host の正規公開履歴から直接データを読み取ります。
- `AGENTS.md` の本文は正規ストレージに保持され、公開履歴にはメタデータ（パス・バイト数・ハッシュ）のみが投影されます。

---

<a id="native-launcher"></a>
## Native launcher（ソースビルド版ネイティブランチャー）

日常利用では共通キットのラッパー（`~/.local/bin/unreal`）を使いますが、リポジトリをソースビルドした場合は `bin/unreal`（ネイティブランチャー）を直接実行できます。

### ビルドと起動手順

1. [プロバイダ設定](providers.md) に従ってサブスクリプションログインを準備します。
2. リポジトリのルートでビルドします。
   ```bash
   make build
   bin/unreal --help
   ```
   `make build` により `bin/unreal-agent`、`bin/unreal`（`unreal-agent` へのシンボリックリンク。実行ファイル名で動作が切り替わります）、`bin/unreal-agent-runner`、`bin/unreal-agent-auth` が生成されます。
3. 名前を指定してセッションを開きます。
   ```bash
   bin/unreal development
   ```
   ※ 共通キットのラッパーとは異なり、引数なしで `bin/unreal` を実行するとセッション一覧ではなく `default` という名前のセッションを開きます。

初回起動時に設定ファイルが存在しない場合、プロバイダ自動検出が走って `runtime.json` を生成します（両方使える場合は Claude を優先。`UNREAL_HARNESS_LLM_PROVIDER` と `UNREAL_HARNESS_LLM_MODEL` で明示指定も可能）。必要に応じてバックグラウンド Host を起動し、準備完了を待ってからセッションへ接続します。

### 既存 Host との互換性確認

ネイティブランチャーの `launcher.check` は設定・Session・credential・Codex auth の各パスと normal-launch binding を比較します。実行ファイルの digest や protocol revision は比較しません。共通キットのラッパーによる build/profile 更新検出と Host の自動再起動とは別の確認です。

### ネイティブランチャーのフラグ一覧 ([`cmd/unreal-agent/launcher.go`](../../cmd/unreal-agent/launcher.go))

フラグはセッション名の前後どちらにも指定できます。XDG ベースディレクトリは絶対パスである必要があります。

| フラグ | 既定値と意味 |
| --- | --- |
| `--config <path>` | 省略時は `${XDG_CONFIG_HOME:-$HOME/.config}/unreal-agent/runtime.json`。ファイルを明示するとその設定を使用。 |
| `--state-directory <dir>` | `${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent` |
| `--session-directory <dir>` | `<state-directory>/sessions` |
| `--socket <path>` | 利用可能なら `$XDG_RUNTIME_DIR/unreal-agent/host.sock`、それ以外は `<state-directory>/run/host.sock` |
| `--credential-directory <dir>` | 未設定。開発者設定向けの管理キーストアディレクトリ。 |
| `--codex-auth-file <path>` | 明示しない限り外部 Codex 認証ファイルを自動探索。 |
| `--startup-timeout <dur>` | `0`（キャンセルされるまで待機）。正の Go 時間表記（例: `30s`）で起動待機の上限を設定。 |
| `--bootstrap-codex-disabled` | `false`。初回構成生成時に Codex を無効化。 |
| `--bootstrap-claude-disabled` | `false`。初回構成生成時に Claude を無効化。 |
| `--bootstrap-codex-docker-oauth` | `false`。初回構成生成時に Docker Codex OAuth を明示選択。 |
| `--bootstrap-codex-auto` | `false`。初回構成生成時に Codex の認証経路を自動判別。 |
| `--bootstrap-claude-binary <path>` | 未設定。外部 `claude` 実行ファイルのパスを指定。 |
| `--help`, `-h` | ネイティブランチャーのヘルプを表示。 |

### 設定ファイル (`runtime.json`) の構成

設定ローダーは未知の JSON フィールドを拒否します。トップレベルのキー（[`cmd/unreal-agent/run.go`](../../cmd/unreal-agent/run.go)）は以下の通りです。
- `Runtime`, `Permissions`, `PermissionModes`（任意）, `Providers`, `Launcher`, `Context`, `LanguageServers`, `DebugAdapters`, `Subagents`
- Claude 実行ファイルのパスは `Runtime.Claude.Binary` と各プロバイダの `Claude.Binary`（自動検出用は `Launcher.ClaudeBinary`）に保持されます。
- プロバイダのリトライ回数は `max_attempts` で指定します。

より低レベルな Host CLI オプションを確認する場合は、`bin/unreal-agent serve -h` または `bin/unreal-agent attach -h` を実行してください。

---

## 内部パッケージ仕様書一覧

| パッケージ | ドキュメント |
| --- | --- |
| `harness/authflow` | [認証フロー管理 (`unreal-agent-auth`)](harness/authflow/README.md) |
| `harness/credential` | [クレデンシャル保存と有効期限](harness/credential/README.md) |
| `harness/dap` | [DAP デバッガランタイム](harness/dap/README.md) |
| `harness/lsp` | [言語サーバー管理 (LSP)](harness/lsp/README.md) |
| `harness/permission` | [ケーパビリティと権限の強制](harness/permission/README.md) |
| `harness/profile` | [プロンプト・ツールプロファイル](harness/profile/README.md) |
| `harness/provider` | [プロバイダ構成とレジストリ](harness/provider/README.md) |
| `harness/subagent` | [非同期サブエージェント](harness/subagent/README.md) |
| `harness/viewer` | [サブエージェント・Operation ビューア](harness/viewer/README.md) |
| `internal/apijson` | [生成 OpenAI API 型向け JSON コーデック](internal/apijson/README.md) |
| `third_party/openai-openapi` | [OpenAI OpenAPI 仕様書ベンダリング](third_party/openai-openapi/README.md) |
