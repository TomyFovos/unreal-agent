#!/usr/bin/env python3
"""Operator-only Sandbox pilot: existing binaries, isolated Host, one smoke turn."""

import argparse
import fcntl
import http.client
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import uuid


BASELINE = "6284e49e70fd603902133bc194ae1ca81c777edd"
PROVIDERS = {"asp": "openai-codex", "devlore": "claude-code"}
SETTINGS = '{"disableAllHooks":true,"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"syncClaudeAiSkills":false,"syncClaudeAiPlugins":false}'
ISOLATION = ["--safe-mode", "--restricted", "--setting-sources", "", "--settings", SETTINGS,
             "--tools", "", "--disallowedTools", "*", "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
             "--disable-slash-commands", "--no-chrome"]


class PilotError(Exception):
    def __init__(self, code, action):
        self.code, self.action = code, action
        super().__init__(f"{code}: {action}")


def fail(code, action):
    raise PilotError(code, action)


def decode(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate member")
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=unique, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("non-JSON number")))


def private_directory(path):
    """Never follow redirects or chmod an existing parent."""
    path = path.absolute()
    for part in (*reversed(path.parents), path):
        if part.is_symlink():
            fail("private_path_redirect", "Use a dedicated non-symlink Pilot directory; nothing overwritten.")
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    # mkdir(parents=True) uses default permissions for parents; make each new
    # directory explicitly private instead (callers create ancestors in order).
    info = path.stat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
        fail("private_path_permissions", "Pilot directories must be user-owned mode 0700; existing modes were not changed.")
    return path


def private_tree(home, relative):
    path = home
    for component in Path(relative).parts:
        path = path / component
        if path.is_symlink():
            fail("private_path_redirect", "Remove the Pilot path redirect only after operator review.")
        if not path.exists():
            path.mkdir(mode=0o700)
        info = path.stat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
            fail("private_path_permissions", "Pilot parent must be user-owned and not group/world writable.")
    return private_directory(path)


def publish_json(path, data, replace=False):
    encoded = (json.dumps(data, ensure_ascii=True, indent=2) + "\n").encode()
    if path.is_symlink():
        fail("configuration_redirect", "Existing Pilot file is a symlink; nothing overwritten.")
    if path.exists():
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 1 << 20:
            fail("configuration_permissions", "Existing Pilot file is not a private regular file.")
        if not replace:
            if decode(path.read_bytes()) != data:
                fail("configuration_mismatch", "Keep the existing config/history; choose a new --instance for changed runtime settings.")
            return
    fd, temporary = tempfile.mkstemp(prefix=".pilot-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        if replace:
            os.replace(temporary, path)
        else:
            try:
                os.link(temporary, path)
            except FileExistsError:
                publish_json(path, data)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(temporary).unlink(missing_ok=True)


def configuration(sandbox, workspace, model, effort, claude_binary="", audited=False, trust=False):
    if sandbox not in PROVIDERS or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", model):
        fail("invalid_runtime_selection", "Specify a public, explicit provider model ID.")
    if effort not in ("low", "medium", "high", "xhigh", "max"):
        fail("invalid_effort", "Use an existing supported effort enum.")
    if sandbox == "devlore" and not (audited and trust):
        fail("managed_policy_audit_required", "Review devlore's local/remote managed policy and hooks; then explicitly opt in with --policy-audited --trust-managed-policy.")
    if sandbox == "asp" and (audited or trust):
        fail("unexpected_managed_trust", "asp uses only External Codex; remove Claude policy options.")
    provider = PROVIDERS[sandbox]
    runtime = {
        "Version": 1, "Workspace": str(workspace),
        "SystemPrompt": "Read-only Pilot. Use only permitted Unreal read/grep/glob. Do not write, edit, run commands, start children or access the network with tools.",
        "ReasoningEffort": effort,
        "Profile": {"id": "conservative", "version": 1, "source": "configured default"},
        "Provider": {"version": 1, "provider": provider, "model": {"id": model, "family": "claude" if sandbox == "devlore" else "openai-reasoning",
                     "capabilities": ["reasoning"] if sandbox == "devlore" else ["tools", "reasoning"]},
                     "auth": {"provider": provider, "method": "oauth", "id": "external-claude-code" if sandbox == "devlore" else "external-codex"},
                     "max_attempts": 1, "source": "explicit Sandbox pilot operator selection"},
    }
    permissions = {"Tools": ["read", "grep", "glob"], "ReadRoots": [str(workspace)],
                   "WriteRoots": [], "FilesystemUnrestricted": False, "NetworkUnrestricted": False, "ProcessMode": ""}
    if sandbox == "asp":
        runtime["Provider"]["endpoint"] = "https://chatgpt.com/backend-api/codex"
        permissions["NetworkOrigins"] = ["https://chatgpt.com"]
    else:
        runtime["ClaudeCode"] = {
            "Binary": claude_binary, "managedPolicyMode": "trust",
            "ToolBridge": {"Enabled": True, "Mode": "structured", "MaxActionRounds": 8, "TimeoutMillis": 120000, "ResultBytes": 4096},
            "Models": [{"ID": model, "Name": "Explicit Pilot model", "Efforts": [effort], "DefaultEffort": effort}],
        }
    return {"Launcher": {"Version": 1, "DiscoverProviders": False},
            "Runtime": runtime, "Permissions": permissions}


def bounded_probe(argv, env, timeout=15, limit=1 << 20, *, capture_stderr=False):
    """No raw stdout/stderr escapes on errors; never use the project as cwd."""
    with tempfile.TemporaryDirectory(prefix="unreal-pilot-preflight-") as directory:
        process = subprocess.Popen(argv, env=env, cwd=directory, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT if capture_stderr else subprocess.DEVNULL,
                                   start_new_session=True)
        output = bytearray()
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                deadline = time.monotonic() + timeout
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        fail("preflight_timeout", "Inspect provider installation/connectivity; no inference was sent.")
                    for key, _ in selector.select(min(remaining, 0.25)):
                        chunk = os.read(key.fileobj.fileno(), 8192)
                        if not chunk:
                            selector.unregister(key.fileobj)
                        else:
                            output.extend(chunk)
                            if len(output) > limit:
                                fail("preflight_output_limit", "Provider status exceeded its bound; raw output withheld.")
                code = process.wait(timeout=max(0.01, deadline - time.monotonic()))
                return bytes(output), code
        finally:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            if process.poll() is None:
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            process.stdout.close()


def claude_preflight(binary, home, environ, audited, trust):
    # Check explicit environmental audit/consent before ANY Claude invocation.
    if not (audited and trust):
        fail("managed_policy_audit_required", "Audit devlore's administrative policy, hooks, helpers and telemetry; opt in explicitly, independently of asp.")
    for key in ("CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS"):
        if environ.get(key, "").strip().lower() not in ("", "0", "false"):
            fail("claude_subscription_mode_conflict", "Remove conflicting routing only after operator review; no provider fallback.")
    for key in ("ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_CONFIG_DIR", "CLAUDE_CODE_CLIENT_DATA_URL"):
        if environ.get(key):
            fail("claude_subscription_mode_conflict", "Existing custom provider routing is incompatible with this Team pilot; nothing changed.")
    path = shutil.which(binary)
    if not path:
        fail("claude_binary_missing", "Install/sign in with the existing Claude CLI separately; no fallback attempted.")
    config_dir = environ.get("CLAUDE_CONFIG_DIR") or str(home / ".claude")
    if not Path(config_dir).is_absolute():
        fail("claude_configuration_invalid", "CLAUDE_CONFIG_DIR must be absolute; existing settings were not changed.")
    # This mirrors the existing adapter transport whitelist, not parent env.
    env = {"HOME": str(home), "CLAUDE_CONFIG_DIR": config_dir, "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "TERM": "dumb", "NO_COLOR": "1",
           "CLAUDE_CODE_SAFE_MODE": "1", "CLAUDE_CODE_SKIP_PROMPT_HISTORY": "1", "DISABLE_AUTOUPDATER": "1",
           "DISABLE_COMPACT": "1", "DISABLE_AUTO_COMPACT": "1", "CLAUDE_AGENT_SDK_DISABLE_BUILTIN_AGENTS": "1",
           "CLAUDE_CODE_AUTO_CONNECT_IDE": "false", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION": "false"}
    version, code = bounded_probe([path, "--version"], env)
    if code != 0 or not re.match(rb"2\.1\.(?:[0-9]+)(?:\s|$)", version) or int(version.split()[0].split(b".")[2]) < 285:
        fail("claude_version_unsupported", "Use the supported Claude Code 2.1.285+ contract; raw output withheld.")
    help_text, code = bounded_probe([path, "--help"], env)
    required = ["--safe-mode", "--restricted", "--setting-sources", "--settings", "--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config",
                "--disable-slash-commands", "--no-chrome", "--no-session-persistence", "--model", "--effort", "--input-format", "--output-format",
                "--verbose", "--include-partial-messages", "--permission-prompts", "--allowedTools", "--json-schema"]
    if code != 0 or any(flag.encode() not in help_text for flag in required):
        fail("claude_contract_unavailable", "Installed CLI lacks required isolation/structured flags; no inference/fallback.")
    raw, code = bounded_probe([path, *ISOLATION, "--system-prompt-file", "/dev/null", "--max-turns", "1", "auth", "status"], env, limit=65536)
    try:
        status = decode(raw)
    except (ValueError, UnicodeError):
        fail("claude_status_invalid", "CLI auth status is malformed; raw account/status output withheld.")
    if not isinstance(status, dict) or type(status.get("loggedIn")) is not bool:
        fail("claude_status_invalid", "CLI auth status has an invalid shape; no inference.")
    if code == 1 or not status["loggedIn"]:
        fail("claude_login_required", "Use the existing Claude CLI's login flow separately, then retry this script.")
    if code != 0 or status.get("authMethod") != "claude.ai" or status.get("apiProvider") != "firstParty" or status.get("subscriptionType") not in ("team", "enterprise", "pro", "max"):
        fail("claude_subscription_unavailable", "A supported first-party Claude subscription is required; no API-key/provider fallback.")
    print("PASS: Claude CLI auth status; policy audit/managed trust explicitly operator-attested (not a policy isolation proof)")
    return str(Path(path).absolute())


def codex_preflight(home, environ):
    path = Path(environ.get("OPENAI_CODEX_AUTH_FILE") or str(Path(environ.get("CODEX_HOME") or home / ".codex") / "auth.json"))
    if not path.is_absolute():
        fail("codex_auth_path_invalid", "Use an absolute existing External Codex auth path.")
    try:
        info = path.lstat()
    except FileNotFoundError:
        fail("codex_login_required", "Use existing Codex authentication separately; no auth file was read or created.")
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or not os.access(path, os.R_OK):
        fail("codex_auth_path_permissions", "Existing auth must be a readable user-owned private regular file; no chmod/import performed.")
    print("PASS: External Codex auth path metadata only; token/account/content/validity not inspected")
    return str(path)


def installed_kit(home, supplied):
    if supplied:
        directory = Path(supplied).absolute()
    else:
        matches = list((home / ".local/opt/unreal-agent-pilot/asp").glob("6284e49-*"))
        if len(matches) != 1:
            fail("pilot_install_required", "Reuse the existing 6284e49 kit installation, or specify its directory with --kit-directory; no binary installed/overwritten.")
        directory = matches[0]
    for parent in (*directory.parents, directory):
        if parent.is_symlink():
            fail("pilot_install_redirect", "Use a verified non-symlink existing Pilot installation.")
    for name in ("KIT.json", "SNAPSHOT.json", "doctor.sh", "SHA256SUMS"):
        path = directory / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 65536:
            fail("pilot_install_invalid", "Existing Pilot metadata is invalid; preserve it and inspect the original kit.")
    manifest = decode((directory / "KIT.json").read_bytes())
    snapshot = decode((directory / "SNAPSHOT.json").read_bytes())
    if manifest.get("baseline") != BASELINE or snapshot.get("revision") != BASELINE or snapshot.get("cgo_enabled") is not True:
        fail("pilot_revision_mismatch", "This profile requires the existing 6284e49 CGO snapshot.")
    # Verify the doctor before executing it; auth contents are never inspected.
    import hashlib
    doctor_sum = hashlib.sha256((directory / "doctor.sh").read_bytes()).hexdigest()
    if f"{doctor_sum}  doctor.sh" not in (directory / "SHA256SUMS").read_text().splitlines():
        fail("pilot_checksum_invalid", "Existing doctor checksum mismatch; nothing overwritten.")
    _, code = bounded_probe(["/bin/bash", str(directory / "doctor.sh"), "--preflight"], {"HOME": str(home), "PATH": "/usr/bin:/bin"}, timeout=30)
    if code != 0:
        fail("pilot_binary_preflight_failed", "Run the existing kit doctor; architecture/glibc/dependencies/checksums must pass first.")
    print("PASS: existing 6284e49 kit / Linux amd64 / libraries / CGO provenance")
    return directory


class Gateway(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=5)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)

    def call(self, method, payload):
        try:
            self.request("POST", "/v1/" + method, json.dumps(payload), {"Content-Type": "application/json"})
            response = self.getresponse()
            raw = response.read((4 << 20) + 1)
            if response.status != 200 or len(raw) > 4 << 20:
                fail("gateway_protocol_failed", "Pilot gateway returned an invalid/bounded response; raw contents withheld.")
            result = decode(raw)
            if not isinstance(result, dict) or result.get("Error"):
                fail("gateway_request_failed", "Pilot control request failed; inspect the matching Pilot Host, without retrying inference.")
            return result
        finally:
            self.close()


def host_arguments(kit, paths, auth_file):
    args = [str(kit / "unreal-agent"), "serve", "--config", str(paths["config"]), "--session-directory", str(paths["sessions"]), "--socket", str(paths["socket"])]
    if auth_file:
        args += ["--codex-auth-file", auth_file]
    return args


def launcher_arguments(kit, paths, auth_file, session_id):
    args = [str(kit / "unreal"), "--config", str(paths["config"]), "--state-directory", str(paths["state"]),
            "--socket", str(paths["socket"]), "--startup-timeout", "90s"]
    if auth_file:
        args += ["--codex-auth-file", auth_file]
    return [*args, session_id]


def host_binding(paths, auth_file):
    return {"Action": "launcher.check", "HostBinding": {"Configuration": str(paths["config"]), "Sessions": str(paths["sessions"]),
            "Credentials": "", "CodexFile": auth_file, "Normal": False}}


def ensure_host(kit, paths, auth_file, environ, *, docker_sandbox_oauth=False):
    if docker_sandbox_oauth and environ.get("SBX_CRED_OPENAI_MODE") != "oauth":
        fail("docker_sandbox_oauth_unavailable", "Docker kit must explicitly use OAuth; no API-key or provider fallback.")
    lock = paths["state"] / "provider-start.lock"
    fd = os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        gateway = Gateway(paths["socket"])
        if paths["socket"].is_symlink():
            fail("pilot_socket_redirect", "Existing socket is a symlink; nothing unlinked or reused.")
        if paths["socket"].exists():
            info = paths["socket"].stat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                fail("pilot_socket_invalid", "Keep the existing path; a private user-owned socket is required.")
            # Never unlink a socket, kill an existing owner or reuse a mismatch.
            gateway.call("extension", host_binding(paths, auth_file))
            return gateway
        log = paths["state"] / "host.log"
        if log.exists() and (not stat.S_ISREG(log.lstat().st_mode) or log.stat().st_uid != os.getuid() or log.stat().st_mode & 0o077):
            fail("pilot_log_invalid", "Keep the existing log; it must be a private user-owned regular file.")
        log_fd = os.open(log, os.O_CREAT | os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW, 0o600)
        env = {key: value for key, value in environ.items() if key in ("HOME", "PATH", "LANG", "TERM", "COLORTERM", "NO_COLOR", "CLAUDE_CONFIG_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR")}
        env["HOME"] = str(paths["home"])
        if docker_sandbox_oauth:
            env["SBX_CRED_OPENAI_MODE"] = "oauth"
        try:
            process = subprocess.Popen(host_arguments(kit, paths, auth_file), cwd=kit, env=env,
                                       stdin=subprocess.DEVNULL, stdout=log_fd, stderr=log_fd, start_new_session=True)
        finally:
            os.close(log_fd)
        deadline = time.monotonic() + 90
        try:
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    fail("pilot_host_start_failed", "Pilot startup failed; inspect its private host.log. No inference/fallback was sent.")
                try:
                    gateway.call("auth/methods", {})
                    gateway.call("extension", host_binding(paths, auth_file))
                    print("PASS: private Pilot Host ready; default Host untouched")
                    return gateway
                except (OSError, http.client.HTTPException):
                    time.sleep(0.1)
            fail("pilot_host_timeout", "Pilot Host did not become ready through its gateway; keep history/logs for diagnosis.")
        except BaseException:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    fail("pilot_host_shutdown_pending", "Only the newly started Pilot Host was signaled; inspect it manually, keep its state.")
            raise
    finally:
        os.close(fd)


def smoke_outcome(view, expected):
    if not isinstance(view, dict):
        return "protocol_failure"
    if view.get("Failure"):
        return "provider_failure"
    if view.get("Operations"):
        return "unexpected_operation"
    page = view.get("History", {})
    if not isinstance(page, dict) or not isinstance(page.get("Items", []), list):
        return "protocol_failure"
    if page.get("More"):
        return "history_bound"
    messages = []
    for item in page.get("Items", []):
        if not isinstance(item, dict) or not isinstance(item.get("Data", {}), dict):
            return "protocol_failure"
        if item.get("Kind") == "tool_call_status":
            return "unexpected_operation"
        if item.get("Kind") != "model_response":
            continue
        response = item.get("Data", {}).get("Response", {})
        if not isinstance(response, dict) or not isinstance(response.get("Output", []), list):
            return "protocol_failure"
        if response.get("Failure"):
            return "provider_failure"
        for output in response.get("Output", []):
            if not isinstance(output, dict) or not isinstance(output.get("Data", {}), dict):
                return "protocol_failure"
            if output.get("Type") == "tool_call":
                return "unexpected_operation"
            if output.get("Type") == "message" and output.get("Data", {}).get("Role") == "assistant":
                text = output["Data"].get("Text", "")
                if not isinstance(text, str):
                    return "protocol_failure"
                if response.get("Stop") != "complete":
                    return "pending"
                messages.append(text)
    if messages:
        return "pass" if len(messages) == 1 and messages[0].strip() == expected else "response_mismatch"
    return "pending"


def smoke(gateway, sandbox, pointer, timeout=180):
    session_id = f"pilot-{sandbox}-smoke-{uuid.uuid4().hex[:12]}"
    view = gateway.call("open", {"ID": session_id, "Mode": "create", "Limit": 64})["View"]
    publish_json(pointer, {"sandbox": sandbox, "session": session_id}, replace=True)
    expected = f"Unreal {sandbox} provider smoke OK."
    # Exactly one submission; never retry on timeout/network/provider failure.
    gateway.call("submit", {"ID": session_id, "Generation": view["Generation"],
                 "Input": {"ID": "pilot-smoke-input", "Kind": "external", "Payload": f"Reply with exactly: {expected} Do not call tools or inspect any file. Return only the final response."}})
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        view = gateway.call("inspect", {"ID": session_id, "After": 0, "Limit": 64})["View"]
        outcome = smoke_outcome(view, expected)
        if outcome == "pass":
            print(f"PASS: provider={PROVIDERS[sandbox]} real-smoke response durable; Operations=0; no automatic retry/fallback")
            return session_id
        if outcome != "pending":
            fail("smoke_" + outcome, "History preserved. Use --attach-only with the same profile for /analyze Errors; no inference resubmitted.")
        time.sleep(0.2)
    fail("smoke_timeout", "No request retry/cancel/replay performed; the dedicated Host and canonical history remain available via --attach-only.")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sandbox", choices=PROVIDERS)
    parser.add_argument("--model", required=True, help="explicit operator model ID; availability is not guessed")
    parser.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"))
    parser.add_argument("--instance", default="v1")
    parser.add_argument("--kit-directory")
    parser.add_argument("--claude-binary", default="claude")
    parser.add_argument("--policy-audited", action="store_true")
    parser.add_argument("--trust-managed-policy", action="store_true")
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--preflight-only", action="store_true")
    modes.add_argument("--smoke-only", action="store_true")
    modes.add_argument("--attach-only", action="store_true")
    args = parser.parse_args(argv)
    try:
        home = Path(os.environ.get("HOME", ""))
        if not home.is_absolute() or not home.is_dir() or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,31}", args.instance):
            fail("invalid_pilot_path", "Existing absolute HOME and a safe --instance name required.")
        home = home.resolve()
        workspace = home / "workspace"
        if workspace.is_symlink() or not workspace.is_dir():
            fail("workspace_unavailable", "Use the Sandbox's existing ~/workspace directory; no project created or changed.")
        # Validate trust/selection before invoking any external provider process.
        effort = args.effort or ("high" if args.sandbox == "devlore" else "medium")
        configuration(args.sandbox, workspace, args.model, effort, audited=args.policy_audited, trust=args.trust_managed_policy)
        kit = installed_kit(home, args.kit_directory)
        auth_file, claude_binary = "", ""
        if args.sandbox == "asp":
            auth_file = codex_preflight(home, os.environ)
        else:
            claude_binary = claude_preflight(args.claude_binary, home, os.environ, args.policy_audited, args.trust_managed_policy)
        config_dir = private_tree(home, f".config/unreal-agent-pilot/{args.sandbox}/{args.instance}")
        state = private_tree(home, f".local/state/unreal-agent-pilot/{args.sandbox}/{args.instance}")
        run_dir = private_tree(state, "run")
        paths = {"home": home, "config": config_dir / "runtime.json", "state": state, "sessions": state / "sessions", "socket": run_dir / "host.sock"}
        if len(os.fsencode(paths["socket"])) > 107:
            fail("pilot_socket_path_too_long", "Use a shorter private HOME/instance; no default socket is reused.")
        publish_json(paths["config"], configuration(args.sandbox, workspace, args.model, effort, claude_binary, args.policy_audited, args.trust_managed_policy))
        print(f"PASS: dedicated {args.sandbox} config; read/grep/glob only; process/write/network tool execution denied")
        if args.preflight_only:
            print("INFO: no Host or inference started; auth/API connectivity still unproven")
            return 0
        gateway = ensure_host(kit, paths, auth_file, os.environ)
        pointer = state / "latest-smoke.json"
        if args.attach_only:
            info = pointer.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 1024:
                fail("pilot_session_pointer_invalid", "Keep the pointer and history; invalid Pilot pointer.")
            record = decode(pointer.read_bytes())
            session_id = record.get("session", "")
            if record.get("sandbox") != args.sandbox or not re.fullmatch(f"pilot-{args.sandbox}-smoke-[0-9a-f]{{12}}", session_id):
                fail("pilot_session_pointer_invalid", "Preserve the pointer/history and inspect the Pilot state; no new inference.")
        else:
            session_id = smoke(gateway, args.sandbox, pointer)
        if not args.smoke_only:
            command = launcher_arguments(kit, paths, auth_file, session_id)
            # Explicit config + matching socket/store; native launcher owns attach/resume.
            os.execv(command[0], command)
        return 0
    except PilotError as error:
        print(f"FAIL: {error.code}: {error.action}", file=sys.stderr)
    except (OSError, ValueError, UnicodeError, KeyError, TypeError, subprocess.SubprocessError, http.client.HTTPException):
        print("FAIL: pilot_preflight_or_transport_failed: Raw details withheld. Preserve existing settings/history and inspect Pilot prerequisites.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
