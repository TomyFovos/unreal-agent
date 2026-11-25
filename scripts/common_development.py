#!/usr/bin/env python3
"""Install a common wrapper; native Unreal owns discovery, Host and execution."""

import argparse
from contextlib import closing
from datetime import datetime
import hashlib
from http.client import HTTPException
import json
import os
from pathlib import Path
import re
import select
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
from zoneinfo import ZoneInfo

import daily_development as daily
import devlore_launcher as files
import pilot_provider as pilot


NAMESPACE = "unreal-agent-common"
EXTRAS = ("asp_oauth.py", "common_development.py", "daily_development.py", "development.sh",
          "devlore_launcher.py", "permission_acceptance.py", "pilot_provider.py")
MEMBERS = (*daily.asp.pilot_binaries(), "LICENSE", "SNAPSHOT.json", "KIT.json", "README.md",
           "install.sh", "doctor.sh", *EXTRAS)
PROVIDER_REASONS = {"", "auth_required", "cli_missing_or_unsupported", "managed_trust_required",
                    "docker_proxy_invalid", "catalog_unavailable", "operator_disabled", "provider_check_failed"}


def settings_path(home):
    return home / ".config" / NAMESPACE / "launcher.json"


def shared_state_directory(home):
    return Path(os.environ.get("XDG_STATE_HOME") or home / ".local/state") / "unreal-agent"


def package(directory):
    return pilot.decode((directory / "KIT.json").read_bytes())


def read_settings(home):
    # Old v1-v3 bindings may still carry retired audit/hash fields.
    value = pilot.decode(files.read_owned(settings_path(home), private=True, limit=8192))
    daily.safe_path(value["BinaryDirectory"])
    value.setdefault("CodexRoute", "docker" if value["DockerOAuth"] else "external")
    value.setdefault("ClaudeDecision", "enabled")
    return value


def validate_status(status, api_ids=()):
    if not isinstance(status, list) or len(status) != 2 + len(api_ids):
        pilot.fail("common_preflight_invalid", "Expected two independent safe provider statuses.")
    ids = set()
    for entry in status:
        if (not isinstance(entry, dict) or set(entry) != {"ID", "Availability", "Reason"}
                or entry["ID"] not in ("openai-codex", "claude", "claude-code", *api_ids) or entry["ID"] in ids
                or entry["Availability"] not in ("available", "unavailable")
                or entry["Reason"] not in PROVIDER_REASONS
                or (entry["Availability"] == "available") != (entry["Reason"] == "")):
            pilot.fail("common_preflight_invalid", "Provider preflight returned an invalid safe status; body withheld.")
        ids.add(entry["ID"])
    canonical = {"claude" if key == "claude-code" else key for key in ids}
    if canonical != {"openai-codex", "claude", *api_ids}:
        pilot.fail("common_preflight_invalid", "Provider status does not match explicit connections.")
    return status


def api_key_sources(args, previous):
    sources = dict(previous.get("APIKeys", {}) if previous else {})
    for provider, prefix in (("openai", "openai"), ("claude-api", "anthropic")):
        path = getattr(args, prefix + "_api_key_file", "")
        env = getattr(args, prefix + "_api_key_env", "")
        if path and env:
            pilot.fail("common_api_key_source_invalid", "Choose a key file or an environment variable name, not both.")
        if path:
            path = Path(path).expanduser().resolve()
            try:
                info = path.stat()
            except OSError:
                pilot.fail("common_api_key_file_unreadable", "API key file is unreadable; value withheld.")
            if not path.is_file() or info.st_mode & 0o777 != 0o600:
                pilot.fail("common_api_key_file_requires_0600", "API key file must be a regular file with permissions 0600 (chmod 600).")
            sources[provider] = {"file": str(path)}
        elif env:
            if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", env):
                pilot.fail("common_api_key_source_invalid", "Specify an environment variable name; no key values.")
            sources[provider] = {"env": env}
    return sources


def api_key_arguments(settings):
    result = []
    for provider, prefix in (("openai", "openai"), ("claude-api", "anthropic")):
        source = settings.get("APIKeys", {}).get(provider, {})
        for kind in ("file", "env"):
            if source.get(kind):
                result += ["--" + prefix + "-api-key-" + kind, source[kind]]
    return result


def provider_choices(args, previous):
    route_flags = [name for name in ("docker_sandbox_oauth", "external_codex", "disable_codex", "codex_auto") if getattr(args, name, False)]
    if len(route_flags) > 1:
        pilot.fail("common_provider_conflict", "Conflicting explicit provider choices; no probe or installation started.")
    route = {"docker_sandbox_oauth": "docker", "external_codex": "external", "disable_codex": "disabled", "codex_auto": "auto"}.get(route_flags[0]) if route_flags else (
        previous["CodexRoute"] if previous else "auto")
    decision = previous["ClaudeDecision"] if previous else "enabled"
    if getattr(args, "configure_providers", False): decision = "enabled"
    if getattr(args, "disable_claude", False): decision = "disabled"
    return route, decision


def preflight(settings):
    command = [str(Path(settings["BinaryDirectory"]) / "unreal-agent"), "providers"]
    if settings["DockerOAuth"]: command.append("--docker-sandbox-oauth")
    if settings["CodexRoute"] == "auto": command.append("--codex-auto")
    if settings["CodexRoute"] == "disabled": command.append("--codex-disabled")
    if settings["ClaudeDecision"] == "disabled": command.append("--claude-disabled")
    if settings["ClaudeBinary"]: command += ["--claude-binary", settings["ClaudeBinary"]]
    command += api_key_arguments(settings)
    raw, code = pilot.bounded_probe(command, dict(os.environ))
    if code: pilot.fail("common_preflight_failed", "Native auth/catalog preflight failed; body withheld. No inference or fallback.")
    result = pilot.decode(raw)
    expected = {"Providers", "CodexRoute"} if settings["CodexRoute"] == "auto" else {"Providers"}
    if (not isinstance(result, dict) or set(result) != expected or not isinstance(result["Providers"], list)
            or len(result["Providers"]) != 2 + len(settings.get("APIKeys", {})) or any(not isinstance(p, dict) for p in result["Providers"])):
        pilot.fail("common_preflight_invalid", "Invalid native provider status; body withheld.")
    statuses = validate_status([{"ID": p.get("ID"), "Availability": p.get("Availability"), "Reason": p.get("Reason", "")} for p in result["Providers"]], settings.get("APIKeys", {}))
    route = result.get("CodexRoute", settings["CodexRoute"])
    routes = ("auto", "external", "docker") if settings["CodexRoute"] == "auto" else (settings["CodexRoute"],)
    if route not in routes or (settings["CodexRoute"] == "auto" and
            (next(p for p in statuses if p["ID"] == "openai-codex")["Availability"] == "available") != (route != "auto")):
        pilot.fail("common_preflight_invalid", "Invalid native source decision; values withheld.")
    return statuses, route


def wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent common development; native discovery and Host.\nexec "
            + shlex.quote(str(Path(sys.executable).resolve())) + " -B "
            + shlex.quote(str(directory / "common_development.py")) + ' launch "$@"\n').encode()


def agent_wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent subagent CLI.\nexec "
            + shlex.quote(str(directory / "unreal-agent")) + ' "$@"\n').encode()


def shell_path_script():
    return b'''case ":${PATH-}:" in
 *":$HOME/.local/bin:"*) ;;
 *) PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;
esac
export PATH
ucd() {
    local unreal_workspace
    unreal_workspace="$("$HOME/.local/bin/unreal" --where "$@")" || return
    builtin cd -- "$unreal_workspace"
}
'''


def owned_legacy(home, value, previous=None):
    if daily.owned_unreal_command(home, value, [previous["BinaryDirectory"]] if previous else []):
        return True
    for sandbox in ("asp", "devlore"):
        path = daily.settings_path(home, sandbox)
        if os.path.lexists(path):
            old = daily.read_settings(home, sandbox)
            if daily.owned_previous_command(home, sandbox, value, old): return True
    return False


def previous_version(home, installed, previous):
    """The launcher binding is authoritative; current.json covers older kits."""
    if previous:
        for value in (previous.get("BinaryDirectory"), previous.get("PreviousBinaryDirectory")):
            if value and Path(value) != installed:
                return Path(value)
    choices = (home / ".config" / NAMESPACE / "repos").glob("*/current.json")
    candidates = []
    for path in choices:
        try:
            value = json.loads(path.read_text()).get("BinaryDirectory")
            if value and Path(value) != installed:
                candidates.append((path.stat().st_mtime_ns, Path(value)))
        except (OSError, ValueError, TypeError, AttributeError):
            continue
    return max(candidates, key=lambda item: item[0])[1] if candidates else None


def running_versions(versions, proc=Path("/proc")):
    """Keep executable mappings and scripts/wrappers named by a live process."""
    active = set()
    for process in proc.iterdir():
        if not process.name.isdigit():
            continue
        references = []
        try:
            references.append(os.fsencode(os.readlink(process / "exe")))
        except OSError:
            pass  # the process may already have exited, or belong to another user
        try:
            references.extend((process / "cmdline").read_bytes().split(b"\0"))
        except OSError:
            pass
        for version in versions:
            name = os.fsencode(version)
            if any(ref == name or name + b"/" in ref for ref in references):
                active.add(version)
    return active


def allocated_bytes(directory):
    total = directory.lstat().st_blocks * 512
    for root, directories, names in os.walk(directory, followlinks=False):
        for name in (*directories, *names):
            total += (Path(root) / name).lstat().st_blocks * 512
    return total


def prune_versions(home, installed, previous, proc=Path("/proc")):
    base = home / ".local/opt/unreal-agent-pilot/common"
    versions = sorted(path for path in base.iterdir() if not path.is_symlink()
                      and path.is_dir() and re.fullmatch(r"[0-9a-f]{7}-[0-9a-f]{16}", path.name))
    keep = {installed, previous} | running_versions(versions, proc)
    removed, freed = [], 0
    for version in versions:
        if version in keep:
            continue
        try:
            size = allocated_bytes(version)
            shutil.rmtree(version)
            removed.append(version.name)
            freed += size
        except OSError as err:
            print(f"旧版の削除を見送り: {version.name}: {err}", file=sys.stderr)
    print(f"旧版を削除: {', '.join(removed) or 'なし'}（{freed / 1e6:.0f} MB 解放）")


def open_code_go_key(args, meta):
    path = getattr(args, "opencode_go_api_key_file", "")
    env = getattr(args, "opencode_go_api_key_env", "")
    if not path and not env:
        return None
    # Check the distributor choice before reading either source. No secret or
    # source value is ever included in an error.
    if not meta.get("opencode_go_enabled", True):
        pilot.fail("opencode_go_disabled", "この配布物では OpenCode Go は使えません。キーは受け付けません。")
    if path and env:
        pilot.fail("common_api_key_source_invalid", "Choose a key file or an environment variable name, not both.")
    if path:
        try:
            source = Path(path).expanduser()
            if not source.is_file() or source.stat().st_mode & 0o777 != 0o600:
                pilot.fail("common_api_key_file_requires_0600", "API key file must be a regular file with permissions 0600.")
            raw = source.read_bytes()
        except OSError:
            pilot.fail("common_api_key_file_unreadable", "API key file is unreadable; value withheld.")
    else:
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", env):
            pilot.fail("common_api_key_source_invalid", "Specify an environment variable name; no key values.")
        raw = os.environ.get(env, "").encode()
    if not raw.strip() or len(raw) > 16384:
        pilot.fail("common_api_key_source_invalid", "API key source is empty or invalid; value withheld.")
    return raw


def install(args, directory, meta):
    go_key = open_code_go_key(args, meta)
    home = daily.home()
    if args.claude_binary and (len(args.claude_binary) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in args.claude_binary)):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    previous = read_settings(home) if os.path.lexists(settings_path(home)) else None
    zone = getattr(args, "time_zone", None) or (previous or {}).get("TimeZone")
    max_children = getattr(args, "max_active_children", None)
    if max_children is None:
        max_children = (previous or {}).get("MaxActiveChildren")
    choices = provider_choices(args, previous)
    sources = api_key_sources(args, previous)
    content_id = hashlib.sha256(files.read_owned(directory / "SHA256SUMS", limit=4096)).hexdigest()[:16]
    installed = home / ".local/opt/unreal-agent-pilot/common" / (meta["baseline"][:7] + "-" + content_id)
    prior_version = previous_version(home, installed, previous)
    command, new_command = home / ".local/bin/unreal", wrapper(installed)
    old = files.read_owned(command, private=True) if os.path.lexists(command) else None
    if old is not None and old != new_command:
        if not owned_legacy(home, old, previous):
            pilot.fail("common_command_conflict", "Existing unrelated unreal command preserved.")
    agent_command = home / ".local/bin/unreal-agent"
    new_agent = agent_wrapper(installed)
    old_agent = files.read_owned(agent_command, private=True) if os.path.lexists(agent_command) else None
    if old_agent is not None and old_agent != new_agent:
        if previous is None or old_agent != agent_wrapper(Path(previous["BinaryDirectory"])):
            pilot.fail("common_command_conflict", "Existing unrelated unreal-agent command preserved.")
    profiles = files.shell_profiles(home)
    files.inspect_shell_paths({"home": home})
    directory_config = pilot.private_tree(home, ".config/" + NAMESPACE)
    with daily.locked(directory_config):
        route, decision = choices
        settings = {"Version": 3, "Home": str(home), "BinaryDirectory": str(installed), "Revision": meta["baseline"],
                    "DockerOAuth": route == "docker", "CodexRoute": route, "ClaudeDecision": decision,
                    "ClaudeBinary": args.claude_binary or (previous["ClaudeBinary"] if previous else "")}
        if sources:
            settings["APIKeys"] = sources
        if zone:
            settings["TimeZone"] = zone
        if max_children is not None:
            settings["MaxActiveChildren"] = max_children
        if prior_version is not None:
            settings["PreviousBinaryDirectory"] = str(prior_version)
        old_settings = files.read_owned(settings_path(home), private=True) if previous else None
        raw, code = pilot.bounded_probe(["bash", str(directory / "install.sh")],
                                       {"HOME": str(home), "PATH": os.environ["PATH"]})
        if code:
            pilot.fail("common_install_failed", "Inspect kit doctor; existing CLI/auth/config/history unchanged.")
        settings["ProviderStatus"], resolved = preflight(settings)
        if previous is None and route == "auto":
            settings.update(CodexRoute=resolved, DockerOAuth=resolved == "docker")
        new_settings = (json.dumps(settings, indent=2, ensure_ascii=True) + "\n").encode()
        files.command_directory(home)
        if old is not None and old != new_command:
            daily.replace_owned(command, new_command, old, 0o700)
        else:
            files.publish_bytes(command, new_command, 0o700)
        if old_agent is not None and old_agent != new_agent:
            daily.replace_owned(agent_command, new_agent, old_agent, 0o700)
        else:
            files.publish_bytes(agent_command, new_agent, 0o700)
        if old_settings is not None and old_settings != new_settings:
            daily.replace_owned(settings_path(home), new_settings, old_settings, 0o600)
        else:
            files.publish_bytes(settings_path(home), new_settings)
        path_script = directory_config / "path.sh"
        new_path_script = shell_path_script()
        if os.path.lexists(path_script):
            previous_path_script = files.read_owned(path_script, private=True)
            if previous_path_script != new_path_script:
                daily.replace_owned(path_script, new_path_script, previous_path_script, 0o600)
        else:
            files.publish_bytes(path_script, new_path_script)
        block = ("# Unreal Agent common development PATH\n[ ! -r " + shlex.quote(str(path_script))
                 + " ] || . " + shlex.quote(str(path_script)) + "\n").encode()
        for profile in profiles:
            files.append_profile(profile, block)
        if any(kind != b"owned" or location != b"present" for kind, location in files.inspect_shell_paths({"home": home})):
            pilot.fail("common_shell_path_unavailable", "Keep installed files. Open a fresh Bash; current aliases/functions cannot be removed by this installer. Use $HOME/.local/bin/unreal explicitly.")
        prune_versions(home, installed, prior_version)
    if go_key is not None:
        result = subprocess.run([str(installed / "unreal-agent-auth"), "login", "--store",
                                 str(shared_state_directory(home) / "credentials"), "--provider", "opencode-go",
                                 "--id", "primary", "--key-stdin"], input=go_key,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        if result.returncode:
            pilot.fail("common_api_key_install_failed", "OpenCode Go のキーを保存できませんでした。値は表示しません。")
    print_install_result(settings, home)


def shell_masks_unreal(home):
    # type -a resolves aliases/functions before files. Emit only a closed enum;
    # never print profile output, executable values or the shell environment.
    script = '''type -a unreal >/dev/null 2>&1 || exit 0
kind=$(type -t unreal || :)
if [ "$kind" = file ] && [ "$(type -P unreal)" = "$1" ]; then
  printf '\\nUNREAL_MASKED:no\\n'
else
  printf '\\nUNREAL_MASKED:yes\\n'
fi
'''
    env = {"HOME": str(home), "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "TERM": "dumb", "LANG": "C.UTF-8"}
    for flags in (("--noprofile", "-ic"), ("-lic",)):
        raw, code = pilot.bounded_probe(["/bin/bash", *flags, script, "unreal-check", str(home / ".local/bin/unreal")], env)
        if not code and b"UNREAL_MASKED:yes" in raw.splitlines():
            return True
    return False


def provider_guidance(status):
    if status["ID"] in ("openai", "claude-api"):
        name = "OpenAI API" if status["ID"] == "openai" else "Claude API"
        if status["Availability"] != "available":
            hint = "Claude Code をインストールしてください" if status["Reason"] == "cli_missing_or_unsupported" else "選択したキーの参照先と 0600 のファイル権限を確認してください"
            return name + "（API キー・別課金）: " + hint
        return name + "（API キー・別課金）: /model で明示的に選ぶと使います"
    claude = status["ID"] in ("claude", "claude-code")
    name = "Claude" if claude else "Codex"
    cli = "Claude Code" if claude else "Codex CLI"
    login = "claude を起動して /login" if claude else "codex login"
    if status["Availability"] == "available":
        return name + ": 利用できます"
    reason = status["Reason"]
    if reason == "cli_missing_or_unsupported":
        return name + ": " + cli + " をインストールしてから " + login + " でログインしてください"
    if reason == "operator_disabled":
        return name + ": 設定で無効になっています"
    if reason == "catalog_unavailable":
        return name + ": モデル一覧を読み取れません。" + cli + " の起動・ログイン状態を確認してください（" + login + "）"
    if reason == "auth_required":
        return name + ": ログインが必要です。" + login
    return name + ": 接続を確認してください。" + cli + " の起動・ログイン状態を確認（" + login + "）"


def print_install_result(settings, home):
    print("インストール完了")
    statuses = settings["ProviderStatus"]
    for status in statuses:
        print(provider_guidance(status))
    codex = any(p["ID"] == "openai-codex" and p["Availability"] == "available" for p in statuses)
    claude = any(p["ID"] in ("claude", "claude-code") and p["Availability"] == "available" for p in statuses)
    if codex and claude:
        print("Claude と ChatGPT（Codex）の両方で使えます。新規の統括は Claude で始まります。")
    elif claude:
        print("Claude だけで使えます。ChatGPT のログインは不要です。")
    elif codex:
        print("ChatGPT（Codex）だけで使えます。Claude のログインは不要です。")
    if any(p["ID"] in ("claude", "claude-code") and p["Availability"] == "available" for p in statuses):
        print("Claude の操作は /permissions で選んだ権限に従います。")
    if any(p["Availability"] == "available" for p in statuses):
        print("次に: 新しい Bash で作業フォルダに移動し、unreal を起動してください。/model でモデルを選べます。")
    else:
        print("次に: 使うにはどちらかの準備（インストールとログイン）が必要です。unreal を起動すると最初の入力にもログイン方法を表示します。")
    print("あとからログインしたら /model refresh で再検出し、/model で選んでください。インストールのやり直しは不要です。")
    if any(p["Reason"] == "operator_disabled" for p in statuses):
        print("明示的に無効にした接続先を使う場合は development.sh --configure-providers を再実行してください。Codex の再有効化には --codex-auto も付けてください。")
    if shell_masks_unreal(home):
        print("別の unreal が先に見つかりました。type -a unreal で確認するか、$HOME/.local/bin/unreal を使ってください。")


def repo_paths(home, workspace, instance="default"):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}", instance):
        pilot.fail("common_instance_invalid", "Use a short instance name without path separators.")
    identity = hashlib.sha256(os.fsencode(workspace)).hexdigest()
    key = "r-" + identity[:16] + ("" if instance == "default" else "-" + instance)
    root = home / ".config" / NAMESPACE / "repos" / key
    state = home / ".local/state" / NAMESPACE / key
    socket = state / "run/host.sock"
    if len(os.fsencode(socket)) > 100:
        pilot.fail("common_socket_too_long", "Use a shorter HOME/instance; no existing socket reused.")
    return {"root": root, "xdg": root / "native", "config": root / "native/unreal-agent/runtime.json",
            "state": state, "socket": socket, "binding": root / "repo.json", "identity": identity}


def command_for(settings, workspace, paths, session):
    binary_directory = paths.get("binary_directory", Path(settings["BinaryDirectory"]))
    command = [str(binary_directory / "unreal"),
               "--state-directory", str(paths["state"]), "--socket", str(paths["socket"]),
               "--shared-state-directory", str(shared_state_directory(Path(settings["Home"])))]
    if paths.get("explicit_config"): command += ["--config", str(paths["config"])]
    elif "MaxActiveChildren" in settings and not paths["config"].exists():
        command += ["--bootstrap-max-active-children", str(settings["MaxActiveChildren"])]
    if settings["DockerOAuth"]: command.append("--bootstrap-codex-docker-oauth")
    if settings.get("CodexRoute") == "auto": command.append("--bootstrap-codex-auto")
    if settings.get("CodexRoute") == "disabled": command.append("--bootstrap-codex-disabled")
    if settings.get("ClaudeDecision") == "disabled": command.append("--bootstrap-claude-disabled")
    if settings["ClaudeBinary"]: command.extend(("--bootstrap-claude-binary", settings["ClaudeBinary"]))
    command += api_key_arguments(settings)
    command.append(session)
    return command


def prepare_repo(settings, workspace, instance):
    home = Path(settings["Home"])
    p = repo_paths(home, workspace, instance)
    for directory in (p["xdg"], p["socket"].parent):
        pilot.private_tree(home, directory.relative_to(home))
    expected = (json.dumps({"Version": 1, "Workspace": str(workspace), "Identity": p["identity"]},
                           ensure_ascii=True, indent=2) + "\n").encode()
    with daily.locked(p["root"]):
        files.publish_bytes(p["binding"], expected)
        if os.path.lexists(p["config"]):
            old_config = files.read_owned(p["config"], private=True)
            config = pilot.decode(old_config)
            if not isinstance(config, dict) or not isinstance(config.get("Runtime"), dict) or config["Runtime"].get("Workspace") != str(workspace):
                pilot.fail("common_workspace_binding_mismatch", "Keep existing config/history; workspace changed. Use an explicit separate --instance.")
            changed = "MaxActiveChildren" in settings and config.get("MaxActiveChildren") != settings["MaxActiveChildren"]
            if changed:
                config["MaxActiveChildren"] = settings["MaxActiveChildren"]
            if settings.get("APIKeys"):
                launcher = config.get("Launcher") or {"Version": 1}
                config["Launcher"] = launcher
                sources = launcher.setdefault("APIKeys", {})
                if any(sources.get(key) != source for key, source in settings["APIKeys"].items()):
                    sources.update(settings["APIKeys"])
                    changed = True
            if changed:
                updated = (json.dumps(config, indent=2, ensure_ascii=True) + "\n").encode()
                daily.replace_owned(p["config"], updated, old_config, 0o600)
    return p


def provider_profile(settings):
    # Availability is transient; only explicit choices belong to the profile.
    value = {"CodexRoute": settings.get("CodexRoute", "docker" if settings["DockerOAuth"] else "external"),
             "ClaudeDecision": settings.get("ClaudeDecision", "enabled"), "ClaudeBinary": settings["ClaudeBinary"]}
    if settings.get("APIKeys"):
        value["APIKeys"] = settings["APIKeys"]
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


class LiveHost(pilot.Gateway):
    pid = None

    def connect(self):
        super().connect()
        pid, uid, _ = struct.unpack("3i", self.sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        if uid != os.getuid() or pid <= 1 or self.pid not in (None, pid):
            raise OSError("Host socket owner changed")
        self.pid = pid

    def installation(self, paths):
        process = Path("/proc") / str(self.pid)
        binary = Path(os.readlink(process / "exe"))
        args = os.fsdecode((process / "cmdline").read_bytes()).split("\0")
        if binary.name not in ("unreal", "unreal-agent") or args[1:2] != ["serve"]:
            return None, False
        expected = {"--config": paths["config"], "--socket": paths["socket"],
                    "--session-directory": paths["state"] / "sessions"}
        bound = True
        for flag, value in expected.items():
            assigned = [arg.partition("=")[2] for arg in args if arg.startswith(flag + "=")]
            assigned.extend(arg for option, arg in zip(args, args[1:]) if option == flag)
            if assigned != [str(value)]:
                bound = False
                break
        return binary.parent, bound


def legacy_workload(host, paths, session):
    # The old gateway has only per-Session inspection. Use the existing name
    # discovery for addresses and take every activity count from the gateway.
    names = {name for _, name in main_session_files(paths["state"])}
    work = {"ActiveSessions": 0, "Operations": 0, "Children": 0}
    for name in sorted(names | {session}):
        reply = host.call("inspect", {"ID": name, "Limit": 1}, allow_error=True)
        if reply.get("Error") == "not_found":
            continue
        view = reply.get("View")
        if not isinstance(view, dict):
            pilot.fail("common_host_workload_unavailable", "The running Host could not report its work.")
        progress = view.get("Progress") or {}
        active = view["Running"] and bool(progress) and not progress.get("Done")
        for op in view.get("Operations") or ():
            if op["Status"] in ("completed", "failed", "canceled"):
                continue
            active = True
            kind = "Children" if op["Type"] == "remote_job" and op.get("ToolName") == "SubagentStart" else "Operations"
            work[kind] += 1
        work["ActiveSessions"] += int(active)
    return work


def confirm_restart(work, *, legacy=False):
    impact = (f"{work['Children']} direct child agents, {work['Operations']} other operations, "
              f"{work['ActiveSessions']} active Sessions")
    scope = " (observed on the older Host)" if legacy else ""
    try:
        answer = input(f"Restart this workspace's Host and interrupt its work? {impact}{scope}. "
                       "Child agents' descendants are affected too. [y/N] ")
    except EOFError:
        return False
    return answer.strip().lower() in ("y", "yes")


def restart_legacy_host(host, paths, session):
    # Pin the socket owner before prompting so a PID reuse cannot turn this
    # confirmation into a signal to another process.
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        pilot.fail("common_host_identity_unavailable", "Restarting this older Host requires Linux pidfd support.")
    fd = os.pidfd_open(host.pid)
    try:
        work = legacy_workload(host, paths, session)
        while confirm_restart(work, legacy=True):
            current = legacy_workload(host, paths, session)
            if current != work:
                work = current
                continue
            signal.pidfd_send_signal(fd, signal.SIGTERM)
            exited = select.poll()
            exited.register(fd, select.POLLIN)
            if not exited.poll(10_000):  # retain the existing shutdown grace period
                signal.pidfd_send_signal(fd, signal.SIGKILL)
                exited.poll()
            return True
        return False
    finally:
        os.close(fd)


def select_host(settings, paths, session, *, updated, restart, restart_command):
    installed = Path(settings["BinaryDirectory"])
    host = LiveHost(paths["socket"])
    try:
        with closing(host):
            host.connect()
            directory, bound = host.installation(paths)
    except (FileNotFoundError, ConnectionRefusedError):
        return None
    # Matching the installed directory covers both the revision and compiled
    # features. A deferred attach uses the TUI from the socket owner's build.
    if not updated and (directory is None or directory == installed) and not restart:
        return None
    legacy = False
    if bound:
        try:
            reply = host.call("host/workload" if restart else "host/restart", {}, allow_error=True)
            legacy = reply.get("Error") in ("not_found", "unknown_method")
            if legacy and restart and restart_legacy_host(host, paths, session):
                return None
            while restart and "Workload" in reply and not reply.get("Restarting"):
                work = reply["Workload"]
                if any(work.values()) and not confirm_restart(work):
                    break
                reply = host.call("host/restart", {"Workload": work}, allow_error=True)
            if reply.get("Restarting"):
                print("Updated build/settings: restarting this workspace's Host; Sessions and settings carry on.", flush=True)
                return None
            if reply.get("Error") == "stopped":
                return None  # native launcher waits for the retiring Host's socket lock
        except (OSError, ValueError, HTTPException, pilot.PilotError):
            if restart:
                raise
    elif restart:
        pilot.fail("common_host_binding_mismatch", "The running Host does not match this workspace's config and socket.")

    if updated or directory != installed:
        if legacy:
            hint = f"This Host cannot verify workspace-wide idle; run {restart_command} to switch."
        else:
            hint = f"Reopen when idle or run {restart_command} to switch now."
        print(f"Updated build/settings are ready. Keeping the running Host. {hint}", flush=True)
    return directory or installed


def read_workspace_choice(path, workspace):
    value = pilot.decode(files.read_owned(path, private=True))
    if value["Workspace"] != str(workspace):
        pilot.fail("common_workspace_binding_invalid", "Workspace choice belongs to another workspace.")
    return value


def select_repo(settings, workspace, instance, *, explicit_config=None, session=None, restart=False):
    home = Path(settings["Home"])
    def select_launch_host(paths, updated=False):
        if session is None:
            return None
        command = ["unreal", session, "--restart-host"]
        if instance is not None:
            command += ["--instance", instance]
        if explicit_config is not None:
            command += ["--config", str(paths["config"])]
        directory = select_host(settings, paths, session, updated=updated, restart=restart,
                                restart_command=shlex.join(command))
        if directory is not None:
            paths["binary_directory"] = directory
        return directory

    # Explicit operator destinations never get rebound by common defaults.
    if instance is not None or explicit_config is not None:
        paths = prepare_repo(settings, workspace, instance or "default")
        if explicit_config is not None:
            config_path = daily.safe_path(explicit_config)
            config = pilot.decode(files.read_owned(config_path, private=True))
            if not isinstance(config, dict) or not isinstance(config.get("Runtime"), dict) or config["Runtime"].get("Workspace") != str(workspace):
                pilot.fail("common_workspace_binding_mismatch", "Explicit config workspace differs; no rewrite or migration.")
            paths["config"] = config_path
            paths["explicit_config"] = True
        select_launch_host(paths)
        return paths
    paths = prepare_repo(settings, workspace, "default")
    choice_path = paths["root"] / "current.json"
    with daily.locked(paths["root"]):
        old_bytes = files.read_owned(choice_path, private=True) if os.path.lexists(choice_path) else None
        choice = read_workspace_choice(choice_path, workspace) if old_bytes is not None else None
        selected_instance = choice["Instance"] if choice else "default"
        selected = paths if selected_instance == "default" else prepare_repo(settings, workspace, selected_instance)
        profile = provider_profile(settings)
        updated = choice is not None and (choice.get("Revision") != settings["Revision"]
                                        or choice.get("BinaryDirectory") != settings["BinaryDirectory"]
                                        or choice.get("Profile") != profile)
        if select_launch_host(selected, updated) is not None or updated and session is None:
            return selected
        value = {"Version": 1, "Workspace": str(workspace), "Instance": selected_instance, "Profile": profile,
                 "Revision": settings["Revision"], "BinaryDirectory": settings["BinaryDirectory"]}
        new_bytes = (json.dumps(value, ensure_ascii=True, indent=2) + "\n").encode()
        if old_bytes is not None:
            if value != choice: daily.replace_owned(choice_path, new_bytes, old_bytes, 0o600)
        else: files.publish_bytes(choice_path, new_bytes)
        return selected


def discover_workspaces(home):
    """Read existing workspace bindings; Session contents are never inspected."""
    bindings = {}
    for root in (home / ".config" / NAMESPACE / "repos").glob("r-*"):
        for name in ("current.json", "repo.json"):
            try:
                value = pilot.decode(files.read_owned(root / name, private=True))
                workspace = Path(value["Workspace"])
                if not workspace.is_absolute():
                    continue
                bindings[root.name] = {"workspace": workspace.resolve(),
                                       "instance": value.get("Instance", "default")}
                break
            except (OSError, ValueError, TypeError, KeyError, pilot.PilotError):
                continue
    return bindings


def main_session_files(state):
    for path in (state / "sessions").glob("*.session.jsonl"):
        name = path.name[:-len(".session.jsonl")]
        if not name.startswith("child-") and re.fullmatch(r"[A-Za-z0-9-]+", name):
            yield path, name


def archive_marker(item):
    return item["state"] / f"{item['name']}.session-archived"


def discover_sessions(home, bindings=None, *, name=None):
    """Build the main Session inventory from names, mtimes and workspace metadata."""
    if bindings is None:
        bindings = discover_workspaces(home)
    sessions = []
    # Longer prefixes distinguish explicit instances from their default binding.
    keys = sorted(bindings, key=len, reverse=True)
    for state in (home / ".local/state" / NAMESPACE).glob("r-*"):
        key = next((key for key in keys if state.name == key or state.name.startswith(key + "-")), None)
        if key is None:
            continue
        binding = bindings[key]
        base_match = re.match(r"r-[0-9a-f]{16}(?=-|$)", key)
        base = base_match.group() if base_match else key
        instance = state.name[len(base) + 1:] if state.name != base else "default"
        current = bindings.get(base, binding)["instance"]
        for path, session in main_session_files(state):
            if name is not None and session != name:
                continue
            try:
                if not path.is_file() or path.is_symlink():
                    continue
                modified = path.stat().st_mtime
            except OSError:
                continue  # Sessions can disappear while another process lists them.
            item = {"name": session, "workspace": binding["workspace"], "state": state,
                    "instance": instance, "current": instance == current, "mtime": modified}
            item["archived"] = archive_marker(item).is_file()
            sessions.append(item)
    return sessions


def host_running(state):
    # The native launcher uses the same read-only readiness request.
    try:
        reply = LiveHost(state / "run/host.sock").call("auth/methods", {}, allow_error=True)
    except (OSError, ValueError, HTTPException, pilot.PilotError):
        return False
    methods = reply.get("Methods")
    return isinstance(methods, list) and bool(methods) and not reply.get("Error")


def session_rows(sessions):
    running = {}
    for item in sorted(sessions, key=lambda item: (-item["mtime"], item["workspace"], item["name"], item["instance"])):
        state = item["state"]
        if state not in running:
            running[state] = host_running(state)
        yield item, running[state]


def session_row(item, running):
    when = datetime.fromtimestamp(item["mtime"]).astimezone().strftime("%Y-%m-%d %H:%M:%S %Z")
    status = "稼働中" if running else "停止中"
    return f"{item['name']}  {item['workspace']}  {when}  Host {status}  instance={item['instance']}"


def list_sessions(settings):
    """Print every main Session, newest first, without choosing or starting one."""
    choices = [item for item in discover_sessions(Path(settings["Home"])) if not item["archived"]]
    if not choices:
        print("Session はありません。新しく作るときは unreal NAME と名前を指定してください。")
    for item, running in session_rows(choices):
        print(session_row(item, running))


def choose_session(sessions, *, by_name):
    if len(sessions) == 1 and by_name:
        return sessions[0]
    rows = list(session_rows(sessions))
    default = next((n for n, (_, running) in enumerate(rows, 1) if running), 1) if by_name else None
    if by_name:
        # Names are unique by design; several copies are leftovers to retire.
        print(f"同じ名前の Session が {len(rows)} 個あります（名前は1つの Session にだけ使います）。"
              "使わないものは unreal --archive-session NAME --workspace PATH で外してください。", file=sys.stderr)
    print("Session を選んでください:", file=sys.stderr)
    for number, (item, running) in enumerate(rows, 1):
        print(f"{number}. {session_row(item, running)}", file=sys.stderr)
    while True:
        prompt = f"番号 [{default}]: " if default is not None else "番号: "
        print(prompt, end="", file=sys.stderr, flush=True)
        try:
            answer = input().strip()
        except EOFError:
            if default is not None:
                return rows[default - 1][0]
            pilot.fail("common_session_choice_required", "端末で Session の番号を選んでください。")
        if not answer and default is not None:
            return rows[default - 1][0]
        if answer.isdecimal() and 1 <= int(answer) <= len(rows):
            return rows[int(answer) - 1][0]
        print("一覧の番号を入力してください。", file=sys.stderr)


def archive_session(settings, name, workspace, instance):
    home = Path(settings["Home"])
    workspace = daily.safe_path(Path(workspace).expanduser().resolve())
    matches = [item for item in discover_sessions(home, name=name)
               if item["workspace"] == workspace and (instance is None or item["instance"] == instance)]
    if not matches:
        pilot.fail("common_session_not_found", f"{workspace} に Session {name} が見つかりません。")
    if len(matches) > 1:
        instances = ", ".join(sorted(item["instance"] for item in matches))
        pilot.fail("common_session_archive_ambiguous", f"--instance で対象を指定してください ({instances})。")
    item = matches[0]
    pilot.private_tree(home, item["state"].relative_to(home))
    files.publish_bytes(archive_marker(item), b"archived\n")
    print(f"{name} をアーカイブしました ({workspace}, instance={item['instance']})。")


def validate_discovered_workspace(workspace):
    # Discovery changes where the launcher will run. Keep the existing Git
    # override and private-directory checks at that destination as well.
    previous = Path.cwd()
    try:
        os.chdir(workspace)
        daily.git_root()
    finally:
        os.chdir(previous)


def resolve_session(settings, args):
    """Resolve a launch or --where without writing bindings or starting a Host."""
    home = Path(settings["Home"])
    where = getattr(args, "where", None)
    name = where if where is not None else args.session
    explicit = args.instance is not None or getattr(args, "config", None) is not None
    if where is None and (explicit or args.workspace_info or args.acceptance_fixture):
        target = daily.git_root()
        if explicit and name is not None and not args.workspace_info and not args.acceptance_fixture:
            # A Session name names one Session: an explicit instance may reopen its
            # own Session, but never start a second Session under a taken name.
            instance = args.instance or "default"
            taken = [item for item in discover_sessions(home, name=name) if not item["archived"]
                     and (item["workspace"], item["instance"]) != (target, instance)]
            if taken:
                pilot.fail("common_session_name_taken",
                           f"Session {name} は {taken[0]['workspace']} にあります。同じ名前の Session は作れません。"
                           f"unreal {name} で開いてください。")
        return target, name or "default", args.instance
    bindings = discover_workspaces(home)
    cwd = Path.cwd().resolve()
    local = [item["workspace"] for item in bindings.values()
             if cwd == item["workspace"] or item["workspace"] in cwd.parents]
    workspace = max(local, key=lambda path: len(path.parts)) if local else None
    sessions = discover_sessions(home, bindings, name=name)
    matches = [item for item in sessions if not item["archived"]]
    if matches:
        chosen = choose_session(matches, by_name=name is not None)
        if where is None:
            validate_discovered_workspace(chosen["workspace"])
        return chosen["workspace"], chosen["name"], None if chosen["current"] else chosen["instance"]
    if where is not None:
        pilot.fail("common_session_not_found", f"Session が見つかりません: {where}")
    # A new Session is created only on an explicit name and a confirmation.
    if name is None:
        pilot.fail("common_session_name_required", "Session がありません。新しく作るときは unreal NAME と名前を指定してください。")
    if workspace is not None:
        validate_discovered_workspace(workspace)
    target = workspace or daily.git_root()
    if any(item["archived"] and item["workspace"] == target and item["current"] for item in sessions):
        pilot.fail("common_session_archived", f"{target} の Session {name} はアーカイブ済みです。")
    print(f"Session {name} はありません。{target} に新しく作りますか？ [y/N]: ", end="", file=sys.stderr, flush=True)
    try:
        answer = input()
    except EOFError:
        answer = ""
    if answer.strip().lower() not in ("y", "yes"):
        pilot.fail("common_session_not_created", f"Session {name} は作りませんでした。")
    return target, name, args.instance


def create_fixture():
    home = daily.home()
    base = pilot.private_tree(home, "unreal-agent-acceptance")
    workspace = Path(tempfile.mkdtemp(prefix="repair-", dir=base))
    files.publish_bytes(workspace / "AGENTS.md", b"Use workspace-relative native paths. Read before editing; never commit or push.\n")
    files.publish_bytes(workspace / "sample.txt", b"synthetic continuation line\n" * 99 + b"UNREAL_REPAIR_BEFORE\n" + b"synthetic continuation line\n" * 100)
    subprocess.run(["git", "init", "--quiet", str(workspace)], check=True,
                   env={"PATH": os.environ["PATH"], "HOME": str(home), "GIT_OPTIONAL_LOCKS": "0"})
    return workspace


def launch(args, directory):
    if args.readme:
        print(files.read_owned(directory / "README.md").decode("utf-8")); return
    if args.acceptance_fixture:
        os.chdir(create_fixture())
    settings = read_settings(daily.home())
    if settings.get("TimeZone") and not os.environ.get("TZ"):
        os.environ["TZ"] = settings["TimeZone"]
        time.tzset()
    if getattr(args, "list", False):
        list_sessions(settings); return
    if getattr(args, "archive_session", None) is not None:
        archive_session(settings, args.archive_session, args.workspace, args.instance)
        return
    workspace, session, instance = resolve_session(settings, args)
    if getattr(args, "where", None) is not None:
        print(workspace)
        return
    # Installer and launcher share the settings lock; instances keep their histories.
    with daily.locked(settings_path(daily.home()).parent):
        paths = select_repo(settings, workspace, instance, explicit_config=getattr(args, "config", None),
                            session=None if args.workspace_info else session, restart=getattr(args, "restart_host", False))
    if args.workspace_info:
        print("Workspace: " + str(workspace) + "\nConfig: " + str(paths["config"]) + "\nState: " + str(paths["state"]) + "\nSocket: " + str(paths["socket"]))
        return
    print("Unreal Agent - Quick Start\nEnter  メッセージ送信   /model  モデル変更\n/permissions  権限変更   Ctrl+D  detach\nFull Access は OS の隔離ではありません。", flush=True)
    env = dict(os.environ, XDG_CONFIG_HOME=str(paths["xdg"]))
    os.chdir(workspace)
    argv = command_for(settings, workspace, paths, session)
    os.execve(argv[0], argv, env)


def time_zone_name(value):
    try:
        ZoneInfo(value)
    except (KeyError, ValueError) as err:
        raise argparse.ArgumentTypeError("expected an IANA time zone such as Asia/Tokyo") from err
    return value


def max_active_children(value):
    try:
        limit = int(value)
    except ValueError as err:
        raise argparse.ArgumentTypeError("expected a nonnegative integer") from err
    if not 0 <= limit <= sys.maxsize:
        raise argparse.ArgumentTypeError("expected a nonnegative integer")
    return limit


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    install_args = modes.add_parser("install")
    install_args.add_argument("--configure-providers", action="store_true")
    install_args.add_argument("--time-zone", type=time_zone_name, metavar="IANA_ZONE",
                              help="Save a time zone for Unreal, for example Asia/Tokyo")
    install_args.add_argument("--max-active-children", type=max_active_children, metavar="N",
                              help="Save the maximum concurrent children per Session tree (0 is unlimited)")
    for prefix in ("openai", "anthropic"):
        install_args.add_argument("--" + prefix + "-api-key-file", default="", metavar="PATH")
        install_args.add_argument("--" + prefix + "-api-key-env", default="", metavar="NAME")
    install_args.add_argument("--opencode-go-api-key-file", default="", metavar="PATH")
    install_args.add_argument("--opencode-go-api-key-env", default="", metavar="NAME")
    install_args.add_argument("--external-codex", action="store_true")
    install_args.add_argument("--codex-auto", action="store_true")
    install_args.add_argument("--disable-codex", action="store_true")
    install_args.add_argument("--disable-claude", action="store_true")
    install_args.add_argument("--docker-sandbox-oauth", action="store_true")
    # Legacy options are accepted without an audit/trust gate or persisted declaration.
    install_args.add_argument("--policy-audited", action="store_true")
    install_args.add_argument("--trust-managed-policy", action="store_true")
    install_args.add_argument("--reject-managed-policy", action="store_true")
    install_args.add_argument("--claude-binary", default="")
    install_args.add_argument("--replace-pilot-launcher", action="store_true")
    launch_args = modes.add_parser("launch")
    launch_args.add_argument("session", nargs="?", default=None)
    session_actions = launch_args.add_mutually_exclusive_group()
    session_actions.add_argument("--where", metavar="NAME", help="Print the workspace of an existing main Session")
    session_actions.add_argument("--list", action="store_true", help="List main Sessions, newest first")
    session_actions.add_argument("--archive-session", metavar="NAME", help="Hide a Session from name lookup and lists")
    launch_args.add_argument("--workspace", metavar="PATH", help="Workspace of the Session to archive")
    launch_args.add_argument("--instance", default=None)
    launch_args.add_argument("--restart-host", action="store_true", help="Restart the workspace Host, confirming before interrupting work")
    launch_args.add_argument("--config", default=None)
    launch_args.add_argument("--readme", action="store_true")
    launch_args.add_argument("--workspace-info", action="store_true")
    launch_args.add_argument("--acceptance-fixture", action="store_true")
    args = parser.parse_args(argv)
    if args.mode == "launch":
        if (args.workspace is None) != (args.archive_session is None):
            launch_args.error("--archive-session and --workspace must be used together")
        if args.archive_session is not None and (
                args.session is not None or args.config is not None or args.restart_host
                or args.readme or args.workspace_info or args.acceptance_fixture):
            launch_args.error("--archive-session can only be combined with --workspace and --instance")
    try:
        directory = Path(__file__).resolve().parent
        if args.mode == "install": install(args, directory, package(directory))
        else: launch(args, directory)
    except (pilot.PilotError, OSError, ValueError, TypeError, HTTPException, subprocess.SubprocessError) as err:
        if isinstance(err, pilot.PilotError): print(str(err), file=sys.stderr)
        else: print(f"common launcher failed: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
