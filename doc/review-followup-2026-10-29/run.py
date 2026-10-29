#!/usr/bin/env python3
"""Reproduce the three residual fix defects using isolated Go overlays."""

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
    out = args.out or Path(tempfile.mkdtemp(prefix="session-memory-followup-"))
    out.mkdir(parents=True, exist_ok=True)
    out = out.resolve()
    packages = ["streamcap", "sync", "transport"]
    overlay = out / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(root / "internal" / name / "followup_overlay_test.go"):
        str(here / (name + "_test.go.txt")) for name in packages
    }}, indent=2))
    command = ["go", "test", "-tags", "dev,failpoint", "-overlay", str(overlay),
               "-run", "^TestFollowup", "-count", str(args.count), "-timeout", "3m", "-json"]
    if args.race:
        command.append("-race")
    command += ["./internal/" + name for name in packages]
    env = os.environ.copy()
    for name in ("SM_LIVE_OUT", "SM_LIVE_CODEX", "SM_LIVE_WEIYUN", "SM_REVIEW_LIVE",
                 "WEIYUN_MCP_TOKEN", "WEIYUN_MCP_TOKEN_2", "WEIYUN_MCP_URL",
                 "WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR"):
        env[name] = ""
    log = out / "followup.jsonl"
    with log.open("w") as stream:
        process = subprocess.run(command, cwd=root, env=env, stdout=stream,
                                 stderr=subprocess.STDOUT, check=False)
    counts, failures = collections.Counter(), collections.Counter()
    build_errors, races = [], []
    for line in log.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        test, action = event.get("Test"), event.get("Action")
        if test and action in ("pass", "fail", "skip"):
            counts[action] += 1
            if action == "fail":
                failures[test] += 1
        if event.get("FailedBuild") or action == "build-fail":
            build_errors.append(event)
        if "DATA RACE" in event.get("Output", ""):
            races.append(test)
    summary = {"command": command, "returncode": process.returncode,
               "test_and_subtest_events": dict(counts), "failing_tests": dict(failures),
               "distinct_failing_top_level_tests": sum("/" not in t for t in failures),
               "build_errors": build_errors, "data_races": races, "log": str(log)}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    # With unfixed business code, failed safety assertions intentionally yield
    # a nonzero exit. A build failure is not a reproduced finding.
    return process.returncode


if __name__ == "__main__":
    sys.exit(main())
