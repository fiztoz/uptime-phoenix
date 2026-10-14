"""Offline regressions for exhaustive repository CI partitioning/fan-in."""

import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import tempfile
import unittest
from unittest import mock

import repository_test_shards as shards

SHA = "a" * 40
INVENTORY = ["TestAlpha", "TestBeta", "TestDelta", "TestGamma"]
REQUIRED = {"TestAlpha/mariadb/required"}


def passing_report(index, count=4, inventory=INVENTORY):
    lines = []
    for parent in shards.partition(inventory, count)[index]:
        for test in [parent, f"{parent}/mariadb/required"]:
            lines.extend(json.dumps({"Package": shards.PACKAGE, "Action": action, "Test": test})
                         for action in ("run", "pass"))
    lines.append(json.dumps({"Package": shards.PACKAGE, "Action": "pass"}))
    report = shards.summarize(lines, inventory, index, count, SHA)
    report["go_exit"] = 0
    return report


class RepositoryShardTest(unittest.TestCase):
    def setUp(self):
        self.required = mock.patch.object(shards.gate, "REQUIRED", REQUIRED)
        self.required.start()
        self.addCleanup(self.required.stop)

    def test_partition_covers_every_parent_once_and_automatically_includes_new_tests(self):
        names = INVENTORY + ["TestFuture", "ExampleFuture", "FuzzFuture"]
        plan = shards.partition(list(reversed(names)), 4)
        flattened = [name for shard in plan for name in shard]
        self.assertEqual(sorted(names), sorted(flattened))
        self.assertEqual(len(names), len(set(flattened)))
        self.assertEqual(plan, shards.partition(names, 4))
        self.assertLessEqual(max(map(len, plan)) - min(map(len, plan)), 1)

    def test_partition_refuses_invalid_or_empty_selection(self):
        for names, count in [([], 1), (INVENTORY, 0), (INVENTORY, 5),
                             (INVENTORY, True), (["TestAlpha"] * 2, 1),
                             (["TestAlpha/subtest"], 1), (["BenchmarkOnly"], 1)]:
            with self.subTest(names=names, count=count), self.assertRaises(ValueError):
                shards.partition(names, count)

    def test_complete_reports_pass(self):
        result = shards.verify_reports([passing_report(n) for n in range(4)], INVENTORY, 4, SHA)
        self.assertEqual("passed", result["result"])
        self.assertEqual(4, result["parents"])
        self.assertEqual(4, result["mariadb_named_passes"])
        self.assertEqual(1, result["required_leaves"])
        self.assertEqual([], result["missing_required"])

    def test_missing_extra_duplicate_or_stale_shard_refuses(self):
        reports = [passing_report(n) for n in range(4)]
        stale = copy.deepcopy(reports)
        stale[1]["source_sha"] = "b" * 40
        cases = [reports[:-1], reports + [reports[0]], [reports[0]] * 4, stale]
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                shards.verify_reports(case, INVENTORY, 4, SHA)

    def test_common_truncated_inventory_cannot_hide_a_new_test(self):
        reports = [passing_report(n) for n in range(4)]
        with self.assertRaises(ValueError):
            shards.verify_reports(reports, INVENTORY + ["TestFuture"], 4, SHA)

    def test_bad_process_or_provenance_refuses(self):
        cases = {"go_exit": 1, "malformed_json": 1, "foreign_package": True,
                 "package_pass": False, "package_fail": True,
                 "source_sha": "b" * 40, "version": 0,
                 "count": 5, "index": True, "package": "foreign"}
        for key, value in cases.items():
            report = passing_report(0)
            report[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                shards.validate_report(report, INVENTORY, 4, SHA)

    def test_parent_pass_is_not_missing_leaf_coverage(self):
        report = passing_report(0)
        report["passed"].remove("TestAlpha/mariadb/required")
        report["runs"].append("TestAlpha/mariadb/unrelated")
        report["passed"].append("TestAlpha/mariadb/unrelated")
        with self.assertRaisesRegex(ValueError, "missing required real-engine leaves"):
            shards.validate_report(report, INVENTORY, 4, SHA)

    def test_parent_cannot_pass_without_a_real_run(self):
        for key in ("runs", "passed"):
            report = passing_report(0)
            report[key].remove("TestAlpha")
            with self.subTest(key=key), self.assertRaises(ValueError):
                shards.validate_report(report, INVENTORY, 4, SHA)

    def test_unexpected_duplicate_failed_or_skipped_tests_refuse(self):
        for key, value in [("runs", "TestForeign"), ("passed", "TestAlpha"),
                           ("failed", "TestAlpha/mariadb/required"),
                           ("skipped", "TestAlpha/mariadb/optional"),
                           ("skipped", "TestAlpha/sqlite/optional")]:
            report = passing_report(0)
            report[key].append(value)
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                shards.validate_report(report, INVENTORY, 4, SHA)

    def test_every_shard_needs_actual_mariadb_execution(self):
        report = passing_report(1)
        report["passed"] = ["TestBeta"]
        with self.assertRaisesRegex(ValueError, "MariaDB"):
            shards.validate_report(report, INVENTORY, 4, SHA)

    def test_summarizer_rejects_malformed_and_foreign_package_events(self):
        for line, key in [("not json", "malformed_json"),
                          (json.dumps({"Package": "foreign", "Test": "TestAlpha", "Action": "pass"}),
                           "foreign_package")]:
            report = shards.summarize([line], INVENTORY, 0, 4, SHA)
            self.assertTrue(report[key])

    def test_discovery_uses_compiled_inventory_and_includes_examples_and_fuzz(self):
        output = [json.dumps({"Package": shards.PACKAGE, "Action": "output", "Output": n + "\n"})
                  for n in ("TestNew", "ExampleNew", "FuzzNew", "BenchmarkNotRun")]
        output.append(json.dumps({"Package": shards.PACKAGE, "Action": "pass"}))
        proc = mock.Mock(returncode=0, stdout="\n".join(output))
        with mock.patch.object(shards.subprocess, "run", return_value=proc) as run:
            self.assertEqual(["ExampleNew", "FuzzNew", "TestNew"], shards.discover())
            self.assertEqual(["go", "test", "-json", "-list", ".", shards.TARGET], run.call_args.args[0])

    def test_discovery_failure_empty_missing_completion_and_wrong_package_refuse(self):
        for proc in [mock.Mock(returncode=1, stdout=""), mock.Mock(returncode=0, stdout=""),
                     mock.Mock(returncode=0, stdout=json.dumps({"Package": shards.PACKAGE, "Action": "output", "Output": "TestNew\n"})),
                     mock.Mock(returncode=0, stdout=json.dumps({"Package": "foreign", "Action": "pass"}))]:
            with mock.patch.object(shards.subprocess, "run", return_value=proc), self.assertRaises(ValueError):
                shards.discover()

    def test_missing_database_refuses_before_discovery_or_test_execution(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(shards, "discover") as discover:
            with io.StringIO() as stderr, mock.patch("sys.stderr", stderr):
                self.assertEqual(1, shards.main(["run", "--source-sha", SHA, "--report", "unused.json"]))
            discover.assert_not_called()

    def test_actual_runner_preserves_race_timeout_all_subtests_and_provenance(self):
        report = passing_report(0)
        events = []
        for test in report["runs"]:
            events.extend(json.dumps({"Package": shards.PACKAGE, "Action": action, "Test": test}) + "\n"
                          for action in ("run", "pass"))
        events.append(json.dumps({"Package": shards.PACKAGE, "Action": "pass"}) + "\n")
        proc = mock.MagicMock()
        proc.__enter__.return_value = proc
        proc.stdout = iter(events)
        proc.wait.return_value = 0
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.dict(os.environ, {"TEST_MARIADB_DSN": "fixture-only"}), \
                mock.patch.object(shards, "discover", return_value=INVENTORY), \
                mock.patch.object(shards.subprocess, "Popen", return_value=proc) as launch, \
                mock.patch("sys.stdout", io.StringIO()):
            path = Path(directory) / "coverage.json"
            self.assertEqual(0, shards.main(["run", "--index", "0", "--count", "4",
                                            "--source-sha", SHA, "--report", str(path)]))
            cmd = launch.call_args.args[0]
            self.assertIn("-race", cmd)
            self.assertIn("-count=1", cmd)
            self.assertEqual("40m", cmd[cmd.index("-timeout") + 1])
            pattern = cmd[cmd.index("-run") + 1]
            self.assertTrue(re.fullmatch(pattern, "TestAlpha"))
            self.assertFalse(re.fullmatch(pattern, "TestAlphaExtra"))
            self.assertNotIn("/", pattern)  # no subtest filter
            self.assertNotIn("fixture-only", path.read_text())
            self.assertEqual(SHA, json.loads(path.read_text())["source_sha"])

    def test_workflow_fan_in_and_other_packages_cannot_silently_drop_coverage(self):
        ci = (shards.ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("shard: [0, 1, 2, 3]", ci)
        self.assertIn("fail-fast: false", ci)
        self.assertIn("needs: repository-race\n    if: always()", ci)
        self.assertIn('run: test "$MATRIX_RESULT" = success', ci)
        self.assertIn("TEST_MARIADB_DSN:", ci)
        self.assertIn("if-no-files-found: error", ci)
        self.assertIn("--count 4 --source-sha", ci)
        self.assertIn("scripts/repository_test_shards.py verify", ci)
        self.assertIn("if: matrix.shard == 0", ci)
        self.assertIn("go list ./internal/adapters/repository/...", ci)
        self.assertIn("go list ./...", ci)
        self.assertIn("grep -vx '" + shards.PACKAGE + "'", ci)
        self.assertIn('go test -race -count=1 -timeout 40m "${packages[@]}"', ci)

    def test_release_gate_requires_real_expanded_matrix_names(self):
        path = shards.ROOT / "scripts/release/test-ci-gate.py"
        spec = importlib.util.spec_from_file_location("release_ci_test", path)
        release = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(release)
        actual = release.ci_job_names(shards.ROOT / ".github/workflows/ci.yml")
        self.assertEqual(sorted(release.REQUIRED_JOBS), sorted(actual))
        for index in range(4):
            self.assertIn(f"repository-race-{index}", actual)


if __name__ == "__main__":
    unittest.main()
