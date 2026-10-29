# Unreal Agent 共通開発キット

**Codex か Claude のどちらか一方が使えれば始められます。** 同じ `/model` で
接続先・モデル・effort を選べます。API キーは使いません。

asp/devlore の両方で同じキットを使います。Linux amd64、glibc @MIN_GLIBC@ 以上、
Python 3・Bash・Git が必要です。ローカル snapshot（@REVISION@）です。
認証情報は同梱しません。

## 使う接続先の準備

- **Codex:** 公式の Codex CLI をインストールし、Bash で `codex login` を実行します。
  Docker Sandboxes のログインをすでに使っている場合は、その接続も利用できます。
- **Claude:** 公式の Claude Code をインストールし、Bash で `claude` を起動します。
  Claude Code の中で `/login` を実行し、Claude サブスクでログインします。

Unreal の `/login` はこの手順を案内します。あとからログインした側は
`/model refresh`、または `/model` でその接続先を選ぶと再検出します。
Unreal を開き直す必要はありません。

## インストール

Windows PowerShell から Sandbox に転送する場合（`sbx` が使える環境）:

```powershell
sbx cp .\@ARCHIVE@ asp:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
sbx cp .\@ARCHIVE@ devlore:/tmp/@ARCHIVE@
sbx cp .\@ARCHIVE@.sha256 devlore:/tmp/@ARCHIVE@.sha256
```

各 Sandbox の Bash で実行します。

```bash
cd /tmp
archive='@ARCHIVE@'
test -f "$archive" && test -f "$archive.sha256" && sha256sum -c "$archive.sha256" &&
kit=$(mktemp -d "$HOME/unreal-common.XXXXXXXX") &&
tar -xzf "$archive" -C "$kit" &&
cd "$kit/@KIT_ROOT@" &&
bash development.sh --configure-providers --replace-pilot-launcher
```

「インストール完了」の後に、接続先ごとの利用状態が表示されます。
使えない側にはログイン方法やインストールの案内が出ます。
両方未ログインなら、どちらかにログインしてから起動してください。

## 起動と操作

新しい Bash を開き、作業フォルダで実行します。

```bash
cd ~/workspace/your-existing-project
unreal development
# Ctrl+D で画面を離れた後も、同じコマンドで再接続できます
unreal development
```

`unreal` が別のコマンドに隠れる場合は `type -a unreal` で確認し、
`"$HOME/.local/bin/unreal"` を使ってください。

- `/model`: 接続先 → モデル → effort を選びます。両方使える初回は Claude、
  一方だけなら使える側で始まります。使えない側も状態と準備手順を表示します。
- `/permissions`: Read Only（読むだけ）、Ask（書き込みと実行の前に確認）、
  Full Access（確認なし）を選びます。Full Access は OS の隔離ではありません。
- `/view`: Chat / Orchestration / Split を選びます。初期は Split です。
- `/copy`、`/export`: 会話をコピー・Markdown 出力します。
- `/stop`、`/resume`: 作業の停止対象を選ぶ・止まった会話を再開します。
- `/analyze`: 詳しい診断情報を確認します。

Claude Code の操作も Unreal の権限に従います。Claude Code が自分では実行しない
コマンドは、Unreal の通常の Bash 要求として同じ権限で扱います。
接続先が使えなくなった場合はログインし直すか、停止中でも `/model` で
別の接続先を選び、`/resume` で再開できます。自動で別の接続先には切り替えません。

## 既存 workspace への更新

同じインストール手順で更新します。その workspace で次に `unreal NAME` を実行すると、
更新を検出してバックグラウンド処理（Host）を再起動し、**会話と設定を引き継ぎます**。
別の Session を作る必要はありません。既存の会話・認証情報は削除しません。

workspace ごとに設定と会話を保存します。`--instance` や `--config` を明示した場合は
その利用先を使います。`unreal --workspace-info` で保存場所、`unreal --readme` でこの説明、
`bash doctor.sh` で実行ファイルと動作環境を確認できます。

接続方法を明示する必要がある場合は `--docker-sandbox-oauth` / `--external-codex` /
`--codex-auto`、接続先を無効にする場合は `--disable-codex` / `--disable-claude` を
セットアップ時に指定できます。ログイン情報のコピーや CLI のログイン操作は行いません。

## 新規合成workspaceでの受入

`unreal --acceptance-fixture repair`は200行のsample.txt（中央行だけ
UNREAL_REPAIR_BEFORE）とAGENTS.mdを含む新規Git fixtureを作ります。
実プロジェクトを探索・変更しません。最初に`/model`、`/permissions`を確認し、
人間が必要な権限を承認して次を送信します。

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

通常引用文を今回の実行結果にしません。現在のcanonical receiptから受け取った
本文/revision/exitは観測値として確認します。read/edit/Bash receipts、実ファイル、
TPS/scroll、detach/attach履歴を独立に確認してください。
この手順は推論を自動送信しません。実機受入は合成検証とは別です。
