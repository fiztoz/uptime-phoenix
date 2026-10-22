#!/usr/bin/env python3
"""Strict completion audit for the dedicated actual OpenSSH M8 stage fixture."""
from __future__ import annotations

import json
import re

PACKAGE = "github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
TOP = "TestSSHBinaryStageOpenSSH"
REQUIRED = (
    TOP,
    TOP + "/Fixture",
    TOP + "/ActualHelper/amd64",
    TOP + "/ActualHelper/arm64",
    TOP + "/ReplayPreservesForeign",
    TOP + "/PostStageNonzero",
    TOP + "/PostStageLostReceipt",
    TOP + "/LastByteDisconnectUnknown",
)
ALLOWED = REQUIRED + (TOP + "/ActualHelper",)
BAD_OUTPUT = re.compile(r"panic:|fatal error:|DATA RACE|test timed out|no tests to run|\[no test files\]", re.I)


def audit_named_events(text: str, repetitions: int, exit_code: int, *, top: str,
                       required: tuple[str, ...], allowed: tuple[str, ...]) -> dict:
    """Audit a fixed caller-owned name contract, including every immediate parent."""
    errors: list[str] = []
    if (not top or top not in required or not set(required) <= set(allowed) or
            len(set(required)) != len(required) or len(set(allowed)) != len(allowed) or
            any(name != top and (not name.startswith(top + "/") or
                name.rsplit("/", 1)[0] not in allowed) for name in allowed)):
        errors.append("invalid_named_contract")
    runs = dict.fromkeys(allowed, 0)
    passes = dict.fromkeys(allowed, 0)
    active: set[str] = set()
    package_passes = 0
    package_done = False
    iteration_runs: set[str] = set()
    iteration_passes: set[str] = set()
    if repetitions < 1 or exit_code != 0:
        errors.append("normal_successful_process_required")
    if not text or not text.endswith("\n"):
        errors.append("complete_json_lines_required")
    for line in text.splitlines():
        try:
            event = json.loads(line)
            if not isinstance(event, dict) or event.get("Package") != PACKAGE:
                errors.append("wrong_event_package")
                continue
            action = event.get("Action")
            name = event.get("Test")
            if action in ("fail", "skip"):
                errors.append("failed_or_skipped_event")
            if BAD_OUTPUT.search(event.get("Output", "")):
                errors.append("abnormal_test_output")
            if package_done:
                errors.append("event_after_package_completion")
            if name is not None:
                if name not in runs:
                    errors.append("unexpected_test_name")
                    continue
                if action == "run":
                    if name in active or runs[name] >= repetitions:
                        errors.append("duplicate_or_overlapping_run")
                    if name == top:
                        iteration_runs = set()
                        iteration_passes = set()
                    if name in iteration_runs:
                        errors.append("duplicate_test_in_repetition")
                    iteration_runs.add(name)
                    if name != top and top not in active:
                        errors.append("child_without_active_top")
                    if name != top and name.rsplit("/", 1)[0] not in active:
                        errors.append("child_without_active_parent")
                    runs[name] += 1
                    active.add(name)
                elif action == "pass":
                    if name not in active or any(child.startswith(name + "/") for child in active):
                        errors.append("pass_without_joined_run")
                    if name == top and (iteration_runs != set(allowed) or iteration_passes != set(allowed) - {top}):
                        errors.append("incomplete_repetition")
                    if name in iteration_passes:
                        errors.append("duplicate_pass_in_repetition")
                    iteration_passes.add(name)
                    active.discard(name)
                    passes[name] += 1
                elif action not in ("output", "fail", "skip"):
                    errors.append("unsupported_test_action")
            elif action == "pass":
                package_passes += 1
                package_done = True
                if active:
                    errors.append("package_with_active_tests")
            elif action not in ("start", "output", "fail", "skip"):
                errors.append("unsupported_package_action")
        except (ValueError, TypeError, AttributeError):
            errors.append("malformed_json_event")
    if active or package_passes != 1:
        errors.append("package_completion_missing")
    if any(runs[name] != repetitions or passes[name] != repetitions for name in allowed):
        errors.append("required_named_completion_missing")
    return {"passed": not errors, "errors": sorted(set(errors)), "repetitions": repetitions,
            "package_passes": package_passes, "required_leaves": len(required),
            "required_leaf_passes": sum(passes.get(name, 0) for name in required),
            "test_runs": sum(runs.values()), "test_passes": sum(passes.values()),
            "runs": runs, "passes": passes}


def audit_events(text: str, repetitions: int, exit_code: int) -> dict:
    """Preserve the original fixed sender matrix and public audit API."""
    return audit_named_events(text, repetitions, exit_code, top=TOP,
                              required=REQUIRED, allowed=ALLOWED)
