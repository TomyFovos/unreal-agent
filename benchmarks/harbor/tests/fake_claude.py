#!/usr/bin/env python3
"""Deterministic Claude stream-json peer for Docker smoke; no model API."""

import json
import os
import re
import subprocess
import sys
from pathlib import Path


def send(value):
    print(json.dumps(value), flush=True)


def receive():
    line = sys.stdin.readline()
    return json.loads(line) if line else None


def ask(name, request):
    send({"type": "control_request", "request_id": name, "request": request})
    while message := receive():
        if message.get("response", {}).get("request_id") == name:
            return message["response"].get("response", {})
    return {}


def main():
    initialize = receive()
    if not initialize:
        return
    hooks = initialize["request"].get("hooks", {})
    send(
        {
            "type": "control_response",
            "response": {
                "subtype": "success",
                "request_id": initialize["request_id"],
                "response": {
                    "models": [
                        {
                            "value": "smoke",
                            "displayName": "smoke",
                            "supportedEffortLevels": ["max"],
                            "supportsEffort": True,
                        }
                    ],
                    "account": {"apiProvider": "firstParty"},
                },
            },
        }
    )
    user = receive()
    if user is None:
        return
    send({"type": "system", "subtype": "init", "model": "smoke"})
    send(
        {
            "type": "user",
            "isReplay": True,
            "parent_tool_use_id": None,
            "message": user["message"],
        }
    )
    system = sys.argv[sys.argv.index("--append-system-prompt") + 1]
    marker = Path.home() / "smoke-delegated"
    if not marker.exists():
        cli = re.search(r"- Start the lead,.*?: (\S+) subagent start", system)
        command = (
            cli[1]
            + " subagent start --template worker-max"
            + " --task 'Run the smoke task and finish'"
            if cli
            else "printf done > /app/answer"
        )
        input_value = {"command": command}
        call_id = "smoke-bash"
        send(
            {
                "type": "assistant",
                "message": {
                    "id": "before",
                    "model": "smoke",
                    "content": [
                        {
                            "type": "tool_use",
                            "name": "Bash",
                            "id": call_id,
                            "input": input_value,
                        }
                    ],
                },
            }
        )
        decision = ask(
            "permission",
            {
                "subtype": "can_use_tool",
                "tool_name": "Bash",
                "tool_use_id": call_id,
                "input": input_value,
            },
        )
        assert decision["behavior"] == "allow", decision
        result = subprocess.run(
            command, shell=True, text=True, capture_output=True, check=True
        )
        marker.touch()
        send(
            {
                "type": "user",
                "message": {
                    "content": [
                        {
                            "type": "tool_result",
                            "tool_use_id": call_id,
                            "content": result.stdout,
                        }
                    ]
                },
            }
        )
        for matcher in hooks.get("PostToolBatch", []):
            for callback in matcher.get("hookCallbackIds", []):
                ask(
                    "batch",
                    {
                        "subtype": "hook_callback",
                        "callback_id": callback,
                        "input": {
                            "hook_event_name": "PostToolBatch",
                            "cwd": os.getcwd(),
                            "tool_calls": [
                                {
                                    "tool_name": "Bash",
                                    "tool_input": input_value,
                                    "tool_use_id": call_id,
                                    "tool_response": result.stdout,
                                }
                            ],
                        },
                    },
                )
    counts = {
        "input_tokens": 7,
        "cache_read_input_tokens": 2,
        "cache_creation_input_tokens": 1,
        "output_tokens": 3,
    }
    send(
        {
            "type": "assistant",
            "message": {
                "id": "end",
                "model": "smoke",
                "usage": counts,
                "content": [{"type": "text", "text": "Smoke task complete"}],
            },
        }
    )
    send(
        {
            "type": "result",
            "subtype": "success",
            "result": "Smoke task complete",
            "usage": counts,
        }
    )
    while receive():
        pass


if __name__ == "__main__":
    main()
