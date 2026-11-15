"""Offline fixture: replace Claude Code with a deterministic protocol peer."""

from pathlib import Path

from harness_harbor.agent import UnrealAgent


class SmokeAgent(UnrealAgent):
    async def install(self, environment):
        mode = self._configuration
        # Reuse Host installation, but do not download/run an actual Claude CLI.
        self._configuration = "gpt"
        try:
            await super().install(environment)
        finally:
            self._configuration = mode
        if mode != "gpt":
            await self._upload_bytes(
                environment,
                Path(__file__).with_name("fake_claude.py").read_bytes(),
                str(self._remote / "bin/claude"),
            )
            await self.exec_as_root(
                environment, command=f"chmod 755 {self._remote}/bin/claude"
            )
