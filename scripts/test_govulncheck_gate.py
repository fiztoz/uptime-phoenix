"""Unit tests for the fail-closed govulncheck gate (issue #225)."""

from datetime import date
import copy
import json
from pathlib import Path
import tempfile
import unittest

import govulncheck_gate as gate


def make_stream(*entries):
    return "\n".join(json.dumps(entry, indent=2) for entry in entries) + "\n"


ECHO_OSV_6872 = {
    "osv": {
        "id": "GO-2026-6872",
        "aliases": ["CVE-2026-118653", "GHSA-246p-cpwv-v3jq"],
        "summary": "Rate Limiter Bypass via Spoofed X-Forwarded-For Header in Default Configuration in github.com/labstack/echo",
    }
}

ECHO_FINDING_6872 = {
    "finding": {
        "osv": "GO-2026-6872",
        "trace": [
            {
                "module": "github.com/labstack/echo/v4",
                "version": "v4.16.3",
                "package": "github.com/labstack/echo/v4",
                "function": "New",
            }
        ],
    }
}

FIXABLE_FINDING = {
    "finding": {
        "osv": "GO-2026-6865",
        "fixed_version": "v1.20.2",
        "trace": [
            {
                "module": "github.com/klauspost/compress",
                "version": "v1.20.1",
                "package": "github.com/klauspost/compress/fse",
                "function": "Decompress",
            }
        ],
    }
}

ECHO_SCOPE = {
    "GO-2026-6872": {
        "id": "GO-2026-6872",
        "module": "github.com/labstack/echo/v4",
        "versions": {"v4.16.3"},
    }
}

ECHO_6872_CONFIG = """
[[IgnoredVulns]]
id = "GO-2026-6872"
ignoreUntil = 2026-12-05
reason = "Echo 4.16.3 fixes GHSA-246p-cpwv-v3jq upstream; vuln.go.dev omits fixed version on v4."
"""


def echo_finding(module: str, version: str) -> dict:
    finding = copy.deepcopy(ECHO_FINDING_6872)
    finding["finding"]["trace"][0]["module"] = module
    finding["finding"]["trace"][0]["version"] = version
    return finding


MODULE_ONLY_FINDING = {
    "finding": {
        "osv": "GO-2026-5932",
        "fixed_version": "v0.58.1",
        "trace": [
            {
                "module": "golang.org/x/crypto",
                "version": "v0.58.0",
            }
        ],
    }
}


class GovulncheckGateTests(unittest.TestCase):
    def write_config(self, body: str) -> Path:
        handle = tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False)
        handle.write(body)
        handle.close()
        path = Path(handle.name)
        self.addCleanup(path.unlink, missing_ok=True)
        return path

    def test_allows_unfixed_symbol_finding_covered_by_active_exception(self):
        cfg = self.write_config(ECHO_6872_CONFIG)
        ignores = gate.load_ignores(cfg, today=date(2026, 11, 7))
        stream = make_stream(ECHO_OSV_6872, ECHO_FINDING_6872, MODULE_ONLY_FINDING)
        blocked, suppressed = gate.evaluate_stream(stream, ignores, ECHO_SCOPE)
        self.assertEqual(blocked, [])
        self.assertEqual(sorted(suppressed), ["GO-2026-6872"])

    def test_rejects_listed_finding_at_older_affected_version(self):
        ignores = gate.load_ignores(self.write_config(ECHO_6872_CONFIG), today=date(2026, 11, 7))
        for version in ("v4.16.2", "v4.15.0"):
            with self.subTest(version=version):
                stream = make_stream(ECHO_OSV_6872, echo_finding("github.com/labstack/echo/v4", version))
                blocked, suppressed = gate.evaluate_stream(stream, ignores, ECHO_SCOPE)
                self.assertEqual(suppressed, set())
                self.assertEqual([f["osv"] for f in blocked], ["GO-2026-6872"])

    def test_rejects_listed_finding_in_mismatched_module(self):
        ignores = gate.load_ignores(self.write_config(ECHO_6872_CONFIG), today=date(2026, 11, 7))
        for module in ("github.com/labstack/echo", "github.com/labstack/echo/v5", "example.com/fork/echo/v4"):
            with self.subTest(module=module):
                stream = make_stream(ECHO_OSV_6872, echo_finding(module, "v4.16.3"))
                blocked, suppressed = gate.evaluate_stream(stream, ignores, ECHO_SCOPE)
                self.assertEqual(suppressed, set())
                self.assertEqual([f["osv"] for f in blocked], ["GO-2026-6872"])

    def test_rejects_listed_finding_without_scope_entry(self):
        ignores = gate.load_ignores(self.write_config(ECHO_6872_CONFIG), today=date(2026, 11, 7))
        stream = make_stream(ECHO_OSV_6872, ECHO_FINDING_6872)
        blocked, suppressed = gate.evaluate_stream(stream, ignores, {})
        self.assertEqual(suppressed, set())
        self.assertEqual([f["osv"] for f in blocked], ["GO-2026-6872"])

    def test_rejects_scope_without_ignore_entry(self):
        scope = self.write_config(
            """
[[Scope]]
id = "GO-2026-6875"
module = "github.com/labstack/echo/v4"
versions = ["v4.16.3"]
"""
        )
        ignores = gate.load_ignores(self.write_config(ECHO_6872_CONFIG), today=date(2026, 11, 7))
        with self.assertRaisesRegex(ValueError, "no IgnoredVulns entry"):
            gate.load_scopes(scope, ignores)

    def test_rejects_scope_without_versions(self):
        scope = self.write_config(
            """
[[Scope]]
id = "GO-2026-6872"
module = "github.com/labstack/echo/v4"
versions = []
"""
        )
        ignores = gate.load_ignores(self.write_config(ECHO_6872_CONFIG), today=date(2026, 11, 7))
        with self.assertRaisesRegex(ValueError, "invalid Scope entry"):
            gate.load_scopes(scope, ignores)

    def test_rejects_expired_exception_in_config(self):
        cfg = self.write_config(
            """
[[IgnoredVulns]]
id = "GO-2026-6872"
ignoreUntil = 2026-11-01
reason = "Expired exception must fail closed."
"""
        )
        with self.assertRaisesRegex(ValueError, "expired"):
            gate.load_ignores(cfg, today=date(2026, 11, 7))

    def test_rejects_symbol_finding_with_available_fixed_version_even_if_listed(self):
        cfg = self.write_config(
            """
[[IgnoredVulns]]
id = "GO-2026-6865"
ignoreUntil = 2026-12-05
reason = "Must not suppress a symbol finding that has a fixed_version."
"""
        )
        ignores = gate.load_ignores(cfg, today=date(2026, 11, 7))
        stream = make_stream(FIXABLE_FINDING)
        blocked, suppressed = gate.evaluate_stream(stream, ignores, ECHO_SCOPE)
        self.assertEqual(suppressed, set())
        self.assertEqual(len(blocked), 1)
        self.assertEqual(blocked[0]["osv"], "GO-2026-6865")

    def test_rejects_unlisted_symbol_finding(self):
        cfg = self.write_config("")
        ignores = gate.load_ignores(cfg, today=date(2026, 11, 7))
        stream = make_stream(ECHO_OSV_6872, ECHO_FINDING_6872)
        blocked, suppressed = gate.evaluate_stream(stream, ignores, ECHO_SCOPE)
        self.assertEqual(suppressed, set())
        self.assertEqual(len(blocked), 1)
        self.assertEqual(blocked[0]["osv"], "GO-2026-6872")

    def test_repository_osv_scanner_toml_is_valid_today(self):
        repo_cfg = Path(__file__).resolve().parent.parent / "osv-scanner.toml"
        ignores = gate.load_ignores(repo_cfg, today=date(2026, 11, 7))
        self.assertIn("GO-2026-6872", ignores)
        self.assertIn("GO-2026-6875", ignores)

    def test_repository_scope_pins_echo_exceptions_to_go_mod_version(self):
        root = Path(__file__).resolve().parent.parent
        ignores = gate.load_ignores(root / "osv-scanner.toml", today=date(2026, 11, 7))
        scopes = gate.load_scopes(root / "govulncheck-scope.toml", ignores)
        go_mod = (root / "go.mod").read_text(encoding="utf-8")
        for vuln_id in ("GO-2026-6872", "GO-2026-6875"):
            scope = scopes[vuln_id]
            self.assertEqual(scope["module"], "github.com/labstack/echo/v4")
            self.assertEqual(scope["versions"], {"v4.16.3"})
            self.assertIn("github.com/labstack/echo/v4 v4.16.3", go_mod)


if __name__ == "__main__":
    unittest.main()
