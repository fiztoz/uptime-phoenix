#!/usr/bin/env python3
"""Fail closed unless an M5 Go JSON log proves the required read API coverage."""

import argparse
import json
import sys
from pathlib import Path

ROOT = "github.com/fiztoz/uptime-phoenix/internal/"
REQUIRED = {
    (ROOT + "core/services", "TestMonitorRegionalServiceScopeAndFreshness"),
    (ROOT + "core/services", "TestMonitorRegionalServiceLegacyAndFailures"),
    (ROOT + "core/services", "TestRegionalDisplayHealthBoundaries"),
    (ROOT + "core/services", "TestProbeDiagnosticsSummaryBoundaries"),
    (ROOT + "core/services", "TestProbeFleetServiceListAndDetail"),
    (ROOT + "adapters/http/handlers", "TestMonitorRegionalHTTPFixtures"),
    (ROOT + "adapters/http/handlers", "TestMonitorRegionalHTTPErrors"),
    (ROOT + "adapters/http/handlers", "TestMonitorRegionalHTTPUnknownAndEmpty"),
    (ROOT + "adapters/http/handlers", "TestMonitorRegionalHTTPDiagnosticFills"),
    (ROOT + "adapters/http/handlers", "TestProbeFleetHTTPFixtures"),
    (ROOT + "adapters/http/handlers", "TestProbeFleetHTTPErrors"),
    (ROOT + "adapters/http/handlers", "TestProbeFleetHTTPSecretExclusion"),
    (ROOT + "adapters/repository", "TestM5ReadAPI/sqlite"),
    (ROOT + "adapters/repository", "TestM5ReadAPI/mariadb"),
    (ROOT + "adapters/repository", "TestM5FleetAPI/sqlite"),
    (ROOT + "adapters/repository", "TestM5FleetAPI/mariadb"),
}


def audit(path: Path) -> list[str]:
    passed: set[tuple[str, str]] = set()
    packages: set[str] = set()
    errors: list[str] = []
    try:
        with path.open() as log:
            for line in log:
                if not line.strip():
                    continue
                event = json.loads(line)
                package, test = event.get("Package", ""), event.get("Test", "")
                action = event.get("Action")
                if action == "fail":
                    errors.append(f"Failed: {package} {test}")
                if action == "pass":
                    if test:
                        passed.add((package, test))
                    else:
                        packages.add(package)
                if action == "skip" and any(
                    package == required_package
                    and (
                        required_test == test
                        or required_test.startswith(test + "/")
                        or test.startswith(required_test + "/")
                    )
                    for required_package, required_test in REQUIRED
                ):
                    errors.append(f"Required test skipped: {package} {test}")
    except (OSError, ValueError, AttributeError) as error:
        return [f"Unreadable or invalid Go JSON evidence: {error}"]
    errors.extend(f"Missing pass: {package} {test}" for package, test in sorted(REQUIRED - passed))
    errors.extend(f"Package did not finish successfully: {package}" for package in sorted({p for p, _ in REQUIRED} - packages))
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("log", type=Path, help="go test -json output (focused or full suite)")
    args = parser.parse_args()
    errors = audit(args.log)
    if errors:
        print("M5 evidence rejected:\n" + "\n".join(errors), file=sys.stderr)
        return 1
    print("M5 read API evidence passed: service, wire fixtures, SQLite and MariaDB (regional reads + fleet diagnostics).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
