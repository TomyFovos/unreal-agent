#!/usr/bin/env python3
"""Install a common wrapper; native Unreal owns discovery, Host and execution."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
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
KEYS = {"Version", "Home", "BinaryDirectory", "Revision", "DockerOAuth", "PolicyAudited",
        "TrustManagedPolicy", "ClaudeBinary", "CommandHash"}
CONFIGURED_KEYS = KEYS | {"CodexRoute", "ClaudeDecision", "ProviderStatus"}
CONSENT_KEYS = CONFIGURED_KEYS | {"TrustSource"}
PROVIDER_REASONS = {"", "auth_required", "cli_missing_or_unsupported", "managed_trust_required",
                    "docker_proxy_invalid", "catalog_unavailable", "operator_disabled", "provider_check_failed"}


def settings_path(home):
    return home / ".config" / NAMESPACE / "launcher.json"


def package(directory):
    directory = daily.safe_path(directory.absolute())
    if any(p.is_symlink() for p in (directory, *directory.parents)):
        pilot.fail("common_kit_redirect", "Use an extracted verified kit without symlink redirects.")
    meta = pilot.decode(files.read_owned(directory / "KIT.json", limit=4096))
    snap = pilot.decode(files.read_owned(directory / "SNAPSHOT.json", limit=4096))
    if (not isinstance(meta, dict) or meta.get("kind") != "common-development-kit"
            or "sandbox" in meta or not re.fullmatch(r"[0-9a-f]{40}", meta.get("baseline", ""))
            or not isinstance(snap, dict) or snap.get("revision") != meta["baseline"]
            or any(m.get("target") != "linux/amd64" or m.get("cgo_enabled") is not True
                   or m.get("formal_release") is not False for m in (meta, snap))
            or snap.get("kind") != "local-snapshot" or meta.get("goamd64") != "v1"):
        pilot.fail("common_kit_invalid", "Verified Linux amd64 CGO snapshot provenance is required.")
    sums = b"".join(hashlib.sha256(files.read_owned(directory / name, limit=200 << 20)).hexdigest().encode()
                    + b"  " + name.encode() + b"\n" for name in MEMBERS)
    if files.read_owned(directory / "SHA256SUMS", limit=4096) != sums:
        pilot.fail("common_kit_checksum", "Exact kit inventory/checksums failed; previous installs preserved.")
    return meta


def read_settings(home):
    value = pilot.decode(files.read_owned(settings_path(home), private=True, limit=8192))
    if (not isinstance(value, dict) or (value.get("Version"), frozenset(value)) not in
            ((1, frozenset(KEYS)), (2, frozenset(CONFIGURED_KEYS)), (3, frozenset(CONSENT_KEYS)))
            or value["Home"] != str(home) or not re.fullmatch(r"[0-9a-f]{40}", value["Revision"])
            or not re.fullmatch(r"[0-9a-f]{64}", value["CommandHash"])
            or any(type(value[k]) is not bool for k in ("DockerOAuth", "PolicyAudited", "TrustManagedPolicy"))
            or value["Version"] < 3 and value["TrustManagedPolicy"] and not value["PolicyAudited"]
            or value["Version"] == 1 and value["PolicyAudited"] != value["TrustManagedPolicy"]):
        pilot.fail("common_binding_invalid", "Keep existing configuration/history; installation binding is invalid.")
    daily.safe_path(value["BinaryDirectory"])
    if not isinstance(value["ClaudeBinary"], str) or len(value["ClaudeBinary"]) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in value["ClaudeBinary"]):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    if value["Version"] >= 2:
        routes = ("external", "docker", "disabled", "auto") if value["Version"] == 3 else ("external", "docker", "disabled")
        if (value["CodexRoute"] not in routes
                or value["ClaudeDecision"] not in ("enabled", "disabled")
                or value["DockerOAuth"] != (value["CodexRoute"] == "docker")
                or value["ClaudeDecision"] == "disabled" and (value["PolicyAudited"] or value["TrustManagedPolicy"])):
            pilot.fail("common_binding_invalid", "Provider choices are inconsistent; values withheld.")
        validate_status(value["ProviderStatus"])
    if value["Version"] == 3:
        if (value["TrustSource"] not in ("", "install-consent", "legacy-opt-in")
                or bool(value["TrustSource"]) != value["TrustManagedPolicy"]
                or value["TrustSource"] == "install-consent" and value["PolicyAudited"]):
            pilot.fail("common_binding_invalid", "Trust provenance is invalid; installation does not prove a policy audit.")
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
    if (len(route_flags) > 1 or getattr(args, "disable_claude", False) and (args.policy_audited or args.trust_managed_policy)
            or getattr(args, "reject_managed_policy", False) and (args.policy_audited or args.trust_managed_policy)):
        pilot.fail("common_provider_conflict", "Conflicting explicit provider choices; no probe or installation started.")
    if args.policy_audited and not args.trust_managed_policy:
        pilot.fail("common_trust_confirmation_required", "An audit assertion alone is not trust consent; no process started.")
    # Preserve explicit old routes/reject choices. Only native discovery may
    # resolve a NEW automatic route; Python never parses credentials or proxy.
    route = {"docker_sandbox_oauth": "docker", "external_codex": "external", "disable_codex": "disabled", "codex_auto": "auto"}.get(route_flags[0]) if route_flags else (
        previous.get("CodexRoute", "docker" if previous["DockerOAuth"] else "external") if previous else "auto")
    decision = previous.get("ClaudeDecision", "enabled") if previous else "enabled"
    if getattr(args, "configure_providers", False): decision = "enabled"
    audited = previous["PolicyAudited"] if previous else False
    trust = previous["TrustManagedPolicy"] if previous else True
    source = previous.get("TrustSource", "legacy-opt-in" if trust else "") if previous else "install-consent"
    if args.trust_managed_policy:
        decision, audited, trust = "enabled", args.policy_audited, True
        source = "legacy-opt-in" if audited else "install-consent"
    if getattr(args, "reject_managed_policy", False): audited, trust, source = False, False, ""
    if getattr(args, "disable_claude", False): decision, audited, trust, source = "disabled", False, False, ""
    return route, decision, audited, trust, source


def preflight(settings):
    command = [str(Path(settings["BinaryDirectory"]) / "unreal-agent"), "providers"]
    if settings["DockerOAuth"]: command.append("--docker-sandbox-oauth")
    if settings["CodexRoute"] == "auto": command.append("--codex-auto")
    if settings["CodexRoute"] == "disabled": command.append("--codex-disabled")
    if settings["ClaudeDecision"] == "disabled": command.append("--claude-disabled")
    if settings["ClaudeBinary"]: command += ["--claude-binary", settings["ClaudeBinary"]]
    raw, code = pilot.bounded_probe(command, dict(os.environ), timeout=60)
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


def owned_legacy(home, value):
    # Opt-in still requires the exact old checksum-bound wrapper, not a comment.
    for sandbox in ("asp", "devlore"):
        path = daily.settings_path(home, sandbox)
        previous = daily.read_settings(home, sandbox) if os.path.lexists(path) else None
        if previous and daily.owned_previous_command(home, sandbox, value, previous):
            return True
        if sandbox == "devlore" and os.path.lexists(home / ".config/unreal-agent-pilot/devlore/launcher-v1/launcher.json"):
            if daily.owned_previous_command(home, sandbox, value, None):
                return True
    return False


def install(args, directory, meta):
    home = daily.home()
    if args.claude_binary and (len(args.claude_binary) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in args.claude_binary)):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    previous = read_settings(home) if os.path.lexists(settings_path(home)) else None
    choices = provider_choices(args, previous)
    content_id = hashlib.sha256(files.read_owned(directory / "SHA256SUMS", limit=4096)).hexdigest()[:16]
    installed = home / ".local/opt/unreal-agent-pilot/common" / (meta["baseline"][:7] + "-" + content_id)
    command, new_command = home / ".local/bin/unreal", wrapper(installed)
    old = files.read_owned(command, private=True) if os.path.lexists(command) else None
    if old is not None and old != new_command:
        owned = previous and hashlib.sha256(old).hexdigest() == previous["CommandHash"] and old == wrapper(Path(previous["BinaryDirectory"]))
        if not args.replace_pilot_launcher or not (owned or owned_legacy(home, old)):
            pilot.fail("common_command_conflict", "Existing unreal preserved. --replace-pilot-launcher permits only a verified owned wrapper; arbitrary commands are never replaced.")
    profiles = files.shell_profiles(home)
    shells = files.inspect_shell_paths({"home": home})
    directory_config = pilot.private_tree(home, ".config/" + NAMESPACE)
    with daily.locked(directory_config):
        if previous != (read_settings(home) if os.path.lexists(settings_path(home)) else None):
            pilot.fail("common_update_conflict", "Installation changed concurrently; nothing replaced.")
        route, decision, audited, trust, source = choices
        settings = {"Version": 3, "Home": str(home), "BinaryDirectory": str(installed), "Revision": meta["baseline"],
                    "DockerOAuth": route == "docker", "CodexRoute": route, "ClaudeDecision": decision,
                    "PolicyAudited": audited, "TrustManagedPolicy": trust, "TrustSource": source,
                    "ClaudeBinary": args.claude_binary or (previous["ClaudeBinary"] if previous else ""),
                    "CommandHash": hashlib.sha256(new_command).hexdigest()}
        old_settings = files.read_owned(settings_path(home), private=True) if previous else None
        if previous and (provider_profile(settings) != provider_profile(previous) or old != new_command) and not args.replace_pilot_launcher:
            pilot.fail("common_update_confirmation_required", "Use explicit --replace-pilot-launcher for a verified update; old settings/history remain intact.")
        raw, code = pilot.bounded_probe(["bash", str(directory / "install.sh")],
                                       {"HOME": str(home), "PATH": os.environ["PATH"]}, timeout=90)
        if code or package(installed) != meta:
            pilot.fail("common_install_failed", "Inspect kit doctor; existing CLI/auth/config/history unchanged.")
        settings["ProviderStatus"], resolved = preflight(settings)
        if previous is None and route == "auto":
            settings.update(CodexRoute=resolved, DockerOAuth=resolved == "docker")
        new_settings = (json.dumps(settings, indent=2, ensure_ascii=True) + "\n").encode()
        if old_settings is not None and old_settings != new_settings or old is not None and old != new_command:
            backups = pilot.private_tree(home, ".config/" + NAMESPACE + "/backups")
            for data, suffix in ((old, ".sh"), (old_settings, ".json")):
                if data is not None:
                    files.publish_bytes(backups / (hashlib.sha256(data).hexdigest() + suffix), data, 0o600)
        files.command_directory(home)
        if old is not None and old != new_command:
            daily.replace_owned(command, new_command, old, 0o700)
        else:
            files.publish_bytes(command, new_command, 0o700)
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
    print("PASS: common unreal installed; no Host/inference/credential change or Session migration")
    for status in settings["ProviderStatus"]:
        print(status["ID"] + ": " + status["Availability"] + (" (" + status["Reason"] + ")" if status["Reason"] else ""))
    if settings["ClaudeDecision"] != "disabled":
        print("Claude: your signed-in Claude Code runs its own tools; Unreal approves each call by its /permissions mode (Read Only until you choose).")
        print("Commands Claude Code's organization policy refuses are run by Unreal's own shell under that mode.")
    print("NEXT: open a fresh Bash, cd to a Git worktree, run unreal [SESSION]; select provider/model/effort in /model")
    print("NOTE: current shell aliases/functions may still mask unreal; type -a unreal or use $HOME/.local/bin/unreal")


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
    # Availability may change without operator consent. Only wanted security
    # choices and the build identity choose an instance, never cached status.
    value = {k: settings[k] for k in ("Revision", "DockerOAuth", "PolicyAudited", "TrustManagedPolicy", "ClaudeBinary")}
    value.update(CodexRoute=settings.get("CodexRoute", "docker" if settings["DockerOAuth"] else "external"),
                 ClaudeDecision=settings.get("ClaudeDecision", "enabled"))
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=True).encode()).hexdigest()


def read_workspace_choice(path, workspace):
    value = pilot.decode(files.read_owned(path, private=True, limit=16384))
    if (not isinstance(value, dict) or set(value) != {"Version", "Workspace", "Profile", "Instance", "Previous", "Revision", "BinaryDirectory", "ConfigHash"}
            or value["Version"] != 1 or value["Workspace"] != str(workspace)
            or not re.fullmatch(r"[0-9a-f]{40}", value["Revision"])
            or not re.fullmatch(r"[0-9a-f]{64}", value["Profile"])
            or value["ConfigHash"] != "" and not re.fullmatch(r"[0-9a-f]{64}", value["ConfigHash"])
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}", value["Instance"])
            or not isinstance(value["Previous"], list) or len(value["Previous"]) > 32):
        pilot.fail("common_workspace_binding_invalid", "Workspace choice is invalid; no prior Host/config/history changed.")
    daily.safe_path(value["BinaryDirectory"])
    for old in value["Previous"]:
        if (not isinstance(old, dict) or set(old) != {"Instance", "ConfigHash", "Revision", "BinaryDirectory"}
                or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}", old["Instance"])
                or not re.fullmatch(r"[0-9a-f]{64}", old["ConfigHash"])
                or old["Revision"] != "" and not re.fullmatch(r"[0-9a-f]{40}", old["Revision"])):
            pilot.fail("common_workspace_binding_invalid", "Invalid previous-instance reference; value withheld.")
        if old["BinaryDirectory"] != "": daily.safe_path(old["BinaryDirectory"])
    return value


def effective_profile_matches(config, settings):
    if not isinstance(config, dict): return False
    launcher = config.get("Launcher", {})
    runtime = config.get("Runtime", {})
    if not isinstance(launcher, dict) or not isinstance(runtime, dict): return False
    if (launcher.get("Version") != 1 or launcher.get("DiscoverProviders") is False
            or bool(launcher.get("CodexDockerSandboxOAuth")) != settings["DockerOAuth"]
            or bool(launcher.get("CodexAuto")) != (settings.get("CodexRoute") == "auto")
            or bool(launcher.get("CodexDisabled")) != (settings.get("CodexRoute") == "disabled")
            or bool(launcher.get("ClaudeDisabled")) != (settings.get("ClaudeDecision") == "disabled")):
        return False
    provider = runtime.get("Provider", {})
    backends = config.get("Providers", {})
    if not isinstance(provider, dict) or not isinstance(backends, dict): return False
    # The Claude route keeps only an explicit Claude Code executable.
    return (launcher.get("ClaudeBinary") or "") == settings["ClaudeBinary"]


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
        profile = provider_profile(settings)
        if choice and choice["Profile"] == profile:
            selected = paths if choice["Instance"] == "default" else prepare_repo(settings, workspace, choice["Instance"])
            if not os.path.lexists(selected["config"]): return selected
            data = files.read_owned(selected["config"], private=True)
            digest = hashlib.sha256(data).hexdigest()
            if (choice["ConfigHash"] in ("", digest) and effective_profile_matches(pilot.decode(data), settings)):
                if choice["ConfigHash"] == "":
                    choice["ConfigHash"] = digest
                    daily.replace_owned(choice_path, (json.dumps(choice, ensure_ascii=True, indent=2) + "\n").encode(), old_bytes, 0o600)
                return selected
        # New untouched workspace has no prior identity to migrate. Existing
        # config (including mono-provider/precedence variants) stays immutable.
        if choice is None and not os.path.lexists(paths["config"]):
            instance, previous = "default", []
        else:
            old_instance = choice["Instance"] if choice else "default"
            old_paths = repo_paths(home, workspace, old_instance)
            config_data = files.read_owned(old_paths["config"], private=True) if os.path.lexists(old_paths["config"]) else b""
            # The old instance stays as it was, so starting the new one needs
            # no question (U8); the old one remains one command away.
            print("Provider/build settings changed for this workspace: starting a new instance. Old Host and history are kept.")
            print("Old config: " + str(old_paths["config"]))
            print("Reconnect to the old instance: unreal --instance " + old_instance + " SESSION (matching old binary may be required)")
            instance = "p-" + hashlib.sha256((profile + hashlib.sha256(config_data).hexdigest()).encode()).hexdigest()[:12]
            previous = list(choice["Previous"]) if choice else []
            reference = {"Instance": old_instance, "ConfigHash": hashlib.sha256(config_data).hexdigest(),
                         "Revision": choice["Revision"] if choice else "", "BinaryDirectory": choice["BinaryDirectory"] if choice else ""}
            if reference not in previous: previous.append(reference)
            if len(previous) > 32:
                pilot.fail("common_workspace_history_limit", "Previous instance references retained; choose an explicit instance.")
        value = {"Version": 1, "Workspace": str(workspace), "Profile": profile, "Instance": instance, "Previous": previous,
                 "Revision": settings["Revision"], "BinaryDirectory": settings["BinaryDirectory"], "ConfigHash": ""}
        new_bytes = (json.dumps(value, ensure_ascii=True, indent=2) + "\n").encode()
        selected = paths if instance == "default" else prepare_repo(settings, workspace, instance)
        if os.path.lexists(selected["config"]) and not effective_profile_matches(pilot.decode(files.read_owned(selected["config"], private=True)), settings):
            pilot.fail("common_profile_instance_conflict", "Existing instance has a different effective provider profile; no overwrite or false update.")
        if old_bytes is not None: daily.replace_owned(choice_path, new_bytes, old_bytes, 0o600)
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
    meta = package(installed)
    if meta["baseline"] != settings["Revision"]:
        pilot.fail("common_binary_binding_mismatch", "Installation revision mismatch; no Host started.")
    # Installer and launcher share the settings lock. A profile change starts
    # a separate instance and leaves the old one intact.
    with daily.locked(settings_path(daily.home()).parent):
        if settings != read_settings(daily.home()): pilot.fail("common_update_conflict", "Provider setup changed concurrently; retry without altering old Sessions.")
        paths = select_repo(settings, workspace, args.instance, explicit_config=getattr(args, "config", None))
    if args.workspace_info:
        print("Workspace: " + str(workspace) + "\nConfig: " + str(paths["config"]) + "\nState: " + str(paths["state"]) + "\nSocket: " + str(paths["socket"]))
        current = repo_paths(daily.home(), workspace)["root"] / "current.json"
        if os.path.lexists(current):
            for old in read_workspace_choice(current, workspace)["Previous"]:
                print("Preserved: unreal --instance " + old["Instance"] + " SESSION; config=" + str(repo_paths(daily.home(), workspace, old["Instance"])["config"]))
        return
    print("Unreal Agent - Quick Start\nEnter  メッセージ送信   /model  モデル変更\n/permissions  権限変更   Ctrl+D  detach\n注意：Full AccessはOS隔離を提供しない。Git公開は別の承認が必要。", flush=True)
    env = dict(os.environ, XDG_CONFIG_HOME=str(paths["xdg"]))
    with daily.locked(settings_path(daily.home()).parent):
        if settings != read_settings(daily.home()): pilot.fail("common_update_conflict", "Setup changed concurrently; no Host started. Run unreal again.")
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
        else: print("common launcher I/O failure; details withheld, previous files/history preserved", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
