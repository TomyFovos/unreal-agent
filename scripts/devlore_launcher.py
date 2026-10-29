#!/usr/bin/env python3
"""File and Bash-profile helpers shared by the common development kit."""

import os
from pathlib import Path
import stat
import tempfile

import pilot_provider as pilot


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
        raw, code = pilot.bounded_probe(["/bin/bash", *flags, script, "devlore-path-check", str(paths["home"] / ".local/bin/unreal"), str(paths["home"] / ".local/bin")], env)
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
