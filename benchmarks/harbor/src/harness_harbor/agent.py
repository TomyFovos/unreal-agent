"""Harbor attachment to the product Host, including its normal child CLI."""

import json
import shlex
from pathlib import Path, PurePosixPath
from tempfile import NamedTemporaryFile
from typing import Any, override
from uuid import uuid4

import certifi
from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from harbor.models.trajectories import Agent

from harness_harbor.bundle import Bundle
from harness_harbor.product import export_product
from harness_harbor.runner import RunnerAgent
from harness_harbor.security import Redactor, read_key

EFFORTS = {"low", "medium", "high", "xhigh", "max"}
TOOLS = [
    "Bash",
    "ViewImage",
    "read",
    "write",
    "edit",
    "grep",
    "glob",
    "ast_grep",
    "ast_edit",
    "claude_code",
    "SkillUse",
    "LSP",
    "DAP",
    "Finish",
    "SendParent",
    "SubagentStart",
    "SubagentSend",
    "SubagentCancel",
    "SubagentApprove",
]


class UnrealAgent(RunnerAgent):
    def __init__(
        self,
        *args: Any,
        bundle: str,
        configuration: str = "claude-gpt",
        main_model: str | None = None,
        main_effort: str = "max",
        child_model: str | None = None,
        child_effort: str | None = None,
        openai_key_file: str | None = None,
        anthropic_key_file: str | None = None,
        **kwargs: Any,
    ) -> None:
        BaseInstalledAgent.__init__(self, *args, **kwargs)
        if configuration not in {"claude-gpt", "gpt", "claude"}:
            raise ValueError("configuration must be claude-gpt, gpt or claude")
        if main_effort not in EFFORTS or (
            child_effort is not None and child_effort not in EFFORTS
        ):
            raise ValueError("Unsupported main_effort or child_effort")
        self._configuration = configuration
        prefix = "openai" if configuration == "gpt" else "anthropic"
        selected = (
            main_model
            or self.model_name
            or f"{prefix}/{'gpt-6.1-sol' if configuration == 'gpt' else 'opus'}"
        )
        self._model = self._model_id(selected, prefix)
        if configuration == "claude":
            self._child_model = (
                self._model_id(child_model, "anthropic") if child_model else ""
            )
            self._child_effort = child_effort or ""
        else:
            self._child_model = self._model_id(child_model or "gpt-6.1-sol", "openai")
            self._child_effort = child_effort or "max"
        self.model_name = f"{prefix}/{self._model}"
        self._main_effort = main_effort
        self._key_files = {"openai": openai_key_file, "claude-api": anthropic_key_file}
        self._secrets: list[str] = []
        self._bundle = Bundle.load(bundle)
        self._bundle.read_file("unreal-agent")
        self._runner_session = str(uuid4())
        self._remote = PurePosixPath("/installed-agent/unreal-agent")
        self._state = PurePosixPath(f"/tmp/unreal-{self._runner_session}")
        self._not_offered = {
            "task_mcp_servers": [s.name for s in self.mcp_servers],
            "task_skills_dir": self.skills_dir,
        }

    @staticmethod
    def _model_id(value: str, provider: str) -> str:
        if "/" in value:
            prefix, value = value.split("/", 1)
            if prefix != provider:
                raise ValueError("Model provider does not match configuration")
        if not value or any(c.isspace() for c in value):
            raise ValueError("Model ID must be nonempty and contain no whitespace")
        return value

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_curl(environment)
        await self.ensure_system_dependencies(environment, ("python3",))
        await self.exec_as_root(environment, command=f"mkdir -p {self._remote}/bin")
        names = ["unreal-agent"]
        if self._configuration != "gpt":
            names.append("claude-install.py")
        for name in names:
            await self._upload_bytes(
                environment, self._bundle.read_file(name), str(self._remote / name)
            )
        for name in ("launch.py", "security.py", "session_files.py"):
            await environment.upload_file(
                Path(__file__).with_name(name), str(self._remote / name)
            )
        await environment.upload_file(
            Path(certifi.where()), str(self._remote / "ca.pem")
        )
        await self.exec_as_root(
            environment,
            command=(
                f"chmod 755 {self._remote}/unreal-agent && "
                f"chmod 644 {self._remote}/launch.py "
                f"{self._remote}/security.py {self._remote}/session_files.py "
                f"{self._remote}/ca.pem"
            ),
        )
        if self._configuration != "gpt":
            await self.exec_as_root(
                environment,
                command=shlex.join(
                    [
                        "python3",
                        str(self._remote / "claude-install.py"),
                        "--destination",
                        str(self._remote / "bin"),
                        "--arch",
                        self._bundle.arch,
                    ]
                ),
            )
        await self.exec_as_agent(
            environment,
            command=f"umask 077; mkdir -p {self._state}; chmod 700 {self._state}",
        )

    async def _upload_bytes(self, environment, data: bytes, destination: str) -> None:
        with NamedTemporaryFile() as temporary:
            temporary.write(data)
            temporary.flush()
            await environment.upload_file(Path(temporary.name), destination)
        command = shlex.join(["chmod", "600", destination])
        if environment.default_user is not None:
            command += " && " + shlex.join(
                ["chown", str(environment.default_user), destination]
            )
        await self.exec_as_root(environment, command=command)

    def _keys(self) -> dict[str, str]:
        providers = ["openai"] if self._configuration == "gpt" else ["claude-api"]
        if self._configuration == "claude-gpt":
            providers.append("openai")
        result = {}
        for provider in providers:
            path = self._key_files[provider]
            env = "OPENAI_API_KEY" if provider == "openai" else "ANTHROPIC_API_KEY"
            value = read_key(path) if path else self._get_env(env)
            if not value or any(c in value for c in "\0\r\n"):
                raise ValueError(f"No valid explicitly supplied key for {provider}")
            result[provider] = value
        self._secrets = list(result.values())
        return result

    def _runtime_config(self, keys: dict[str, str]) -> dict:
        def backend(provider, model, effort):
            selection = {
                "version": 1,
                "provider": provider,
                "model": {"id": model},
                "auth": {
                    "provider": provider,
                    "method": "api_key",
                    "id": "explicit-api-key",
                },
                "max_attempts": 2,
                "source": "Harbor explicit API selection",
            }
            value = {"Provider": selection, "ReasoningEffort": effort}
            if provider == "openai":
                selection["endpoint"] = (
                    self._get_env("OPENAI_BASE_URL") or "https://api.openai.com/v1"
                )
            else:
                value["Claude"] = {"Binary": str(self._remote / "bin/claude")}
                if self._configuration == "claude":
                    value["Claude"].update(
                        {
                            "AgentModel": self._child_model,
                            "AgentEffort": self._child_effort,
                        }
                    )
            return value

        main = "openai" if self._configuration == "gpt" else "claude-api"
        runtime = {
            "Version": 1,
            "Workspace": ".",
            "Profile": {
                "id": "conservative",
                "version": 1,
                "source": "configured default",
            },
            **backend(main, self._model, self._main_effort),
        }
        maximum = {
            "Tools": TOOLS,
            "FilesystemUnrestricted": True,
            "NetworkUnrestricted": True,
            "ProcessMode": "unrestricted",
        }
        config = {
            "Runtime": runtime,
            "Permissions": maximum,
            "PermissionModes": {"Enabled": True, "Maximum": maximum},
            "Launcher": {
                "Version": 1,
                "DiscoverProviders": False,
                "CodexDisabled": True,
                "ClaudeDisabled": True,
                "APIKeys": {p: {"file": path} for p, path in keys.items()},
            },
        }
        if self._configuration == "claude-gpt":
            config["Providers"] = {
                "openai": backend("openai", self._child_model, self._child_effort)
            }
        if self._configuration == "claude":
            config["DisableSubagents"] = True
        else:
            config["SubagentDefaults"] = {
                "Provider": "openai",
                "Model": self._child_model,
                "Effort": self._child_effort,
            }
        return config

    @with_prompt_template
    @override
    async def run(
        self, instruction: str, environment: BaseEnvironment, context: AgentContext
    ) -> None:
        keys = self._keys()
        paths = {p: str(self._state / f"{p}.key") for p in keys}
        for provider, key in keys.items():
            await self._upload_bytes(environment, key.encode(), paths[provider])
        # Everything uploaded to logs is sanitized by the attachment first.
        environment_values = {"SSL_CERT_FILE": str(self._remote / "ca.pem")}
        if value := self._get_env("ANTHROPIC_BASE_URL"):
            environment_values["ANTHROPIC_BASE_URL"] = value
        request = {
            "binary": str(self._remote / "unreal-agent"),
            "bin": str(self._remote / "bin"),
            "state": str(self._state),
            "logs": str(self.environment_logs_dir),
            "session": self._runner_session,
            "keys": paths,
            "environment": environment_values,
        }
        for name, data in (
            ("config.json", json.dumps(self._runtime_config(paths))),
            ("prompt", instruction),
            ("launch.json", json.dumps(request)),
        ):
            await self._upload_bytes(
                environment, data.encode(), str(self._state / name)
            )
        await self.exec_as_agent(
            environment,
            command=shlex.join(
                [
                    "python3",
                    str(self._remote / "launch.py"),
                    str(self._state / "launch.json"),
                ]
            ),
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        redactor = Redactor(self._secrets)
        # Defense in depth for logs downloaded by a nonstandard environment.
        for path in self.logs_dir.rglob("*"):
            if path.is_file() and path.suffix in {
                ".json",
                ".jsonl",
                ".stderr",
                ".stdout",
                ".log",
            }:
                raw = path.read_text()
                path.write_text(
                    redactor.jsonl(raw)
                    if path.suffix == ".jsonl"
                    else redactor.text(raw)
                )
        identity = Agent(
            name=self.name(),
            version=self.version(),
            model_name=self.model_name,
            extra={
                "binary_sha256": self._bundle.files["unreal-agent"],
                "arch": self._bundle.arch,
                "configuration": self._configuration,
                "runtime": self._bundle.runtime if self._configuration != "gpt" else {},
                "main_effort": self._main_effort,
                "child_model": self._child_model,
                "child_effort": self._child_effort,
            },
        )
        if not (
            self.logs_dir / "sessions" / f"{self._runner_session}.session.jsonl"
        ).exists():
            context.metadata = {"agent_logs_missing": True, "revision": self.version()}
            return
        trajectory = export_product(self.logs_dir, identity, self._runner_session)
        metrics = trajectory.final_metrics
        context.n_input_tokens = metrics.total_prompt_tokens
        context.n_cache_tokens = metrics.total_cached_tokens
        context.n_output_tokens = metrics.total_completion_tokens
        context.cost_usd = metrics.total_cost_usd
        context.metadata = {
            **(context.metadata or {}),
            **identity.extra,
            **trajectory.extra,
            **self._not_offered,
            "revision": self.version(),
        }
