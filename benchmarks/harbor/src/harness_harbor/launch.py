"""In-container headless attachment and sanitized artifact collection (stdlib)."""

import json
import os
import signal
import subprocess
import sys
from pathlib import Path

from security import Redactor, read_key
from session_files import session_paths


def main(request):
    state, logs = Path(request["state"]), Path(request["logs"])
    secrets = [read_key(p) for p in request["keys"].values()]
    redactor = Redactor(secrets)
    env = dict(os.environ)
    # The Host resolves only the explicit files. Do not make either API key
    # ambient to the Host, its Bash tools, or its GPT children.
    for key in list(env):
        if key.endswith(("API_KEY", "AUTH_TOKEN", "ACCESS_TOKEN", "REFRESH_TOKEN")):
            env.pop(key)
    env.update(request["environment"])
    home = state / "home"
    home.mkdir(mode=0o700, parents=True, exist_ok=True)
    env["HOME"] = str(home)
    env["PATH"] = request["bin"] + ":" + env.get("PATH", "/usr/bin:/bin")
    command = [
        request["binary"],
        "exec",
        "--config",
        str(state / "config.json"),
        "--session-directory",
        str(state / "sessions"),
        "--socket",
        str(state / "host.sock"),
        "--session",
        request["session"],
        "--prompt-file",
        str(state / "prompt"),
        "--permission-mode",
        "full_access",
    ]
    child = None

    def terminate(signum, _frame):
        if child is not None:
            child.send_signal(signum)

    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, terminate)
    code = 1
    try:
        with (
            (state / "host.stdout").open("wb") as stdout,
            (state / "host.stderr").open("wb") as stderr,
        ):
            child = subprocess.Popen(command, env=env, stdout=stdout, stderr=stderr)
            code = child.wait()
    except OSError:
        (state / "host.stderr").write_text("Could not start the headless Host\n")
    finally:
        logs.mkdir(parents=True, exist_ok=True)
        (logs / "sessions").mkdir(exist_ok=True)
        for source in session_paths(state / "sessions").values():
            # Export copies, not executable Session/replay state or provider auth.
            (logs / "sessions" / source.name).write_text(
                redactor.jsonl(source.read_text())
            )
        for name in ("host.stdout", "host.stderr"):
            source = state / name
            if source.exists():
                (logs / name).write_text(
                    redactor.text(source.read_text(errors="replace"))
                )
    return code


if __name__ == "__main__":
    try:
        raise SystemExit(main(json.loads(Path(sys.argv[1]).read_text())))
    except (ValueError, OSError, KeyError):
        print("Benchmark attachment or artifact collection failed", file=sys.stderr)
        raise SystemExit(1) from None
