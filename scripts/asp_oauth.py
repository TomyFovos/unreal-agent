#!/usr/bin/env python3
"""asp-only explicit Docker OAuth profile; existing CLI/auth/default Host untouched."""

import os
from pathlib import Path
import stat

import pilot_provider as pilot


def configuration(workspace, model, effort):
    result = pilot.configuration("asp", workspace, model, effort)
    result["Runtime"]["Provider"]["auth"] = {
        "provider": "openai-codex", "method": "docker_sandbox_oauth", "id": "docker-sandbox-oauth"}
    return result


def pilot_binaries():
    return ("unreal", "unreal-agent", "unreal-agent-runner", "unreal-agent-auth")


def auth_path(home, environ):
    if environ.get("SBX_CRED_OPENAI_MODE") != "oauth":
        pilot.fail("docker_sandbox_oauth_unavailable", "Docker kit mode must already be OAuth; use host-managed ChatGPT authentication, no API-key fallback.")
    path = Path(environ.get("OPENAI_CODEX_AUTH_FILE") or str(Path(environ.get("CODEX_HOME") or home / ".codex") / "auth.json"))
    if not path.is_absolute() or path.is_symlink():
        pilot.fail("codex_auth_path_invalid", "An existing absolute kit auth file is required; no copying or chmod.")
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        pilot.fail("codex_auth_path_invalid", "Keep the existing auth file; regular, user-owned, not writable by others required.")
    return str(path)


def preflight(kit, home, environ):
    path = auth_path(home, environ)
    env = {"PATH": environ["PATH"], "HOME": str(home), "SBX_CRED_OPENAI_MODE": "oauth"}
    for key in ("HTTPS_PROXY", "https_proxy"):
        if key in environ:
            env[key] = environ[key]
    # The script inspects only path metadata. The native protocol adapter owns
    # the bounded JSON/TOML checks, so there is one interpretation of the kit.
    # Only this native, checksum-verified preflight opts into bounded stderr.
    # Its closed error code is written there; never publish captured text.
    output, code = pilot.bounded_probe([str(kit / "unreal-agent-auth"), "check-docker-sandbox-oauth", "--codex-auth-file", path, "--check-forward-proxy"], env, capture_stderr=True)
    if code:
        if output.strip() == b"credential: docker_sandbox_proxy_unavailable":
            pilot.fail("docker_sandbox_proxy_unavailable", "Existing Docker HTTPS proxy is missing or unrecognized. Keep authentication/config unchanged; no direct sentinel request or arbitrary proxy fallback.")
        pilot.fail("docker_sandbox_oauth_unavailable", "Native kit/routing validation failed. Keep existing authentication/config; no provider/API-key fallback.")
    print("PASS: explicit Docker host-owned ChatGPT OAuth and forward proxy routing; no token/account import; connectivity unproven")
    return path
