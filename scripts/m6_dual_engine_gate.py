#!/usr/bin/env python3
"""Backend race gate on a newly created, disposable MariaDB test DB.

Default scope is the full backend; --scope m8-install-journal selects the
journal's hermetic domain/use-case and exact SQLite/MariaDB acceptance leaves.
--scope m8-install-runner also requires the one-shot coordinator's real-store leaves.
--scope m8-stage-authorization adds irreversible admission/signing acceptance and
validates ordered package/test lifecycles, including its 18 SQLite leaves.
The image and Go modules/toolchain must already be cached. This gate is a
backend regression run, NOT proof that the full section-13 matrix or a canary
was executed. Never accepts an externally supplied test DSN.
"""

import argparse
import collections
import json
import os
import secrets
from pathlib import Path
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_IMAGE = "mariadb:11"
SKIPS_ALLOWED = {
    "TestDatabaseChecker_Check_MongoDB_RealServer",  # optional external target
    "TestTelegramSender_Send_DownSeverity",          # optional provider
    "TestEdgeCheckCrashChild",                        # killed only by its parent test
    "TestConfigGroupCycleHelperProcess",               # invoked by required parent cases below
    "TestEdgeDiskFullCriticalCommit",                 # dedicated Linux tmpfs gate
}
REQUIRED = {
    "TestConfigGroupCycle_DoesNotKillProcess/self",
    "TestConfigGroupCycle_DoesNotKillProcess/two",
    "TestConfigGroupCycle_DoesNotKillProcess/three",
    "TestEdgeCheckCrashAroundCommit/inside-transaction",
    "TestEdgeCheckCrashAroundCommit/after-commit",
    "TestHubWorkerReadinessAttestationIsUtcBound/mariadb",
    "TestSession_IncomingFrameRejections/oversized_inbound_frame_terminates_the_reader",
    "TestMariaDB037ResumesEveryCommittedStatementPrefix",
    "TestMariaDB037RejectsMalformedExistingIndex",
    "TestMariaDBMigrationOwnershipBlocksUntilOwnerReleases",
    "TestMariaDBMigrationDiscardsConnectionWhenLockReleaseFails",
    "TestDeclarativeRestoreFleetAdmission/mariadb",
    "TestMonitorUpdatePreservesConcurrentWorkerLease/sqlite",
    "TestMonitorUpdatePreservesConcurrentWorkerLease/mariadb",
    # These exact unsuffixed leaves use MariaDB-only fixtures. They must run;
    # aggregate engine counts or a parent PASS cannot establish their coverage.
    "TestMonitorMutationsWaitBeforeAppliedSourceGraph/update",
    "TestMonitorMutationsWaitBeforeAppliedSourceGraph/delete",
    "TestMonitorMutationsRejectMissingLocalRegistration/update",
    "TestMonitorMutationsRejectMissingLocalRegistration/delete",
    "TestMonitorDeleteConcurrencyDiagnostic/AppliedSourceReaderVersusDelete",
    "TestMonitorDeleteConcurrencyDiagnostic/DeleteVersusWorkerLeaseLifecycle",
    "TestMonitorDeleteReplayDiagnostic/ReplayCommitVersusDelete",
    "TestMonitorDeleteReplayDiagnostic/CurrentSnapshotCommitVersusDelete",
    # M7 receipt-backed reads must execute on InnoDB, not only SQLite.
    "TestRemoteAppliedConfigContract/mariadb/ImmutableReceiptSelection",
    "TestRemoteAppliedConfigContract/mariadb/ConcurrentReceiptRead",
    "TestRemoteAppliedConfigContract/mariadb/MissingReceipt",
    "TestRemoteAppliedConfigContract/mariadb/ReceiptHash",
    "TestRemoteAppliedConfigContract/mariadb/ReceiptHub",
    "TestRemoteAppliedConfigContract/mariadb/ReceiptCount",
    "TestRemoteAppliedConfigContract/mariadb/ReceiptTime",
    "TestRemoteAppliedConfigContract/mariadb/SnapshotHash",
    "TestRemoteAppliedConfigContract/mariadb/SnapshotHub",
    "TestRemoteAppliedConfigContract/mariadb/ActiveCount",
    "TestRemoteAppliedConfigContract/mariadb/ForeignActiveTarget",
    "TestRemoteAppliedConfigContract/mariadb/AuthenticationAndValidation",
    "TestDeliveryConfigEvidenceAPI/mariadb",
    "TestLocalDeliveryConfigContract/mariadb/ImmutableReceiptSelection",
    "TestLocalDeliveryConfigContract/mariadb/ConcurrentReceiptRead",
    "TestLocalDeliveryConfigContract/mariadb/MissingReceipt",
    "TestLocalDeliveryConfigContract/mariadb/ReceiptHash",
    "TestLocalDeliveryConfigContract/mariadb/ReceiptCount",
    "TestLocalDeliveryConfigContract/mariadb/ReceiptTime",
    "TestLocalDeliveryConfigContract/mariadb/SnapshotHash",
    "TestLocalDeliveryConfigContract/mariadb/ForeignActiveTarget",
    "TestLocalDeliveryConfigContract/mariadb/AuthenticationAndValidation",
    "TestLocalDeliveryConfigEvidenceAPI/mariadb",
    "TestAssignmentRevisionBindingContract/mariadb/LocalBoundABA",
    "TestAssignmentRevisionBindingContract/mariadb/StalePreparedNoLegacyFallback",
    "TestAssignmentRevisionBindingContract/mariadb/LegacyLocalExecutionAndRefresh",
    "TestAssignmentRevisionBindingContract/mariadb/RemoteReceiptBinding",
    "TestAssignmentRevisionBindingContract/mariadb/InvalidSourceRevision",
    "TestAssignmentRevisionSessionContract/mariadb/regional",
    "TestAssignmentRevisionSessionContract/mariadb/aggregate",
    "TestAssignmentRevisionSessionContract/mariadb/both",
    # Local regional application facts must exercise real hub transactions.
    "TestLocalRegionalApplicationContract/mariadb/regional",
    "TestLocalRegionalApplicationContract/mariadb/aggregate",
    "TestLocalRegionalApplicationContract/mariadb/both",
    "TestLocalRegionalApplicationContract/mariadb/ActivationRollback",
    "TestLocalRegionalApplicationContract/mariadb/RetryAndUpgrade",
    "TestLocalRegionalApplicationContract/mariadb/PartialUpgrade",
    "TestLocalRegionalApplicationContract/mariadb/RecordingFence",
    "TestLocalRegionalApplicationContract/mariadb/Migration",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/assignment_revision",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/generation",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/hub_id",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/sha256",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/monitor_id",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/stream_id",
    "TestLocalRegionalApplicationContract/mariadb/PresentMismatch/alert_delivery",
    "TestLocalRegionalApplicationContract/mariadb/OutboxBeforeSend",
    "TestLocalRegionalApplicationContract/mariadb/EscalationEnqueue",
    # Authenticated reports need real receipt/session/expiry/atomicity coverage.
    *{
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
    },
    *{
        f"TestDeliveryPreparationContract/mariadb/{case}"
        for case in (
            "Positive", "Prerequisites", "CurrentABA", "ImmutableDuplicates",
            "CapabilityStarvation", "StaleSessionExpiryRollback", "ServiceDispatch",
            "StaleSessionExpiryRollback/owner", "StaleSessionExpiryRollback/generation",
            "StaleSessionExpiryRollback/hub",
        )
    },
    *{f"TestDeliveryPreparationSessionContract/mariadb/{mode}"
      for mode in ("regional", "aggregate", "both")},
    *{f"TestOperativeCutoverContract/mariadb/{case}" for case in ("ColdMembership", "DrainAmbiguousBlocks", "DrainPendingAndStarted", "FencesAndStageOrder", "GateSuppression", "LocalProviderJournal", "MigrationConstraints", "MissingPreparation", "RemoteMembership", "aggregate", "both",)},
    *{f"TestOperativeRefinementContract/mariadb/{case}" for case in ("AdoptionFutureReceipt", "AdoptionObservationBoundary", "AdoptionReceiptBoundary", "BusyCodeAlignsAndRearms", "CorruptionBlocks", "DigestMismatchRefused", "DigestOnlyPlans", "DisableDrainsApplying", "DisableDrainsQuiesced", "DisableDrainsQuiescing", "RemoteBusyRearmsFromReceipt", "RepeatThrottle", "ResumeWithdrawsQueuedPhase", "StandbyNeverPreventsOperativeOpen",)},
    *{f"TestOperativeProviderLifecycle/mariadb/{case}" for case in ("ProviderErrorBlocksDrain", "aggregate", "both",)},
    *{f"TestOperativeCutoverSessionContract/mariadb/{case}" for case in ("aggregate", "both",)},
    *{f"TestOperativeUpgradeContract/mariadb/{case}" for case in ("LegacyAmbiguity", "HeldRuntime",)},
    *{f"TestOperativeProviderFences/mariadb/{case}" for case in ("MissingRegionalFact", "MissingPreparation", "AvailableAt", "FinishClockRollback")},
    *{f"TestOperativeActivationCycle/mariadb/{case}" for case in ("aggregate", "both",)},
    *{f"TestAggregateStandbyContract/mariadb/{case}"
      for case in (
          "regional", "aggregate", "both", "ColdPlanningDeadline", "EvidenceFilters",
          "AdministrativeABA", "ChannelIdentity", "AtomicRollback", "LeaseFences",
          "ExpiryDuringEvaluation", "LockWaitExpiry", "ConcurrentWorkers", "Migration",
          "WorkerBeforeAssignment", "CurrentAfterReplay", "UnknownHolds",
          "CorruptStandbyRegional", "ConcurrentStateWriter", "ClockDomains",
          "RepeatSettings", "IndependentBatch",
      )},
    *{f"TestDeliveryPreparationRoundContract/mariadb/{case}"
      for case in (
          "RemoteReuseCold", "TerminalCooldownRearm", "ExpiryRearm", "MixedAllSources",
          "HistoricalFailure", "ABA", "AtomicRollback", "CorruptMembership",
          "ConcurrentCreators", "CanceledStaleAuthority", "Migration",
      )},
    *{f"TestLocalDeliveryPreparationRoundRefresh/mariadb/{mode}" for mode in ("aggregate", "both")},
    *{f"TestDeliveryPreparationRoundSessionContract/mariadb/{mode}" for mode in ("aggregate", "both")},
    *{f"TestLocalDeliveryPreparationContract/mariadb/{case}"
      for case in (
          "regional", "aggregate", "both", "AtomicRollback", "RetryAndColdUpgrade",
          "PartialUpgrade", "ModeAndPolicyABA", "PrerequisitesAndCancellation",
          "ConcurrentSelection", "Migration", "WorkerBeforeAssignment",
          "PresentMismatch/assignment_revision", "PresentMismatch/generation",
          "PresentMismatch/hub_id", "PresentMismatch/sha256", "PresentMismatch/monitor_id",
          "PresentMismatch/stream_id", "PresentMismatch/alert_delivery",
      )},
}

# M8 discovery must exercise real database authority, not merely compile or
# pass a SQLite-only/parent test. Keep each operation leaf mandatory.
REQUIRED |= {
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


INSTALL_JOURNAL_REQUIRED = {
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
    f"TestM8InstallJournalClockAfterAuthorityLock/mariadb/{action}"
    for action in ("Begin", "Reserve", "Record", "Finish", "Observe", "Cancel")
}
INSTALL_RUNNER_REQUIRED = {
    f"TestM8InstallRunner/mariadb/{case}"
    for case in (
        "ResourcesReady/systemd", "ResourcesReady/docker", "ConcurrentRunSingleBegin",
        "LostReservationAckNoInvocation", "RemoteCancelJoinsBeforeTerminal",
        "OwnerLossUnknownStopsDependents", "FinalAppliedOwnerLoss", "ColdExpiryRejectsLateApplied",
        "MalformedOutcomeCannotAdvance", "IdentityOwnerNoDispatch",
        "ObservationLossStopsDispatch",
    )
}
STAGE_AUTHORIZATION_REQUIRED = {
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
STAGE_AUTHORIZATION_SQLITE_REQUIRED = {
    name.replace("/mariadb/", "/sqlite/") for name in STAGE_AUTHORIZATION_REQUIRED
}
STAGE_AUTHORIZATION_RUN = (
    "^TestProbeInstall(Journal|Identity|Graph|States)|^TestInstallRunner|"
    "^TestM8Install(Journal|Runner)|^TestM8BinaryStageAuthorization$|"
    "^TestProbeBinaryStage(Admission|Grant|Authorization)|^TestEd25519ProbeStageGrantSigner"
)
REQUIRED |= INSTALL_JOURNAL_REQUIRED | INSTALL_RUNNER_REQUIRED | STAGE_AUTHORIZATION_REQUIRED
STAGE_OWNER_LABEL = "io.uptime-phoenix.stage-authorization-run"


class StageAuthorizationEvents:
    """Validate complete ordered lifecycles in the new bounded scope."""

    repository = "github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
    packages = {
        repository,
        "github.com/fiztoz/uptime-phoenix/internal/core/domain",
        "github.com/fiztoz/uptime-phoenix/internal/core/services",
        "github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning",
    }

    def __init__(self, required, *, packages=None, include_sqlite=True):
        # Main always supplies the fixed scope. Narrow internal use is solely
        # for the separately reported, fixed-name semantic mutation controls.
        self.required = required | (STAGE_AUTHORIZATION_SQLITE_REQUIRED if include_sqlite else set())
        self.packages = self.packages if packages is None else packages
        # Frozen repository cases use real root and engine t.Run calls. Keep
        # those mandatory even though deeper name segments may be literals.
        self.required_parents = {"/".join(name.split("/")[:depth])
                                 for name in self.required for depth in (1, 2)}
        self.started = set()
        self.completed = set()
        self.states = {}
        self.errors = []

    def refuse(self, reason):
        if len(self.errors) < 20:
            self.errors.append(reason)

    def observe(self, event):
        package, action, name = event["Package"], event["Action"], event.get("Test", "")
        if package not in self.packages:
            self.refuse("unexpected package")
            return
        if action == "start" and not name:
            if package in self.started:
                self.refuse("duplicate package start")
            self.started.add(package)
            return
        if package not in self.started or package in self.completed:
            self.refuse("event outside live package")
            return
        if action == "output":
            output = event.get("Output")
            if not isinstance(output, str) or any(marker in output for marker in (
                    "panic:", "fatal error:", "WARNING: DATA RACE", "test timed out")):
                self.refuse("abnormal output")
            return
        if not name:
            if action != "pass" or any(p == package and state != "pass" for (p, _), state in self.states.items()):
                self.refuse("abnormal or incomplete package completion")
            if not any(p == package for p, _ in self.states):
                self.refuse("package selected zero named tests")
            self.completed.add(package)
            return
        item = (package, name)
        if action == "run":
            if item in self.states:
                self.refuse("duplicate test run")
            if "/" in name:
                # A slash can occur inside a single t.Run argument (e.g. I/O).
                # Only emitted ancestors have lifecycles; textual prefixes do
                # not invent parent run/pass events. Never bypass the nearest
                # emitted ancestor when it has already completed or paused.
                parts = name.split("/")
                ancestors = ("/".join(parts[:depth]) for depth in range(len(parts)-1, 0, -1))
                parent = next((candidate for candidate in ancestors
                               if (package, candidate) in self.states), None)
                if parent is None or self.states[(package, parent)] != "run":
                    self.refuse("test without live parent")
            self.states[item] = "run"
        elif action == "pause":
            if self.states.get(item) != "run":
                self.refuse("pause without run")
            self.states[item] = "pause"
        elif action == "cont":
            if self.states.get(item) != "pause":
                self.refuse("continue without pause")
            self.states[item] = "run"
        elif action in ("pass", "fail", "skip"):
            if self.states.get(item) != "run":
                self.refuse("terminal test without live run")
            if any(p == package and child.startswith(name + "/") and state in ("run", "pause")
                   for (p, child), state in self.states.items()):
                self.refuse("parent completed before children")
            if action != "pass":
                self.refuse("failed or skipped named test")
            self.states[item] = action
        else:
            self.refuse("unknown event action")

    def finish(self):
        if self.started != self.packages or self.completed != self.packages:
            self.refuse("missing package lifecycle")
        if any(state != "pass" for state in self.states.values()):
            self.refuse("incomplete or unsuccessful named lifecycle")
        if any(self.states.get((self.repository, name)) != "pass" for name in self.required_parents):
            self.refuse("missing required engine parent lifecycle")
        missing = sorted(name for name in self.required
                         if self.states.get((self.repository, name)) != "pass")
        if missing:
            self.refuse("required leaf lacks repository run and pass")
        return self.errors, missing


def unique_event_object(pairs):
    """Reject duplicate JSON event fields."""
    result = {}
    for name, value in pairs:
        if name in result:
            raise ValueError("duplicate event field")
        result[name] = value
    return result


def docker(*args, env=None):
    result = subprocess.run(["docker", *args], env=env, text=True, capture_output=True, timeout=90, check=False)
    if result.returncode:
        raise RuntimeError(f"docker {args[0]} failed (exit {result.returncode})")
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scope", choices=("full", "m8-install-journal", "m8-install-runner", "m8-stage-authorization"), default="full",
                        help="full backend or bounded M8 journal/coordinator acceptance (not installation)")
    parser.add_argument("--mariadb-image", choices=(DEFAULT_IMAGE, "mariadb:12.3", "mariadb@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15"),
                        default=DEFAULT_IMAGE, help="cached disposable DB image, never a live instance")
    parser.add_argument("--go-json", type=Path,
                        help="save complete Go JSON events to a NEW private file for scenario evaluation")
    args = parser.parse_args()
    required = {
        "full": REQUIRED,
        "m8-install-journal": INSTALL_JOURNAL_REQUIRED,
        "m8-install-runner": INSTALL_JOURNAL_REQUIRED | INSTALL_RUNNER_REQUIRED,
        "m8-stage-authorization": INSTALL_JOURNAL_REQUIRED | INSTALL_RUNNER_REQUIRED | STAGE_AUTHORIZATION_REQUIRED,
    }[args.scope]
    strict_events = StageAuthorizationEvents(required) if args.scope == "m8-stage-authorization" else None
    minimum_engine_passes = 300 if args.scope == "full" else len(required)
    allowed_skips = SKIPS_ALLOWED if args.scope == "full" else set()
    event_log = None
    if args.go_json:
        # Exclusive creation preserves earlier evidence; raw migration logs may
        # contain connection details and must never be world-readable.
        descriptor = os.open(args.go_json, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        event_log = os.fdopen(descriptor, "w")
    host = os.environ.get("DOCKER_HOST") or docker(
        "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"
    )
    if not host.startswith("unix://"):
        raise RuntimeError("requires a local Unix Docker socket; refuses remote Docker")
    docker("image", "inspect", args.mariadb_image)  # Do not pull an image on demand.
    run_id = uuid.uuid4().hex
    name = "phoenix-m6-matrix-" + run_id[:10]
    summary = None
    container_id = ""
    root_password = secrets.token_urlsafe(32)
    db_password = secrets.token_urlsafe(32)
    try:
        container_id = docker("run", "-d", "--pull=never", "--name", name,
               "--label", f"{STAGE_OWNER_LABEL}={run_id}", "--memory", "1g", "--cpus", "2",
               "-p", "127.0.0.1::3306", "-e", "MARIADB_ROOT_PASSWORD",
               "-e", "MARIADB_DATABASE=phoenix_ci", "-e", "MARIADB_USER=phoenix",
               "-e", "MARIADB_PASSWORD", args.mariadb_image,
               env={**os.environ, "MARIADB_ROOT_PASSWORD": root_password,
                    "MARIADB_PASSWORD": db_password})
        port = int(docker("port", name, "3306/tcp").split(":")[-1])
        if not 1024 < port <= 65535:
            raise RuntimeError("Docker did not bind a random localhost test port")
        for _ in range(90):
            ready = subprocess.run(
                ["docker", "exec", "-e", "MYSQL_PWD", name, "mariadb", "-uphoenix",
                 "-N", "-e", "SELECT 1", "phoenix_ci"],
                env={**os.environ, "MYSQL_PWD": db_password},
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False
            )
            if ready.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError("disposable database did not become ready")
        # Migration failure/restart tests need their own schemas on this owned
        # disposable server. Schema access for the ordinary test principal remains
        # scoped to phoenix_ci plus this test-only schema-name prefix.
        # Exact Delete diagnostics observe the held InnoDB transaction/lock
        # dependency. PROCESS permits this metadata read only on the freshly
        # created disposable server; it is not an application deployment grant.
        if args.scope == "full":
            docker("exec", "-e", "MYSQL_PWD", name, "mariadb", "-uroot", "-e",
               "GRANT PROCESS ON *.* TO 'phoenix'@'%'",
               env={**os.environ, "MYSQL_PWD": root_password})
        if args.scope == "full":
            docker("exec", "-e", "MYSQL_PWD", name, "mariadb", "-uroot", "-e",
               "GRANT ALL PRIVILEGES ON `phoenix_migration_%`.* TO 'phoenix'@'%'",
               env={**os.environ, "MYSQL_PWD": root_password})
        # Do not pass caller DB/provider credentials or integration-test toggles
        # into a disposable acceptance run. Only toolchain/cache paths survive.
        safe = {"PATH", "HOME", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE",
                "GOROOT", "LANG", "LC_ALL", "TZ", "XDG_CACHE_HOME"}
        env = {key: value for key, value in os.environ.items() if key in safe}
        # Execute the already installed exact toolchain, without the Go shim's
        # checksum-db bootstrap (which would fail with GOSUMDB=off).
        gopath = Path(env.get("GOPATH", str(Path.home() / "go")).split(os.pathsep)[0])
        toolchains = list((gopath / "pkg" / "mod" / "golang.org").glob(
            "toolchain@v0.0.1-go1.26.9.*/bin/go"
        ))
        if len(toolchains) != 1:
            raise RuntimeError("cached Go 1.26.9 toolchain required; refusing download")
        env.pop("GOROOT", None)
        env["PATH"] = str(toolchains[0].parent) + os.pathsep + env.get("PATH", "")
        env.update(GOENV="off", GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOWORK="off",
                   TEST_MARIADB_DSN=f"phoenix:{db_password}@tcp(127.0.0.1:{port})/phoenix_ci?parseTime=true&loc=UTC&multiStatements=true")
        started = time.monotonic()
        cmd = [str(toolchains[0]), "test", "-race", "-count=1", "-json", "-timeout", "2400s",
               "-p", "4", "./..."]
        if args.scope == "m8-install-journal":
            cmd = [str(toolchains[0]), "test", "-race", "-count=1", "-json", "-timeout", "300s",
                   "-p", "4", "./internal/core/domain", "./internal/core/services",
                   "./internal/adapters/repository", "-run",
                   "^TestProbeInstall(Journal|Identity|Graph|States)|^TestM8InstallJournal"]
        if args.scope == "m8-install-runner":
            cmd = [str(toolchains[0]), "test", "-race", "-count=1", "-json", "-timeout", "300s",
                   "-p", "4", "./internal/core/domain", "./internal/core/services",
                   "./internal/adapters/repository", "-run",
                   "^TestProbeInstall(Journal|Identity|Graph|States)|^TestInstallRunner|^TestM8Install(Journal|Runner)"]
        if args.scope == "m8-stage-authorization":
            cmd = [str(toolchains[0]), "test", "-race", "-count=1", "-json", "-timeout", "300s",
                   "-p", "4", "./internal/core/domain", "./internal/core/services",
                   "./internal/adapters/repository", "./internal/adapters/provisioning", "-run",
                   STAGE_AUTHORIZATION_RUN]
        results = collections.Counter()
        mariadb = collections.Counter()
        packages = set()
        passed = set()
        failed = []
        skipped = []
        malformed = 0
        with subprocess.Popen(cmd, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL) as proc:
            for line in proc.stdout:
                if event_log:
                    event_log.write(line)
                try:
                    event = json.loads(line, object_pairs_hook=unique_event_object)
                except ValueError:
                    malformed += 1
                    continue
                if (not isinstance(event, dict) or not isinstance(event.get("Action"), str)
                        or not isinstance(event.get("Package"), str)
                        or not isinstance(event.get("Test", ""), str)):
                    malformed += 1
                    continue
                if strict_events:
                    if not line.endswith("\n"):
                        strict_events.refuse("unterminated event line")
                    strict_events.observe(event)
                action = event.get("Action")
                test = event.get("Test", "")
                if test and action in ("pass", "fail", "skip"):
                    results[action] += 1
                    if "/mariadb" in test:
                        mariadb[action] += 1
                    if action == "pass":
                        passed.add(test)
                    if action == "fail":
                        failed.append(test)
                    if action == "skip":
                        skipped.append(test)
                if not test and action == "pass":
                    packages.add(event.get("Package", ""))
            code = proc.wait()
        lifecycle_errors, missing_lifecycles = strict_events.finish() if strict_events else ([], [])
        summary = {
            "scope": args.scope,
            "result": "passed" if code == 0 else "failed", "go_exit": code,
            "elapsed_seconds": round(time.monotonic() - started, 1),
            "passed_packages": len(packages), "named_passes": results["pass"],
            "named_failures": results["fail"], "named_skips": results["skip"],
            "mariadb_named_passes": mariadb["pass"],
            "mariadb_named_skips": mariadb["skip"],
            "skipped_tests": sorted(skipped), "failed_tests": sorted(failed)[:12],
            "missing_required": sorted(required - passed),
            "unrecognized_skips": sorted(set(skipped) - allowed_skips),
            "malformed_json_lines": malformed,
            "lifecycle_errors": lifecycle_errors,
            "missing_required_lifecycles": missing_lifecycles,
            "database": f"fresh disposable {args.mariadb_image} on random localhost port",
            "full_section_13_matrix": False,
        }
        if (code or failed or malformed or lifecycle_errors or missing_lifecycles
                or mariadb["pass"] < minimum_engine_passes or mariadb["skip"]
                or required - passed or set(skipped) - allowed_skips):
            summary["result"] = "failed"
    finally:
        if event_log:
            event_log.close()
        if args.scope == "m8-stage-authorization":
            # Inspect only public identity fields, never the generated credentials.
            identity = subprocess.run(
                ["docker", "container", "inspect", "--format",
                 '{{.Id}} {{index .Config.Labels "' + STAGE_OWNER_LABEL + '"}}', name],
                text=True, capture_output=True, check=False)
            if identity.returncode == 0:
                fields = identity.stdout.strip().split()
                if (len(fields) != 2 or len(fields[0]) != 64
                        or any(c not in "0123456789abcdef" for c in fields[0])
                        or fields[1] != run_id or (container_id and fields[0] != container_id)):
                    raise RuntimeError("disposable identity mismatch; refusing removal")
                docker("rm", "-f", "-v", fields[0])
            elif container_id:
                raise RuntimeError("owned disposable cleanup could not verify identity")
        else:
            exists = subprocess.run(["docker", "container", "inspect", name],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                    check=False)
            if exists.returncode == 0:
                docker("rm", "-f", "-v", name)
    if summary is not None:
        print(json.dumps(summary, indent=2))
        if summary["result"] != "passed":
            raise SystemExit(1)


if __name__ == "__main__":
    main()
