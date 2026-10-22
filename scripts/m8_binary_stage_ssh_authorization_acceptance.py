#!/usr/bin/env python3
"""Fixed actual-OpenSSH restricted-key contract; not a configurable subset gate."""
from __future__ import annotations

from m8_binary_stage_ssh_acceptance import audit_named_events

TOP = "TestSSHBinaryStageAuthorization"
REQUIRED = (
    TOP,
    TOP + "/Fixture",
    TOP + "/Sender/amd64",
    TOP + "/Sender/arm64",
    TOP + "/ForcedExec",
    TOP + "/ForcedShell",
    TOP + "/RestrictedSubsystem",
    TOP + "/ControlCommand",
    TOP + "/ControlSubsystem",
    TOP + "/Forwarding",
    TOP + "/ForcedOnlyRootLogin",
    TOP + "/ReplayPreservesForeign",
)
ALLOWED = REQUIRED + (TOP + "/Sender",)


def audit_events(text: str, repetitions: int, exit_code: int) -> dict:
    """Require the entire fixed authorization matrix, never just named totals."""
    return audit_named_events(text, repetitions, exit_code, top=TOP,
                              required=REQUIRED, allowed=ALLOWED)
