# Unreal Agent 共通開発キット

**Codex か Claude のどちらか一方が使えれば始められます。** 同じ `/model` メニューで接続先・モデル・effort を選べます。Codex / Claude のサブスク接続には API キーは不要です。

- **対応環境:** `asp` / `devlore` の両方で同じキットを使います。Linux amd64、glibc @MIN_GLIBC@ 以上、Python 3・Bash・Git が必要です。
- **パッケージ内容:** ローカル snapshot（`@REVISION@`）です。認証情報は同梱しません。

---

## 1. 使う接続先の準備

- **Codex:** 公式の Codex CLI をインストールし、Bash で `codex login` を実行します（Docker Sandboxes のログインをすでに使っている場合は、その接続も利用できます）。
- **Claude:** 公式の Claude Code をインストールし、Bash で `claude` を起動します。Claude Code の中で `/login` を実行し、Claude サブスクでログインします。

> **補足:** Unreal の `/login` はこの手順を案内します。あとからログインした側は `/model refresh`、または `/model` でその接続先を選ぶと再検出します（Unreal を開き直す必要はありません）。

---

## 2. インストール

1. **Windows PowerShell から Sandbox に転送する場合（`sbx` が使える環境）:**

   ```powershell
   sbx cp .\@ARCHIVE@ asp:/tmp/@ARCHIVE@
   sbx cp .\@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
   sbx cp .\@ARCHIVE@ devlore:/tmp/@ARCHIVE@
   sbx cp .\@ARCHIVE@.sha256 devlore:/tmp/@ARCHIVE@.sha256
   ```

2. **各 Sandbox の Bash で実行:**

   ```bash
   cd /tmp
   archive='@ARCHIVE@'
   test -f "$archive" && test -f "$archive.sha256" && sha256sum -c "$archive.sha256" &&
   kit=$(mktemp -d "$HOME/unreal-common.XXXXXXXX") &&
   tar -xzf "$archive" -C "$kit" &&
   cd "$kit/@KIT_ROOT@" &&
   bash development.sh --configure-providers --replace-pilot-launcher
   ```

「インストール完了」の後に、接続先ごとの利用状態が表示されます。使えない側にはログイン方法やインストールの案内が出ます。両方未ログインなら、どちらかにログインしてから起動してください。

Sandbox が UTC の場合は、インストール時に `--time-zone Asia/Tokyo` を追加すると日本時間で表示します。日本以外では現地の IANA タイムゾーン名を指定してください。設定は更新後も引き継がれ、起動シェルの `TZ` が優先されます。

子 Session の同時実行数をツリーごとに制限するには `--max-active-children 10` を追加します。設定は更新後も引き継がれ、次回の起動時に workspace の runtime.json に反映されます。既定と `0` は上限なしです。

---

## 3. 起動と操作

新しい Bash を開き、作業フォルダで実行します。

```bash
cd ~/workspace/your-existing-project
unreal development
# Ctrl+D で画面を離れた後も、同じコマンドで再接続できます
unreal development
```

### セッション起動・検索コマンド

| コマンド | 動作 |
| --- | --- |
| `unreal NAME` | 名前で Session に接続します。どのディレクトリから実行しても、その Session を作った workspace で動きます。名前は1つの Session にだけ使い、同じ名前の Session は作れません。見つからなければ現在の workspace での新規作成を確認します。古い版で同名が複数できていたら、警告と一覧を出し（Enter で稼働中の Host がある最新の候補）、不要な方の `--archive-session` を促します。 |
| `unreal`（引数なし） | 既存のメイン Session を新しい順に一覧表示し、番号で選びます（一覧から新規作成はしません）。 |
| `unreal --list` | Session の一覧（名前・workspace・最終更新・Host の稼働状態・instance）を新しい順に表示します。 |
| `unreal --where NAME` | その Session の workspace のパスを表示します。 |
| `ucd NAME` | Bash でその Session の workspace ディレクトリへ移動します。 |
| `unreal --archive-session NAME --workspace PATH` | 指定した workspace の Session を名前検索と一覧から除外します。同じ workspace に複数あれば `--instance` も指定します。 |

アーカイブ後も会話の記録は残ります。開くには対象 workspace に移動し、`unreal NAME --instance INSTANCE` で instance を明示します。

※ `unreal` が別のコマンドに隠れる場合は `type -a unreal` で確認し、`"$HOME/.local/bin/unreal"` を使ってください。

### TUI 内の主なスラッシュコマンド

- `/model`: 接続先 → モデル → effort を選びます（両方使える初回は Claude、一方だけなら使える側で始まります。使えない側も状態と準備手順を表示します）。
- `/permissions`: `Read Only`（読むだけ）、`Ask`（書き込みと実行の前に確認）、`Full Access`（確認なし）を選びます。※ `Full Access` は OS の隔離ではありません。
- `/view`: `Chat` / `Orchestration` / `Split` を選びます（初期は `Split` です）。
- `/copy`、`/export`: 会話をコピー・Markdown 出力します。
- `/stop`、`/resume`: 作業の停止対象を選ぶ・止まった会話を再開します。
- `/analyze`: 詳しい診断情報を確認します。

> **Claude Code とプロバイダ切替の扱い:**
> - Claude Code の操作も Unreal の権限に従います。Claude Code が自分では実行しないコマンドは、Unreal の通常の Bash 要求として同じ権限で扱います。
> - 接続先が使えなくなった場合はログインし直すか、停止中でも `/model` で別の接続先を選び、`/resume` で再開できます（自動で別の接続先には切り替えません）。

---

## 4. 既存 workspace への更新と設定確認

同じインストール手順で更新します。その workspace で次に `unreal NAME` を実行したとき、Host がアイドルなら新しいビルドで再起動し、**会話と設定を引き継ぎます**。いずれかの Session でモデルターン、操作、子エージェントが動いていれば、更新を知らせて稼働中の Host と同じビルドの TUI で接続します。Host がアイドルになってから開き直すと更新が適用されます。

すぐ切り替えるには `unreal NAME --restart-host` を使います。作業を中断する前に、直接の子エージェント、その他の操作、稼働中の Session の件数を表示して確認を求めます。workspace 全体のアイドル状態を確認できない旧 Host もこのコマンドで切り替えます。

- **workspace ごとの保存と明示指定:** workspace ごとに設定と会話を保存します。`--instance` や `--config` を明示した場合は現在の workspace 内の明示した利用先を使い、全 workspace の Session 探索は行いません。
- **確認用コマンド:**
  - `unreal --workspace-info`: 現在の workspace の **Workspace / Config / State / Socket** の4項目を表示します（log のパスは出しません。表示された State 内に `host.log` があります）。
  - `unreal --readme`: この説明を表示します。
  - `bash doctor.sh`: 実行ファイルと動作環境を確認できます。
- **セットアップ時のオプション（`development.sh`）:**
  - 接続方法の明示: `--docker-sandbox-oauth` / `--external-codex` / `--codex-auto`
  - 接続先の無効化: `--disable-codex` / `--disable-claude`
  - ※ ログイン情報のコピーや CLI のログイン操作は行いません。

---

## 5. 新規合成 workspace での受入

`unreal --acceptance-fixture repair` は 200行の `sample.txt`（中央行だけ `UNREAL_REPAIR_BEFORE`）と `AGENTS.md` を含む新規 Git fixture を作ります（実プロジェクトを探索・変更しません）。最初に `/model`、`/permissions` を確認し、人間が必要な権限を承認して次を送信します。

```text
sample.txtをnative readで200行全体読み、受信した本文とwhole-file revisionだけで
中央のUNREAL_REPAIR_BEFOREをUNREAL_REPAIR_AFTERへnative editしてください。
再readし、200行と他の行が保持されたことを観測してください。
Bashは/bin/sh -cです。POSIX sh構文で以下を検証してください：
set -eu
[ "$(wc -l < sample.txt)" -eq 200 ]
grep -q '^UNREAL_REPAIR_AFTER$' sample.txt
! grep -q '^UNREAL_REPAIR_BEFORE$' sample.txt
git diff -- sample.txt
実際のexit codeとcanonical receiptを観測してからFinalを返してください。
非0、拒否、秘匿stubを成功にせず、成功時だけUNREAL_ACCEPTANCE_OKと記載してください。
commit/push、shellによる編集、provider切替は禁止です。
```

- 通常引用文を今回の実行結果にせず、現在の canonical receipt から受け取った本文 / revision / exit を観測値として確認します。
- `read` / `edit` / `Bash` receipts、実ファイル、TPS / scroll、detach / attach 履歴を独立に確認してください。
- この手順は推論を自動送信しません。実機受入は合成検証とは別です。

## OpenCode Go の配布時設定

`KIT.json` の `opencode_go_enabled` と `unreal-agent --version` で有効・無効を確認できます。有効版は `bash development.sh --opencode-go-api-key-file /absolute/go.key`（0600）、または `--opencode-go-api-key-env MY_GO_KEY` でキーを登録し、`/model` で明示選択します。無効版ではどちらも値を表示せず拒否します。無効版は `/model`・discovery・モデル一覧に OpenCode Go を出しません。古い設定や環境変数では戻せず、ソースからの再ビルドが必要です。保存済み Session は元の選択を保持し、`/model` でほかの接続を選ぶよう案内します。子も案内つきで失敗し、自動切替はしません。
