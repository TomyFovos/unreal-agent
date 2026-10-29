# Codex の追記型コンテキスト

Canonical Session history が原本。Codex の入力は `deriveItems` が記録順に導出し、
省略・圧縮と Provider／モデル変更の境界以外では、前回の入力に末尾を追加する。
Claude Code 自身の会話の圧縮は別の経路で行う（[Claude の経路](claude-route.md)）。
`Respond` が返り、モデル呼び出し・未完了のツール／Operation がない待機状態で、
直前の文脈が `Context.ClaudeCompactThresholdTokens`（既定 200,000）を超えていれば、
同じ Claude Code 会話へ裏で `/compact` を送る。閾値は暫定で、本番の観測で見直す。
ユーザー入力や子の知らせで次のターンが始まるときは圧縮プロセスを停止・回収し、
圧縮前の会話から本来の入力を送る。中断を短く記録し、次の待機時にまだ閾値を
超えていれば再試行する。同じ会話への Claude Code の起動は直列化する。
ターン開始時に待って圧縮するのは、直前の文脈が閾値の2倍を超える場合だけ。
ヘッダーとメインのカードにはスピナー付きで「会話を圧縮中」を表示し、入力欄は
使えるままにする。成功時は圧縮前後のトークン数を Session に残し、Unreal の原本は
置き換えない。
ゲージの「文脈」も最後の `message_start` の input + cache_read + cache_creation
を使う。区切りの間に複数回呼んでも入力を合計せず、キャッシュ率と `/analyze` の
累計だけは従来どおり呼び出しの合計を使う。

- instructions は基本文、AGENTS.md の保存済みスナップショット、モード別固定文。
  Responses API では既存どおり先頭の system message として送る。
  tools は名前順・決定的 JSON。指示／ツール変更は canonical context record に残す。
- 各項目のモデル表現と Responses 表現をメモ化する。毎回の検索、途中への挿入、
  並べ替え、リクエストごとの予算による削除、quoted 再試行は行わない。
- 動く状態は実行中 Operation の短いメッセージとして、変化したときだけ末尾へ記録する。
  時刻・履歴カーソルは instructions に入れない。
- 大きなツール結果は記録時に既存の `max_output_length`（既定 40,000 文字）で
  先頭・末尾を固定し、Unit ID による canonical history の参照を添える。
  元の receipt／Operation／既存の出力ファイルは保持する。
- 同じ Provider／モデルの推論は encrypted reasoning と item ID を再送する。
  切替を記録した境界以前だけ portable 表現にする。

ツール結果は区切り（epoch）の間は追記だけで保持する。リクエスト直前の
`PrepareContext` は、新しい結果から合計 `ToolResultKeepTokens`（既定 15,000）に
達する結果までを直近として残す。それより古い未省略の結果を短い表現にした場合の
差分が `ToolResultMinPruneTokens`（既定 15,000）以上たまったら、一度に省略する。
値は既存の context 設定に置く。見積もりは既存の保守的 JSON バイト数を使い、
実測入力がある場合は、その Turn のリクエスト見積もりとの比率でトークン量に補正する。

直近でも、同じファイルの後の再読込・編集によって古くなった読み込みは、その区切りの
候補に含める。`read` と単一ファイルの単純な `sed -n 'A,Bp' PATH`、`cat PATH`、
`head`／`tail`（任意の `-n N`）、`nl -ba PATH`、これらへの `| sed -n ...` だけを
読み込みとして判定する。展開・複数ファイル・複雑な Bash は推測しない。
編集は `edit`／`write`／適用する `ast_edit` と、Operation が記録した実際の変更先を使う。
失敗した再読込やプレビューは、古い読み込みを置き換えた根拠にしない。

一つの `prune` ContextRecord に複数 Unit の置換を保存し、保存後に使う。
呼び出しと結果の対・call_id を保ち、結果本文だけを英語1行にする。例:
`[Output omitted to keep the context small: Bash "sed -n '1,200p' src/foo.go" (812 lines). Run it again if you need it. Canonical Unit ID unit-….]`
シェルの出力ファイルが記録されていれば、そのパスも添える。原本と記録時の秘密除外は
維持する。未受領の継続、実行中 Operation の結果、推論、モデル発言、ユーザー・親入力は
省略しない。再起動後も同じ記録から同じ導出を復元する。旧1 Unit形式の `prune` も読める。
区切りで1回だけ入力の先頭一致が切れ、その次からまた追記になる。Codex のメイン・子に
同じ処理を使い、Claude 経路には適用しない。

圧縮の閾値は `min(0.8 × W, W − 最大出力 − 64,000)`。
既存の明示的 InputBudget が低ければそれも使う。W が不明なら既存の既定値を使い、
直前の実測 input tokens、未取得なら JSON バイト数による保守的見積もりで判断する。
最大出力の指定がない場合は既存の ResponseReserve を使う。

通常の epoch 省略はこの閾値より前に行う。閾値を超えた場合は既存どおり、
まず古い、受領済みの大きな結果を短い参照へ置き換える記録を作る。
それで収まらなければ、同じ instructions・tools・導出済み入力の末尾に要約指示を
一件足し、同じ Provider／モデル／Session の cache key で要約する。
置換範囲と要約を canonical context record に保存し、窓の約 16% の直近履歴と
未受領の継続に必要な呼び出し・結果を残す。過去の原本は消さない。
文脈超過エラーでは閾値にかかわらず圧縮し、通常リクエストを一度だけ再試行する。
要約後も必要な入力が入らなければ `required_context_exceeds_budget` を返す。

既存の継続・受領 ACK、中断時の固定結果、秘密の除外は維持する。
旧フィールド・チェックポイント付き Session は移行せず読める。
新しい view の記録も復元時に順番に適用する。旧 retrieval 設定フィールドは
既存設定の読み込み用に残すが、導出には使わない。

Split の統計、Session 分析と Markdown エクスポートに、直近と累計の
`cache NN%`（cached input / input）を表示する。未報告は `cache -`。
Session 分析（`/analyze`）と分析・会話の Markdown エクスポートには、報告のある
呼び出しの平均入力トークン（要約呼び出しを含む）、省略した結果数、正味の推定削減
トークン合計、`prune` の区切り回数も出す。記録時の head/tail 制限は省略件数に数えない。
旧 `prune` に削減量の記録がない場合、件数と区切りだけを集計する。
95% は実機の観測で確認する目標であり、キャッシュ保持や新規入力の割合により変わる。
