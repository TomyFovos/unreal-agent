"""Recalculate a completed job from Session copies, without changing the job."""

import argparse
import json
from pathlib import Path

from harness_harbor.pricing import load_prices, price_path
from harness_harbor.product import aggregate, load_session, usage_rows


def recalculate(job: Path, *, prices=None):
    prices = load_prices() if prices is None else prices
    trials, job_rows = [], []
    for directory in sorted(job.glob("*/agent/sessions")):
        loaded = {
            path.name.removesuffix(".session.jsonl"): load_session(path)
            for path in sorted(directory.glob("*.session.jsonl"))
        }
        children = {
            child["session_id"]
            for _, _, descendants in loaded.values()
            for child in descendants
        }
        sessions, trial_rows = {}, []
        for sid, (items, identity, _) in loaded.items():
            rows = usage_rows(items, identity)
            connections, totals = aggregate(rows, prices=prices)
            sessions[sid] = {
                "role": "child" if sid in children else "main",
                "usage_by_connection": connections,
                **totals,
            }
            # Each file contributes its own usage, never a recursive total.
            trial_rows.extend(rows)
        connections, totals = aggregate(trial_rows, prices=prices)
        trial = directory.parents[1].name
        trials.append(
            {
                "trial": trial,
                "task": trial.split("__", 1)[0],
                "sessions": sessions,
                "usage_by_connection": connections,
                **totals,
            }
        )
        job_rows.extend(trial_rows)
    if not trials:
        raise ValueError(f"No */agent/sessions directories in {job}")
    connections, totals = aggregate(job_rows, prices=prices)
    return {"trials": trials, "usage_by_connection": connections, **totals}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("job", type=Path, help="completed Harbor job directory")
    parser.add_argument(
        "--prices",
        type=Path,
        help="saved LiteLLM price JSON (default: packaged backup)",
    )
    args = parser.parse_args(argv)
    path = args.prices or price_path()
    report = recalculate(args.job, prices=load_prices(path))
    print(
        json.dumps({"price_table": str(path), **report}, ensure_ascii=False, indent=2)
    )


if __name__ == "__main__":
    main()
