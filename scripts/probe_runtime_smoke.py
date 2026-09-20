"""Exercise real M2/M3 configuration-sync binaries against a fresh disposable local MariaDB.

DB_DSN must name a localhost database ending in _smoke. All check targets and
provider recipients are local. Logs, private stores and a secret-free report are
retained under --output. No service is left running after success or failure.
"""

import argparse
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("app", "probe", "admin"):
        parser.add_argument(f"--{name}-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--port", type=int, default=38920)
    args = parser.parse_args()
    dsn = os.environ.get("DB_DSN", "")
    if not re.fullmatch(r"[^@]+@tcp\(127\.0\.0\.1:\d+\)/[a-zA-Z0-9_]+_smoke\?.+", dsn):
        parser.error("DB_DSN must name a disposable localhost database ending in _smoke")
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
    target_status, provider_status = 200, 200
    request_count, hooks, provider_attempts = 0, [], []
    lock = threading.Lock()
    token = None

    class TargetAndSink(BaseHTTPRequestHandler):
        def do_GET(self):
            nonlocal request_count
            with lock:
                request_count += 1
                status = target_status
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
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
            raise AssertionError(name + " did not shut down gracefully")
        assert process.returncode == 0, f"{name} failed shutdown"

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
        with sqlite3.connect(f"file:{edge_dir}/edge.db?mode=ro", uri=True, timeout=2) as db:
            db.row_factory = sqlite3.Row
            return [dict(row) for row in db.execute(query)]

    def edge_progress():
        return edge_rows("SELECT probe_id, stream_id, last_created_seq, config_revision, connection_generation FROM edge_identity")[0]

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
                           f"wss://127.0.0.1:{args.port + 3}/ws/probe/v1", "--fingerprint", identity["certificate_fingerprint"])
        admin("enroll", "--probe-id", identity["probe_id"], "--token-file", str(enrollment_file))
        assignment = admin("assign", "--monitor-id", str(monitor), "--expected-revision", "1", "--probes", identity["probe_id"])
        generation = next(member["generation"] for member in assignment["assignments"] if member["probe_id"] == identity["probe_id"])
        api("POST", f"/api/notifications/{notification}/monitor/{monitor}", {"include_target": False})
        assert int(generation) > 0
        assert admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "0"
        for worker in ("worker-a", "worker-b"):
            start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
        wait_for("real hub connector activates accepted config", lambda: edge_progress()["config_revision"] == 1)
        wait_for("edge records healthy checks", lambda: edge_rows("SELECT status FROM edge_regional_state") == [{"status": 1}])
        assert admin("status", "--probe-id", identity["probe_id"])["state"] == "active"
        wait_for("hub durably records exact applied receipt", lambda: admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "1")
        api("PUT", f"/api/monitors/{monitor}", {"name": "edge-target-edited"})
        wait_for("saved API edit automatically reaches edge", lambda: edge_progress()["config_revision"] == 2)
        wait_for("new desired revision has durable applied receipt", lambda: admin("status", "--probe-id", identity["probe_id"])["applied_revision"] == "2")
        assert admin("status", "--probe-id", identity["probe_id"])["sync_pending"] is False
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
        assert inspected["stream_id"] == identity["stream_id"] and int(inspected["last_created_seq"]) == before["last_created_seq"]
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
        for worker in ("worker-a", "worker-b"):
            start(worker, args.app_binary, [], dict(env, MODE="worker", WORKER_ID=worker, PROBES_ENABLED="true"))
        wait_for("reconnection advances fence without resetting source sequence", lambda: edge_progress()["connection_generation"] > first["connection_generation"])
        time.sleep(3)
        final = edge_progress()
        assert final["last_created_seq"] > before["last_created_seq"] and final["config_revision"] == 2
        assert len(hooks) == 2
        mark("reconnection preserves stream, accepted config and delivery history")
        report = {"passed": passed, "identity": identity, "before_restart": before, "final": final,
                  "incident": incident, "provider_attempt_statuses": provider_attempts, "successful_webhook_statuses": [h["status"] for h in hooks]}
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
        for log in logs:
            log.close()
        if shutdown_errors:
            raise AssertionError("; ".join(shutdown_errors))


if __name__ == "__main__":
    main()
