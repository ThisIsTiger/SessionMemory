#!/usr/bin/env python3
"""Run release-review assertions with synthetic credentials and isolated fixtures."""
import argparse
import collections
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--count", type=int, default=1)
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    if args.count < 1:
        parser.error("--count must be positive")
    here = Path(__file__).resolve().parent
    root = here.parent.parent
    out = (args.out or Path(tempfile.mkdtemp(prefix="session-memory-release-review-"))).resolve()
    out.mkdir(parents=True, exist_ok=True)
    # S02, S03, U01 and U02 are fixed and S01 is a toolchain pin; their tests are
    # in the normal suite. The original reproductions are kept in fixed/ for the
    # record. Put a *_test.go.txt of a new open finding next to this script to
    # have it run here.
    targets = {"transport_test.go.txt": "internal/transport/release_review_test.go",
               "cli_test.go.txt": "cmd/session-memory/release_review_test.go"}
    replace = {str(root / dest): str(here / name) for name, dest in targets.items() if (here / name).exists()}
    if not replace:
        print(json.dumps({"returncode": 0, "failing_tests": {}, "note": "no open release-review findings; "
                          "regression tests are in the normal suite (original repros: fixed/)"}, indent=2))
        return 0
    overlay = out / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replace}, indent=2))
    command = ["go", "test", "-race", "-tags", "dev,failpoint", "-overlay", str(overlay),
               "-run", "^TestRelease", "-count", str(args.count), "-timeout", "4m", "-json",
               "./internal/transport", "./cmd/session-memory"]
    env = os.environ.copy()
    for key in ("SM_LIVE_OUT", "SM_LIVE_CODEX", "SM_LIVE_WEIYUN", "SM_REVIEW_LIVE",
                "WEIYUN_MCP_TOKEN", "WEIYUN_MCP_TOKEN_2", "WEIYUN_MCP_URL",
                "WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR"):
        env[key] = ""
    log = out / "release-review.jsonl"
    with log.open("w") as stream:
        result = subprocess.run(command, cwd=root, env=env, stdout=stream,
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
    summary = {"command": command, "returncode": result.returncode,
               "test_and_subtest_events": dict(counts), "failing_tests": dict(failures),
               "build_errors": build_errors, "data_races": races, "log": str(log)}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
