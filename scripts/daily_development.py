#!/usr/bin/env python3
"""Bind a Git root to the existing native launcher; never submit model requests."""

import argparse
import contextlib
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys
import tempfile
import unicodedata

import asp_oauth as asp
import devlore_launcher as files
import permission_acceptance as gate
import pilot_provider as pilot


NAMESPACE = "unreal-agent-dev"
EXTRAS = ("asp_oauth.py", "daily.sh", "daily_development.py", "devlore_launcher.py",
          "permission_acceptance.py", "pilot_provider.py")
MEMBERS = (*asp.pilot_binaries(), "LICENSE", "SNAPSHOT.json", "KIT.json", "README.md",
           "install.sh", "doctor.sh", *EXTRAS)
SETTINGS_KEYS = {"Version", "Sandbox", "Home", "BinaryDirectory", "Revision", "Model", "Effort",
                 "CodexFile", "ClaudeBinary", "PolicyAudited", "TrustManagedPolicy", "CommandHash"}


def safe_path(path):
    value = os.fspath(path)
    if (not Path(value).is_absolute() or len(os.fsencode(value)) > 4096
            or any(unicodedata.category(c) in ("Cc", "Cf", "Cs", "Zl", "Zp") for c in value)):
        pilot.fail("development_path_invalid", "Use an absolute repository/user path without control characters; values withheld.")
    return Path(value)


def home():
    result = safe_path(os.environ.get("HOME", ""))
    if not result.is_dir():
        pilot.fail("development_home_invalid", "An existing user HOME is required; no configuration changed.")
    return result.resolve()


def package(directory):
    directory = safe_path(directory.absolute())
    if any(p.is_symlink() for p in (directory, *directory.parents)):
        pilot.fail("development_kit_redirect", "Use the verified non-redirected daily development kit.")
    meta = pilot.decode(files.read_owned(directory / "KIT.json", limit=4096))
    snap = pilot.decode(files.read_owned(directory / "SNAPSHOT.json", limit=4096))
    if (not isinstance(meta, dict) or meta.get("kind") != "daily-development-kit"
            or meta.get("sandbox") not in pilot.PROVIDERS
            or not re.fullmatch(r"[0-9a-f]{40}", meta.get("baseline", ""))
            or not isinstance(snap, dict) or snap.get("revision") != meta["baseline"]
            or snap.get("kind") != "local-snapshot" or snap.get("formal_release") is not False
            or any(m.get("target") != "linux/amd64" or m.get("cgo_enabled") is not True for m in (meta, snap))
            or meta.get("goamd64") != "v1"):
        pilot.fail("development_kit_invalid", "Daily development Linux amd64 CGO provenance is required.")
    sums = b"".join(hashlib.sha256(files.read_owned(directory / name, limit=200 << 20)).hexdigest().encode()
                    + b"  " + name.encode() + b"\n" for name in MEMBERS)
    if files.read_owned(directory / "SHA256SUMS", limit=4096) != sums:
        pilot.fail("development_kit_checksum", "Exact kit inventory/checksums failed; existing installs preserved.")
    _, code = pilot.bounded_probe(["bash", str(directory / "doctor.sh"), "--preflight"],
                                 {"HOME": os.environ["HOME"], "PATH": os.environ["PATH"]}, timeout=30)
    if code:
        pilot.fail("development_kit_preflight", "Linux amd64/glibc/library preflight failed; no packages changed.")
    return directory, meta


def settings_path(user_home, sandbox):
    return user_home / ".config" / NAMESPACE / sandbox / "launcher.json"


def read_settings(user_home, sandbox):
    value = pilot.decode(files.read_owned(settings_path(user_home, sandbox), private=True, limit=8192))
    if (not isinstance(value, dict) or set(value) != SETTINGS_KEYS or value["Version"] != 1
            or value["Home"] != str(user_home) or value["Sandbox"] != sandbox
            or not re.fullmatch(r"[0-9a-f]{40}", value["Revision"])
            or not re.fullmatch(r"[0-9a-f]{64}", value["CommandHash"])
            or type(value["PolicyAudited"]) is not bool or type(value["TrustManagedPolicy"]) is not bool
            or (sandbox == "devlore" and not (value["PolicyAudited"] and value["TrustManagedPolicy"]))):
        pilot.fail("development_binding_invalid", "Keep the existing configuration/history; install binding is invalid.")
    for key in ("BinaryDirectory", "CodexFile" if sandbox == "asp" else "ClaudeBinary"):
        safe_path(value[key])
    pilot.configuration(sandbox, user_home, value["Model"], value["Effort"], value["ClaudeBinary"],
                        value["PolicyAudited"], value["TrustManagedPolicy"])
    return value


def preflight(directory, user_home, sandbox, *, binary="claude", audited=False, trust=False):
    with contextlib.redirect_stdout(io.StringIO()):
        if sandbox == "asp":
            return asp.preflight(directory, user_home, os.environ), ""
        return "", pilot.claude_preflight(binary, user_home, os.environ, audited, trust)


def wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent daily development; native Host owns Session lifecycle.\nexec "
            + shlex.quote(str(Path(sys.executable).resolve())) + " -B "
            + shlex.quote(str(directory / "daily_development.py")) + ' launch "$@"\n').encode()


def owned_previous_command(user_home, sandbox, command, previous):
    """Only a checksum-bound daily wrapper or the exact old devlore wrapper."""
    if previous:
        return (hashlib.sha256(command).hexdigest() == previous["CommandHash"]
                and command == wrapper(Path(previous["BinaryDirectory"])))
    if sandbox != "devlore":
        return False
    old_paths = user_home / ".config/unreal-agent-pilot/devlore/launcher-v1"
    old = pilot.decode(files.read_owned(old_paths / "launcher.json", private=True, limit=4096))
    if (not isinstance(old, dict) or old.get("Version") != 1 or old.get("Home") != str(user_home)
            or old.get("BinaryRevision") != pilot.BASELINE or old.get("PolicyAudited") is not True):
        return False
    for code_dir in (user_home / ".local/opt/unreal-agent-pilot/devlore").glob("launcher-v1-*"):
        content_id = files.validate_package(code_dir)
        if code_dir.name != "launcher-v1-" + content_id:
            continue
        expected = ("#!/bin/bash\n# Unreal Agent devlore permanent launcher; no shell evaluation.\nexec "
                    + shlex.quote(str(Path(sys.executable).resolve())) + " "
                    + shlex.quote(str(code_dir / "devlore_launcher.py")) + ' launch "$@"\n').encode()
        if command == expected:
            return True
    return False


@contextlib.contextmanager
def locked(directory):
    fd = os.open(directory / ".development.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & 0o077 or info.st_nlink != 1):
            pilot.fail("development_lock_unsafe", "Existing lock is preserved.")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def replace_owned(path, new_value, previous_value, mode):
    # Used only for an explicitly approved, verified launcher update. Preserve
    # the old bytes in a private content-addressed backup before publication.
    if files.read_owned(path, private=True) != previous_value:
        pilot.fail("development_update_conflict", "Launcher changed concurrently; nothing replaced.")
    fd, temporary = tempfile.mkstemp(prefix=".development-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(new_value); stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        Path(temporary).unlink(missing_ok=True)


def install(args, directory, meta):
    user_home, sandbox = home(), meta["sandbox"]
    config_dir = settings_path(user_home, sandbox).parent
    previous = read_settings(user_home, sandbox) if os.path.lexists(config_dir / "launcher.json") else None
    model = args.model or (previous["Model"] if previous else "gpt-6.1-sol" if sandbox == "asp" else "opus")
    effort = args.effort or (previous["Effort"] if previous else "high")
    binary_choice = args.claude_binary or (previous["ClaudeBinary"] if previous and sandbox == "devlore" else "claude")
    pilot.configuration(sandbox, user_home, model, effort, binary_choice,
                        args.policy_audited, args.trust_managed_policy)
    identity = hashlib.sha256(files.read_owned(directory / "SHA256SUMS", limit=4096)).hexdigest()[:16]
    installed = user_home / ".local/opt/unreal-agent-pilot" / sandbox / (meta["baseline"][:7] + "-" + identity)
    new_command = wrapper(installed)
    command_path = user_home / ".local/bin/unreal"
    if command_path.is_symlink():
        pilot.fail("development_command_conflict", "Existing unreal redirect is preserved; no command overwritten.")
    old_command = files.read_owned(command_path, private=True) if os.path.lexists(command_path) else None
    if old_command is not None and old_command != new_command:
        if not args.replace_pilot_launcher or not owned_previous_command(user_home, sandbox, old_command, previous):
            pilot.fail("development_command_conflict", "Existing unreal is preserved. Only --replace-pilot-launcher can replace an exactly verified owned Pilot wrapper; arbitrary commands are never overwritten.")
    profiles = files.shell_profiles(user_home)
    shell_paths = files.inspect_shell_paths({"home": user_home})
    auth, binary = preflight(directory, user_home, sandbox, binary=binary_choice,
                             audited=args.policy_audited, trust=args.trust_managed_policy)
    pilot.private_tree(user_home, config_dir.relative_to(user_home))
    with locked(config_dir):
        if previous != (read_settings(user_home, sandbox) if os.path.lexists(config_dir / "launcher.json") else None):
            pilot.fail("development_update_conflict", "Install binding changed concurrently; preserve existing files.")
        # The existing content-addressed installer owns binary installation.
        _, code = pilot.bounded_probe(["bash", str(directory / "install.sh")],
                                     {"HOME": str(user_home), "PATH": os.environ["PATH"]}, timeout=90)
        if code:
            pilot.fail("development_install_failed", "Existing installations preserved; inspect the kit doctor.")
        _, installed_meta = package(installed)
        if installed_meta != meta:
            pilot.fail("development_install_mismatch", "Installed kit provenance differs; no configuration/command changed.")
        settings = {"Version": 1, "Sandbox": sandbox, "Home": str(user_home), "BinaryDirectory": str(installed),
                    "Revision": meta["baseline"], "Model": model, "Effort": effort, "CodexFile": auth,
                    "ClaudeBinary": binary, "PolicyAudited": args.policy_audited,
                    "TrustManagedPolicy": args.trust_managed_policy, "CommandHash": hashlib.sha256(new_command).hexdigest()}
        old_settings = files.read_owned(config_dir / "launcher.json", private=True) if previous else None
        settings_bytes = (json.dumps(settings, ensure_ascii=True, indent=2) + "\n").encode()
        changed = old_settings is not None and old_settings != settings_bytes
        if changed and not args.replace_pilot_launcher:
            pilot.fail("development_update_confirmation_required", "Use --replace-pilot-launcher for a verified owned update; existing repo configs/Sessions remain unchanged.")
        if old_command is not None and old_command != new_command or changed:
            backups = pilot.private_tree(user_home, (config_dir / "backups").relative_to(user_home))
            if old_command is not None:
                files.publish_bytes(backups / (hashlib.sha256(old_command).hexdigest() + ".sh"), old_command, 0o700)
            if old_settings is not None:
                files.publish_bytes(backups / (hashlib.sha256(old_settings).hexdigest() + ".json"), old_settings)
        files.command_directory(user_home)
        if old_command is None or old_command == new_command:
            files.publish_bytes(command_path, new_command, 0o700)
        else:
            replace_owned(command_path, new_command, old_command, 0o700)
        if changed:
            replace_owned(config_dir / "launcher.json", settings_bytes, old_settings, 0o600)
        else:
            files.publish_bytes(config_dir / "launcher.json", settings_bytes)
        if any(location == b"missing" for _, location in shell_paths):
            path_script = config_dir / "path.sh"
            files.publish_bytes(path_script, b'case ":${PATH-}:" in\n  *":$HOME/.local/bin:"*) ;;\n  *) PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;\nesac\nexport PATH\n')
            block = ("# Unreal Agent daily development PATH\n[ ! -r " + shlex.quote(str(path_script))
                     + " ] || . " + shlex.quote(str(path_script)) + "\n").encode()
            for profile in profiles:
                files.append_profile(profile, block)
        if any(kind != b"owned" or location != b"present" for kind, location in files.inspect_shell_paths({"home": user_home})):
            pilot.fail("development_shell_path_unavailable", "Existing Bash startup prevents discovery; no startup file is replaced.")
    print("PASS: daily unreal installed; new interactive/login Bash verified; no Host or inference started")
    print("NEXT: cd to your Git repository, then unreal or unreal SESSION_NAME (initial Read Only)")
    print("PRESERVED: Gate D/E, default config/history, external CLI/auth; Full Access still requires human confirmation")


def git_root():
    # Do not let inherited Git plumbing redirect the workspace to another repo.
    if any(os.environ.get(key) for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE")):
        pilot.fail("development_git_override", "Use a shell without Git workspace overrides; no repository changed.")
    cwd = safe_path(Path.cwd()).resolve()
    env = {"PATH": os.environ["PATH"], "HOME": os.environ["HOME"], "LANG": "C.UTF-8", "GIT_OPTIONAL_LOCKS": "0"}
    raw, code = pilot.bounded_probe(["git", "-C", str(cwd), "rev-parse", "--show-toplevel"], env, limit=8192)
    if code or not raw.endswith(b"\n"):
        pilot.fail("development_git_repository_required", "cd to the Git repository you want to develop, then run unreal SESSION_NAME; ~/workspace need not be a repository. No project is created or changed.")
    root = safe_path(os.fsdecode(raw[:-1])).resolve()
    if not root.is_dir() or cwd != root and root not in cwd.parents:
        pilot.fail("development_workspace_mismatch", "Git root does not contain this directory; no Host started.")
    user_home = home()
    # Metadata-only exclusion of credential/config/install trees and ancestors.
    protected = [user_home / name for name in (".codex", ".claude", ".config", ".local/state", ".local/opt", ".local/bin")]
    for key in ("CODEX_HOME", "CLAUDE_CONFIG_DIR", "OPENAI_CODEX_AUTH_FILE"):
        if os.environ.get(key):
            protected.append(safe_path(os.environ[key]).resolve())
    if root == user_home or root in user_home.parents or any(root == p or root in p.parents or p in root.parents for p in protected):
        pilot.fail("development_workspace_private", "Choose a project repository outside credential/configuration/Host storage; no filesystem grant made.")
    return root


def repo_paths(user_home, sandbox, workspace):
    identity = hashlib.sha256(os.fsencode(workspace)).hexdigest()
    key = "r-" + identity[:16]
    config_dir = settings_path(user_home, sandbox).parent / "repos" / key
    state = user_home / ".local/state" / NAMESPACE / sandbox / key
    socket = state / "run/host.sock"
    if len(os.fsencode(socket)) > 100:
        pilot.fail("development_socket_too_long", "User HOME is too long for a private Unix socket; no default socket reused.")
    return {"home": user_home, "workspace": workspace, "identity": identity, "config_dir": config_dir,
            "config": config_dir / "runtime.json", "binding": config_dir / "repo.json",
            "state": state, "sessions": state / "sessions", "socket": socket}


def configuration(sandbox, workspace, model, effort, binary="", *, audited=False, trust=False):
    # Reuse the reviewed Read Only base and human approval ceiling. Enabling
    # modes is not an approval and does not override any later operator deny.
    cfg = gate.configuration(sandbox, workspace, model, effort, binary, audited=audited, trust=trust)
    cfg["Runtime"]["SystemPrompt"] = ("Develop only the selected Git repository workspace. Unreal owns validation, permissions, canonical Operations and receipts. "
        "Native file tools require workspace-relative paths and retain their immutable workspace boundary even in Full Access. "
        "Read before edit; use the exact whole-file revision. For new write use expected:{exists:false} without a digest; its parent must exist. "
        "Observe actual tool results before reporting success. Permission approval belongs to the human, never the model. "
        "Full Access means ambient filesystem/network/process access, not OS isolation; it is not Git push/commit/publication authorization. "
        "Do not commit, push, open PRs, merge, tag or release without explicit user instructions.")
    cfg["Runtime"]["Provider"]["source"] = "explicit daily development operator selection"
    if sandbox == "devlore":
        cfg["Runtime"]["ClaudeCode"]["Models"][0]["Name"] = "Configured daily model"
    return cfg


def prepare_repo(settings, workspace):
    p = repo_paths(Path(settings["Home"]), settings["Sandbox"], workspace)
    expected = {"Version": 1, "Workspace": str(workspace), "Identity": p["identity"],
                "Sandbox": settings["Sandbox"], "CodexFile": settings["CodexFile"], "ClaudeBinary": settings["ClaudeBinary"]}
    for directory in (p["config_dir"], p["socket"].parent):
        pilot.private_tree(p["home"], directory.relative_to(p["home"]))
    with locked(p["config_dir"]):
        existing = os.path.lexists(p["binding"]) or os.path.lexists(p["config"])
        if existing:
            if pilot.decode(files.read_owned(p["binding"], private=True, limit=8192)) != expected:
                pilot.fail("development_repo_binding_mismatch", "Repo identity/auth reference changed or hash collision; existing history/configuration preserved.")
            cfg = pilot.decode(files.read_owned(p["config"], private=True))
            if (not isinstance(cfg, dict) or cfg.get("Runtime", {}).get("Workspace") != str(workspace)
                    or cfg["Runtime"].get("Provider", {}).get("provider") != pilot.PROVIDERS[settings["Sandbox"]]
                    or cfg["Runtime"]["Provider"].get("auth", {}).get("method") != ("docker_sandbox_oauth" if settings["Sandbox"] == "asp" else "oauth")
                    or settings["Sandbox"] == "devlore" and (cfg["Runtime"].get("ClaudeCode", {}).get("Binary") != settings["ClaudeBinary"]
                        or cfg["Runtime"]["ClaudeCode"].get("managedPolicyMode") != "trust"
                        or cfg["Runtime"]["ClaudeCode"].get("ToolBridge", {}).get("Mode") != "structured")):
                pilot.fail("development_repo_configuration_mismatch", "Existing runtime binding differs; no config/history rewritten or provider fallback.")
            # Native Host owns authoritative config/permission validation.
            # Never regenerate existing startup configuration from new defaults.
        else:
            cfg = configuration(settings["Sandbox"], workspace, settings["Model"], settings["Effort"], settings["ClaudeBinary"],
                                audited=settings["PolicyAudited"], trust=settings["TrustManagedPolicy"])
            pilot.publish_json(p["config"], cfg)
            pilot.publish_json(p["binding"], expected)
    return p


def launch(args, directory, meta):
    if args.readme:
        gate.readme(directory)
        return
    # Validate name before any filesystem publication or provider process.
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]{0,127}", args.session):
        pilot.fail("development_session_invalid", "Use a native Session name: letters, digits or hyphens.")
    workspace = git_root()
    user_home = home()
    settings = read_settings(user_home, meta["sandbox"])
    if settings["BinaryDirectory"] != str(directory) or settings["Revision"] != meta["baseline"]:
        pilot.fail("development_launcher_version_mismatch", "Run the installed unreal command for the selected kit; old config/history retained.")
    if not args.workspace_info:
        gate.instructions(directory, wait=True, readme_command="unreal --readme")
    auth, binary = preflight(directory, user_home, meta["sandbox"], binary=settings["ClaudeBinary"],
                             audited=settings["PolicyAudited"], trust=settings["TrustManagedPolicy"])
    if (auth, binary) != (settings["CodexFile"], settings["ClaudeBinary"]):
        pilot.fail("development_auth_reference_changed", "Existing authentication discovery differs; no config/history rewritten.")
    p = prepare_repo(settings, workspace)
    if args.workspace_info:
        print(json.dumps({"workspace": str(workspace), "session": args.session, "provider": pilot.PROVIDERS[meta["sandbox"]],
                          "config": str(p["config"]), "state": str(p["state"]), "socket": str(p["socket"]),
                          "initial_permissions": "read_only"}, ensure_ascii=False, indent=2))
        return
    command = pilot.launcher_arguments(directory, p, auth, args.session)
    os.chdir(workspace)
    os.execv(command[0], command)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    setup = modes.add_parser("install")
    setup.add_argument("--model")
    setup.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"))
    setup.add_argument("--claude-binary", help="first install discovers claude; updates preserve the bound CLI unless explicitly changed")
    setup.add_argument("--policy-audited", action="store_true")
    setup.add_argument("--trust-managed-policy", action="store_true")
    setup.add_argument("--replace-pilot-launcher", action="store_true")
    run = modes.add_parser("launch", prog="unreal")
    run.add_argument("session", nargs="?", default="default")
    options = run.add_mutually_exclusive_group()
    options.add_argument("--workspace-info", action="store_true", help="show selected repository and persistent paths; no Host/inference")
    options.add_argument("--readme", action="store_true", help="show the detailed operator README")
    args = parser.parse_args(argv)
    try:
        directory, meta = package(Path(__file__).resolve().parent)
        install(args, directory, meta) if args.mode == "install" else launch(args, directory, meta)
        return 0
    except pilot.PilotError as error:
        print(f"FAIL: {error.code}: {error.action}", file=sys.stderr)
    except (OSError, ValueError, UnicodeError, KeyError, TypeError, AttributeError, subprocess.SubprocessError):
        print("FAIL: development_unavailable: Raw details withheld; keep existing projects/config/auth/history.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
