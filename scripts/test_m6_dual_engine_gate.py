"""Offline regressions for the strict backend gate's actual Go-event parser."""

import contextlib
import io
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

import m6_dual_engine_gate as gate

PACKAGE = "github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
DELETE_LEAVES = {
    "TestMonitorDeleteReplayDiagnostic/ReplayCommitVersusDelete",
    "TestMonitorDeleteReplayDiagnostic/CurrentSnapshotCommitVersusDelete",
}
MUTATION_LEAVES = {
    "TestMonitorUpdatePreservesConcurrentWorkerLease/sqlite",
    "TestMonitorUpdatePreservesConcurrentWorkerLease/mariadb",
    "TestMonitorMutationsWaitBeforeAppliedSourceGraph/update",
    "TestMonitorMutationsWaitBeforeAppliedSourceGraph/delete",
    "TestMonitorMutationsRejectMissingLocalRegistration/update",
    "TestMonitorMutationsRejectMissingLocalRegistration/delete",
    "TestMonitorDeleteConcurrencyDiagnostic/AppliedSourceReaderVersusDelete",
    "TestMonitorDeleteConcurrencyDiagnostic/DeleteVersusWorkerLeaseLifecycle",
}
M7_LEAVES = {
    f"TestRemoteAppliedConfigContract/mariadb/{case}"
    for case in (
        "ImmutableReceiptSelection", "ConcurrentReceiptRead", "MissingReceipt",
        "ReceiptHash", "ReceiptHub", "ReceiptCount", "ReceiptTime", "SnapshotHash",
        "SnapshotHub", "ActiveCount", "ForeignActiveTarget", "AuthenticationAndValidation",
    )
} | {
    f"TestLocalDeliveryConfigContract/mariadb/{case}"
    for case in (
        "ImmutableReceiptSelection", "ConcurrentReceiptRead", "MissingReceipt",
        "ReceiptHash", "ReceiptCount", "ReceiptTime", "SnapshotHash",
        "ForeignActiveTarget", "AuthenticationAndValidation",
    )
} | {
    f"TestAssignmentRevisionBindingContract/mariadb/{case}"
    for case in (
        "LocalBoundABA", "StalePreparedNoLegacyFallback", "LegacyLocalExecutionAndRefresh",
        "RemoteReceiptBinding", "InvalidSourceRevision",
    )
} | {
    f"TestAssignmentRevisionSessionContract/mariadb/{mode}"
    for mode in ("regional", "aggregate", "both")
} | {
    f"TestLocalRegionalApplicationContract/mariadb/{case}"
    for case in (
        "regional", "aggregate", "both", "ActivationRollback", "RetryAndUpgrade",
        "PartialUpgrade", "RecordingFence", "Migration", "OutboxBeforeSend", "EscalationEnqueue",
        "PresentMismatch/assignment_revision", "PresentMismatch/generation", "PresentMismatch/hub_id",
        "PresentMismatch/sha256", "PresentMismatch/monitor_id", "PresentMismatch/stream_id", "PresentMismatch/alert_delivery",
    )
} | {
    f"TestRemoteRegionalApplicationContract/mariadb/{case}"
    for case in (
        "ReceiptPrerequisite", "ImmutableReconnectAndABA", "TupleAndBatchRollback",
        "CorruptRetainedAndDuplicate", "ConcurrentDuplicate", "LeaseExpiresDuringDecode",
        "CancellationAndRedaction", "Migration", "ResetPending",
        "SessionFence/owner", "SessionFence/generation", "SessionFence/expiry",
        "SessionFence/disabled", "SessionFence/revoked", "SessionFence/stream",
        "SessionFence/retired", "SessionFence/key", "SessionFence/hub",
        "TupleAndBatchRollback/monitor", "TupleAndBatchRollback/binding",
        "TupleAndBatchRollback/generation", "TupleAndBatchRollback/revision",
        "TupleAndBatchRollback/hash", "TupleAndBatchRollback/stream",
        "TupleAndBatchRollback/target", "TupleAndBatchRollback/aggregate",
        "TupleAndBatchRollback/both", "TupleAndBatchRollback/unbound",
    )
} | {
    f"TestDeliveryPreparationContract/mariadb/{case}"
    for case in (
        "Positive", "Prerequisites", "CurrentABA", "ImmutableDuplicates",
        "CapabilityStarvation", "StaleSessionExpiryRollback", "ServiceDispatch",
        "StaleSessionExpiryRollback/owner", "StaleSessionExpiryRollback/generation",
        "StaleSessionExpiryRollback/hub",
    )
} | {f"TestDeliveryPreparationSessionContract/mariadb/{mode}"
     for mode in ("regional", "aggregate", "both")} | {
    f"TestLocalDeliveryPreparationContract/mariadb/{case}"
    for case in (
        "regional", "aggregate", "both", "AtomicRollback", "RetryAndColdUpgrade",
        "PartialUpgrade", "ModeAndPolicyABA", "PrerequisitesAndCancellation",
        "ConcurrentSelection", "Migration", "WorkerBeforeAssignment",
        "PresentMismatch/assignment_revision", "PresentMismatch/generation",
        "PresentMismatch/hub_id", "PresentMismatch/sha256", "PresentMismatch/monitor_id",
        "PresentMismatch/stream_id", "PresentMismatch/alert_delivery",
    )
}
M7_LEAVES |= {
    f"TestDeliveryPreparationRoundContract/mariadb/{case}"
    for case in (
        "RemoteReuseCold", "TerminalCooldownRearm", "ExpiryRearm", "MixedAllSources",
        "HistoricalFailure", "ABA", "AtomicRollback", "CorruptMembership",
        "ConcurrentCreators", "CanceledStaleAuthority", "Migration",
    )
} | {f"TestLocalDeliveryPreparationRoundRefresh/mariadb/{mode}" for mode in ("aggregate", "both")} | {
    f"TestDeliveryPreparationRoundSessionContract/mariadb/{mode}" for mode in ("aggregate", "both")
}
M7_LEAVES |= {
    f"TestAggregateStandbyContract/mariadb/{case}"
    for case in (
        "regional", "aggregate", "both", "ColdPlanningDeadline", "EvidenceFilters",
        "AdministrativeABA", "ChannelIdentity", "AtomicRollback", "LeaseFences",
        "ExpiryDuringEvaluation", "LockWaitExpiry", "ConcurrentWorkers", "Migration",
        "WorkerBeforeAssignment", "CurrentAfterReplay", "UnknownHolds",
        "CorruptStandbyRegional", "ConcurrentStateWriter", "ClockDomains",
          "RepeatSettings", "IndependentBatch",
    )
}
M7_LEAVES |= {"TestDeliveryConfigEvidenceAPI/mariadb", "TestLocalDeliveryConfigEvidenceAPI/mariadb"}
OPTIONAL_SKIPS = {
    "TestDatabaseChecker_Check_MongoDB_RealServer",
    "TestTelegramSender_Send_DownSeverity",
    "TestEdgeCheckCrashChild",
    "TestConfigGroupCycleHelperProcess",
    "TestEdgeDiskFullCriticalCommit",
}


def event(test=None, action="pass"):
    value = {"Action": action, "Package": PACKAGE}
    if test is not None:
        value["Test"] = test
    return json.dumps(value) + "\n"


def passing_events(omit=(), mariadb_count=300):
    """The old aggregate count passes independently of every required marker."""
    lines = [event(f"TestExistingStorage/mariadb/case_{i}")
             for i in range(mariadb_count)]
    lines.extend(event(name) for name in sorted(gate.REQUIRED - set(omit)))
    lines.append(event())
    return lines


class StrictBackendGateTest(unittest.TestCase):
    def run_gate(self, lines, exit_code=0, refuse_process_grant=False):
        """Exercise main with no real commands, Docker socket or database I/O."""
        process = mock.MagicMock()
        process.__enter__.return_value = process
        process.stdout = iter(lines)
        process.wait.return_value = exit_code
        output = io.StringIO()
        self.command_order = []
        self.docker_calls = []
        self.go_launches = []

        def docker_result(*args, **kwargs):
            self.command_order.append("docker")
            self.docker_calls.append((args, kwargs))
            if refuse_process_grant and args[-1] == "GRANT PROCESS ON *.* TO 'phoenix'@'%'":
                raise PermissionError("offline PROCESS grant refused")
            return "127.0.0.1:43306" if args[0] == "port" else ""

        def launch_process(*args, **kwargs):
            self.command_order.append("go")
            self.go_launches.append((args, kwargs))
            return process

        with (mock.patch.dict(gate.os.environ, {
                "DOCKER_HOST": "unix:///offline-gate-fixture.sock",
                "GOPATH": "/offline-gate-fixture/go",
            }, clear=True),
            mock.patch.object(sys, "argv", ["m6_dual_engine_gate.py"]),
            mock.patch.object(gate, "docker", side_effect=docker_result) as docker,
            mock.patch.object(gate.Path, "glob", return_value=[Path("/offline-gate-fixture/go1.26.6/bin/go")]),
            mock.patch.object(gate.subprocess, "run", return_value=mock.Mock(returncode=0)),
            mock.patch.object(gate.subprocess, "Popen", side_effect=launch_process) as launch,
            contextlib.redirect_stdout(output)):
            status = 0
            try:
                gate.main()
            except SystemExit as exc:
                status = exc.code

        # This is the production gate command, but Popen/run/docker above are
        # doubles: these tests create no containers and execute no Go tests.
        self.assertEqual(launch.call_args.args[0][1:],
                         ["test", "-race", "-count=1", "-json", "-timeout", "2400s", "-p", "4", "./..."])
        self.assertEqual(docker.call_args.args[:2], ("rm", "-f"))
        return status, json.loads(output.getvalue())

    def assert_failed(self, lines, **kwargs):
        status, summary = self.run_gate(lines, **kwargs)
        self.assertEqual(status, 1)
        self.assertEqual(summary["result"], "failed")
        return summary

    def test_both_exact_delete_leaves_and_prior_mutations_are_required(self):
        self.assertTrue(DELETE_LEAVES | MUTATION_LEAVES <= gate.REQUIRED)
        self.assertEqual(gate.SKIPS_ALLOWED, OPTIONAL_SKIPS)

    def test_each_m7_receipt_leaf_needs_an_exact_real_engine_pass(self):
        self.assertTrue(M7_LEAVES <= gate.REQUIRED)
        for leaf in sorted(M7_LEAVES):
            for action in (None, "skip", "fail"):
                with self.subTest(leaf=leaf, action=action):
                    lines = passing_events(omit={leaf})
                    # Parent and SQLite completion cannot replace this leaf.
                    lines.append(event(leaf.rsplit("/", 1)[0]))
                    lines.append(event(leaf.replace("/mariadb", "/sqlite")))
                    if action is not None:
                        lines.append(event(leaf, action))
                    summary = self.assert_failed(lines)
                    self.assertEqual(summary["missing_required"], [leaf])

    def test_process_observer_grant_uses_only_created_disposable_server(self):
        status, _summary = self.run_gate(passing_events())
        self.assertEqual(status, 0)
        created = next(call for call in self.docker_calls if call[0][0] == "run")
        name = created[0][created[0].index("--name") + 1]
        self.assertRegex(name, r"^phoenix-m6-matrix-[0-9a-f]{10}$")
        grants = [call for call in self.docker_calls
                  if call[0][-1] == "GRANT PROCESS ON *.* TO 'phoenix'@'%'"]
        self.assertEqual(len(grants), 1)
        arguments, kwargs = grants[0]
        self.assertEqual(
            arguments,
            ("exec", "-e", "MYSQL_PWD", name, "mariadb", "-uroot", "-e",
             "GRANT PROCESS ON *.* TO 'phoenix'@'%'")
        )
        self.assertEqual(kwargs["env"]["MYSQL_PWD"], created[1]["env"]["MARIADB_ROOT_PASSWORD"])
        self.assertNotIn("MYSQL_PWD", self.go_launches[0][1]["env"])
        self.assertNotIn("MARIADB_ROOT_PASSWORD", self.go_launches[0][1]["env"])
        # The one Go launch happens after fixture provisioning; cleanup follows.
        self.assertEqual(self.command_order.count("go"), 1)
        self.assertEqual(self.command_order[-2:], ["go", "docker"])
        self.assertEqual(self.docker_calls[-1][0], ("rm", "-f", "-v", name))
        self.assertEqual(self.go_launches[0][1]["env"]["GOENV"], "off")

    def test_refused_process_grant_prevents_go_and_cleans_created_container(self):
        with self.assertRaisesRegex(PermissionError, "offline PROCESS grant refused"):
            self.run_gate(passing_events(), refuse_process_grant=True)
        created = next(call for call in self.docker_calls if call[0][0] == "run")
        name = created[0][created[0].index("--name") + 1]
        self.assertEqual(self.go_launches, [])
        self.assertNotIn("go", self.command_order)
        self.assertEqual(self.docker_calls[-1][0], ("rm", "-f", "-v", name))

    def test_old_300_mariadb_passes_cannot_hide_missing_delete_leaf(self):
        for leaf in sorted(DELETE_LEAVES):
            with self.subTest(leaf=leaf):
                summary = self.assert_failed(passing_events(omit={leaf}))
                self.assertGreaterEqual(summary["mariadb_named_passes"], 300)
                self.assertEqual(summary["missing_required"], [leaf])

    def test_failed_or_skipped_delete_leaf_is_not_completion(self):
        for leaf in sorted(DELETE_LEAVES):
            for action in ("fail", "skip"):
                with self.subTest(leaf=leaf, action=action):
                    summary = self.assert_failed(passing_events(omit={leaf}) + [event(leaf, action)])
                    self.assertEqual(summary["missing_required"], [leaf])
                    self.assertIn(leaf, summary["failed_tests" if action == "fail" else "skipped_tests"])

    def test_failure_is_retained_even_if_same_leaf_also_has_pass(self):
        leaf = sorted(DELETE_LEAVES)[0]
        summary = self.assert_failed(passing_events() + [event(leaf, "fail")])
        self.assertEqual(summary["missing_required"], [])
        self.assertIn(leaf, summary["failed_tests"])

    def test_parent_and_invented_engine_suffix_cannot_replace_exact_leaf(self):
        lines = passing_events(omit=DELETE_LEAVES)
        lines.append(event("TestMonitorDeleteReplayDiagnostic"))
        lines.extend(event(leaf + "/mariadb") for leaf in sorted(DELETE_LEAVES))
        summary = self.assert_failed(lines)
        self.assertEqual(summary["missing_required"], sorted(DELETE_LEAVES))

    def test_each_prior_mutation_leaf_requires_its_own_pass(self):
        for leaf in sorted(MUTATION_LEAVES):
            with self.subTest(leaf=leaf):
                summary = self.assert_failed(passing_events(omit={leaf}))
                self.assertEqual(summary["missing_required"], [leaf])

    def test_exact_named_completion_and_only_audited_skips_pass(self):
        lines = passing_events() + [event(name, "skip") for name in sorted(OPTIONAL_SKIPS)]
        status, summary = self.run_gate(lines)
        self.assertEqual(status, 0)
        self.assertEqual(summary["result"], "passed")
        self.assertEqual(summary["missing_required"], [])
        self.assertEqual(summary["unrecognized_skips"], [])
        self.assertEqual(summary["mariadb_named_skips"], 0)
        self.assertEqual(summary["named_skips"], 5)

    def test_cycle_helper_skip_requires_every_actual_subprocess_case(self):
        cases = {f"TestConfigGroupCycle_DoesNotKillProcess/{variant}"
                 for variant in ("self", "two", "three")}
        self.assertTrue(cases <= gate.REQUIRED)
        for case in sorted(cases):
            for action in (None, "skip", "fail"):
                with self.subTest(case=case, action=action):
                    lines = passing_events(omit={case})
                    lines.append(event("TestConfigGroupCycleHelperProcess", "skip"))
                    if action is not None:
                        lines.append(event(case, action))
                    summary = self.assert_failed(lines)
                    self.assertEqual(summary["missing_required"], [case])

    def test_unsuffixed_mariadb_only_leaves_do_not_inflate_engine_count(self):
        status, summary = self.run_gate(passing_events())
        self.assertEqual(status, 0)
        # Only genuinely engine-suffixed required leaves inflate this count;
        # MariaDB-only diagnostic names remain exact, unsuffixed requirements.
        engine_leaves = sum("/mariadb" in name for name in gate.REQUIRED)
        self.assertEqual(summary["mariadb_named_passes"], 300 + engine_leaves)
        self.assert_failed(passing_events(mariadb_count=299 - engine_leaves))

    def test_mariadb_skip_is_rejected(self):
        summary = self.assert_failed(passing_events() + [event("TestExistingStorage/mariadb/skipped", "skip")])
        self.assertEqual(summary["mariadb_named_skips"], 1)

    def test_unrecognized_skip_malformed_stream_and_nonzero_go_exit_fail(self):
        self.assert_failed(passing_events() + [event("TestUnexpectedOptionalTarget", "skip")])
        summary = self.assert_failed(passing_events() + ["not-json\n"])
        self.assertEqual(summary["malformed_json_lines"], 1)
        summary = self.assert_failed(passing_events(), exit_code=1)
        self.assertEqual(summary["go_exit"], 1)


if __name__ == "__main__":
    unittest.main()
