# OpenCode Go 接続

OpenCode Go のモデル（取り込み元が実機で確かめた 24 個。先頭は DeepSeek V4.1 Flash）を、Claude の下で実装と調査を
担当するモデルの選択肢として使うための接続の仕様です。

## 目的

現在の機能に影響を与えず、Claude の下で実装・調査を担当するモデルの選択肢として
OpenCode のモデルを追加します。使えるのは、取り込み元が実機で確かめた 24 個のモデルで、先頭は DeepSeek V4.1 Flash
（`deepseek-v4.1-flash`）です。何も設定しなければ、動作は今までと同じです。

## 使い方

`/model` で会話の相手（指揮者）を選びます。サブエージェントはメインエージェントに指示し、
`unreal-agent subagent model` CLI で `lead`・`worker`・`collector` ごとに設定します。
TUI に子モデル選択メニューはありません。役割とプロバイダの組み合わせは固定していません。

キーの保存は接続先の登録だけで、OpenCode Go を自動選択しません。`/model` またはメインエージェント経由の
CLI 設定で明示的に選ぶか、初回の `UNREAL_HARNESS_LLM_PROVIDER=opencode-go` で選びます。
保存キーのある Go だけが利用可能でも、初期接続の自動選択には含めません。

### OpenCode 側の準備

1. OpenCode Go を契約する。
2. Extra Usage をオフにする。オンのままだと、上限を超えた分が従量課金になり、Unreal からは
   気づけない。
3. 従量課金の残高を 0 にし、自動チャージを使わない。
4. Settings > Privacy > Regions で Global を有効にする（DeepSeek に必要）。
5. 管理画面で Go の API キーを作る。

### API キーの保存

キーはチャットや画面に貼らず、ターミナルで管理ストアに保存します。キーを打ち込む方法は、画面に出るので
受け付けません。`<Unreal の場所>` は `unreal` と同じディレクトリです。TUI の `/login` には、
その環境での実際のコマンドが出ます。

Mac では、キーをコピーしてから次を実行します。

```sh
pbpaste | <Unreal の場所>/unreal-agent-auth login --store "${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/credentials" --provider opencode-go --id primary --key-stdin
```

`pbpaste` がない環境（Linux など）では、次を実行してからキーを貼り付けて Enter を押します。入力は
表示されません。

```sh
read -rs KEY && printf %s "$KEY" | <Unreal の場所>/unreal-agent-auth login --store "${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/credentials" --provider opencode-go --id primary --key-stdin; unset KEY
```

保存先は `~/.local/state/unreal-agent/credentials` です。`XDG_STATE_HOME` が設定されていれば
`$XDG_STATE_HOME/unreal-agent/credentials` を使います。共通キットのインストーラー、TUI の `/login`、
Mac のランチャーは同じ保存先を使い、同じ環境の全ワークスペース・instance の Host がキーを探すたびに参照します。

### 使い始める

保存したら、Unreal の画面で `/model refresh` を実行します。`/model` に「OpenCode Go · available」が
出れば使えます。Unreal を起動し直す必要はありません。Unreal が動いていなければ、`unreal` を起動すると
最初から出ます。

`/model` で「OpenCode Go」を選ぶと、モデルの一覧が出ます。取り込み元が実機で確かめた 24 個から選び、effort の段階が
あるモデルは続けて effort も選びます（一覧に effort がないモデルは、その段階が出ません）。選んだモデルは
現在のツール一式が完了したあとのリクエストから適用し、話している相手（指揮者）が替わります。実装者（サブエージェント）は各自のモデルで作業を続けます。[モデルの選択と切り替え](ja/providers.md#モデルの選択と切り替え-model)

会話を Claude から OpenCode Go へ切り替えると、Unreal は L2 の写しと L1 の末尾から新しいウィンドウを組み立てます。次のモデルにはこのウィンドウからの通常のリクエストを送ります。 [3層の文脈](ja/context-3layer.md)

### サブエージェントのモデルを選ぶ

メインエージェントに「lead は Codex の gpt-6.2-sol max、worker は OpenCode Go の DeepSeek V4.1 Flash max にして」
と指示します。メインは自身の Session のシェルで次の CLI を使います。利用者が環境変数や設定ファイルを手で編集する必要はありません。

```sh
unreal-agent subagent model show
unreal-agent subagent model set --role lead --provider openai-codex --model gpt-6.2-sol --effort max
unreal-agent subagent model set --role worker --provider opencode-go --model deepseek-v4.1-flash --effort max
unreal-agent subagent model show --role worker
```

| `--role` | 適用するテンプレート |
| --- | --- |
| `lead` | issue を担当する統括の子。組み込み既定は `gpt-6.2-sol` / max |
| `worker` | lead 配下の `worker-low`〜`worker-max` のすべて |
| `collector` | lead 配下の収集担当。変更しなければ `gpt-6-luna` / medium |
| `all` | 全役割の設定を置き換える |

メイン Session が `worker-*`・`collector` を直接開始した場合、起動は成功し、`lead` の provider・model・effort を使います。
実作業は lead に委譲し、その配下の worker・collector の結果を lead がレビューしてメインへ返します。
worker・collector の役割設定は lead 配下だけに適用され、worker を挟む深い階層でも維持します。
解決した layer は開始記録に保存され、`/analyze` でもメイン直下を `lead` と表示します。
明示的な `--model`・`--effort` は、解決した役割で従来の指定規則に従います。

全役割を同じ設定にする場合も CLI を使います。

```sh
unreal-agent subagent model set --role all --provider opencode-go --model deepseek-v4.1-flash --effort max
```

`--provider` は登録済みで利用可能な接続先、`--model` はその接続先のカタログにあるモデルを指定します。
`--effort` は省略可能で、明示する場合はモデルが対応する値だけを受け付けます。対応しない provider・model・effort は
理由を返して失敗し、保存済み設定を変更しません。API キー接続も利用者が明示した場合だけ使い、別 provider への自動切替はしません。
設定・参照は Host 経由で処理し、操作した Session に役割・provider・model・effort を記録します。

トップレベルの Session をこのバージョンで初めて開くと、共有ディレクトリの `~/.local/state/unreal-agent/subagent-model.json`
（`XDG_STATE_HOME` 設定時は `$XDG_STATE_HOME/unreal-agent/subagent-model.json`）を初期値に使います。
Version 1 の単一設定は全役割に適用します。Session ごとの設定は Host 再起動後も使えます。
`model set` を実行した Session で、次に起動する子と、実行中の lead が次に起動する worker・collector に適用します。
同じ初期値ファイルを使う保存済み Session と今後の Session に反映するには `--all-sessions` を指定します。

旧共通キットでリポジトリ別の State に保存した `subagent-model.json` は、共有ファイルがないときに
そのリポジトリの起動時に内容を変えずに移行します。複数の旧設定があれば最初に起動したリポジトリの選択を共有し、
元ファイルはすべて残します。共有ファイルがある場合はその選択を優先し、旧設定で上書きしません。
Session・ログは従来のリポジトリ別 State に保持します。

保存済み設定がない役割では、親が OpenAI API を明示選択していればその API、他の親では Codex を使います。
Codex を明示した役割は、API の親からでもサブスク接続を使います。
`subagent start --model ID --effort LEVEL` でその子だけのモデルと effort を指定できます。
複数の利用可能な接続先が同じモデルを提供している場合は、表示された ID の一つを `--provider ID` で指定します。
Claude の委譲案内と可用性確認も役割ごとの選択を使い、使えない役割を別接続へ回しません。

Go で effort を省略した場合、lead とメイン直下の子は最高を使います。lead 配下では worker-max は最高、worker-low / collector は最低、
他の worker は自分の effort（なければ既定。既定が自分より低ければ、自分より低い中で最高）を使います。
effort がないモデルには指定を送りません。Flash では次のようになります。

| テンプレート | effort 省略時 | worker に max を明示した場合 |
| --- | --- | --- |
| `lead` | `max` | lead 自身の設定を維持 |
| `worker-max` | `max` | `max` |
| `worker-high`、`worker-xhigh`、`worker-medium` | `high` | `max` |
| `worker-low` | `low` | `max` |
| `collector` | `low` | collector 自身の設定を維持 |

Claude を選んだ役割も Unreal の子 Session として起動し、保存したモデルと effort を使います。
たとえば collector を Sonnet の high にするには、`unreal-agent subagent model set --role collector --provider claude --model claude-sonnet-5-5 --effort high` を実行します。
worker・collector の役割設定は lead 配下で適用されます。Claude の lead からも Codex の lead からも Claude の子を起動できます。
使用量は子 Session に記録し、親が承認を行います。Claude Code の `Agent` と `Task` は親・子ともに禁止です。

テンプレートのない Host では CLI が再起動後から有効と表示します。
Codex 無効・自動検出なしの Host でも、利用可能な Go または Claude を選べば次の Host 起動時にテンプレートを用意します。

### キーの削除

```sh
<Unreal の場所>/unreal-agent-auth logout --store ~/.local/state/unreal-agent/credentials --provider opencode-go --id primary
```

## 仕様

変わるのは「Unreal → HTTP → プロバイダ」の末端です。Claude との関係、通知、承認、Operation は
変わりません。

### 接続先と要求

- 接続先は `https://opencode.ai/inference/go/openai/v1/responses`。`/inference/go/` 配下だけを許可し、
  従量課金の `/inference/openai/...`（OpenCode Zen）は拒否します。リダイレクトも拒否します。
- 本文は `responsesapi` が組み立てます（`store: false`、`stream: true`）。履歴は公開メッセージとツール結果で構成します。
- ヘッダーは、API キーを Authorization ヘッダーで送る形、`Content-Type`、`User-Agent: unreal-agent`、
  `x-opencode-session` です。`prompt_cache_key` は送りません（受け付けるかが未確認のため）。
- `x-opencode-session` には、Session ごとのキャッシュキーの SHA-256 を入れます。すべての要求に付き、このヘッダーのない要求は 400 で断られます。
- 3層ウィンドウの区切りの間は追記し、キャッシュのプレフィックスを維持します。
- `ultra` は `max` として送ります。

### モデル一覧と登録

- 管理ストアに `opencode-go` / `api_key` / `primary` があるときだけ、公開のモデル一覧
  （`https://opencode.ai/inference/go/v1/models`）を読んで `opencode-go` を登録します。一覧はキーを
  送らず、リダイレクトを拒否し、20 秒で打ち切ります。
- 選択肢に出すのは、取り込み元が実機で確かめた 24 個のモデルです。先頭が `deepseek-v4.1-flash` で、残りは一覧の順
  です（一覧の順は OpenCode が決めているので、並べ替えません）。名前、effort、文脈長を読みます。一覧に
  既定の effort がないので、`high` があれば `high` を既定にします。
- 一覧に Unreal の effort（`low`〜`max`）がないモデルは、effort を選ばせず、要求にも `reasoning` を
  付けません。`none`、`minimal`、`toggle` は扱いません。
- 出さないモデルがあります。`mimo-v2.6-flash` はツールを呼ばず、続く段階を実行できていないためです。
  Contributor の 2 個（送った内容が学習に使われる）と Free のモデル（`beam-free`、
  `iceman-contributor-free`）は調べていません。
- 一覧を読めなければ OpenCode Go だけを「使えない」と表示し、他へ切り替えません。キーがない人には、
  通信も表示も増やしません。
- キーは要求ごとに管理ストア（`~/.local/state/unreal-agent/credentials`）から読みます。保存し直した
  場合は、Host を起動し直さなくてよいです。
- 通常の Host は、起動時に必ず `https://opencode.ai` を通信の許可に加えます（設定ファイルには
  書きません）。この許可を使うのはモデルへの要求と子の設定の確認だけで、AI が使うツールには関係
  しません。キーがなければ何も送りません。

### 上限とエラー

Codex と同じ経路に乗せます。ただし共通の再試行の判定は、迷うと再試行する設計で、再試行しないコードは
OpenAI の名前で固定されています。OpenCode が枯渇を別のコードで返すと一時的な制限と判定され、子が枠の
回復まで再試行を続け、親にも通知が届きません。そのため `opencodego` で、枯渇を示すエラーを
`usage_limit_reached` に変換します。形式が分かるまでの暫定として、429 で `Retry-After` が既存の上限
（30 秒）を超えるものを枯渇として扱います。

| | 一時的な制限 | 利用枠の枯渇 |
| --- | --- | --- |
| エラーの例 | `rate_limit_exceeded`、コード不明の 429、5xx | `usage_limit_reached`（code または type） |
| API 呼び出し内の再試行 | 最大 5 回（ストリーム中は 6 回）。待ち時間は最大 30 秒で、`Retry-After` も 30 秒が上限 | しない |
| 親 Session | 再試行が尽きると Session が失敗 | 解除時刻まで待機して自動再開 |
| 子 Session | 2 秒後にモデル呼び出しをやり直す。回数の上限なし | 解除時刻まで待機して自動再開。親にも待機状態を通知 |

共通: 上限や障害で、別のプロバイダや API キーへ自動で切り替えません（Codex へも切り替えません）。
実行済みの Operation は再実行しません。利用上限では `resets_at` / `reset_at`、`resets_in_seconds`、
`Retry-After`、`try again in` の解除ヒントを使い、TUI に `usage limit — waiting until <時刻>` を表示します。
ヒントがない場合は既存バックオフの最長段階（30 秒）を使います。待機回数の上限はなく、`/stop` は直ちに反映されます。
`insufficient_quota` など、利用上限以外の恒久エラーは従来どおり失敗を返します。

### 接続検証（2026-11-02）

`harness/llm/clients/opencodego/live_probe_test.go` で、Unreal が組み立てる Responses の要求を
`deepseek-v4.1-flash` に 10 回送りました。匿名化した記録は
`harness/llm/clients/opencodego/testdata/probe-2026-11-02.json` にあります（キーと Session の値が
含まれないことはテスト内で確認済み）。

| 確認項目 | 結果 |
| --- | --- |
| 応答 | 10 回とも 200。標準の Responses の SSE イベント（`response.created` から `response.completed` まで） |
| 課金 | `usage.opencode.billing_source` は `go`、`usage.cost` は 0。Zen の従量課金ではない |
| `include` と `reasoning.summary` | 受け付けた。推論は `reasoning` 項目の `summary` で返り、`encrypted_content` は返らない |
| ツール往復 | 推論項目を送り返しても、送り返さなくても 200 で、ツールの結果を使って答えた。DeepSeek 単体で起きる 400 は出なかった |
| 途中の system メッセージ | 受け付け、指示にも従った。ただしモデルの推論要約には、user ターンに埋め込まれた信頼できない指示として映った |
| キャッシュ | 2 回目以降は共通部分がキャッシュから読まれた。追記で続けた会話でも読まれた |
| effort | `low`、`medium`、`high`、`xhigh`、`max` のすべてを受け付け、応答にもそのまま返った |
| 使用量 | `usage.input_tokens` はキャッシュ分を含む合計。`reasoning_tokens` は推論要約があっても常に 0 |
| `opencode.allowance` | `response.usage.opencode.allowance` に 1 回分の数値。同じ場所に `billing_source`、`list_cost`、`tokens` がある |
| 応答ヘッダー | 上限や残量を示すヘッダーはない。`X-Opencode-Log-Id`、`X-Zen-Model` がある |

ここから分かったこと:

- Responses で動くので、Chat Completions のアダプタは作りません。DeepSeek のネイティブ API は
  chat-completions ですが、Unreal の要求は Responses のままで通ります。
- 推論内容の再送は、Unreal の既存の扱い（`llm.Reasoning.Raw` をそのまま送り返す）で問題ありませんでした。
- 途中の system メッセージは、OpenCode の変換で user ターン内の文として渡っている可能性が高いです。
  ただし Unreal は途中に system メッセージを送らないので、影響はありません。
- `x-opencode-session` が同じでないとキャッシュが効かないのかは、2026-11-05 に確かめました。値が違っても
  効きます。このヘッダー自体は必須で、付けない要求は 400 で断られます。

## テスト方針

合格の条件は、次の 3 区分で定義します。

| 区分 | 対象 | 合格条件 |
| --- | --- | --- |
| 既存テストの維持 | 全テスト（Go のテスト関数と Fuzz） | 期待値を変えずに全部通す |
| OpenCode 専用テスト | ストリーム処理、文脈管理、Session の切替、子の実行、再試行、リダイレクト拒否など | Codex のテストと同じ観点の OpenCode 専用テストを新設して通す（一覧で回している `providers_stream_test.go` だけは既存の一覧に OpenCode の行を足す） |
| 置き換え | OAuth と Docker Sandbox の 26 件、Codex CLI カタログの 15 件 | 同じ観点の OpenCode 版を新設する。Docker Sandbox 経路は対象外 |

専用テストは Codex のテストが増えても自動ではついてこないので、Codex 側のテストを変えたときは、
対応を見直します。

偽のサーバーでは DeepSeek の実際の挙動までは証明できないので、次を加えています。

- 接続検証で実際の通信を匿名化して記録し、テストの入力に使う。
- `UNREAL_LIVE_OPENCODE_GO=1` のときだけ動く実機テストを用意する。CI では動かないので、手元で
  実行する。

実機テストは、どれも実際の API に要求を送るので、あらかじめ送る回数の上限を決めて実行します。
上限を超えた分は送らずに断ります。キーはテストが読まず、Host かクライアントが保存場所から読みます。

| テスト | 確かめること | 送る回数 |
| --- | --- | --- |
| `harness/llm/clients/opencodego` の `TestLiveOpenCodeGoResponsesProbe`（`UNREAL_OPENCODE_GO_CAPTURE` に匿名化した記録を書く） | 接続検証。Responses の要求が通るか | 10 回まで |
| `harness/llm/clients/opencodego` の `TestLiveOpenCodeGoClient` | Session のヘッダー、キャッシュ、ツール往復 | 4 回（再試行しない） |
| `cmd/unreal-agent` の `TestLiveOpenCodeGoThroughHost` | 実際のモデル一覧を読み、Host で 1 ターン送る | 1 回 |
| `cmd/unreal-agent` の `TestLiveOpenCodeGoSubagentsThroughHost` | `lead` と worker を一周させる | 20 回まで |

## 未確認事項

2026-11-02 の接続検証で確かめた項目は「確認済み」とし、詳細は「接続検証（2026-11-02）」に書きました。

- 確認済み: DeepSeek が Responses の変換経由でツール往復できる。推論項目を送り返しても、送り返さなくても動く
- 未確認: OpenCode が送り返した推論要約を DeepSeek の `reasoning_content` として渡しているか
- 確認済み: `reasoning.summary` と `include` は受け付ける。`encrypted_content` は返らない
- 一部確認: effort の `medium`、`xhigh` は受け付け、そのまま返る。内部での扱いは未確認（Flash の一覧に
  あるのは `low`、`high`、`max` だけ）
- 確認済み: キャッシュは追記型の文脈で効く。新しい `x-opencode-session` の値でも、同じ前置きは初回から
  キャッシュが効いた（2026-11-03）。キャッシュがどの単位で共有されるかは未確認
- 未確認: `prompt_cache_key` を受け付けるか（送っていない）
- 未確認: 画像入力（`images` は宣言していない）
- 未確認: 途中の system メッセージがどう渡るか。user ターン内の文として渡っている可能性が高い。
  Unreal は途中に system を送らないので影響しない
- 未確認: 上限到達時のエラー形式
- 確認済み: `opencode.allowance` の位置と形式（`response.usage.opencode.allowance`、1 回分の数値）。
  値の意味（利用枠から使った米ドル額か）は未確認
- 未確認: Linux での保存手順（`read -rs`）は、実際には試していない
- 確認済み: 実際の API で、Flash の `lead` が `worker-low` を起動し、その報告を受けて終われる（2026-11-04。
  設計者の役はテストが担った）
- 確認済み: 実際のインストールで、Claude（設計者）から Flash の `lead` と `worker` まで一周できる（2026-11-04）
- 確認済み: サブエージェントの要求でもキャッシュは効く。2 回目以降の要求に効いた（2026-11-04〜05）
- 未確認: 実際のインストールでの確認で、キャッシュが 4 回とも 0 だった理由。OpenCode が返した値が 0 で、
  同じ形の要求では再現しなかった
- 確認済み: `x-opencode-session` のない要求は 400 になる。値を変えても、同じ前置きのキャッシュは効く
- 未確認: モデル一覧の各項目（`capabilities.reasoning.effort`、`limit.context`）の意味は、公開の文書では
  確かめていない。2026-11-03 の一覧の形から読んだ
- 確認済み: 2026-11-05 の実機確認で、Flash 以外の 22 モデルが、実行した段階をすべて通った
- 未確認: 各モデルは 1 回の実機確認しかしていない（同じモデルで繰り返していない）。繰り返したときの
  安定性、負荷がかかったときの上限、時間帯による違いは未確認
- 確認済み: Gemini 4 Argon は、2026-11-14 の実機確認で `plain` を含む 8 段階すべてが通った。
  2026-11-05 の 429「Endpoint is unavailable」は再現しなかった
- 未確認: 他のモデルの会話を Gemini 4 Argon に切り替えたときの引き継ぎ（Gemini の思考の署名を含む）
- 未確認: MiMo-V2.6-Flash のツールの扱い。`tool_call` でツールを呼ばず、ツール結果、途中の system
  メッセージ、追記の続きは実行していない
- 未確認: `none`、`minimal`、`toggle` の扱い（Unreal は送らない）
- 未確認: GLM-5.3 のキャッシュが 8 回とも 10〜50 のままだった理由（GLM-5.2 は約 2.3k まで増えた）
- 確認済み: 実際の TUI で、同じ会話を `deepseek-v4-pro`、`kimi-k2.7-code`、Claude の順に切り替えても、
  前の内容を引き継げた（2026-11-06）
- 旧要約方式での計測: Claude の会話を `deepseek-v4-pro` に替える向きは、新しいモデルが会話を読んで要約を書くところ
  までと、その要約の内容が正しいところまで（2026-11-06、Claude が確認）。その要約から答えるかは未確認
  （返事の前にターンを止めた）

## 配布者による無効化

既定は有効です。案件向けに禁止する場合は `make snapshot OPENCODE_GO_ENABLED=false` で作り、共通キットへ `--disable-opencode-go` を付けて再梱包してください。[ほかのビルド方法と記録](ja/distribution.md#opencode-go-を無効にした配布物)も参照してください。

無効版は `/model`・discovery・モデル一覧・認証登録から接続全体を除外します。キーのファイル指定・環境変数指定も値を表示せず拒否します。環境変数・以前の設定・保存済みの選択では戻せず、ソースからの再ビルドが必要です。OpenCode Go を選んでいた Session はそのまま開いて `/model` でほかの接続を選ぶよう案内し、推論を拒否します。子の指定も案内つきで失敗し、自動切替は行いません。`KIT.json`・`SNAPSHOT.json` の `opencode_go_enabled` と各実行ファイルの `--version` で確認できます。
