#!/usr/bin/env python3
"""Strict acceptance parser for the M8 fixed host-local binary staging launcher.

The frozen contract lives in ``docs/multi-region/M8_BINARY_STAGE_HOST_LAUNCHER.md``
(section "Worker B").  This module is verification-only: it decides, from the
``go test -json`` stream of the authorized isolated Linux gate, whether the
mandatory leaf set really ran and passed with a coherent lifecycle.  It makes no
application decisions, performs no I/O beyond the one input file the caller
names, and never echoes raw log text or exception messages -- refusals carry a
fixed reason code plus compact integer counts only.

CLI::

    python3 scripts/m8_binary_stage_host_acceptance.py \\
        --go-json PATH --scenario positive --repetitions 3 --process-exit 0

Standard library only.  Exit status is ``0`` only for an accepted run.
"""
from __future__ import annotations

import argparse
import json
import math
import re
import sys
from typing import Any, Dict, Iterator, List, Optional, Tuple

PACKAGE = "github.com/fiztoz/uptime-phoenix/internal/bootstrap"
TOP_LEVEL = "TestProbeBinaryStageHostSandbox"
POSITIVE_SCENARIO = "positive"

#: Mandatory leaves under ``TestProbeBinaryStageHostSandbox/`` in the positive
#: namespace, exactly as frozen in the contract's acceptance section.
POSITIVE_LEAVES: Tuple[str, ...] = (
    "ParentFDAttributes",
    "ActualAdmin/amd64",
    "ActualAdmin/arm64",
    "ReplayPreservesForeign",
    "HeaderRefusal",
    "BodyRefusal",
    "WithheldEOFSignal",
    "UnknownOutputRetainsGraph",
    "UsageBeforeRoot",
    "SourceCloseBeforeMutation",
)

#: Named negative namespaces; each requires its refusal leaf and top-level run.
NEGATIVE_LEAVES: Dict[str, str] = {
    "unsafe_owner": "RefusesParent/unsafe_owner",
    "missing_opt": "RefusesParent/missing_opt",
    "symlink_opt": "RefusesParent/symlink_opt",
    "regular_opt": "RefusesParent/regular_opt",
    "nonroot": "RefusesParent/nonroot",
    "unsafe_root_owner": "RefusesParent/unsafe_root_owner",
    "unsafe_root_mode": "RefusesParent/unsafe_root_mode",
}

SCENARIOS: Tuple[str, ...] = (POSITIVE_SCENARIO,) + tuple(NEGATIVE_LEAVES)

ACTIONS_WITH_TEST = frozenset({"run", "pause", "cont", "pass", "fail", "skip", "output"})
ACTIONS_WITHOUT_TEST = frozenset({"start", "output", "pass", "fail", "skip"})
VALID_ACTIONS = ACTIONS_WITH_TEST | ACTIONS_WITHOUT_TEST

EVENT_KEYS = frozenset({"Time", "Action", "Package", "Test", "Elapsed", "Output"})

COUNT_KEYS: Tuple[str, ...] = (
    "events",
    "test_runs",
    "test_passes",
    "package_passes",
    "required_leaves",
    "required_leaf_passes",
    "repetitions",
)

#: ``go test -json`` timestamps are RFC3339 (nanoseconds optional, offset or Z).
TIMESTAMP = re.compile(
    r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})"
)

#: Fixed output marker sets, checked in this order for a deterministic reason.
OUTPUT_MARKERS: Tuple[Tuple[str, Tuple[str, ...]], ...] = (
    ("panic_output", ("panic: ", "fatal error: ")),
    ("race_output", ("DATA RACE",)),
    ("timeout_output", ("test timed out",)),
    ("no_tests_output", ("no test files", "no tests to run")),
    ("failure_output", ("--- FAIL:",)),
)


def required_leaves(scenario: str) -> Tuple[str, ...]:
    """Return the mandatory full leaf names for a frozen scenario name."""
    if scenario == POSITIVE_SCENARIO:
        return (TOP_LEVEL,) + tuple(TOP_LEVEL + "/" + leaf for leaf in POSITIVE_LEAVES)
    return (TOP_LEVEL, TOP_LEVEL + "/" + NEGATIVE_LEAVES[scenario])


def ancestors(test_name: str) -> Iterator[str]:
    """Yield every proper ancestor path of a slash-separated subtest name."""
    parts = test_name.split("/")
    for index in range(1, len(parts)):
        yield "/".join(parts[:index])


def counts(**overrides: int) -> Dict[str, int]:
    """Return a fresh compact count record, optionally overridden."""
    record = {key: 0 for key in COUNT_KEYS}
    record.update(overrides)
    return record


def result(passed: bool, reason: str, observed: Optional[Dict[str, int]] = None) -> Dict[str, Any]:
    """Build the fixed result shape: status, one reason code, compact counts."""
    return {"passed": bool(passed), "reason": reason, "counts": dict(observed or counts())}


def _integer(value: Any) -> bool:
    """True for a real int (``bool`` is explicitly not an int here)."""
    return isinstance(value, int) and not isinstance(value, bool)


def _validate_event(event: Dict[str, Any]) -> Optional[str]:
    """Validate one parsed event's shape; return a fixed reason code or None."""
    if set(event) - EVENT_KEYS:
        return "invalid_event_field"

    stamp = event.get("Time")
    if not isinstance(stamp, str) or TIMESTAMP.fullmatch(stamp) is None:
        return "invalid_event_field"

    action = event.get("Action")
    if not isinstance(action, str) or action not in VALID_ACTIONS:
        return "invalid_action"

    package = event.get("Package")
    if not isinstance(package, str) or not package:
        return "invalid_event_field"
    if package != PACKAGE:
        return "foreign_package"

    test = event.get("Test")
    if test is None:
        if action not in ACTIONS_WITHOUT_TEST:
            return "invalid_event_field"
    else:
        if not isinstance(test, str) or not test:
            return "invalid_event_field"
        if action not in ACTIONS_WITH_TEST:
            return "invalid_event_field"

    output = event.get("Output")
    if output is not None and not isinstance(output, str):
        return "invalid_event_field"

    elapsed = event.get("Elapsed")
    if elapsed is not None:
        if not isinstance(elapsed, (int, float)) or isinstance(elapsed, bool):
            return "invalid_event_field"
        try:
            numeric = float(elapsed)
        except (OverflowError, ValueError):
            return "invalid_event_field"
        if not math.isfinite(numeric) or numeric < 0:
            return "invalid_event_field"

    return None


def audit_events(
    text: str,
    scenario: str,
    repetitions: int,
    process_exit: int,
) -> Dict[str, Any]:
    """Audit one ``go test -json`` stream against one frozen scenario.

    Returns the fixed result shape and never raises: every rejected input maps to
    a fixed reason code, so no raw output or parser exception can escape.
    """
    observed = counts()

    if not _integer(repetitions) or repetitions < 1:
        return result(False, "invalid_repetitions", observed)
    observed["repetitions"] = repetitions

    if not _integer(process_exit):
        return result(False, "invalid_process_exit", observed)

    if not isinstance(scenario, str) or scenario not in SCENARIOS:
        return result(False, "unknown_scenario", observed)

    if not isinstance(text, str):
        return result(False, "invalid_args", observed)

    if process_exit != 0:
        return result(False, "nonzero_process_exit", observed)

    required = required_leaves(scenario)
    allowed_parents = set()
    for name in required:
        allowed_parents.update(ancestors(name))
    allowed_parents.difference_update(required)

    tracker: Dict[str, Dict[str, int]] = {}
    package_passes = 0
    test_events = 0

    for line in text.splitlines():
        stripped = line.strip()
        if not stripped:
            continue
        try:
            event = json.loads(stripped)
        except Exception:  # noqa: BLE001 - any decoder pathology is a malformed line
            return result(False, "malformed_event", observed)
        if not isinstance(event, dict):
            return result(False, "malformed_event", observed)

        observed["events"] += 1
        reason = _validate_event(event)
        if reason is not None:
            return result(False, reason, observed)

        action = event["Action"]
        test = event.get("Test")
        output = event.get("Output")

        if action == "fail":
            return result(False, "failure_event", observed)
        if action == "skip":
            return result(False, "skip_event", observed)
        if output:
            for marker_reason, markers in OUTPUT_MARKERS:
                if any(marker in output for marker in markers):
                    return result(False, marker_reason, observed)

        if test is None:
            if action == "pass":
                package_passes += 1
                observed["package_passes"] = package_passes
            continue

        test_events += 1
        slot = tracker.get(test)
        if slot is None:
            slot = tracker[test] = {"runs": 0, "passes": 0, "pending": 0}

        if action == "run":
            slot["runs"] += 1
            slot["pending"] += 1
            observed["test_runs"] += 1
        elif action == "pass":
            if slot["runs"] == 0:
                return result(False, "pass_before_run", observed)
            if slot["pending"] <= 0:
                return result(False, "duplicate_terminal", observed)
            slot["pending"] -= 1
            slot["passes"] += 1
            observed["test_passes"] += 1

    if observed["events"] == 0:
        return result(False, "empty_output", observed)

    if any(slot["pending"] != 0 for slot in tracker.values()):
        return result(False, "unmatched_pending_run", observed)

    if test_events == 0:
        return result(False, "parent_only_coverage", observed)

    if package_passes == 0:
        return result(False, "missing_package_pass", observed)
    if package_passes > 1:
        return result(False, "duplicate_package_pass", observed)

    observed["required_leaves"] = len(required)
    leaf_passes = 0
    for name in required:
        slot = tracker.get(name)
        if slot is None or slot["passes"] == 0:
            observed["required_leaf_passes"] = leaf_passes
            return result(False, "missing_leaf", observed)
        if slot["passes"] != repetitions:
            observed["required_leaf_passes"] = leaf_passes
            return result(False, "leaf_pass_count_mismatch", observed)
        if slot["runs"] != repetitions:
            observed["required_leaf_passes"] = leaf_passes
            return result(False, "leaf_run_count_mismatch", observed)
        leaf_passes += slot["passes"]
    observed["required_leaf_passes"] = leaf_passes

    for name in tracker:
        if name in required or name in allowed_parents:
            continue
        return result(False, "excess_leaf", observed)

    return result(True, "ok", observed)


def emit(value: Dict[str, Any]) -> int:
    """Write the compact fixed-shape JSON result and return the exit status."""
    sys.stdout.write(json.dumps(value, separators=(",", ":"), sort_keys=True) + "\n")
    sys.stdout.flush()
    return 0 if value.get("passed") is True else 1


def main(argv: Optional[List[str]] = None) -> int:
    """Parse the command line, audit the named stream, and report the verdict."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-json", required=True, help="go test -json output to audit")
    parser.add_argument("--scenario", required=True, help="positive or a named negative namespace")
    parser.add_argument("--repetitions", required=True, help="positive repetition count")
    parser.add_argument("--process-exit", required=True, help="exit status of the go test process")
    args = parser.parse_args(argv)

    try:
        repetitions = int(args.repetitions, 10)
    except (TypeError, ValueError):
        return emit(result(False, "invalid_repetitions"))

    try:
        process_exit = int(args.process_exit, 10)
    except (TypeError, ValueError):
        return emit(result(False, "invalid_process_exit"))

    try:
        with open(args.go_json, "r", encoding="utf-8") as stream:
            text = stream.read()
    except (OSError, UnicodeError):
        return emit(result(False, "unreadable_input"))

    try:
        verdict = audit_events(text, args.scenario, repetitions, process_exit)
    except Exception:  # noqa: BLE001 - refusal must never leak a traceback or message
        return emit(result(False, "internal_error"))
    return emit(verdict)


if __name__ == "__main__":
    raise SystemExit(main())