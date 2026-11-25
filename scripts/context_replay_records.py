"""Read canonical session logs into the metadata used by context_replay."""

from bisect import bisect_right
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime
import hashlib
import json
import posixpath
import re
import shlex


ROUTE_PROVIDERS = {
    "claude": "claude",
    "codex": "openai-codex",
    "deepseek": "opencode-go",
    "opencode-go": "opencode-go",
    "claude-api": "claude-api",
}
LOG_SUFFIX = ".session.jsonl"


def timestamp(text):
    return datetime.fromisoformat(text).timestamp()


@dataclass
class Window:
    causes: tuple
    before: int
    after: int
    measurement: str
    header: int
    l2: int
    tail: int

    @classmethod
    def read(cls, fields):
        return cls(
            tuple(fields.get("Causes", ())), fields["BeforeTokens"], fields["AfterTokens"],
            fields.get("BeforeMeasurement", ""), fields.get("HeaderTokens", 0),
            fields.get("L2Tokens", 0), (fields.get("Tail") or {}).get("Tokens", 0),
        )


@dataclass
class Request:
    route: str
    model: str
    effort: str
    at: float
    end: float
    seq: int
    input: int
    read: int | None
    write: int | None
    output: int | None
    admission: dict
    window: Window | None
    header_key: str
    human: int
    new_human_tokens: int
    peers: int
    invocation: str
    implementation: str
    activity: list = field(default_factory=list)
    role: str = ""

    @property
    def estimate(self):
        return self.admission.get("EstimatedInputTokens", 0)

    @property
    def budget(self):
        return self.admission.get("BudgetTokens", 0)


@dataclass
class Turn:
    at: float
    seq: int
    selection: dict
    admission: dict
    invocation: str


def json_dict(value):
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except ValueError:
            return {}
        if isinstance(parsed, dict):
            return parsed
    return {}


def tool_actions(output):
    if output.get("Type") != "tool_call" or output.get("ParentCallID") or output.get("OutputLimit"):
        return []
    tool = output["Data"]
    name, args = tool.get("Name", "").lower(), json_dict(tool.get("Arguments"))
    if name == "claude_code":
        name, args = args.get("tool", "").lower(), json_dict(args.get("input"))
    if name in {"read", "readfile", "read_file"}:
        files = args.get("files")
        paths = [file.get("path") for file in files if isinstance(file, dict)] if isinstance(files, list) else [
            args.get("file_path") or args.get("path")]
        return [("file", digest(path)) for path in dict.fromkeys(posixpath.normpath(p) for p in paths if isinstance(p, str) and p)]
    if name in {"bash", "shell", "exec_command"}:
        command = args.get("command") or args.get("cmd")
        if isinstance(command, str) and command.strip():
            path = simple_read(command)
            return [("file", digest(path))] if path else [("repeat", digest(command.strip()))]
    return []


def digest(text):
    return hashlib.sha256(text.encode()).digest()


def simple_read(command):
    """Count direct single-file shell reads when the path is unambiguous."""
    if re.search(r"[|;&\n$<>{}*?]", command):
        return None
    try:
        words = shlex.split(command)
    except ValueError:
        return None
    if not words:
        return None
    if words[0] == "cat":
        args = words[1:]
    elif words[0] == "sed" and len(words) == 4 and words[1] == "-n" and words[2].endswith("p"):
        args = words[3:]
    elif words[0] in {"head", "tail", "nl"} and len(words) >= 2:
        args = words[-1:] if len(words) == 2 or len(words) == 4 and words[1] in {"-n", "-ba"} else []
    else:
        return None
    paths = [arg for arg in args if arg != "--"]
    return posixpath.normpath(paths[0]) if len(paths) == 1 and not paths[0].startswith("-") else None


def session_items(paths):
    # Roots are supplied oldest first. A fresh slice can overlap a full copy.
    last = 0
    for path in paths:
        with path.open("rb") as source:
            for line in source:
                if line.startswith(b'{"type":"operation"'):
                    continue
                if not line.endswith(b"\n"):
                    break
                item = json.loads(line)["data"].get("Item")
                if item and item["Sequence"] > last:
                    last = item["Sequence"]
                    yield item


def response_usage(usage, route):
    unknown = set(usage.get("Unknown") or ())
    raw = usage.get("Raw") or {}
    details = raw.get("input_tokens_details") or {}
    read = details.get("cached_tokens", usage.get("CachedInputTokens"))
    write = details.get("cache_write_tokens", usage.get("CacheWriteInputTokens"))
    if "cached_input" in unknown and "cached_tokens" not in details:
        read = None
    # DeepSeek does disk writes at the standard miss rate; no separate billed count.
    if route == "deepseek":
        write = 0
    return usage.get("InputTokens", 0), read, write, usage.get("OutputTokens")


def response_route(selection, admission, model):
    provider = selection.get("Provider", "")
    estimator = admission.get("Estimator", "")
    if provider == "openai-codex" or estimator.endswith(":openai-codex"):
        return "codex"
    if provider == "opencode-go" or estimator.endswith(":opencode-go"):
        return "deepseek" if "deepseek" in model.lower() else "opencode-go"
    return None


def load_session(paths, claude_main):
    requests, actions = [], []
    turns, streams, selections = {}, {}, {}
    active, window = {}, None
    implementation, header, role = "", "", ""
    human = human_tokens = peers = 0
    tool_ids = set()
    for item in session_items(paths):
        seq, kind, data = item["Sequence"], item["Kind"], item["Data"]
        if kind == "input":
            if data.get("Kind") == "external":
                human += 1
                text = json_dict(data.get("Payload")).get("Text", "")
                human_tokens = len(text.encode()) // 4 if isinstance(text, str) else 0
            if data.get("Kind") == "peer":
                peers += 1
        elif kind == "turn" and data["Type"] == "regular":
            turn_id = data["ID"]
            turns[turn_id] = Turn(timestamp(item["RecordedAt"]), seq,
                                  selections.get(data.get("RuntimeRevision"), active),
                                  data.get("ContextAdmission") or {},
                                  data.get("ToolContinuation") or turn_id)
        elif kind == "model_response":
            response = data["Response"]
            turn = turns.get(data["TurnID"])
            if turn is not None:
                generation = response.get("Generation") or {}
                model = response.get("Model") or generation.get("Model") or turn.selection.get("Model", "")
                route = response_route(turn.selection, turn.admission, model)
                if route:
                    use = response_usage(response.get("Usage") or {}, route)
                    if use[0] > 0:
                        requests.append(Request(
                            route, model, turn.selection.get("Effort", ""), turn.at,
                            timestamp(item["RecordedAt"]), turn.seq, *use, turn.admission,
                            window, header, human, human_tokens, peers, turn.invocation, implementation,
                        ))
            if response.get("Stop") not in {"max_tokens", "max_output_tokens", "length"}:
                for output in response.get("Output") or []:
                    reads = tool_actions(output)
                    if reads:
                        call = output["Data"].get("CallID")
                        if not call or call not in tool_ids:
                            actions.extend((seq, *action) for action in reads)
                            tool_ids.add(call)
        elif kind == "host_record":
            context = data.get("Context") or {}
            if data.get("Kind") == "configuration":
                lineage = json_dict(data.get("Configuration"))
                role = lineage.get("Layer") or role
            role = (context.get("Policy") or {}).get("Role") or role
            ctx_kind = context.get("Kind")
            if ctx_kind in {"window_start", "window_boundary"}:
                header = context.get("Header", "")
            if ctx_kind == "window_boundary":
                window = Window.read(context["Window"])
            if ctx_kind == "record_read":
                read = context["RecordRead"]
                actions.append((read.get("After", seq), read["Action"], bool(read.get("PreviousTailHits"))))
            if data.get("Kind") == "runtime_applied":
                active = data["Selection"]
                selections[active.get("Revision")] = active
            activity = data.get("Activity") or {}
            if activity.get("Kind") == "host_start":
                implementation = activity.get("Revision", "")
            if activity.get("Kind") != "claude_stream" or activity.get("Model") == "<synthetic>":
                continue
            event = json_dict(activity.get("Reason"))
            if event.get("parent_tool_use_id") or event.get("compaction"):
                continue
            key = activity.get("ID", "")
            turn = turns.get(key.partition("/stream/")[0])
            use = activity.get("RequestUsage") or {}
            if event.get("event") == "message_start" and key not in streams:
                if turn and use.get("Input", 0) > 0:
                    provider = activity.get("Provider") or turn.selection.get("Provider")
                    if provider not in {"claude", "claude-api"}:
                        if not claude_main and provider:
                            continue
                        provider = "claude"
                    first = not requests or requests[-1].invocation != turn.invocation
                    # Admission is recorded once for the subprocess, not for its later API calls.
                    admission = turn.admission if first else {}
                    at = timestamp(activity.get("StartedAt") or item["RecordedAt"])
                    request = Request(
                        provider, activity.get("Model", ""), turn.selection.get("Effort", ""),
                        at, at, seq, use["Input"], use.get("Read"), use.get("Write"), None,
                        admission, window, header, human, human_tokens, peers, turn.invocation, implementation,
                    )
                    requests.append(request)
                    streams[key] = request
            if event.get("event") == "message_stop" and key in streams:
                request = streams[key]
                request.end = timestamp(activity.get("EndedAt") or item["RecordedAt"])
                request.output = use.get("Output", event.get("output_tokens"))
    for request in requests:
        request.role = role
    requests.sort(key=lambda request: request.seq)
    seqs = [request.seq for request in requests]
    for seq, kind, value in sorted(actions, key=lambda entry: entry[0]):
        index = bisect_right(seqs, seq) - 1
        if index >= 0:
            requests[index].activity.append((kind, value))
    return requests


def read_traces(roots, claude_main=None):
    groups = defaultdict(list)
    for root in roots:
        if not root.is_dir():
            raise ValueError(f"not a session copy directory: {root}")
        for path in sorted(root.rglob(f"*{LOG_SUFFIX}")):
            groups[path.name.removesuffix(LOG_SUFFIX)].append(path)
    return {name: load_session(paths, name == claude_main) for name, paths in groups.items()}
