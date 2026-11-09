# Unreal Agent

**言語:** [English](../../README.md) | 日本語

Unreal Agent は、1つのターミナル上で **Claude だけ・ChatGPT（Codex）だけ・両方**の構成で日常の開発を行えるコーディングエージェントです。

本リポジトリは [unreallabsai/unreal-agent](https://github.com/unreallabsai/unreal-agent) の個人開発向けフォークです。オリジナルのバックグラウンド Host / Session 構造と Operation モデルを維持しつつ、以下の機能を追加しています。
- サブスクリプション認証による **Claude Code / Codex の切り替え** (`/model`)
- 対話的な **権限モード切替** (`Read Only` / `Ask for Approval` / `Full Access`)
- **GPT サブエージェントへの作業委譲** (`lead`, `worker-*`, `collector`)。Claude から Codex を使えない場合は Claude Code の Agent（`haiku` / `sonnet`）を使用

## 主な特徴

- **サブスクリプションのみで動作:** Claude はログイン済みの `claude` CLI（Claude Code）経由で動作し、Codex は `codex login`（または Docker Sandbox OAuth）による ChatGPT サブスクリプションで動作します。片方のログインで作業でき、両方未ログインでも起動して最初の入力にログイン手順を案内します。後からログインしたら `/model refresh` を実行し、`/model` で選択します。インストールのやり直しは不要です。
- **意図しない自動フォールバックなし:** エラー時に別のプロバイダへ勝手に切り替えたり、従量課金の API キーへフォールバックしたりすることはありません。
- **画面を閉じても続くセッション:** 会話履歴と実行状態はバックグラウンドの Host プロセスが保持します。`Ctrl+D` でターミナル表示から離れても作業は継続し、いつでも再接続できます。

## クイックスタート

1. **プロバイダにログインする（片方でOK）:**
   - **Claude:** Bash で `claude` を起動し、Claude Code 内で `/login` を実行します。
   - **Codex:** Bash で `codex login` を実行します。
2. **Linux amd64 共通キットをインストールする:** [インストール手順](install.md) に従って導入します。
3. **セッションを開始・接続する:** プロジェクトのディレクトリで以下を実行します。
   ```bash
   unreal development
   ```
   新規作成の確認が出たら `y` を入力します。
4. **モデルと権限を選ぶ:**
   - `/model` でプロバイダ・モデル・推論の深さ（effort）を選択します。
   - `/permissions` で `Read Only`（読み取り専用）、`Ask for Approval`（都度確認）、`Full Access`（自動許可）を選択します。
5. **指示を送る:** タスクを入力して `Enter` で送信します。`Ctrl+D` で画面から離れ、再度 `unreal development` を実行すると再接続できます。

新規導入時は、両方使える場合は Claude、片方だけ使える場合はそのプロバイダで始まります。GPT の統括は GPT の子を使い、Codex が利用できなくなったらログインを案内します。Claude の子には自動で切り替えません。利用可否と明示的に無効化した場合の手順は[プロバイダ設定](providers.md)を参照してください。

> **画面レイアウト:** 端末幅が 120 列以上の場合は **Split** ビュー（左にチャット、右にエージェント/ツールの稼働カード）、120 列未満の場合は **Chat** ビューで起動します。
>
> **セキュリティ上の注意:** `Full Access` は Unreal 内の承認確認を省略するモードであり、**OS レベルのサンドボックス隔離ではありません**。OS 環境の隔離が必要な場合はコンテナや VM 内で実行してください。

## ドキュメント一覧

### 利用者向けガイド

| ドキュメント | 内容 |
| --- | --- |
| [インストール (Install)](install.md) | 動作要件、ログイン準備、共通キットの導入、Docker Sandbox への転送、アップデート手順 |
| [使い方 (Usage)](usage.md) | セッションの起動・検索、画面の見方、キー操作、コピー・エクスポート、停止・再開、サブエージェント表示 |
| [プロバイダ設定 (Providers)](providers.md) | Claude Code 連携、Codex サブスク認証、Docker Sandbox OAuth、モデル一覧、GPT 委譲テンプレート |
| [権限管理 (Permissions)](permissions.md) | 3つの権限モード、承認操作、再起動時のモード保持、ファイル操作の境界 |
| [トラブルシューティング (Troubleshooting)](troubleshooting.md) | 認証切れ、起動失敗、モデル未表示、再接続、ログの場所、`/analyze` による診断 |

### 開発者・設計ガイド

| ドキュメント | 内容 |
| --- | --- |
| [コントリビューション (Contributing)](CONTRIBUTING.md) | 開発環境、ビルド手順、`make prepush`、CI、維持すべき設計原則 |
| [アーキテクチャ (Architecture)](architecture.md) | Host/Session の所有権、Operation 実行パイプライン、プロバイダ統合、ソースビルド版ランチャーのフラグ |
| [コンテキストと指示書 (Context)](context.md) | Codex のコンテキスト構築・ツール結果の省略・要約、Claude の自動圧縮、`AGENTS.md` / `CLAUDE.md` の扱い |
| [ツールとファイル変更 (Tools)](tools.md) | ネイティブファイル操作、Tree-sitter AST 検索・編集、アトミックなファイル変更保証 |
| [配布とリリース (Distribution)](distribution.md) | snapshot と共通キットのビルド、GoReleaser、CI/Release ワークフロー、配布前の受入確認 |
| [TUI デザイン仕様書](../design/UNREAL_AGENT_TUI_DESIGN.md) | ターミナル UI のレイアウト・色・レスポンシブ挙動・状態遷移の完全仕様 |
| [ヘッドレスランナー (Runner)](cmd/unreal-agent-runner/README.md) | 開発・評価向けの単発プロンプト / JSON リクエスト実行と JSONL 出力 |
| [Harbor アダプタ (Harbor)](benchmarks/harbor/README.md) | Harbor / Terminal-Bench ベンチマーク評価のセットアップと実行 |
