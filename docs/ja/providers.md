# プロバイダ設定 (Providers)

**言語:** [English](../providers.md) | 日本語

Unreal Agent は共通の `/model` で、Claude・ChatGPT のサブスク、任意の OpenAI・Anthropic API 接続、OpenCode Go に対応します。**API キーを使う接続は明示的に選んだときだけ使います。** OpenAI・Anthropic API は別課金、OpenCode Go は Go のサブスク用キーです。接続先の自動切替や、サブスク認証切れ時の API キーへのフォールバックは行いません。[OpenCode Go](#opencode-go-api-key) も参照してください。

---

## モデルの選択と切り替え (`/model`)

1. TUI で `/model` を入力し、`ChatGPT（サブスク）`、`Claude（サブスク）`、`OpenAI API（API キー・別課金）`、`Claude（API キー・別課金）` から接続を選びます。キーを保存すると `OpenCode Go` も選べます。API の行は明示的に登録した場合だけ表示されます。
2. その接続のモデル一覧からモデルを選び、対応する場合は推論の深さ（effort）を選択します。
3. 後からログインした場合や外部 CLI を更新した場合は、`/model refresh` で認証状態とモデル一覧を再検出し、`/model` で選択します。インストールのやり直しは不要です。既存 Session のプロバイダは自分で選び直すまで維持されます。

> **補足:**
> - 初回起動時は利用できる Claude・ChatGPT のログインを優先し、両方使えれば **Claude**、片方ならその接続で始まります。API キーの登録だけではその接続を選択しません。API キーを使う接続しかなくても、起動後に `/model` で選ぶか、初期接続を `UNREAL_HARNESS_LLM_PROVIDER=openai`、`claude-api`、`opencode-go` で明示します。`UNREAL_HARNESS_LLM_MODEL` でモデルも指定できます。
> - 未ログインのプロバイダもメニュー内に理由と準備手順付きで表示されます。
> - サブスクが両方未準備でも起動します。最初の入力ではログイン手順を案内し、API 接続が登録済みなら `/model` へ案内します。`/login` でもサブスクの手順を確認できます。
> - 明示的に無効化したプロバイダはログインだけでは有効になりません。Codex は `bash development.sh --configure-providers --codex-auto`、Claude は `bash development.sh --configure-providers` で再び有効にします。
> - モデル一覧に表示されていても、実際の利用可否は選択したサブスクまたは API アカウントの権限に依存します。接続一覧の表示では推論しません。OpenAI API のモデル一覧はその接続を選んでから取得し、Go の自動検出はキーなしで公開モデル一覧を読みます。

---

## Claude（Claude Code CLI 連携）

### セットアップ手順

1. Unreal を実行する Linux 環境に公式の `claude` CLI をインストールします。
2. 別の Bash シェルで `claude` を起動し、Claude Code 内で `/login` を実行して Claude サブスクリプションでログインします。
3. Unreal に戻り、`/model refresh` を実行します。

### 仕組み

- **プロバイダ ID:** `claude`
- **会話とツールの管理:** Claude Code 自身が会話履歴と内蔵ツールを管理します。Unreal は `claude` CLI を起動してプロンプトを渡し、実行されたツールの結果を `claude_code` Operation として記録します。
- **権限と Bash フォールバック:** Claude Code の内蔵ツール呼び出しはすべて Unreal の承認フックを通過します。Claude Code 自身が Bash コマンドの実行を辞退した場合は、Unreal のネイティブ Bash 実行器が同じ権限モードのもとで引き継いで実行し、結果を Claude に返します。
- **モデル一覧:** Claude Code の初期化レスポンスから自動取得されます。
- **会話圧縮と指示書:** [コンテキストと指示書](context.md) を参照してください。

---

## Codex（通常のサブスクリプション経路）

### セットアップ手順

1. 公式の `codex` CLI をインストールし、Bash で `codex login` を実行して ChatGPT サブスクリプションでログインします。
2. 初回導入時などにモデル一覧が未作成と表示された場合は、一度 `codex` CLI を開いてローカルの `models_cache.json` を生成させます。
3. Unreal を起動するか、既存セッションで `/model refresh` を実行します。

### 仕組み

- **プロバイダ ID:** `openai-codex`
- **実行パイプライン:** Unreal 内蔵の Codex アダプタを通じてリクエストを送り、Unreal のネイティブファイルツール・AST ツール・Bash を実行します。
- **CLI の有無:** 選択した認証とモデル一覧が有効な間は、Codex CLI がなくても推論できます。ログインと認証の更新には公式 CLI を使います。
- **認証情報の管理:** OAuth トークンの保存と更新は外部の `codex` CLI が担います。Unreal は認証ファイルを読み取り専用で参照し、独自のキーストアへトークンをコピーしません。
- **認証ファイルの探索順序:**
  1. `--codex-auth-file` フラグ（ソースビルド版ランチャー）
  2. `OPENAI_CODEX_AUTH_FILE` 環境変数
  3. `$CODEX_HOME/auth.json`
  4. `$HOME/.codex/auth.json`
  ※ 通常のプライベートファイルである必要があり、同じフォルダ内の `models_cache.json` からモデル一覧を読み取ります。

- **モデル一覧の取得規則:** 通常 Codex のモデル探索は、選択した認証ファイルと同じディレクトリの `models_cache.json` の読み取りのみを行います（`/model refresh` 時にも CLI を起動して列挙することはしません）。モデル一覧の取得と認証・推論権限の検査は独立しています。

---

## 任意の API キー接続

キーを入れたファイルを **0600** にし、使いたい接続だけ登録します。

```bash
chmod 600 /absolute/openai.key /absolute/anthropic.key
bash development.sh --openai-api-key-file /absolute/openai.key --anthropic-api-key-file /absolute/anthropic.key
```

各フラグは任意です。OpenAI API（`openai`）は既存の Responses アダプタを使い、Codex のサブスクログインは不要です。Claude API（`claude-api`）は公式 Claude Code CLI が必要で、会話・ツール・権限の扱いはサブスクの Claude と共通です。

- **参照先の明示:** 設定に保存するのはファイルパスか、`--openai-api-key-env NAME` / `--anthropic-api-key-env NAME` で明示した環境変数名だけです。`OPENAI_API_KEY` や `ANTHROPIC_API_KEY` が存在するだけでは API を登録・選択しません。キーの値は設定・ログ・履歴・画面・エラーへ書きません。ファイルは使用時に読み直し、0600 でないものは拒否します。
- **選択:** 登録後、`/model` で API の行を選びます。選択した接続は Session の resume 後も維持され、サブスクと API の間では自動で切り替えません。`/model` とステータス表示の API ラベルで別課金を示します。
- **モデル一覧:** OpenAI API の接続を開くと、その API の [`GET /v1/models`](https://developers.openai.com/api/reference/resources/models/methods/list) を参照します。この応答には effort 情報がないため、省略します。Claude API は選択した Claude Code の初期化応答から取得します。
- **Claude の認証:** `claudeagent` が API 経路を選択したときだけ `ANTHROPIC_API_KEY` を渡します。サブスク経路では、従来どおり API キーと auth-token の環境変数を除去します。Claude Code **2.1.322** で偽 API キーと偽サブスク OAuth を同居させた試験では、`init.apiKeySource=ANTHROPIC_API_KEY`、ローカル偽サーバーへの認証は `x-api-key` でした。実推論は行っていません。追加の settings フラグは不要です。空の `--setting-sources` では OAuth を隔離できず、`--bare` は指示書の読み込みも変えるため、この経路では付けません。
- **使用量:** 応答の `Usage.AuthMethod` と `claude_usage` の接続情報でサブスクと API キーを区別し、トークン数を残します。API 費用の計算は含みません。

手動管理の `runtime.json` に記載する参照先は次の形です。

```json
{
  "Launcher": {
    "Version": 1,
    "APIKeys": {
      "openai": {"file": "/absolute/openai.key"},
      "claude-api": {"file": "/absolute/anthropic.key"}
    }
  }
}
```

この断片は接続の登録だけを行い、Session の選択は変えません。環境変数を明示する場合は `file` の代わりに `{"env": "CHOSEN_KEY_NAME"}` を使います。

<a id="docker-sandboxes-codex-oauth"></a>
## Docker Sandboxes Codex OAuth

既存の **Docker Sandbox** 内で Unreal を動かす場合、Docker の認証フォワードプロキシを経由してホスト側の OpenAI OAuth ログインを安全に利用できます。

### セットアップ手順

1. **Docker ホスト側:** Sandbox ツールで OpenAI OAuth ログインを設定します。
   ```bash
   sbx secret set openai --oauth
   ```
   詳しくは [Docker 公式の Codex セットアップ手順](https://docs.docker.com/ai/sandboxes/agents/codex/) を参照してください。
2. **Sandbox 内:** [インストール手順](install.md) に従って共通キットを導入します。既定の自動判定（`--codex-auto`）でも認識されますが、明示的に指定する場合は `bash development.sh` に `--docker-sandbox-oauth` を追加します。
3. Unreal を起動し、`/model` で **ChatGPT（サブスク）** を選択します。

### 検証条件とモデルキャッシュの仕組み

- **環境と設定の検査:** `SBX_CRED_OPENAI_MODE=oauth`、非秘匿の認証マーカーファイル、`.codex/config.toml` の `sandboxd` プロバイダ設定、および Codex エンドポイント URL を検査します（設定内の `forced_login_method = "api"` はプロキシ経路用の指定であり、API キー課金にはなりません）。
- **フォワードプロキシ:** 環境変数 `HTTPS_PROXY`（または `https_proxy`）に `http://host.docker.internal:3128` または `http://gateway.docker.internal:3128` が設定されている必要があります（大文字・小文字で異なる値が設定されている場合は拒否されます）。プロキシが通信時にホストの認証情報を付与するため、Unreal がホストのトークンを読み取ったり証明書を導入したりすることはありません。
- **モデル一覧の取得とキャッシュ:**
  - まず `models_cache.json` を確認し、存在しない場合は `codex app-server` のメタデータ取得（`model/list`）のみを実行します。取得結果は `~/.cache/unreal-agent/codex-cli-catalog.json` に保存され、24時間再利用されます。
  - `models_cache.json` がない状態で `/model refresh` を実行すると、保存済み CLI カタログのキャッシュを迂回して再取得します。プロキシ経路を検証した後、取得が失敗・遅延した場合は最後の成功済みカタログを保持します。
  - 一覧取得は推論の認証ではなく、直接通信・別の認証源・API キー・他プロバイダへのフォールバックは行いません。

プロキシ経路が有効でモデル一覧を取得済みなら、Codex CLI がなくても利用できます。

<a id="opencode-go-api-key"></a>
## OpenCode Go（API キー）

OpenCode Go は、管理ストアに API キーを保存すると追加できる 3 つ目のプロバイダです。接続先は OpenCode Go サブスクリプションのエンドポイント（`https://opencode.ai/inference/go/openai/v1`、Responses API）だけで、従量課金の Zen エンドポイントへは接続しません。利用できるのは、この窓口で実機に確かめた 24 個のモデルです（先頭は DeepSeek V4.1 Flash、`deepseek-v4.1-flash`）。MiMo-V2.6-Flash、Contributor の 2 個（送った内容が学習に使われる）、Free のモデルは出しません。

### セットアップ手順

1. 管理ストア（`~/.local/state/unreal-agent/credentials`、プロバイダ `opencode-go`、id `primary`）に、`unreal-agent-auth login ... --key-stdin` でキーを保存します。TUI の `/login` に、その環境での実際のコマンドが表示されます。
2. `/model refresh` を実行します。`/model` に「OpenCode Go · available」と表示され、Host の再起動は不要です。
3. `/model` で選ぶと、その会話自身のモデルだけが切り替わります。モデルは 24 個から選び、effort があるモデルは続けて effort も選びます（一覧に effort がないモデルは、その段階が出ません）。

### 仕組み

- **プロバイダ ID:** `opencode-go`
- **登録:** config への登録は不要です。管理ストアにキーがある間だけ登録され、キーがなければ何も変わらず、`/model` に現れず、OpenCode へ何も送りません。
- **選択:** キーの保存は接続先の登録だけで、自動選択しません。`/model` か `/model-sub` で選ぶか、初回に `UNREAL_HARNESS_LLM_PROVIDER=opencode-go` を明示します。自動の初期選択には含めません。
- **接続先:** 上記の Go サブスクリプションのエンドポイントだけに送ります。Zen や別プロバイダ、API キーへ自動で切り替えることはなく、選択したモデルが利用できない場合は要求が失敗します。

詳細は [OpenCode Go](../opencode-go.md) を参照してください。

---

<a id="delegated-gpt-work"></a>
## Delegated GPT work（GPT サブエージェントへの作業委譲）

Codex が設定済みか自動検出が有効、または OpenAI API が明示的に登録済みで、Permission Modes の `Maximum` が設定されており、明示的な `Subagents` 設定がなければ、以下の GPT テンプレートを準備します。テンプレートのモデルは親 Session の選択モデルとは独立しています。

`/model-sub` の保存済み選択がない場合、**OpenAI API** を統括に選んだ Session では、組み込みの GPT の子とその作業者も `openai` を使います。それ以外の統括では、従来どおりサブスクの `openai-codex` を使います。可用性を理由に接続を切り替えません。API の子はテンプレートのモデルと設定された effort を保ちます。サブスクでは下表の effort を維持します。利用者が設定したテンプレートや既存の子は記録された接続を保ちます。

Claude の統括が GPT の子を案内するのは、その Session で Codex の認証とモデル一覧が使える場合だけです。使えなければ Claude Code の Agent（`haiku` / `sonnet`、`/model-sub` で Claude を選んだ場合はそのモデル）を使います。選択した GPT 接続が使えない場合は手順を案内し、別の接続には切り替えません。
子に OpenCode Go を選んだ場合は、その接続と保存キーの可用性を確認し、モデル一覧は子の起動時に読みます。GPT の子がある Session でも、Claude モデルを明示した作業には Agent を使えます。

| テンプレート / 役割 | モデル・Effort | 主な用途 |
| --- | --- | --- |
| `lead` | `gpt-6.1-sol` (`max`) | 実装作業の進行・統括（配下に `worker-*` や `collector` を起動可能） |
| `worker-low`, `worker-medium`, `worker-high`, `worker-xhigh`, `worker-max` | `gpt-6.1-sol`（各 effort） | コード実装および担当範囲のテスト実行 |
| `collector` | `gpt-6-luna` (`medium`) | 検索、ファイル・ログからの値抽出、URL/API 確認、CI 状態確認、読み取り専用の要約 |

### 委譲の動き

1. Claude に「サブエージェントに委譲して」「lead に実装を渡して」と指示します。
2. Claude は `~/.local/bin/unreal-agent subagent` コマンドでサブエージェントを起動してタスクを渡し、いったん自分のターンを終了します。
3. サブエージェントが完了または連絡を送ると、親セッションが自動的に再開して結果を処理します。
4. サブエージェントの権限モードは親セッションの選択モード（および上限ポリシー）に従い、承認要求は親エージェントが判断します。詳しくは [権限管理](permissions.md) を参照してください。

> **環境変数の継承:** 子は Host の環境（`PATH`、`HOME`、ツール設定など）を継承し、除去するのは `OPENAI_CODEX_ACCESS_TOKEN` と `OPENAI_CODEX_ACCOUNT_ID` のみです。環境隔離を保証するものではありません。

### サブエージェントのモデルを選ぶ（`/model-sub`）

`/model-sub` はサブエージェントが使うモデルを選びます。**OpenAI Codex**（サブスク接続、表示名は `ChatGPT（サブスク）`）か、**OpenCode Go** → 取り込み元が実機で確かめた 24 個のモデル（先頭は DeepSeek V4.1 Flash）か、**Claude**（`/model` に Claude が表示されているときに選べ、続けて `/model` が Claude に出すのと同じモデルから選びます）です。選択は `STATE/subagent-model.json` に保存され、同じインストールの shared state を使うすべての Session に適用され、その後に開始する子から有効になります。実行中の子は自身のモデルを維持します。`/children start ...` で明示的に runtime を指定した子と、config の明示的な `Subagents` や `SubagentDefaults` は影響を受けません。`subagent start --model ID --effort LEVEL`（テンプレートのプロバイダはそのままで、モデルと effort だけを変える指定）を組み込みテンプレートに使えるのは、`/model-sub` が OpenAI Codex のときだけです。OpenCode Go か Claude を選んでいる間は Codex で動かさずに断り（`override_unavailable`）、Claude への案内からも外します。

選択をまだ保存していなければ、OpenAI API の親の組み込み GPT 子はその明示済み API 接続を使い、それ以外の親は Codex を使います。「OpenAI Codex」を明示的に保存すると、API の親でもサブスクの Codex 接続に固定します。指定先が使えない場合に、親の API 接続へ切り替わることはありません。

Claude を選んだ場合、Claude Code の Agent が選んだモデルでサブエージェントを動かし、Unreal の子は作りません（Unreal は Claude Code の起動環境に `CLAUDE_CODE_SUBAGENT_MODEL` と `CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1` を設定し、委譲の案内では `haiku` / `sonnet` の代わりにそのモデル名を出します）。FORCE を付けると、組み込みの Explore / Plan を含むすべての Agent のサブエージェントが選んだモデルで動き、Agent ツールから呼び出しごとの `model` がなくなるので、Claude は別のモデルを指定できません（Claude Code 2.1.257 以降が必要です）。Claude Code の文書によると、fork と、サブエージェントで動く `model: inherit` の skill は Claude 自身のモデルのままです。Agent の認証は親 Session で選んだ Claude 接続を使い、子モデルの選択だけでサブスクと API キーを切り替えません。Claude の親には Unreal の子がないので、選択は Claude の次のターンから効き、実行中の子は自身のモデルを維持します。Codex や OpenCode Go の親が組み込みテンプレートの子を起動しようとすると `provider_unavailable` で断り、`/model-sub` で OpenAI Codex か OpenCode Go を選ぶよう案内します。別のプロバイダへ切り替えることはありません。実機の CLI で、環境変数が Claude Code の Agent のサブエージェントに届くことと、そのツール呼び出しが TUI に表示されることは未確認で、実機での確認が必要です。

OpenCode Go を選んだ場合、どのテンプレートも `/model-sub` で選んだモデルで動きます。DeepSeek V4.1 Flash では、これまでどおり次のとおりです。

| テンプレート | Effort |
| --- | --- |
| `lead`, `worker-max` | `max` |
| `worker-xhigh`, `worker-high`, `worker-medium` | `high` |
| `worker-low`, `collector` | `low` |

役割ごとの effort は、選んだモデルの一覧から決まります。`lead` と `worker-max` は一番高い effort、`worker-low` と `collector` は一番低い effort、他の worker は自分の effort があればそれ、なければ既定の effort（既定が自分より低いときは、自分より低い中で一番高い effort。`qwen3.8-max` では `worker-high` が `medium`）です。一覧に effort がないモデルは、effort なしで動きます。選択したモデルが利用できない場合（上限到達、障害）、子の起動または要求は失敗し、Codex や別プロバイダへ自動で切り替わることはありません。OpenCode Go を選んでいるとき、Claude が子を案内するのは、Codex が利用できる間ではなく、OpenCode Go が登録されキーが保存されている間です。モデル一覧は子の起動時に読みます。接続先の自動検出が有効（既定）なら、Codex にログインする前からテンプレートが用意されるので、後からの選択も次に起動する子から使われます。Codex を無効にした Host や自動検出を切った Host で、Host 起動時に OpenCode Go を選んでいなかった場合だけテンプレートがなく、選択は保存され次の Host 起動から使われます（`/model-sub` がその旨を表示します）。

### 配布時に OpenCode Go を無効にした場合

`disable_opencode_go` で作った配布物は、OpenCode Go 接続全体を `/model`・`/model-sub`・discovery・モデル一覧・認証方式一覧から除きます。環境変数・古い設定・キー・保存済み Session では有効に戻せません。Session は元の選択を保持して開き、無効化の理由と `/model` での選び直しを案内します。子も `/model-sub` での選び直しを案内して失敗し、自動フォールバックは行いません。`--version` の `opencode_go_enabled=false` で確認できます。[配布物の作り方](distribution.md#opencode-go-を無効にした配布物)を参照してください。
