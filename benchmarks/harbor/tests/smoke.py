"""Run a real Harbor Docker trial against the deterministic in-container model."""

import argparse
import base64
import json
import os
import struct
import subprocess
from pathlib import Path
from tempfile import TemporaryDirectory

from harbor.models.trajectories import Trajectory


def smoke(bundle: Path, configuration: str = "runner") -> None:
    with TemporaryDirectory(prefix="harness-harbor-smoke-") as directory:
        jobs = Path(directory)
        product = configuration != "runner"
        credentials = ["--ae", "OPENAI_API_KEY=harbor-smoke-test-key"]
        if product:
            credentials = []
            for provider, key in (
                ("openai", "harbor-smoke-test-key"),
                ("anthropic", "harbor-smoke-claude-key"),
            ):
                path = jobs / f"{provider}.key"
                path.write_text(key)
                path.chmod(0o600)
                credentials += ["--ak", f"{provider}_key_file={path}"]
        extra = (
            [
                "--ak",
                f"configuration={configuration}",
                "--ak",
                "child_model=smoke",
            ]
            if product
            else []
        )
        subprocess.run(
            [
                "harbor",
                "run",
                "-p",
                str(Path(__file__).parent / "task"),
                "-a",
                "smoke_agent:SmokeAgent"
                if product
                else "harness_harbor.runner:RunnerAgent",
                "-m",
                "openai/smoke"
                if configuration in {"gpt", "runner"}
                else "anthropic/smoke",
                "--ak",
                f"bundle={bundle.resolve()}",
                *credentials,
                "--ae",
                "OPENAI_BASE_URL=http://127.0.0.1:8765/v1",
                "-e",
                "docker",
                "-n",
                "1",
                "-k",
                "1",
                "-o",
                str(jobs),
                "--job-name",
                "smoke",
                *extra,
            ],
            check=True,
            env={**os.environ, "PYTHONPATH": str(Path(__file__).parent.resolve())},
            timeout=360,
        )
        trials = list(jobs.glob("smoke/*/result.json"))
        assert len(trials) == 1, trials
        trial = trials[0].parent
        result = json.loads(trials[0].read_text())
        if result.get("exception_info"):
            stderr = trial / "agent/runner.stderr"
            detail = stderr.read_text() if stderr.exists() else "No runner stderr"
            raise AssertionError(
                f"{result['exception_info']['exception_message']}\n{detail}"
            )
        assert result["verifier_result"]["rewards"]["reward"] == 1
        trajectory = Trajectory.model_validate_json(
            (trial / "agent/trajectory.json").read_text()
        )
        if product:
            from harness_harbor.security import scan

            assert not scan(
                [trial], ["harbor-smoke-test-key", "harbor-smoke-claude-key"]
            )
            usage = result["agent_result"]["metadata"]["usage_by_connection"]
            assert ("openai" in usage) == (configuration != "claude"), usage
            assert ("claude-api" in usage) == (configuration != "gpt"), usage
            if configuration == "claude-gpt":
                assert trajectory.extra["children"], "Missing GPT child reference"
                for child in trajectory.extra["children"]:
                    assert (trial / "agent" / child["trajectory_path"]).exists()
            print(
                f"Product Docker smoke passed: {configuration}, usage and child traces."
            )
            return
        requests = [
            json.loads(line)
            for line in (trial / "verifier/model-requests.jsonl")
            .read_text()
            .splitlines()
        ]
        assert requests and all(request.get("stream") is True for request in requests)
        assert all(
            {"Bash", "ViewImage", "read", "write", "edit", "grep", "glob"}
            <= {tool["name"] for tool in request["tools"]}
            for request in requests
        )
        observed = set()
        for request in requests:
            for item in request["input"]:
                if item.get("type") != "function_call_output":
                    continue
                output = item["output"]
                parts = []
                for part in output:
                    if part["type"] == "input_text":
                        parts.append(("text", part["text"]))
                    else:
                        assert part["type"] == "input_image", part
                        parts.append(("image", part["image_url"]))
                observed.add((item["call_id"], tuple(parts)))
        exported = set()
        for step in trajectory.steps:
            if not step.observation:
                continue
            for observation in step.observation.results:
                if "available_before_turn" not in observation.extra:
                    continue
                if isinstance(observation.content, str):
                    parts = [("text", observation.content)]
                else:
                    parts = []
                    for part in observation.content:
                        if part.type == "text":
                            parts.append(("text", part.text))
                        else:
                            data = (trial / "agent" / part.source.path).read_bytes()
                            assert data[:8] == b"\x89PNG\r\n\x1a\n"
                            assert struct.unpack(">II", data[16:24]) == (2000, 1)
                            url = (
                                f"data:{part.source.media_type};base64,"
                                + base64.b64encode(data).decode()
                            )
                            parts.append(("image", url))
                    assert parts[0][0] == "image", parts
                    assert parts[1] == (
                        "text",
                        "original MIME type: image/bmp; original dimensions: 4000x2; "
                        "multiply coordinates by 2.00 to approximate original",
                    ), parts
                exported.add((observation.source_call_id, tuple(parts)))
        assert {call_id for call_id, _ in exported} == {"plain", "bounded", "image"}, (
            exported
        )
        assert exported == observed, (exported, observed)
        assert len([s for s in trajectory.steps if s.source == "user"]) == 1
        metrics = result["agent_result"]
        assert metrics["n_input_tokens"] == 10 * len(requests), metrics
        assert metrics["n_cache_tokens"] == 4 * len(requests), metrics
        assert metrics["n_output_tokens"] == 3 * len(requests), metrics
        assert metrics["cost_usd"] is None, metrics
        assert (
            trajectory.agent.extra["binary_sha256"]
            == json.loads((bundle / "manifest.json").read_text())["sha256"]
        )
        sessions = list((trial / "agent/sessions").glob("*.session.jsonl"))
        assert sessions, "No persisted session was collected"
        for session in sessions:
            assert "harbor-smoke-test-key" not in session.read_text()
        print(
            "Harbor Docker smoke passed: reward, text/image observations, "
            "usage and provenance."
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument(
        "--configuration",
        choices=("runner", "claude-gpt", "gpt", "claude", "all"),
        default="all",
    )
    args = parser.parse_args()
    for configuration in (
        ("runner", "claude-gpt", "gpt", "claude")
        if args.configuration == "all"
        else (args.configuration,)
    ):
        smoke(args.bundle, configuration)
