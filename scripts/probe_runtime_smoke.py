"""Exercise real probe configuration and replay against disposable local MariaDB.

DB_DSN must name a localhost database ending in _smoke. All check targets and
provider recipients are local. Logs, private stores and a secret-free report are
retained under --output. No service is left running after success or failure.
Pass --verify-replay and --mariadb-container to additionally inspect committed
hub mirrors/cursors through the container's MariaDB client. Queries are read-only.
"""

import argparse
from contextlib import closing
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import select
import socket
import socketserver
import sqlite3
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid


class PartitionRelay(socketserver.ThreadingTCPServer):
    """Transparent local TCP fault injector; TLS stays end to end."""

    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, address, destination):
        self.destination = destination
        self.gate = threading.Lock()
        self.blocked = False
        self.connections = set()

        class Handler(socketserver.BaseRequestHandler):
            def handle(inner):
                pair = [inner.request]
                try:
                    with self.gate:
                        if self.blocked:
                            return
                    upstream = socket.create_connection(self.destination, timeout=2)
                    pair.append(upstream)
                    with self.gate:
                        if self.blocked:
                            return
                        self.connections.update(pair)
                    for conn in pair:
                        conn.settimeout(2)
                    while True:
                        ready, _, _ = select.select(pair, [], [], 1)
                        for source in ready:
                            data = source.recv(65536)
                            if not data:
                                return
                            pair[1 if source is pair[0] else 0].sendall(data)
                except OSError:
                    pass
                finally:
                    with self.gate:
                        self.connections.difference_update(pair)
                    for conn in pair:
                        conn.close()

        super().__init__(address, Handler)

    def partition(self, blocked):
        with self.gate:
            self.blocked = blocked
            if blocked:
                for conn in self.connections:
                    try:
                        conn.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("app", "probe", "admin"):
        parser.add_argument(f"--{name}-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--port", type=int, default=38920)
    parser.add_argument("--verify-replay", action="store_true")
    parser.add_argument("--verify-shutdown", action="store_true", help="verify bounded compiled termination and exact unacknowledged bytes across restart")
    parser.add_argument("--verify-history", action="store_true", help="verify the production history worker after replay and restart")
    parser.add_argument("--verify-watchdog", action="store_true", help="verify real both-side watchdog paging across a network partition and restart")
    parser.add_argument("--verify-command", action="store_true", help="exercise queued original-incident ACK across a real link partition and restart")
    parser.add_argument("--verify-partition", action="store_true", help="also fail/recover an independent target inside the command partition and verify exact replay and current-state priority")
    parser.add_argument("--verify-credential-rotation", action="store_true", help="verify durable queued credential rotation across hub and edge restart")
    parser.add_argument("--verify-certificate-rotation", action="store_true", help="verify durable TLS certificate rotation across hub and edge restart")
    parser.add_argument("--verify-stream-reset", action="store_true", help="verify explicit source archive and hub reset through compiled CLIs and real admission")
    parser.add_argument("--command-partition-seconds", type=int, default=15)
    parser.add_argument("--mariadb-container")
    args = parser.parse_args()
    dsn = os.environ.get("DB_DSN", "")
    if not re.fullmatch(r"[^@]+@tcp\(127\.0\.0\.1:\d+\)/[a-zA-Z0-9_]+_smoke\?.+", dsn):
        parser.error("DB_DSN must name a disposable localhost database ending in _smoke")
    if args.verify_credential_rotation and not args.verify_replay:
        parser.error("--verify-credential-rotation requires --verify-replay")
    if args.verify_certificate_rotation and (not args.verify_replay or args.verify_credential_rotation):
        parser.error("--verify-certificate-rotation requires --verify-replay and a separate run from credential rotation")
    if args.verify_stream_reset and (not args.verify_replay or args.verify_certificate_rotation or args.verify_credential_rotation):
        parser.error("--verify-stream-reset requires --verify-replay and a separate run from rotations")
    if args.verify_history and not args.verify_replay:
        parser.error("--verify-history requires --verify-replay")
    if args.verify_shutdown and not args.verify_replay:
        parser.error("--verify-shutdown requires --verify-replay")
    if args.verify_watchdog and not args.verify_replay:
        parser.error("--verify-watchdog requires --verify-replay")
    if args.verify_command and (not args.verify_replay or not 1 <= args.command_partition_seconds <= 3600):
        parser.error("--verify-command requires --verify-replay and a partition duration between 1 and 3600 seconds")
    if args.verify_partition and (not args.verify_command or args.command_partition_seconds < 30):
        parser.error("--verify-partition requires --verify-command and at least 30 seconds; milestone acceptance requires 900 seconds")
    if args.verify_replay and not args.mariadb_container:
        parser.error("--verify-replay requires --mariadb-container for independent hub evidence")
    output = args.output.resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    edge_dir = output / "edge"
    key = output / "hub.key"
    key.write_bytes(secrets.token_bytes(32))
    key.chmod(0o600)
    policy = output / "endpoints.json"
    policy.write_text('{"allowed_cidrs":["127.0.0.1/32"]}\n')
    password = secrets.token_urlsafe(24)
    env = dict(os.environ, DB_ENGINE="mariadb", DB_DSN=dsn, HOST="127.0.0.1",
               MODE="api", BOOTSTRAP_USERNAME="edge-smoke", BOOTSTRAP_PASSWORD=password,
               JWT_SECRET=secrets.token_urlsafe(40), REDIS_URL="", OIDC_ISSUER="",
               OTEL_EXPORTER_OTLP_ENDPOINT="", PUBLIC_URL="", PRODUCTION="false",
               PROBES_ENABLED="false", PROBE_SECRET_KEY_FILE=str(key),
               PROBE_ENDPOINT_POLICY_FILE=str(policy), PROBE_HUB_ID="",
               HEARTBEAT_RETENTION_DAYS="0", SHARD_POLL_EVERY="1")
    edge_env = dict(env, PROBE_SECRET_KEY_FILE="")
    processes, logs, passed = {}, [], []
    shutdown_evidence = []
    target_status, partition_target_status, provider_status = 200, 200, 200
    request_count, hooks, provider_attempts = 0, [], []
    lock = threading.Lock()
    token = None

    class TargetAndSink(BaseHTTPRequestHandler):
        def do_GET(self):
            nonlocal request_count
            with lock:
                request_count += 1
                status = partition_target_status if self.path == "/partition-target" else target_status
            self.send_response(status)
            self.end_headers()
            self.wfile.write(b"controlled M2 target")

        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            with lock:
                status = provider_status
                provider_attempts.append(status)
                if status == 200:
                    hooks.append(payload)
            self.send_response(status)
            self.end_headers()

        def log_message(self, *_):
            pass

    sink = ThreadingHTTPServer(("127.0.0.1", args.port + 1), TargetAndSink)
    threading.Thread(target=sink.serve_forever, daemon=True).start()
    sink_url = f"http://127.0.0.1:{args.port + 1}"
    relay = None
    if args.verify_watchdog or args.verify_command or args.verify_credential_rotation or args.verify_certificate_rotation or args.verify_stream_reset:
        relay = PartitionRelay(("127.0.0.1", args.port + 4), ("127.0.0.1", args.port + 3))
        threading.Thread(target=relay.serve_forever, daemon=True).start()

    def mark(label):
        passed.append(label)
        print("PASS", label, flush=True)

    def wait_for(label, predicate, timeout=40):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for name, process in processes.items():
                if process.poll() is not None:
                    raise AssertionError(f"{name} exited; inspect its log")
            if predicate():
                mark(label)
                return
            time.sleep(0.1)
        raise AssertionError("timed out: " + label)

    def command(binary, arguments, command_env=env):
        result = subprocess.run([str(binary.resolve()), *arguments], env=command_env,
                                capture_output=True, timeout=30)
        if result.returncode:
            raise AssertionError(f"{binary.name} {arguments[0]} failed: " + result.stderr.decode())
        return json.loads(result.stdout)

    def admin(*arguments):
        return command(args.admin_binary, list(arguments))

    def start(name, binary, arguments, process_env):
        log = (output / f"{name}.log").open("ab")
        logs.append(log)
        processes[name] = subprocess.Popen([str(binary.resolve()), *arguments], env=process_env,
                                           stdout=log, stderr=subprocess.STDOUT)

    def stop(name):
        process = processes.pop(name)
        inspect_shutdown = args.verify_shutdown and name == "edge"
        if inspect_shutdown:
            prior_progress = edge_progress()
            prior_rows = edge_rows("SELECT seq,hex(payload) AS payload FROM edge_telemetry_outbox ORDER BY seq")
            log_offset = (output / "edge.log").stat().st_size
        started = time.monotonic()
        process.terminate()
        try:
            process.wait(timeout=25 if name == "edge" else 15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise AssertionError(name + " did not shut down gracefully")
        assert process.returncode == 0, f"{name} failed shutdown"
        if inspect_shutdown:
            elapsed = time.monotonic() - started
            progress = edge_progress()
            assert progress["stream_id"] == prior_progress["stream_id"]
            assert progress["config_revision"] >= prior_progress["config_revision"]
            assert progress["last_created_seq"] >= prior_progress["last_created_seq"]
            assert progress["committed_seq"] >= prior_progress["committed_seq"]
            remaining = {row["seq"]: row["payload"] for row in edge_rows("SELECT seq,hex(payload) AS payload FROM edge_telemetry_outbox")}
            for row in prior_rows:
                if row["seq"] > progress["committed_seq"]:
                    assert remaining.get(row["seq"]) == row["payload"], "shutdown changed unacknowledged bytes"
            entries = []
            for line in (output / "edge.log").read_bytes()[log_offset:].decode().splitlines():
                try:
                    entries.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
            drains = [entry for entry in entries if entry.get("msg", "").startswith("probe shutdown ")]
            assert len(drains) == 1, "missing or duplicate shutdown outcome"
            drain = drains[0]
            assert drain["target_seq"] == progress["last_created_seq"]
            assert drain["committed_seq"] <= progress["committed_seq"]
            flushed = drain["msg"] == "probe shutdown telemetry flushed"
            if flushed:
                assert progress["committed_seq"] == progress["last_created_seq"] and not remaining
            shutdown_evidence.append({"seconds": elapsed, "flushed": flushed,
                                      "target_seq": drain["target_seq"], "committed_seq": progress["committed_seq"],
                                      "retained_rows": len(remaining), "exact_unacknowledged_bytes_preserved": True})

    def api(method, path, body=None):
        req = urllib.request.Request(f"http://127.0.0.1:{args.port}" + path, method=method,
                                     headers={"Content-Type": "application/json"})
        if token:
            req.add_header("Authorization", "Bearer " + token)
        with urllib.request.urlopen(req, json.dumps(body).encode() if body is not None else None,
                                    timeout=3) as response:
            data = response.read()
            return json.loads(data) if data else None

    def api_ready():
        try:
            return api("GET", "/api/health/ready")["status"] == "ready"
        except (urllib.error.URLError, TimeoutError):
            return False

    def edge_rows(query):
        with closing(sqlite3.connect(f"file:{edge_dir}/edge.db?mode=ro", uri=True, timeout=2)) as db:
            db.row_factory = sqlite3.Row
            return [dict(row) for row in db.execute(query)]

    def edge_progress():
        return edge_rows("SELECT probe_id, stream_id, last_created_seq, committed_seq, config_revision, connection_generation FROM edge_identity")[0]

    def hub_query(query):
        # The harness permits only its disposable localhost _smoke database.
        # Keep credentials out of argv, captured diagnostics and the report.
        credentials, destination = dsn.split("@tcp(", 1)
        username, _, db_password = credentials.partition(":")
        database = destination.split(")/", 1)[1].split("?", 1)[0]
        result = subprocess.run(
            ["docker", "exec", "-i", "--env", "MYSQL_PWD", args.mariadb_container,
             "mariadb", "--user", username, "--database", database,
             "--batch", "--raw", "--skip-column-names"],
            input=query + ";\n", env=dict(os.environ, MYSQL_PWD=db_password),
            text=True, capture_output=True, timeout=5)
        if result.returncode:
            raise AssertionError("read-only hub evidence query failed")
        return [json.loads(line) for line in result.stdout.splitlines() if line]

    def hub_cursor():
        rows = hub_query("SELECT JSON_OBJECT('cursor', committed_seq) FROM probe_streams "
                         f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{identity['stream_id']}'")
        return rows[0]["cursor"] if rows else -1

    def edge_http(path):
        # Test client verifies the exact locally initialized certificate pin
        # before sending any HTTP bytes. No management token is used here.
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        context.minimum_version = ssl.TLSVersion.TLSv1_3
        conn = http.client.HTTPSConnection("127.0.0.1", args.port + 3, context=context, timeout=2)
        try:
            conn.connect()
            assert hashlib.sha256(conn.sock.getpeercert(binary_form=True)).hexdigest() == identity["certificate_fingerprint"]
            conn.request("GET", path)
            response = conn.getresponse()
            response.read()
            return response.status
        except (ConnectionRefusedError, TimeoutError):
            return 0
        finally:
            conn.close()

    edge_args = ["run", "--data-dir", str(edge_dir), "--listen", f"127.0.0.1:{args.port + 3}"]
    try:
        identity = command(args.probe_binary, ["init", "--data-dir", str(edge_dir)], edge_env)
        enrollment_file = output / "enrollment.token"
        enrollment_file.write_text(identity.pop("enrollment_token"))
        enrollment_file.chmod(0o600)
        start("edge", args.probe_binary, edge_args, edge_env)
        wait_for("TLS-only edge starts without hub", lambda: edge_http("/healthz") == 200)
        assert edge_http("/readyz") == 503
        for path in ("/api/auth/register", "/api/monitors", "/", "/api/users"):
            assert edge_http(path) == 404, path
        duplicate = subprocess.run([str(args.probe_binary.resolve()), *edge_args], env=edge_env,
                                   capture_output=True, timeout=5)
        assert duplicate.returncode != 0
        mark("duplicate data-directory owner rejected; hub routes absent")
        start("api", args.app_binary, [], dict(env, PORT=str(args.port)))
        wait_for("hub API ready", api_ready)
        login = api("POST", "/api/auth/login", {"username": "edge-smoke", "password": password})
        token = login.get("token") or login["access_token"]
        assert api("GET", "/api/monitors") == [], "requires a fresh disposable database"
        notification = api("POST", "/api/notifications", {"name": "edge-hook", "type": "webhook", "active": True,
                                                           "config": {"url": sink_url + "/hook"}})["id"]
        monitor = api("POST", "/api/monitors", {"name": "edge-target", "type": "http", "active": True,
                      "interval": 1, "retry_interval": 1, "max_retries": 1, "timeout": 2,
                      "resend_interval": 0, "config": {"url": sink_url + "/target"}})["id"]
        registered = admin("register", "--probe-id", identity["probe_id"], "--stream-id", identity["stream_id"],
                           "--key", "edge-smoke", "--name", "Edge smoke", "--endpoint",
                           f"wss://127.0.0.1:{args.port + (4 if relay else 3)}/ws/probe/v1", "--fingerprint", identity["certificate_fingerprint"], "--location", "Local verification")
        assignment = admin("assign", "--monitor-id", str(monitor), "--expected-revision", "1", "--probes", identity["probe_id"])
        generation = next(member["generation"] for member in assignment["assignments"] if member["probe_id"] == identity["probe_id"])
        api("POST", f"/api/notifications/{notification}/monitor/{monitor}", {"include_target": False})
        assert int(generation) > 0
        assert admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "0"
        for worker in ("worker-a", "worker-b"):
            start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
        if args.verify_replay:
            wait_for("background worker owns the not-yet-enrolled runtime", lambda: bool(hub_query(
                "SELECT JSON_OBJECT('epoch', epoch) FROM probe_runtime_owners "
                f"WHERE probe_id='{identity['probe_id']}' AND owner_id <> ''")))
        admin("enroll", "--probe-id", identity["probe_id"], "--token-file", str(enrollment_file))
        mark("operator enrollment succeeds with background connectors already running")
        wait_for("real hub connector activates accepted config", lambda: edge_progress()["config_revision"] == 1)
        wait_for("edge records healthy checks", lambda: edge_rows("SELECT status FROM edge_regional_state") == [{"status": 1}])
        assert admin("status", "--probe-id", identity["probe_id"])["state"] == "active"
        wait_for("hub durably records exact applied receipt", lambda: admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "1")
        api("PUT", f"/api/monitors/{monitor}", {"name": "edge-target-edited"})
        wait_for("saved API edit automatically reaches edge", lambda: edge_progress()["config_revision"] == 2)
        wait_for("new desired revision has durable applied receipt", lambda: admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "2")
        assert admin("status", "--probe-id", identity["probe_id"])["sync_pending"] is False
        if args.verify_replay:
            wait_for("live telemetry reaches a durable hub and edge cursor", lambda: edge_progress()["committed_seq"] > 0 and hub_cursor() >= edge_progress()["committed_seq"])
        runtime_evidence = None
        if args.verify_replay:
            def runtime_owner():
                return hub_query("SELECT JSON_OBJECT('epoch', epoch, 'owner_id', owner_id) FROM probe_runtime_owners "
                                 f"WHERE probe_id='{identity['probe_id']}'")[0]

            owner_before = runtime_owner()
            generation_before = edge_progress()["connection_generation"]
            stop("edge")
            wait_for("closed edge session releases its connection fence", lambda: hub_query(
                "SELECT JSON_OBJECT('connected', connected) FROM probe_sessions "
                f"WHERE probe_id='{identity['probe_id']}'") == [{"connected": 0}])
            time.sleep(3)  # Another worker must not steal ownership during backoff.
            assert runtime_owner() == owner_before
            start("edge", args.probe_binary, edge_args, edge_env)
            wait_for("edge reconnect advances session within the same runtime owner", lambda:
                     edge_progress()["connection_generation"] > generation_before and edge_progress()["committed_seq"] > 0)
            assert runtime_owner() == owner_before
            runtime_evidence = {"epoch": owner_before["epoch"], "generation_before": generation_before,
                                "generation_after": edge_progress()["connection_generation"], "stable_across_backoff": True}
            mark("two hub workers preserve runtime ownership across an edge restart")
        first = edge_progress()
        time.sleep(16)  # Cross a real lease renewal with two competing workers.
        assert edge_progress()["connection_generation"] == first["connection_generation"]
        assert not hooks
        mark("two hub workers retain one fenced management session")
        for name in ("worker-a", "worker-b", "api"):
            stop(name)
        assert edge_http("/readyz") == 200
        with lock:
            target_status, provider_status = 503, 503
        wait_for("offline DOWN and provider retry persist atomically", lambda: bool(edge_rows("SELECT delivery_id FROM edge_delivery_outbox WHERE status='retrying' AND attempt>0")))
        incident = edge_rows("SELECT source_alert_id FROM edge_alerts")[0]["source_alert_id"]
        queued = edge_rows("SELECT delivery_id FROM edge_delivery_outbox")[0]["delivery_id"]
        before = edge_progress()
        stop("edge")
        inspected = command(args.probe_binary, ["inspect", "--data-dir", str(edge_dir)], edge_env)
        assert inspected["stream_id"] == identity["stream_id"] and int(inspected["last_created_seq"]) >= before["last_created_seq"]
        with lock:
            provider_status = 200
        start("edge", args.probe_binary, edge_args, edge_env)
        wait_for("pending provider intent resumes after offline process restart", lambda: len(hooks) == 1)
        assert edge_rows("SELECT delivery_id, status FROM edge_delivery_outbox") == [{"delivery_id": queued, "status": "sent"}]
        assert edge_rows("SELECT source_alert_id, status FROM edge_alerts") == [{"source_alert_id": incident, "status": "firing"}]
        assert edge_progress()["stream_id"] == identity["stream_id"]
        with lock:
            target_status = 200
        wait_for("offline UP resolves the same incident and sends recovery", lambda: len(hooks) == 2)
        assert edge_rows("SELECT source_alert_id, status FROM edge_alerts") == [{"source_alert_id": incident, "status": "resolved"}]
        assert [h["status"] for h in hooks] == [0, 1]
        assert edge_http("/readyz") == 200
        offline = edge_progress()
        backlog = edge_rows("SELECT seq, kind FROM edge_telemetry_outbox ORDER BY seq")
        if args.verify_replay:
            assert offline["last_created_seq"] > offline["committed_seq"]
            assert {row["kind"] for row in backlog} == {"observation", "alert.transition", "delivery.result"}
            mark("offline mixed telemetry survives restart and provider recovery")
        for worker in ("worker-a", "worker-b"):
            start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
        wait_for("reconnection advances fence without resetting source sequence", lambda: edge_progress()["connection_generation"] > first["connection_generation"])
        replay_evidence = None
        if args.verify_replay:
            high_water = offline["last_created_seq"]
            wait_for("reconnected workers commit the full offline telemetry prefix", lambda: edge_progress()["committed_seq"] >= high_water and hub_cursor() >= high_water)
            assert not edge_rows(f"SELECT seq FROM edge_telemetry_outbox WHERE seq <= {high_water}")
            observation_seqs = [row["seq"] for row in backlog if row["kind"] == "observation" and row["seq"] <= high_water]
            sequence_sql = ",".join(map(str, observation_seqs))
            mirrored = hub_query("SELECT JSON_OBJECT('seq', seq) FROM probe_observations "
                                 f"WHERE stream_id='{identity['stream_id']}' AND seq IN ({sequence_sql}) ORDER BY seq")
            assert [row["seq"] for row in mirrored] == observation_seqs
            incidents = hub_query("SELECT JSON_OBJECT('id', source_alert_id, 'status', status) FROM probe_incidents "
                                  f"WHERE probe_id='{identity['probe_id']}'")
            assert incidents == [{"id": incident, "status": "resolved"}]
            outcomes = hub_query("SELECT JSON_OBJECT('id', delivery_id, 'status', status) FROM probe_delivery_events "
                                 f"WHERE probe_id='{identity['probe_id']}' ORDER BY delivery_id")
            source_outcomes = edge_rows("SELECT delivery_id AS id, status FROM edge_delivery_outbox ORDER BY delivery_id")
            assert outcomes == source_outcomes
            send_work = hub_query("SELECT JSON_OBJECT('count', COUNT(*)) FROM probe_delivery_intents "
                                  f"WHERE probe_id='{identity['probe_id']}'")[0]["count"]
            assert send_work == 0
            mark("replay mirrors observations and outcomes without hub provider work")
            # Reopen the actual private store after ACK pruning, then reconnect
            # two workers. This proves the ACK cursor survives process teardown.
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            persisted = command(args.probe_binary, ["inspect", "--data-dir", str(edge_dir)], edge_env)
            assert persisted["stream_id"] == identity["stream_id"]
            assert edge_progress()["committed_seq"] >= high_water
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("cold restart resumes beyond the acknowledged backlog", lambda: edge_progress()["committed_seq"] > high_water)
            replay_evidence = {"offline_high_water": high_water, "offline_backlog_count": len(backlog),
                               "mirrored_observation_sequences": observation_seqs,
                               "mirrored_incidents": incidents, "mirrored_delivery_outcomes": outcomes,
                               "hub_send_intents": send_work, "hub_cursor": hub_cursor(),
                               "edge_cursor": edge_progress()["committed_seq"]}
        history_evidence = None
        if args.verify_history:
            first_bucket = hub_query("SELECT JSON_OBJECT('bucket', FLOOR(UNIX_TIMESTAMP(MIN(observed_at))/60)*60) "
                                     f"FROM probe_observations WHERE probe_id='{identity['probe_id']}' AND monitor_id={monitor}")[0]["bucket"]
            assert first_bucket is not None
            bucket = int(first_bucket)

            def history_ready():
                nonlocal history_evidence
                if time.time() < bucket + 60:
                    return False
                rows = hub_query("SELECT JSON_OBJECT('total_checks', total_checks, 'known_us', up_us+down_us+pending_us+maintenance_us, "
                                 "'unknown_us', unknown_us, 'managed', history_managed) FROM heartbeat_1m "
                                 f"WHERE monitor_id={monitor} AND probe_id='{identity['probe_id']}' AND bucket=FROM_UNIXTIME({bucket})")
                if not rows or not rows[0]["managed"]:
                    return False
                source_count = hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_observations "
                                         f"WHERE monitor_id={monitor} AND probe_id='{identity['probe_id']}' "
                                         f"AND observed_at>=FROM_UNIXTIME({bucket}) AND observed_at<FROM_UNIXTIME({bucket+60})")[0]["count"]
                pending = hub_query("SELECT JSON_OBJECT('count', COUNT(*)) FROM probe_dirty_buckets "
                                    f"WHERE monitor_id={monitor} AND bucket=FROM_UNIXTIME({bucket}) AND resolution IN ('1m','overall')")[0]["count"]
                overall = hub_query("SELECT JSON_OBJECT('coverage_us', COALESCE(SUM(TIMESTAMPDIFF(MICROSECOND, "
                                    f"GREATEST(started_at,FROM_UNIXTIME({bucket})), LEAST(ended_at,FROM_UNIXTIME({bucket+60})))),0)) "
                                    f"FROM monitor_health_history WHERE monitor_id={monitor} "
                                    f"AND started_at<FROM_UNIXTIME({bucket+60}) AND ended_at>FROM_UNIXTIME({bucket})")[0]["coverage_us"]
                if pending or rows[0]["total_checks"] != source_count or overall != 60000000:
                    return False
                assert source_count > 0 and rows[0]["known_us"] > 0
                history_evidence = dict(rows[0], bucket_utc_epoch=bucket, source_observations=source_count,
                                        overall_coverage_us=overall, pending_minute_work=pending)
                return True

            wait_for("production workers recompute closed regional and overall history after restart", history_ready, timeout=100)
        time.sleep(3)
        final = edge_progress()
        assert final["last_created_seq"] > before["last_created_seq"] and final["config_revision"] == 2
        assert len(hooks) == 2
        mark("reconnection preserves stream, accepted config and delivery history")
        watchdog_evidence = None
        if args.verify_watchdog:
            admin("watchdog", "--probe-id", identity["probe_id"], "--expected-revision", "0",
                  "--enabled=true", "--notifications", str(notification), "--lost-after-seconds", "90",
                  "--recover-after-seconds", "30", "--resend-interval", "0")
            wait_for("enabled watchdog config durably applied on both sides", lambda:
                     edge_progress()["config_revision"] == 3 and admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "3")
            wait_for("both watchdogs observe healthy application frames", lambda:
                     edge_rows("SELECT status FROM edge_watchdog_state") == [{"status": "healthy"}] and
                     hub_query("SELECT JSON_OBJECT('status',status) FROM probe_watchdog_state") == [{"status": "healthy"}])
            hook_start = len(hooks)
            relay.partition(True)
            partition_started = time.monotonic()
            wait_for("both live owners send watchdog DOWN during network partition", lambda:
                     len(hooks) == hook_start + 2, timeout=140)
            down_elapsed = time.monotonic() - partition_started
            source = edge_rows("SELECT source_alert_id, status FROM edge_alerts WHERE subject_kind='watchdog'")
            assert len(source) == 1 and source[0]["status"] == "firing"
            edge_watchdog_id = source[0]["source_alert_id"]
            hub_source = hub_query("SELECT JSON_OBJECT('source_alert_id',i.source_alert_id,'status',i.status) "
                                   "FROM probe_incidents i JOIN probe_hub_watchdog_incidents h ON h.source_alert_id=i.source_alert_id")
            assert len(hub_source) == 1 and hub_source[0]["status"] == "firing"
            hub_watchdog_id = hub_source[0]["source_alert_id"]
            assert edge_watchdog_id != hub_watchdog_id
            down = list(hooks[hook_start:])
            assert {h["source_alert_id"] for h in down} == {edge_watchdog_id, hub_watchdog_id}
            assert all(h["status"] == 0 and h["probe"]["id"] == identity["probe_id"] and "monitor" not in h for h in down)
            stop("edge")
            start("edge", args.probe_binary, edge_args, edge_env)
            wait_for("offline edge restart retains the original watchdog", lambda:
                     edge_http("/readyz") == 200 and edge_rows("SELECT source_alert_id,status FROM edge_alerts WHERE subject_kind='watchdog'") == source)
            time.sleep(6)
            assert len(hooks) == hook_start + 2
            mark("watchdog restart does not duplicate the initial notification")
            relay.partition(False)
            restored_at = time.monotonic()
            wait_for("both watchdogs send stable recovery after reconnect", lambda:
                     len(hooks) == hook_start + 4, timeout=100)
            recovery_elapsed = time.monotonic() - restored_at
            assert recovery_elapsed >= 30, "reconnect alone resolved a watchdog"
            watchdog_hooks = list(hooks[hook_start:])
            for source_id in (edge_watchdog_id, hub_watchdog_id):
                assert [h["status"] for h in watchdog_hooks if h["source_alert_id"] == source_id] == [0, 1]
            high_water = edge_progress()["last_created_seq"]
            wait_for("edge watchdog lifecycle and provider outcomes replay durably", lambda:
                     hub_cursor() >= high_water and edge_progress()["committed_seq"] >= high_water)
            mirrored = hub_query("SELECT JSON_OBJECT('id',source_alert_id,'status',status,'version',transition_version) "
                                 f"FROM probe_incidents WHERE source_alert_id='{edge_watchdog_id}'")
            assert mirrored == [{"id": edge_watchdog_id, "status": "resolved", "version": 2}]
            outcomes = hub_query("SELECT JSON_OBJECT('status',status,'count',COUNT(*)) FROM probe_delivery_events "
                                 f"WHERE source_alert_id='{edge_watchdog_id}' GROUP BY status")
            assert outcomes == [{"status": "sent", "count": 2}]
            assert hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_delivery_intents "
                             f"WHERE source_alert_id='{edge_watchdog_id}'") == [{"count": 0}]
            rejected = hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_telemetry_receipts "
                                 f"WHERE probe_id='{identity['probe_id']}' AND rejection_code<>''")
            assert rejected == [{"count": 0}]
            mark("mirrored edge watchdog causes no hub redelivery or rejection")
            watchdog_evidence = {"edge_source_id": edge_watchdog_id, "hub_source_id": hub_watchdog_id,
                                 "down_after_seconds": down_elapsed, "recovery_after_reconnect_seconds": recovery_elapsed,
                                 "edge_mirror": mirrored, "edge_delivery_outcomes": outcomes,
                                 "hook_statuses": [h["status"] for h in watchdog_hooks], "hub_edge_send_intents": 0,
                                 "high_water": high_water}
            final = edge_progress()
        command_evidence = None
        partition_evidence = None
        if args.verify_command:
            if args.verify_partition:
                start("api", args.app_binary, [], dict(env, PORT=str(args.port)))
                wait_for("hub API restarts for partition target setup", api_ready)
                partition_monitor = api("POST", "/api/monitors", {"name": "partition-target", "type": "http", "active": True,
                    "interval": 1, "retry_interval": 1, "max_retries": 1, "timeout": 2,
                    "resend_interval": 0, "config": {"url": sink_url + "/partition-target"}})["id"]
                admin("assign", "--monitor-id", str(partition_monitor), "--expected-revision", "1", "--probes", identity["probe_id"])
                api("POST", f"/api/notifications/{notification}/monitor/{partition_monitor}", {"include_target": False})
                wait_for("independent partition target is active on the edge", lambda: bool(edge_rows(
                    f"SELECT seq FROM edge_regional_state WHERE monitor_id={partition_monitor} AND status=1")))
                stop("api")
            with lock:
                target_status = 503
            wait_for("new regional incident fires before ACK partition", lambda: bool(edge_rows(
                "SELECT source_alert_id FROM edge_alerts WHERE subject_kind='availability' AND status='firing'")))
            original_id = edge_rows("SELECT source_alert_id FROM edge_alerts WHERE subject_kind='availability' AND status='firing'")[0]["source_alert_id"]
            wait_for("original ACK target is mirrored before partition", lambda: hub_query(
                "SELECT JSON_OBJECT('status',status) FROM probe_incidents "
                f"WHERE source_alert_id='{original_id}'") == [{"status": "firing"}])
            relay.partition(True)
            partition_started = time.monotonic()
            command_id = str(uuid.uuid4())
            ack_args = ("ack", "--probe-id", identity["probe_id"], "--command-id", command_id,
                        "--source-alert-id", original_id, "--assignment-generation", generation, "--actor", "Smoke operator")
            receipt = admin(*ack_args)["command"]
            assert receipt["status"] == "pending" and receipt["remote_confirmed"] is False
            assert "may continue" in receipt["pending_message"]
            repeated = admin(*ack_args)["command"]
            assert repeated["created_at"] == receipt["created_at"] and repeated["expires_at"] == receipt["expires_at"]
            assert edge_rows(f"SELECT status FROM edge_alerts WHERE source_alert_id='{original_id}'") == [{"status": "firing"}]
            if args.verify_partition:
                with lock:
                    partition_target_status = 503
                wait_for("independent target fails during the link partition", lambda: bool(edge_rows(
                    f"SELECT source_alert_id FROM edge_alerts WHERE monitor_id={partition_monitor} AND status='firing'")))
                partition_incident = edge_rows(f"SELECT source_alert_id FROM edge_alerts WHERE monitor_id={partition_monitor} AND status='firing'")[0]["source_alert_id"]
                retained_before_restart = edge_rows("SELECT seq,hex(payload) AS payload FROM edge_telemetry_outbox ORDER BY seq")
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("offline process restart preserves unacknowledged original incident", lambda:
                     edge_http("/readyz") == 200 and edge_rows(
                         f"SELECT status, transition_version FROM edge_alerts WHERE source_alert_id='{original_id}'") ==
                     [{"status": "firing", "transition_version": 1}])
            if args.verify_partition:
                retained_after_restart = {row["seq"]: row["payload"] for row in edge_rows("SELECT seq,hex(payload) AS payload FROM edge_telemetry_outbox")}
                assert all(retained_after_restart.get(row["seq"]) == row["payload"] for row in retained_before_restart)
                with lock:
                    partition_target_status = 200
                wait_for("independent target recovers before the partition ends", lambda: edge_rows(
                    f"SELECT status FROM edge_alerts WHERE source_alert_id='{partition_incident}'") == [{"status": "resolved"}])
            while time.monotonic() - partition_started < args.command_partition_seconds:
                assert admin("command-status", "--probe-id", identity["probe_id"], "--command-id", command_id)["command"]["remote_confirmed"] is False
                time.sleep(max(0.01, min(5, args.command_partition_seconds - (time.monotonic() - partition_started))))
            partition_elapsed = time.monotonic() - partition_started
            if args.verify_partition:
                retained = edge_rows("SELECT seq,kind,observed_at,hex(payload) AS payload FROM edge_telemetry_outbox ORDER BY seq")
                prefix_end = retained[-1]["seq"]
                current_seq = edge_rows(f"SELECT seq FROM edge_regional_state WHERE monitor_id={partition_monitor} AND status=1")[0]["seq"]
                assert partition_elapsed >= args.command_partition_seconds
            relay.partition(False)
            if args.verify_partition:
                first_state = []

                def capture_first_state():
                    rows = hub_query("SELECT JSON_OBJECT('watermark',r.last_created_seq,'applied_us',CAST(UNIX_TIMESTAMP(r.applied_at)*1000000 AS SIGNED),"
                        "'seq',s.seq,'status',s.status) FROM probe_state_receipts r JOIN monitor_probe_state s ON s.probe_id=r.probe_id AND s.stream_id=r.stream_id "
                        f"WHERE r.probe_id='{identity['probe_id']}' AND r.stream_id='{identity['stream_id']}' AND s.monitor_id={partition_monitor} AND r.last_created_seq>={prefix_end}")
                    if rows:
                        first_state.extend(rows)
                        return True
                    return False

                wait_for("fresh UP state arrives while partition history replays", capture_first_state, timeout=100)
                assert len(first_state) == 1 and first_state[0]["status"] == 1 and first_state[0]["seq"] >= current_seq
            wait_for("pending ACK receives durable source confirmation after reconnect", lambda:
                     admin("command-status", "--probe-id", identity["probe_id"], "--command-id", command_id)["command"]["remote_confirmed"], timeout=100)
            confirmed = admin("command-status", "--probe-id", identity["probe_id"], "--command-id", command_id)["command"]
            assert confirmed["status"] == "applied"
            assert edge_rows(f"SELECT status, transition_version, ack_command_id FROM edge_alerts WHERE source_alert_id='{original_id}'") == [
                {"status": "acked", "transition_version": 2, "ack_command_id": command_id}]
            assert edge_rows(f"SELECT COUNT(*) AS count FROM edge_applied_commands WHERE command_id='{command_id}'") == [{"count": 1}]
            wait_for("ACK telemetry is correlated with the issued command", lambda: hub_query(
                "SELECT JSON_OBJECT('status',status,'version',transition_version,'command',ack_command_id) FROM probe_incidents "
                f"WHERE source_alert_id='{original_id}'") == [{"status": "acked", "version": 2, "command": command_id}])
            with lock:
                target_status = 200
            wait_for("ACKed source recovers while preserving original actor and command", lambda: edge_rows(
                f"SELECT status, transition_version, ack_command_id FROM edge_alerts WHERE source_alert_id='{original_id}'") == [
                {"status": "resolved", "transition_version": 3, "ack_command_id": command_id}])
            with lock:
                target_status = 503
            wait_for("later outage gets an independent source incident", lambda: bool(edge_rows(
                f"SELECT source_alert_id FROM edge_alerts WHERE subject_kind='availability' AND status='firing' AND source_alert_id<>'{original_id}'")))
            later_id = edge_rows("SELECT source_alert_id FROM edge_alerts WHERE subject_kind='availability' AND status='firing'")[0]["source_alert_id"]
            assert admin(*ack_args)["command"]["status"] == "applied"
            late_id = str(uuid.uuid4())
            late_args = list(ack_args)
            late_args[late_args.index("--command-id")+1] = late_id
            admin(*late_args)
            wait_for("new ACK of resolved original returns already_resolved", lambda:
                     admin("command-status", "--probe-id", identity["probe_id"], "--command-id", late_id)["command"]["status"] == "already_resolved")
            assert edge_rows(f"SELECT status, ack_command_id FROM edge_alerts WHERE source_alert_id='{later_id}'") == [{"status": "firing", "ack_command_id": None}]
            with lock:
                target_status = 200
            wait_for("later outage recovers independently", lambda: edge_rows(
                f"SELECT status FROM edge_alerts WHERE source_alert_id='{later_id}'") == [{"status": "resolved"}])
            high_water = edge_progress()["last_created_seq"]
            wait_for("all command-era history is durably replayed", lambda: hub_cursor() >= high_water and edge_progress()["committed_seq"] >= high_water)
            assert hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_telemetry_receipts "
                             f"WHERE source_alert_id='{original_id}' AND transition_version=2 AND rejection_code=''") == [{"count": 1}]
            assert hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_telemetry_receipts "
                             f"WHERE probe_id='{identity['probe_id']}' AND rejection_code<>''") == [{"count": 0}]
            assert hub_query("SELECT JSON_OBJECT('count',COUNT(*)) FROM probe_delivery_intents "
                             f"WHERE probe_id='{identity['probe_id']}'") == [{"count": 0}]
            if args.verify_partition:
                seqs = ",".join(str(row["seq"]) for row in retained)
                receipts = hub_query("SELECT JSON_OBJECT('seq',seq,'digest',digest,'kind',kind,'rejection',rejection_code,"
                    "'received_us',CAST(UNIX_TIMESTAMP(received_at)*1000000 AS SIGNED)) FROM probe_telemetry_receipts "
                    f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{identity['stream_id']}' AND seq IN ({seqs}) ORDER BY seq")
                expected = [{"seq": row["seq"], "digest": hashlib.sha256(bytes.fromhex(row["payload"])).hexdigest(), "kind": row["kind"], "rejection": ""} for row in retained]
                assert [{k: row[k] for k in ("seq", "digest", "kind", "rejection")} for row in receipts] == expected
                assert first_state[0]["applied_us"] <= min(row["received_us"] for row in receipts), "backlog overtook initial current state"
                observations = hub_query("SELECT JSON_OBJECT('seq',seq,'observed_us',CAST(UNIX_TIMESTAMP(observed_at)*1000000 AS SIGNED)) FROM probe_observations "
                    f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{identity['stream_id']}' AND seq IN ({seqs}) ORDER BY seq")
                assert observations == [{"seq": row["seq"], "observed_us": row["observed_at"]} for row in retained if row["kind"] == "observation"]
                mirrored = hub_query("SELECT JSON_OBJECT('status',status,'version',transition_version) FROM probe_incidents "
                    f"WHERE source_alert_id='{partition_incident}'")
                assert mirrored == [{"status": "resolved", "version": 2}]
                partition_evidence = {"duration_seconds": partition_elapsed, "milestone_duration_met": partition_elapsed >= 900,
                    "monitor_id": partition_monitor, "source_alert_id": partition_incident, "restart_preserved_rows": len(retained_before_restart),
                    "replayed_rows": len(retained), "observations_with_original_times": len(observations), "prefix_end": prefix_end,
                    "first_current_state": first_state[0], "first_replay_received_us": min(row["received_us"] for row in receipts),
                    "last_replay_received_us": max(row["received_us"] for row in receipts), "hub_send_intents": 0,
                    "receipt_digest_sha256": hashlib.sha256(json.dumps(expected, separators=(",", ":")).encode()).hexdigest()}
                mark("partition DOWN/UP and restart replay exactly once with original times and current state first")
            mark("queued ACK applies once to its original incident and cannot silence a later outage")
            command_evidence = {"command_id": command_id, "original_source_id": original_id, "later_source_id": later_id,
                                "partition_seconds": partition_elapsed, "source_receipt_count": 1, "confirmed": confirmed,
                                "ack_transition_receipt_count": 1, "hub_send_intents": 0, "high_water": high_water}
            final = edge_progress()
        rotation_evidence = None
        if args.verify_credential_rotation:
            relay.partition(True)
            rotation_id = str(uuid.uuid4())
            rotation_args = ("rotate-credential", "--probe-id", identity["probe_id"],
                             "--rotation-id", rotation_id, "--credential-version", "2")
            issued = admin(*rotation_args)["rotation"]
            assert issued["state"] == "preparing" and issued["prepared_at"] is None and issued["activated_at"] is None
            assert admin(*rotation_args)["rotation"] == issued
            assert "token" not in json.dumps(issued).lower() and "protected" not in json.dumps(issued).lower()
            mark("offline rotation persists one nonsecret operation before dispatch")
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("offline credential restart preserves original active identity", lambda:
                     edge_http("/readyz") == 200 and edge_rows("SELECT version AS credential_version FROM edge_credentials WHERE kind='runtime'") == [{"credential_version": 1}])
            assert admin("rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["rotation"] == issued
            relay.partition(False)
            wait_for("credential rotation confirms both source effects after reconnect", lambda:
                     admin("rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["rotation"]["state"] == "active", timeout=120)
            active = admin("rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["rotation"]
            assert active["overlap_expires_at"] == issued["overlap_expires_at"]
            assert active["prepare_command_id"] == issued["prepare_command_id"] and active["activate_command_id"] == issued["activate_command_id"]
            assert active["prepared_at"] and active["activated_at"]
            assert edge_rows("SELECT version AS credential_version FROM edge_credentials WHERE kind='runtime'") == [{"credential_version": 2}]
            assert hub_query("SELECT JSON_OBJECT('version',credential_version) FROM probe_connections "
                             f"WHERE probe_id='{identity['probe_id']}'") == [{"version": 2}]
            for command_id in (active["prepare_command_id"], active["activate_command_id"]):
                assert edge_rows(f"SELECT COUNT(*) AS count FROM edge_applied_commands WHERE command_id='{command_id}'") == [{"count": 1}]
                assert admin("command-status", "--probe-id", identity["probe_id"], "--command-id", command_id)["command"]["remote_confirmed"] is True
            assert admin(*rotation_args)["rotation"] == active
            generation_before = edge_progress()["connection_generation"]
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("cold restart authenticates with the promoted credential", lambda:
                     edge_progress()["connection_generation"] > generation_before and hub_query(
                         "SELECT JSON_OBJECT('connected',connected) FROM probe_sessions "
                         f"WHERE probe_id='{identity['probe_id']}'") == [{"connected": 1}], timeout=90)
            high_water = edge_progress()["last_created_seq"]
            wait_for("credential rotation preserves ordered telemetry progress", lambda:
                     hub_cursor() >= high_water and edge_progress()["committed_seq"] >= high_water)
            final = edge_progress()
            assert final["probe_id"] == identity["probe_id"] and final["stream_id"] == identity["stream_id"]
            rotation_evidence = {"operation": active, "source_receipts_per_command": 1,
                                 "hub_credential_version": 2, "source_credential_version": 2,
                                 "generation_before_restart": generation_before,
                                 "generation_after_restart": final["connection_generation"], "high_water": high_water}
        certificate_evidence = None
        if args.verify_certificate_rotation:
            relay.partition(True)
            rotation_id = str(uuid.uuid4())
            rotation_args = ("rotate-certificate", "--probe-id", identity["probe_id"],
                             "--rotation-id", rotation_id, "--certificate-version", "2", "--valid-for-days", "365")
            issued = admin(*rotation_args)["certificate_rotation"]
            assert issued["state"] == "preparing" and issued["prepared_at"] is None and issued["activated_at"] is None
            assert admin(*rotation_args)["certificate_rotation"] == issued
            assert not any(word in json.dumps(issued).lower() for word in ("token", "protected", "private", "pem"))
            mark("offline certificate rotation persists one nonsecret operation before dispatch")
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("offline certificate restart preserves bootstrap identity", lambda:
                     edge_http("/readyz") == 200 and edge_rows("SELECT active_version FROM edge_certificate_state") == [{"active_version": 1}])
            assert admin("certificate-rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["certificate_rotation"] == issued
            relay.partition(False)
            wait_for("certificate rotation confirms both source effects after reconnect", lambda:
                     admin("certificate-rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["certificate_rotation"]["state"] == "active", timeout=120)
            active = admin("certificate-rotation-status", "--probe-id", identity["probe_id"], "--rotation-id", rotation_id)["certificate_rotation"]
            for field in ("overlap_expires_at", "prepare_command_id", "activate_command_id"):
                assert active[field] == issued[field]
            assert active["prepared_at"] and active["activated_at"] and active["certificate_not_after"]
            assert edge_rows("SELECT active_version FROM edge_certificate_state") == [{"active_version": 2}]
            assert edge_rows("SELECT version FROM edge_credentials WHERE kind='runtime'") == [{"version": 1}]
            assert hub_query("SELECT JSON_OBJECT('certificate_version',certificate_version,'credential_version',credential_version,'fingerprint',fingerprint) FROM probe_connections "
                             f"WHERE probe_id='{identity['probe_id']}'") == [{"certificate_version": 2, "credential_version": 1, "fingerprint": active["fingerprint"]}]
            for command_id in (active["prepare_command_id"], active["activate_command_id"]):
                assert edge_rows(f"SELECT COUNT(*) AS count FROM edge_applied_commands WHERE command_id='{command_id}'") == [{"count": 1}]
                assert admin("command-status", "--probe-id", identity["probe_id"], "--command-id", command_id)["command"]["remote_confirmed"] is True
            assert admin(*rotation_args)["certificate_rotation"] == active
            generation_before = edge_progress()["connection_generation"]
            for name in ("worker-a", "worker-b", "edge"):
                stop(name)
            start("edge", args.probe_binary, edge_args, edge_env)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            wait_for("cold restart authenticates with the promoted certificate", lambda:
                     edge_progress()["connection_generation"] > generation_before and hub_query(
                         "SELECT JSON_OBJECT('connected',connected) FROM probe_sessions "
                         f"WHERE probe_id='{identity['probe_id']}'") == [{"connected": 1}], timeout=90)
            high_water = edge_progress()["last_created_seq"]
            wait_for("certificate rotation preserves ordered telemetry progress", lambda:
                     hub_cursor() >= high_water and edge_progress()["committed_seq"] >= high_water)
            final = edge_progress()
            assert final["probe_id"] == identity["probe_id"] and final["stream_id"] == identity["stream_id"]
            certificate_evidence = {"operation": active, "source_receipts_per_command": 1,
                                    "hub_certificate_version": 2, "source_certificate_version": 2,
                                    "credential_version": 1, "generation_before_restart": generation_before,
                                    "generation_after_restart": final["connection_generation"], "high_water": high_water}
        reset_evidence = None
        if args.verify_stream_reset:
            stop("edge")
            old_stream = identity["stream_id"]
            original = edge_progress()
            anchors = {name: hashlib.sha256((edge_dir / name).read_bytes()).hexdigest()
                       for name in ("identity.json", "tls.pem", "config.key")}
            old_history = hub_query("SELECT JSON_OBJECT('receipts',COUNT(*)) FROM probe_telemetry_receipts "
                                    f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{old_stream}'")[0]
            reset_id, new_stream = str(uuid.uuid4()), str(uuid.uuid4())
            prepare_args = ("prepare-reset", "--probe-id", identity["probe_id"], "--reset-id", reset_id,
                            "--previous-stream-id", old_stream, "--stream-id", new_stream)
            plan = admin(*prepare_args)
            assert admin(*prepare_args) == plan
            status_args = ("reset-status", "--probe-id", identity["probe_id"], "--reset-id", reset_id)
            assert admin(*status_args)["state"] == "prepared"
            assert hub_query("SELECT JSON_OBJECT('owner',owner_id,'lease',lease_until,'connected',connected) FROM probe_sessions "
                             f"WHERE probe_id='{identity['probe_id']}'") == [{"owner": "", "lease": 0, "connected": 0}]
            mark("reset preparation retries preserve plan and revoke live connector authority")
            plan_file = output / "reset-plan.json"
            plan_file.write_text(json.dumps(plan))
            plan_file.chmod(0o600)
            reset_args = ["reset-stream", "--data-dir", str(edge_dir), "--plan-file", str(plan_file)]
            receipt = command(args.probe_binary, reset_args, edge_env)
            assert command(args.probe_binary, reset_args, edge_env) == receipt
            assert receipt["state"] == "source_applied" and receipt["unobserved_coverage"] == "unknown"
            assert int(receipt["source_last_created_seq"]) == original["last_created_seq"]
            archive = edge_dir / "stream-archives" / reset_id / "edge.db"
            assert hashlib.sha256(archive.read_bytes()).hexdigest() == receipt["archive_sha256"]
            with closing(sqlite3.connect(f"file:{archive}?mode=ro&immutable=1", uri=True)) as archived:
                assert archived.execute("SELECT stream_id,last_created_seq FROM edge_identity").fetchone() == (old_stream, original["last_created_seq"])
            assert edge_progress()["stream_id"] == new_stream and edge_progress()["last_created_seq"] == 0
            mark("source reset and lost-output retry preserve a verified archive and start new epoch at zero")
            start("edge", args.probe_binary, edge_args, edge_env)
            wait_for("source-only committed reset restarts before hub activation", lambda: edge_http("/readyz") == 200)
            assert admin(*status_args)["state"] == "prepared"
            stop("edge")
            assert command(args.probe_binary, reset_args, edge_env) == receipt
            receipt_file = output / "reset-source-receipt.json"
            receipt_file.write_text(json.dumps(receipt))
            receipt_file.chmod(0o600)
            activate_args = ("activate-reset", "--probe-id", identity["probe_id"], "--reset-id", reset_id,
                             "--receipt-file", str(receipt_file))
            activated = admin(*activate_args)
            assert activated["state"] == "awaiting_peer" and "confirmed_at" not in activated
            assert admin(*activate_args) == activated
            assert hub_query("SELECT JSON_OBJECT('cursor',committed_seq,'retired',retired_at IS NOT NULL) FROM probe_streams "
                             f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{old_stream}'") == [{"cursor": int(plan["hub_committed_seq"]), "retired": 1}]
            assert hub_query("SELECT JSON_OBJECT('receipts',COUNT(*)) FROM probe_telemetry_receipts "
                             f"WHERE probe_id='{identity['probe_id']}' AND stream_id='{old_stream}'")[0] == old_history
            assert hub_query("SELECT JSON_OBJECT('reason',reason,'stream',stream_id) FROM probe_missing_state "
                             f"WHERE probe_id='{identity['probe_id']}'") == [{"reason": "stream_reset", "stream": new_stream}]
            mark("hub activation retains old history and reports UNKNOWN while peer is stopped")
            for name in ("worker-a", "worker-b"):
                stop(name)
            for worker in ("worker-a", "worker-b"):
                start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
            assert admin(*status_args)["state"] == "awaiting_peer"
            identity["stream_id"] = new_stream
            start("edge", args.probe_binary, edge_args, edge_env)
            wait_for("authenticated new-stream health confirms reset after both-side restart", lambda:
                     admin(*status_args)["state"] == "complete", timeout=90)
            complete = admin(*status_args)
            assert complete["confirmed_at"]
            wait_for("new-stream sequence one replays independently from retained old sequence one", lambda:
                     hub_cursor() >= 1 and edge_progress()["committed_seq"] >= 1)
            assert hub_query("SELECT JSON_OBJECT('count',COUNT(DISTINCT stream_id)) FROM probe_telemetry_receipts "
                             f"WHERE probe_id='{identity['probe_id']}' AND seq=1") == [{"count": 2}]
            assert {name: hashlib.sha256((edge_dir / name).read_bytes()).hexdigest() for name in anchors} == anchors
            assert admin(*prepare_args) == plan
            assert admin(*activate_args) == complete
            final = edge_progress()
            reset_evidence = {"operation": complete, "old_history": old_history,
                              "source_before_reset": original, "bootstrap_files_unchanged": True,
                              "source_only_restart_verified": True, "hub_only_restart_verified": True}
        if args.verify_shutdown:
            assert any(row["flushed"] for row in shutdown_evidence)
            assert any(not row["flushed"] and row["retained_rows"] > 0 for row in shutdown_evidence)
            mark("compiled shutdown flushes healthy prefixes and preserves offline bytes within its bound")
        report = {"passed": passed, "identity": identity, "before_restart": before, "final": final,
                  "shutdown_verified": args.verify_shutdown, "shutdown": shutdown_evidence,
                  "incident": incident, "provider_attempt_statuses": provider_attempts, "successful_webhook_statuses": [h["status"] for h in hooks],
                  "replay_verified": args.verify_replay, "replay": replay_evidence,
                  "history_verified": args.verify_history, "history": history_evidence, "runtime_ownership": runtime_evidence,
                  "watchdog_verified": args.verify_watchdog, "watchdog": watchdog_evidence,
                  "command_verified": args.verify_command, "command": command_evidence,
                  "partition_verified": args.verify_partition, "partition": partition_evidence,
                  "credential_rotation_verified": args.verify_credential_rotation, "credential_rotation": rotation_evidence,
                  "certificate_rotation_verified": args.verify_certificate_rotation, "certificate_rotation": certificate_evidence,
                  "stream_reset_verified": args.verify_stream_reset, "stream_reset": reset_evidence}
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print("Evidence:", output, flush=True)
    finally:
        shutdown_errors = []
        for name in list(processes):
            try:
                stop(name)
            except AssertionError as error:
                shutdown_errors.append(str(error))
        sink.shutdown()
        sink.server_close()
        if relay:
            relay.partition(True)
            relay.shutdown()
            relay.server_close()
        for log in logs:
            log.close()
        if shutdown_errors:
            raise AssertionError("; ".join(shutdown_errors))


if __name__ == "__main__":
    main()
