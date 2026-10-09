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
    if (not isinstance(value, dict) or set(value) != KEYS or value["Version"] != 1
            or value["Home"] != str(home) or not re.fullmatch(r"[0-9a-f]{40}", value["Revision"])
            or not re.fullmatch(r"[0-9a-f]{64}", value["CommandHash"])
            or any(type(value[k]) is not bool for k in ("DockerOAuth", "PolicyAudited", "TrustManagedPolicy"))
            or value["PolicyAudited"] != value["TrustManagedPolicy"]):
        pilot.fail("common_binding_invalid", "Keep existing configuration/history; installation binding is invalid.")
    daily.safe_path(value["BinaryDirectory"])
    if not isinstance(value["ClaudeBinary"], str) or len(value["ClaudeBinary"]) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in value["ClaudeBinary"]):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    return value


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
    if args.policy_audited != args.trust_managed_policy:
        pilot.fail("common_trust_confirmation_required", "Managed trust requires explicit policy audit AND opt-in; no Claude process started.")
    if args.claude_binary and (len(args.claude_binary) > 4096 or any(
            ord(c) < 32 or ord(c) == 127 for c in args.claude_binary)):
        pilot.fail("common_binding_invalid", "Invalid executable reference; value withheld.")
    previous = read_settings(home) if os.path.lexists(settings_path(home)) else None
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
        settings = {"Version": 1, "Home": str(home), "BinaryDirectory": str(installed), "Revision": meta["baseline"],
                    "DockerOAuth": args.docker_sandbox_oauth or bool(previous and previous["DockerOAuth"]),
                    "PolicyAudited": args.policy_audited or bool(previous and previous["PolicyAudited"]),
                    "TrustManagedPolicy": args.trust_managed_policy or bool(previous and previous["TrustManagedPolicy"]),
                    "ClaudeBinary": args.claude_binary or (previous["ClaudeBinary"] if previous else ""),
                    "CommandHash": hashlib.sha256(new_command).hexdigest()}
        new_settings = (json.dumps(settings, indent=2, ensure_ascii=True) + "\n").encode()
        old_settings = files.read_owned(settings_path(home), private=True) if previous else None
        if previous and old_settings != new_settings and not args.replace_pilot_launcher:
            pilot.fail("common_update_confirmation_required", "Use explicit --replace-pilot-launcher for a verified update; old settings/history remain intact.")
        raw, code = pilot.bounded_probe(["bash", str(directory / "install.sh")],
                                       {"HOME": str(home), "PATH": os.environ["PATH"]}, timeout=90)
        if code or package(installed) != meta:
            pilot.fail("common_install_failed", "Inspect kit doctor; existing CLI/auth/config/history unchanged.")
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
    print("PASS: common unreal installed; no Host/inference/authentication/configuration migration")
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
    if settings["DockerOAuth"]: command.append("--bootstrap-codex-docker-oauth")
    if settings["PolicyAudited"] and settings["TrustManagedPolicy"]:
        command.extend(("--bootstrap-claude-policy-audited", "--bootstrap-claude-managed-trust"))
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
            if not isinstance(config, dict) or config.get("Runtime", {}).get("Workspace") != str(workspace):
                pilot.fail("common_workspace_binding_mismatch", "Keep existing config/history; workspace changed. Use an explicit separate --instance.")
    return p


def create_fixture():
    home = daily.home()
    base = pilot.private_tree(home, "unreal-agent-acceptance")
    workspace = Path(tempfile.mkdtemp(prefix="repair-", dir=base))
    files.publish_bytes(workspace / "AGENTS.md", b"Use workspace-relative native paths. Read before editing; never commit or push.\n")
    files.publish_bytes(workspace / "sample.txt", b"synthetic continuation line\n" * 200)
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
    paths = prepare_repo(settings, workspace, args.instance)
    if args.workspace_info:
        print("Workspace: " + str(workspace) + "\nConfig: " + str(paths["config"]) + "\nState: " + str(paths["state"]) + "\nSocket: " + str(paths["socket"])); return
    print("Unreal Agent - Quick Start\nEnter  メッセージ送信   /model  モデル変更\n/permissions  権限変更   Ctrl+D  detach\n注意：Full AccessはOS隔離を提供しない。Git公開は別の承認が必要。", flush=True)
    if sys.stdin.isatty() and sys.stdout.isatty():
        print("[Enterで起動]", flush=True)
        try: input()
        except EOFError: pilot.fail("common_launch_canceled", "No explicit Enter; Host not started.")
    env = dict(os.environ, XDG_CONFIG_HOME=str(paths["xdg"]))
    os.chdir(workspace)
    os.execve(str(installed / "unreal"), command_for(settings, workspace, paths, args.session), env)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    install_args = modes.add_parser("install")
    install_args.add_argument("--docker-sandbox-oauth", action="store_true")
    install_args.add_argument("--policy-audited", action="store_true")
    install_args.add_argument("--trust-managed-policy", action="store_true")
    install_args.add_argument("--claude-binary", default="")
    install_args.add_argument("--replace-pilot-launcher", action="store_true")
    launch_args = modes.add_parser("launch")
    launch_args.add_argument("session", nargs="?", default="default")
    launch_args.add_argument("--instance", default="default")
    launch_args.add_argument("--readme", action="store_true")
    launch_args.add_argument("--workspace-info", action="store_true")
    launch_args.add_argument("--acceptance-fixture", action="store_true")
    args = parser.parse_args(argv)
    try:
        directory = Path(__file__).resolve().parent
        if args.mode == "install": install(args, directory, package(directory))
        else: launch(args, directory)
    except (pilot.PilotError, OSError, ValueError, subprocess.SubprocessError) as err:
        if isinstance(err, pilot.PilotError): print(str(err), file=sys.stderr)
        else: print("common launcher I/O failure; details withheld, previous files/history preserved", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
