#!/usr/bin/env python3
"""Summarize go test -json durations, without enforcing a time budget."""

import json
import math
from pathlib import Path
import sys


def collect(lines):
    tests, packages = [], []
    for line in lines:
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except ValueError:
            continue  # Build diagnostics or an interrupted final JSON line.
        if not isinstance(event, dict) or event.get("Action") not in ("pass", "fail", "skip"):
            continue
        package, test = event.get("Package"), event.get("Test")
        elapsed = event.get("Elapsed", 0)
        if not isinstance(package, str) or not isinstance(elapsed, (int, float)):
            continue
        if not math.isfinite(elapsed) or elapsed < 0:
            continue
        row = (elapsed, package, test or "", event["Action"])
        if isinstance(test, str) and test:
            tests.append(row)
        elif not test:
            packages.append(row)
    return tests, packages


def cell(value):
    return str(value).replace("\n", " ").replace("|", r"\|")


def summary(lines):
    tests, packages = collect(lines)
    output = ["## Slowest tests (top 20)", "",
              "Tests and subtests are separate rows; package times are not their sum.", "",
              "| Elapsed (s) | Result | Package / test |", "| ---: | --- | --- |"]
    order = lambda row: (-row[0], row[1], row[2])
    for elapsed, package, test, action in sorted(tests, key=order)[:20]:
        output.append(f"| {elapsed:.3f} | {action} | {cell(package)} / {cell(test)} |")
    output += ["", "## Package elapsed time", "",
               "| Elapsed (s) | Result | Package |", "| ---: | --- | --- |"]
    for elapsed, package, _, action in sorted(packages, key=order):
        output.append(f"| {elapsed:.3f} | {action} | {cell(package)} |")
    return "\n".join(output) + "\n"


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    try:
        if not args or args == ["-"]:
            print(summary(sys.stdin), end="")
        else:
            with Path(args[0]).open(encoding="utf-8", errors="replace") as source:
                print(summary(source), end="")
    except (OSError, ValueError) as error:
        print(f"Test duration summary unavailable: {error}", file=sys.stderr)
    return 0  # Reporting is informational, including missing/partial input.


if __name__ == "__main__":
    sys.exit(main())
