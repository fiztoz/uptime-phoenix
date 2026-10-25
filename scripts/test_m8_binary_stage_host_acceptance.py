"""Fast synthesized controls for the M8 host launcher acceptance parser.

Every stream here is synthesized in Python.  Nothing in this module is an actual
Linux/Docker acceptance run: a green suite proves parser behaviour only, never
that the isolated namespace gate executed the real leaves.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import m8_binary_stage_host_acceptance as acceptance

PKG = acceptance.PACKAGE
TOP = acceptance.TOP_LEVEL
STAMP = "2024-01-02T03:04:05.123456789Z"
FOREIGN_PKG = "github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
FOREIGN_LEAF = TOP + "/NotAFrozenLeaf"
SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "m8_binary_stage_host_acceptance.py")

#: Every reason code this parser is allowed to emit.  A refusal must never carry
#: raw output, a traceback or a filesystem exception message instead of a code.
FIXED_REASONS = frozenset({
    "ok",
    "invalid_args",
    "invalid_repetitions",
    "invalid_process_exit",
    "unknown_scenario",
    "unreadable_input",
    "internal_error",
    "malformed_event",
    "invalid_event_field",
    "invalid_action",
    "foreign_package",
    "failure_event",
    "skip_event",
    "panic_output",
    "race_output",
    "timeout_output",
    "no_tests_output",
    "failure_output",
    "nonzero_process_exit",
    "empty_output",
    "parent_only_coverage",
    "missing_package_pass",
    "duplicate_package_pass",
    "missing_leaf",
    "leaf_pass_count_mismatch",
    "leaf_run_count_mismatch",
    "excess_leaf",
    "pass_before_run",
    "duplicate_terminal",
    "unmatched_pending_run",
})


def event(action, test=None, output=None, elapsed=None, package=PKG, stamp=STAMP):
    """Build one ``go test -json`` event the way the tool emits it."""
    record = {"Time": stamp, "Action": action, "Package": package}
    if test is not None:
        record["Test"] = test
    if output is not None:
        record["Output"] = output
    if elapsed is not None:
        record["Elapsed"] = elapsed
    return record


def stream(events):
    """Join events into a newline-terminated ``go test -json`` text stream."""
    return "".join(json.dumps(item) + "\n" for item in events)


def ancestors(name):
    """Return the proper ancestor paths of a slash-separated subtest name."""
    parts = name.split("/")
    return ["/".join(parts[:index]) for index in range(1, len(parts))]


def leaves(scenario="positive"):
    """Return the mandatory leaf names for a scenario, mirroring the contract."""
    if scenario == "positive":
        return [TOP + "/" + leaf for leaf in acceptance.POSITIVE_LEAVES]
    return [TOP + "/RefusesParent/" + scenario]


def valid_events(scenario="positive", repetitions=3, parents=True):
    """Synthesize an accepted stream: package start, N repeats, package pass."""
    names = leaves(scenario)
    middle = sorted({name for leaf in names for name in ancestors(leaf)} - {TOP})
    events = [event("start")]
    for _ in range(repetitions):
        events.append(event("run", TOP, elapsed=0.0))
        opened = []
        if parents:
            for name in middle:
                events.append(event("run", name, elapsed=0.0))
                opened.append(name)
        for name in names:
            events.append(event("run", name, elapsed=0.0))
            events.append(event("pass", name, elapsed=0.0))
        for name in reversed(opened):
            events.append(event("pass", name, elapsed=0.1))
        events.append(event("pass", TOP, elapsed=0.2))
    events.append(event("pass", elapsed=1.5))
    return events


def without(events, test):
    """Drop every event carrying one test name (run and terminal together)."""
    return [item for item in events if item.get("Test") != test]


def drop_last_pair(events, test):
    """Remove the final run/pass pair of one test, keeping the lifecycle clean."""
    positions = [index for index, item in enumerate(events) if item.get("Test") == test]
    dropped = set(positions[-2:])
    return [item for index, item in enumerate(events) if index not in dropped]


def inject(events, extra):
    """Insert extra events immediately after the package start event."""
    return events[:1] + list(extra) + events[1:]


class ParserCase(unittest.TestCase):
    """Shared assertions: fixed codes only, never leakage in the result."""

    def audit(self, events, scenario="positive", repetitions=3, process_exit=0):
        return acceptance.audit_events(stream(events), scenario, repetitions, process_exit)

    def assertAccepted(self, verdict):
        self.assertIs(verdict["passed"], True)
        self.assertEqual(verdict["reason"], "ok")
        self.assertIn(verdict["reason"], FIXED_REASONS)

    def assertRefused(self, verdict, reason):
        self.assertIs(verdict["passed"], False)
        self.assertEqual(verdict["reason"], reason)
        self.assertIn(verdict["reason"], FIXED_REASONS)

    def refuse(self, events, reason, scenario="positive", repetitions=3, process_exit=0):
        verdict = self.audit(events, scenario, repetitions, process_exit)
        self.assertRefused(verdict, reason)
        return verdict


class AuditAcceptsValidStreams(ParserCase):
    def test_positive_three_repetitions_passes(self):
        self.assertAccepted(self.audit(valid_events()))

    def test_positive_one_repetition_passes(self):
        self.assertAccepted(self.audit(valid_events(repetitions=1), repetitions=1))

    def test_positive_two_repetitions_passes(self):
        self.assertAccepted(self.audit(valid_events(repetitions=2), repetitions=2))

    def test_every_negative_scenario_passes_at_one_repetition(self):
        for scenario in acceptance.NEGATIVE_LEAVES:
            with self.subTest(scenario=scenario):
                self.assertAccepted(self.audit(valid_events(scenario, 1), scenario, 1))

    def test_every_negative_scenario_passes_at_three_repetitions(self):
        for scenario in acceptance.NEGATIVE_LEAVES:
            with self.subTest(scenario=scenario):
                self.assertAccepted(self.audit(valid_events(scenario, 3), scenario, 3))

    def test_intermediate_parent_events_are_optional(self):
        self.assertAccepted(self.audit(valid_events(parents=False)))
        for scenario in acceptance.NEGATIVE_LEAVES:
            with self.subTest(scenario=scenario):
                self.assertAccepted(self.audit(valid_events(scenario, 1, parents=False), scenario, 1))

    def test_trailing_blank_lines_and_crlf_are_tolerated(self):
        text = "\r\n" + stream(valid_events(repetitions=1)).replace("\n", "\r\n") + "\r\n\r\n"
        self.assertAccepted(acceptance.audit_events(text, "positive", 1, 0))

    def test_pause_and_cont_do_not_break_the_lifecycle(self):
        events = valid_events(repetitions=1)
        name = leaves("positive")[0]
        events.insert(2, event("pause", name))
        events.insert(3, event("cont", name))
        self.assertAccepted(self.audit(events, repetitions=1))

    def test_result_shape_is_compact_and_typed(self):
        verdict = self.audit(valid_events())
        self.assertEqual(set(verdict), {"passed", "reason", "counts"})
        self.assertEqual(set(verdict["counts"]), set(acceptance.COUNT_KEYS))
        for value in verdict["counts"].values():
            self.assertIsInstance(value, int)
            self.assertNotIsInstance(value, bool)
        self.assertEqual(verdict["counts"]["repetitions"], 3)
        self.assertEqual(verdict["counts"]["required_leaves"], 11)
        self.assertEqual(verdict["counts"]["required_leaf_passes"], 33)
        self.assertEqual(verdict["counts"]["package_passes"], 1)

    def test_negative_scenario_requires_one_leaf(self):
        verdict = self.audit(valid_events("nonroot", 2), "nonroot", 2)
        self.assertEqual(verdict["counts"]["required_leaves"], 2)
        self.assertEqual(verdict["counts"]["required_leaf_passes"], 4)


class AuditRefusesCoverage(ParserCase):
    def test_missing_top_level_is_refused(self):
        self.refuse(without(valid_events(), TOP), "missing_leaf")

    def test_every_positive_leaf_is_mandatory(self):
        for name in [TOP] + leaves("positive"):
            with self.subTest(leaf=name):
                self.refuse(without(valid_events(), name), "missing_leaf")

    def test_every_negative_leaf_is_mandatory(self):
        for scenario in acceptance.NEGATIVE_LEAVES:
            with self.subTest(scenario=scenario):
                self.refuse(without(valid_events(scenario), leaves(scenario)[0]), "missing_leaf", scenario)

    def test_leaf_repetition_shortfall_is_refused(self):
        events = drop_last_pair(valid_events(), leaves("positive")[0])
        self.refuse(events, "leaf_pass_count_mismatch")

    def test_top_level_repetition_shortfall_is_refused(self):
        events = drop_last_pair(valid_events(), TOP)
        self.refuse(events, "leaf_pass_count_mismatch")

    def test_extra_repetition_is_refused(self):
        events = valid_events(repetitions=2)
        name = leaves("positive")[0]
        first_pass = next(index for index, item in enumerate(events) if item.get("Action") == "pass")
        events.insert(first_pass, event("pass", name, elapsed=0.0))
        self.refuse(events, "duplicate_terminal", repetitions=2)

    def test_leaves_present_but_only_run_is_refused(self):
        name = leaves("positive")[0]
        events = [item for item in valid_events(repetitions=1)
                  if not (item.get("Test") == name and item["Action"] == "pass")]
        self.refuse(events, "unmatched_pending_run", repetitions=1)

    def test_excess_leaf_is_refused(self):
        self.refuse(inject(valid_events(), [event("run", FOREIGN_LEAF, elapsed=0.0),
                                            event("pass", FOREIGN_LEAF, elapsed=0.0)]), "excess_leaf")

    def test_excess_intermediate_parent_is_refused(self):
        extra = TOP + "/RefusesParent"
        self.refuse(inject(valid_events(), [event("run", extra, elapsed=0.0),
                                            event("pass", extra, elapsed=0.0)]), "excess_leaf")

    def test_positive_leaves_are_excess_in_negative_scenario(self):
        extra = TOP + "/ParentFDAttributes"
        self.refuse(inject(valid_events("unsafe_owner", 1), [event("run", extra, elapsed=0.0),
                                                             event("pass", extra, elapsed=0.0)]),
                    "excess_leaf", "unsafe_owner", 1)

    def test_negative_leaf_is_excess_in_positive_scenario(self):
        extra = TOP + "/RefusesParent/nonroot"
        self.refuse(inject(valid_events(), [event("run", extra, elapsed=0.0),
                                            event("pass", extra, elapsed=0.0)]), "excess_leaf")

    def test_package_only_stream_is_parent_only_coverage(self):
        self.refuse([event("start"), event("pass", elapsed=1.0)], "parent_only_coverage", repetitions=1)

    def test_top_level_only_stream_is_refused(self):
        events = [event("start"), event("run", TOP, elapsed=0.0),
                  event("pass", TOP, elapsed=0.1), event("pass", elapsed=1.0)]
        self.refuse(events, "missing_leaf", repetitions=1)

    def test_empty_and_blank_output_is_refused(self):
        for text in ("", "\n", "   \n\t\n"):
            with self.subTest(text=repr(text)):
                verdict = acceptance.audit_events(text, "positive", 1, 0)
                self.assertRefused(verdict, "empty_output")

    def test_missing_package_pass_is_refused(self):
        events = [item for item in valid_events()
                  if not (item.get("Test") is None and item["Action"] == "pass")]
        self.refuse(events, "missing_package_pass")

    def test_duplicate_package_pass_is_refused(self):
        self.refuse(valid_events() + [event("pass", elapsed=2.0)], "duplicate_package_pass")

    def test_zero_repetitions_is_refused(self):
        for repetitions in (0, -1, -7):
            with self.subTest(repetitions=repetitions):
                verdict = acceptance.audit_events(stream(valid_events()), "positive", repetitions, 0)
                self.assertRefused(verdict, "invalid_repetitions")

    def test_repetition_type_must_be_int(self):
        for repetitions in ("3", 3.0, True, False, None, [3]):
            with self.subTest(repetitions=repetitions):
                verdict = acceptance.audit_events(stream(valid_events()), "positive", repetitions, 0)
                self.assertRefused(verdict, "invalid_repetitions")


class AuditRefusesLifecycle(ParserCase):
    def test_pass_before_run_is_refused(self):
        self.refuse(inject(valid_events(), [event("pass", leaves("positive")[0], elapsed=0.0)]),
                    "pass_before_run")

    def test_duplicate_terminal_is_refused(self):
        name = leaves("positive")[0]
        events = valid_events(repetitions=1)
        index = next(i for i, item in enumerate(events)
                     if item.get("Test") == name and item["Action"] == "pass")
        events.insert(index + 1, event("pass", name, elapsed=0.0))
        self.refuse(events, "duplicate_terminal", repetitions=1)

    def test_unmatched_pending_run_is_refused(self):
        self.refuse(valid_events(repetitions=1) + [event("run", leaves("positive")[1], elapsed=0.0)],
                    "unmatched_pending_run", repetitions=1)

    def test_pending_top_level_run_is_refused(self):
        events = [item for item in valid_events(repetitions=1)
                  if not (item.get("Test") == TOP and item["Action"] == "pass")]
        self.refuse(events, "unmatched_pending_run", repetitions=1)


class AuditRefusesEvents(ParserCase):
    def test_malformed_json_line_is_refused(self):
        for tail in ("not json", "{", "[}", "{\"Action\":}"):
            with self.subTest(tail=tail):
                verdict = acceptance.audit_events(stream(valid_events()) + tail + "\n", "positive", 3, 0)
                self.assertRefused(verdict, "malformed_event")

    def test_non_object_json_line_is_refused(self):
        for tail in ("[]", "\"text\"", "42", "null", "true", "0.5"):
            with self.subTest(tail=tail):
                verdict = acceptance.audit_events(stream(valid_events()) + tail + "\n", "positive", 3, 0)
                self.assertRefused(verdict, "malformed_event")

    def test_foreign_package_is_refused(self):
        verdict = self.refuse(valid_events() + [event("output", output="elsewhere", package=FOREIGN_PKG)],
                              "foreign_package")
        self.assertEqual(verdict["reason"], "foreign_package")

    def test_missing_or_empty_package_is_refused(self):
        record = event("output", output="x")
        del record["Package"]
        self.refuse(valid_events() + [record], "invalid_event_field")
        self.refuse(valid_events() + [event("output", output="x", package="")], "invalid_event_field")
        self.refuse(valid_events() + [event("output", output="x", package=7)], "invalid_event_field")

    def test_unknown_field_is_refused(self):
        record = event("output", output="x")
        record["ImportPath"] = PKG
        self.refuse(valid_events() + [record], "invalid_event_field")

    def test_missing_or_bad_time_is_refused(self):
        record = event("output", output="x")
        del record["Time"]
        self.refuse(valid_events() + [record], "invalid_event_field")
        for stamp in ("", "2024-01-02 03:04:05", "2024-01-02T03:04:05", 1700000000, None):
            with self.subTest(stamp=stamp):
                self.refuse(valid_events() + [event("output", output="x", stamp=stamp)],
                            "invalid_event_field")

    def test_invalid_action_is_refused(self):
        for action in ("bench", "explode", "RUN", "Pass", "", None, 3):
            with self.subTest(action=action):
                record = event("output", output="x")
                record["Action"] = action
                verdict = self.audit(valid_events() + [record])
                self.assertRefused(verdict, "invalid_action")

    def test_action_must_match_test_presence(self):
        # A package-level action may not carry a test name, and vice versa.
        self.refuse(valid_events() + [event("start", test=TOP)], "invalid_event_field")
        self.refuse(valid_events() + [event("run")], "invalid_event_field")
        self.refuse(valid_events() + [event("cont")], "invalid_event_field")
        self.refuse(valid_events() + [event("pause")], "invalid_event_field")

    def test_empty_or_non_string_test_name_is_refused(self):
        self.refuse(valid_events() + [event("run", test="")], "invalid_event_field")
        record = event("output", output="x")
        record["Test"] = 12
        self.refuse(valid_events() + [record], "invalid_event_field")

    def test_output_must_be_a_string(self):
        record = event("output", output="x")
        record["Output"] = 5
        self.refuse(valid_events() + [record], "invalid_event_field")

    def test_elapsed_must_be_a_finite_nonnegative_number(self):
        for value in ("1.0", True, -1, -0.5, float("nan"), float("inf")):
            with self.subTest(elapsed=value):
                record = event("output", output="x")
                record["Elapsed"] = value
                self.refuse(valid_events() + [record], "invalid_event_field")

    def test_failure_action_is_refused(self):
        self.refuse(valid_events() + [event("fail", leaves("positive")[0], elapsed=0.0)], "failure_event")
        self.refuse(valid_events() + [event("fail", elapsed=1.0)], "failure_event")

    def test_skip_action_is_refused(self):
        self.refuse(valid_events() + [event("skip", leaves("positive")[0], output="skipped")], "skip_event")
        self.refuse(valid_events() + [event("skip", output="skipped")], "skip_event")

    def test_panic_output_is_refused(self):
        self.refuse(valid_events() + [event("output", output="panic: boom")], "panic_output")
        self.refuse(valid_events() + [event("output", output="fatal error: all goroutines are asleep")],
                    "panic_output")

    def test_race_output_is_refused(self):
        verdict = self.refuse(valid_events() + [event("output", output="WARNING: DATA RACE")],
                              "race_output")
        self.assertEqual(verdict["reason"], "race_output")

    def test_timeout_output_is_refused(self):
        self.refuse(valid_events() + [event("output", output="panic: test timed out after 3m0s")],
                    "panic_output")
        self.refuse(valid_events() + [event("output", output="test timed out after 180s")],
                    "timeout_output")

    def test_no_tests_output_is_refused(self):
        self.refuse(valid_events() + [event("output", output="testing: warning: no tests to run")],
                    "no_tests_output")
        self.refuse(valid_events() + [event("output", output="?   \t" + PKG + "\t[no test files]")],
                    "no_tests_output")

    def test_failure_marker_output_is_refused(self):
        self.refuse(valid_events() + [event("output", output="--- FAIL: TestProbeBinaryStageHostSandbox")],
                    "failure_output")

    def test_nonzero_process_exit_is_refused(self):
        for process_exit in (1, 2, 127, -1):
            with self.subTest(process_exit=process_exit):
                verdict = acceptance.audit_events(stream(valid_events()), "positive", 3, process_exit)
                self.assertRefused(verdict, "nonzero_process_exit")

    def test_process_exit_type_must_be_int(self):
        for process_exit in ("0", True, False, 0.0, None):
            with self.subTest(process_exit=process_exit):
                verdict = acceptance.audit_events(stream(valid_events()), "positive", 3, process_exit)
                self.assertRefused(verdict, "invalid_process_exit")

    def test_unknown_scenario_is_refused(self):
        for scenario in ("Positive", "POSITIVE", "refusesparent/nonroot", "nonroot/", "", None, 5,
                         "RefusesParent/unsafe_owner"):
            with self.subTest(scenario=scenario):
                verdict = acceptance.audit_events(stream(valid_events()), scenario, 3, 0)
                self.assertRefused(verdict, "unknown_scenario")

    def test_non_string_text_is_refused(self):
        for text in (b"{}", None, 5, ["{}"]):
            with self.subTest(text=text):
                verdict = acceptance.audit_events(text, "positive", 1, 0)
                self.assertRefused(verdict, "invalid_args")

    def test_audit_never_raises_on_hostile_payloads(self):
        payloads = (
            "{" + "\"a\":[" * 400 + "]" * 400 + "}",
            "{\"Time\":\"2024-01-02T03:04:05Z\",\"Action\":\"output\",\"Package\":\"" + PKG
            + "\",\"Elapsed\":1e999999}",
            "\x00\x01{\"Action\":\"run\"}",
        )
        for payload in payloads:
            with self.subTest(payload=payload[:24]):
                verdict = acceptance.audit_events(payload, "positive", 1, 0)
                self.assertIs(verdict["passed"], False)
                self.assertIn(verdict["reason"], FIXED_REASONS)


class AuditKeepsRefusalsQuiet(ParserCase):
    def test_refusal_never_echoes_output_text(self):
        canary = "ECHO-CANARY-LOGLINE"
        verdict = self.audit(valid_events() + [event("output", output="panic: " + canary)])
        self.assertRefused(verdict, "panic_output")
        self.assertNotIn(canary, json.dumps(verdict))

    def test_refusal_never_echoes_a_foreign_package_path(self):
        canary = "ECHO-CANARY-PACKAGE"
        verdict = self.audit(valid_events() + [event("output", output="x", package=canary)])
        self.assertRefused(verdict, "foreign_package")
        self.assertNotIn(canary, json.dumps(verdict))

    def test_refusal_never_echoes_a_foreign_test_name(self):
        canary = "ECHO-CANARY-LEAF"
        verdict = self.audit(inject(valid_events(), [event("run", TOP + "/" + canary, elapsed=0.0),
                                                     event("pass", TOP + "/" + canary, elapsed=0.0)]))
        self.assertRefused(verdict, "excess_leaf")
        self.assertNotIn(canary, json.dumps(verdict))

    def test_accepted_result_carries_only_counts(self):
        verdict = self.audit(valid_events())
        self.assertAccepted(verdict)
        serialized = json.dumps(verdict, sort_keys=True)
        self.assertNotIn(TOP, serialized)
        self.assertNotIn(PKG, serialized)


class CliCase(ParserCase):
    def run_cli(self, *argv):
        return subprocess.run([sys.executable, "-B", SCRIPT, *argv],
                              capture_output=True, text=True, check=False)

    def write(self, text, directory):
        path = os.path.join(directory, "events.json")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(text)
        return path

    def payload(self, completed):
        self.assertEqual(len(completed.stdout.strip().splitlines()), 1, completed.stdout)
        return json.loads(completed.stdout)

    def test_cli_accepts_a_valid_stream(self):
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(valid_events()), work)
            completed = self.run_cli("--go-json", path, "--scenario", "positive",
                                     "--repetitions", "3", "--process-exit", "0")
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertAccepted(self.payload(completed))

    def test_cli_accepts_a_negative_scenario(self):
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(valid_events("symlink_opt", 3)), work)
            completed = self.run_cli("--go-json", path, "--scenario", "symlink_opt",
                                     "--repetitions", "3", "--process-exit", "0")
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertAccepted(self.payload(completed))

    def test_cli_refusal_exits_nonzero_with_a_fixed_code(self):
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(without(valid_events(), leaves("positive")[0])), work)
            completed = self.run_cli("--go-json", path, "--scenario", "positive",
                                     "--repetitions", "3", "--process-exit", "0")
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(self.payload(completed)["reason"], "missing_leaf")

    def test_cli_reports_nonzero_process_exit(self):
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(valid_events()), work)
            completed = self.run_cli("--go-json", path, "--scenario", "positive",
                                     "--repetitions", "3", "--process-exit", "1")
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(self.payload(completed)["reason"], "nonzero_process_exit")

    def test_cli_reports_unreadable_input(self):
        with tempfile.TemporaryDirectory() as work:
            path = os.path.join(work, "absent.json")
            completed = self.run_cli("--go-json", path, "--scenario", "positive",
                                     "--repetitions", "1", "--process-exit", "0")
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(self.payload(completed)["reason"], "unreadable_input")

    def test_cli_rejects_invalid_arguments_nonzero(self):
        cases = (
            ("--go-json", "/dev/null", "--scenario", "positive", "--repetitions", "0", "--process-exit", "0"),
            ("--go-json", "/dev/null", "--scenario", "positive", "--repetitions", "x", "--process-exit", "0"),
            ("--go-json", "/dev/null", "--scenario", "positive", "--repetitions", "1", "--process-exit", "y"),
            ("--go-json", "/dev/null", "--scenario", "positive", "--repetitions", "1"),
            ("--go-json", "/dev/null", "--repetitions", "1", "--process-exit", "0"),
            ("--scenario", "positive", "--repetitions", "1", "--process-exit", "0"),
            (),
        )
        for argv in cases:
            with self.subTest(argv=argv):
                completed = self.run_cli(*argv)
                self.assertNotEqual(completed.returncode, 0)

    def test_cli_rejects_unknown_scenario(self):
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(valid_events()), work)
            completed = self.run_cli("--go-json", path, "--scenario", "bogus",
                                     "--repetitions", "1", "--process-exit", "0")
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(self.payload(completed)["reason"], "unknown_scenario")

    def test_cli_never_prints_raw_log_content(self):
        canary = "ECHO-CANARY-LOGLINE"
        with tempfile.TemporaryDirectory() as work:
            path = self.write(stream(valid_events() + [event("output", output="panic: " + canary)]), work)
            completed = self.run_cli("--go-json", path, "--scenario", "positive",
                                     "--repetitions", "3", "--process-exit", "0")
        self.assertEqual(completed.returncode, 1)
        self.assertNotIn(canary, completed.stdout)
        self.assertNotIn(canary, completed.stderr)
        self.assertEqual(self.payload(completed)["reason"], "panic_output")


if __name__ == "__main__":
    unittest.main()