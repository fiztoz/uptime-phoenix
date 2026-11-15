#!/usr/bin/env python3
"""M6 matrix runner exit-status contracts; no Go compiler or database needed."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "m6_matrix_run", Path(__file__).with_name("m6_matrix_run.py"))
matrix = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(matrix)


class MatrixExitStatusTest(unittest.TestCase):
    def run_case(self, events, code=0):
        rows = [dict(T="T01", verdict="COVERED", pkg="./fixture",
                     tests=["TestFixture"], note="")]
        def execute(command, **kwargs):
            self.assertIn("-race", command)
            self.assertIn("^(TestFixture)$", command)
            for event in events:
                kwargs["stdout"].write(json.dumps(event) + "\n")
            return SimpleNamespace(returncode=code)
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(matrix, "load_rows", return_value=rows), \
                 patch.object(matrix.subprocess, "run", side_effect=execute), \
                 patch.object(matrix.sys, "argv", ["m6_matrix_run.py", directory]), \
                 contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaises(SystemExit) as result:
                    matrix.main()
            summary = json.loads((Path(directory) / "summary.json").read_text())
        return result.exception.code, summary

    def test_success(self):
        status, summary = self.run_case([dict(Test="TestFixture", Action="pass")])
        self.assertEqual(status, 0)
        self.assertEqual(summary[0]["top"], "pass")

    def test_nonzero_exit_after_named_pass_fails(self):
        status, summary = self.run_case([dict(Test="TestFixture", Action="pass")], 1)
        self.assertEqual(status, 1)
        self.assertEqual(summary[0]["top"], "pass")

    def test_signaled_process_after_named_pass_fails(self):
        status, _ = self.run_case([dict(Test="TestFixture", Action="pass")], -9)
        self.assertEqual(status, 1)

    def test_package_failure_event_without_test_fails(self):
        status, _ = self.run_case([dict(Test="TestFixture", Action="pass"),
                                   dict(Package="fixture", Action="fail")])
        self.assertEqual(status, 1)

    def test_fixture_skip_remains_permitted(self):
        status, summary = self.run_case([dict(Test="TestFixture", Action="skip"),
                                        dict(Package="fixture", Action="skip")])
        self.assertEqual(status, 0)
        self.assertEqual(summary[0]["top"], "skip")

    def test_missing_named_test_fails(self):
        status, _ = self.run_case([dict(Package="fixture", Action="skip")])
        self.assertEqual(status, 1)

    def test_failed_subtest_fails_even_with_parent_pass(self):
        status, _ = self.run_case([dict(Test="TestFixture/child", Action="fail"),
                                   dict(Test="TestFixture", Action="pass")])
        self.assertEqual(status, 1)


if __name__ == "__main__":
    unittest.main()
