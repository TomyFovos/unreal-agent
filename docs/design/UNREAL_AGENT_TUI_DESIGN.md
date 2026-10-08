# Unreal Agent TUI Design Specification

対象: `bin/unreal-agent attach` の対話型ターミナルUI（Linux / macOS / WSL）
性質: デザイン仕様書のみ。実装方法・コード・API変更は含まない。
前提: TUIは Host の disposable projection。表示する値はすべて既存の projection（tui Snapshot / host.View / viewer Row）から得られるものに限定する（付録A参照）。

---

## 0. 読み方

- UI上の文言（コピー）は英語で定義する（現行UIと同じ）。本文は日本語。
- モックアップ中の記号の意味:
  - `█` = 端末のハードウェアカーソル位置
  - 行頭が `…` だけの行 = 省略された行
  - モックアップは色を表現できないため、スタイルは各節の表（7.2〜7.5）で定義する
- 幅・高さは「列×行」。全モックアップは右端1列を空けている（7.6 の安全余白規則）。
- 幅広モックアップ（120/160列）は、Markdownビューアでの桁ずれを避けるため英語の会話例を使う。実際の日本語表示は 7.7 の規則（CJK=2セル）に従う。

---

## 1. Design goals

| # | 目標 | 意味 |
|---|---|---|
| G1 | 会話が主役 | 典型状態の80x24で、会話領域が画面行の60%以上を占める。 |
| G2 | 「今」を一目で | ライブ状態（AIの作業・ツール・child・接続）は常に決まった位置に出る。読まなくても位置と記号で分かる。 |
| G3 | 平常は静か、異常は明確 | 正常状態は dim / 記号のみに圧縮してよい。異常状態は必ず単語で書き、太字＋色で出す。 |
| G4 | canonical と temporary を視覚的に分離 | ストリーミング中のドラフト、送信中表示、通知は、canonical historyと見間違えない形で出す。 |
| G5 | 並列エージェントは「レーン」 | child は位置が動かない行（レーン）として並び、並列に進んでいることが自然に分かる。 |
| G6 | キーボードのみで発見可能 | `/` で始めればコマンド候補が出る。キーヒントは文脈で変わり、Ctrl-C の「今の意味」を常に示す。 |
| G7 | 端末ネイティブ・幅安全 | 箱・カード・背景色を使わない。空白と字下げで構造を作る。CJK・曖昧幅・NO_COLOR・ASCIIでも崩れない。 |
| G8 | projection として誠実 | 分からない値は「unknown / ? / 非表示」。推測で補わない。UIは状態を所有しない。 |

非目標: マウス操作、Web的カードUI、新しいbackendデータ、既存コマンド/ショートカットの意味変更。

---

## 2. Current-state observations

現行 `Render` / `Panel.Render` と `CURRENT_TUI_SAMPLE.txt` から観察した事実。

### 2.1 課題

1. **視覚的な重みが全要素で同じ。** 色・太字・dimが無く、ヘッダー・本文・ツール・ヒントが同じ強さで並ぶ。
2. **ヘッダーの状態が文章になっている。** `stopped; /resume continues this session` や `gap/disconnect; resyncing` のように、状態の種類と次の行動が1つの文字列に混在し、接続状態とランタイム状態の区別がつかない。
3. **話者の境界が1行目にしか無い。** `you>` / `agent>` は先頭行だけに付き、折り返し行は0桁目から始まる。長文では誰の発言か分からなくなる。
4. **折り返しが単語を考慮しない。** grapheme単位で幅いっぱいに切るため、英単語やパスが途中で切れる。コードブロックの区別も無い。
5. **一時progressとcanonicalの区別がラベルだけ。** `[streaming (temporary)]` が本文末尾に付くだけで、thinking / waiting / retry の区別も無い。
6. **Operationが全件・ID順で常時並ぶ。** `tool> op-ID TOOL STATUS` が最新snapshotの全Operationについて出るため、完了済みも残り続け、セッションが長いほど会話を押し出す。op IDは利用者にとって意味が薄い。
7. **Child panelが常時コマンド一覧を出す。** child 0件でも `Children: /child ID /child-history ...` が1行を占める。
8. **親セッション行がプロンプトに見える。** サンプル最終行 `> codex-smoke-1 | runtime: running | ...` は「選択中」マーカー `>` だが、入力プロンプトと紛らわしい。しかもセッションの elapsed / usage はこの行にしか無い。
9. **結果・エラー・ヒントが1つのstatus文字列を共有。** `/help` も長い1行として最大6行に折り返される。
10. **フッターが固定文言。** Ctrl-C は「鍵入力の取消 → 待機の取消 → セッション停止」と文脈で意味が変わるのに、表示は常に `Ctrl-C stop`。
11. **入力欄は最終行だけ表示。** 貼り付けで複数行になると内容が見えない。カーソルは文字列に挿入した `▏` で、端末カーソルは非表示のまま。日本語IMEの変換窓がキャレット位置に出ない可能性があり、`▏` 自体も East Asian Ambiguous 幅。
12. **高さ不足時は本文の古い側から切り捨てるだけ。** スクロールバックは無い（docsに明記）。
13. **毎フレーム全消去して再描画。** ストリーミング中（最大約33回/秒）にちらつきうる。

### 2.2 維持すべき良い点

- 外部テキストの制御文字除去（SafeText）、grapheme単位の編集と幅計算
- bracketed paste は入力テキスト扱いで、`/` コマンドを実行せず、改行で送信もしない
- 資格情報のマスク入力と別経路送信
- 表示上限（1,024エントリ、1エントリ4 KiB）と、canonical historyは別に保持されるという明示
- gap / 世代変更 / 切断時の可視的なresync
- 描画と購読受信の分離、resizeでセッション状態を変えない

本デザインはこれらを一切弱めない。

---

## 3. Three design concepts

3案は「ライブ状態をどこに置くか」で分類した。

| 案 | ライブ状態の置き場所 | 一言で |
|---|---|---|
| A. Ledger | 時間軸（会話ストリームの中） | すべては時系列ログの1行 |
| B. Switchboard | 空間（常設の固定ペイン） | 状態は場所で決まるダッシュボード |
| C. Live Dock | 注意の位置（入力欄の直上、動いている間だけ） | 今動いているものだけが入力欄の上に浮かぶ |

### A. Ledger（時系列ログ一体型）

会話・ツール・child の開始/終了・状態変化を、時刻付きの1行として同じストリームに積む。固定領域は最下部の「状態入り罫線」と入力行だけ。

- 長所: 構造が単純で、どの幅でも崩れにくい。ログとして読める。色なしでも成立しやすい。
- 短所: 並列childの「今」が流れて消える。現在の状態を知るには読むしかない。ツール行とchildイベントが会話を押し流し、会話の読みやすさが落ちる。

### B. Switchboard（常設パネル型ダッシュボード）

ヘッダーにメーター、会話・Operations・Child agents・Selected を固定ペインで常時表示し、全コマンドを下部に並べる。

- 長所: 並列状態の一覧性が最大。監視用途に強い。
- 短所: 80x24では会話が数行しか残らない。アイドル時も空のペインが残る。表形式は未観測childのデータ欠落（tokens / activity が `?`）を露呈する。Webダッシュボード的で、常時のコマンド一覧はノイズ。

### C. Live Dock（会話＋ライブドック型）

会話はcanonicalな記録として主役。ツール呼び出しは会話の中に1行のレシートとして出る。ストリーミング中のドラフトは `⋮` の縦線で区別する。入力欄の直上の「ドック」には、稼働中のchildレーンとセッション状態だけが出て、何も動いていなければドックは0行になる。幅120以上でchildが存在すれば、childレーンは右側の「レール」に移る。

- 長所: 平常時は会話と入力だけで静か。作業中だけ情報が増える。並列childは位置が動かないレーンとして見える。
- 短所: レイアウト規則（優先度・行数予算）が多い。ドックの高さが変わるため、規則で動きを抑える必要がある。

---

## 4. ASCII mockups

同じ場面を3案で描く。ユーザーが子エージェント表示の整理を依頼し、エージェントが3つのchildを起動し、1つが完了、2つが実行中、親エージェントはドラフトを生成中。

### 4.1 A. Ledger — 80x24

<!-- mockup 80x24 -->
```text
unreal agent  codex-work-1

12:03:58    you   viewer の子エージェント表示を整理して、テストも追加して。
12:04:02  agent   了解しました。まず現在の描画処理を確認します。
12:04:03   tool   ✓ read harness/viewer/panel.go
12:04:03   tool   ✓ read harness/viewer/render.go
12:04:04   tool   ✓ grep "RenderRows" harness/viewer
12:04:09  agent   描画を3つの作業に分けて、子エージェントに並行して任せます。
12:04:10  child   ▸ child-7f3a started: Refactor RenderRows into columns
12:04:10  child   ▸ child-91b2 started: Add panel rendering tests
12:04:10  child   ▸ child-0c4e started: Update viewer README
12:05:12  child   child-0c4e: README の Panel 節を更新しました。
12:05:12  child   ✓ child-0c4e finished: success
12:06:40  child   child-91b2: panel_test.go に3ケースを追加しました。
12:07:31  agent ⋮ 待つ間に既存テストの構成を確認します。panel_test.go は
                ⋮ Render の出力文字列を直接比較しているため、列の変更には
                ⠹ generating  6s




── ⇄ connected  running  4m12s  in 48.2k out 3.1k  child 2 running ──────────
› █
  ⏎ send  / commands  ^C stop session  ^D detach
```

### 4.2 A. Ledger — 120x30

<!-- mockup 120x30 -->
```text
unreal agent  codex-work-1

12:01:15    you   viewer のテストを実行して、失敗があれば教えて。
12:01:16   tool   ✓ Bash go test ./harness/viewer/...
12:01:31  agent   harness/viewer の 14 テストはすべて成功しました。client_test.go の 1 秒タイムアウトは環境によって
                  不安定になる可能性があります。
12:03:58    you   viewer の子エージェント表示を整理して、テストも追加して。
12:04:02  agent   了解しました。まず現在の描画処理を確認します。
12:04:03   tool   ✓ read harness/viewer/panel.go
12:04:03   tool   ✓ read harness/viewer/render.go
12:04:04   tool   ✓ grep "RenderRows" harness/viewer
12:04:09  agent   描画を3つの作業に分けて、子エージェントに並行して任せます。
12:04:10  child   ▸ child-7f3a started: Refactor RenderRows into columns
12:04:10  child   ▸ child-91b2 started: Add panel rendering tests
12:04:10  child   ▸ child-0c4e started: Update viewer README
12:04:41  child   child-7f3a: RenderRows を列単位の組み立てに分割しています。
12:05:12  child   child-0c4e: README の Panel 節を更新しました。
12:05:12  child   ✓ child-0c4e finished: success
12:06:40  child   child-91b2: panel_test.go に3ケースを追加しました。
12:07:31  agent ⋮ 待つ間に既存テストの構成を確認します。panel_test.go は Render の出力文字列を直接比較しているため、
                ⋮ 列の変更にはテストの期待値更新が必要です。
                ⠹ generating  6s





── ⇄ connected  running  4m12s  in 48.2k out 3.1k  child 2 running, 1 done ────────────────────────────────────────────
› █
  ⏎ send  / commands  ^C stop session  ^D detach
```

### 4.3 B. Switchboard — 80x24

<!-- mockup 80x24 -->
```text
unreal agent  codex-work-1       ⇄ connected  running  4m12s  in 48.2k out 3.1k
── conversation ───────────────────────────────────────────────────────────────
   you   viewer の子エージェント表示を整理して、テストも追加して。
 agent   描画を3つの作業に分けて、子エージェントに並行して任せます。
       ⋮ 待つ間に既存テストの構成を確認します。
       ⠹ generating  6s
── operations ───────────────────────────────── 1 running  6 done  0 failed ──
  ▸ op-0012  edit  harness/viewer/render.go                                0:04
  ✓ op-0011  read  harness/viewer/panel_test.go                            0:00
  ✓ op-0010  grep  "RenderRows"                                            0:01
── child agents ────────────────────────────────────────── 2 running  1 done ──
  ID          STATUS   ELAPSED  TOKENS      ACTIVITY
  child-7f3a  running  3m40s    12.1k/1.2k  edit render.go
  child-91b2  running  3m38s    ?           ?
  child-0c4e  success  1m02s    3.2k/0.3k   README updated
── selected: child-7f3a ───────────────────────────────────────────────────────
  Refactor RenderRows into columns
  ▸ edit harness/viewer/render.go 12s   ✓ read 4   ✓ grep 1
  usage in 12.1k out 1.2k   finish: none yet
───────────────────────────────────────────────────────────────────────────────
› █
───────────────────────────────────────────────────────────────────────────────
/help  /stop  /resume  /retry  /child ID  /child-send TEXT  /child-cancel
^C stop session  ^D detach  /login  /logout  /credentials  /methods
```

### 4.4 B. Switchboard — 120x30

<!-- mockup 120x30 -->
```text
unreal agent  codex-work-1                                               ⇄ connected  running  4m12s  in 48.2k out 3.1k
── conversation ──────────────────────────────────────────────   ── operations ─────────────────── 1 running  6 done ──
   you   Run the viewer tests and tell me what fails.              ▸ op-0012  edit  harness/viewer/render.go     0:04
 agent   All 14 tests in harness/viewer pass. The 1s timeout       ✓ op-0011  read  harness/viewer/panel_test.go 0:00
         in client_test.go may be flaky on slow machines.          ✓ op-0010  grep  "RenderRows"                 0:01
                                                                   ✓ op-0009  read  harness/viewer/render.go     0:00
   you   Clean up how the viewer panel shows child agents, and
         add tests.                                              ── child agents ───────────────── 2 running  1 done ──
                                                                   ID          STATUS   ELAPSED  TOKENS
 agent   I'll read the current rendering code first.               child-7f3a  running  3m40s    12.1k/1.2k
         Splitting this into three parallel child agents:          child-91b2  running  3m38s    ?
         layout, tests, and docs.                                  child-0c4e  success  1m02s    3.2k/0.3k

 agent ⋮ While the children work, I'm checking how               ── selected: child-7f3a ──────────────────────────────
       ⋮ panel_test.go asserts on rendered output. It compares     Refactor RenderRows into columns
       ⋮ full strings, so column changes will need updated         ▸ edit harness/viewer/render.go          12s
       ⠹ generating  6s                                            ✓ read 4   ✓ grep 1
                                                                   usage in 12.1k out 1.2k    finish: none yet







───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
› █
───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
/help  /stop [idle]  /resume  /retry  /child ID  /child-history  /child-send TEXT  /child-cancel  /child-resume
^C stop session  ^D detach  /login PROVIDER ID  /logout PROVIDER ID  /credentials  /methods
```

### 4.5 C. Live Dock — 80x24（推薦案）

<!-- mockup 80x24 -->
```text
unreal agent  codex-work-1       ⇄ connected  running  4m12s  in 48.2k out 3.1k

   you   viewer の子エージェント表示を整理して、テストも追加して。

 agent   了解しました。まず現在の描画処理を確認します。
         ✓ 3 tool calls: read 2, grep 1

         描画を3つの作業に分けて、子エージェントに並行して任せます。
         ▸ spawn  child-7f3a  Refactor RenderRows into columns
         ▸ spawn  child-91b2  Add panel rendering tests
         ✓ spawn  child-0c4e  Update viewer README

       ⋮ 待つ間に既存テストの構成を確認します。panel_test.go は Render の出力文
       ⋮ 字列を直接比較しているため、列の変更には期待値の更新が必要になります。
       ⋮ 既存の比較方式を活かしつつ、
       ⠹ generating  6s

 child   2 running, 1 done                                   /child ID to focus
       ▸ child-7f3a  Refactor RenderRows into columns    3m40s  edit render.go
       ▸ child-91b2  Add panel rendering tests           3m38s
       ✓ child-0c4e  Update viewer README                1m02s  success
───────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send   / commands   ^C stop session   ^D detach
```

### 4.6 C. Live Dock — 120x30（推薦案、レールあり）

<!-- mockup 120x30 -->
```text
unreal agent  codex-work-1                                               ⇄ connected  running  4m12s  in 48.2k out 3.1k

   you   Run the viewer tests and tell me what fails.                              child agents       2 running, 1 done

 agent   ✓ Bash  go test ./harness/viewer/...                                        ▸ child-7f3a                 3m40s
                                                                                       Refactor RenderRows into columns
         All 14 tests in harness/viewer pass. The 1s timeout in client_test.go         edit render.go
         may be flaky on slow machines.
                                                                                     ▸ child-91b2                 3m38s
   you   Clean up how the viewer panel shows child agents, and add tests.              Add panel rendering tests

 agent   I'll read the current rendering code first.                                 ✓ child-0c4e                 1m02s
         ✓ 3 tool calls: read 2, grep 1                                                Update viewer README
                                                                                       finished: success
         Splitting this into three parallel child agents: layout, tests, and
         docs.                                                                       /child ID to focus a child
         ▸ spawn  child-7f3a  Refactor RenderRows into columns
         ▸ spawn  child-91b2  Add panel rendering tests
         ✓ spawn  child-0c4e  Update viewer README

       ⋮ While the children work, I'm checking how panel_test.go asserts on
       ⋮ rendered output. It compares full strings, so column changes will need
       ⋮ updated expectations. I'll keep the existing
       ⠹ generating  6s



───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send   / commands   ^C stop session   ^D detach   PgUp/PgDn scroll
```

---

## 5. Comparison

評価: ◎ 優れる / ○ 良い / △ 条件付き / × 不向き

| 観点 | A. Ledger | B. Switchboard | C. Live Dock |
|---|---|---|---|
| 会話の読みやすさ | △ ツール/childイベントが割り込む | × 80x24で会話3〜4行 | ◎ 会話が主役、ツールは1行レシート |
| AIの作業中状態 | ○ 末尾に出るが流れる | ◎ | ◎ pulse行が常に同じ位置 |
| ツールの見え方 | △ 全件が行として残る | ○ 表で常時 | ◎ 実行中は個別、完了は畳む |
| 並列childの理解 | × イベントが散在、今が分からない | ◎ 一覧表 | ◎ 位置が動かないレーン |
| アイドル時の静けさ | ○ | × 空ペインと全コマンド | ◎ 会話と入力だけ |
| 80x24 での成立 | ○ | × | ○ ドック予算で制御 |
| narrow（<70） | ◎ | × | ○ 圧縮規則あり |
| 既存projectionとの整合 | ○ | △ 未観測childの値が `?` だらけ | ◎ 親側Operationの状態を主に使う |
| NO_COLOR / ASCII | ◎ | ○ | ◎ 記号＋単語＋位置で成立 |
| 実装リスク | 低 | 中（ペインのサイズ計算） | 中（優先度と予算の規則） |
| 独自性（他CLIとの差） | 低 | 中 | 高（ガターラベル、⋮ドラフト、レーン、レール） |

---

## 6. Recommended design

**推薦: C. Live Dock**

理由:

1. **conversation first と低ノイズを同時に満たすのはCだけ。** Aは会話がイベントに押し流され、Bは会話を削ってダッシュボードにする。Cは平常時に会話と入力だけを残し、作業中だけ情報を足す。
2. **並列childがこの製品の要であることに最も素直。** childは開始順に並ぶ「レーン」で、行の位置が動かない。何本並走しているか、どれが終わったかが、読まずに形で分かる。幅があればレールに昇格して詳細が増える。
3. **projectionとしての誠実さを保てる。** レーンの状態は、常に取得できる親側のSubagentStart Operation状態とcanonical Finishを使い、未観測childのactivityやtokensを要求しない。Bのように欠損値の表を並べずに済む。
4. **canonical / temporary の分離を形で表現できる。** ドラフトは `⋮`、送信中表示と通知はドック、canonical historyだけが会話領域の通常行になる。
5. **既存のコマンドとキーの意味を変えずに、発見性を上げられる。** `/` 入力時の候補、Tab補完、文脈依存キーバーはすべてUIローカルで、Hostへの影響が無い。

---

## 7. Complete design specification

### 7.1 Screen anatomy

上から下への領域構成。各領域は条件を満たさないときは0行になる（ヘッダー、罫線、入力欄を除く）。

| 領域 | 行数 | 表示条件 | 内容 |
|---|---|---|---|
| R1 Header | 1 | 常時 | ブランド、セッションID、接続状態、ランタイム状態、elapsed、tokens |
| R2 Transcript | 可変（残り全部） | 常時 | canonical history（発話・ツール行・host行・child行）＋ドラフト |
| R3 Pulse | 0〜1 | エージェントの作業フェーズがあるとき（7.10） | Transcript領域の最終行に固定。スピナー＋フェーズ名＋経過 |
| R4 Dock | 0〜可変 | 中身があるとき | 上から: シート（コマンド候補 / help / child focus / コマンド出力）またはchildレーン → セッション状態行 → 通知行 |
| R5 Rule | 1 | 常時 | 入力欄の上の罫線。入力モードのラベルを左に、補助タグを右に埋め込める |
| R6 Composer | 1〜可変 | 常時 | ガターラベル（you / cmd / key）、区切り `›`、入力テキスト |
| R7 Keybar | 0〜1 | 高さ16行以上 | 文脈依存のキーヒント |
| R8 Rail | 右列 | 幅120以上かつchildが1つ以上 | childレーン（詳細版）と focus ブロック。R2〜R4 の右側に並ぶ |

**contextual help** は単独の領域を持たず、Keybar（R7）、Ruleラベル（R5）、入力欄プレースホルダー（R6）、`/` 候補とhelpシート（R4）に分散させる。

#### ガターと縦の軸（全領域共通）

Standard幅以上では、すべての行が同じ縦の軸に揃う。

```text
col: 1-6     7   8      9   10-
     label   sp  seam   sp  text
     "   you"    " "        viewer の子エージェント表示を…   ← canonical
     " agent"    "⋮"        待つ間に既存テストの…           ← ドラフト
     "      "    "⠹"        generating  6s                ← pulse
     " child"    " "        2 running, 1 done            ← ドック見出し
     "      "    "▸"        child-7f3a  …                ← レーン
     "   you"    "›"        █                            ← 入力欄
```

- **label**（1〜6列）: 右寄せの役割ラベル。`you` `agent` `child` `peer` `host` `error` `help` `cmd` `key`。
- **seam**（8列）: 1文字の状態溝。canonicalは空白、ドラフトは `⋮`、pulseはスピナー、レーンと実行中ツールは状態記号、入力欄は `›`。
- **text**（10列〜）: 本文。折り返し行も必ずこの列から始まる（ハンギングインデント）。
- ツール行はtext列から始まり、状態記号を本文側の先頭に置く（例: `         ✓ read  …`）。

### 7.2 Visual hierarchy

| レベル | 見せ方 | 対象 |
|---|---|---|
| L1 強 | bold（＋色） | 役割ラベル、異常状態の単語（disconnected / failed / stopped / error）、選択中のchild ID、入力欄ラベル、ブランド |
| L2 通常 | normal | 発話本文、実行中のツール行、レーンのIDとラベル、pulseのフェーズ名、失敗したツール行 |
| L3 弱 | faint (dim) | elapsed・tokens・時刻、完了したツール行と畳んだ要約、レーンのactivity、キーバー、罫線、プレースホルダー、host行、ヒント |
| 通常時は隠す | 表示しない | op ID、generation / revision、history cursor、input ID、project instructions のdigest（focusのTall時のみ表示）、コマンド一覧、正常な接続の単語（狭い幅では記号のみ） |

原則:

- 1画面で最も目立つのは「最新のエージェント発話」と「異常がある場合はその異常」。
- 色は意味を補強するだけで、単独では使わない（7.3）。
- 完了したものは時間とともに弱くなる（実行中 L2 → 完了 L3 → 畳まれる）。

### 7.3 Color system

ANSI 16色の前景色と属性のみを使う。背景色は使わない（反転表示を除く）。

| 役割 | 色（16色） | 使う場所 | NO_COLOR時 | 属性なし（plain）時 |
|---|---|---|---|---|
| text | 端末既定の前景色 | 本文 | 同じ | 同じ |
| meta | faint (SGR 2) | 7.2のL3すべて | faint | 区別なし（位置と語で判別） |
| you | cyan + bold | `you` ラベル、入力欄の `you ›` | bold | ラベル文字 |
| agent / brand | magenta + bold | `agent` ラベル、ブランド | bold | ラベル文字 |
| live | magenta | pulseスピナー、実行中記号 `▸` | 記号のみ | 記号のみ |
| success | green | `✓` | 記号のみ | 記号のみ |
| failure | red + bold | `✗`、`error` ラベル、`failed` / `disconnected` | bold＋記号＋単語 | 記号＋単語 |
| warning | yellow + bold | `!`、`?`、`reconnecting` / `resyncing` / `unknown` / `partial` | bold＋記号＋単語 | 記号＋単語 |
| neutral end | 既定色 | `⊘ canceled`、`▪ stopped` | 同じ（stoppedはbold） | 記号＋単語 |
| command | 既定色 + bold | `cmd` ラベル | bold | ラベル文字 |
| selection | reverse (SGR 7) | コマンド候補の選択行 | reverse | 行頭 `›` マーカー |
| private | reverse + bold | `key` ラベル | reverse | ラベル文字＋Ruleラベル `private` |

色の使用規則:

- **blue（34）と bright black（90）を文字色に使わない。** テーマによって背景と同化する（例: Solarized系の bright black）。
- 黄色は明るい背景で読みにくいため、黄色の部分には必ず単語を添える。
- 色の段階は「状態が変わったこと」を示す。平常の `connected` `running` は faint で、色を付けない。
- 256色・truecolorは不要。将来使う場合も上表の役割に写像する。

色モードの選択:

| 条件 | モード |
|---|---|
| `NO_COLOR` が設定され空でない | NO_COLOR: 色SGRを出さない。bold / faint / reverse は使う |
| `TERM=dumb`、または属性が使えない端末 | plain: SGRを一切出さない |
| それ以外 | color |

### 7.4 Typography treatment

| 扱い | 使う | 使わない |
|---|---|---|
| bold | 役割ラベル、ヘッダーのセッションID、異常状態の単語、選択中child ID、Markdown見出し、`**強調**`、レールの節見出し | 本文全体、ヒント |
| faint | メタ情報、完了済み、キーバー、罫線、プレースホルダー、host行、activity | 本文（ドラフトを含む）、エラー本文 |
| normal | 発話本文、ドラフト本文、実行中ツール、レーンのID/ラベル | — |
| reverse | コマンド候補の選択行、`key` ラベル | 広い面積の塗り |
| symbol | 状態の1文字（7.5）。必ず単語か位置と組み合わせる | 装飾目的の記号 |
| underline / italic / blink | 使わない | — |

補足:

- ドラフト本文は faint にしない（長文を読むため）。ドラフトであることは seam の `⋮` で示す。
- 数値は桁を揃えない（表にしない）。右寄せするのはレーンの elapsed だけ。

### 7.5 Symbols

状態記号はすべて1セル。East Asian Width（EAW）が N（Neutral）の文字を選び、曖昧幅（A）の文字は罫線と省略記号に限定する（10.4）。

| 意味 | Unicode | ASCII | EAW | 色の役割 | 添える単語 |
|---|---|---|---|---|---|
| queued（待機） | `◌` | `.` | N | meta | `queued` |
| running（静止表示） | `▸` | `>` | N | live | 経過時間 |
| スピナー（アニメーション） | `⠁ ⠈ ⠐ ⠠ ⢀ ⡀ ⠄ ⠂`（1点が周回、約120ms/コマ） | `\| / - \` | N | live | フェーズ名 |
| completed | `✓` | `+` | N | success | （不要） |
| failed | `✗` | `x` | N | failure | `failed` 必須 |
| canceled | `⊘` | `-` | N | neutral | `canceled` 必須 |
| attention（要確認） | `!` | `!` | Na | warning | 内容 |
| unknown | `?` | `?` | Na | warning | `unknown`（ヘッダー） |
| session stopped | `▪` | `#` | N | neutral | `stopped` 必須 |
| link connected | `⇄` | （省略） | N | meta | `connected`（狭い幅では省略可） |
| ドラフトの溝 | `⋮` | `:` | N | meta | — |
| 入力欄の区切り | `›` | `>` | N | you / command | — |
| 選択中child | `›` | `*` | N | bold | — |
| コード折り返し継続 | `↪` | `>` | N | meta | — |
| Enter / Tab | `⏎` / `⇥` | `enter` / `tab` | N | meta | — |
| Ctrl | `^C` `^D` | 同じ | Na | meta | — |
| 罫線 | `─` | `-` | **A** | meta | — |
| 切り詰め | `…` | `~` | **A** | meta | — |

規則:

- 異常・終端状態（failed / canceled / stopped / unknown）は記号だけで終わらせず、必ず単語を伴う。
- アニメーションは画面全体で最大2つ: エージェントのpulse（または代替のreconnecting表示）と、ローカル操作（送信中・接続中）。ツール行とレーンは静止記号＋経過時間の更新で生きていることを示す。
- ASCIIモードは、ロケールがUTF-8でないとき、または利用者が明示的に選んだときに使う（選択手段の名前は実装側で決める）。ASCIIモードで置き換えるのはUIの装飾（chrome）だけで、会話本文はそのまま表示する。

### 7.6 Responsive rules

詳細は §8。要点:

| 幅クラス | 列数 | レイアウト |
|---|---|---|
| Tiny | < 50 | ラベルを本文の上に積む。ヘッダーは最小。ドックはchild要約1行のみ |
| Narrow | 50–69 | ガターあり。ヘッダーからブランド・単語を落とす。レーンはactivity列なし |
| Standard | 70–119 | 基準レイアウト（4.5）。childはドックのレーン |
| Wide | 120–159 | childが1つ以上ならレール（幅 = 列数×0.3、34〜48）。ドックにレーンは出さない |
| XWide | ≥ 160 | レール48列。レーンは3行表示。本文幅は最大100セル |

共通規則:

- **右端1列は書かない（安全余白）。** 曖昧幅文字が2セルで描かれても行が折り返さないようにする。
- 本文（text列）の最大幅は100セル。それ以上の幅は右側の余白またはレールに使う。
- resize は次のフレームでクラスを再判定するだけで、選択中child・入力内容・カーソル位置・スクロール位置（下端からの距離）を保つ。

### 7.7 Conversation rendering

#### 役割ラベル

| ラベル | 対象（canonical history） | 本文スタイル |
|---|---|---|
| `you` | 外部入力（このターミナルからのプロンプト） | normal、逐語表示 |
| `agent` | モデル応答のメッセージ | normal、軽量Markdown |
| `child` | 既知のchildから届いたpeer入力 | normal。本文の先頭に送信元IDをbold、種別タグをfaintで付ける |
| `peer` | 送信元が既知のchildでないpeer入力 | normal |
| `host` | 制御入力（stop要求など）、crash記録 | faint、1行 |
| `error` | モデル応答の失敗（provider error） | 7.12 |

- **ラベルは発話の連続（run）の先頭にだけ付ける。** 同じ話者の連続するエントリは、空行を挟んでラベルなしで続ける（4.5の2つ目のagent発話）。
- runの先頭行が画面の上に流れて見えなくなった場合は、表示領域の最初の行にそのrunのラベルを再表示する（8.3）。
- 本文の無いモデル応答（ツール呼び出しだけ）も同じ扱いで、ラベル行にツール行を置く（4.6の `agent   ✓ Bash …`）。
- 話者が変わるときは空行1行を入れる。エージェント発話とそのツール行の間には空行を入れない。
- ドラフトは直前の話者が `agent` ならラベルなし、そうでなければ `agent` ラベルから始める。

#### 折り返し

- ラテン文字は空白で折り返す。1語が行幅より長いとき（長いパスやURL）だけ、grapheme境界で強制的に切る。
- CJKは任意の文字間で折り返せる。ただし行頭禁則として `、。，．）」』】〉》！？ー` で行を始めない（前の行に追い込む）。行末禁則として `（「『【〈《` で行を終えない。
- grapheme（結合文字・ZWJ絵文字）は絶対に分割しない。
- タブは4セルの空白として表示する（現行と同じ）。

#### 長文

- 発話は省略しない。既存の表示上限（1エントリ4 KiB）で切られたエントリは、末尾に faint で `[clipped for display; canonical history retained]` を付ける。
- 表示ウィンドウ（直近1,024エントリ）より古い側に達したら、会話領域の最上部に faint で `earlier entries are not kept on this screen; the host retains the full history` を1行出す。
- 会話領域は下端追従。スクロールバック中（7.11、PgUp）を除き、最新の内容が常に見える。

#### コードブロック（agent発話のみ）

<!-- snippet 80 -->
~~~text
 agent   変更後の呼び出し側は次のとおりです。
         ```go
         for _, r := range rows {
             line := renderRow(r, selected, width)
             fmt.Fprintf(&b, "%s\n", clipLine(line, width)) // clip by columns,
       ↪     never by bytes
         }
         ```
         列幅は width から計算し、grapheme を分割しません。
~~~

- フェンス行（```` ```go ```` と ```` ``` ````）は faint で残す。境界がはっきりし、コピーしたとき元の形に近い。
- コード行は空白で折り返さず、行幅を超えた位置で切り、継続行は seam 列に `↪` を置き、元の行の字下げを保つ。
- コード行に装飾文字（縦線など）を前置しない。端末でマウス選択してコピーしても余計な文字が入らない。
- シンタックスハイライトは行わない。

#### 軽量Markdown（agentのcanonical発話のみ）

| 記法 | 表示 |
|---|---|
| `# 見出し` 〜 `###` | `#` を外して bold |
| `**強調**` | 記号を外して bold |
| 箇条書き `-` `*` `1.` | 記号を残し、折り返しは記号の後ろに揃える |
| 引用 `>` | 記号を faint で残す |
| `` `inline` `` | そのまま（バッククォートを残す） |
| リンク `[text](url)` | `text (url)`、url は faint |
| 表、その他 | 逐語 |

- ユーザー発話とドラフトは変換しない（逐語）。ドラフトが確定してcanonicalに置き換わったときに整形が適用される。

### 7.8 Tool activity

**方針: ツールは会話の中に、呼び出された位置で1行のレシートとして出す。実行中は個別に、完了したら畳む。失敗は畳まない。**

#### ツール行の形

```text
         {記号} {tool}  {target}                                  {経過 / 状態語}
         ▸ edit   harness/viewer/render.go                                  4s
         ◌ read   harness/viewer/panel_test.go                          queued
         ✓ grep   "RenderRows" harness/viewer
         ✗ edit   harness/viewer/render.go                              failed
                  old_string not found in file
```

- `tool` はOperationのツール名をそのまま表示する。例外として SubagentStart は `spawn` と表示する。
- `target` は呼び出し引数から主対象を1つだけ選ぶ:

| ツール | target |
|---|---|
| read / write / edit / ViewImage | ファイルパス |
| grep | パターン（引用符付き）＋パス |
| glob | パターン |
| Bash | コマンドの1行目 |
| AST / LSP / DAP | 操作名＋ファイル（または対象シンボル） |
| SkillUse | スキル名 |
| SubagentStart | child ID＋タスクラベル |
| 判定できない | 名前のみ |

- targetは長すぎる場合、中央を `…` で切り詰める（パスは末尾のファイル名を残す）。
- 経過時間はOperationの経過が分かるときだけ、faint で右寄せ。実行中は毎秒更新。完了は5秒以上かかったものだけ表示。
- failed / canceled は状態語を右に、理由がOperation snapshotにあれば次の行に faint で1行。
- op ID は表示しない。

#### 状態の対応

| Operation状態 | 記号 | 表示 |
|---|---|---|
| Awaiting | `◌` | `queued`（付録B-1） |
| Ready（実行中） | `▸` | 経過時間 |
| Completed | `✓` | （なし） |
| Failed | `✗` | `failed` ＋理由 |
| Canceled | `⊘` | `canceled` |
| 不明（切断中・resync中） | `?` | faint |

#### 畳み込み

- 1つのモデル応答から出たツール行を「ステップ」とする。
- ステップ内の全ツールが Completed で2件以上なら、1行に畳む: `✓ 3 tool calls: read 2, grep 1`（faint）。
- 1件だけの完了ツールは畳まず faint で残す。
- failed / canceled を含むステップでは、完了分だけを要約行にし、失敗・取消は個別行で残す。
- `spawn` 行は畳まない（childは重要情報のため）。
- 実行中・待機中の行は常に個別に表示する。

#### Pulseとの関係

- pulse（7.10）は実行中ツールを1語で要約する: `running edit`、`running 3 tools`。
- ツール行は静止記号、動くのはpulseのスピナーだけ。

### 7.9 Child agents

#### 状態の決め方（優先順）

1. canonical Finish がある → Finish.Status で決める（`success` は `✓`、`failure` / `failed` / `error` は `✗`、それ以外は `!` ＋状態語をそのまま）。Finishは履歴上の記録なので接続断中も変えない
2. 親との接続が切れている、またはresyncが必要 → `?`（unknown、faint）
3. 親側 SubagentStart Operation が終端 → Completed は `▪ ended (no Finish)`、Failed は `✗ failed`、Canceled は `⊘ canceled`
4. 親側 Operation が非終端 → Awaiting は `◌ queued`、Ready は `▸ running`

- childのruntime livenessはsnapshotのため常に unknown。UIは「生きている」と主張せず、親側Operationの状態を「running」として表示する（親側Operationが実行状態の正）。
- 経過時間: 親側Operationの経過が分かればそれ、なければchildセッションの経過が分かればそれ、どちらも無ければ表示しない。
- activity と usage は、そのchildが観測済み（選択されたことがある）のときだけ表示する。選択中以外は最後に観測した値で、faint。未観測なら表示しない。

#### 件数ごとの表示（Standard幅、ドック内）

**0件:** child関連の表示は一切出さない（ドック・レール・キーバーとも）。child系コマンドは `/` 候補とhelpにだけ出る。

**1件:** 見出しなしで、レーン1行にラベル `child` を付ける。

<!-- snippet 80 -->
```text
 child ▸ child-7f3a  Refactor RenderRows into columns    3m40s  edit render.go
```

**2件以上:** 見出し行（件数の要約）＋レーン。

<!-- snippet 80 -->
```text
 child   2 running, 1 done                                   /child ID to focus
       ▸ child-7f3a  Refactor RenderRows into columns    3m40s  edit render.go
       ▸ child-91b2  Add panel rendering tests           3m38s
       ✓ child-0c4e  Update viewer README                1m02s  success
```

**予算を超える場合:** ドックのchild行数（見出し込み）は `clamp(floor(高さ/6), 1, 6)`（24行→4、30行→5、20行→3、16行→2）。

- 非終端のレーンを優先して表示し、残りの行に直近で終わったものを入れる。入らない分は見出しの件数にだけ反映する（例: `5 running, 3 done, 1 failed  (4 hidden)`）。
- 1行しか無いときは見出し行だけ（`child   5 running, 3 done, 1 failed`）。

#### レーンの規則

- **並び順は安定させる。** viewerの行順（ID順）を保ち、activityや状態が変わっても並べ替えない。終わったレーンもその位置で `✓` に変わり、予算が足りなくなった時だけ見出しに畳む。
- 孫child（Depth ≥ 2）は、深さ1ごとにIDを2セル字下げする。
- レーンの列: 状態記号（seam列）→ ID → タスクラベル（SubagentStartの計画テキスト）→ 経過（右寄せ）→ activity / Finish状態（faint）。
- IDは切り詰めない（コマンド入力に使うため）。12セルを超えるIDは、表示中のレーン間で一意になる範囲で中央を `…` で切り詰め、Tab補完で全IDを入力できるようにする。
- `Problem` / `NeedsResync` があるレーンは、末尾に `! resync required` などを warning で出す。
- 親側の観測問題（Panelの problem）はドック見出しの右に `! observation: …` として1行。

#### 選択（focus）

`/child ID` で選択したchildはレーンの選択マーカー `›` と bold で示し、ドックにfocusシートを出す（Wideではレールのfocusブロック）。

<!-- snippet 80 -->
```text
 child › child-7f3a  Refactor RenderRows into columns            running  3m40s
         activity  edit harness/viewer/render.go
         ops       ✓ read 4   ✓ grep 1   ▸ edit render.go  12s
         usage     in 12.1k out 1.2k
         finish    not recorded yet
         others    ▸ child-91b2 3m38s   ✓ child-0c4e success
         /child-send TEXT  /child-cancel  /child-history  /child codex-work-1
─ focus child-7f3a ────────────────────────────────────────────────────────────
   you › █
         ⏎ send to codex-work-1   /child-send TEXT   ^C stop session
```

- focusシートの行: 状態と経過、activity、ops（完了は種類ごとの件数、実行中は個別）、usage、finish（Finishがあれば状態と要約1行、ChangedFiles / Tests / Blockers の件数）、others（他のレーンを1行に要約）、そのchildに使えるコマンド。
- Tall（35行以上）のときだけ project instructions のメタデータ（source、bytes、digest先頭8文字）を `project` 行に出す。本文は出さない。
- **プロンプトは常に親セッションに送られる。** focus中はキーバーとプレースホルダーでそれを明示する（`⏎ send to codex-work-1`）。
- focusの解除は既存のとおり `/child <親セッションID>`。シート最終行にその形をそのまま示し、Tab補完の候補先頭にも親IDを `(this session)` として出す。
- `/child-history` を実行すると、focusシートの中身を履歴ページに置き換える:

<!-- snippet 80 -->
```text
 child › child-7f3a  history after 0, 6 items          more: /child-next
         1  input  Refactor RenderRows into columns. Keep the public API.
         2  agent  I'll start with render.go and keep the RenderRows signature.
         3  tool   read harness/viewer/render.go
         4  tool   read harness/viewer/panel.go
         5  agent  The row format mixes fields; I'll build columns first.
         6  tool   edit harness/viewer/render.go
```

#### 会話中のchild

- 起動: 親のツール行 `▸ spawn  child-7f3a  Refactor RenderRows into columns`。状態記号はレーンと同じ規則。
- childから親へのpeer入力: ラベル `child`、本文先頭に送信元ID（bold）。
- `/child-send` などの操作結果は会話に出さず、通知行（7.12）に出す。canonicalな反映はchildの履歴とレーンで見る（楽観的に表示を変えない）。

#### Wide / XWide のレール

- 幅120以上でchild行が1つ以上あるとき、右側にレールを出す。以後、child行がある限り出し続ける（出たり消えたりして会話が再折り返しされるのを防ぐ）。
- レールとの間は空白3列。縦線は引かない。
- レールの構成: 見出し `child agents` ＋件数（右寄せ）→ レーンブロック（1行目: 選択マーカー、記号、ID、経過右寄せ / 2行目: タスクラベル / 3行目: activity または `finished: STATUS`、faint）→ focusブロック（選択時）。
- 行が足りないときは、非選択レーンを1行（記号・ID・経過）に縮め、次に終端レーンを見出しの件数に畳む。
- レールがあるときドックにはレーンを出さない（重複させない）。

### 7.10 Progress

canonicalな応答の前の一時状態は、**pulse行**（フェーズ）と**ドラフト**（ストリーミング本文）の2つで表す。

#### フェーズの決め方（上から順に最初に当てはまるもの）

| # | 条件（既存projectionで判定） | pulse表示 |
|---|---|---|
| 1 | 親との接続が切れている | pulseなし（7.12の接続表示に置き換え） |
| 2 | Progressあり、Attempt ≥ 2、本文が空 | `⠹ retrying (attempt 2)` |
| 3 | Progressあり、Mode = streaming、本文あり | `⠹ generating`（Attempt ≥ 2なら `generating (attempt 2)`）＋ドラフト |
| 4 | Progressあり、Mode = streaming、本文が空 | `⠹ thinking` |
| 5 | Progressあり、Mode ≠ streaming | `⠹ waiting for completed response` |
| 6 | Progressなし、SubagentStart以外の非終端Operationあり | `⠹ running edit` / `⠹ running 3 tools` |
| 7 | Progressなし、ランタイムがrunningで、最新のcanonicalエントリが「このターミナルの入力」または「終端したツール結果」で、その後にモデル応答が無い | `⠹ thinking` |
| 8 | それ以外 | pulseなし（idle） |

- 経過時間（faint）: フェーズ6は実行中Operationの最長経過、それ以外は最新のcanonicalエントリの記録時刻からの経過。記録時刻が無ければ出さない。
- pulseは会話領域の最終行に固定し、スクロールバック中も見える。
- 親がidleでchildだけ動いているときはpulseを出さない。状況はレーン（またはレール）で分かる。

#### ドラフト

- Progress本文を会話の末尾（全canonicalエントリの後）に、seam `⋮` 付きで逐語表示する。本文は normal（dimにしない）。
- 末尾が見えるように下端追従する。pulse行を押し出さない。
- **canonical応答が届いた時点でドラフトを消し、同じ位置に確定メッセージを通常行で出す。** ドラフトと確定メッセージが同時に見えるフレームを作らない。
- 新しいattemptの開始（リセット）でドラフトを消し、フェーズ2に戻る。
- 切断時はドラフトを消す（現行どおり）。再接続後にProgressが続いていれば再表示する。
- 古い世代・epochのProgressは表示しない（現行どおり）。

<!-- snippet 80 -->
```text
       ⋮ 待つ間に既存テストの構成を確認します。panel_test.go は Render の出力文
       ⋮ 字列を直接比較しているため、
       ⠹ generating  6s
```
↓ canonical応答の到着後

<!-- snippet 80 -->
```text
         待つ間に既存テストの構成を確認します。panel_test.go は Render の出力文
         字列を直接比較しているため、列の変更には期待値の更新が必要です。
```

### 7.11 Input

#### 入力欄の形

```text
───────────────────────────────────────────────────────────────────────────────
   you › 入力テキスト█
         ⏎ send   / commands   ^C stop session   ^D detach
```

- ガターラベルは「Enterで何が起きるか」を示す:

| ラベル | 条件 | Enterの結果 |
|---|---|---|
| `you` | 通常（貼り付けで始まる `/` を含む） | 親セッションへのプロンプト送信 |
| `cmd` | 入力の先頭がキー入力された `/` | ローカルコマンドの実行 |
| `key` | `/login` 後の秘匿入力中 | 資格情報の保存（別経路） |

- **貼り付けた `/stop` は `you` のまま。** Ruleに `pasted: sent as text, not as a command` を出す（既存の安全規則を見える形にする）。
- 端末のハードウェアカーソルをキャレット位置に表示する。日本語IMEの変換表示がキャレット位置に出る。`▏` のような文字は挿入しない。
- 入力欄の高さは内容の行数（貼り付けの改行と折り返し）に応じて伸び、最大 `clamp(floor(高さ/5), 1, 8)` 行（24行→4、30行→6、40行→8）。超える分はキャレット周辺を表示し、Ruleの右側に `+12 lines` を出す。
- 入力量が上限64 KiBの75%を超えたら、Ruleの右側に `49 KB of 64 KB` を出す。

#### Ruleラベル（入力モードの表示）

罫線の左に1つだけ埋め込む。優先順: private → pasted → commands → scrollback → focus。

| ラベル | 条件 |
|---|---|
| `─ private: API key for openai/primary ─…` | 秘匿入力中 |
| `─ pasted: sent as text, not as a command ─…` | 貼り付けを含む |
| `─ commands ─…` | `/` 候補を表示中 |
| `─ scrollback: 42 lines below; PgDn returns ─…` | スクロールバック中 |
| `─ focus child-7f3a ─…` | childを選択中 |

#### 状態ごとの表示

| 状態 | ラベル | プレースホルダー（空のとき、faint） | Keybar |
|---|---|---|---|
| idle | `you` | `Ask, or type / for commands` | `⏎ send   / commands   ^C stop session   ^D detach` |
| エージェント作業中 | `you` | `Ask, or type / for commands` | 同上（送信は可能） |
| コマンド入力中 | `cmd` | — | `⇥ complete   ⏎ run   ^C stop session   ^D detach` |
| 送信待ち（pending） | `you` | — | `^C cancel waiting   ^D detach` |
| 秘匿入力 | `key`（reverse） | `Type or paste the key; it stays masked` | `⏎ store key   ^C cancel entry` |
| focus中 | `you` | `Message codex-work-1, or /child-send TEXT for child-7f3a` | `⏎ send to codex-work-1   /child-send TEXT   ^C stop session` |
| 接続中・オフライン | `you` | `Offline; you can keep typing` | `⏎ send (fails until reconnected)   ^D detach` |
| stopped | `you` | `Session stopped; /resume continues it` | `/resume continue   ^D detach` |
| failed | `you` | `Session failed; see above` | `/resume   ^D detach` |
| スクロールバック中 | `you` | — | `PgUp/PgDn scroll   ⏎ send and return to live` |

#### 送信待ち（busy）

- Enterで入力欄は空になり（現行どおり）、ドックの通知行に送信中表示（outbox）を出す。送信テキストは1行に切り詰める。秘匿入力の値は決して出さない。

<!-- snippet 80 -->
```text
       ⠹ sending  viewer の子エージェント表示を整理して、テストも追加して。
───────────────────────────────────────────────────────────────────────────────
   you › █
         ^C cancel waiting   ^D detach
```

- 成功: outboxを消すだけ（`input committed` は出さない）。canonicalな `you` エントリは購読経由で会話に現れる。
- 失敗: 通知行をエラーにする（7.12）。
- 待機中のEnter: 通知 `request pending; ^C cancels waiting`。
- 待機中も入力欄の編集はできる（現行どおり）。

#### 秘匿入力（/login）

<!-- snippet 80 -->
```text
─ private: API key for openai/primary ─────────────────────────────────────────
    key › ****************█
         ⏎ store key   ^C cancel entry   masked; never sent to the agent
```

- 表示するのは `*` のみ（最大64個、現行どおり）。値・長さの正確な数・一部の文字は出さない。
- この間 `/` 候補は開かない。会話とドックは通常どおり更新される。
- 取消時の通知: `key entry canceled`。保存後: `API key stored; it is checked on the next provider request`。

#### キー一覧

| キー | 動作 | 区分 |
|---|---|---|
| Enter | 送信 / コマンド実行 / 鍵の保存 | 既存（不変） |
| ← → Home End Backspace | grapheme単位の編集 | 既存（不変） |
| Ctrl-C | 秘匿入力の取消 → 送信待ちの取消 → セッションのhard stop | 既存（不変） |
| Ctrl-D | detach | 既存（不変） |
| bracketed paste | テキストとして挿入。実行・送信しない | 既存（不変） |
| Tab | `/` コマンドとchild IDの補完 | 新規、UIローカル |
| PgUp / PgDn | 会話領域のスクロール | 新規、UIローカル |

- Tab: 候補が1つならそれに確定（引数を取るコマンドは後ろに空白を付ける）。複数なら共通接頭辞まで補完し、続けて押すと候補を順に選ぶ。
- PgUp / PgDn: 会話領域を1画面分動かす。最下端に戻ると自動追従を再開する。Enterで送信すると最下端に戻る。

### 7.12 Errors

| 種類 | 表示場所 | 形 | 持続 | 次の行動の提示 |
|---|---|---|---|---|
| provider error（モデル応答の失敗） | 会話（canonical） | `error` ラベル（red bold）＋メッセージ＋既知コードのヒント（faint） | 履歴として残る | コードごとのヒント |
| 送信の失敗（ACK不明） | 通知行 | `✗ not confirmed: {err}` ＋ `/retry` の説明 | 次のEnterまで | `/retry resends it with the same input ID` |
| 待機の取消 | 通知行 | `! stopped waiting; the input may already be committed and will appear above if so` | 次のEnterまで | — |
| 接続断 | ヘッダー＋ドックの状態行 | 7.12の接続表示 | 復帰まで | `^D` でdetach可能 |
| resync | ヘッダー＋ドックの状態行（faint） | `rebuilding the view from canonical history` | 完了まで | — |
| session stopped | ヘッダー＋ドックの状態行 | `▪ session stopped; /resume continues this session` | 停止中ずっと | `/resume` |
| session failed | ヘッダー＋ドックの状態行（red） | `✗ session failed: {failure}`（最大3行） | 失敗中ずっと | `/resume`（付録B-2） |
| tool failure | 会話のツール行 | `✗ edit  path  failed` ＋理由1行 | 畳まない | — |
| child failure | レーン／focus | `✗` ＋状態語。focusでBlockersを表示 | 状態が変わるまで | `/child ID` |
| childコマンドの失敗 | 通知行 | `✗ child request failed: {err}; /child-retry reuses its input ID` | 次のEnterまで | `/child-retry` |
| 使い方の誤り・不明コマンド | 通知行（warning） | 既存の `usage: …` 文言、`unknown command /foo; type / or /help` | 次のEnterまで | — |

#### provider error の例

<!-- snippet 80 -->
```text
 error   external_reauth_required: Codex credential file is missing or expired
         Renew the login with Codex CLI (codex login) outside this TUI; the
         next request rereads the file.
```

- 1行目はメッセージをそのまま（サニタイズ済み）。既知のコードだけ2行目以降にヒントを出す。未知のコードにヒントは付けない。
- 既知ヒント（docsに記載のもの）: `external_reauth_required`（Codex CLIで再ログイン）、`writer_owned`（他のクライアントがwriterを所有中、`/resume` は使えない）。

#### 接続の表示

接続断は短い途切れと継続的な断で見せ方を変える。

| 状態 | 条件 | ヘッダー | ドックの状態行 | その他 |
|---|---|---|---|---|
| reconnecting（短い） | 断から2秒未満 | `⠹ reconnecting`（yellow） | なし | ドラフト消去、runtime `? unknown` |
| disconnected（継続） | 断から2秒以上 | `✗ disconnected`（red bold） | `⠹ reconnecting  12s  session state is unknown until the host answers` | レーンはすべて `?`、elapsedを隠す、tokensは faint のまま残す |
| resyncing | gap・世代変更 | `⠹ resyncing`（yellow） | `rebuilding the view from canonical history`（faint） | 会話の表示は消さない |

- 再接続の試行（250 ms間隔）は内部の動きで、表示は点滅させない。
- 2秒の閾値は表示上の区別で、再接続の動作は変えない。

#### 通知行の規則

- ドックの最下段（Ruleの直上）に1つだけ。最大2行。
- 成功・情報は faint で、次のEnterまたは8秒で消える。警告・エラーは次のEnterまで残る。
- 通知は会話（canonical history）に書き込まない。

### 7.13 Help / commands

**普段の画面にコマンド一覧を出さない。** 発見の経路は3つ: キーバーの `/ commands`、`/` 入力時の候補、`/help` シート。

#### `/` 候補（コマンドメニュー）

キー入力で先頭に `/` を打つと、Ruleの上に候補を出す。前方一致で絞り込む。空白を打った後は、そのコマンドの使い方1行に切り替わる。

<!-- snippet 80 -->
```text
       › /child ID                       focus a child agent
         /child-history [after] [limit]  load a page of its history
         /child-next                     load the next history page
         /child-send TEXT                send a message to the focused child
         /child-cancel                   cancel the focused child
         /child-resume                   confirm ownership of the focused child
         /child-retry                    retry the last uncertain child request
─ commands ────────────────────────────────────────────────────────────────────
    cmd › /ch█
         ⇥ complete   ⏎ run   ^C stop session   ^D detach
```

- 選択行は reverse（plainでは `›` マーカー）。Tabで選択行に補完する。
- 候補の行数は `clamp(floor(高さ/3), 3, 10)`。それを超える場合は最終行に `+3 more` を出す。
- 候補を出している間、ドックのレーンは見出し1行に縮める。
- 貼り付けた `/…` と秘匿入力中は候補を出さない。
- 引数入力中の使い方表示の例: `/login PROVIDER ID   then enter the key privately`。

#### `/help` シート

<!-- snippet 80 -->
```text
  help   session  /stop   /stop idle   /resume   /retry   /detach
         auth     /login PROVIDER ID   /logout PROVIDER ID
                  /credentials   /methods
         child    /child ID   /child-history [after] [limit]   /child-next
                  /child-send TEXT   /child-cancel   /child-resume
                  /child-retry
         keys     ⏎ send   ⇥ complete   PgUp/PgDn scroll   ^D detach
                  ^C cancels key entry or waiting; otherwise stops the session
         Type / to search commands. This closes on the next Enter.
───────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send   / commands   ^C stop session   ^D detach
```

- ドックにシートとして出し、会話には書き込まない。次のEnterで閉じる。閉じるためにCtrl-Cを押させない（Ctrl-Cは停止になりうるため）。
- 幅100以上では「コマンド＋説明」の2列表示にしてよい。
- 高さが足りないときは `keys` → `child` → `auth` → `session` の順に残し、入らない分は `+N more; type / to search` にする。

#### コマンド出力

`/methods`、`/credentials` の結果は、現行の1行連結をやめ、helpと同じシートに1項目1行で出す（`openai/api_key   supported` の形。状態語は既存の支援表をそのまま使う）。次のEnterで閉じる。

#### コマンド一覧（既存、意味は変更しない）

| グループ | コマンド | 候補の説明文 |
|---|---|---|
| session | `/stop` | hard stop this session |
| | `/stop idle` | stop when the agent is idle |
| | `/resume` | resume this stopped session |
| | `/retry` | resend the last uncertain input (same input ID) |
| | `/detach` | leave; the session keeps running |
| auth | `/login PROVIDER ID` | store an API key (masked entry) |
| | `/logout PROVIDER ID` | remove a stored credential |
| | `/credentials` | list stored credentials |
| | `/methods` | show supported authentication methods |
| child | `/children` | how to select a child |
| | `/child ID` | focus a child agent |
| | `/child-history [after] [limit]` | load a page of its history |
| | `/child-next` | load the next history page |
| | `/child-send TEXT` | send a message to the focused child |
| | `/child-cancel` | cancel the focused child |
| | `/child-resume` | confirm ownership of the focused child |
| | `/child-retry` | retry the last uncertain child request |
| other | `/help` | show commands and keys |

### 7.14 Interaction states

→ §9 に画面差分として定義する。

---

## 8. Responsive behavior

### 8.1 幅クラス別の規則

| 要素 | Tiny <50 | Narrow 50–69 | Standard 70–119 | Wide 120–159 | XWide ≥160 |
|---|---|---|---|---|---|
| ガター | なし。ラベルを本文の上の行に積み、本文は2セル字下げ。seamは1列目 | 9セル | 9セル | 9セル | 9セル |
| 本文幅 | 幅−3 | 幅−10 | 幅−10（最大100） | 会話列−9 | 会話列−9（最大100） |
| ヘッダー | ID＋記号＋elapsed | ブランドなし、`⇄` 記号のみ、tokens短縮 `48.2k/3.1k` | 全項目 | 全項目 | 全項目 |
| childの場所 | ドック要約1行 | ドック（activity列なし） | ドック | レール34〜44列 | レール48列 |
| ツール行 | ステップを1行要約 | 通常 | 通常 | 通常 | 通常 |
| Keybar | 高さ16以上で短縮形 | 短縮形 `⏎ send  / cmds  ^C stop  ^D detach` | 標準 | 標準＋`PgUp/PgDn scroll` | 標準＋`PgUp/PgDn scroll` |

ヘッダーの項目は、入らない場合に次の順で落とす（常に残すもの: 接続状態、異常時のランタイム状態）:

1. ブランド `unreal agent`
2. tokensのラベル（`in 48.2k out 3.1k` → `48.2k/3.1k`）
3. elapsed
4. tokens
5. 正常時の `running` の単語
6. セッションIDの中央切り詰め（最小8セル）
7. 正常時の `connected` の単語（記号 `⇄` のみ）

### 8.2 高さクラス別の規則

| 要素 | Short <20 | Standard 20–34 | Tall ≥35 |
|---|---|---|---|
| ヘッダー下の空行 | なし | あり | あり |
| 会話とドックの間の空行 | なし | 24行以上であり | あり |
| ドックのchild行数 | `clamp(floor(H/6),1,6)` | 同左 | 同左（Wideではレール） |
| focusシート | 見出し＋activityの2行 | 最大7行 | 最大10行（project行を含む） |
| Keybar | 16行未満は隠し、入力欄右端に `^C stop` だけ出す | あり | あり |
| 会話領域の最小行数 | 3 | `floor(H×0.4)` | `floor(H×0.4)` |

- シート・レーン・状態行は、会話領域の最小行数を割らない範囲でしか伸びない。入らない分は `+N more` に畳む。
- 20x6 未満では、1行目に状態記号だけ（`⇄ ▸` など）、最終行に入力欄だけを出し、他は省略する。

### 8.3 Narrow — 60x20

<!-- mockup 60x20 -->
```text
codex-work-1                  ⇄  running  4m12s  48.2k/3.1k

   you   viewer の子エージェント表示を整理して、テストも追
         加して。

 agent   描画を3つの作業に分けて、子エージェントに並行して
         任せます。
         ▸ spawn  child-7f3a  Refactor RenderRows into co…
         ▸ spawn  child-91b2  Add panel rendering tests
         ✓ spawn  child-0c4e  Update viewer README

       ⋮ 待つ間に既存テストの構成を確認します。
       ⋮ panel_test.go は Render の出力文字列を直接比較して
       ⠹ generating  6s
 child   2 running, 1 done                        /child ID
       ▸ child-7f3a  Refactor RenderRows into…   3m40s
       ▸ child-91b2  Add panel rendering tests   3m38s
───────────────────────────────────────────────────────────
   you › █
         ⏎ send  / cmds  ^C stop  ^D detach
```

### 8.4 Tiny — 44x14

<!-- mockup 44x14 -->
```text
codex-work-1               ⇄ running  4m12s
you
  viewer の子エージェント表示を整理して、テ
  ストも追加して。
agent
  描画を3つの作業に分けて、子エージェントに
  並行して任せます。
  ▸ 3 spawns: 2 running, 1 done

⋮ 待つ間に既存テストの構成を確認します。
⠹ generating  6s
child  2 running, 1 done
───────────────────────────────────────────
› █                                 ^C stop
```

- Tinyでは入力欄のラベル（you / cmd / key）を出さず、入力モードはRuleラベルで示す。

### 8.5 XWide — 160x40（レール＋focus）

<!-- mockup 160x40 -->
```text
unreal agent  codex-work-1                                                                                       ⇄ connected  running  4m12s  in 48.2k out 3.1k

   you   Run the viewer tests and tell me what fails.                                                          child agents                   2 running, 1 done

 agent   ✓ Bash  go test ./harness/viewer/...                                                                  › ▸ child-7f3a                             3m40s
                                                                                                                   Refactor RenderRows into columns
         All 14 tests in harness/viewer pass. The 1s timeout in client_test.go may be flaky on slow                edit harness/viewer/render.go
         machines.
                                                                                                                 ▸ child-91b2                             3m38s
   you   Clean up how the viewer panel shows child agents, and add tests.                                          Add panel rendering tests

 agent   I'll read the current rendering code first.                                                             ✓ child-0c4e                             1m02s
         ✓ 3 tool calls: read 2, grep 1                                                                            Update viewer README
                                                                                                                   finished: success
         RenderRows mixes identity, status and usage in one pipe-separated string. Splitting this into three
         parallel child agents: layout, tests, and docs.                                                       focus  child-7f3a
         ▸ spawn  child-7f3a  Refactor RenderRows into columns                                                   state      running (parent operation ready)
         ▸ spawn  child-91b2  Add panel rendering tests                                                          activity   edit harness/viewer/render.go
         ✓ spawn  child-0c4e  Update viewer README                                                               ops        ✓ read 4   ✓ grep 1
                                                                                                                            ▸ edit  render.go               12s
 child   child-0c4e  README updated; the Panel section now documents focus and lanes.                            usage      in 12.1k out 1.2k
                                                                                                                 finish     not recorded yet
 agent   While the children work, I'm checking how panel_test.go asserts on rendered output.                     project    AGENTS.md  2,148 B  3f9a1c0e
         ✓ read   harness/viewer/panel_test.go
                                                                                                                 /child-send TEXT   /child-cancel
         It compares whole frames, so the column change needs new expectations. I'll add a helper that           /child-history     /child-resume
         renders one row at a time so the tests stay readable:                                                   /child codex-work-1  ends focus
         ```go
         func renderRow(r Row, selected session.ID, width int) string
         ```

       ⋮ Next I'll ask child-91b2 to use renderRow in the new tests once child-7f3a lands the refactor, so
       ⋮ both changes
       ⠹ generating  4s



─ focus child-7f3a ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send to codex-work-1   /child-send TEXT   ⇥ complete   PgUp/PgDn scroll   ^C stop session   ^D detach
```

### 8.6 resize

- 次のフレームで幅・高さクラスを再判定し、全領域を組み直す。セッション状態には触れない（現行どおり）。
- 下端追従中なら新しいサイズでも最新内容が見える。スクロールバック中なら「下端からの行数」を保つ。
- Wide↔Standardの境界を跨いだとき、childの表示場所（レール↔ドック）を即座に切り替える。選択中childは保つ。
- 入力欄の内容とキャレット位置は保つ。

---

## 9. Interaction states

### 9.1 状態×領域の差分表

| 状態 | Header | Transcript / Pulse | Dock | Rule / Composer / Keybar |
|---|---|---|---|---|
| connecting | `⠹ connecting`。runtime・elapsed・tokensなし | faint `connecting to the host` 1行 | なし | プレースホルダー `Connecting to the host`。Keybar `⏎ send   ^D detach` |
| connected（idle） | `⇄ connected  running  9s  in 1,053 out 11`（faint） | 会話のみ。pulseなし | なし | 標準 |
| model thinking | 同上 | pulse `⠹ thinking  3s` | 変化なし | 標準 |
| model streaming | 同上 | ドラフト `⋮` ＋pulse `⠹ generating  6s` | 変化なし | 標準 |
| waiting（非ストリーミング） | 同上 | pulse `⠹ waiting for completed response  8s` | 変化なし | 標準 |
| retrying | 同上 | ドラフト消去、pulse `⠹ retrying (attempt 2)` | 変化なし | 標準 |
| tool running | 同上 | ツール行 `▸`＋経過、pulse `⠹ running edit  4s` | 変化なし | 標準 |
| child running | 同上 | `spawn` 行 | レーン（Wideではレール） | 標準 |
| request pending | 同上 | 変化なし | 通知行にoutbox `⠹ sending …` | Keybar `^C cancel waiting   ^D detach` |
| reconnecting（<2秒） | `⠹ reconnecting`（yellow）、runtime `? unknown` | ドラフトとpulseを消す | なし | 標準 |
| disconnected（≥2秒） | `✗ disconnected`（red bold）、`? unknown`、elapsedなし | ドラフトとpulseを消す、表示中の会話は残す | 状態行 `⠹ reconnecting  12s  session state is unknown until the host answers`。レーンは `?` | プレースホルダー `Offline; you can keep typing`。Keybar `⏎ send (fails until reconnected)   ^D detach` |
| resyncing | `⠹ resyncing`（yellow）、runtime `? unknown`、elapsedなし | 会話は残す | 状態行 `rebuilding the view from canonical history`（faint） | 標準 |
| stopped | `⇄ connected  ▪ stopped`、elapsedなし | pulseなし。停止要求があれば `host` 行 | 状態行 `▪ session stopped; /resume continues this session` | プレースホルダー `Session stopped; /resume continues it`。Keybar `/resume continue   ^D detach` |
| failed | `⇄ connected  ✗ failed`（red bold） | pulseなし | 状態行 `✗ session failed: {failure}`（最大3行、red） | プレースホルダー `Session failed; see above`。Keybar `/resume   ^D detach` |
| private entry | 変化なし | 変化なし | 変化なし | Ruleラベル `private: …`、ラベル `key`、Keybar `⏎ store key   ^C cancel entry` |
| command menu / help | 変化なし | 変化なし | シート（レーンは見出し1行に縮める） | Ruleラベル `commands`（menu時）、ラベル `cmd` |
| child focus | 変化なし | 変化なし | focusシート（Wideではレールのfocus） | Ruleラベル `focus child-…`、送信先を示すKeybar |
| scrollback | 変化なし | 過去を表示、pulseは最終行に固定 | 変化なし | Ruleラベル `scrollback: N lines below; PgDn returns` |

### 9.2 状態別の画面

#### connecting

<!-- snippet 80 -->
```text
unreal agent  codex-work-1                                       ⠹ connecting

         connecting to the host
…
───────────────────────────────────────────────────────────────────────────────
   you › █ Connecting to the host
         ⏎ send   ^D detach
```

#### connected（idle）— 80x24 全画面

<!-- mockup 80x24 -->
```text
unreal agent  codex-smoke-1           ⇄ connected  running  9s  in 1,053 out 11

   you   「Codex TUI smoke test OK」とだけ返してください。

 agent   Codex TUI smoke test OK
















───────────────────────────────────────────────────────────────────────────────
   you › █ Ask, or type / for commands
         ⏎ send   / commands   ^C stop session   ^D detach
```

スタイル注記（上の画面）:

| 行 | 要素 | スタイル |
|---|---|---|
| 1 | `unreal agent` | magenta bold |
| 1 | `codex-smoke-1` | bold |
| 1 | `⇄ connected  running  9s  in 1,053 out 11` | faint |
| 3 | `you` | cyan bold |
| 5 | `agent` | magenta bold |
| 22 | 罫線 | faint |
| 23 | `you ›` | cyan bold、プレースホルダーは faint |
| 24 | キーバー | faint（`⏎` `/` `^C` `^D` はnormal） |

#### model thinking

<!-- snippet 80 -->
```text
   you   panel.go の Render を列揃えにして。

       ⠹ thinking  3s
───────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send   / commands   ^C stop session   ^D detach
```

#### model streaming / waiting / retrying

<!-- snippet 80 -->
```text
 agent ⋮ RenderRows を列単位に分けます。まず各列の幅を width から計算し、
       ⋮ 残りを activity に割り当てます。
       ⠹ generating  6s
```

<!-- snippet 80 -->
```text
       ⠹ waiting for completed response  8s
```

<!-- snippet 80 -->
```text
       ⠹ retrying (attempt 2)  1s
```

#### tool running / tool failure

<!-- snippet 80 -->
```text
 agent   render.go の列幅計算を修正します。
         ✓ read   harness/viewer/render.go
         ▸ edit   harness/viewer/render.go                                  4s
         ◌ read   harness/viewer/panel_test.go                          queued
       ⠹ running edit  4s
```

<!-- snippet 80 -->
```text
         ✓ read   harness/viewer/render.go
         ✗ edit   harness/viewer/render.go                              failed
                  old_string not found in file
```

#### child running

4.5（Standard）と 4.6（Wide）を参照。

#### request pending → 失敗 → /retry

<!-- snippet 80 -->
```text
       ✗ not confirmed: connection refused
         /retry resends it with the same input ID; it cannot be committed twice
───────────────────────────────────────────────────────────────────────────────
   you › █
         ⏎ send   / commands   ^C stop session   ^D detach
```

#### disconnected（2秒以上）

<!-- snippet 80 -->
```text
unreal agent  codex-work-1         ✗ disconnected  ? unknown  in 48.2k out 3.1k
…
 child   2 unknown, 1 done
       ? child-7f3a  Refactor RenderRows into columns
       ? child-91b2  Add panel rendering tests
       ✓ child-0c4e  Update viewer README                1m02s  success
       ⠹ reconnecting  12s  session state is unknown until the host answers
───────────────────────────────────────────────────────────────────────────────
   you › █ Offline; you can keep typing
         ⏎ send (fails until reconnected)   ^D detach
```

- Finishが記録済みのレーン（child-0c4e）はcanonicalなのでそのまま。それ以外は `?`（faint）。

#### resyncing

<!-- snippet 80 -->
```text
unreal agent  codex-work-1            ⠹ resyncing  ? unknown  in 48.2k out 3.1k
…
         rebuilding the view from canonical history
───────────────────────────────────────────────────────────────────────────────
```

#### stopped

<!-- snippet 80 -->
```text
unreal agent  codex-work-1            ⇄ connected  ▪ stopped  in 48.2k out 3.1k
…
  host   stop requested from this terminal
…
       ▪ session stopped; /resume continues this session
───────────────────────────────────────────────────────────────────────────────
   you › █ Session stopped; /resume continues it
         /resume continue   ^D detach
```

#### failed

<!-- snippet 80 -->
```text
unreal agent  codex-work-1             ⇄ connected  ✗ failed  in 48.2k out 3.1k
…
       ✗ session failed: provider returned an unrecoverable error after 2
         attempts
───────────────────────────────────────────────────────────────────────────────
   you › █ Session failed; see above
         /resume   ^D detach
```

#### scrollback

<!-- snippet 80 -->
```text
…
       ⠹ generating  6s
─ scrollback: 42 lines below; PgDn returns ────────────────────────────────────
   you › █
         PgUp/PgDn scroll   ⏎ send and return to live
```

---

## 10. Accessibility / no-color behavior

### 10.1 3段階の表示モード

| モード | 出すもの | 意味の伝え方 |
|---|---|---|
| color | 前景色16色＋bold / faint / reverse | 色＋記号＋単語＋位置 |
| NO_COLOR | bold / faint / reverse のみ | 記号＋単語＋位置。強調は bold、弱は faint |
| plain | SGRなし | 記号＋単語＋位置のみ |

すべてのモードで成立させるための規則:

- **状態はすべて「記号＋単語」または「記号＋固定位置」で読める。** 色は補強のみ。
- 異常状態（disconnected / reconnecting / resyncing / unknown / stopped / failed / error / canceled）は常に単語を出す。
- 選択はreverseに加えて `›`（ASCIIでは `*`）マーカーを出す。
- ドラフトは `⋮` / `:` の溝、pulseはseam列のスピナーで、色なしでもcanonicalと区別できる。
- 入力モードはガターラベル（you / cmd / key）とRuleラベルで区別できる。

### 10.2 明暗テーマ

- 背景色を設定しないため、明るいテーマ・暗いテーマの両方で同じ見た目の階層になる。
- 白・黒・bright black を文字色に使わない。

### 10.3 動きの抑制

- 画面上のアニメーションは最大2つ（7.5）。それ以外の「生きている」表示は経過時間の更新で行う。
- スピナーを止める利用者設定を用意する場合、静止記号 `▸` / `>` と経過時間で同じ情報を伝える（付録B-8）。

### 10.4 幅の安全（CJK・曖昧幅）

- 全幅文字（CJK）は2セルとして計算する。grapheme単位で幅を測る（現行どおり）。
- 状態記号は East Asian Width が N の文字から選んでいる（7.5）。曖昧幅（A）は罫線 `─` と切り詰め `…` だけ。
- `…` は1行に最大1つ。右端1列の安全余白があるため、曖昧幅を2セルで描く端末でも行が折り返さない。
- 罫線は曖昧幅を2セルで描く端末では長さが倍になる。こうした端末ではASCIIモードを使う（選択手段は付録B-7）。
- 入力キャレットはハードウェアカーソルで示し、日本語IMEの変換表示がキャレット位置に出るようにする。

### 10.5 安全性の不変条件（表示面）

- provider / tool / user / child の文字列はすべてサニタイズ後に表示する。制御文字・エスケープ・双方向制御文字は端末に届かない（現行どおり）。
- 秘匿入力の値は画面のどこにも出ない（マスク `*` のみ）。資格情報は `provider/ID` の参照名だけを表示する。
- 貼り付けは `cmd` ラベルにならず、実行されない。
- 一時progress（ドラフト・pulse）、送信中表示、通知、シートは、canonical historyの行と同じ見た目にならない。

---

## 11. Implementation acceptance criteria

「画面上で以下が成立していればデザイン通り」とする。

### レイアウトと境界

- AC-01 どのサイズでもフレームが端末の行数・列数を超えず、右端1列に文字が書かれない。
- AC-02 80x24・idle・child 0件では、ヘッダー1行、空行、会話、罫線、入力欄、キーバーのみが表示され、コマンド一覧やchild関連の文字が無い。
- AC-03 動いているものが無いとき、会話と罫線の間に何も無い（ドック0行）。
- AC-04 会話が溢れたとき、スクロールバック中を除き最新の内容が見えている。
- AC-05 resize後の最初のフレームで新しい幅クラスのレイアウトになり、入力内容・キャレット位置・選択中childが保たれている。

### ヘッダー

- AC-06 幅40以上で、セッションID・接続状態・ランタイム状態が常に表示されている。
- AC-07 異常状態はヘッダーで単語として表示されている（記号だけにならない）。
- AC-08 値が不明なとき0や古い値を現在値として表示しない（elapsedは隠す、runtimeは `unknown`、tokensの部分集計は `~` 付き）。

### 会話

- AC-09 話者の連続の先頭にラベルがあり、折り返し行を含む本文がすべてtext列から始まっている。
- AC-10 英単語は行幅より長い場合を除き途中で切れず、CJKの行が `、。」）` などで始まっていない。
- AC-11 コードブロックはフェンスが faint で表示され、コード行が空白で折り返されず、継続行に `↪` がある。
- AC-12 4 KiBで切られたエントリに `[clipped for display; canonical history retained]` が付いている。
- AC-13 ユーザー発話がMarkdown変換されず逐語で表示されている。

### ドラフトとPulse

- AC-14 Progress本文がある間、会話末尾に各行 `⋮` 付きのドラフトが出ている。
- AC-15 canonical応答の到着でドラフトが消え、同じ内容が `⋮` 無しで表示される。両方が同時に見えるフレームが無い。
- AC-16 作業フェーズがあるとき、pulseが会話領域の最終行にちょうど1つあり、フェーズ名が §7.10 の語（thinking / generating / waiting for completed response / retrying (attempt N) / running TOOL / running N tools）のいずれかである。
- AC-17 アニメーションしている記号が画面上に2つを超えない。ツール行とレーンは静止記号である。

### ツール

- AC-18 各ツール呼び出しが会話の中に1回だけ、呼び出したステップの直下に状態記号付きで表示される。
- AC-19 全件完了したステップ（2件以上）が `✓ N tool calls: …` の1行に畳まれ、failed / canceled の行は畳まれず状態語を伴う。
- AC-20 op IDがメイン画面に表示されない。
- AC-21 `spawn` 行は畳まれない。

### Child agents

- AC-22 child 0件のとき、ドック・レール・キーバーにchild関連の表示が無い。
- AC-23 child 1件のとき、ラベル `child` 付きのレーン1行で表示される。
- AC-24 child 2件以上のとき、件数の見出しとレーンが表示され、行数予算で隠れたchildが見出しの件数に含まれている。
- AC-25 childの状態や activity が変わってもレーンの並び順が変わらない。
- AC-26 親との接続が切れている間、Finish記録済みのレーン以外は running を表示せず `?` になっている。
- AC-27 幅120以上かつchildが1件以上でレールが表示され、ドックにレーンが重複して出ていない。
- AC-28 `/child ID` の後、該当レーンに `›` が付き、focusシート（またはレールのfocus）が表示され、キーバーがプロンプトの送信先が親セッションであることを示している。
- AC-29 未観測のchildにactivityやusageが表示されていない。

### 入力

- AC-30 入力ラベルが、通常は `you`、キー入力の `/` で始まると `cmd`、秘匿入力中は `key` になる。貼り付けで始まる `/` は `you` のままで、Ruleに `pasted` ラベルが出る。
- AC-31 端末カーソルが入力欄のキャレット位置に表示されている。
- AC-32 複数行の入力が入力欄の最大行数まで見えており、超過分がRuleに `+N lines` で示されている。
- AC-33 秘匿入力中は `*`（最大64個）だけが表示され、入力値はどの領域・どのフレームにも現れない。
- AC-34 送信待ちの間、通知行にoutboxが出て、キーバーが `^C cancel waiting` を示している。

### コマンドとヘルプ

- AC-35 先頭に `/` をキー入力すると候補が出て、前方一致で絞り込まれ、Tabで補完される。貼り付けと秘匿入力では候補が出ない。
- AC-36 `/help` の結果がドックのシートとして表示され、会話には追加されず、次のEnterで閉じる。
- AC-37 キーバーのCtrl-Cの説明が、その時点の動作（cancel entry / cancel waiting / stop session）と一致している。

### エラーと状態

- AC-38 provider errorが会話に `error` ラベルで残り、既知コードにはヒントが付いている。
- AC-39 送信失敗の通知が `/retry` の説明付きで、次のEnterまで残る。
- AC-40 断が2秒以上続くと、ヘッダーに `disconnected`、ドックに再接続の経過、ランタイムに `unknown` が出て、ドラフトが消えている。
- AC-41 停止中はヘッダーに `stopped`、ドックに `/resume` の案内があり、pulseが無い。
- AC-42 失敗時はヘッダーに `failed`、ドックに失敗内容（最大3行）が表示されている。

### 色・NO_COLOR・ASCII

- AC-43 `NO_COLOR` 設定時、色のSGRが出力されず、すべての状態が記号と単語で区別できる。
- AC-44 plainモードでSGRが一切出力されず、それでも全状態が区別できる。
- AC-45 ASCIIモードでは、UIの装飾にASCII以外の文字が使われていない（会話本文は除く）。
- AC-46 反転表示以外の背景色が使われていない。

### 安全性と描画品質

- AC-47 外部テキスト由来の制御文字・エスケープ・双方向制御文字が端末に出力されない。
- AC-48 資格情報の値が表示されず、参照名（provider/ID）のみ表示される。
- AC-49 ストリーミング中（約33回/秒の更新）に、画面全体が空白になるフレームが見えない（ちらつかない）。
- AC-50 PgUp / PgDn以外の新規キーが追加されておらず、既存コマンドとショートカットの結果が変わっていない（表示文言を除く）。

---

## 付録A. 表示要素とデータの対応

すべて既存のprojectionから得られる。backend / Host APIの変更は不要。

| UI要素 | 既存の情報源 | 備考 |
|---|---|---|
| セッションID | tui Snapshot.ID | |
| 接続状態 | tui Model の status / Connected（connecting、connected、disconnected; reconnecting、resync required、gap/disconnect; resyncing） | 文字列を状態に対応づける。2秒閾値は表示上の区別 |
| ランタイム状態 | Snapshot.Running、host.View.Failure | 未接続時は unknown |
| elapsed / tokens | viewer Row（親セッション）の Elapsed、Usage（Known / Partial / Input / Output） | 現行のパネル行と同じ値 |
| 会話エントリ | host.HistoryItem: Input（External / Peer / Control / Crash）、ModelResponse（Message / ToolCall / Failure）、ToolCallStatus、HostRecord | |
| ツール行の状態 | 最新Operation snapshot。ToolCallStatusの CallID と Operations で呼び出しに対応づける | |
| ツール行の経過 | viewer OperationRow.Elapsed | Known のときのみ |
| ツールのtarget | ModelResponse の ToolCall 引数 | 引数名はツール定義に従う（付録B-5） |
| ドラフト / pulse | Snapshot.Progress（Mode / Text / Attempt / TurnID / Epoch） | |
| childレーン | viewer Model.Rows: ID、Label、Depth、ParentOperationStatus、Finish、Activity、Usage、Elapsed、NeedsResync、Problem | 現在は Panel が整形済み文字列を返すため、同じ Model の構造化データを使う形に表示側を組み替える（フロントエンド内部のみ） |
| 選択中child | viewer Model.Selected / `/child ID` | |
| childの履歴ページ | Panel の transcript（`/child-history`、`/child-next`） | |
| project instructions | Row.ProjectInstructions（SourceKind / SourcePath / ByteLength / Digest） | 本文は表示しない |
| 通知 | コマンド結果（commandResult の text / err）、Panel.Command の戻り文字列、Panel の problem | |
| `/retry` 可否 | TUI の retry、Panel の retry | |
| 秘匿入力・送信待ち | TUI の private、busy | |

## 付録B. 前提と未確定事項

| # | 内容 | 本書での扱い |
|---|---|---|
| B-1 | Operation状態 Awaiting / Ready と queued / running の対応 | Awaiting = queued、Ready = running と仮定（viewerがReadyから経過を数えるため）。コード上の意味と違えば対応表だけ直す |
| B-2 | session failed 後に有効な次の操作 | `/resume` を案内すると仮定。不可なら案内を `/detach` のみにする |
| B-3 | stopped中のプロンプト送信の可否 | 送れるとは約束せず `/resume` を案内する |
| B-4 | provider / model 名の表示 | 既存Viewにruntime identityがあれば、Wide以上でヘッダー右側に faint で出す。無ければ出さない |
| B-5 | ツール引数の項目名 | target の選び方（7.8）はツール定義に合わせて実装側で決める |
| B-6 | Ctrl-Cの即時hard stop | 既存動作のため変更しない。キーバーで常に意味を示すことで誤操作を減らす。確認ステップの追加は将来の検討事項 |
| B-7 | 曖昧幅を2セルで描く端末の扱い | ASCIIモードを明示的に選べるようにする。設定名は実装側で決める |
| B-8 | スピナー停止の設定 | 任意。用意する場合は 10.3 に従う |
| B-9 | 畳んだツール行の展開 | v1では行わない。将来、スクロールバック中の展開などを検討 |
| B-10 | Finish.Status の語彙 | `success` 以外はそのまま表示する（7.9） |

## 付録C. UI文言一覧

| 場所 | 文言 |
|---|---|
| ヘッダー（接続） | `connecting` / `connected` / `reconnecting` / `disconnected` / `resyncing` |
| ヘッダー（ランタイム） | `running` / `stopped` / `failed` / `unknown` |
| pulse | `thinking` / `generating` / `waiting for completed response` / `retrying (attempt {n})` / `running {tool}` / `running {n} tools` |
| ドック状態行 | `session stopped; /resume continues this session` / `session failed: {failure}` / `reconnecting {since}  session state is unknown until the host answers` / `rebuilding the view from canonical history` |
| outbox | `sending  {text}` / `storing key for {provider}/{id}` |
| 通知 | `not confirmed: {err}` ＋ `/retry resends it with the same input ID; it cannot be committed twice` |
| 通知 | `stopped waiting; the input may already be committed and will appear above if so` |
| 通知 | `request pending; ^C cancels waiting` / `no uncertain input to retry` |
| 通知 | `key entry canceled` / `API key stored; it is checked on the next provider request` / `credential removed` |
| 通知 | `unknown command {cmd}; type / or /help` / 既存の `usage: …` 文言 |
| 通知（child） | `sent to {child}; its state updates as it runs` / `child request failed: {err}; /child-retry reuses its input ID` / `parent ownership confirmed; child recovery is owned by the host` |
| 会話 | `[clipped for display; canonical history retained]` / `earlier entries are not kept on this screen; the host retains the full history` / `connecting to the host` |
| レーン | `queued` / `running` / `finished: {status}` / `failed` / `canceled` / `ended (no Finish)` / `resync required` |
| ドック見出し | `{n} running, {n} done, {n} failed  ({n} hidden)` / `/child ID to focus` |
| プレースホルダー | `Ask, or type / for commands` / `Connecting to the host` / `Offline; you can keep typing` / `Session stopped; /resume continues it` / `Session failed; see above` / `Type or paste the key; it stays masked` / `Message {session}, or /child-send TEXT for {child}` |
| Ruleラベル | `private: API key for {provider}/{id}` / `pasted: sent as text, not as a command` / `commands` / `scrollback: {n} lines below; PgDn returns` / `focus {child}` / `+{n} lines` / `{n} KB of 64 KB` |
