#!/usr/bin/env python3
"""Fail-closed govulncheck gate honoring dated unfixed OSV exceptions (#225).

govulncheck has no native ignore file. When vuln.go.dev publishes an advisory
with no fixed version on our module major (e.g. GO-2026-6872 and GO-2026-6875
on github.com/labstack/echo/v4, where vuln.go.dev omitted fixed=4.16.3 and
symbol metadata so every symbol in echo/v4 matches), this wrapper consults
osv-scanner.toml and suppresses ONLY symbol-level findings that:
  1. have no fixed_version in govulncheck's output (Fixed in: N/A),
  2. match an unexpired [[IgnoredVulns]] entry with a non-empty reason, and
  3. are in the exact module and resolved version that govulncheck-scope.toml
     verified for that entry.

Any finding with an available fixed_version, any unlisted symbol finding, any
finding in an unverified module or version, any expired ignoreUntil date, or
any non-zero govulncheck execution error fails closed.
"""

import argparse
from datetime import date, datetime, timezone
import json
from pathlib import Path
import subprocess
import sys
import tomllib


def load_ignores(config_path: Path, today: date | None = None) -> dict[str, dict]:
    if today is None:
        today = datetime.now(timezone.utc).date()
    data = tomllib.loads(config_path.read_text(encoding="utf-8"))
    entries = data.get("IgnoredVulns", [])
    if not isinstance(entries, list):
        raise ValueError("IgnoredVulns must be a list of tables")
    ignores: dict[str, dict] = {}
    for entry in entries:
        vuln_id = str(entry.get("id", "")).strip()
        reason = str(entry.get("reason", "")).strip()
        until = entry.get("ignoreUntil")
        if not vuln_id or not reason or not isinstance(until, date) or isinstance(until, datetime):
            raise ValueError(f"invalid IgnoredVulns entry: {entry!r}")
        if until < today:
            raise ValueError(f"IgnoredVulns entry {vuln_id} expired on {until.isoformat()}")
        ignores[vuln_id] = {
            "id": vuln_id,
            "ignoreUntil": until,
            "reason": reason,
        }
    return ignores


def load_scopes(scope_path: Path, ignores: dict[str, dict]) -> dict[str, dict]:
    data = tomllib.loads(scope_path.read_text(encoding="utf-8"))
    entries = data.get("Scope", [])
    if not isinstance(entries, list):
        raise ValueError("Scope must be a list of tables")
    scopes: dict[str, dict] = {}
    for entry in entries:
        vuln_id = str(entry.get("id", "")).strip()
        module = str(entry.get("module", "")).strip()
        versions = entry.get("versions")
        if (
            not vuln_id
            or not module
            or not isinstance(versions, list)
            or not versions
            or not all(isinstance(v, str) and v.strip() for v in versions)
        ):
            raise ValueError(f"invalid Scope entry: {entry!r}")
        if vuln_id in scopes:
            raise ValueError(f"duplicate Scope entry {vuln_id}")
        if vuln_id not in ignores:
            raise ValueError(f"Scope entry {vuln_id} has no IgnoredVulns entry")
        scopes[vuln_id] = {
            "id": vuln_id,
            "module": module,
            "versions": {v.strip() for v in versions},
        }
    return scopes


def in_scope(finding: dict, scope: dict | None) -> bool:
    if scope is None:
        return False
    frame = (finding.get("trace") or [{}])[0]
    module = str(frame.get("module", "")).strip()
    version = str(frame.get("version", "")).strip()
    return module == scope["module"] and version in scope["versions"]


def iter_json_objects(stream: str):
    decoder = json.JSONDecoder()
    idx = 0
    length = len(stream)
    while idx < length:
        while idx < length and stream[idx].isspace():
            idx += 1
        if idx >= length:
            break
        obj, idx = decoder.raw_decode(stream, idx)
        if isinstance(obj, dict):
            yield obj


def is_symbol_finding(finding: dict) -> bool:
    trace = finding.get("trace") or []
    return bool(trace and isinstance(trace[0], dict) and trace[0].get("function"))


def evaluate_stream(stream: str, ignores: dict[str, dict], scopes: dict[str, dict]) -> tuple[list[dict], set[str]]:
    osv_aliases: dict[str, set[str]] = {}
    findings: list[dict] = []
    for obj in iter_json_objects(stream):
        if "osv" in obj and isinstance(obj["osv"], dict):
            osv = obj["osv"]
            osv_id = str(osv.get("id", "")).strip()
            if osv_id:
                aliases = {osv_id}
                for alias in osv.get("aliases") or []:
                    if isinstance(alias, str) and alias.strip():
                        aliases.add(alias.strip())
                osv_aliases[osv_id] = aliases
        if "finding" in obj and isinstance(obj["finding"], dict):
            findings.append(obj["finding"])

    blocked: list[dict] = []
    suppressed: set[str] = set()
    seen_blocked_ids: set[str] = set()
    for finding in findings:
        if not is_symbol_finding(finding):
            continue
        osv_id = str(finding.get("osv", "")).strip()
        fixed_version = str(finding.get("fixed_version", "")).strip()
        candidate_ids = osv_aliases.get(osv_id, {osv_id})
        matched_ignore = next((ignores[cid] for cid in candidate_ids if cid in ignores), None)
        if (
            matched_ignore is not None
            and not fixed_version
            and in_scope(finding, scopes.get(matched_ignore["id"]))
        ):
            suppressed.add(osv_id)
            continue
        if osv_id not in seen_blocked_ids:
            seen_blocked_ids.add(osv_id)
            blocked.append(finding)
    return blocked, suppressed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--config",
        type=Path,
        default=Path("osv-scanner.toml"),
        help="Path to osv-scanner.toml containing dated IgnoredVulns entries",
    )
    parser.add_argument(
        "--scope",
        type=Path,
        default=Path("govulncheck-scope.toml"),
        help="Path to the module/version scope required for each suppression",
    )
    parser.add_argument(
        "--govulncheck",
        default="govulncheck",
        help="govulncheck binary to invoke",
    )
    parser.add_argument(
        "packages",
        nargs="+",
        help="Go package patterns to scan (e.g. ./...)",
    )
    args = parser.parse_args(argv)

    try:
        ignores = load_ignores(args.config)
    except Exception as exc:
        print(f"govulncheck-gate: invalid config {args.config}: {exc}", file=sys.stderr)
        return 1
    try:
        scopes = load_scopes(args.scope, ignores)
    except Exception as exc:
        print(f"govulncheck-gate: invalid scope {args.scope}: {exc}", file=sys.stderr)
        return 1

    cmd = [args.govulncheck, "-format", "json", *args.packages]
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        if proc.stderr:
            sys.stderr.write(proc.stderr)
        if proc.stdout:
            sys.stderr.write(proc.stdout)
        return proc.returncode

    blocked, suppressed = evaluate_stream(proc.stdout, ignores, scopes)
    for osv_id in sorted(suppressed):
        info = ignores.get(osv_id)
        until = info["ignoreUntil"].isoformat() if info else "alias"
        print(f"govulncheck-gate: suppressed unfixed symbol finding {osv_id} (until {until})")

    if blocked:
        for finding in blocked:
            osv_id = finding.get("osv", "<unknown>")
            fixed = finding.get("fixed_version") or "N/A"
            trace = (finding.get("trace") or [{}])[0]
            mod = trace.get("module", "")
            ver = trace.get("version", "")
            print(
                f"govulncheck-gate: blocking symbol finding {osv_id} in {mod}@{ver} (fixed in: {fixed})",
                file=sys.stderr,
            )
        return 3

    print("govulncheck-gate: 0 unignored symbol vulnerabilities.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
