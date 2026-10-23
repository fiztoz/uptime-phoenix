"""Offline regressions for strict backend evidence and active Go toolchain pins."""

import argparse
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
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
M7_LEAVES |= {f"TestOperativeCutoverContract/mariadb/{case}" for case in ("ColdMembership", "DrainAmbiguousBlocks", "DrainPendingAndStarted", "FencesAndStageOrder", "GateSuppression", "LocalProviderJournal", "MigrationConstraints", "MissingPreparation", "RemoteMembership", "aggregate", "both",)}
M7_LEAVES |= {f"TestOperativeRefinementContract/mariadb/{case}" for case in ("AdoptionFutureReceipt", "AdoptionObservationBoundary", "AdoptionReceiptBoundary", "BusyCodeAlignsAndRearms", "CorruptionBlocks", "DigestMismatchRefused", "DigestOnlyPlans", "DisableDrainsApplying", "DisableDrainsQuiesced", "DisableDrainsQuiescing", "RemoteBusyRearmsFromReceipt", "RepeatThrottle", "ResumeWithdrawsQueuedPhase", "StandbyNeverPreventsOperativeOpen",)}
M7_LEAVES |= {f"TestOperativeProviderLifecycle/mariadb/{case}" for case in ("ProviderErrorBlocksDrain", "aggregate", "both",)}
M7_LEAVES |= {f"TestOperativeCutoverSessionContract/mariadb/{case}" for case in ("aggregate", "both",)}
M7_LEAVES |= {f"TestOperativeUpgradeContract/mariadb/{case}" for case in ("LegacyAmbiguity", "HeldRuntime",)}
M7_LEAVES |= {f"TestOperativeProviderFences/mariadb/{case}" for case in ("MissingRegionalFact", "MissingPreparation", "AvailableAt", "FinishClockRollback")}
M7_LEAVES |= {f"TestOperativeActivationCycle/mariadb/{case}" for case in ("aggregate", "both",)}
M7_LEAVES |= {"TestDeliveryConfigEvidenceAPI/mariadb", "TestLocalDeliveryConfigEvidenceAPI/mariadb"}
M8_LEAVES = {
    f"TestM8PreflightOperationContract/mariadb/{case}"
    for case in (
        "CreateCommitAndExactRetryIdentity", "BeginOwnerExpiryFences",
        "FinishStateExpiryAndIdempotentRetry", "CancellationIntentVersusJoinedOutcome",
        "ExpiredCancellationCannotFinishLate", "ObserveColdReadNeverCreates",
        "SeparateConnectionConcurrentCreates", "CancelFinishRaceStaysConsistent",
        "RawCorruptionAndSchemaRefusal", "ConflictPathsRollbackWithoutPartialWrite",
        "ClockRollbackNeverRewindsUpdatedAt", "DowngradeRefusesNonterminalKeepsTerminalHistory",
    )
} | {"TestM8PreflightAPI/mariadb", "TestM8PreflightClockAfterAuthorityLock/mariadb"}
INSTALL_JOURNAL_LEAVES = {
    f"TestM8InstallJournalStore/mariadb/{case}"
    for case in (
        "AtomicCreateAndImmutableRetry", "OwnerStateExpiryFences",
        "CommittedPrerequisiteAndOneAttempt", "ResourceOutcomeIdempotency",
        "UnknownOutcomeStopsDependents", "CancellationIntentVersusJoinedFinish",
        "ColdExpiryMakesInFlightUnknown", "CorruptGraphRefusalBeforeReconcile",
        "ConflictRollsBack", "GlobalAttemptIdentity", "DowngradeGuard",
        "UTCAndClockRollback", "SeparateConnectionConcurrentAdmission",
    )
} | {
    f"TestM8InstallJournalSchema/mariadb/{case}"
    for case in (
        "OperationNullCode_failed", "OperationNullCode_canceled",
        "OperationNullCode_interrupted", "ResourceNullCode_not_started",
        "ResourceNullCode_unknown", "SubmicrosecondCorruptionRefused",
        "SubmicrosecondResourceCorruptionRefused",
    )
} | {
    f"TestM8InstallJournalLifecycle/mariadb/{case}"
    for case in (
        "ResourcesReadyIsNotInstallation/systemd", "ResourcesReadyIsNotInstallation/docker",
        "CancellationRequiresIntent", "LostAcknowledgementNeverReplays",
        "ColdReplicaAndLateReport", "LastResourceCancelRace",
        "ConcurrentReadersAndReservations", "CorruptionBeforeExpiry",
    )
} | {
    f"TestM8InstallJournalClockAfterAuthorityLock/mariadb/{case}"
    for case in ("Begin", "Reserve", "Record", "Finish", "Observe", "Cancel")
}
INSTALL_RUNNER_LEAVES = {
    f"TestM8InstallRunner/mariadb/{case}"
    for case in (
        "ResourcesReady/systemd", "ResourcesReady/docker", "ConcurrentRunSingleBegin",
        "LostReservationAckNoInvocation", "RemoteCancelJoinsBeforeTerminal",
        "OwnerLossUnknownStopsDependents", "FinalAppliedOwnerLoss", "ColdExpiryRejectsLateApplied",
        "MalformedOutcomeCannotAdvance", "IdentityOwnerNoDispatch", "ObservationLossStopsDispatch",
    )
}
OPTIONAL_SKIPS = {
    "TestDatabaseChecker_Check_MongoDB_RealServer",
    "TestTelegramSender_Send_DownSeverity",
    "TestEdgeCheckCrashChild",
    "TestConfigGroupCycleHelperProcess",
    "TestEdgeDiskFullCriticalCommit",
}


STAGE_AUTHORIZATION_LEAVES = {
    f"TestM8BinaryStageAuthorization/mariadb/{case}"
    for case in (
        "GrantBindsCommittedReservation", "RejectPlannedResource",
        "RejectUncommittedPredecessor", "IdentityOwnerAttemptMismatch",
        "CancelBeforeAdmission", "AdmissionBeforeCancel", "ExpiryAfterLock",
        "CorruptGraphNoGrant", "ConcurrentAdmissionSingleGrant", "ExactReplayNoGrant",
        "DifferentChallengeNoRearm", "LostAdmissionAckNoSign", "SignFailureConsumesAdmission",
        "CallerCanceledAfterCommitNoGrant", "IndependentReaderSeesAdmissionBeforeSign",
        "ColdReopenNoGrant", "ClockRollbackStamp", "DowngradeRetainsAdmission",
    )
}
# Independently restated from the actual SQLite run of the host-action tests.
HOST_ACTION_LEAVES = {
    "TestM8HostActionAdmissionSchema/mariadb/ArtifactDirectoryRefused",
    "TestM8HostActionAdmissionSchema/mariadb/DowngradeGuard",
    "TestM8HostActionAdmissionSchema/mariadb/SchemaConstraints",
    "TestM8HostActionAdmissionSchema/mariadb/UnknownOperationNotFound",
    "TestM8HostActionAuthorization/mariadb/data_directory/AdmitsAndGrantsVerifiedSignature",
    "TestM8HostActionAuthorization/mariadb/data_directory/CallerCanceledAfterCommitNoGrant",
    "TestM8HostActionAuthorization/mariadb/data_directory/CancelRequestedRefused",
    "TestM8HostActionAuthorization/mariadb/data_directory/CanceledContextRefused",
    "TestM8HostActionAuthorization/mariadb/data_directory/ClockRollbackStamp",
    "TestM8HostActionAuthorization/mariadb/data_directory/ConcurrentAdmissionSingleGrant",
    "TestM8HostActionAuthorization/mariadb/data_directory/CorruptGraphNoGrant",
    "TestM8HostActionAuthorization/mariadb/data_directory/DifferentChallengeNoRearm",
    "TestM8HostActionAuthorization/mariadb/data_directory/ExactReplayConflict",
    "TestM8HostActionAuthorization/mariadb/data_directory/ExpiredDeadlineDBClock",
    "TestM8HostActionAuthorization/mariadb/data_directory/ExpiryAfterAuthorityLock",
    "TestM8HostActionAuthorization/mariadb/data_directory/IdentityMismatch",
    "TestM8HostActionAuthorization/mariadb/data_directory/IndependentReaderSeesAdmissionBeforeSign",
    "TestM8HostActionAuthorization/mariadb/data_directory/LostAdmissionAckNoSign",
    "TestM8HostActionAuthorization/mariadb/data_directory/NonceReuseAcrossAttempts",
    "TestM8HostActionAuthorization/mariadb/data_directory/ResourceAlreadyApplied",
    "TestM8HostActionAuthorization/mariadb/data_directory/ResourceNotInFlight",
    "TestM8HostActionAuthorization/mariadb/data_directory/SignFailureConsumesAdmission",
    "TestM8HostActionAuthorization/mariadb/data_directory/StoreRecordsExactRow",
    "TestM8HostActionAuthorization/mariadb/data_directory/UncommittedPredecessorNotVisible",
    "TestM8HostActionAuthorization/mariadb/data_directory/WrongAttempt",
    "TestM8HostActionAuthorization/mariadb/data_directory/WrongOwner",
    "TestM8HostActionAuthorization/mariadb/service/AdmitsAndGrantsVerifiedSignature",
    "TestM8HostActionAuthorization/mariadb/service/CallerCanceledAfterCommitNoGrant",
    "TestM8HostActionAuthorization/mariadb/service/CancelRequestedRefused",
    "TestM8HostActionAuthorization/mariadb/service/CanceledContextRefused",
    "TestM8HostActionAuthorization/mariadb/service/ClockRollbackStamp",
    "TestM8HostActionAuthorization/mariadb/service/ConcurrentAdmissionSingleGrant",
    "TestM8HostActionAuthorization/mariadb/service/CorruptGraphNoGrant",
    "TestM8HostActionAuthorization/mariadb/service/DifferentChallengeNoRearm",
    "TestM8HostActionAuthorization/mariadb/service/ExactReplayConflict",
    "TestM8HostActionAuthorization/mariadb/service/ExpiredDeadlineDBClock",
    "TestM8HostActionAuthorization/mariadb/service/ExpiryAfterAuthorityLock",
    "TestM8HostActionAuthorization/mariadb/service/IdentityMismatch",
    "TestM8HostActionAuthorization/mariadb/service/IndependentReaderSeesAdmissionBeforeSign",
    "TestM8HostActionAuthorization/mariadb/service/LostAdmissionAckNoSign",
    "TestM8HostActionAuthorization/mariadb/service/NonceReuseAcrossAttempts",
    "TestM8HostActionAuthorization/mariadb/service/PredecessorNotApplied",
    "TestM8HostActionAuthorization/mariadb/service/ResourceAlreadyApplied",
    "TestM8HostActionAuthorization/mariadb/service/ResourceNotInFlight",
    "TestM8HostActionAuthorization/mariadb/service/SignFailureConsumesAdmission",
    "TestM8HostActionAuthorization/mariadb/service/StoreRecordsExactRow",
    "TestM8HostActionAuthorization/mariadb/service/UncommittedPredecessorNotVisible",
    "TestM8HostActionAuthorization/mariadb/service/WrongAttempt",
    "TestM8HostActionAuthorization/mariadb/service/WrongOwner",
    "TestM8HostActionAuthorization/mariadb/unit_file/AdmitsAndGrantsVerifiedSignature",
    "TestM8HostActionAuthorization/mariadb/unit_file/CallerCanceledAfterCommitNoGrant",
    "TestM8HostActionAuthorization/mariadb/unit_file/CancelRequestedRefused",
    "TestM8HostActionAuthorization/mariadb/unit_file/CanceledContextRefused",
    "TestM8HostActionAuthorization/mariadb/unit_file/ClockRollbackStamp",
    "TestM8HostActionAuthorization/mariadb/unit_file/ConcurrentAdmissionSingleGrant",
    "TestM8HostActionAuthorization/mariadb/unit_file/CorruptGraphNoGrant",
    "TestM8HostActionAuthorization/mariadb/unit_file/DifferentChallengeNoRearm",
    "TestM8HostActionAuthorization/mariadb/unit_file/ExactReplayConflict",
    "TestM8HostActionAuthorization/mariadb/unit_file/ExpiredDeadlineDBClock",
    "TestM8HostActionAuthorization/mariadb/unit_file/ExpiryAfterAuthorityLock",
    "TestM8HostActionAuthorization/mariadb/unit_file/IdentityMismatch",
    "TestM8HostActionAuthorization/mariadb/unit_file/IndependentReaderSeesAdmissionBeforeSign",
    "TestM8HostActionAuthorization/mariadb/unit_file/LostAdmissionAckNoSign",
    "TestM8HostActionAuthorization/mariadb/unit_file/NonceReuseAcrossAttempts",
    "TestM8HostActionAuthorization/mariadb/unit_file/PredecessorNotApplied",
    "TestM8HostActionAuthorization/mariadb/unit_file/ResourceAlreadyApplied",
    "TestM8HostActionAuthorization/mariadb/unit_file/ResourceNotInFlight",
    "TestM8HostActionAuthorization/mariadb/unit_file/SignFailureConsumesAdmission",
    "TestM8HostActionAuthorization/mariadb/unit_file/StoreRecordsExactRow",
    "TestM8HostActionAuthorization/mariadb/unit_file/UncommittedPredecessorNotVisible",
    "TestM8HostActionAuthorization/mariadb/unit_file/WrongAttempt",
    "TestM8HostActionAuthorization/mariadb/unit_file/WrongOwner",
}
STAGE_SQLITE_LEAVES = {name.replace("/mariadb/", "/sqlite/") for name in STAGE_AUTHORIZATION_LEAVES | HOST_ACTION_LEAVES}
STAGE_REQUIRED = INSTALL_JOURNAL_LEAVES | INSTALL_RUNNER_LEAVES | STAGE_AUTHORIZATION_LEAVES | HOST_ACTION_LEAVES


def stage_event_values(omit=()):
    """Synthetic complete Go lifecycles; these are parser doubles, not DB proof."""
    values = []
    selected = {
        PACKAGE: (STAGE_REQUIRED | STAGE_SQLITE_LEAVES) - set(omit),
        "github.com/fiztoz/uptime-phoenix/internal/core/domain": {"TestProbeBinaryStageAdmissionSigningMessage"},
        "github.com/fiztoz/uptime-phoenix/internal/core/services": {"TestProbeBinaryStageAuthorizationBudget"},
        "github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning": {"TestEd25519ProbeStageGrantSigner"},
    }
    for package, names in sorted(selected.items()):
        values.append({"Action": "start", "Package": package})
        tree = {}
        for name in sorted(names):
            node = tree
            for part in name.split("/"):
                node = node.setdefault(part, {})

        def walk(node, prefix=""):
            for part, children in sorted(node.items()):
                name = prefix + part
                values.append({"Action": "run", "Package": package, "Test": name})
                walk(children, name + "/")
                values.append({"Action": "pass", "Package": package, "Test": name})

        walk(tree)
        values.append({"Action": "pass", "Package": package})
    return values


def stage_lines(values=None):
    return [json.dumps(value) + "\n" for value in (stage_event_values() if values is None else values)]


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


class BackendGateHarness:
    def run_gate(self, lines, exit_code=0, refuse_process_grant=False, scope="full",
                 cleanup_identity=None, expect_cleanup=True):
        """Exercise main with no real commands, Docker socket or database I/O."""
        process = mock.MagicMock()
        process.__enter__.return_value = process
        process.stdout = iter(lines)
        process.wait.return_value = exit_code
        output = io.StringIO()
        self.command_order = []
        self.docker_calls = []
        self.go_launches = []
        self.subprocess_calls = []
        fixture_id = "d" * 64

        def docker_result(*args, **kwargs):
            self.command_order.append("docker")
            self.docker_calls.append((args, kwargs))
            if refuse_process_grant and args[-1] == "GRANT PROCESS ON *.* TO 'phoenix'@'%'":
                raise PermissionError("offline PROCESS grant refused")
            if args[0] == "port":
                return "127.0.0.1:43306"
            if args[0] == "run" and scope == "m8-stage-authorization":
                return fixture_id
            return ""

        def run_command(args, **kwargs):
            self.subprocess_calls.append((args, kwargs))
            if args[:3] == ["docker", "container", "inspect"] and "--format" in args:
                created = next(call[0] for call in self.docker_calls if call[0][0] == "run")
                owner = created[created.index("--label") + 1].split("=", 1)[1]
                return mock.Mock(returncode=0, stdout=cleanup_identity if cleanup_identity is not None
                                 else fixture_id + " " + owner + "\n")
            return mock.Mock(returncode=0)

        def launch_process(*args, **kwargs):
            self.command_order.append("go")
            self.go_launches.append((args, kwargs))
            return process

        with (mock.patch.dict(gate.os.environ, {
                "DOCKER_HOST": "unix:///offline-gate-fixture.sock",
                "GOPATH": "/offline-gate-fixture/go",
            }, clear=True),
            mock.patch.object(sys, "argv", ["m6_dual_engine_gate.py", "--scope", scope]),
            mock.patch.object(gate, "docker", side_effect=docker_result) as docker,
            mock.patch.object(gate.Path, "glob", return_value=[Path("/offline-gate-fixture/go1.26.9/bin/go")]),
            mock.patch.object(gate.subprocess, "run", side_effect=run_command),
            mock.patch.object(gate.subprocess, "Popen", side_effect=launch_process) as launch,
            contextlib.redirect_stdout(output)):
            status = 0
            try:
                gate.main()
            except SystemExit as exc:
                status = exc.code

        # This is the production gate command, but Popen/run/docker above are
        # doubles: these tests create no containers and execute no Go tests.
        expected = ["test", "-race", "-count=1", "-json", "-timeout", "2400s", "-p", "4", "./..."]
        if scope == "m8-install-journal":
            expected = ["test", "-race", "-count=1", "-json", "-timeout", "300s", "-p", "4",
                        "./internal/core/domain", "./internal/core/services", "./internal/adapters/repository",
                        "-run", "^TestProbeInstall(Journal|Identity|Graph|States)|^TestM8InstallJournal"]
        if scope == "m8-install-runner":
            expected = ["test", "-race", "-count=1", "-json", "-timeout", "300s", "-p", "4",
                        "./internal/core/domain", "./internal/core/services", "./internal/adapters/repository",
                        "-run", "^TestProbeInstall(Journal|Identity|Graph|States)|^TestInstallRunner|^TestM8Install(Journal|Runner)"]
        if scope == "m8-stage-authorization":
            expected = ["test", "-race", "-count=1", "-json", "-timeout", "300s", "-p", "4",
                        "./internal/core/domain", "./internal/core/services", "./internal/adapters/repository",
                        "./internal/adapters/provisioning", "-run", gate.STAGE_AUTHORIZATION_RUN]
        self.assertEqual(launch.call_args.args[0][1:], expected)
        if expect_cleanup:
            self.assertEqual(docker.call_args.args[:2], ("rm", "-f"))
        return status, json.loads(output.getvalue())

    def assert_failed(self, lines, **kwargs):
        status, summary = self.run_gate(lines, **kwargs)
        self.assertEqual(status, 1)
        self.assertEqual(summary["result"], "failed")
        return summary

class StrictBackendGateTest(BackendGateHarness, unittest.TestCase):
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

    def test_each_m8_discovery_leaf_needs_an_exact_real_engine_pass(self):
        self.assertTrue(M8_LEAVES <= gate.REQUIRED)
        for leaf in sorted(M8_LEAVES):
            for action in (None, "skip", "fail"):
                with self.subTest(leaf=leaf, action=action):
                    lines = passing_events(omit={leaf})
                    lines.append(event(leaf.rsplit("/", 1)[0]))
                    lines.append(event(leaf.replace("/mariadb", "/sqlite")))
                    if action is not None:
                        lines.append(event(leaf, action))
                    summary = self.assert_failed(lines)
                    self.assertEqual(summary["missing_required"], [leaf])

    def test_each_install_journal_leaf_is_required_in_full_scope(self):
        self.assertEqual(gate.INSTALL_JOURNAL_REQUIRED, INSTALL_JOURNAL_LEAVES)
        self.assertEqual(len(INSTALL_JOURNAL_LEAVES), 34)
        self.assertTrue(INSTALL_JOURNAL_LEAVES <= gate.REQUIRED)
        for leaf in sorted(INSTALL_JOURNAL_LEAVES):
            with self.subTest(leaf=leaf):
                summary = self.assert_failed(passing_events(omit={leaf}))
                self.assertEqual(summary["missing_required"], [leaf])

    def test_scoped_journal_gate_does_not_require_unrelated_m7_or_privileges(self):
        lines = [event(name) for name in sorted(INSTALL_JOURNAL_LEAVES)] + [event()]
        status, summary = self.run_gate(lines, scope="m8-install-journal")
        self.assertEqual(status, 0)
        self.assertEqual(summary["scope"], "m8-install-journal")
        self.assertEqual(summary["mariadb_named_passes"], 34)
        self.assertEqual(summary["missing_required"], [])
        self.assertEqual(summary["named_skips"], 0)
        self.assertFalse(any("GRANT " in str(call[0]) for call in self.docker_calls))
        self.assertNotIn("MYSQL_PWD", self.go_launches[0][1]["env"])
        self.assertNotIn("MARIADB_ROOT_PASSWORD", self.go_launches[0][1]["env"])
        self.assertFalse(summary["full_section_13_matrix"])

    def test_scoped_journal_missing_failed_or_skipped_leaf_cannot_be_replaced(self):
        for leaf in sorted(INSTALL_JOURNAL_LEAVES):
            for action in (None, "skip", "fail"):
                with self.subTest(leaf=leaf, action=action):
                    lines = [event(name) for name in sorted(INSTALL_JOURNAL_LEAVES - {leaf})]
                    lines += [event(leaf.rsplit("/", 1)[0]), event(leaf.replace("/mariadb", "/sqlite")), event()]
                    if action is not None:
                        lines.append(event(leaf, action))
                    status, summary = self.run_gate(lines, scope="m8-install-journal")
                    self.assertEqual(status, 1)
                    self.assertEqual(summary["missing_required"], [leaf])

    def test_scoped_journal_no_real_engine_execution_fails_closed(self):
        lines = [event(name.replace("/mariadb", "/sqlite")) for name in sorted(INSTALL_JOURNAL_LEAVES)] + [event()]
        status, summary = self.run_gate(lines, scope="m8-install-journal")
        self.assertEqual(status, 1)
        self.assertEqual(summary["missing_required"], sorted(INSTALL_JOURNAL_LEAVES))
        self.assertEqual(summary["mariadb_named_passes"], 0)

    def test_scoped_journal_rejects_every_skip_malformed_failure_and_nonzero_exit(self):
        good = [event(name) for name in sorted(INSTALL_JOURNAL_LEAVES)] + [event()]
        leaf = sorted(INSTALL_JOURNAL_LEAVES)[0]
        for extra, code in ((["not-json\\n"], 0), ([event(leaf, "fail")], 0),
                            ([event(sorted(OPTIONAL_SKIPS)[0], "skip")], 0), ([], 1)):
            with self.subTest(extra=extra, code=code):
                status, summary = self.run_gate(good + extra, exit_code=code, scope="m8-install-journal")
                self.assertEqual(status, 1)
                self.assertEqual(summary["result"], "failed")

    def test_runner_exact_leaves_are_required_in_full_scope(self):
        self.assertEqual(gate.INSTALL_RUNNER_REQUIRED, INSTALL_RUNNER_LEAVES)
        self.assertEqual(len(INSTALL_RUNNER_LEAVES), 11)
        self.assertTrue(INSTALL_RUNNER_LEAVES <= gate.REQUIRED)
        for leaf in sorted(INSTALL_RUNNER_LEAVES):
            with self.subTest(leaf=leaf):
                summary = self.assert_failed(passing_events(omit={leaf}))
                self.assertEqual(summary["missing_required"], [leaf])

    def test_runner_scope_requires_original_journal_and_runner_without_extra_grants(self):
        required = INSTALL_JOURNAL_LEAVES | INSTALL_RUNNER_LEAVES
        status, summary = self.run_gate([event(n) for n in sorted(required)] + [event()], scope="m8-install-runner")
        self.assertEqual(status, 0)
        self.assertEqual(summary["scope"], "m8-install-runner")
        self.assertEqual(summary["mariadb_named_passes"], 45)
        self.assertEqual(summary["missing_required"], [])
        self.assertFalse(any("GRANT " in str(call[0]) for call in self.docker_calls))
        self.assertNotIn("MYSQL_PWD", self.go_launches[0][1]["env"])
        self.assertFalse(summary["full_section_13_matrix"])

    def test_runner_scope_missing_failed_skipped_parent_or_sqlite_substitutes_refuse(self):
        required = INSTALL_JOURNAL_LEAVES | INSTALL_RUNNER_LEAVES
        for leaf in sorted(required):
            for action in (None, "fail", "skip"):
                with self.subTest(leaf=leaf, action=action):
                    lines = [event(n) for n in sorted(required - {leaf})]
                    lines += [event(leaf.rsplit("/", 1)[0]), event(leaf.replace("/mariadb", "/sqlite")), event()]
                    if action is not None:
                        lines.append(event(leaf, action))
                    status, summary = self.run_gate(lines, scope="m8-install-runner")
                    self.assertEqual(status, 1)
                    self.assertEqual(summary["missing_required"], [leaf])

    def test_runner_scope_no_real_engine_execution_fails_closed(self):
        required = INSTALL_JOURNAL_LEAVES | INSTALL_RUNNER_LEAVES
        status, summary = self.run_gate([event(n.replace("/mariadb", "/sqlite")) for n in sorted(required)] + [event()], scope="m8-install-runner")
        self.assertEqual(status, 1)
        self.assertEqual(summary["missing_required"], sorted(required))
        self.assertEqual(summary["mariadb_named_passes"], 0)

    def test_runner_scope_refuses_malformed_failure_any_skip_or_nonzero_exit(self):
        required = INSTALL_JOURNAL_LEAVES | INSTALL_RUNNER_LEAVES
        good = [event(n) for n in sorted(required)] + [event()]
        for extra, code in ((["not-json\\n"], 0), ([event(sorted(required)[0], "fail")], 0),
                            ([event(sorted(OPTIONAL_SKIPS)[0], "skip")], 0), ([], 1)):
            with self.subTest(extra=extra, code=code):
                status, summary = self.run_gate(good + extra, exit_code=code, scope="m8-install-runner")
                self.assertEqual(status, 1)
                self.assertEqual(summary["result"], "failed")

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
        if engine_leaves < 300:
            self.assert_failed(passing_events(mariadb_count=299 - engine_leaves))
        else:
            # The required engine leaves alone now exceed the 300-pass floor, so
            # it can no longer be under-shot; a missing required leaf still fails.
            omitted = sorted(name for name in gate.REQUIRED if "/mariadb" in name)[0]
            self.assert_failed(passing_events(omit=(omitted,), mariadb_count=0))

    def test_mariadb_skip_is_rejected(self):
        summary = self.assert_failed(passing_events() + [event("TestExistingStorage/mariadb/skipped", "skip")])
        self.assertEqual(summary["mariadb_named_skips"], 1)

    def test_unrecognized_skip_malformed_stream_and_nonzero_go_exit_fail(self):
        self.assert_failed(passing_events() + [event("TestUnexpectedOptionalTarget", "skip")])
        summary = self.assert_failed(passing_events() + ["not-json\n"])
        self.assertEqual(summary["malformed_json_lines"], 1)
        summary = self.assert_failed(passing_events(), exit_code=1)
        self.assertEqual(summary["go_exit"], 1)


class StageAuthorizationGateTest(BackendGateHarness, unittest.TestCase):
    """The new scope must reject false evidence even when Go reports exit zero."""

    def run_stage(self, values=None, **kwargs):
        return self.run_gate(stage_lines(values), scope="m8-stage-authorization", **kwargs)

    def test_stage_fixed_names_preserve_all_old_requirements(self):
        self.assertEqual(gate.STAGE_AUTHORIZATION_REQUIRED, STAGE_AUTHORIZATION_LEAVES)
        self.assertEqual(gate.HOST_ACTION_REQUIRED, HOST_ACTION_LEAVES)
        self.assertEqual(gate.STAGE_AUTHORIZATION_SQLITE_REQUIRED, STAGE_SQLITE_LEAVES)
        self.assertEqual(len(STAGE_AUTHORIZATION_LEAVES), 18)
        self.assertEqual(len(HOST_ACTION_LEAVES), 72)
        self.assertEqual(len(STAGE_REQUIRED), 135)
        self.assertTrue(STAGE_REQUIRED <= gate.REQUIRED)
        status, summary = self.run_stage()
        self.assertEqual(status, 0)
        self.assertEqual(summary["lifecycle_errors"], [])
        self.assertEqual(summary["missing_required_lifecycles"], [])
        self.assertEqual(summary["passed_packages"], 4)
        self.assertFalse(any("GRANT " in str(call[0]) for call in self.docker_calls))

    def test_stage_every_required_leaf_needs_its_own_run_and_pass(self):
        for leaf in sorted(STAGE_REQUIRED | STAGE_SQLITE_LEAVES):
            with self.subTest(leaf=leaf):
                status, summary = self.run_stage(stage_event_values(omit={leaf}))
                self.assertEqual(status, 1)
                self.assertIn(leaf, summary["missing_required_lifecycles"])

    def test_stage_each_new_leaf_rejects_skip_failure_wrong_engine_and_parent_only(self):
        for leaf in sorted(STAGE_AUTHORIZATION_LEAVES):
            for action in ("skip", "fail"):
                with self.subTest(leaf=leaf, action=action):
                    values = stage_event_values()
                    next(v for v in values if v.get("Test") == leaf and v["Action"] == "pass")["Action"] = action
                    status, summary = self.run_stage(values)
                    self.assertEqual(status, 1)
                    self.assertIn(leaf, summary["missing_required_lifecycles"])
            for replacement in (leaf.replace("/mariadb/", "/sqlite/"), leaf.rsplit("/", 1)[0], leaf + "/mariadb"):
                with self.subTest(leaf=leaf, replacement=replacement):
                    values = stage_event_values(omit={leaf})
                    at = next(i for i, v in enumerate(values) if v["Package"] == PACKAGE and v["Action"] == "pass" and not v.get("Test"))
                    values[at:at] = [{"Action": "run", "Package": PACKAGE, "Test": replacement},
                                    {"Action": "pass", "Package": PACKAGE, "Test": replacement}]
                    status, summary = self.run_stage(values)
                    self.assertEqual(status, 1)
                    self.assertIn(leaf, summary["missing_required_lifecycles"])

    def test_stage_wrong_package_cannot_supply_required_names(self):
        values = stage_event_values()
        for value in values:
            if value["Package"] == PACKAGE:
                value["Package"] = "github.com/fiztoz/uptime-phoenix/internal/core/domain"
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertEqual(set(summary["missing_required_lifecycles"]), STAGE_REQUIRED | STAGE_SQLITE_LEAVES)

    def test_stage_terminal_name_bag_and_zero_selection_refuse(self):
        values = stage_event_values()
        for selected in ([], [v for v in values if v["Action"] != "run"],
                         [v for v in values if not v.get("Test") or "/" not in v["Test"]]):
            with self.subTest(length=len(selected)):
                status, summary = self.run_stage(selected)
                self.assertEqual(status, 1)
                self.assertTrue(summary["lifecycle_errors"])

    def test_stage_duplicate_run_or_terminal_refuses(self):
        leaf = sorted(STAGE_AUTHORIZATION_LEAVES)[0]
        for action, reason in (("run", "duplicate test run"), ("pass", "terminal test without live run")):
            with self.subTest(action=action):
                values = stage_event_values()
                at = next(i for i, v in enumerate(values) if v.get("Test") == leaf and v["Action"] == action)
                values.insert(at + 1, dict(values[at]))
                status, summary = self.run_stage(values)
                self.assertEqual(status, 1)
                self.assertIn(reason, summary["lifecycle_errors"])

    def test_stage_missing_package_or_top_completion_refuses(self):
        for package in sorted(gate.StageAuthorizationEvents.packages):
            for action in ("start", "pass"):
                values = [v for v in stage_event_values() if not (v["Package"] == package and v["Action"] == action and not v.get("Test"))]
                with self.subTest(package=package, action=action):
                    status, summary = self.run_stage(values)
                    self.assertEqual(status, 1)
                    self.assertTrue(summary["lifecycle_errors"])
        values = [v for v in stage_event_values() if not (v.get("Test") == "TestM8BinaryStageAuthorization" and v["Action"] == "pass")]
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertTrue(summary["lifecycle_errors"])

    def test_stage_literal_slashes_do_not_invent_parent_events(self):
        # Real Go t.Run("attempts are copied before I/O", ...) emits one
        # child name containing two slashes, not an intermediate I event.
        values = stage_event_values()
        package = "github.com/fiztoz/uptime-phoenix/internal/core/services"
        root = "TestInstallRunnerDefensiveCopies"
        child = root + "/attempts_are_copied_before_I/O"
        at = next(i for i, value in enumerate(values) if value["Package"] == package
                  and value["Action"] == "pass" and not value.get("Test"))
        values[at:at] = [{"Action": action, "Package": package, "Test": name}
                        for action, name in (("run", root), ("run", child),
                                             ("pass", child), ("pass", root))]
        status, summary = self.run_stage(values)
        self.assertEqual(status, 0)
        self.assertEqual(summary["lifecycle_errors"], [])
        # A previously emitted, completed ancestor cannot be skipped in favor
        # of a still-running root, even for names that contain literal slashes.
        values = stage_event_values()
        values[at:at] = [{"Action": action, "Package": package, "Test": name}
                        for action, name in (("run", root), ("run", child),
                                             ("pass", child), ("run", child + "/nested"),
                                             ("pass", child + "/nested"), ("pass", root))]
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertIn("test without live parent", summary["lifecycle_errors"])

    def test_stage_required_engine_parent_cannot_be_omitted(self):
        parent = "TestM8BinaryStageAuthorization/mariadb"
        values = [value for value in stage_event_values() if value.get("Test") != parent]
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertTrue(summary["lifecycle_errors"])
        self.assertEqual(summary["missing_required_lifecycles"], [])

    def test_stage_parent_cannot_complete_while_child_is_running(self):
        values = stage_event_values()
        leaf = sorted(STAGE_AUTHORIZATION_LEAVES)[0]
        parent = leaf.rsplit("/", 1)[0]
        terminal = next(v for v in values if v.get("Test") == parent and v["Action"] == "pass")
        values.remove(terminal)
        at = next(i for i, v in enumerate(values) if v.get("Test") == leaf and v["Action"] == "run")
        values.insert(at + 1, terminal)
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertIn("parent completed before children", summary["lifecycle_errors"])

    def test_stage_pause_and_resume_require_legal_order(self):
        for actions, expected in ((("pause", "cont"), 0), (("cont",), 1), (("pause", "pause", "cont"), 1), (("pause",), 1)):
            values = stage_event_values()
            leaf = sorted(STAGE_AUTHORIZATION_LEAVES)[0]
            at = next(i for i, v in enumerate(values) if v.get("Test") == leaf and v["Action"] == "run")
            values[at + 1:at + 1] = [{"Action": action, "Package": PACKAGE, "Test": leaf} for action in actions]
            with self.subTest(actions=actions):
                status, _summary = self.run_stage(values)
                self.assertEqual(status, expected)

    def test_stage_abnormal_output_exit_or_package_failure_refuses(self):
        for output in ("panic: fixture", "fatal error: fixture", "WARNING: DATA RACE", "test timed out"):
            values = stage_event_values()
            values.insert(1, {"Action": "output", "Package": values[0]["Package"], "Output": output})
            status, summary = self.run_stage(values)
            self.assertEqual(status, 1)
            self.assertIn("abnormal output", summary["lifecycle_errors"])
        status, summary = self.run_stage(exit_code=1)
        self.assertEqual(status, 1)
        self.assertEqual(summary["go_exit"], 1)
        values = stage_event_values()
        values[-1]["Action"] = "fail"
        status, summary = self.run_stage(values)
        self.assertEqual(status, 1)
        self.assertTrue(summary["lifecycle_errors"])

    def test_stage_malformed_typed_or_duplicate_json_refuses(self):
        for extra in ("not-json\n", "[]\n", "null\n", '{"Action":"pass","Action":"run","Package":"fixture"}\n',
                      '{"Action":[],"Package":"fixture"}\n', '{"Action":"pass","Package":null}\n',
                      '{"Action":"pass","Package":"fixture","Test":null}\n'):
            with self.subTest(extra=extra):
                status, summary = self.run_gate(stage_lines() + [extra], scope="m8-stage-authorization")
                self.assertEqual(status, 1)
                self.assertGreater(summary["malformed_json_lines"], 0)
        lines = stage_lines()
        lines[-1] = lines[-1].rstrip("\n")
        status, summary = self.run_gate(lines, scope="m8-stage-authorization")
        self.assertEqual(status, 1)
        self.assertIn("unterminated event line", summary["lifecycle_errors"])

    def test_stage_cleanup_requires_matching_container_and_owner_identity(self):
        for identity in ("", "d" * 64 + " foreign-owner\n", "e" * 64 + " foreign-owner\n"):
            with self.subTest(identity=identity):
                with self.assertRaisesRegex(RuntimeError, "identity mismatch"):
                    self.run_stage(cleanup_identity=identity, expect_cleanup=False)
                self.assertFalse(any(call[0][0] == "rm" for call in self.docker_calls))
        status, _summary = self.run_stage()
        self.assertEqual(status, 0)
        self.assertEqual(self.docker_calls[-1][0], ("rm", "-f", "-v", "d" * 64))

    def test_stage_generated_credentials_and_exact_compiler_are_scoped(self):
        status, _summary = self.run_stage()
        self.assertEqual(status, 0)
        args, kwargs = next(call for call in self.docker_calls if call[0][0] == "run")
        self.assertIn("--pull=never", args)
        self.assertNotIn("-v", args)
        self.assertNotIn("--mount", args)
        self.assertNotEqual(kwargs["env"]["MARIADB_PASSWORD"], kwargs["env"]["MARIADB_ROOT_PASSWORD"])
        self.assertGreaterEqual(len(kwargs["env"]["MARIADB_PASSWORD"]), 32)
        self.assertFalse(any(kwargs["env"]["MARIADB_PASSWORD"] in arg for arg in args))
        go_env = self.go_launches[0][1]["env"]
        self.assertTrue(go_env["PATH"].startswith("/offline-gate-fixture/go1.26.9/bin"))
        for name in ("MARIADB_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_PWD"):
            self.assertNotIn(name, go_env)
        readiness = next(call for call in self.subprocess_calls if call[0][:2] == ["docker", "exec"])
        self.assertEqual(readiness[1]["env"]["MYSQL_PWD"], kwargs["env"]["MARIADB_PASSWORD"])


def stage_mutation_verdict(lines, code, leaf, expected_assertion=None):
    """Only exact assertion failures count; raw mutant logs are never rewritten."""
    expected_names = {leaf, leaf.rsplit("/", 1)[0], leaf.split("/", 1)[0]}
    audit = gate.StageAuthorizationEvents({leaf}, packages={PACKAGE}, include_sqlite=False)
    passed, failed = set(), set()
    assertion_seen = False
    try:
        for line in lines:
            if not line.endswith("\n"):
                return False
            value = json.loads(line, object_pairs_hook=gate.unique_event_object)
            if (not isinstance(value, dict) or not isinstance(value.get("Action"), str)
                    or not isinstance(value.get("Package"), str)
                    or not isinstance(value.get("Test", ""), str)):
                return False
            name, action = value.get("Test", ""), value["Action"]
            if action == "pass" and name:
                passed.add(name)
            if action == "fail" and name:
                failed.add(name)
            if (expected_assertion and action == "output" and name == leaf
                    and expected_assertion in value.get("Output", "")):
                assertion_seen = True
            # After independently requiring the exact failure set and exit 1,
            # normalize only terminal actions in memory to reuse lifecycle
            # validation. This is NOT a passing acceptance transcript.
            normalized = dict(value)
            if expected_assertion and action == "fail":
                normalized["Action"] = "pass"
            audit.observe(normalized)
    except (ValueError, TypeError):
        return False
    errors, missing = audit.finish()
    if errors or missing:
        return False
    if expected_assertion:
        return code == 1 and failed == expected_names and not passed and assertion_seen
    return code == 0 and passed == expected_names and not failed


def stage_source_mutations():
    """Fixed mutations of authority semantics, not caller-selected gate leaves."""
    store = "internal/adapters/repository/probe_binary_stage_admission.go"
    journal = "internal/adapters/repository/probe_install_journal.go"
    service = "internal/core/services/probe_binary_stage_authorization.go"
    prefix = "TestM8BinaryStageAuthorization/sqlite/"
    return (
        ("owner", prefix + "IdentityOwnerAttemptMismatch", "zero grant/fixed refusal missing", {
            store: (("current.OwnerID != intent.OwnerID", "false"),),
        }),
        ("deadline", prefix + "ExpiryAfterLock", "zero grant/fixed refusal missing", {
            store: (
                ("reconciled, err := graph.reconcileExpiry(ctx, tx, now)", "reconciled, err := false, error(nil)"),
                ("!now.Before(current.DeadlineAt)", "false"),
                ("!stamp.Before(current.DeadlineAt)", "false"),
            ),
        }),
        ("predecessor", prefix + "RejectUncommittedPredecessor", "predecessor refusal must leave zero grant and zero signing", {
            journal: (("if !out.Valid() {\n\t\treturn domain.ProbeInstallOperation{}, fmt.Errorf(\"corrupt probe install journal %s: %w\", m.OperationID, domain.ErrValidation)",
                       "if false {\n\t\treturn domain.ProbeInstallOperation{}, fmt.Errorf(\"corrupt probe install journal %s: %w\", m.OperationID, domain.ErrValidation)"),),
            store: (("current.Resources[i].State != domain.ProbeInstallResourceApplied", "false"),),
        }),
        ("single_use", prefix + "ExactReplayNoGrant", "zero grant/fixed refusal missing", {
            store: (("if installJournalRaced(err) {\n\t\t\t\treturn ports.ErrConflict\n\t\t\t}",
                     "if installJournalRaced(err) {\n\t\t\t\tout = domain.ProbeBinaryStageAdmission{Request: request, AdmittedAt: stamp}\n\t\t\t\treturn nil\n\t\t\t}"),),
        }),
        ("sign_before_commit", prefix + "IndependentReaderSeesAdmissionBeforeSign", "expected exact cryptographically verified admission", {
            service: (
                ("admission, err := s.repository.AdmitBinaryStage(opCtx, request)",
                 "grant, err := s.signer.SignStageAdmission(opCtx, domain.ProbeBinaryStageAdmission{Request: request, AdmittedAt: time.Now().UTC().Truncate(time.Microsecond)})\n\tif err != nil { return zero, installJournalError(err) }\n\tadmission, err := s.repository.AdmitBinaryStage(opCtx, request)"),
                ("grant, err := s.signer.SignStageAdmission(opCtx, admission)", "// Deliberate control: retain the prematurely signed grant."),
            ),
        }),
    )


def run_stage_authorization_mutations(output=None):
    """Run SQLite semantic controls only; never replaces the dual-engine gate."""
    root = Path(__file__).resolve().parents[1]
    version = re.search(r"^go (\d+\.\d+\.\d+)$", (root / "go.mod").read_text(), re.M).group(1)
    gopath = Path(os.environ.get("GOPATH", str(Path.home() / "go")).split(os.pathsep)[0])
    choices = list((gopath / "pkg/mod/golang.org").glob(f"toolchain@v0.0.1-go{version}.*/bin/go"))
    if len(choices) != 1:
        raise RuntimeError("exact cached toolchain required; no download permitted")
    go = choices[0]
    safe = {"PATH", "HOME", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE", "LANG", "LC_ALL", "TZ", "XDG_CACHE_HOME"}
    env = {key: value for key, value in os.environ.items() if key in safe}
    env.update(GOENV="off", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOWORK="off", TEST_MARIADB_DSN="")
    env["PATH"] = str(go.parent) + os.pathsep + env.get("PATH", "")
    tool = subprocess.run([str(go), "version"], cwd=root, env=env, capture_output=True, text=True, check=True)
    if not tool.stdout.startswith(f"go version go{version} "):
        raise RuntimeError("cached toolchain identity mismatch")
    output = output or root / "tmp" / ("m8-stage-authorization-mutations-" + os.urandom(8).hex())
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    specs = stage_source_mutations()
    inputs = {name for _, _, _, patches in specs for name in patches} | {
        "internal/adapters/repository/m8_binary_stage_authorization_test.go"}
    originals = {name: (root / name).read_bytes() for name in inputs}
    results = []

    def private_write(path, text):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as handle:
            handle.write(text)

    def execute(label, leaf, overlay=None, assertion=None):
        selector = "^" + leaf.replace("/", "$/^") + "$"
        cmd = [str(go), "test", "-race", "-count=1", "-json", "-timeout=60s"]
        if overlay:
            cmd.extend(["-overlay", str(overlay)])
        cmd.extend(["./internal/adapters/repository", "-run", selector])
        started = time.monotonic()
        proc = subprocess.Popen(cmd, cwd=root, env=env, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, start_new_session=True)
        try:
            stdout, stderr = proc.communicate(timeout=120)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                stdout, stderr = proc.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                stdout, stderr = proc.communicate()
            private_write(output / (label + ".jsonl"), stdout)
            private_write(output / (label + ".stderr"), stderr)
            raise RuntimeError("owned mutation process timed out; not an intended failure") from None
        private_write(output / (label + ".jsonl"), stdout)
        private_write(output / (label + ".stderr"), stderr)
        valid = stage_mutation_verdict(stdout.splitlines(keepends=True), proc.returncode, leaf, assertion)
        results.append({"label": label, "leaf": leaf, "exit": proc.returncode,
                        "valid": valid, "seconds": round(time.monotonic() - started, 3)})
        if not valid:
            raise RuntimeError("mutation/control did not meet its exact effect assertion: " + label)

    try:
        for label, leaf, assertion, patches in specs:
            execute(label + "-pre", leaf)
            with tempfile.TemporaryDirectory(prefix=label + "-", dir=output) as work:
                mapping = {}
                for name, edits in patches.items():
                    text = originals[name].decode()
                    for old, new in edits:
                        if text.count(old) != 1:
                            raise RuntimeError("mutation anchor drifted: " + name)
                        text = text.replace(old, new)
                    destination = Path(work) / Path(name).name
                    private_write(destination, text)
                    mapping[str(root / name)] = str(destination)
                overlay = Path(work) / "overlay.json"
                private_write(overlay, json.dumps({"Replace": mapping}))
                execute(label + "-mutant", leaf, overlay, assertion)
            execute(label + "-post", leaf)
    finally:
        unchanged = all((root / name).read_bytes() == body for name, body in originals.items())
        private_write(output / "result.json", json.dumps({
            "toolchain": version, "engine": "sqlite", "results": results,
            "source_unchanged": unchanged,
            "source_hashes": {name: hashlib.sha256(body).hexdigest() for name, body in originals.items()},
            "passed": unchanged and len(results) == 15 and all(result["valid"] for result in results),
            "dual_engine_gate": False,
        }, indent=2) + "\n")
        if not unchanged:
            raise RuntimeError("source changed during semantic controls")
    print(json.dumps({"result": "passed", "semantic_mutants": 5, "unmodified_controls": 10,
                      "engine": "sqlite", "evidence": str(output)}))


class StageMutationVerdictTest(unittest.TestCase):
    def test_fixed_mutant_names_and_unique_source_anchors(self):
        root = Path(__file__).resolve().parents[1]
        specs = stage_source_mutations()
        self.assertEqual(len(specs), 5)
        self.assertEqual(len({label for label, _, _, _ in specs}), 5)
        for _, leaf, _, patches in specs:
            self.assertIn(leaf, STAGE_SQLITE_LEAVES)
            for name, edits in patches.items():
                text = (root / name).read_text()
                for old, new in edits:
                    self.assertEqual(text.count(old), 1, name)
                    text = text.replace(old, new)

    def test_mutant_verdict_rejects_false_failures_and_survivors(self):
        leaf = "TestM8BinaryStageAuthorization/sqlite/ExactReplayNoGrant"
        names = [leaf.split("/", 1)[0], leaf.rsplit("/", 1)[0], leaf]
        events = [{"Action": "start", "Package": PACKAGE}]
        events += [{"Action": "run", "Package": PACKAGE, "Test": name} for name in names]
        events.append({"Action": "output", "Package": PACKAGE, "Test": leaf, "Output": "intended assertion"})
        events += [{"Action": "fail", "Package": PACKAGE, "Test": name} for name in reversed(names)]
        events.append({"Action": "fail", "Package": PACKAGE})
        lines = stage_lines(events)
        self.assertTrue(stage_mutation_verdict(lines, 1, leaf, "intended assertion"))
        for code in (0, 2, -9):
            self.assertFalse(stage_mutation_verdict(lines, code, leaf, "intended assertion"))
        self.assertFalse(stage_mutation_verdict(lines, 1, leaf, "different assertion"))
        self.assertFalse(stage_mutation_verdict(lines, 1, leaf))
        for abnormal in ("panic: fixture", "WARNING: DATA RACE", "test timed out"):
            altered = [dict(value) for value in events]
            altered[4]["Output"] += abnormal
            self.assertFalse(stage_mutation_verdict(stage_lines(altered), 1, leaf, "intended assertion"))
        self.assertFalse(stage_mutation_verdict(lines[:-1], 1, leaf, "intended assertion"))
        good = [dict(value, Action="pass") if value["Action"] == "fail" else value for value in events]
        self.assertTrue(stage_mutation_verdict(stage_lines(good), 0, leaf))
        self.assertFalse(stage_mutation_verdict(stage_lines(good), 0, leaf, "intended assertion"))


class GoToolchainPinTest(unittest.TestCase):
    """Active build and verification pins must follow the module's patch version."""

    def assert_pins(self, version, values):
        self.assertTrue(values, "expected at least one active toolchain pin")
        for value in values:
            self.assertEqual(value, version, "toolchain pin drifted from go.mod")

    def test_active_pins_match_go_mod(self):
        root = Path(__file__).resolve().parent.parent
        version = re.search(r"^go (\d+\.\d+\.\d+)$", (root / "go.mod").read_text(), re.M)
        self.assertIsNotNone(version, "go.mod must pin an exact Go patch release")
        version = version.group(1)
        patterns = {
            "Makefile": r"^export GOTOOLCHAIN := go(\d+\.\d+\.\d+)$",
            "Dockerfile": r"golang:(\d+\.\d+\.\d+)-alpine",
            "Dockerfile.probe": r"golang:(\d+\.\d+\.\d+)-alpine",
            "Dockerfile.split": r"golang:(\d+\.\d+\.\d+)-alpine",
            ".github/workflows/ci.yml": r"GOTOOLCHAIN: go(\d+\.\d+\.\d+)",
            ".github/workflows/release.yml": r"GOTOOLCHAIN: go(\d+\.\d+\.\d+)",
            "web/tests/start-e2e-server.sh": r"GOTOOLCHAIN:-go(\d+\.\d+\.\d+)",
            "scripts/m6_dual_engine_gate.py": r"toolchain@v0\.0\.1-go(\d+\.\d+\.\d+)\.\*",
        }
        for path, pattern in patterns.items():
            with self.subTest(path=path):
                self.assert_pins(version, re.findall(pattern, (root / path).read_text(), re.M))

    def test_same_minor_patch_drift_is_rejected(self):
        with self.assertRaises(AssertionError):
            self.assert_pins("1.26.9", ["1.26.8"])


if __name__ == "__main__":
    if "--stage-authorization-mutations" in sys.argv:
        parser = argparse.ArgumentParser(description="Fixed SQLite negative controls; not the dual-engine acceptance gate")
        parser.add_argument("--stage-authorization-mutations", action="store_true", required=True)
        parser.add_argument("--output", type=Path, help="new private evidence directory (must not exist)")
        args = parser.parse_args()
        run_stage_authorization_mutations(args.output)
    else:
        unittest.main()
