#!/usr/bin/env python3
"""Build the M4 certificate paging evidence manifest.

Reads a `go test -json -race` run that was executed against a disposable
MariaDB schema through TEST_MARIADB_DSN, plus the compiled working tree, and
emits the hashed manifest that docs/multi-region/M4_CERT_PAGING.md cites.

This gate deliberately fails when the MariaDB coverage it is claiming did not
actually execute: a package that skipped its real-engine contracts is not
acceptance evidence (AGENTS.md rule 13).

Usage:
  python3 scripts/m4_cert_paging_evidence.py \
      --gate /tmp/m4cert_final.json \
      --mariadb-version 11.8.9 \
      > docs/multi-region/M4_CERT_PAGING_EVIDENCE.json
"""

from __future__ import annotations

import argparse
import collections
import hashlib
import json
import os
import subprocess
import sys

# Named cases that must execute on the real production engine for this slice.
REQUIRED_MARIADB_CASES = [
    "TestProbeCertificatePagingAcceptance/mariadb/MirrorLifecycleWithoutHubProviderWork",
    "TestProbeCertificatePagingAcceptance/mariadb/SubjectIdentityIsImmutable",
    "TestProbeCertificatePagingAcceptance/mariadb/AcknowledgementAndMalformedSubjectRejected",
    "TestProbeCertificatePagingAcceptance/mariadb/DeliveryWithoutAuthorizedTransition",
    "TestProbeCertificatePagingAcceptance/mariadb/MigrationRoundTripAndCertificateGuard",
]

SLICE_TEST_MARKERS = ("CertAlertPaging", "CertificatePaging", "CertificateIncident", "EdgeCertificatePaging")


def sha256(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(65536), b""):
            digest.update(block)
    return digest.hexdigest()


def changed_files() -> list[str]:
    """Every file this slice owns: tracked modifications plus new files."""
    tracked = subprocess.run(["git", "diff", "--name-only", "HEAD"], check=True, capture_output=True, text=True).stdout
    untracked = subprocess.run(
        ["git", "ls-files", "--others", "--exclude-standard"], check=True, capture_output=True, text=True
    ).stdout
    names = sorted({line.strip() for line in (tracked + untracked).splitlines() if line.strip()})
    return [name for name in names if not name.startswith("docs/local/")]


def read_gate(path: str):
    passes, failures, skips = 0, [], collections.Counter()
    passed_tests: set[str] = set()
    packages = collections.Counter()
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            action, test = event.get("Action"), event.get("Test")
            package = (event.get("Package") or "").split("/")[-1]
            if action == "pass" and test:
                passes += 1
                passed_tests.add(test)
                packages[package] += 1
            elif action == "fail":
                failures.append(f"{package}:{test or '<package>'}")
            elif action == "skip" and test:
                skips[test] += 1
    return passes, sorted(failures), dict(skips), passed_tests, packages


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--gate", required=True, help="path to a go test -json race run")
    parser.add_argument("--mariadb-version", required=True)
    parser.add_argument("--baseline", default="")
    parser.add_argument("--date", default="")
    args = parser.parse_args()

    passes, failures, skips, passed, packages = read_gate(args.gate)

    missing = [case for case in REQUIRED_MARIADB_CASES if case not in passed]
    slice_cases = sorted(name for name in passed if any(marker in name for marker in SLICE_TEST_MARKERS))

    evidence = {
        "date": args.date or "",
        "baseline": args.baseline,
        "scope": "M4 remote certificate paging slice; capacity state and the rest of M4 remain open",
        "toolchain": os.environ.get("GOTOOLCHAIN", "default"),
        "mariadb": args.mariadb_version,
        "commands": [
            "CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...",
            "GOTOOLCHAIN=go1.26.6 go test -json -race -count=1 -timeout=2400s -p 4 ./... (TEST_MARIADB_DSN set to the disposable CI schema)",
            "GOTOOLCHAIN=go1.26.6 golangci-lint run --timeout=8m",
            "gofmt -l internal && git diff --check && docs link check",
        ],
        "race": {
            "named_passes": passes,
            "packages_with_named_passes": len(packages),
            "failures": len(failures),
            "failure_detail": failures[:25],
            "skipped": sorted(skips),
        },
        "mariadb_coverage": {
            "required_named_cases": REQUIRED_MARIADB_CASES,
            "missing_required_cases": missing,
            "slice_named_cases_total": len(slice_cases),
            "slice_named_mariadb_cases": len([name for name in slice_cases if "/mariadb" in name]),
        },
        "slice_named_passes": slice_cases,
        "source_hashes": {name: sha256(name) for name in changed_files()},
    }

    json.dump(evidence, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    if failures:
        print("gate reported failures; evidence is not acceptance", file=sys.stderr)
        return 1
    if missing:
        print("required MariaDB cases did not execute: " + ", ".join(missing), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
