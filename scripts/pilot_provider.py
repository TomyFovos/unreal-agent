#!/usr/bin/env python3
"""File, provider preflight and gateway helpers for development and acceptance."""

import http.client
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import stat
import subprocess
import tempfile


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


def bounded_probe(argv, env, *, capture_stderr=False):
    """Wait for status or caller interruption; raw output stays private."""
    with tempfile.TemporaryDirectory(prefix="unreal-pilot-preflight-") as directory:
        process = subprocess.Popen(argv, env=env, cwd=directory, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT if capture_stderr else subprocess.DEVNULL,
                                   start_new_session=True)
        try:
            output, _ = process.communicate()
            return output, process.returncode
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
    raw, code = bounded_probe([path, *ISOLATION, "--system-prompt-file", "/dev/null", "--max-turns", "1", "auth", "status"], env)
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
    print("PASS: Claude CLI subscription auth status")
    return str(Path(path).absolute())


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


def host_binding(paths, auth_file, binary):
    return {"Action": "launcher.check", "HostBinding": {"Configuration": str(paths["config"]), "Sessions": str(paths["sessions"]),
            "Credentials": "", "CodexFile": auth_file, "Normal": False}}


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
