# 文脈の3層

Provider 非依存の Host API で生記録を読む L3 と、現在の状態を扱う L2 を提供します。3層設計の進行は [Issue #41](https://github.com/TomyFovos/unreal-agent/issues/41) にあります。

## 正本を読む

`record show LOCATOR` は locator が指す記録を読みます。`record find TEXT` は Session と配下の子の確定記録を検索します。アーカイブ済みの子も対象です。ネイティブツール名は `RecordShow` と `RecordFind` です。

- `rec:<session>/<sequence>` は改行を含む JSONL の Item をそのまま返します。接尾辞でユーザー・Peer・統括の原文、ツール呼び出し、子の Finish、保存した成果物を指定できます。子の Finish には handle が付き、`summary`、`tests`、`changed-files`、`blockers`、記録されている `next` を指定できます。
- `rec:<session>/output/<operation>/<stream>` は終了した Operation の出力を指します。シェルの標準出力・標準エラーは既存の capture ファイルから読みます。Remote Job は表示用の値と原文が異なる場合に元のバイト列を保持します。原文のない旧記録は取得不能を明示します。独立した Operation チェックポイントにも JSONL 上の locator があります。
- `rec:<session>/<sequence>/tool-error` は Operation を始める前に失敗したツール呼び出しのエラー原文を読みます。
- `Session.RecordArtifact` は明示的に指定されたファイルの版を保存します。locator はパスと SHA-256 を持ちます。保存された版はワークスペースで上書き・削除した後も読めます。
- `?bytes=<開始>-<終了>` は 0 始まりの半開区間です。`?line=<行番号>` は 1 始まりで改行も含みます。呼び出し元がページのバイト数を指定し、明示された継続 locator をたどります。0 は選択範囲の全体を返します。
- 検索は記録の種類、handle、時刻範囲（開始を含み、終了を除く）、大文字・小文字を区別する部分文字列で絞れます。抜粋は一致箇所、先頭行、または構造化された記録の字句を原文のまま返します。時刻で絞るには記録時刻が必要です。

`record show` は選んだバイト列をそのまま返します。`--json` または `--page-bytes` を指定すると、テキストか base64 と、続きがある場合の locator を JSON で返します。`record find` は一致した記録を JSON で返し、`--kind`、`--handle`、`--from`、`--before` を使えます。時刻は RFC3339 形式です。

L2 の処理済み件数は `rec:<session>/context/<slot>` を参照します。子の一覧には handle、状態、Operation と報告の locator が載ります。他の項目の一覧には出典の locator が載ります。

ファイルを参照する際は、その時点の Session の読取り Permission に従います。Session 履歴には既存の正本・アーカイブの読取り経路を使います。入力の受領記録と観測値は出典の locator を持ちます。ターン途中で渡した入力の locator は継続記録に残り、到達確認は既存の input watermark と応答で行います。

## 現在の状態を再生する

L2 の13項目は `session`、`requests`、`rules`、`plan`、`decisions`、`children`、`approvals`、`waiting`、`questions_to_user`、`artifacts`、`issues`、`recent_events`、`metrics` です。各項目は出典の locator を持ちます。進行中の本人の依頼は予算に収まる限り原文を写し、混載入力は alias と出典を保持します。処理済みの項目は件数と L3 の一覧への locator に集約します。L2 の予算を超えた場合は、`record show rec:<session>/context/state` で記録された状態の全体と出典を、`record find` で過去の版を含む個々の記録を取得できます。

S6 には未終了の子と受入待ちの終了済みの子が載ります。明示的な受入・差し戻し・取消があれば処理済みです。過去の依頼で終了した未レビューの子は、親が終了報告を含むリクエストに応答した時点で処理済みとします。終了通知、子のツール結果、記録済みの `show` や `wait` の結果が報告の配信を示します。Operation の終了チェックポイントだけでは受入待ちが続きます。現在の依頼中に終了した未レビューの子は報告を読んだ後も受入待ちに残ります。処理済みの子は template・model・終わり方で集計します。レビューの統計は明示的な記録に基づきます。

S10 には S6 に載る子が報告したパスと、最後の意味上の区切り以降に統括が書いたパスが載ります。初回の区切りまでは Session 開始以降の統括の書き込みが対象です。複数の書き手が同じパスを使った場合は、最後の書き手を付けて1件とします。他のパスは件数に集約します。

`Session.AppendContextEvent` は明示的な `context_state_event` を会話と同じ JSONL に追記します。`Session.ContextState` は独立したスナップショットを返します。reducer は同じイベント ID を一度だけ適用し、追記を再試行すると既存の locator が返ります。復元チェックポイントは状態と適用済み ID を保存し、正本の全再生とチェックポイント以降の再生は同じ状態のバイト列を生成します。fork では元のイベントを証跡として引き継ぎ、現在の状態は新しい Session のイベントから作ります。

書き込み時と再生時に、同じ入力・応答・承認記録・Operation チェックポイントから機械イベントを作ります。子の開始には記録済みの brief を含めます。進捗と観測した振り分け規則の ID は直近の出来事に入ります。終了した子に Finish があれば、その locator が付きます。旧記録に Finish が埋め込まれている場合は、チェックポイントの locator から読めます。S7・S8・S11 には未処理の承認・待ち・問題が載り、解決、後続の記録による置き換え、関係する子の終了で処理済みになります。S9 には次の本人入力または明示的な回答の関連付けまで、行頭に `【要確認】` のある質問が載ります。次の本人入力で L2 の待ちは解消し、回答との対応付けは統括が後から記録できます。

成果物のパスは Finish の changed files と、成功した書き込みの受領記録から取り込み、出典を付けます。明示的なファイル保存は、その版への locator を記録します。内容は保存した版または通常のファイル読取りツールで取得できます。

未対応のイベント版・種類は正本に保持し、reducer は認識できる更新を適用します。`state show` と `StateShow` で最新の L2 を確認できます。

## Responses のウィンドウを再構築する

入力全体が W に達すると最初の再構築を行います。長時間稼働した Session を更新した直後のリクエストも対象です。区切りごとに system、ツール定義、証拠 locator 付きの L2 の写しを固定します。L2 では進行中の依頼、未処理の承認・質問・待ち、稼働中の子を優先します。末尾が予算に収まる場合は最後の本人発言から引き継ぎます。超える場合は進行中のターンまで開始点を進め、必要に応じてそのターンの配信済みバッチも L3 に移します。未配信入力と、到達確認前のツール呼び出しと結果のまとまりを保持します。L1 の冒頭には L3 にある過去の会話の locator 範囲を示します。

再構築後は `max(1, floor(PostRebuildRatio × min(W, 入力予算)))` トークンを目指します。必要に応じて長いツール出力とアシスタント発言を抜粋し、この段階では受信したメッセージを原文で保持します。抜粋には先頭・末尾と L3 の locator が付き、抜粋量がゼロの場合は省略表示と locator が残ります。必須部分は目標を超えても入力予算の範囲で保持します。入力予算に収める段階では受信メッセージも抜粋対象になります。画像は元の型と内容を保持します。最小の投影でも入力予算に収まらない場合は推論前にエラーを返し、ヘッダー、L2、末尾のサイズを表示します。

再構築後は会話を記録順で末尾に追加し、前の区切りからの推定増分が W に達すると次の再構築を行います。入力予算の超過やヘッダー変更でも切り替わります。プロバイダ側の上限超過では即座に再構築し、1回だけ再試行します。長いツール結果には先頭・末尾の抜粋と、元のストリームや受領記録の locator を付けます。画像は元の型を保ちます。長い原文は `RecordShow` のバイト範囲やページ指定で抜粋量に収まる大きさに分けて取得できます。

`window_boundary` は起点、写しの取得時点、保持範囲、前後の入力トークン数、必須部分が収まったかを記録します。トークンの推定方法と、再構築前に実測プレフィックスを使ったかも記録します。直後の最初の Turn が区切りを参照し、その応答が provider 報告のキャッシュ read と write を記録します。予算に合わせた境界は `Fit: excerpted`、入力予算、抜粋前の必須入力と末尾のサイズ、選んだ文字数と適用範囲を記録します。復元チェックポイントからも同じ L2 の写し、末尾、抜粋を再生します。

`runtime.json` の `Context` で次を設定できます。

| キー | 既定 | 意味 |
| --- | --- | --- |
| `WindowThresholdTokens` | `200000` | 初回のしきい値と次の区切りまでの増分を決める正の値 W。 |
| `PostRebuildRatio` | `0.5` | 再構築後の目標を決める比率。0 より大きく 1 未満です。 |
| `L2TargetTokens` | `5000` | 再構築時の L2 のトークン予算。収まらない出典付きの項目は L3 で取得できます。 |
| `RecentEventCount` | `0` | 前の区切り以降、初回は現在の本人の依頼が始まってからの出来事を写します。正の値で直近 N 件を選べます。出来事の全履歴も取得できます。 |
| `ToolOutputExcerptChars` | `0` | ツール既存の表示量を使います。正の値でテキスト部分ごとの先頭・末尾の合計文字数を指定します。 |
| `CacheRewarmMinutes` | `0` | PR7 で比較する任意の温め直し間隔を記録します。実行時の要求は既存の Session の動作に従います。 |

`Context.Routes` にプロバイダ ID を指定すると、表の設定と `InputBudget`、`FallbackWindow`、`ResponseReserve`、`ProtocolReserve` を上書きできます。子 Session は共通設定、現在選択しているプロバイダ設定、`Context.Routes.<provider>.Roles.<role>` の順に適用します。`<role>` には起動時に確定した `lead`・`worker`・`collector` などの役割が入ります。メイン Session には共通設定とプロバイダ設定が適用されます。省略した項目は直前の設定を継承します。各実効設定の `L2TargetTokens` は、W と比率で計算した再構築目標以下に収めます。モデルの入力予算やヘッダーの占有量に応じて、実際の L2 予算はさらに小さくなります。

たとえば、Host の設定ファイルの `Context` に次の値を追加します。

```json
{
  "Context": {
    "WindowThresholdTokens": 200000,
    "PostRebuildRatio": 0.5,
    "L2TargetTokens": 5000,
    "Routes": {
      "claude": {"WindowThresholdTokens": 180000, "PostRebuildRatio": 0.6},
      "opencode-go": {
        "L2TargetTokens": 3500,
        "Roles": {
          "lead": {"WindowThresholdTokens": 64000},
          "worker": {"WindowThresholdTokens": 96000}
        }
      },
      "openai-codex": {"WindowThresholdTokens": 220000}
    }
  }
}
```

### 稼働中の Host で調整する

Host が使っている `runtime.json` 全体を一時ファイルに保存し、同じディレクトリ内で置き換えます。Host と稼働中の子は、次のリクエスト準備時または Claude のツールバッチ後に `Context` を読み直します。無効な編集は警告を記録し、直前の有効な値で動作を続けます。次の判定から新しいしきい値を使い、再構築の条件を満たすまでは現在のウィンドウを使います。新しい Session は初回のウィンドウに適用値を記録し、実効値の変更ごとに `context_policy` イベントを残します。旧ログから再開した Session は次の判定で初回の適用値を記録します。

`Context.WindowStrategy` は起動時に廃止済みと1行ログに記録し、設定には反映しません。旧 Claude の圧縮しきい値も互換性のために読み込めます。

## Claude Code のウィンドウを再構築する

サブスクと明示的に選択した API キーの経路にも同じ区切り条件と末尾の選択を適用します。区切りでは新しい `--session-id` で `claude -p` を起動し、従来の system 追加指示、L2 の写しと L1 の末尾を渡します。署名付き思考はディスク上の旧会話に残ります。

`provider_conversation_binding` に Unreal Session の区切りと Claude Code の会話 ID の対応を記録します。区切りの間と Host 再起動後は、その ID を `--resume` に渡して新しい入力を追記します。子、承認待ち、未回答の質問、利用上限待ちは Unreal が管理します。

メインの `PostToolBatch` では、承認とツールの完了を待ち、結果を記録してから、直近のモデル呼び出しの入力トークン数を確認します。キャッシュの read と write も合算します。再構築の条件を満たすと、待機中の入力を新しい窓に含めて `mid_turn` の区切りを記録します。`continue: false` と `stopReason` を返し、CLI がターンを終えてから新しい会話を開始します。窓の内側では `PostToolBatch` が同じ会話の次の呼び出しに入力を渡します。最終回答で再構築の条件を満たした場合は、次のリクエストで切り替えます。 [Claude Code の PostToolBatch フック](https://code.claude.com/docs/en/hooks#posttoolbatch)

入力予算の判定では、組み立てたウィンドウ本文と system・ツール定義のトークン数を推定します。入力実測があるときは、直近のモデル呼び出しの総入力に推定差分を加えます。境界後のキャッシュ記録には、同じターン内に後続のモデル呼び出しがあっても、メインの最初の呼び出しの read・write を記録します。

各ターンの起動環境と `--settings` に `DISABLE_AUTO_COMPACT=1` と `DISABLE_COMPACT=1` を設定します。CLI の `Prompt is too long` も、通常の上限超過と同じように再構築して1回だけ再試行します。 [Claude Code の環境変数](https://code.claude.com/docs/en/env-vars)

## 統括の判断を記録する

Responses 系はネイティブツールを使います。Claude Code の統括は、Session のシェルから `unreal-agent subagent` と同じ Host の実行ファイルとソケットを使います。Read Only でも使えます。証拠の読取りには Session の通常の Permission が適用されます。

| `unreal-agent` に続ける CLI | ネイティブツール | 操作 |
| --- | --- | --- |
| `state show` | `StateShow` | 未回答の質問 ID と locator を含む現在の状態を読む。 |
| `state decide TEXT [--why TEXT]` | `StateDecide` | 決定、任意の根拠、退けた案、関連 ID を記録する。 |
| `state plan ID --text TEXT [--status STATUS]` | `StatePlan` | 手順を追加・更新する。順序、担当、依存先、決定 ID を指定できる。 |
| `state rule add EXCERPT --source LOCATOR` | `StateRuleAdd` | 指定した本人発言から原文の引用を登録する。 |
| `state accept HANDLE` または `state reject HANDLE [--why TEXT]` | `StateAccept` または `StateReject` | 自分の子の受入・差し戻しを記録する。理由は任意。 |
| `state question TEXT [--id ID] [--source LOCATOR]` | `StateQuestion` | 質問を手記録する。ユーザーには本文で質問する。既存の統括発言も出典に指定できる。 |
| `state answer ID --source LOCATOR` | `StateAnswer` | 未回答の質問に本人の回答を結び付け、待ちを解消する。 |
| `state import SESSION --sessions DIR` | （CLI のみ） | 別の Session の記録を読み取り専用で再生し、現在の規則・決定・未完了の手順・未回答の質問を、この Session の L2 に取り込む。ID には元の Session 名を前に付け、locator は元の Session を指したまま残す。同じ取り込みを繰り返しても状態は変わらない。 |

`state question answer` も使えます。テキストに `-` を渡すと標準入力から原文を読みます。各コマンドの `--help` でフラグを確認できます。

`state accept`・`state reject` と `subagent review HANDLE accept|reject [--reason TEXT]` は同じ正本にレビューを記録します。L2 はそのレビューから子の状態を更新し、`/analyze` はレビュー済みの子を分母として差し戻し率を計算します。子の実行結果は独立した記録です。

<a id="measuring"></a>
## 計測

`/analyze` の **文脈（Context）** で入力と W、境界直後と定常時のキャッシュ読取率を比較できます。**書き出し（Export）** の JSON と Markdown に、アーカイブ済みの子を含む全体の計測値が入ります。方式（`three-layer`・`historical summary`）、プロバイダ、モデル、effort、Fast ごとの内訳があります。記録された値は `measured`、計算や推定で得た値は `derived`、欠損は `unknown` です。JSON では `unknown` の値を `null` で表し、既知の集計値にも欠けた観測の件数を付けます。

文脈ビューではルートと役割ごとの最新の適用値と、各 Session の適用期間を表示します。期間ごとに設定値、再構築回数、再構築直後の平均入力サイズ、最初のリクエストのキャッシュ読取率、L3 の show・find 回数を比較できます。ウィンドウの計測は、そのウィンドウを組み立てたポリシーに集計します。次の再構築をまたいで完了した L3 呼び出しは、開始時のウィンドウの件数に含めます。ポリシー記録が始まる前の再構築はポリシー不明として表示します。

| 計測項目 | 比較できる値 |
| --- | --- |
| 境界 | `threshold`、`over_budget`、`header_change`、`mid_turn`、プロバイダ側の上限超過による件数です。1つの境界に複数の理由が付きます。境界前後の入力、system とツール定義のトークン数、L2 の写しと目標、L1 末尾のトークン数と項目数、W、必須部分が目標を超えたかを表示します。直近イベントや抜粋の設定、連続する境界の間のリクエスト数と経過時間も表示します。集計行では既知のサイズと間隔の平均を表示します。 |
| 予算に合わせた抜粋 | 入力予算、抜粋前の入力全体と末尾、抜粋後の入力と末尾、選んだ文字数を表示します。 |
| キャッシュ | 境界直後の最初のリクエストと窓内の後続リクエストについて、読み込み・書き込み・キャッシュ外の入力を表示します。Claude Code はメインのストリームごとに数えます。ヒット率は、両方の報告があるリクエストのキャッシュ読み込み合計を入力合計で割ります。定価換算額は入力・キャッシュ読み込み・書き込み・出力の内訳を表示します。 |
| 旧方式の記録 | 旧 CLI と Unreal の圧縮の結果、入力サイズ、出力使用量と所要時間を表示します。旧方式への切り替えも記録されたサイズを確認できます。旧記録は正本として読め、次の推論では3層の窓を再構築します。 |
| L3 | 窓ごとの `record show` と `record find` の回数、ページ本文・抜粋のバイト数です。前の L1 末尾を参照した率は、照合できた呼び出しを分母とし、該当する locator が1件でもあればその呼び出しを1回と数えます。行やバイト範囲で指定しても同じ原文として照合します。 |
| L2 | `state` の決定・計画・規則・受入・差し戻し・質問の記録数と、正規履歴の再生で得た機械イベントの種類別件数です。境界では未回答のユーザー・子の質問と保留中の承認を表示します。 |
| 子のレビュー | 委任時の方式と子のモデルで集計し、子自身の方式も分かる場合は表示します。差し戻し率の分母は正常終了し、明示的な受入・差し戻しがある子です。正常終了した未レビューの子には別の件数を表示します。 |
| ユーザーへの質問 | `【要確認】` で始まる表示対象のアシスタントメッセージを方式ごとに数えます。作業時間は各 Session の記録済みターンとツールの時間区間を重複なく足します。ツリーでは並行する子も含め、Session ごとの作業時間を足します。 |

サブスク利用率は、取得できた利用枠・割合・リセット時刻を観測時点の値として表示します。定価換算は価格表に基づく USD 表示です。 [Codex の利用枠ヘッダー](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs) · [Claude Code の利用枠イベント](https://code.claude.com/docs/en/agent-sdk/typescript#sdkratelimitevent)

### 候補をオフラインでリプレイする

```bash
python3 scripts/context_replay.py ./session-copies --since 2026-11-24T00:00:00+09:00 --config ./candidate/runtime.json
```

Session ログのコピーを入力にします。`candidate` にはルートごとの実効値、設定された役割の `role_policies`、全期間・検証期間それぞれの試算が出ます。リプレイは子の起動設定またはポリシー記録から役割を取得します。役割の記録がないログにはプロバイダ設定を使います。`--request-csv` は同じ候補を使い、子の役割も出力します。従来の集計名 `codex` と `deepseek` には、それぞれ `openai-codex` と `opencode-go` の値を適用します。試算は設定された W、比率、L2 上限、入力予算を使い、投影後のサイズやキャッシュ挙動、費用をリクエストの記録から推定します。
