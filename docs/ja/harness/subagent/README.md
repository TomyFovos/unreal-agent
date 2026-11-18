# 非同期サブエージェント (`harness/subagent`)

**言語:** [English](../../../../harness/subagent/README.md) | 日本語

`harness/subagent` パッケージは、非同期で動作する子エージェントのサブプロセス（`unreal-agent child --stdio`）、親子間の双方向メッセージ通信、および永続化・リカバリを管理します。

---

## 1. 親 Host と子プロセスの構成

### 親 Host 側の組み込み

1. `host.SessionFromContext(ctx)` から親 Session のオーナーを取得します。
2. `subagent.NewManager(ctx, Config{Owner: owner, Directory: sessionDirectory, Binary: absoluteAgentBinary, Arguments: []string{"child", "--stdio"}, Templates: allowedTemplates})` を生成し、`LocalOperationManager` に登録します。
3. `tool/subagent.Extensions(owner.ID, templates, false)` をツールレジストリに追加し、親ランタイムの終了時にハンドラの `Close` を呼びます。
4. `Runtime.InterruptedInput = manager.InterruptedInput` と `Runtime.CompletedInput = manager.CompletedInput` を設定します。Coordinator は実行中のモデルが子の報告を読んだか確認できる段階で終了通知を記録し、その後に one-shot Session の終了を判断します。

- **テンプレートによる安全な制限:** 実行バイナリ、プロセス引数、および子に許可する権限は Host 側のテンプレート設定（`Subagents`）で固定されます。モデルはタスク文字列と任意の依頼文の欄を指定できます。モデルとプロバイダの選択は Host の resolver を通ります。
- **権限の制限と共有ロック:** 子のツール権限は常に親ポリシーの部分集合になります。新しい子は起動時に自身のワークスペースの `AGENTS.md` を読み直し、resume・restart ではその子に記録した snapshot を再生します。親と共有する `MutationStateDirectory` によりファイルロックの整合性を保ちます。

### 子プロセス側 (`unreal-agent child --stdio`)

`subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory)` を実行し、`Lifecycle="child"` の子 Host を開きます。子にはテンプレートで許可したツールに加え、親への連絡用 `SendParent` と完了報告用 `Finish` を提供します。`lead` のようなテンプレートは孫を起動するツールも持ちます。

---

## 2. 通信プロトコル・永続化・完了判定

- **決定論的な Child ID:** 親 Session ID と `SubagentStart` の公開 Operation ID から一意に導出されます。
- **フレーム付き JSON 通信:** 標準入出力パイプ上で、4バイトのビッグエンディアン長プレフィックス＋バージョン付き JSON（長さは uint32）で通信します。
- **永続化後の ACK 応答 (`InputPeer`):** 親子間のメッセージは正規の `InputPeer` レコードとして `AppendInput` がコミットされた後にのみ ACK を返します。同じペイロードでの再送は元の Receipt を返します。
- **親の読み取り境界**。作業中の親には、ツールの実行後、次のモデルリクエストで蓄積した `InputPeer` を受信順に届けます。その応答で読み取りを確定し、復元時にも確定済みの入力を新着として扱いません。`ready` はモデル入力から省き、進捗はモデルを呼ぶ機会に同梱します。どちらも待機中の親を起こしません。
- **依頼文と振り分け。** 起動時の Operation チェックポイントと子の設定に同じ依頼文を記録します。`SendParent` の本文が `brief <field>` に完全一致し、その欄に値があれば、子の Operation の receipt に値、規則 ID、参照位置を記録します。空欄や他の質問は通常どおり親の inbox に届きます。終了通知やツールの完了結果には正規の Finish 報告の全文が入り、受入は親が判断します。 [委譲の依頼文](../../usage.md#委譲の依頼文)
- **親ターンの失敗。** 親のモデル呼び出しがエラーを返すと、そのターンが終了します。子は実行を継続し、連絡や完了報告は親の inbox に保存されます。承認待ちは次の親ターンに引き継がれます。ユーザー入力、起床対象の子メッセージ、または既存の利用上限解除待ちの期限で親が再び動きます。失敗したリクエストの入力は履歴に残り、次に親が起床したときに参照できます。
- **明示的な `Finish` 完了契約:** ツールを使える子の成功報告は `Finish` で記録します。他の呼び出しや Operation が残る間は拒否されます。Finish なしでターンを終え、継続手段がない子は、モデルターンの ID と stop 理由を持つ正規の `HostRecord(kind=finish)` に失敗として記録します。空応答・推論だけ・拒否・出力上限・本文だけの応答も対象です。ツール・承認・子の実行中、または progress ではない `SendParent` の返答待ちは維持します。出力上限の自動再試行はせず、次の依頼は親が判断します。text-only child の既存の完了契約は維持します。
- **段階的なキャンセル処理:** キャンセル時は制御入力と `canceling` チェックポイントを保存したうえで通常停止を要求し、2秒以内に終了しない場合はプロセスグループごと強制終了します。
- **Host 再起動。** 親は書き込みロックを保持する子に再接続し、元の実行の完了を観測します。実行主体を失った子は `ended_by=host_restart` として記録され、親への終了通知は1件にまとまります。
- **チャネル識別と再送の整合性:** 各通信チャネルは connection・sender・target の ID を固定し、古いチャネルや不一致のフレームを拒否します。同じ `InputID`・同一 payload の再送は元の receipt を返し、異なる payload は競合（conflict）として拒否されます。`ready` や進捗通知は最終完了とは別経路であり、完了には正規の `Finish` レコードが必要です。

---

## 3. オペレーター制御 API

- `ReadChild` — 子の正規履歴ページ、Operation 一覧、`ChildConfig`、および `Finish` 状態を読み取ります。
- `SteerChild` — 稼働中の子へ追加指示を送ります（パイプへ直接書き込まず、親の永続的な operation-intent 入力として記録されます）。
- `CancelChild` — 稼働中の子をキャンセルします。
- `ResumeChild` — 稼働中の親 Host が未完了の子を所有・復元していることを確認します（二重にプロセスを起動することはありません）。

> **制御リクエストの検証:** すべての制御要求は、現在の親 ID / generation、正確な `SubagentStart` Operation と子の組み合わせ、および空でない安定した `InputID` を検証します。すでに終了（`completed` / `failed` / `canceled`）した子を再開（resume）することはできません。
