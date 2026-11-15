import asyncio
import hashlib
import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from harbor.environments.base import ExecResult
from harbor.models.agent.context import AgentContext
from test_trajectory import record, response

from harness_harbor.runner import APT_ARCHIVE_FALLBACK
from harness_harbor.runner import RunnerAgent as UnrealAgent


class ScriptedEnvironment:
    """Debian bullseye image without curl whose default apt mirror has moved."""

    def __init__(self, mirror_broken: bool) -> None:
        self.commands: list[str] = []
        self.curl_installed = False
        self.mirror_broken = mirror_broken

    async def exec(
        self, command: str, user: str | None = None, **_: object
    ) -> ExecResult:
        self.commands.append(command)
        if "command -v" in command and "curl" in command:
            return ExecResult(return_code=0 if self.curl_installed else 1)
        if "for manager in apt-get" in command:
            return ExecResult(stdout="apt-get", return_code=0)
        if "archive.debian.org" in command:
            self.mirror_broken = False
            return ExecResult(return_code=0)
        if "apt-get install" in command:
            if self.mirror_broken:
                return ExecResult(
                    stderr="E: Unable to fetch some archives", return_code=100
                )
            self.curl_installed = True
            return ExecResult(return_code=0)
        raise AssertionError(f"unexpected command: {command}")


def make_agent(directory: Path) -> UnrealAgent:
    (directory / "unreal-agent-runner").write_bytes(b"runner")
    (directory / "manifest.json").write_text(
        json.dumps(
            {
                "revision": "b" * 40,
                "sha256": hashlib.sha256(b"runner").hexdigest(),
                "goos": "linux",
                "goarch": "amd64",
            }
        )
    )
    return UnrealAgent(
        bundle=str(directory), logs_dir=directory, model_name="openai/test"
    )


class InstallTests(unittest.TestCase):
    def test_post_run_accepts_summary_and_rejects_failed_session_end(self):
        for failed in (False, True):
            with self.subTest(failed=failed), TemporaryDirectory() as temporary:
                root = Path(temporary)
                agent = make_agent(root)
                usage = response("t", [])["Response"]["Usage"]
                terminal = (
                    json.dumps({"type": "session_end", "is_error": True})
                    if failed
                    else record(
                        2,
                        "session_end",
                        {
                            "IsError": False,
                            "Usage": usage,
                            "ToolCalls": 0,
                            "ChangedFiles": [],
                        },
                    )
                )
                (root / "runner.jsonl").write_text(
                    record(1, "model_response", response("t", []))
                    + "\n"
                    + terminal
                    + "\n"
                )
                context = AgentContext()
                if failed:
                    with self.assertRaisesRegex(
                        RuntimeError, "session ended with an error"
                    ):
                        agent.populate_context_post_run(context)
                else:
                    agent.populate_context_post_run(context)
                self.assertEqual(context.n_input_tokens, 10)
                self.assertEqual(context.n_output_tokens, 3)
                self.assertEqual(context.metadata["session_end"]["IsError"], failed)
                self.assertTrue((root / "trajectory.json").exists())

    def test_curl_is_installed_before_the_agent_runs(self):
        with TemporaryDirectory() as temporary:
            agent = make_agent(Path(temporary))
            environment = ScriptedEnvironment(mirror_broken=False)
            asyncio.run(agent.ensure_curl(environment))
            self.assertTrue(environment.curl_installed)
            self.assertFalse(
                any("archive.debian.org" in c for c in environment.commands)
            )

    def test_moved_apt_mirror_falls_back_to_the_archive(self):
        with TemporaryDirectory() as temporary:
            agent = make_agent(Path(temporary))
            environment = ScriptedEnvironment(mirror_broken=True)
            asyncio.run(agent.ensure_curl(environment))
            self.assertTrue(environment.curl_installed)
            installs = [
                i for i, c in enumerate(environment.commands) if "apt-get install" in c
            ]
            fallback = [
                i
                for i, c in enumerate(environment.commands)
                if APT_ARCHIVE_FALLBACK in c
            ]
            self.assertEqual(len(installs), 2)
            self.assertEqual(len(fallback), 1)
            self.assertLess(installs[0], fallback[0])
            self.assertLess(fallback[0], installs[1])


if __name__ == "__main__":
    unittest.main()
