# Unreal Agent asp Pilot 配布キット

Linux amd64 / glibc @MIN_GLIBC@以上用のローカルsnapshotです。4 binaryを含み、
asp内のビルド、Go、CGOコンパイラは不要です。CGO/tree-sitterを維持しています。
基準revisionは`@REVISION@`。元の`SNAPSHOT.json`のdirty-worktree表示を保持しており、
正式Releaseではありません。認証情報、設定、Session履歴は同梱していません。

1. **Windowsホストからaspへコピー**

   配布ファイルのあるWindowsフォルダを開き、次を実行してください。
   ホストのCLIは未確認です。現在の公式`sbx cp`を使用します。
   CLIがない/構文が未対応の場合はそこで停止し、既存Sandboxを変更しないでください。

   ```text
   sbx cp ./@ARCHIVE@ asp:/tmp/@ARCHIVE@
   sbx cp ./@ARCHIVE@.sha256 asp:/tmp/@ARCHIVE@.sha256
   ```

   参照：[Docker公式 sbx cp](https://docs.docker.com/reference/cli/sbx/cp/)。
   Sandbox側のコピー先は絶対パスです。既存ファイルがある場合はコピー前に
   別名の専用ディレクトリを指定し、以下の`/tmp`もその場所に読み替えてください。

2. **asp内でインストール・診断を一括実行**

   ```sh
   (set -eu; cd /tmp; sha256sum -c @ARCHIVE@.sha256; work=$(mktemp -d /tmp/unreal-pilot.XXXXXXXX); tar -xzf @ARCHIVE@ -C "$work"; bash "$work/@KIT_ROOT@/install.sh")
   ```

   checksum、OS/CPU、glibc、依存ライブラリを検査してから、
   `$HOME/.local/opt/unreal-agent-pilot/asp/@REVISION_SHORT@-<content-id>/`へ配置します。
   同じキットの再実行は内容検証後に既存配置を再利用します。変更された配置や
   symlinkは上書きせず停止します。中断したinstallerのlockが残る場合も停止します。
   同梱doctorは4 binaryのhelp/methodsのみを空の一時HOMEで実行します。
   実推論、プロジェクト操作、Host起動、OSパッケージ変更は行いません。

必要な既存コマンドはbashと一般的なLinux/coreutils（tar、sha256sum、uname、getconf、
ldd、stat、od、tr、env、timeout、mktemp、mkdir、cp、chmod、mv、cmp、grep、rm、rmdir、cat）です。
不足や非互換は診断エラーとして表示し、パッケージを自動導入しません。
External Codex認証は既存のパスの存在・読み取り可否だけ確認します。
内容、有効期限、アカウント、API接続、ネットワーク疎通は確認せず、追加設定もしません。
診断のWARNは、認証/接続の実動作が保証されたという意味ではありません。

既存Codex CLI、PATH、認証ファイル、runtime設定、Session Storeを変更しないため、
復帰時はこれまでのCodex起動操作をそのまま利用できます。Pilot Hostは起動していません。
Pilotを使わない場合も専用ディレクトリを保持して構いません。
通常の`unreal`起動や実推論・編集・Permission付与は今回の2操作には含めません。
