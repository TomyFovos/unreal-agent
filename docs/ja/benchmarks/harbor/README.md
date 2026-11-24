# Harbor による製品評価

**言語:** [English](../../../../benchmarks/harbor/README.md) | 日本語

Harbor **0.22.0** から製品の Host を画面なしで起動します（`unreal-agent exec`）。
通常の runtime、権限処理、Project Instructions、Claude への統括 prompt、
`unreal-agent subagent`、作業者 template、子の完了通知を再利用します。
接続用の処理は課題を1つ渡し、Harbor のコンテナ内で Full Access を選び、
既存の one-shot 終了を待ちます。課題・トークン・費用・子の数の上限は追加せず、
別の認証先への自動切替も行いません。

## この開発 PC で build して、別の PC へコピー

Go と C コンパイラが必要なのは開発 PC だけです。**コミット済み**のソースから
Linux 向けの静的バイナリを build し、native AST parser も維持します。
bundle は Host、従来の runner、版を固定した Claude runtime installer、
commit と SHA-256 の manifest を含みます。既存の bundle は上書きしません。

```bash
make -C benchmarks/harbor build REVISION=HEAD
E2_REV=$(git rev-parse --short HEAD)
tar --exclude=.venv --exclude=__pycache__ --exclude=.ruff_cache \
  -czf "/tmp/unreal-harbor-e2-$E2_REV.tgz" \
  benchmarks/harbor docs/ja/benchmarks/harbor \
  harness/llm/clients/claudeagent/install.py "bin/harbor/$E2_REV"
```

このアーカイブを別の PC へコピーします。adapter と lockfile も含まれます。
**実行先の PC では bundle を build しません。**

## 別の PC：Windows・Docker Desktop・WSL2 Ubuntu

WSL 未導入の場合は、管理者の **Windows PowerShell** で次を丸ごと実行します。
Windows に求められた場合は再起動してください。

```powershell
wsl --install -d Ubuntu-24.04
wsl --update
wsl --set-default-version 2
```

Docker Desktop を導入・起動し、WSL 2 engine と Linux containers を選びます。
**Settings → Resources → WSL Integration → Ubuntu-24.04 → Apply** を有効にします。
詳細は [Docker の WSL 手順](https://docs.docker.com/desktop/features/wsl/) を参照してください。

以下は **Sandbox Bash（実行先 PC の WSL2 Ubuntu）** に丸ごと貼れます。
ジョブとキーは Windows 側の `/mnt/c` ではなく、Linux のホームに置きます。

```bash
set -euo pipefail
sudo apt-get update
sudo apt-get install -y curl ca-certificates python3
curl -LsSf https://astral.sh/uv/0.13.8/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
read -r -e -p 'コピーした .tgz の絶対パス: ' E2_ARCHIVE
mkdir -p "$HOME/unreal-harbor-e2"
tar -xzf "$E2_ARCHIVE" -C "$HOME/unreal-harbor-e2"
cd "$HOME/unreal-harbor-e2"
uv sync --project benchmarks/harbor --locked
uv run --project benchmarks/harbor --locked harbor --version
docker info >/dev/null
export BUNDLE="$(realpath bin/harbor/*)"
python3 - <<'PY'
from pathlib import Path
from getpass import getpass
root = Path.home() / '.config/unreal-harbor'
root.mkdir(parents=True, exist_ok=True, mode=0o700)
root.chmod(0o700)
for provider in ('openai', 'anthropic'):
    path = root / f'{provider}.key'
    if not path.exists():
        with path.open('x') as stream:
            path.chmod(0o600)
            stream.write(getpass(f'{provider} API key: ').strip())
    path.chmod(0o600)
PY
export OPENAI_KEY="$HOME/.config/unreal-harbor/openai.key"
export ANTHROPIC_KEY="$HOME/.config/unreal-harbor/anthropic.key"
```

`uv sync --locked` は同梱 lockfile の Harbor 0.22.0 を導入します。
Host と installer はアップロード前に manifest と照合します。
Claude を使う構成では、コンテナに **Node 22.16.0** と **Claude Code 2.1.322** を導入します。
展開前に Node の [公式 SHA-256](https://nodejs.org/dist/v22.16.0/SHASUMS256.txt) と
[npm のプラットフォームパッケージの SHA-512 integrity](https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/2.1.322)
を検証し、npm lifecycle script は実行しません。
installer と Claude Code の通信・道具固有の処理は `harness/llm/clients/claudeagent` 内にあります。

adapter は必要なキーだけをコンテナの private directory へ送り、**0600** にして、
E1 の API 接続に登録し、明示的に選択します。
`--ak openai_key_file=...` と `--ak anthropic_key_file=...` に入るのはパスだけです。
`--ae OPENAI_API_KEY=...` / `ANTHROPIC_API_KEY=...` も受け付けますが、
Harbor 自体が `--ae` の値をジョブ設定に保存するため、公開用の実行ではキーファイルを使います。
従来の runner の `UNREAL_HARNESS_LLM_API_KEY` は維持しています。

## SWE-bench Verified の固定した課題・3構成

2026-11-13、Harbor 0.22.0 の `harbor dataset list --legacy` と
[registry](https://raw.githubusercontent.com/laude-institute/harbor/main/registry.json) で
**`swebench-verified@1.0`・500課題**を確認しました。全課題が参照する commit は
[`86723674f04e4209ac479d0fb75d9d9f44b4377e`](https://github.com/laude-institute/harbor-datasets/tree/86723674f04e4209ac479d0fb75d9d9f44b4377e/datasets/swebench-verified) です。
[Hub の名前](https://hub.harborframework.com/datasets/swe-bench/swe-bench-verified)
`swe-bench/swe-bench-verified` も 0.22.0 の別の package resolver が扱います。
そちらの版省略は `latest` です。Hub package の固定版は未確認なので、以下は確認済みの legacy 版を使います。

`-n` は**同時実行数**、`-k` は**課題ごとの試行数**、`-l` は課題数の絞り込み、
複数の `-i` は課題名または glob の選択です。以下の3課題名は同じ registry で確認したものです。
比較するすべてのエージェントに同じ課題を渡します。

セットアップ後の **Sandbox Bash** へ丸ごと貼れます。

```bash
DATASET=swebench-verified@1.0
TASKS=(-i django__django-15098 -i sphinx-doc__sphinx-8593 -i sympy__sympy-13852)
hrun() { uv run --project benchmarks/harbor --locked harbor run -d "$DATASET" -e docker -n 2 -k 1 "${TASKS[@]}" "$@"; }
unreal_run() {
  hrun -a harness_harbor.agent:UnrealAgent -m "$3" --job-name "$2" \
    --ak bundle="$BUNDLE" --ak configuration="$1" --ak main_effort=max \
    --ak openai_key_file="$OPENAI_KEY" --ak anthropic_key_file="$ANTHROPIC_KEY" \
    "${@:4}"
}
# 同じ3課題を各1回。
unreal_run claude-gpt e2-mixed-small anthropic/opus --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run gpt e2-gpt-small openai/gpt-6-astra --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run claude e2-claude-small anthropic/opus --ak child_model=sonnet --ak child_effort=high

# 500課題すべてを各1回、同じ3構成で。
TASKS=()
unreal_run claude-gpt e2-mixed-500 anthropic/opus --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run gpt e2-gpt-500 openai/gpt-6-astra --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run claude e2-claude-500 anthropic/opus --ak child_model=sonnet --ak child_effort=high
```

モデル ID は実行者の指定例で、API の利用資格を確認したものではありません。
実装中に本物の API は呼びません。API が強度を拒否した場合は、モデル・指定した強度・
マスク済みの API の理由を返し、別の強度への置換や省略は行いません。

| `--ak` | 意味・既定値 |
|---|---|
| `configuration` | `claude-gpt`（既定）、`gpt`、`claude` |
| `main_model` | `-m` より優先。両方省略時は `anthropic/opus` または `openai/gpt-6.1-sol` |
| `main_effort` | 既定 `max`。low / medium / high / xhigh / max |
| `child_model`, `child_effort` | 混成・GPT：実装 lead の指定。既定 gpt-6.2-sol / max。worker は同じモデルと各 template の強度を使い、collector は製品どおり gpt-6-luna / medium |
| Claude 単独の `child_model`, `child_effort` | Claude API 接続を使う Unreal の子のモデルと effort。省略した項目はメインの設定を引き継ぐ |
| `openai_key_file`, `anthropic_key_file` | ローカルの通常ファイル・0600。選択した構成に必要なキーだけを読む |

どの構成でも Unreal の子 Session に委譲します。

## Terminal-Bench 4.0.0 の課題一覧・実行

`benchmarks/harbor/tasks/terminal-bench-4.0.0.txt` に Software と Operations の課題を
1行1件で24件記載しています。`legacy-utility-triage`（コンピューターユース）、
`medical-claims-processing`（MCP browser）、`live-database-cutover`（16 CPU）は含めません。
`distributed-dedup` は8 CPU・16 GBを使うため `-n 1` で単独実行し、残り23件は
`-n 2` でまとめて実行します。実行はこの2回です。

以下は上の Sandbox Bash セットアップと同じシェルで、**Sandbox Bash** に丸ごと貼れます。
`BUNDLE`、`OPENAI_KEY`、`ANTHROPIC_KEY` を再利用し、既定の `claude-gpt` 構成と
`main_effort=max` を指定します。課題一覧から複数の `-i` を生成します。

```bash
set -euo pipefail
DATASET=terminal-bench/terminal-bench@4.0.0
TASK_FILE=benchmarks/harbor/tasks/terminal-bench-4.0.0.txt
mapfile -t ALL_TASKS < "$TASK_FILE"
DIST_TASK=distributed-dedup
OTHER_TASKS=()
for task in "${ALL_TASKS[@]}"; do
  [[ "$task" == "$DIST_TASK" ]] || OTHER_TASKS+=("$task")
done
OTHER_TASK_ARGS=()
for task in "${OTHER_TASKS[@]}"; do OTHER_TASK_ARGS+=(-i "$task"); done
run_terminal_batch() {
  uv run --project benchmarks/harbor --locked harbor run \
    -d "$DATASET" -e docker -n 2 -k 1 \
    -a harness_harbor.agent:UnrealAgent -m anthropic/claude-opus-5-5 \
    --job-name terminal-bench-4-remaining-23 \
    --ak bundle="$BUNDLE" --ak configuration=claude-gpt --ak main_effort=max \
    --ak openai_key_file="$OPENAI_KEY" --ak anthropic_key_file="$ANTHROPIC_KEY" \
    "${OTHER_TASK_ARGS[@]}"
}
uv run --project benchmarks/harbor --locked harbor run \
  -d "$DATASET" -e docker -n 1 -k 1 \
  -a harness_harbor.agent:UnrealAgent -m anthropic/claude-opus-5-5 \
  --job-name terminal-bench-4-distributed-dedup \
  --ak bundle="$BUNDLE" --ak configuration=claude-gpt --ak main_effort=max \
  --ak openai_key_file="$OPENAI_KEY" --ak anthropic_key_file="$ANTHROPIC_KEY" \
  -i "$DIST_TASK"
run_terminal_batch
```

この手順は実行者が開始するまで API を呼びません。

## Harbor 組み込み Codex CLI・Claude Code との比較

`DATASET`、課題、並行数、試行数を揃えます。CLI の版も明示します
（以下の npm の版は 2026-11-13 に確認。本物のモデルでの実行は未確認）。
モデル ID と実際に受理された強度を揃えて比較し、各ジョブの設定を残してください。

上で定義した `hrun` を使う **Sandbox Bash** の1ブロックです。

```bash
TASKS=(-i django__django-15098 -i sphinx-doc__sphinx-8593 -i sympy__sympy-13852)
baselines() {
  (
    export OPENAI_API_KEY="$(cat "$OPENAI_KEY")"
    export ANTHROPIC_API_KEY="$(cat "$ANTHROPIC_KEY")"
    hrun -a codex -m openai/gpt-6-astra --ak version=0.168.0 --ak reasoning_effort=max --job-name "codex-$1"
    hrun -a claude-code -m anthropic/opus --ak version=2.1.322 --ak reasoning_effort=max --job-name "claude-code-$1"
  )
}
baselines small
TASKS=()
baselines 500
```

[Codex adapter](https://github.com/harbor-framework/harbor/blob/v0.22.0/src/harbor/agents/installed/codex.py) と
[Claude Code adapter](https://github.com/harbor-framework/harbor/blob/v0.22.0/src/harbor/agents/installed/claude_code.py) は
`reasoning_effort` をそれぞれ CLI 引数に渡します。
キーの値はシェル環境から受け継ぎ、Harbor の `--ae` 設定には入れません。

## 記録・費用・公開

`agent/trajectory.json` から Unreal の子・孫の
`agent/trajectory.<session-id>.json` を辿れます。
`agent/sessions/*.session.jsonl` はマスク済みの監査用コピーで、resume 用データではありません。
キーファイル・Provider の home・生の再送用データ・private なコマンド出力ファイルは収集しません。
過去の Claude Agent のメッセージには `[Claude Code Agent <tool-call-id>]` の紐づけがあります。

trajectory の合計と `AgentContext.metadata.usage_by_connection` に、
`claude-api` と `openai` を分け、モデル別にも入力・キャッシュ読取・キャッシュ書込・出力・推論を記録します。
入力には両キャッシュ、出力には推論を含みます。推論が不明なら未計測と記録し、測定済みの0にしません。
Claude Code は1回の実行全体の `claude_usage` を正とし、同じ turn の
`provider_usage` や応答ごとの usage は重ねて足しません。破棄された思考や
回復処理の出力も含め、`<synthetic>` のモデルは除外します。
再開した Session 全体の累計値を足し直しません。
親の合計には接続された GPT の子・孫を各1回だけ含めます。

費用は Harbor が導入する LiteLLM の価格表にモデルが完全一致するときだけ、キャッシュを含めて計算します。
請求額ではなく推定額です。価格がないモデルは費用を null にしてトークン数だけを残し、
1モデルでも計算できなければ全体の費用も null にします。`opus` などの alias に価格がない場合もあります。

終了済みの job を Session の記録だけから計算し直す場合は、次を **Sandbox Bash** に貼れます。
統括・子・孫の各 Session を1回ずつ数え、課題ごと・接続ごと・モデルごとのJSONを標準出力へ出します。
元の job や trajectory は変更せず、API・外部通信も使いません。

```bash
uv run --project benchmarks/harbor --locked python benchmarks/harbor/recalculate_costs.py \
  jobs/unreal-tb4-pilot2 > /tmp/unreal-tb4-pilot2-costs.json
# 当時保存したLiteLLM価格表がある場合は --prices /path/to/model_prices.json を追加します。
```

既定は固定版LiteLLMの `model_prices_and_context_window_backup.json` です。
使用した表のパスは出力の `price_table` に残ります。
価格表にないモデルを別モデルの単価で補完することはありません。

Harbor 0.22.0 の `harbor upload --help` で、**新規 upload は既定で非公開**と確認しました。
公開前に、baseline のログも含むジョブ全体の `config.json`、`result.json`、trajectory、課題の出力を確認します。
以下は **Sandbox Bash** の1ブロックです。scanner は一致した値を出さず、ファイルのパスだけを表示します。

```bash
uv run --project benchmarks/harbor --locked python -m harness_harbor.security \
  jobs --key-file "$OPENAI_KEY" --key-file "$ANTHROPIC_KEY"
uv run --project benchmarks/harbor --locked harbor view jobs/e2-mixed-small
# scanner の一致箇所と課題固有の private 情報を確認した後：
uv run --project benchmarks/harbor --locked harbor upload jobs/e2-mixed-small --private
# 公開するときは、確認したジョブに対して --private の代わりに --public を明示します。
```

scanner は渡されたキー・その符号化・一般的な認証情報の形を検出します。
課題固有の private 情報は人も確認します。自動 upload は行いません。

## Docker なしのテスト・別 PC の Docker smoke

```bash
make -C benchmarks/harbor test check
# Docker＋偽モデル・偽Claude CLI。本物のキー・モデルは使いません。
make -C benchmarks/harbor smoke BUNDLE="$BUNDLE"
# 新しい構成を個別に試す場合：
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration claude-gpt
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration gpt
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration claude
```

smoke の既定は従来 runner と新しい3構成すべてです。
試験用の subclass が Claude の導入だけを偽の stream-json peer に置き換えます。
本物の Node・Claude 導入、Docker Desktop・WSL の挙動、実際の API 利用資格、500課題の評価は
別 PC で確認する範囲です。Docker がない開発 PC では確認していません。

従来の OpenAI / OpenRouter / Fireworks と `thinking_level` を使う runner 経路は、
`-a harness_harbor.runner:RunnerAgent` と明示すれば利用できます。
既存の Terminal-Bench の実行手順ではこのクラスを指定してください。

### OpenCode Go を無効にして作る

`make -C benchmarks/harbor build REVISION=HEAD OPENCODE_GO_ENABLED=false BUNDLE=/absolute/new-bundle`、または `build.py` の `--disable-opencode-go` を指定します。Host と runner の両方に `disable_opencode_go` が入り、`manifest.json` に `opencode_go_enabled: false` を記録します。`--version` でも確認でき、キー・設定・保存済み選択・子の指定で戻せません。ほかの provider は変えません。[配布手順](../../distribution.md#opencode-go-を無効にした配布物)も参照してください。
