# Harbor evaluation

**Language:** English | [日本語](../../docs/ja/benchmarks/harbor/README.md)

Harbor **0.22.0** runs the product Host headlessly (`unreal-agent exec`).
The normal runtime, permission handling, project instructions, Claude delegation
prompt, `unreal-agent subagent` command, worker templates and completion notices
are reused. The attachment supplies one task, selects Full Access inside Harbor's
container, and waits for the Host's normal one-shot completion. It adds no task,
token, cost or child-count limit and never switches authentication providers.

## Build on the development PC, then copy

Go and a C compiler are needed only on the development PC. Build **committed**
source; the static Linux binaries retain the native AST parser. The bundle
contains the Host, legacy runner, pinned Claude runtime installer, commit and
SHA-256 manifest. Existing bundle directories are not overwritten.

```bash
make -C benchmarks/harbor build REVISION=HEAD
E2_REV=$(git rev-parse --short HEAD)
tar --exclude=.venv --exclude=__pycache__ --exclude=.ruff_cache \
  -czf "/tmp/unreal-harbor-e2-$E2_REV.tgz" \
  benchmarks/harbor docs/ja/benchmarks/harbor \
  harness/llm/clients/claudeagent/install.py "bin/harbor/$E2_REV"
```

Copy this archive to the other PC. It contains the adapter and lockfile as well
as the binaries; **do not build a bundle on the evaluation PC**.

## Other PC: Windows, Docker Desktop, WSL2 Ubuntu

If WSL is not installed, run this complete block in **Windows PowerShell** as
administrator, then restart when Windows requests it:

```powershell
wsl --install -d Ubuntu-24.04
wsl --update
wsl --set-default-version 2
```

Install/start Docker Desktop, select the WSL 2 engine and Linux containers, then
turn on **Settings → Resources → WSL Integration → Ubuntu-24.04 → Apply**.
See [Docker's WSL instructions](https://docs.docker.com/desktop/features/wsl/).

Run this complete block in **Sandbox Bash (the evaluation PC's WSL2 Ubuntu)**.
Use its Linux home filesystem for the job directory and key files.

```bash
set -euo pipefail
sudo apt-get update
sudo apt-get install -y curl ca-certificates python3
curl -LsSf https://astral.sh/uv/0.13.8/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
read -r -e -p 'Path of the copied .tgz archive: ' E2_ARCHIVE
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

`uv sync --locked` installs Harbor 0.22.0 from the supplied lockfile. The Host and
installer snapshots are verified before upload. In Claude configurations the
container installs **Node 22.16.0** and **Claude Code 2.1.322**, verifying Node's
[official SHA-256](https://nodejs.org/dist/v22.16.0/SHASUMS256.txt) and the
[npm platform package's SHA-512 integrity](https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/2.1.322)
before extraction. It runs no npm lifecycle scripts. The installer and all
Claude Code wire/tool behavior live under `harness/llm/clients/claudeagent`.

The adapter uploads only the needed keys to a private container directory,
sets files to **0600**, and registers/selects E1's explicit API connections.
`--ak openai_key_file=...` and `--ak anthropic_key_file=...` contain paths only.
`--ae OPENAI_API_KEY=...` / `ANTHROPIC_API_KEY=...` are accepted, but Harbor stores
`--ae` values in job configuration: use key-file arguments for publishable runs.
The legacy runner's `UNREAL_HARNESS_LLM_API_KEY` behavior is unchanged.

## Fixed SWE-bench Verified tasks and three configurations

On 2026-11-13, Harbor 0.22.0's `harbor dataset list --legacy` and its
[registry](https://raw.githubusercontent.com/laude-institute/harbor/main/registry.json)
confirmed **`swebench-verified@1.0`, 500 tasks**. All task records point to
[`86723674f04e4209ac479d0fb75d9d9f44b4377e`](https://github.com/laude-institute/harbor-datasets/tree/86723674f04e4209ac479d0fb75d9d9f44b4377e/datasets/swebench-verified).
The [Hub name](https://hub.harborframework.com/datasets/swe-bench/swe-bench-verified)
`swe-bench/swe-bench-verified` is also accepted by 0.22.0 through a separate
package resolver; its omitted version means `latest`. A fixed Hub package
version was not verified. The commands below use the verified legacy version.

`-n` is **concurrent trials**, `-k` is **attempts per task**, `-l` limits task
count, and repeated `-i` selects task names/globs. These three exact task names
come from the same registry snapshot, so every agent gets the same small set.

Paste this complete **Sandbox Bash** block after setup:

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
# Three identical tasks, each attempted once.
unreal_run claude-gpt e2-mixed-small anthropic/opus --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run gpt e2-gpt-small openai/gpt-6-astra --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run claude e2-claude-small anthropic/opus --ak child_model=sonnet --ak child_effort=high

# All 500 tasks, each attempted once, with the same three configurations.
TASKS=()
unreal_run claude-gpt e2-mixed-500 anthropic/opus --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run gpt e2-gpt-500 openai/gpt-6-astra --ak child_model=gpt-6-astra --ak child_effort=max
unreal_run claude e2-claude-500 anthropic/opus --ak child_model=sonnet --ak child_effort=high
```

The model IDs are operator choices, not confirmation of API access. No real
API requests were made during implementation. If the API rejects an effort,
the selected model, effort and sanitized API reason are reported without
substituting or omitting it.

| `--ak` | Meaning / default |
|---|---|
| `configuration` | `claude-gpt` (default), `gpt`, `claude` |
| `main_model` | Overrides `-m`; otherwise `anthropic/opus` or `openai/gpt-6.1-sol` |
| `main_effort` | `max`; accepts low, medium, high, xhigh, max |
| `child_model`, `child_effort` | Mixed/GPT: implementation lead, default gpt-6.1-sol / max; workers use that model with their template efforts; collector keeps product default gpt-6-luna / medium |
| `child_model`, `child_effort` in Claude-only | Optional native Agent model/effort, applied through Claude Code's hook; if omitted, the unchanged product prompt chooses haiku/sonnet |
| `openai_key_file`, `anthropic_key_file` | Local regular 0600 key files; only the chosen configuration's keys are read |

GPT-only retains the product's GPT child mechanism. Claude-only uses the normal
Claude Code Agent tool; native child messages are linked to its Agent call.

## Built-in Codex CLI and Claude Code baselines

Use the same `DATASET`, task selection, concurrency and attempts. These are
Harbor's built-in agents, with explicit CLI versions (npm versions verified on
2026-11-13); neither was run against a real model here. Match model IDs and
reported effort when comparing, and retain each job's configuration.

Complete **Sandbox Bash** block, using `hrun` from above:

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

The [Codex adapter](https://github.com/harbor-framework/harbor/blob/v0.22.0/src/harbor/agents/installed/codex.py)
and [Claude Code adapter](https://github.com/harbor-framework/harbor/blob/v0.22.0/src/harbor/agents/installed/claude_code.py)
map `reasoning_effort` to their CLI flags. Key values are inherited from the
shell, rather than included in Harbor's `--ae` configuration.

## Records, costs and publication

`agent/trajectory.json` references GPT child/grandchild files
`agent/trajectory.<session-id>.json`. `agent/sessions/*.session.jsonl` are
sanitized audit copies, not executable resume data. Private keys, provider home,
raw provider replay and private operation captures are not collected. Native
Claude Agent messages carry `[Claude Code Agent <tool-call-id>]` links.

Trajectory totals and `AgentContext.metadata.usage_by_connection` separate
`claude-api` and `openai`, including per-model input, cache reads, cache writes,
output and reasoning. Input includes both cache subsets; output includes
reasoning. Unknown reasoning is marked as unknown, not measured zero. Claude's
assistant-message IDs deduplicate fragmented messages and avoid adding resumed
session cumulative totals. Root totals include each connected GPT child once.

Costs use Harbor's installed LiteLLM exact model cost-map entries, including
cache pricing. They are estimates, not invoices. Missing models have null cost;
if any participating model cannot be priced, the overall cost is also null.
Keep tokens for those models. Aliases such as `opus` may not have a price entry.

Harbor 0.22.0 `harbor upload --help` confirms **new uploads are private by
default**. Before uploading, inspect the whole job, including baseline logs,
`config.json`, `result.json`, trajectories and task output. Run this complete
**Sandbox Bash** block (the scanner prints paths, never matching values):

```bash
uv run --project benchmarks/harbor --locked python -m harness_harbor.security \
  jobs --key-file "$OPENAI_KEY" --key-file "$ANTHROPIC_KEY"
uv run --project benchmarks/harbor --locked harbor view jobs/e2-mixed-small
# After reviewing any scanner matches and task-specific private data:
uv run --project benchmarks/harbor --locked harbor upload jobs/e2-mixed-small --private
# To publish the reviewed job, explicitly choose --public instead of --private.
```

The scanner covers known keys, encodings and common credential patterns; manual
review also covers repository-specific private data. No automatic upload is
performed.

## Offline tests and Docker smoke on the other PC

```bash
make -C benchmarks/harbor test check
# Docker + deterministic fake model/Claude CLI, with no real keys or model calls:
make -C benchmarks/harbor smoke BUNDLE="$BUNDLE"
# Individual product configurations:
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration claude-gpt
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration gpt
uv run --project benchmarks/harbor --locked python benchmarks/harbor/tests/smoke.py --bundle "$BUNDLE" --configuration claude
```

The default smoke runs the original runner plus all three product configurations.
Its test subclass replaces only Claude installation with a deterministic
stream-json peer. Real Node/Claude installation, Docker Desktop/WSL behavior,
actual API entitlement and the 500-task evaluation require the other PC; they
were not tested on the development PC without Docker.

For the former runner integration (OpenAI/OpenRouter/Fireworks and its original
`thinking_level` argument), explicitly use `-a harness_harbor.runner:RunnerAgent`.
It remains available for existing Terminal-Bench workflows.

### Build without OpenCode Go

Use `make -C benchmarks/harbor build REVISION=HEAD OPENCODE_GO_ENABLED=false BUNDLE=/absolute/new-bundle` or add `--disable-opencode-go` to `build.py`. Both the Host and runner receive `disable_opencode_go`; `manifest.json` records `opencode_go_enabled: false`, and `--version` reports the compiled choice. Keys, configuration, saved selections and child requests cannot restore this connection. Other providers are unchanged. See [distribution builds](../../docs/distribution.md#distribution-builds-without-opencode-go).
