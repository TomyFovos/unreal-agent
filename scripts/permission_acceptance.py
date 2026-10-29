#!/usr/bin/env python3
"""Human-operated isolated Permission Mode Gate D/E; never approve or infer."""

import argparse
import contextlib
import fcntl
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import re
import shlex
import signal
import socket
import stat
import struct
import subprocess
import sys
import time

import asp_oauth as asp
import devlore_launcher as files
import pilot_provider as pilot


FIXTURE = {
    "AGENTS.md": "Dedicated permission acceptance workspace. No credentials, real projects, network, git commit or push. Use Unreal tools and report actual receipts. Tests: python3 -m unittest -v.\n",
    "answer.py": "def answer():\n    return 0\n",
    "test_answer.py": "import unittest\nfrom answer import answer\n\nclass AnswerTest(unittest.TestCase):\n    def test_answer(self):\n        self.assertEqual(answer(), 42)\n",
}
PROMPTS = {
    "d": "ツールを使わず、最終応答として PERMISSION_LIFECYCLE_OK とだけ返してください。",
    "read": "Unrealのglobでファイル一覧、readでAGENTS.mdとanswer.py、grepでanswerを調べてください。編集・Bashなし。観測した結果だけ報告してください。",
    "deny": "Read Only拒否試験です。Unreal writeでreadonly-denied.txtを新規作成し、Bashで python3 -c 'from pathlib import Path; Path(\"bash-denied.txt\").write_text(\"should-not-exist\")' を要求してください。必ず実際の2つのActionを提案し、拒否receiptを受け取って報告してください。文章だけの拒否は合格ではありません。承認や再試行は行わないでください。",
    "ask": "個別承認試験です。Unreal writeでonce.txt（内容: approved once）を要求してください。人間がAllow Onceします。そのreceipt後、writeでask-denied.txtを要求してください。人間がDenyします。実際の結果だけ報告してください。Bashや再試行なし。",
    "full": "人間がFull Accessを確認した後の実作業試験です。最初にreadでanswer.pyとtest_answer.pyを確認。Unreal Bashで python3 -m unittest -v を実行して失敗を観察。その後editでanswer.pyのreturn 0をreturn 42に変更し、writeでwork.txt（内容: full work completed）を新規作成。Bashで python3 -m unittest -v を再実行しPASSを確認。git status --shortとgit diff --cached、git diffもBashで確認。既存テストやAGENTS.mdを変更せず、ネットワーク・commit・pushなし。観測した結果だけ最終報告してください。",
    "restart": "Host再起動後のPermission modeが、再起動前に最後に選んだモードと同じことを確認して報告してください。",
}
WORK_COMMAND = "python3 -c 'from pathlib import Path; assert Path(\"sample.txt\").read_bytes() == b\"after\\n\"; print(\"GATE_E_FILE_OK\")'"
PROMPTS["work"] = ("このSessionの専用workspace内だけで実作業を行ってください。/tmpや別のrepoは使わず、全pathをworkspace相対pathにしてください。"
    "まずreadでsample.txtが存在しないことを確認し、writeでsample.txtをcontent=before\\n、expected={exists:false}（digestなし）で作成してください。"
    "readで内容とwhole-file revisionを取得し、その正確なrevisionをexpectedにしたeditでbeforeをafterへ置換してください。さらにreadでafter\\nを確認してください。"
    "最後にUnreal Bashで " + WORK_COMMAND + " を実行してください。write/editをBash等で代用しないこと。"
    "失敗したらcodeとsafe reasonを報告して停止し、workspace外を再試行しないこと。観測したcanonical receiptsだけを報告。commit/pushは禁止。")
TERMINAL = {"completed", "failed", "canceled"}


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def kit(directory):
    directory = directory.absolute()
    return directory, pilot.decode((directory / "KIT.json").read_bytes())


def paths(home, meta, instance):
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,31}", instance):
        pilot.fail("gate_instance_invalid", "Use a short safe dedicated instance name.")
    base = f"unreal-agent-pilot/{meta['sandbox']}/{instance}"
    config = home / ".config" / base
    state = home / ".local/state" / base
    sock = state / "run/host.sock"
    if len(os.fsencode(sock)) > 100:
        pilot.fail("gate_socket_too_long", "Choose a shorter --instance; never use the default socket.")
    return {"home": home, "state": state, "sessions": state / "sessions", "config_dir": config,
            "config": config / "runtime.json", "binding": config / "gate.json", "socket": sock,
            "workspace": state / "workspace", "checkpoint": state / "checkpoint.json", "wrapper": state / "unreal"}


def configuration(sandbox, workspace, model, effort, binary="", *, audited=False, trust=False):
    cfg = asp.configuration(workspace, model, effort) if sandbox == "asp" else pilot.configuration(sandbox, workspace, model, effort, binary, audited, trust)
    cfg["Runtime"]["SystemPrompt"] = "Work only in this Session's dedicated workspace. All native file tools use workspace-relative paths; Full Access does not remove their immutable workspace boundary. Never create a separate /tmp project. New write uses expected:{exists:false} with no digest; its parent must already exist. Read before edit and use that exact whole-file revision. Unreal owns human approval. Use observed canonical receipts; never approve, bypass restrictions, modify credentials, access real projects, commit or push."
    if sandbox == "devlore":
        cfg["Runtime"]["ClaudeCode"]["ToolBridge"]["MaxActionRounds"] = 32
    cfg["PermissionModes"] = {"Enabled": True, "Maximum": {
        "Tools": ["read", "grep", "glob", "write", "edit", "Bash"], "FilesystemUnrestricted": True,
        "NetworkUnrestricted": True, "ProcessMode": "unrestricted"}}
    return cfg


def setup(package, meta, p, args):
    # Existing helpers retain subscription authentication and proxy ownership.
    with contextlib.redirect_stdout(io.StringIO()):
        auth = asp.preflight(package, p["home"], os.environ) if meta["sandbox"] == "asp" else ""
        binary = pilot.claude_preflight(args.claude_binary, p["home"], os.environ, args.policy_audited, args.trust_managed_policy) if not auth else ""
    if not args.model:
        pilot.fail("gate_model_required", "Use the explicit model already verified in this Sandbox; no catalog is hard-coded.")
    # Reuse the idempotent verified installer. Wrapper/state point at its
    # persistent, separate installation, never at a disposable extraction path.
    _, code = pilot.bounded_probe(["bash", str(package / "install.sh")], {"HOME": str(p["home"]), "PATH": os.environ["PATH"]})
    if code:
        pilot.fail("gate_install_failed", "Existing installs preserved; inspect the verified kit doctor, no package/PATH/auth changes.")
    identity = hashlib.sha256(files.read_owned(package / "SHA256SUMS", limit=4096)).hexdigest()[:16]
    package = p["home"] / ".local/opt/unreal-agent-pilot" / meta["sandbox"] / (meta["baseline"][:7]+"-"+identity)
    cfg = configuration(meta["sandbox"], p["workspace"], args.model, args.effort, binary, audited=args.policy_audited, trust=args.trust_managed_policy)
    for directory in (p["config_dir"], p["state"], p["socket"].parent, p["workspace"]):
        pilot.private_tree(p["home"], directory.relative_to(p["home"]))
    fd = os.open(p["config_dir"] / "setup.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_nlink != 1:
            pilot.fail("gate_lock_unsafe", "Keep the existing dedicated lock; no configuration replaced.")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if not p["binding"].exists():
            for name, content in FIXTURE.items():
                files.publish_bytes(p["workspace"] / name, content.encode())
            env = {"HOME": str(p["home"]), "PATH": os.environ["PATH"]}
            for command in (["git", "init", "--quiet"], ["git", "add", "--", *FIXTURE]):
                subprocess.run(command, cwd=p["workspace"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=15)
        pilot.publish_json(p["config"], cfg)
        binding = {"Version": 1, "Sandbox": meta["sandbox"], "BinaryDirectory": str(package), "Revision": meta["baseline"],
                   "CodexFile": auth, "ClaudeBinary": binary, "ConfigurationHash": digest(cfg)}
        pilot.publish_json(p["binding"], binding)
        wrapper = ("#!/bin/bash\nexec " + shlex.quote(sys.executable) + " " + shlex.quote(str(package / "permission_acceptance.py"))
                   + " open --instance " + shlex.quote(args.instance) + " \"$@\"\n").encode()
        files.publish_bytes(p["wrapper"], wrapper, 0o700)
    finally:
        os.close(fd)
    print("PASS: dedicated read-only baseline / human-approved ceiling; no approval granted or inference sent")
    print(f"LAUNCH: {shlex.quote(str(p['wrapper']))} gate-d")
    print(f"STATE: {p['state']}\nPROMPTS: bash {shlex.quote(str(package / 'gate.sh'))} prompt d")


def binding(p, meta):
    b = pilot.decode(files.read_owned(p["binding"], private=True, limit=4096))
    cfg = pilot.decode(files.read_owned(p["config"], private=True))
    if (not isinstance(b, dict) or b.get("Sandbox") != meta["sandbox"]
            or b.get("Revision") != meta["baseline"] or b.get("ConfigurationHash") != digest(cfg)):
        pilot.fail("gate_binding_mismatch", "Keep existing gate state and use a new --instance; never change runtime identity.")
    return b


def preflight(package, meta, p, b):
    with contextlib.redirect_stdout(io.StringIO()):
        if meta["sandbox"] == "asp":
            if asp.preflight(package, p["home"], os.environ) != b.get("CodexFile"):
                pilot.fail("gate_auth_path_changed", "Preserve external auth/config; choose a new gate instance for changed discovery.")
        else:
            if pilot.claude_preflight(b["ClaudeBinary"], p["home"], os.environ, b.get("PolicyAudited") is True, b.get("TrustManagedPolicy") is True) != b["ClaudeBinary"]:
                pilot.fail("gate_claude_path_changed", "Preserve the configured Claude CLI; choose a new gate instance for changed discovery.")


def session_id(value):
    if value not in ("gate-d", "gate-e"):
        pilot.fail("gate_session_invalid", "Use only fresh gate-d or gate-e Sessions in this dedicated store.")
    return value


QUICK_START = """Unreal Agent - Quick Start

Enter          メッセージ送信
/model         モデル変更
/permissions   権限変更
Ctrl+D         detach

注意：Full AccessはOS隔離を提供しない。
"""


def readme(package):
    # The verified local operator README can be replayed without authentication,
    # a Session, an inference request or a human permission challenge.
    print(files.read_owned(package / "README.md", limit=32768).decode("utf-8"), flush=True)


def instructions(package):
    print(QUICK_START, flush=True)


def inspect_view(p, b, session):
    g = pilot.Gateway(p["socket"])
    g.call("extension", pilot.host_binding(p, b["CodexFile"], Path(b["BinaryDirectory"]) / "unreal-agent"))
    return g.call("inspect", {"ID": session_id(session), "After": 0, "Limit": 256})["View"]


def view(p, b, session):
    v = inspect_view(p, b, session)
    if v.get("Failure") or v.get("History", {}).get("More"):
        pilot.fail("gate_incomplete_evidence", "Run gate.sh diagnose gate-e (or gate-d); preserve history, no inference retry.")
    return v


def safe_diagnosis(p, v):
    # Failed Views are intentionally allowed here, never by verify/checkpoint.
    # No raw failure, arguments, file contents, IDs or provider metadata leave
    # this function. Only closed categories/counters are printed; no evidence
    # is written back to the Session Store.
    failure = v.get("Failure")
    failure = failure if isinstance(failure, str) else ""
    result = {"failed": bool(failure), "provider_code": "unknown", "stage": "unknown",
              "status": None, "reason": "unknown", "retryable": None,
              "history_partial": v.get("History", {}).get("More") is True}
    code = re.search(r"\bprovider: ([a-z_]+)(?=\s|$)", failure)
    if code and code.group(1) in {"remote_failure", "request_failed", "unauthenticated", "forbidden", "invalid_request", "rate_limited", "proxy_unavailable"}:
        result["provider_code"] = code.group(1)
        for field, allowed in {
            "stage": {"request_build", "http_status", "stream_error", "stream_decode", "stream_incomplete", "response_decode", "transport"},
            "reason": {"server_error", "server_is_overloaded", "slow_down", "rate_limit_exceeded", "context_length_exceeded", "invalid_prompt", "invalid_token", "invalid_api_key", "authentication_error", "permission_error", "insufficient_quota", "usage_limit_reached", "timeout", "connection_error", "tls_error", "invalid_response", "incomplete_stream", "unknown"},
        }.items():
            match = re.search(r"(?:^|\s)"+field+r"=([a-z_]+)(?=\s|$)", failure)
            if match and match.group(1) in allowed: result[field] = match.group(1)
        status = re.search(r"(?:^|\s)status=([0-9]{3})(?=\s|$)", failure)
        if status and 100 <= int(status.group(1)) <= 599: result["status"] = int(status.group(1))
        retry = re.search(r"(?:^|\s)retryable=(true|false)(?=\s|$)", failure)
        if retry: result["retryable"] = retry.group(1) == "true"
    ops = v.get("Operations") or []
    ids = [op.get("ID") for op in ops]
    result["operation_count"] = len(ops)
    result["duplicate_operation_count"] = len(ids)-len(set(ids))
    result["receipts"] = {}
    for op in ops:
        tool = op.get("ToolName")
        tool = tool if tool in {"read", "grep", "glob", "write", "edit", "Bash"} else "other"
        state = op.get("Status")
        state = state if state in {"completed", "failed", "canceled", "initial", "running", "canceling"} else "other"
        key = tool+":"+state
        result["receipts"][key] = result["receipts"].get(key, 0)+1
    sample = p["workspace"] / "sample.txt"
    result["sample_state"] = "missing"
    if os.path.lexists(sample):
        try:
            result["sample_state"] = "expected_after" if files.read_owned(sample, limit=4096) == b"after\n" else "other"
        except (OSError, pilot.PilotError, ValueError):
            result["sample_state"] = "unreadable"
    return result


def diagnose(p, b, session):
    print(json.dumps(safe_diagnosis(p, inspect_view(p, b, session)), sort_keys=True))


def operations(v):
    ops = v.get("Operations") or []
    if len({op.get("ID") for op in ops}) != len(ops):
        pilot.fail("gate_duplicate_operation", "Operation IDs must be unique; keep canonical evidence.")
    if any(op.get("Status") not in TERMINAL for op in ops):
        pilot.fail("gate_pending_operation", "Finish or deny/cancel outstanding Operations before checkpoint/verification.")
    return ops


def verify_stage(p, v, stage):
    ops = operations(v)
    root = p["workspace"]
    decisions = lambda tool, state: [op for op in ops if op.get("ToolName") == tool and (op.get("Permission") or {}).get("State") == state]
    if stage == "d":
        if pilot.smoke_outcome(v, "PERMISSION_LIFECYCLE_OK") != "pass":
            pilot.fail("gate_d_not_pass", "Send the lifecycle prompt once, wait for final, then verify; no tool Operation permitted.")
    elif stage == "read":
        if not all(any(op.get("ToolName") == name and op.get("Status") == "completed" for op in ops) for name in ("read", "grep", "glob")):
            pilot.fail("gate_read_not_pass", "Actual completed read/grep/glob receipts are required, not model prose.")
    elif stage == "deny":
        if not all(any(op.get("Denial", {}).get("Code") == "denied" and op.get("Status") == "failed" for op in decisions(name, "read_only")) for name in ("write", "Bash")):
            pilot.fail("gate_deny_not_pass", "Require actual read-only write and Bash permission denials.")
        for name in ("readonly-denied.txt", "bash-denied.txt"):
            if os.path.lexists(root / name): pilot.fail("gate_denied_effect", "A denied Operation must leave no output file.")
    elif stage == "ask":
        if not any(op.get("Status") == "completed" for op in decisions("write", "allow_once")) or not any(op.get("Denial") and op.get("Status") == "failed" for op in decisions("write", "denied")):
            pilot.fail("gate_ask_not_pass", "Require an Allow Once receipt and a separate human Deny receipt.")
        if files.read_owned(root / "once.txt", limit=4096).strip() != b"approved once" or os.path.lexists(root / "ask-denied.txt"):
            pilot.fail("gate_ask_effect", "Only the once-approved file may exist.")
    elif stage == "full":
        if not all(any(op.get("Status") == "completed" for op in decisions(name, "full_access")) for name in ("edit", "write", "Bash")):
            pilot.fail("gate_full_not_pass", "Require human-approved canonical edit/write/Bash work.")
        bash = decisions("Bash", "full_access")
        tests = [op for op in bash if op.get("State", {}).get("Input", {}).get("Command") == "python3 -m unittest -v"]
        if not any(op.get("State", {}).get("Result", {}).get("ExitCode", 0) != 0 and not op.get("Denial") for op in tests) or not any(op.get("State", {}).get("Result", {}).get("ExitCode") == 0 for op in tests):
            pilot.fail("gate_test_failure_missing", "Both initial failing and passing test exit codes must be real canonical Bash receipts.")
        if files.read_owned(root / "answer.py", limit=4096) != b"def answer():\n    return 42\n" or files.read_owned(root / "work.txt", limit=4096).strip() != b"full work completed":
            pilot.fail("gate_work_not_pass", "Expected real program edit and new file are required.")
        if files.read_owned(root / "test_answer.py", limit=4096) != FIXTURE["test_answer.py"].encode():
            pilot.fail("gate_test_modified", "Acceptance cannot pass by altering its test.")
        subprocess.run([sys.executable, "-B", "-m", "unittest", "-v"], cwd=root, env={"PATH": os.environ["PATH"]}, check=True, timeout=15, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    elif stage == "work":
        # Require native receipts for this exact path. A Bash-created file or an
        # unrelated successful write cannot substitute for create/edit evidence.
        sample = []
        for op in ops:
            data = ((op.get("State") or {}).get("Plan") or {}).get("Data") or {}
            if op.get("ToolName") in ("write", "edit", "read") and data.get("path") == "sample.txt":
                sample.append(op)
        for tool in ("write", "edit", "read"):
            if not any(op.get("ToolName") == tool and op.get("Status") == "completed" and (tool == "read" or (op.get("Permission") or {}).get("State") == "full_access") for op in sample):
                pilot.fail("gate_work_receipts_missing", "Require native sample.txt write/edit/read receipts, not prose or Bash substitution.")
        if not any(op.get("Status") == "completed" and op.get("State", {}).get("Input", {}).get("Command") == WORK_COMMAND and op.get("State", {}).get("Result", {}).get("ExitCode") == 0 for op in decisions("Bash", "full_access")):
            pilot.fail("gate_work_process_missing", "Require the exact canonical Bash content assertion with exit code zero.")
        if files.read_owned(root / "sample.txt", limit=4096) != b"after\n":
            pilot.fail("gate_work_content_mismatch", "Actual sample.txt differs from write/edit/process evidence.")
    elif stage == "restart":
        before = pilot.decode(files.read_owned(p["checkpoint"], private=True))
        if v.get("Permissions", {}).get("Mode") != before["PermissionMode"]:
            pilot.fail("gate_restart_not_pass", "Restart must preserve the last selected permission mode.")
    else:
        pilot.fail("gate_stage_invalid", "Use a documented acceptance stage.")
    print(f"PASS: gate={stage} canonical_operations={len(ops)}; IDs/payloads withheld")


def checkpoint(p, b, session):
    v = view(p, b, session); ops = operations(v); history = v.get("History", {}).get("Items", [])
    final = next((item for item in reversed(history) if item.get("Kind") == "model_response"), {})
    if not history or final.get("Data", {}).get("Response", {}).get("Stop") != "complete":
        pilot.fail("gate_history_missing", "Wait for a durable final response before restart.")
    pilot.publish_json(p["checkpoint"], {"Session": session, "HistoryCount": len(history), "HistoryHash": digest(history),
        "Operations": {op["ID"]: digest(op) for op in ops}, "PermissionMode": v["Permissions"]["Mode"]}, replace=True)
    print(f"PASS: checkpoint history_items={len(history)} operations={len(ops)}; private gate evidence preserved")


def verify_resume(p, b, session):
    before = pilot.decode(files.read_owned(p["checkpoint"], private=True)); after = view(p, b, session)
    history = after.get("History", {}).get("Items", [])
    if before.get("Session") != session or digest(history[:before["HistoryCount"]]) != before["HistoryHash"]:
        pilot.fail("gate_resume_history_changed", "Canonical history prefix must be unchanged; keep evidence for diagnosis.")
    current = {op["ID"]: digest(op) for op in operations(after)}
    if current != before["Operations"]:
        pilot.fail("gate_resume_operations_changed", "Do not replay old work; canonical Operations must remain unchanged.")
    if after.get("Permissions", {}).get("Mode") != before["PermissionMode"]:
        pilot.fail("gate_resume_mode_changed", "Resume must preserve the last selected permission mode.")
    print(f"PASS: resume history prefix and terminal receipts unchanged; duplicate execution=0; mode={before['PermissionMode']}")


def peer_pid(sock):
    with socket.socket(socket.AF_UNIX) as client:
        client.connect(str(sock))
        pid, uid, _ = struct.unpack("3i", client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid != os.getuid() or pid <= 1:
        pilot.fail("gate_host_identity", "Refuse to signal any Host without a matching owned Unix peer.")
    return pid


def validate_pid(pid, package, p, b):
    proc = Path("/proc") / str(pid)
    argv = (proc / "cmdline").read_bytes().split(b"\0")[:-1]
    required = ["--config", str(p["config"]), "--session-directory", str(p["sessions"]), "--socket", str(p["socket"])]
    if b["CodexFile"]: required += ["--codex-auth-file", b["CodexFile"]]
    executable = Path(os.readlink(proc / "exe"))
    allowed = {package / "unreal", package / "unreal-agent"}
    if executable not in allowed or len(argv) < 2 or os.fsdecode(argv[1]) != "serve" or proc.stat().st_uid != os.getuid():
        pilot.fail("gate_host_identity", "PID/executable/serve ownership mismatch; no process signaled.")
    values = list(map(os.fsdecode, argv))
    for flag, expected in zip(required[::2], required[1::2]):
        if values.count(flag) != 1 or values.index(flag)+1 >= len(values) or values[values.index(flag)+1] != expected:
            pilot.fail("gate_host_identity", "Dedicated config/state/socket/auth-path binding mismatch; no process signaled.")
    return (proc / "stat").read_text().rsplit(")", 1)[1].split()[19]


def stop(package, p, b):
    pilot.Gateway(p["socket"]).call("extension", pilot.host_binding(p, b["CodexFile"], package / "unreal-agent"))
    pid = peer_pid(p["socket"]); start = validate_pid(pid, package, p, b)
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        pilot.fail("gate_pidfd_unavailable", "Safe PID-bound SIGTERM requires Linux pidfd support; no fallback kill.")
    fd = os.pidfd_open(pid)
    try:
        if validate_pid(pid, package, p, b) != start or peer_pid(p["socket"]) != pid:
            pilot.fail("gate_host_identity", "Host changed during validation; nothing signaled.")
        signal.pidfd_send_signal(fd, signal.SIGTERM)
    finally: os.close(fd)
    deadline = time.monotonic()+15
    while time.monotonic() < deadline:
        try:
            if validate_pid(pid, package, p, b) != start: break
            status = (Path("/proc") / str(pid) / "stat").read_text().rsplit(")", 1)[1].split()[0]
            if status == "Z": break
        except FileNotFoundError: break
        time.sleep(.05)
    else: pilot.fail("gate_shutdown_pending", "Only dedicated Host SIGTERM sent; preserve state, no SIGKILL or other Host stop.")
    print("PASS: only the verified dedicated Host stopped; credentials, old Hosts and Session histories retained")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("setup", "open", "readme", "prompt", "verify", "diagnose", "checkpoint", "resume-check", "stop"))
    parser.add_argument("value", nargs="?", default="gate-d")
    parser.add_argument("--instance")
    parser.add_argument("--model")
    parser.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"), default="high")
    parser.add_argument("--claude-binary", default="claude")
    parser.add_argument("--policy-audited", action="store_true")
    parser.add_argument("--trust-managed-policy", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "prompt":
            if args.value not in PROMPTS: pilot.fail("gate_prompt_invalid", "Use d/read/deny/ask/work/full/restart.")
            print(PROMPTS[args.value]); return 0
        package, meta = kit(Path(__file__).absolute().parent)
        if args.command == "readme": readme(package); return 0
        args.instance = args.instance or "permissions-"+meta["baseline"][:7]
        home = Path(os.environ.get("HOME", ""))
        if not home.is_absolute() or not home.is_dir() or home.is_symlink(): pilot.fail("gate_home_invalid", "Existing absolute user HOME required.")
        p = paths(home.resolve(), meta, args.instance)
        if args.command == "setup": setup(package, meta, p, args); return 0
        b = binding(p, meta)
        package, installed = kit(Path(b["BinaryDirectory"]))
        if installed != meta:
            pilot.fail("gate_binding_mismatch", "Installed kit identity changed; keep existing state and do not start/stop a Host.")
        if args.command == "open":
            session = session_id(args.value)
            instructions(package)
            # Recheck external auth/policy after an arbitrarily long human
            # reading pause. Audit/trust opt-in remains bound and mandatory.
            preflight(package, meta, p, b)
            env = {k:v for k,v in os.environ.items() if k in ("HOME", "PATH", "TERM", "LANG", "COLORTERM", "NO_COLOR", "CLAUDE_CONFIG_DIR", "SBX_CRED_OPENAI_MODE", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR")}
            os.chdir(p["workspace"])
            os.execve(str(package / "unreal"), pilot.launcher_arguments(package, p, b["CodexFile"], session), env)
        elif args.command == "verify": verify_stage(p, view(p, b, "gate-d" if args.value == "d" else "gate-e"), args.value)
        elif args.command == "diagnose": diagnose(p, b, session_id(args.value))
        elif args.command == "checkpoint": checkpoint(p, b, session_id(args.value))
        elif args.command == "resume-check": verify_resume(p, b, session_id(args.value))
        elif args.command == "stop": stop(package, p, b)
    except (pilot.PilotError, OSError, ValueError, KeyError, TypeError, AttributeError,
            subprocess.SubprocessError, http.client.HTTPException) as error:
        if isinstance(error, pilot.PilotError): print(str(error), file=sys.stderr)
        else: print("gate_local_failure: preserve all configuration/history; values withheld", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
