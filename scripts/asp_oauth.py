#!/usr/bin/env python3
"""asp-only explicit Docker OAuth profile; existing CLI/auth/default Host untouched."""

import argparse
import http.client
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

import pilot_provider as pilot


def configuration(workspace, model, effort):
    result = pilot.configuration("asp", workspace, model, effort)
    result["Runtime"]["Provider"]["auth"] = {
        "provider": "openai-codex", "method": "docker_sandbox_oauth", "id": "docker-sandbox-oauth"}
    return result


def validate_kit(directory):
    if directory.is_symlink() or not directory.is_absolute():
        pilot.fail("pilot_kit_redirect", "Use the new private asp OAuth kit, never an existing install redirect.")
    for name in ("KIT.json", "SNAPSHOT.json", "SHA256SUMS", "doctor.sh", "asp_oauth.py", "pilot_provider.py", *pilot_binaries()):
        path = directory / name
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
            pilot.fail("pilot_kit_invalid", "Kit members must be regular, user-owned and not writable by others.")
        if name.endswith(".json") and info.st_size > 1 << 20:
            pilot.fail("pilot_kit_invalid", "Kit metadata exceeded its bound.")
    manifest = pilot.decode((directory / "KIT.json").read_bytes())
    snapshot = pilot.decode((directory / "SNAPSHOT.json").read_bytes())
    revision = manifest.get("baseline", "")
    if (manifest.get("kind") != "asp-oauth-kit" or not re.fullmatch(r"[0-9a-f]{40}", revision)
            or snapshot.get("revision") != revision or not manifest.get("cgo_enabled")
            or manifest.get("target") != "linux/amd64"):
        pilot.fail("pilot_kit_mismatch", "Old 6284e49 binaries cannot use the explicit OAuth route; keep that install and use this new kit.")
    # Validate the doctor before invoking it, then its exact full inventory.
    import hashlib
    sums = (directory / "SHA256SUMS").read_bytes()
    if len(sums) > 4096 or (hashlib.sha256((directory / "doctor.sh").read_bytes()).hexdigest() + "  doctor.sh\n").encode() not in sums:
        pilot.fail("pilot_kit_checksum", "Doctor checksum mismatch; no existing files changed.")
    _, code = pilot.bounded_probe(["bash", str(directory / "doctor.sh"), "--preflight"], {"PATH": os.environ["PATH"], "HOME": os.environ["HOME"]}, timeout=30)
    if code:
        pilot.fail("pilot_kit_preflight", "Linux amd64/glibc/library/checksum preflight failed; no system changes attempted.")
    return directory


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
    # The script inspects only path metadata. The native protocol adapter owns
    # the bounded JSON/TOML checks, so there is one interpretation of the kit.
    _, code = pilot.bounded_probe([str(kit / "unreal-agent-auth"), "check-docker-sandbox-oauth", "--codex-auth-file", path], env)
    if code:
        pilot.fail("docker_sandbox_oauth_unavailable", "Native kit/routing validation failed. Keep existing authentication/config; no provider/API-key fallback.")
    print("PASS: explicit Docker host-owned ChatGPT OAuth routing; no token/account import; connectivity unproven")
    return path


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("session", nargs="?", default="asp-oauth")
    parser.add_argument("--kit-directory", type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument("--model", help="required on first configuration; retained on subsequent starts")
    parser.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"))
    parser.add_argument("--instance", default="oauth-v1")
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--setup-only", action="store_true", help="validate/create dedicated config; no Host/inference")
    modes.add_argument("--smoke-only", action="store_true", help="operator opt-in: exactly one short real request, no tools")
    args = parser.parse_args(argv)
    try:
        home = Path(os.environ.get("HOME", ""))
        if not home.is_absolute() or not home.is_dir() or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,31}", args.instance):
            pilot.fail("invalid_pilot_path", "Existing absolute HOME and a safe instance name required.")
        home = home.resolve()
        workspace = home / "workspace"
        if workspace.is_symlink() or not workspace.is_dir():
            pilot.fail("workspace_unavailable", "The existing ~/workspace directory is required; no project is created.")
        kit = validate_kit(args.kit_directory)
        auth = preflight(kit, home, os.environ)
        config_dir = pilot.private_tree(home, ".config/unreal-agent-pilot/asp/" + args.instance)
        config_path = config_dir / "runtime.json"
        old = {}
        if config_path.exists():
            info = config_path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 1 << 20:
                pilot.fail("configuration_permissions", "Keep existing config/history; private regular config required.")
            old = pilot.decode(config_path.read_bytes())
        model = args.model or old.get("Runtime", {}).get("Provider", {}).get("model", {}).get("id", "")
        effort = args.effort or old.get("Runtime", {}).get("ReasoningEffort", "medium")
        cfg = configuration(workspace, model, effort)
        pilot.publish_json(config_path, cfg)
        state = pilot.private_tree(home, ".local/state/unreal-agent-pilot/asp/" + args.instance)
        run_dir = pilot.private_tree(state, "run")
        paths = {"home": home, "config": config_path, "state": state, "sessions": state / "sessions", "socket": run_dir / "host.sock"}
        if len(os.fsencode(paths["socket"])) > 107:
            pilot.fail("pilot_socket_path_too_long", "Choose a shorter private HOME/instance; no default socket reused.")
        print("PASS: asp-only dedicated config/state/socket; read/grep/glob only; no write/Bash/child permission")
        if args.setup_only:
            print("INFO: no Host/inference started; existing Codex and Session stores unchanged")
            return 0
        if args.smoke_only:
            gateway = pilot.ensure_host(kit, paths, auth, os.environ, docker_sandbox_oauth=True)
            pilot.smoke(gateway, "asp", state / "latest-smoke.json")
            return 0
        # Normal native launcher owns create/attach/resume and Host readiness.
        # No message is submitted merely by invoking this command or /model.
        command = pilot.launcher_arguments(kit, paths, auth, args.session)
        os.execv(command[0], command)
    except pilot.PilotError as error:
        print(f"FAIL: {error.code}: {error.action}", file=sys.stderr)
    except (OSError, ValueError, UnicodeError, KeyError, TypeError, AttributeError, subprocess.SubprocessError, http.client.HTTPException):
        print("FAIL: asp_oauth_preflight_failed: Raw details withheld; preserve existing config/auth/history.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
