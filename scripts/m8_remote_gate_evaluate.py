#!/usr/bin/env python3
"""Evaluate a remote m8-stage-authorization Go JSON event log with the gate's rules.

The dual-engine gate runs `go test` locally against its own disposable MariaDB.
For a remote host (for example the AWS acceptance run), test binaries run there
with `-test.v=test2json`, are converted locally with `go tool test2json`, and the
concatenated event file is judged here by the same strict lifecycle validator,
required leaf set, skip and failure rules. It runs no command and opens no DB.
Remote cross-compiled binaries are not race-enabled; the summary says so.
"""
import argparse
import collections
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import m6_dual_engine_gate as gate  # noqa: E402


def evaluate(lines):
    required = (gate.INSTALL_JOURNAL_REQUIRED | gate.INSTALL_RUNNER_REQUIRED |
                gate.STAGE_AUTHORIZATION_REQUIRED | gate.HOST_ACTION_REQUIRED | gate.ACTIVATION_REQUIRED)
    strict = gate.StageAuthorizationEvents(required)
    results, mariadb = collections.Counter(), collections.Counter()
    passed, failed, skipped, packages = set(), [], [], set()
    malformed = 0
    for line in lines:
        try:
            event = json.loads(line, object_pairs_hook=gate.unique_event_object)
        except ValueError:
            malformed += 1
            continue
        if (not isinstance(event, dict) or not isinstance(event.get("Action"), str)
                or not isinstance(event.get("Package"), str)
                or not isinstance(event.get("Test", ""), str)):
            malformed += 1
            continue
        if not line.endswith("\n"):
            strict.refuse("unterminated event line")
        strict.observe(event)
        action, test = event["Action"], event.get("Test", "")
        if test and action in ("pass", "fail", "skip"):
            results[action] += 1
            if "/mariadb" in test:
                mariadb[action] += 1
            {"pass": passed.add, "fail": failed.append, "skip": skipped.append}[action](test)
        if not test and action == "pass":
            packages.add(event["Package"])
    lifecycle_errors, missing_lifecycles = strict.finish()
    summary = {
        "scope": "m8-stage-authorization (remote evaluation, not race-enabled)",
        "passed_packages": len(packages), "named_passes": results["pass"],
        "named_failures": results["fail"], "named_skips": results["skip"],
        "mariadb_named_passes": mariadb["pass"], "mariadb_named_skips": mariadb["skip"],
        "required": len(required), "missing_required": sorted(required - passed),
        "failed_tests": sorted(failed)[:12], "skipped_tests": sorted(skipped)[:12],
        "malformed_json_lines": malformed, "lifecycle_errors": lifecycle_errors,
        "missing_required_lifecycles": missing_lifecycles,
    }
    ok = not (failed or malformed or lifecycle_errors or missing_lifecycles or skipped
              or mariadb["pass"] < len(required) or required - passed)
    summary["result"] = "passed" if ok else "failed"
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("events", type=Path, help="concatenated go test -json events")
    args = parser.parse_args()
    with args.events.open() as handle:
        summary = evaluate(handle)
    print(json.dumps(summary, indent=2))
    return 0 if summary["result"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
