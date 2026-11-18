"""Resolve canonical Session copies, including settled children, from the index."""

import json
import re
from pathlib import Path


def session_paths(directory: Path):
    paths = {
        path.name.removesuffix(".session.jsonl"): path
        for path in sorted(directory.glob("*.session.jsonl"))
    }
    archive = directory.with_name(directory.name + "-archive")
    for kind in ("index", "pending"):
        for index in sorted((archive / kind).glob("*.json")):
            entry = json.loads(index.read_text())
            child, parent = entry["child_id"], entry["parent_id"]
            if not all(re.fullmatch(r"[A-Za-z0-9-]+", v) for v in (child, parent)):
                raise ValueError("Invalid archived Session identity")
            if index.stem != child:
                raise ValueError("Archived Session index identity mismatch")
            # A pending move can still have its record in the live directory.
            # Each canonical record contributes exactly once to usage/costs.
            paths.setdefault(child, archive / parent / child / f"{child}.session.jsonl")
    return paths
