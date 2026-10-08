# Multi-provider runtime と subagents

通常利用は `unreal` / `unreal my-project` です。通常 launcher は既存設定の
初期 runtime を維持し、利用可能な external providers を追加登録します。
初回 bootstrap、Team trust の一度だけの設定、Tool Bridge policy status は
[通常 launcher](normal-launcher.md) を参照してください。以下の明示 config と
smoke 手順は operator 設定・検証用で、日常起動に専用パスを指定する必要はありません。

Unreal Host は `Runtime.Provider` の初期 backend と、top-level `Providers` の
追加 backend を同時に登録します。各 provider の client factory、catalog、tool
capability、external authentication は別です。provider failure による自動 fallback、
Auto routing はありません。モデル同士が直接呼び合う構成ではなく、
Unreal が Session・runtime revision・Subagent・Permission・Operation を所有します。

## Config と既存 Session

既存の single-provider JSON は変更せずに使えます。`Providers` は追加登録だけで、
`Runtime.Provider` と重複する key は error です。たとえば Claude が初期 provider の場合:

```json
{
  "Providers": {
    "openai-codex": {
      "Provider": {
        "version": 1,
        "provider": "openai-codex",
        "model": {
          "id": "gpt-6.1-sol",
          "family": "openai-reasoning",
          "capabilities": ["tools", "reasoning"]
        },
        "endpoint": "https://chatgpt.com/backend-api/codex",
        "auth": {
          "provider": "openai-codex",
          "method": "oauth",
          "id": "external-codex"
        },
        "maxAttempts": 1,
        "source": "operator configuration"
      },
      "ReasoningEffort": "medium"
    }
  }
}
```

これは既存 JSON へ追加する部分だけです。初期 provider が Codex なら
`Providers["claude-code"]` に `Provider`、`ReasoningEffort`、`ClaudeCode` を入れます。
Claude の `Provider.auth` は `claude-code / oauth / external-claude-code`、
`ClaudeCode.Binary` は installed executable の path、Team/Enterprise では
`ClaudeCode.managedPolicyMode` を明示的に `trust` にします。モデル固定リストは不要です。

追加登録は既存の immutable creation identity を変更しません。最初の切替から
`RuntimeSelection.Binding` に **非 secret な backend config snapshot** を記録します。
credentials は reference のみで、token/account ID/auth file 内容は含みません。
resume 時には選択済み backend の登録設定がこの snapshot と一致する必要があります。
削除・変更された backend に自動で置き換えることはありません。

既存 `Runtime`、profile、prompt、workspace、subagent template を変更した場合の
configuration mismatch 検査は維持します。特に新しい `Subagents` を加える場合は、
既存 history を保持し、別 Session を作成してください。履歴を編集する migration はありません。

## Main runtime

`/model` は複数登録時に Provider → Model → Effort の順で開きます。
↑↓/Tab、scroll、Enter、Esc、current selection、Tiny/Narrow を維持します。
単一登録時は既存の Model → Effort flow を維持します。最後の effort 確定までは
canonical selection を変更しません。`/model refresh` は現在の provider catalog を更新します。

catalog は provider namespace ごとに独立しています。同じ alias が別 provider にあっても
結合しません。Codex は既存 `models_cache.json`、Claude は isolated control process の
`initialize → list_models` と既存 1 minute cache を使います。公式 discovery が unavailable
なら provider ごとの explicit/current-only fallback を維持し、モデル一覧を推測しません。
モデルごとの supported effort と Unreal enum の intersection のみを表示します。

provider picker の available/auth required/unavailable は read-only auth/catalog probe の結果です。
model inference は行いません。一方が signed out でも複数登録 Host は起動できます。
CLI の実行可否は catalog fallback と別に検証します。設定済み model が残っていても、
CLI 不在・未対応 version を available と扱わず、child 作成は事前に拒否します。
選択中 backend の失敗はその turn の typed error のままです。別 provider へ転送しません。
単一 Claude config の既存 startup preflight は維持します。

切替は既存 `runtime_selection → runtime_applied` の revision を使います。
実行中 request/tool loop は元の provider/model/effort のまま完了し、既存の安全な
turn boundary で次の selection を適用します。未完了 Operation がある間は適用を待ちます。
Turn は `RuntimeRevision` で canonical selection を参照し、response の observed model は
selection alias と別に保持します。resume は applied と pending の両方を復元します。

capability も同じ境界で切替します。Claude では `Request.Tools=[]`、新規 Translator 受付なし、
text-only preamble、skills なしです。明示した `ClaudeCode.ToolBridge.Enabled=true` の
Claude runtime では共有 Unreal tool schema/Translator を利用します。
Codex へ戻すと元の tool schema/Translator が復帰します。
Operation manager と履歴 codec は維持します。context の provider provenance は既存履歴から
再計算し、他 provider の private reasoning/continuation state は送信しません。
歴史上の tool request/receipt は必要に応じて data として引用し、同一 Codex provider の
tool loop は native tool call/result のままです。canonical history 自体は変更しません。

header は幅がある場合に provider/model/effort を表示します。`/analyze` の Overview は
provider、selection ID、effort、revision、capability を表示します。Turns/Agents と
JSON/Markdown export も provider と observed model を識別できます。export は従来どおり
metadata/statistics only で、backend binding、credential、本文、managed settings を含めません。

## Child runtime と operator command

multi-provider Host の child runtime request を省略すると、作成時の parent **applied**
selection をコピーします。pending はまだ有効ではありません。以降 parent が変わっても
child は追従せず、自身の immutable runtime configuration を resume/retry に使います。
既存 single-provider の明示的な固定 template runtime は従来どおり維持します。

```text
/children start worker -- TASK
/children start worker openai-codex MODEL EFFORT -- TASK
/children start worker claude-code MODEL EFFORT -- TASK
/children retry-start
```

`-` effort は「effort parameter なし」で、そのモデルの catalog が明示的に許可する場合のみ有効です。
`/children retry-start` は uncertain start の同じ input ID を再送し、記録済み runtime を再解決しません。
既存 `/child ID` は focus のままで、history/send/cancel/resume/retry の意味は変更しません。
`/child-resume` は既存の owner/recovery 確認、`/child-retry` は既存の uncertain control 再送です。
terminal child の outcome を上書きする再実行機能ではありません。

Codex の `SubagentStart` にも optional `runtime` object を追加しています:

```json
{"template":"worker","task":"TASK","runtime":{"provider":"claude-code","model":"sonnet","effort":"high"}}
```

これは希望値です。Host resolver が provider 登録/auth availability、catalog model、effort、
child completion capability と permission subset を検証して確定します。invalid は closed
`child runtime` error で、入力/Operation/process を作る前に拒否します。モデルは credentials、
permission、process args、runtime JSON を指定できません。recorded child config の replay は
parent の現在選択を再評価せず、登録 binding と template の immutable 部分を検証します。

child template の Tools/read roots/write roots/network origins は parent permission 以下で、
process tool は引き続き denied、filesystem/network unrestricted も禁止です。
Claude child の CLI transport は parent に許可された provider process のみです。
この transport の許可は子の Process/Tool Operation permission を変更しません。
child environment は whitelist のままで、Claude が登録されている場合のみ HOME と必要な
非 secret CLAUDE_CONFIG_DIR source path を追加します。Codex は従来の explicit auth file
path argument を利用し、token/OTEL/API/routing environment は継承しません。

text-only Claude child は最初の durable text response の後、Unreal が既存 canonical Finish record に
`ModelTurnID` を付けて完了します。Finish tool/Operation を捏造せず Tool Operation は 0 件です。
response と finish の間で crash した場合も、記録済み response から完了を復元し、再推論しません。
Tool Bridge を明示した Claude child と Codex child は既存 Finish tool → Translator →
Operation → canonical Finish の経路です。
両方の結果は既存 provider-neutral FinishResult と親 Operation receipt で返します。

`/children`、child dock/rail/focus と viewer Row は parent relation、status、provider/model/effort、
observed model を read-only projection として公開します。未観測 child の activity/usage は推測しません。
`/view orchestration` と Wide `/view split` はこの既存 projection から親子関係と
Operation を表示します。Tool Bridge 専用の別 UI は作りません。

Claude は既定で text-only です。`--safe-mode`、empty built-in tools、no session persistence
を維持し、明示 opt-in の [Tool Bridge v1](claude-tool-bridge.md) では SDK provenance を
確認した Unreal server だけを公開します。builtin/foreign MCP/Claude task の実行は拒否します。
**Claude built-in tool execution = 0**。bridge 有効時は Claude parent が共有 SubagentStart を
要求できます。実際の child runtime 決定・permission・起動は Unreal が所有します。
operator command/core API も引き続き利用可能です。
`managedPolicyMode=trust` は管理者 policy の安全性検証ではなく、組織 policy を external trusted
administrative boundary としてユーザーが明示的に信頼する設定です。managed hooks/env/telemetry 等の
副作用はその境界にあり、Unreal Operation 内の実行保証とは別です。

## 実機 smoke（ユーザーが実行）

以下は config を作成するだけで、model request を送りません。既存の smoke JSON の
非 secret backend 定義を再利用します。primary Runtime を変更しない main config と、
child template を含む別 Session 用 config を作ります。既存 history を削除しません。

```bash
cd /home/e230038/src/unreal-agent
python3 - <<'PY'
import copy, json, os
from pathlib import Path
from urllib.parse import urlsplit
root = Path.home() / '.config/unreal-agent'
base = json.loads((root / 'claude-team-smoke.json').read_text())
codex = json.loads(Path('tmp/codex-live-dock-smoke.json').read_text())['Runtime']
backend = {k: copy.deepcopy(codex[k]) for k in ('Provider', 'ReasoningEffort')}
base['Providers'] = {'openai-codex': backend}
child = copy.deepcopy(base)
child['Permissions']['Tools'] = list(dict.fromkeys(
    child['Permissions'].get('Tools', []) +
    ['SubagentStart', 'SubagentSend', 'SubagentCancel', 'SendParent', 'Finish']))
runtime = copy.deepcopy(child['Runtime'])
runtime['SystemPrompt'] = ('Complete the delegated task. When tools are available, '
                          'call Finish alone with a bounded completion report.')
u = urlsplit(backend['Provider']['endpoint'])
child['Subagents'] = {'worker': {
    'Runtime': runtime,
    'Permissions': {'Tools': ['Finish', 'SendParent'],
                    'NetworkOrigins': [f'{u.scheme}://{u.netloc}']}}}
for name, value in [('multi-provider-main-smoke.json', base),
                    ('multi-provider-child-smoke.json', child)]:
    path = root / name
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as f:
        json.dump(value, f, indent=2)
        f.write('\n')
PY
```

既存 Team Host は memory 上の旧 config のままなので、TUI を Ctrl-D で detach 後、
**その dedicated socket を所有する serve PID を確認して SIGTERM** で停止し、終了を待ってください。
`/stop` は Session を止める command で Host 停止とは違います。lock/socket/history を削除しません。

```bash
./bin/unreal --config "$HOME/.config/unreal-agent/multi-provider-main-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent-claude-team-smoke" \
  --socket "$HOME/.local/state/unreal-agent-claude-team-smoke/run/host.sock" \
  claude-team-smoke
```

既存未回答 input があれば resume がその処理を再開する既存 semantics のままです。
`/model` で Claude → model → effort を選び、1 turn、次に Codex → model → effort を選び、
1 turn 送信してください。`/analyze` の Overview/Turns/Usage、header、detach/reconnect を確認します。

child template は新しい configuration なので別 state/socket/Session で開始します。

```bash
./bin/unreal --config "$HOME/.config/unreal-agent/multi-provider-child-smoke.json" \
  --state-directory "$HOME/.local/state/unreal-agent-multi-provider-child-smoke" \
  --socket "$HOME/.local/state/unreal-agent-multi-provider-child-smoke/run/host.sock" \
  multi-provider-child-smoke
```

1. `/model` で Claude を選びます（model/effort は実際の catalog から選択）。
2. `/children start worker openai-codex gpt-6.1-sol medium -- Reply with exactly: Codex child smoke OK. Then call Finish alone with status completed and that summary, with empty changedFiles/tests/blockers.`
3. `/children` と `/child ID` で Codex child と canonical completion を確認します。
4. 全 child 完了後、`/model` で Codex を選び、parent の text turn を1回完了させます。
5. `/children start worker claude-code sonnet high -- Reply with exactly: Claude child smoke OK`
6. Claude child の response、provider/model、canonical Finish、Tool Operation 0 件を確認します。
7. Ctrl-D で detach し、同じ command で再接続。Session stop 後の再起動/resume でも各 child の
   runtime と完了結果が変わらないことを `/children`、`/child-history`、`/analyze Agents` で確認します。

`sonnet/high` が組織の discovery catalog で selectable でない場合は、実際に表示された
selection ID/effort を使ってください。`/analyze Overview` は selection ID も表示します。
UI またはこの core API が実行元であり、Claude 自身の tool bridge は使用しません。
