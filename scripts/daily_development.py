#!/usr/bin/env python3
"""Workspace, file-update and legacy-wrapper helpers for the common kit."""

import contextlib
import fcntl
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
import pilot_provider as pilot


NAMESPACE = "unreal-agent-dev"


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


def settings_path(user_home, sandbox):
    return user_home / ".config" / NAMESPACE / sandbox / "launcher.json"


def read_settings(user_home, sandbox):
    return pilot.decode(files.read_owned(settings_path(user_home, sandbox), private=True, limit=8192))


def wrapper(directory):
    return ("#!/bin/bash\n# Unreal Agent daily development; native Host owns Session lifecycle.\nexec "
            + shlex.quote(str(Path(sys.executable).resolve())) + " -B "
            + shlex.quote(str(directory / "daily_development.py")) + ' launch "$@"\n').encode()


def owned_unreal_command(user_home, command, configured_directories=()):
    """Recognize an Unreal launcher by its script in the reserved install tree."""
    try:
        lines = [line for line in command.decode().splitlines() if line.strip() and not line.lstrip().startswith("#")]
        if len(lines) != 1: return False
        argv = shlex.split(lines[0])
        if len(argv) < 5 or argv[0] != "exec": return False
        args = argv[2:]
        if args[0] == "-B": args = args[1:]
        if len(args) != 3 or args[1:] != ["launch", "$@"]: return False
        script = Path(args[0])
        return (script.name in ("common_development.py", "daily_development.py", "devlore_launcher.py")
                and (script.parent in map(Path, configured_directories)
                     or script.is_relative_to(user_home / ".local/opt/unreal-agent-pilot"))
                and ".." not in script.parts)
    except (UnicodeError, ValueError, IndexError):
        return False


def owned_previous_command(user_home, sandbox, command, previous):
    return owned_unreal_command(user_home, command, [previous["BinaryDirectory"]] if previous else [])

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
    # The caller has selected a known Unreal installation destination.
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


def git_root():
    # Do not let inherited Git plumbing redirect the workspace to another repo.
    if any(os.environ.get(key) for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE")):
        pilot.fail("development_git_override", "Use a shell without Git workspace overrides; no repository changed.")
    cwd = safe_path(Path.cwd()).resolve()
    env = {"PATH": os.environ["PATH"], "HOME": os.environ["HOME"], "LANG": "C.UTF-8", "GIT_OPTIONAL_LOCKS": "0"}
    raw, code = pilot.bounded_probe(["git", "-C", str(cwd), "rev-parse", "--show-toplevel"], env)
    # Like Codex, a directory outside Git is a workspace of its own.
    root = safe_path(os.fsdecode(raw[:-1])).resolve() if not code and raw.endswith(b"\n") else cwd
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
