#!/usr/bin/env python3
"""Full backend race gate on a newly created, disposable MariaDB test DB.

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
    "TestEdgeDiskFullCriticalCommit",                 # dedicated Linux tmpfs gate
}
REQUIRED = {
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
}


def docker(*args, env=None):
    result = subprocess.run(["docker", *args], env=env, text=True, capture_output=True, timeout=90, check=False)
    if result.returncode:
        raise RuntimeError(f"docker {args[0]} failed (exit {result.returncode})")
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mariadb-image", choices=(DEFAULT_IMAGE, "mariadb:12.3"),
                        default=DEFAULT_IMAGE, help="cached disposable DB image, never a live instance")
    parser.add_argument("--go-json", type=Path,
                        help="save complete Go JSON events to a NEW private file for scenario evaluation")
    args = parser.parse_args()
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
    name = "phoenix-m6-matrix-" + uuid.uuid4().hex[:10]
    summary = None
    root_password = secrets.token_urlsafe(32)
    try:
        docker("run", "-d", "--name", name, "--memory", "1g", "--cpus", "2",
               "-p", "127.0.0.1::3306", "-e", "MARIADB_ROOT_PASSWORD",
               "-e", "MARIADB_DATABASE=phoenix_ci", "-e", "MARIADB_USER=phoenix",
               "-e", "MARIADB_PASSWORD=phoenix", args.mariadb_image,
               env={**os.environ, "MARIADB_ROOT_PASSWORD": root_password})
        port = int(docker("port", name, "3306/tcp").split(":")[-1])
        if not 1024 < port <= 65535:
            raise RuntimeError("Docker did not bind a random localhost test port")
        for _ in range(90):
            ready = subprocess.run(
                ["docker", "exec", name, "mariadb", "-uphoenix", "-pphoenix",
                 "-N", "-e", "SELECT 1", "phoenix_ci"],
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
        docker("exec", "-e", "MYSQL_PWD", name, "mariadb", "-uroot", "-e",
               "GRANT PROCESS ON *.* TO 'phoenix'@'%'",
               env={**os.environ, "MYSQL_PWD": root_password})
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
            "toolchain@v0.0.1-go1.26.6.*/bin/go"
        ))
        if len(toolchains) != 1:
            raise RuntimeError("cached Go 1.26.6 toolchain required; refusing download")
        env.pop("GOROOT", None)
        env.update(GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off",
                   TEST_MARIADB_DSN=f"phoenix:phoenix@tcp(127.0.0.1:{port})/phoenix_ci?parseTime=true&loc=UTC&multiStatements=true")
        started = time.monotonic()
        cmd = [str(toolchains[0]), "test", "-race", "-count=1", "-json", "-timeout", "2400s",
               "-p", "4", "./..."]
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
                    event = json.loads(line)
                except json.JSONDecodeError:
                    malformed += 1
                    continue
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
        summary = {
            "result": "passed" if code == 0 else "failed", "go_exit": code,
            "elapsed_seconds": round(time.monotonic() - started, 1),
            "passed_packages": len(packages), "named_passes": results["pass"],
            "named_failures": results["fail"], "named_skips": results["skip"],
            "mariadb_named_passes": mariadb["pass"],
            "mariadb_named_skips": mariadb["skip"],
            "skipped_tests": sorted(skipped), "failed_tests": sorted(failed)[:12],
            "missing_required": sorted(REQUIRED - passed),
            "unrecognized_skips": sorted(set(skipped) - SKIPS_ALLOWED),
            "malformed_json_lines": malformed,
            "database": f"fresh disposable {args.mariadb_image} on random localhost port",
            "full_section_13_matrix": False,
        }
        if (code or failed or malformed or mariadb["pass"] < 300 or mariadb["skip"]
                or REQUIRED - passed or set(skipped) - SKIPS_ALLOWED):
            summary["result"] = "failed"
    finally:
        if event_log:
            event_log.close()
        exists = subprocess.run(["docker", "container", "inspect", name],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                check=False)
        if exists.returncode == 0:
            docker("rm", "-f", name)
    if summary is not None:
        print(json.dumps(summary, indent=2))
        if summary["result"] != "passed":
            raise SystemExit(1)


if __name__ == "__main__":
    main()
