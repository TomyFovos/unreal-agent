#!/usr/bin/env python3
"""Install a common wrapper; native Unreal owns discovery, Host and execution."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

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


def package(directory):
    return pilot.decode((directory / "KIT.json").read_bytes())


def read_settings(home):
    # Old v1-v3 bindings may still carry retired audit/hash fields.
    value = pilot.decode(files.read_owned(settings_path(home), private=True, limit=8192))
    daily.safe_path(value["BinaryDirectory"])
    value.setdefault("CodexRoute", "docker" if value["DockerOAuth"] else "external")
    value.setdefault("ClaudeDecision", "enabled")
    return value


def validate_status(status):
    if not isinstance(status, list) or len(status) != 2:
        pilot.fail("common_preflight_invalid", "Expected two independent safe provider statuses.")
    ids = set()
    for entry in status:
        if (not isinstance(entry, dict) or set(entry) != {"ID", "Availability", "Reason"}
                or entry["ID"] not in ("openai-codex", "claude", "claude-code") or entry["ID"] in ids
                or entry["Availability"] not in ("available", "unavailable")
                or entry["Reason"] not in PROVIDER_REASONS
                or (entry["Availability"] == "available") != (entry["Reason"] == "")):
            pilot.fail("common_preflight_invalid", "Provider preflight returned an invalid safe status; body withheld.")
        ids.add(entry["ID"])
    return status


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
    raw, code = pilot.bounded_probe(command, dict(os.environ))
    if code: pilot.fail("common_preflight_failed", "Native auth/catalog preflight failed; body withheld. No inference or fallback.")
    result = pilot.decode(raw)
    expected = {"Providers", "CodexRoute"} if settings["CodexRoute"] == "auto" else {"Providers"}
    if (not isinstance(result, dict) or set(result) != expected or not isinstance(result["Providers"], list)
            or len(result["Providers"]) != 2 or any(not isinstance(p, dict) for p in result["Providers"])):
        pilot.fail("common_preflight_invalid", "Invalid native provider status; body withheld.")
    statuses = validate_status([{"ID": p.get("ID"), "Availability": p.get("Availability"), "Reason": p.get("Reason", "")} for p in result["Providers"]])
    route = result.get("CodexRoute", settings["CodexRoute"])
    routes = ("auto", "external", "docker") if settings["CodexRoute"] == "auto" else (settings["CodexRoute"],)
    if route not in routes or (settings["CodexRoute"] == "auto" and
            (statuses[0 if statuses[0]["ID"] == "openai-codex" else 1]["Availability"] == "available") != (route != "auto")):
        pilot.fail("common_preflight_invalid", "Invalid native source decision; values withheld.")
    return statuses, route


def wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent common development; native discovery and Host.\nexec "
            + shlex.quote(str(Path(sys.executable).resolve())) + " -B "
            + shlex.quote(str(directory / "common_development.py")) + ' launch "$@"\n').encode()


def agent_wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent subagent CLI.\nexec "
            + shlex.quote(str(directory / "unreal-agent")) + ' "$@"\n').encode()


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


def install(args, directory, meta):
    home = daily.home()
    if args.claude_binary and (len(args.claude_binary) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in args.claude_binary)):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    previous = read_settings(home) if os.path.lexists(settings_path(home)) else None
    choices = provider_choices(args, previous)
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
    shells = files.inspect_shell_paths({"home": home})
    directory_config = pilot.private_tree(home, ".config/" + NAMESPACE)
    with daily.locked(directory_config):
        route, decision = choices
        settings = {"Version": 3, "Home": str(home), "BinaryDirectory": str(installed), "Revision": meta["baseline"],
                    "DockerOAuth": route == "docker", "CodexRoute": route, "ClaudeDecision": decision,
                    "ClaudeBinary": args.claude_binary or (previous["ClaudeBinary"] if previous else "")}
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
        if any(location == b"missing" for _, location in shells):
            path_script = directory_config / "path.sh"
            files.publish_bytes(path_script, b'case ":${PATH-}:" in\n *":$HOME/.local/bin:"*) ;;\n *) PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;\nesac\nexport PATH\n')
            block = ("# Unreal Agent common development PATH\n[ ! -r " + shlex.quote(str(path_script))
                     + " ] || . " + shlex.quote(str(path_script)) + "\n").encode()
            for profile in profiles:
                files.append_profile(profile, block)
        if any(kind != b"owned" or location != b"present" for kind, location in files.inspect_shell_paths({"home": home})):
            pilot.fail("common_shell_path_unavailable", "Keep installed files. Open a fresh Bash; current aliases/functions cannot be removed by this installer. Use $HOME/.local/bin/unreal explicitly.")
        prune_versions(home, installed, prior_version)
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
    if any(p["ID"] in ("claude", "claude-code") and p["Availability"] == "available" for p in statuses):
        print("Claude の操作は /permissions で選んだ権限に従います。")
    if any(p["Availability"] == "available" for p in statuses):
        print("次に: 新しい Bash で作業フォルダに移動し、unreal を起動してください。/model でモデルを選べます。")
    else:
        print("次に: 使うにはどちらかの準備（インストールとログイン）が必要です。済んだら unreal を起動してください。")
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
    command = [str(Path(settings["BinaryDirectory"]) / "unreal"),
               "--state-directory", str(paths["state"]), "--socket", str(paths["socket"])]
    if paths.get("explicit_config"): command += ["--config", str(paths["config"])]
    if settings["DockerOAuth"]: command.append("--bootstrap-codex-docker-oauth")
    if settings.get("CodexRoute") == "auto": command.append("--bootstrap-codex-auto")
    if settings.get("CodexRoute") == "disabled": command.append("--bootstrap-codex-disabled")
    if settings.get("ClaudeDecision") == "disabled": command.append("--bootstrap-claude-disabled")
    if settings["ClaudeBinary"]: command.extend(("--bootstrap-claude-binary", settings["ClaudeBinary"]))
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
            config = pilot.decode(files.read_owned(p["config"], private=True))
            if not isinstance(config, dict) or not isinstance(config.get("Runtime"), dict) or config["Runtime"].get("Workspace") != str(workspace):
                pilot.fail("common_workspace_binding_mismatch", "Keep existing config/history; workspace changed. Use an explicit separate --instance.")
    return p


def provider_profile(settings):
    # Availability is transient; only explicit choices belong to the profile.
    value = {"CodexRoute": settings.get("CodexRoute", "docker" if settings["DockerOAuth"] else "external"),
             "ClaudeDecision": settings.get("ClaudeDecision", "enabled"), "ClaudeBinary": settings["ClaudeBinary"]}
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def stop_old_host(paths):
    """Stops the instance's Host if it runs an older build: the new launcher
    starts its own build there, with the same Sessions."""
    config = str(paths["config"])
    pids = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open("/proc/" + entry + "/cmdline", "rb") as f:
                args = f.read().split(b"\0")
        except OSError:
            continue
        if b"serve" in args and os.fsencode(config) in args:
            pids.append(int(entry))
    for pid in pids:
        try: os.kill(pid, 15)
        except OSError: pass
    for _ in range(100):
        if not any(os.path.exists("/proc/%d" % pid) for pid in pids): return
        import time; time.sleep(0.1)
    for pid in pids:
        try: os.kill(pid, 9)
        except OSError: pass


def read_workspace_choice(path, workspace):
    value = pilot.decode(files.read_owned(path, private=True))
    if value["Workspace"] != str(workspace):
        pilot.fail("common_workspace_binding_invalid", "Workspace choice belongs to another workspace.")
    return value


def select_repo(settings, workspace, instance, *, explicit_config=None):
    home = Path(settings["Home"])
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
        return paths
    paths = prepare_repo(settings, workspace, "default")
    choice_path = paths["root"] / "current.json"
    with daily.locked(paths["root"]):
        old_bytes = files.read_owned(choice_path, private=True) if os.path.lexists(choice_path) else None
        choice = read_workspace_choice(choice_path, workspace) if old_bytes is not None else None
        instance = choice["Instance"] if choice else "default"
        selected = paths if instance == "default" else prepare_repo(settings, workspace, instance)
        profile = provider_profile(settings)
        if choice and (choice.get("Revision") != settings["Revision"]
                       or choice.get("BinaryDirectory") != settings["BinaryDirectory"]
                       or choice.get("Profile") != profile):
            print("Updated build/settings: restarting this workspace's Host; Sessions and settings carry on.", flush=True)
            stop_old_host(selected)
        value = {"Version": 1, "Workspace": str(workspace), "Instance": instance, "Profile": profile,
                 "Revision": settings["Revision"], "BinaryDirectory": settings["BinaryDirectory"]}
        new_bytes = (json.dumps(value, ensure_ascii=True, indent=2) + "\n").encode()
        if old_bytes is not None:
            if value != choice: daily.replace_owned(choice_path, new_bytes, old_bytes, 0o600)
        else: files.publish_bytes(choice_path, new_bytes)
        return selected


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
    workspace = daily.git_root()
    settings = read_settings(daily.home())
    installed = Path(settings["BinaryDirectory"])
    # Installer and launcher share the settings lock; instances keep their histories.
    with daily.locked(settings_path(daily.home()).parent):
        paths = select_repo(settings, workspace, args.instance, explicit_config=getattr(args, "config", None))
    if args.workspace_info:
        print("Workspace: " + str(workspace) + "\nConfig: " + str(paths["config"]) + "\nState: " + str(paths["state"]) + "\nSocket: " + str(paths["socket"]))
        return
    print("Unreal Agent - Quick Start\nEnter  メッセージ送信   /model  モデル変更\n/permissions  権限変更   Ctrl+D  detach\nFull Access は OS の隔離ではありません。", flush=True)
    env = dict(os.environ, XDG_CONFIG_HOME=str(paths["xdg"]))
    os.chdir(workspace)
    os.execve(str(installed / "unreal"), command_for(settings, workspace, paths, args.session), env)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    install_args = modes.add_parser("install")
    install_args.add_argument("--configure-providers", action="store_true")
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
    launch_args.add_argument("session", nargs="?", default="default")
    launch_args.add_argument("--instance", default=None)
    launch_args.add_argument("--config", default=None)
    launch_args.add_argument("--readme", action="store_true")
    launch_args.add_argument("--workspace-info", action="store_true")
    launch_args.add_argument("--acceptance-fixture", action="store_true")
    args = parser.parse_args(argv)
    try:
        directory = Path(__file__).resolve().parent
        if args.mode == "install": install(args, directory, package(directory))
        else: launch(args, directory)
    except (pilot.PilotError, OSError, ValueError, TypeError, subprocess.SubprocessError) as err:
        if isinstance(err, pilot.PilotError): print(str(err), file=sys.stderr)
        else: print(f"common launcher failed: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
