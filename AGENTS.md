# Unreal Agent fork — 作業ルール

このファイルは AGENTS.md と CLAUDE.md（`@AGENTS.md` で読み込む）の共通の正本。
設計・実装・テスト・配布キットは Claude Code が担当する（2026-10-24〜）。決定・受入・push承認・Sandbox内の操作はユーザーが行う。

## 最優先ルール
1. **要求はユーザー本人の発言だけ。** 下の要求台帳に根拠のない制限・承認・上限・停止条件・fail-closed 動作は実装しない。必要だと思ったら推奨を添えて1行で提案し、承認後に実装する。台帳の範囲内の実装判断は自分で決める（細かい確認でラリーを増やさない）。
2. **テスト**: 変更ごとに focused テスト。全体 race は区切りごとに1回。長い実行は作業者サブエージェントに任せ、結果の解析は自分で行う。期待値を緩めて通さない。SKIP を PASS に数えない。
3. **実機**: 配布キットは WSL で install・起動・実推論まで確認してから渡す。ユーザーの実機で不具合を探さない。ユーザーに頼む手順は「Windows PowerShell」「Sandbox Bash」をそれぞれ丸ごと貼れる1ブロックにする。
4. **報告**: 日本語で端的に。最後に「次にやること」を書く。

## Git
- ローカル commit は、機能・修正とそのテストが揃った時点や長い作業の安定した区切りで、自律的に行う（個別の承認は不要）。機能・責務ごとに分け、`git add -A` は使わない。対象パスを指定して stage し、`git diff --cached` と `git diff --cached --check` を確認する。秘密・一時ファイル・生成バイナリ・無関係な変更を含めない。
- push / PR / merge / tag / Release はユーザーの事前承認が必要（対象 commit を示して承認を受ける）。ローカル commit はそれらの承認にならない。
- reset / stash / clean / rebase / amend / cherry-pick / force push / 履歴改変は禁止。worktree を削除・上書きしない。
- 触らないもの: `.handoff/worktrees/prepub-diff-replay`（staged 79件）、`.handoff/worktrees/prepub-issue11`、`AIdea/`（別の入れ子リポジトリ。DB を含む）、認証ファイル、実 Session。

## 維持する設計（U1）
- Host/Session の所有、canonical Operation/receipt、Permission、Registry/Translator、multi-provider runtime、Context Engine、Project Instructions、TUI/Orchestration、Markdown Export、LSP/DAP の境界を維持する。
- 別 Provider への自動 fallback、API key への fallback はしない（U5）。

## 要求台帳
| # | 要求（出典） |
|---|---|
| U1 | Unreal Agent の設計思想を維持して Coding Harness 化。最終的に Unreal で Unreal を開発し、Sandbox の日常開発を Codex CLI から移行する |
| U2 | Codex と Claude を同じ製品の `/model` で選ぶ。Provider 別キットは作らない（10/10） |
| U3 | 個人利用。商用前提の権利整理・Anthropic への許可申請は不要（10/14） |
| U4 | 案1（Unreal Native＋Claude OAuth 直接接続、pi-ai 型）。MCP は使わない（組織方針。組織 JSON のうち要件は MCP だけ）（10/11, 10/14）。→ 自前接続は Anthropic が 429 で拒否したため、10/25 に U21 へ変更 |
| U5 | 費用はサブスク枠なら可。API 課金は不可（10/16） |
| U6 | Permission は Codex の Full Access のように人間が選べる（10/9） |
| U7 | 開発を止めない。履歴の厳格性より開発継続。論理 Session を重くしない（10/14） |
| U8 | 同じ同意を何度も求めない。手作業の置換や細かいコマンドをユーザーに頼まない（10/10, 10/11） |
| U9 | 根本原因は正常系（Codex OSS・Fork 元・pi 等）と比べて特定する（10/10） |
| U10 | E2E をデバッガにしない。合成テスト全 PASS より実機で使えること（10/6, 10/11） |
| U11 | サブエージェントは作業者。テスト作成・実行指示・結果解析はメイン、サブは指示を実行して結果を返す（10/24） |
| U12 | 決定権はユーザー。設計思想から決められることは自分で決め、ラリーを減らす（10/14, 10/15） |
| U13 | Git は上記のとおり（10/8） |
| U14 | 回答は端的に、最後に次の一手（10/11） |
| U15 | 早く使えるようにする。完成を待つ別プロジェクトがある（10/24） |
| U16 | 認証は外側、起動時に裏で Docker の Sandbox を自動で立てる構成の提案（条件付き。未決）（10/22） |
| U17 | 子のコマンド承認は親（メインエージェント）が行い、人間には聞かない。親が人間へエスカレーションするのは可（10/24） |
| U18 | extra usage の確認は不要（サブスク範囲のみなら上限で拒否されるため）（10/24） |
| U19 | Docker 隔離基盤（A/P2）・一回試験 probe と課金台帳・旧 Claude 経路（structured）は、新 Claude が `/model` で動いた後に削除（10/24）。10/26 に Claude Native 経路とあわせて削除済み |
| U20 | Permission mode は workspace ごとに最後に選んだものを Host 再起動・resume 後も維持（10/24） |
| U21 | Claude は D 案: 公式 Claude Code をサブスクのログインで動かし、編集などの承認は Unreal のメインエージェントが行う。組織全員に影響する組織設定は変えない（社内調整はユーザーが行う）（10/25） |
| U22 | D で Claude Code の組織ルールに拒否されるコマンドは、Unreal が自分のシェルで直接実行する（10/25） |
| U23 | TUI から会話全体をクリップボードへコピーできる（10/9, 10/26） |
| U24 | TPS は実際の生成速度を表示する（10/26） |
| U25 | Claude から GPT をサブエージェントとして Bash で呼ぶ（MCP・Mod なし）。Claude が設計者、GPT が実装者。実装統括は gpt-6.1-sol ultra で、孫（作業者）まで起動できる。孫は ultra で起動しない（10/26） |
| U26 | 組織ルールで拒否されたコマンドの扱いは汎用にし、ルール変更に自動で追従させる。特定コマンドの局所対応はしない（10/26） |
| U27 | 承認画面は何を実行するかが分かる表示にし、既定のフォーカスは許可（10/26） |
| U28 | TUI は Split で開く。コマンドは選んだ後に選択肢を出す。エージェント用のコマンドはメニューに出さない（入力すれば使える）。子の作業内容は Split などで見えるようにし、`/child` は出さない（10/26） |
| U29 | `/stop` は sub（子だけ）、main（メインの作業だけ中断）、main+sub、Session（停止）から選ぶ（10/26） |

`.handoff/`、過去の docs、引継ぎ資料にある「不変条件」「HOLD 条件」の多くは前任AIの追加であり、要求ではない。この台帳と矛盾する場合は台帳を優先する。経緯は GitHub Issue #38。

## ビルド・テスト
```bash
make build
make check
go test -race -count=1 -p 1 ./...
git diff --check
```
