# トラブルシューティング (Troubleshooting)

**言語:** [English](../troubleshooting.md) | 日本語

## ログインが必要・認証期限切れと表示される

`auth required`、`external_reauth_required`、`OAuth session expired`、`OAuth token has expired` などが表示された場合は、選択中のプロバイダの外部ログインを更新する必要があります。Unreal が自動的に別のプロバイダへ切り替えたり、API キーを使ったりすることはありません。

### 対処手順

1. **Unreal を動かしている Linux 環境で再ログインする:**
   - **Claude:** 別の Bash シェルを開き、`claude` を起動して Claude Code 内で `/login` を実行します（Windows 上の Docker Sandbox なら `sbx exec -it <sandbox> claude` を実行）。
   - **Codex（通常ログイン）:** Bash で `codex login` を実行します。
   - **Codex（Docker Sandbox OAuth）:** ホスト側の OAuth ログイン（`sbx secret set openai --oauth`）と Sandbox のプロキシ設定を確認します（[プロバイダ設定](providers.md#docker-sandboxes-codex-oauth) 参照）。
2. **Unreal 側で再検出する:** Unreal に戻り、`/model refresh` を実行する（または `/model` でそのプロバイダを選択する）と認証状態が再確認されます。
3. **停止している場合は再開する:** エラーでセッションが停止していた場合は `/resume` を実行します。

> **補足:** Windows や macOS ホスト側でログインしていても、Linux（WSL や Sandbox）側の CLI からその認証を参照できるとは限りません。また、Unreal 内の `/login` は手順を表示するコマンドであり、実際のログイン操作は常に `claude` または `codex` CLI で行います。

---

## 起動できない・コマンドが見つからない

`unreal` が起動しない、または意図しないコマンドが呼ばれる場合は以下を確認してください。

1. **Bash でコマンドの解決先とワークスペース情報を確認する:**
   ```bash
   type -a unreal claude codex
   "$HOME/.local/bin/unreal" --workspace-info
   ```
2. **別コマンドとの競合や `PATH` 未設定を直す:**
   別のエイリアスや実行ファイルが優先されている場合は、`"$HOME/.local/bin/unreal"` を直接指定するか、シェル設定を読み込み直します。
   ```bash
   . "$HOME/.config/unreal-agent-common/path.sh"
   ```
3. **初回はセッション名を指定して起動する:**
   引数なしの `unreal` は既存セッションの一覧表示専用です。まだセッションがない場合は `unreal development` のように名前を付けて起動してください。
4. **同梱バイナリと動作環境を検査する:**
   展開したキットのフォルダで `bash doctor.sh` を実行すると、バイナリの整合性と glibc 等の環境互換性を確認できます。

> **ヒント:** `unreal --workspace-info` は**現在いるディレクトリ**のワークスペース情報を表示します。別の場所にあるセッションの情報を調べる場合は、先に `ucd <セッション名>` で移動してから `unreal --workspace-info` を実行してください。エラー解消のために設定ファイルやセッションデータを手動で上書き・削除しないでください。

---

## モデルが表示されない・一覧が古い

1. 外部 CLI（`claude` または `codex`）がインストールされ、ログイン済みか確認します。
2. **通常の Codex OAuth の場合:** 初回導入直後で `models_cache.json` がまだ作られていない場合は、一度 `codex` CLI を開いてローカルのモデルキャッシュを生成させます。
3. Unreal 内で `/model refresh` を実行します（起動時に未登録だったプロバイダも含めて再検出されます）。

> **補足:**
> - Docker Sandbox Codex OAuth 経路で CLI からのモデル一覧取得が失敗した場合、前回成功したキャッシュ（`~/.cache/unreal-agent/codex-cli-catalog.json`）が引き続き表示されることがあります。
> - 一覧にモデルが表示されていても、アカウントの契約プランによっては利用できない場合があります。

---

## 再接続・再開・再送

1. **`Ctrl+D` や切断後の再接続:** 同じ `unreal <セッション名>`（例: `unreal development`）を実行すると再接続でき、正規履歴から画面が再構築されます。
2. **停止したセッションの再開:** TUI 内で `/resume` を実行します（または `unreal <セッション名>` で開き直します）。以前のプロバイダが使えない場合は、先に `/model` で別プロバイダを選んでから `/resume` を実行してください。
3. **送信結果が不明なときの再送 (`/retry`):** 通信断などでメッセージが届いたか不明になった場合は `/retry` を使います。同じ送信 ID で再試行するため、メッセージが二重に作られることはありません。
4. **別プロセスが使用中の場合 (`writer_owned`):** 他のターミナル画面でそのセッションを開いている場合は、そちらを閉じてから再接続してください。ロックファイルやセッションデータを削除しないでください。

親のモデルターンが失敗しても子の作業と報告は続きます。親に新しい入力を送ると作業を続けられます。Host の再起動時は、書き込みロックを保持する子に再接続し、実行主体を失った子の終了通知を1件ずつ受け取ります。

---

## ログと保存場所の確認

プロジェクト内で `unreal --workspace-info` を実行すると、Workspace / Config / State / Socket の4項目が表示されます。log のパスは出力されず、表示された State 内に `host.log` があります。

共通キットの既定の保存先は以下の通りです。

- **ワークスペース別設定:** `~/.config/unreal-agent-common/repos/`
- **ワークスペース別状態・ログ:** `~/.local/state/unreal-agent-common/r-…/`（ワークスペース固有。この中に `host.log` と `sessions/` があります）
- **共有認証ストア:** `~/.local/state/unreal-agent/credentials`（インストーラーと `/login` の保存先。全ワークスペース・instance の Host が参照します）
- **共有の役割別モデル選択:** `~/.local/state/unreal-agent/subagent-model.json`
- **ランチャー設定:** `~/.config/unreal-agent-common/launcher.json`

共有ディレクトリは `XDG_STATE_HOME` 設定時には `$XDG_STATE_HOME/unreal-agent/` です。Mac も同じです。
旧リポジトリ別の `subagent-model.json` は共有設定がなければ起動時に移行し、元ファイルを残します。

問題が起きた時刻の前後の `host.log` を確認してください。

> **注意:** `sessions/` 内の JSONL ファイルは会話の正規データであり、一時的なデバッグログではありません（削除しないでください）。また、Issue 等で報告する際は認証情報やプライベートの会話本文を含めないよう注意してください。

---

<a id="read-analysis-and-measurements"></a>
## Read analysis and measurements（診断と計測値の見方）

TUI 内で `/analyze` を実行すると、セッションの稼働統計やトークン使用量を詳しく確認できます。

1. `/analyze` を入力してビュー選択メニューを開きます。
2. 見たい項目を選択します（`Enter` または `Esc` で一覧に戻ります）。
3. **Export** を選ぶと、統計データを JSON または Markdown で保存できます（会話本文も含めて出力する場合は `/export all` を使います）。

### `/analyze` の各ビュー

| ビュー | 確認できる内容 |
| --- | --- |
| **Overview** | プロバイダ・モデル、応答数・ツール実行数、経過時間、Host のコミットリビジョン |
| **Usage** | 入力トークン、キャッシュ済み入力トークン、推論・出力トークン合計、Claude のターン合計値 |
| **Turns** | ターンごとのプロバイダ・モデル・effort とトークン使用量 |
| **Tools** | ツール操作の結果と Claude Bash フックの所要時間（`native_bash`） |
| **Agents** | サブエージェント一覧、終了結果、`child_start` 時のテンプレート・モデル・effort |
| **Context** | コンテキスト上限、平均入力トークン、省略されたツール結果数、推定節約トークン、prune 境界、Claude の自動圧縮記録 |
| **Timeline** | 記録されたイベントの時系列とアクティビティ計測値 |
| **Errors** | 記録されたエラーと秘匿処理済みのプロバイダ診断情報 |

### 数値を読む際のポイント

- **キャッシュ率 (Cache %):** 報告された使用量における `cached_input / input` の割合です。
- **フッターの「文脈」と Average input の違い:** 画面下部の「文脈」は**直近のリクエスト1回分**のコンテキスト量を示します（ツールループ全体の合算ではありません）。`/analyze` の平均入力は要約リクエスト等を含む報告済みモデル呼び出しの平均です。
- **生成速度 (`tok/s`):** 報告された出力トークン数と生成時間から計算されます。`~` 付きはクライアント時計による計測です。Claude の Bash 所要時間は実行前フックから完了までの時間です。
- **Claude の turn-total usage:** すでに応答合計に含まれているため、二重に加算しないでください。
- **不明値 (`—` / `unknown`):** トークン数がプロバイダから報告されなかった場合は `0` ではなく **unknown**（`—`）として扱われます。`partial/incomplete history` や `resync` と表示されている場合は、観測できた範囲のデータのみが集計されています。
