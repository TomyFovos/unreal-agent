# devlore permanent launcher

The devlore update is packaged with `python3 scripts/devlore_launcher_bundle.py`.
It contains installer/launcher scripts and checksums, reuses the already verified
6284e49 Linux amd64 CGO binaries, and never includes authentication or Session data.
Deliver only the devlore archive to the devlore Sandbox. Existing asp packages,
binaries, configuration and stores are untouched.

The two operator actions are transfer from Windows and one installation command;
the generated Japanese README contains both. Installation requires explicit
`--policy-audited --trust-managed-policy`, an authenticated supported Claude CLI,
the existing native Pilot installation, and the existing `~/workspace` directory.
The flags attest to the operator's existing managed-policy audit; they do not
replace or bypass organization policy. No login, model inference or Host startup
is performed by installation.

The installer publishes a private executable `~/.local/bin/unreal`. It refuses
to replace/shadow existing commands, aliases, functions or redirected files.
If necessary it appends only a guarded PATH-file source to `.bashrc` and Bash's
existing first login file (`.bash_profile`, `.bash_login`, or `.profile`). It does
not replace profile contents, introduce a higher-priority login file, modify the
system PATH, chmod existing directories, or automatically remove installations.
Identical installation preserves file contents and mtimes. New interactive and
login Bash checks must discover the installed command before installation passes.

In a new Bash session, `unreal` opens `default`; `unreal NAME` creates, attaches
or resumes that named Session through the existing native launcher. It never
submits an automatic smoke request. Configuration/state live in the dedicated
`unreal-agent-pilot/devlore/launcher-v1` namespace, with `state/run/host.sock`.
Older `devlore/v1` smoke history is neither copied nor removed. Ctrl-D detaches
the TUI while the Host continues; rerunning the same command reconnects.

Claude Code is the only configured provider, with managed trust and the structured
bridge explicitly selected. `/model` uses existing Claude catalog discovery and
canonical model/effort selection. Initial read/grep/glob permissions and workspace
boundaries remain unchanged. Built-in side-effecting tools, foreign MCP, provider
fallback and automatic permission expansion remain unavailable.

Run `make test-devlore-launcher`. Set `UNREAL_PILOT_KIT_DIRECTORY` to an existing
verified 6284e49 kit to exercise the installed command, real PTY detach/reconnect,
catalog/effort selection and durable history using the fake Claude CLI. These WSL
tests never contact a real provider and do not certify Sandbox auth/connectivity.
