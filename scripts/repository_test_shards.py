#!/usr/bin/env python3
"""Partition complete repository tests and prove real-engine CI coverage.

Only the expensive parent repository package is partitioned. Every selected
parent runs all its subtests, with race detection and the unchanged 40m timeout.
Each CI shard must have its own disposable MariaDB; never run concurrent shards
against one DSN. Artifacts contain names/timings, not test output or credentials.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import m6_dual_engine_gate as gate

ROOT = Path(__file__).resolve().parents[1]
PACKAGE = "github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
TARGET = "./internal/adapters/repository"
NAME = re.compile(r"(?:Test|Example|Fuzz)\w+\Z")


def partition(names, count):
    """Assign every discovered parent exactly once, including future tests."""
    if type(count) is not int or count < 1 or count > len(names):
        raise ValueError("shard count must be positive and cannot create empty shards")
    if len(names) != len(set(names)) or any(not NAME.fullmatch(n) for n in names):
        raise ValueError("invalid or duplicate discovered test names")
    ordered = sorted(names)
    return [ordered[index::count] for index in range(count)]


def discover():
    """Use the compiled Go inventory, not a hand-maintained test allowlist."""
    proc = subprocess.run(
        ["go", "test", "-json", "-list", ".", TARGET], cwd=ROOT,
        text=True, capture_output=True, check=False,
    )
    if proc.returncode:
        raise ValueError("repository test discovery failed")
    names = []
    package_pass = False
    for line in proc.stdout.splitlines():
        event = json.loads(line)
        if event.get("Package") != PACKAGE:
            raise ValueError("foreign package in test inventory")
        if event.get("Action") == "pass" and not event.get("Test"):
            package_pass = True
        if event.get("Action") == "output":
            name = event.get("Output", "").strip()
            if NAME.fullmatch(name):
                names.append(name)
    if not package_pass or not names:
        raise ValueError("empty or incomplete repository test inventory")
    partition(names, 1)
    return sorted(names)


def summarize(lines, inventory, index, count, source_sha):
    """Keep auditable coverage metadata without persisting raw test output."""
    selected = partition(inventory, count)[index]
    report = {
        "version": 1, "package": PACKAGE, "source_sha": source_sha,
        "index": index, "count": count, "inventory": inventory,
        "selected": selected, "runs": [], "passed": [], "failed": [],
        "skipped": [], "durations": {}, "malformed_json": 0,
        "foreign_package": False, "package_pass": False,
        "package_fail": False,
    }
    actions = {"run": "runs", "pass": "passed", "fail": "failed", "skip": "skipped"}
    for line in lines:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            report["malformed_json"] += 1
            continue
        if event.get("Package") != PACKAGE:
            report["foreign_package"] = True
            continue
        action, test = event.get("Action"), event.get("Test")
        if test and action in actions:
            report[actions[action]].append(test)
            if action == "pass" and "/" not in test:
                report["durations"][test] = event.get("Elapsed", 0)
        elif not test and action in ("pass", "fail"):
            report["package_" + action] = True
    return report


def validate_report(report, inventory, count, source_sha):
    """Refuse zero selection, partial execution, wrong provenance and skips."""
    index = report["index"]
    if type(index) is not int or not 0 <= index < count:
        raise ValueError("invalid shard index")
    selected = partition(inventory, count)[index]
    if (report["version"] != 1 or report["package"] != PACKAGE
            or report["source_sha"] != source_sha or report["count"] != count
            or report["inventory"] != inventory or report["selected"] != selected):
        raise ValueError("shard provenance or inventory mismatch")
    if (type(report["go_exit"]) is not int or report["go_exit"] != 0
            or report["malformed_json"] != 0 or report["foreign_package"]
            or not report["package_pass"] or report["package_fail"] or report["failed"]):
        raise ValueError("shard failed or produced incomplete Go events")
    for key in ("runs", "passed", "failed", "skipped"):
        values = report[key]
        if (not isinstance(values, list) or len(values) != len(set(values))
                or any(not isinstance(n, str) or n.split("/")[0] not in selected for n in values)):
            raise ValueError("unexpected or duplicate executed test")
    passed = set(report["passed"])
    runs = set(report["runs"])
    if not set(selected) <= runs or not set(selected) <= passed or not passed <= runs:
        raise ValueError("selected parents did not actually execute and pass")
    skipped = set(report["skipped"])
    if (skipped - gate.SKIPS_ALLOWED or any("/mariadb" in n for n in skipped)
            or not any("/mariadb" in n for n in passed)):
        raise ValueError("missing real MariaDB coverage or unrecognized skips")
    required = {n for n in gate.REQUIRED if n.split("/")[0] in selected}
    if required - passed:
        raise ValueError("missing required real-engine leaves: " + ", ".join(sorted(required - passed)))


def verify_reports(reports, inventory, count, source_sha):
    """Fail closed unless all matrix shards cover the fresh inventory once."""
    if len(reports) != count:
        raise ValueError("missing or extra shard reports")
    indices = set()
    for report in reports:
        validate_report(report, inventory, count, source_sha)
        if report["index"] in indices:
            raise ValueError("duplicate shard report")
        indices.add(report["index"])
    if indices != set(range(count)):
        raise ValueError("incomplete shard indices")
    passed = set().union(*(set(r["passed"]) for r in reports))
    required = {n for n in gate.REQUIRED if n.split("/")[0] in inventory}
    return {
        "result": "passed", "shards": count, "parents": len(inventory),
        "named_passes": sum(len(r["passed"]) for r in reports),
        "mariadb_named_passes": sum("/mariadb" in n for r in reports for n in r["passed"]),
        "required_leaves": len(required), "missing_required": sorted(required - passed),
    }


def run_shard(args):
    if not os.environ.get("TEST_MARIADB_DSN"):
        raise ValueError("TEST_MARIADB_DSN must name a disposable MariaDB before execution")
    inventory = discover()
    shards = partition(inventory, args.count)
    if not 0 <= args.index < args.count:
        raise ValueError("invalid shard index")
    selected = shards[args.index]
    pattern = "^(" + "|".join(re.escape(n) for n in selected) + ")$"
    print(f"repository shard {args.index}/{args.count}: {len(selected)} complete parents", flush=True)
    cmd = ["go", "test", "-race", "-count=1", "-json", "-timeout", "40m",
           "-run", pattern, TARGET]
    with subprocess.Popen(cmd, cwd=ROOT, text=True, stdout=subprocess.PIPE) as proc:
        def events():
            for line in proc.stdout:
                try:
                    event = json.loads(line)
                    if event.get("Action") == "output":
                        print(event.get("Output", ""), end="", flush=True)
                except json.JSONDecodeError:
                    pass  # summarize records malformed input as a failure
                yield line
        report = summarize(events(), inventory, args.index, args.count, args.source_sha)
        report["go_exit"] = proc.wait()
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    validate_report(report, inventory, args.count, args.source_sha)
    print(f"PASS repository shard {args.index}: complete parents and exact real-engine leaves")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("run", "verify", "plan"))
    parser.add_argument("--count", type=int, default=4)
    parser.add_argument("--index", type=int, default=0)
    parser.add_argument("--source-sha", default="")
    parser.add_argument("--report", type=Path)
    parser.add_argument("--reports", type=Path)
    args = parser.parse_args(argv)
    try:
        if args.mode == "plan":
            names = discover()
            print(json.dumps({"inventory": names, "shards": partition(names, args.count)}, indent=2))
        else:
            if not re.fullmatch(r"[0-9a-f]{40}", args.source_sha):
                raise ValueError("source SHA must be an exact commit")
            if args.mode == "run":
                if args.report is None:
                    raise ValueError("run requires --report")
                run_shard(args)
            else:
                if args.reports is None:
                    raise ValueError("verify requires --reports")
                reports = [json.loads(p.read_text()) for p in sorted(args.reports.glob("*.json"))]
                print(json.dumps(verify_reports(reports, discover(), args.count, args.source_sha), indent=2))
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as exc:
        # Do not surface subprocess environments or credential-bearing DSNs.
        print(f"repository shard gate refused: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
