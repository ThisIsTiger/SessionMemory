#!/usr/bin/env python3
"""Reproduce review findings without adding failing tests to the normal suite."""

import argparse
import collections
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--count", type=int, default=1)
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    if args.count < 1:
        parser.error("--count must be positive")
    here = Path(__file__).resolve().parent
    root = here.parent.parent
    out = args.out or Path(tempfile.mkdtemp(prefix="session-memory-audit-"))
    out.mkdir(parents=True, exist_ok=True)
    out = out.resolve()
    packages = {
        # A01-A04, A11, A12 and A14 are fixed; their tests are in the normal suite.
        "engine": "internal/engine",
        "transport": "internal/transport",
        "workbuddy": "internal/source/workbuddy",
        "cli": "cmd/session-memory",
    }
    overlay = out / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(root / path / "audit_overlay_test.go"): str(here / (name + "_test.go.txt"))
        for name, path in packages.items()
    }}, indent=2))
    cmd = ["go", "test", "-tags", "dev,failpoint", "-overlay", str(overlay),
           "-run", "^TestAudit", "-count", str(args.count), "-timeout", "5m", "-json"]
    if args.race:
        cmd.append("-race")
    cmd += ["./" + p for p in packages.values()]
    env = os.environ.copy()
    # The audit uses temporary data and local HTTP fixtures, never a real
    # cloud token or an automatically enabled client/model integration.
    for name in ("SM_LIVE_OUT", "SM_LIVE_CODEX", "SM_LIVE_WEIYUN", "SM_REVIEW_LIVE",
                 "WEIYUN_MCP_TOKEN", "WEIYUN_MCP_TOKEN_2", "WEIYUN_MCP_URL",
                 "WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR"):
        env[name] = ""
    log = out / "audit.jsonl"
    with log.open("w") as stream:
        result = subprocess.run(cmd, cwd=root, env=env, stdout=stream,
                                stderr=subprocess.STDOUT, check=False)
    counts = collections.Counter()
    failures = collections.Counter()
    build_errors = []
    races = []
    for line in log.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        test = event.get("Test")
        action = event.get("Action")
        if test and action in ("pass", "fail", "skip"):
            counts[action] += 1
            if action == "fail":
                failures[test] += 1
        if event.get("FailedBuild") or action == "build-fail":
            build_errors.append(event)
        if "DATA RACE" in event.get("Output", ""):
            races.append(test)
    summary = {
        "command": cmd,
        "returncode": result.returncode,
        "test_and_subtest_events": dict(counts),
        "failing_tests": dict(sorted(failures.items())),
        "distinct_failing_top_level_tests": len([t for t in failures if "/" not in t]),
        "build_errors": build_errors,
        "data_races": races,
        "log": str(log),
    }
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    # A nonzero exit is intentional while defects remain. Build failures are
    # not evidence that a finding has reproduced: inspect build_errors first.
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
