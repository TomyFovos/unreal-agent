#!/usr/bin/env python3
"""Install/launch devlore's persistent command using the existing native launcher."""

import argparse
import contextlib
import fcntl
import hashlib
import io
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys
import tempfile

import pilot_provider as pilot


INSTANCE = "launcher-v1"
MEMBERS = ("devlore_launcher.py", "pilot_provider.py", "install.sh", "README.md", "KIT.json")
PATH_MARKER = "# Unreal Agent devlore launcher-v1 PATH"


def read_owned(path, *, limit=1 << 20, private=False):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_mode & (0o077 if private else 0o022) or info.st_size > limit
                or info.st_nlink != 1):
            pilot.fail("launcher_file_unsafe", "Keep existing files; user-owned bounded regular files without redirects are required.")
        with os.fdopen(fd, "rb", closefd=False) as stream:
            value = stream.read(limit + 1)
        if len(value) > limit:
            pilot.fail("launcher_file_unsafe", "File size exceeded its bound; values withheld.")
        return value
    finally:
        os.close(fd)


def publish_bytes(path, value, mode=0o600):
    if os.path.lexists(path):
        if read_owned(path, private=True) != value or stat.S_IMODE(path.stat().st_mode) != mode:
            pilot.fail("launcher_install_conflict", "Existing command/configuration differs; nothing overwritten. Keep it and inspect the conflict.")
        return
    fd, temporary = tempfile.mkstemp(prefix=".launcher-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(value)
            stream.flush()
            os.fsync(stream.fileno())
        try:
            os.link(temporary, path)
        except FileExistsError:
            publish_bytes(path, value, mode)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(temporary).unlink(missing_ok=True)


def validate_package(package):
    if not package.is_absolute() or package.is_symlink():
        pilot.fail("launcher_package_redirect", "Use the verified devlore launcher kit; no existing installation was changed.")
    expected = b"".join(hashlib.sha256(read_owned(package / name)).hexdigest().encode() + b"  " + name.encode() + b"\n" for name in MEMBERS)
    if read_owned(package / "SHA256SUMS", limit=4096) != expected:
        pilot.fail("launcher_checksum_mismatch", "Kit checksum/inventory mismatch; no authentication or configuration changed.")
    meta = pilot.decode(read_owned(package / "KIT.json", limit=4096))
    if (not isinstance(meta, dict) or meta.get("version") != 1
            or meta.get("kind") != "devlore-launcher-kit" or meta.get("binary_revision") != pilot.BASELINE):
        pilot.fail("launcher_package_mismatch", "This kit reuses only the verified 6284e49 native binaries.")
    return hashlib.sha256(expected).hexdigest()[:16]


def home_and_paths():
    home = Path(os.environ.get("HOME", ""))
    if not home.is_absolute() or not home.is_dir() or any(c in str(home) for c in "\x00\r\n"):
        pilot.fail("invalid_launcher_home", "Use an existing absolute HOME; no environment or credentials changed.")
    home = home.resolve()
    workspace = home / "workspace"
    if workspace.is_symlink() or not workspace.is_dir():
        pilot.fail("workspace_unavailable", "Use the existing ~/workspace; no project is created or changed.")
    config = home / ".config/unreal-agent-pilot/devlore" / INSTANCE
    state = home / ".local/state/unreal-agent-pilot/devlore" / INSTANCE
    socket = state / "run/host.sock"
    if len(os.fsencode(socket)) > 100:
        pilot.fail("launcher_socket_path_too_long", "A shorter private HOME is required; no default socket is reused.")
    return {"home": home, "workspace": workspace, "config_dir": config, "config": config / "runtime.json",
            "binding": config / "launcher.json", "state": state, "sessions": state / "sessions", "socket": socket}


def shell_profiles(home):
    # Bash reads only the first existing login file; do not create a higher
    # priority file that would hide the user's existing startup configuration.
    login = next((home / name for name in (".bash_profile", ".bash_login", ".profile")
                  if os.path.lexists(home / name)), home / ".profile")
    profiles = (home / ".bashrc", login)
    for path in profiles:
        if os.path.lexists(path):
            read_owned(path)
    return profiles


def command_directory(home):
    # ~/.local/bin commonly already has mode 0755. It holds no secrets; keep
    # that mode, but reject redirects, foreign owners and group/other writers.
    path = home
    for name in (".local", "bin"):
        path = path / name
        if path.is_symlink():
            pilot.fail("launcher_command_directory_unsafe", "Existing command directory redirect is preserved.")
        path.mkdir(mode=0o700, exist_ok=True)
        info = path.stat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
            pilot.fail("launcher_command_directory_unsafe", "Existing command directory must be user-owned without group/other writers; no chmod.")
    return path


def inspect_shell_paths(paths):
    # Only closed type/PATH-membership enums leave this bounded check. Profile
    # stdout/stderr, command values and shell environment are never published.
    script = '''kind=$(type -t unreal || :)
case "$kind" in
  '') kind=absent;;
  file) if [ "$(type -P unreal)" = "$1" ]; then kind=owned; else kind=conflict; fi;;
  *) kind=conflict;;
esac
case ":$PATH:" in *":$2:"*) location=present;; *) location=missing;; esac
printf '\\nDEVLORE_COMMAND:%s:%s\\n' "$kind" "$location"
'''
    env = {"HOME": str(paths["home"]), "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "TERM": "dumb", "LANG": "C.UTF-8"}
    result = []
    for flags in (("--noprofile", "-ic"), ("-lic",)):
        raw, code = pilot.bounded_probe(["/bin/bash", *flags, script, "devlore-path-check", str(paths["home"] / ".local/bin/unreal"), str(paths["home"] / ".local/bin")], env, limit=65536)
        rows = [line.split(b":") for line in raw.splitlines() if line.startswith(b"DEVLORE_COMMAND:")]
        if (code or len(rows) != 1 or len(rows[0]) != 3 or rows[0][1] not in (b"absent", b"owned", b"conflict")
                or rows[0][2] not in (b"present", b"missing")):
            pilot.fail("launcher_shell_check_failed", "Bash startup did not produce a safe command check; raw profile output withheld.")
        if rows[0][1] == b"conflict":
            pilot.fail("launcher_command_conflict", "An existing unreal command/alias/function is preserved; no automatic replacement or shadowing.")
        result.append(tuple(rows[0][1:]))
    return result


def append_profile(path, block):
    existing = read_owned(path) if os.path.lexists(path) else b""
    if block in existing:
        return
    if PATH_MARKER.encode() in existing:
        pilot.fail("launcher_profile_conflict", "Existing launcher PATH block differs; keep it unchanged.")
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW | (0 if existing or path.exists() else os.O_CREAT | os.O_EXCL), 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022 or info.st_size != len(existing):
            pilot.fail("launcher_profile_conflict", "Bash startup file changed concurrently; nothing replaced.")
        data = b"\n" + block
        with os.fdopen(fd, "ab", closefd=False) as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        os.close(fd)


def install(args):
    paths = home_and_paths()
    home = paths["home"]
    # Check the explicit attestation before any Claude process or publication.
    config = pilot.configuration("devlore", paths["workspace"], args.model, args.effort,
                                 audited=args.policy_audited, trust=args.trust_managed_policy)
    package = args.package.absolute()
    content_id = validate_package(package)
    code_dir = home / ".local/opt/unreal-agent-pilot/devlore" / (INSTANCE + "-" + content_id)
    python = Path(sys.executable).resolve()
    wrapper = ("#!/bin/bash\n# Unreal Agent devlore permanent launcher; no shell evaluation.\nexec "
               + shlex.quote(str(python)) + " " + shlex.quote(str(code_dir / "devlore_launcher.py")) + " launch \"$@\"\n").encode()
    command = home / ".local/bin/unreal"
    if os.path.lexists(command) and (read_owned(command, private=True) != wrapper or stat.S_IMODE(command.stat().st_mode) != 0o700):
        pilot.fail("launcher_command_conflict", "Existing ~/.local/bin/unreal is preserved; no command is overwritten.")
    profiles = shell_profiles(home)
    shells = inspect_shell_paths(paths)
    with contextlib.redirect_stdout(io.StringIO()):
        kit = pilot.installed_kit(home, args.kit_directory)
    claude = pilot.claude_preflight(args.claude_binary, home, os.environ, args.policy_audited, args.trust_managed_policy)
    config["Runtime"]["ClaudeCode"]["Binary"] = claude
    pilot.private_tree(home, ".config/unreal-agent-pilot/devlore/" + INSTANCE)
    pilot.private_tree(home, ".local/state/unreal-agent-pilot/devlore/" + INSTANCE + "/run")
    lock = os.open(paths["config_dir"] / ".install.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(lock)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_nlink != 1:
            pilot.fail("launcher_lock_unsafe", "Existing install lock is preserved.")
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        pilot.publish_json(paths["config"], config)
        pilot.publish_json(paths["binding"], {"Version": 1, "Home": str(home), "BinaryDirectory": str(kit),
                                            "BinaryRevision": pilot.BASELINE, "PolicyAudited": True})
        pilot.private_tree(home, str(code_dir.relative_to(home)))
        for name in (*MEMBERS, "SHA256SUMS"):
            publish_bytes(code_dir / name, read_owned(package / name), 0o700 if name == "install.sh" else 0o600)
        command_directory(home)
        publish_bytes(command, wrapper, 0o700)
        if any(location == b"missing" for _, location in shells):
            path_script = paths["config_dir"] / "path.sh"
            publish_bytes(path_script, b'case ":${PATH-}:" in\n  *":$HOME/.local/bin:"*) ;;\n  *) PATH="$HOME/.local/bin${PATH:+:$PATH}" ;;\nesac\nexport PATH\n')
            block = (PATH_MARKER + "\n[ ! -r " + shlex.quote(str(path_script)) + " ] || . " + shlex.quote(str(path_script)) + "\n").encode()
            for profile in profiles:
                append_profile(profile, block)
        if any(kind != b"owned" or location != b"present" for kind, location in inspect_shell_paths(paths)):
            pilot.fail("launcher_shell_path_unavailable", "Keep installed files/history; existing Bash startup prevents command discovery. No profile is rewritten.")
    finally:
        os.close(lock)
    print("PASS: permanent devlore unreal installed; new interactive/login Bash checks passed; no Host/inference started")
    print("NEXT: open a new Bash session and run unreal or unreal SESSION_NAME; /model changes model/effort")
    print("PRESERVED: Claude auth/CLI/settings, old smoke Sessions, default Host and asp; read/grep/glob only")


def launch(args):
    paths = home_and_paths()
    binding = pilot.decode(read_owned(paths["binding"], limit=4096, private=True))
    if (not isinstance(binding, dict) or set(binding) != {"Version", "Home", "BinaryDirectory", "BinaryRevision", "PolicyAudited"}
            or binding["Version"] != 1 or binding["Home"] != str(paths["home"])
            or binding["BinaryRevision"] != pilot.BASELINE or binding["PolicyAudited"] is not True):
        pilot.fail("launcher_binding_invalid", "Keep the dedicated config/history; launcher binding is invalid.")
    read_owned(paths["config"], private=True)  # Native CLI owns schema/runtime/permission validation.
    with contextlib.redirect_stdout(io.StringIO()):
        kit = pilot.installed_kit(paths["home"], binding["BinaryDirectory"])
    command = pilot.launcher_arguments(kit, paths, "", args.session)
    # No smoke pointer, submission, provider fallback, or second Host lifecycle.
    os.chdir(paths["workspace"])
    os.execv(command[0], command)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    setup = modes.add_parser("install")
    setup.add_argument("--package", type=Path, default=Path(__file__).resolve().parent)
    setup.add_argument("--kit-directory")
    setup.add_argument("--claude-binary", default="claude")
    setup.add_argument("--model", default="claude-opus-5-5")
    setup.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"), default="high")
    setup.add_argument("--policy-audited", action="store_true")
    setup.add_argument("--trust-managed-policy", action="store_true")
    run = modes.add_parser("launch", prog="unreal", description="Claude Code session in devlore.")
    run.add_argument("session", nargs="?", default="default")
    args = parser.parse_args(argv)
    try:
        install(args) if args.mode == "install" else launch(args)
        return 0
    except pilot.PilotError as error:
        print(f"FAIL: {error.code}: {error.action}", file=sys.stderr)
    except (OSError, ValueError, UnicodeError, KeyError, TypeError, subprocess.SubprocessError):
        print("FAIL: devlore_launcher_unavailable: Raw details withheld; preserve existing commands/config/auth/history.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
