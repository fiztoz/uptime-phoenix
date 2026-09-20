"""Run two real sharded apps against a fresh, disposable local MariaDB.

Requires --app-binary and DB_DSN pointing at a database ending in _smoke on
127.0.0.1. Creates test records; never use an existing application database.
All monitored targets and notification recipients are local to this process.
Processes stop on exit; the database and logs remain available for inspection.
"""

import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app-binary", required=True, type=Path)
    parser.add_argument("--port", type=int, default=38766)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--applied-outbox", action="store_true", help="Enable protected local configuration and the durable delivery runtime")
    args = parser.parse_args()
    dsn = os.environ.get("DB_DSN", "")
    if not re.fullmatch(r"[^@]+@tcp\(127\.0\.0\.1:\d+\)/[a-zA-Z0-9_]+_smoke\?.+", dsn):
        parser.error("DB_DSN must name a local disposable database ending in _smoke")
    output = args.output or Path(tempfile.mkdtemp(prefix="phoenix-mr-smoke-"))
    output.mkdir(parents=True, exist_ok=True)
    lock = threading.Lock()
    target_status = {"first": 200, "second": 200}
    requests = {"first": 0, "second": 0}
    hooks = {"direct-first": [], "direct-second": [], "escalation": [], "later": []}

    class TargetAndSink(BaseHTTPRequestHandler):
        def do_GET(self):
            key = self.path.removeprefix("/target/")
            with lock:
                status = target_status.get(key, 404)
                if key in requests:
                    requests[key] += 1
            self.send_response(status)
            self.end_headers()
            self.wfile.write(b"controlled smoke target")

        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            with lock:
                hooks[self.path.removeprefix("/hook/")].append(payload)
            self.send_response(200)
            self.end_headers()

        def log_message(self, *_):
            pass

    sink = ThreadingHTTPServer(("127.0.0.1", args.port + 1), TargetAndSink)
    threading.Thread(target=sink.serve_forever, daemon=True).start()
    sink_url = f"http://127.0.0.1:{args.port + 1}"
    password = secrets.token_urlsafe(24)
    env = dict(os.environ, DB_ENGINE="mariadb", DB_DSN=dsn, HOST="127.0.0.1",
               MODE="all", BOOTSTRAP_USERNAME="smoke-admin", BOOTSTRAP_PASSWORD=password,
               JWT_SECRET=secrets.token_urlsafe(40), REDIS_URL="", OIDC_ISSUER="",
               OTEL_EXPORTER_OTLP_ENDPOINT="", PUBLIC_URL="", PRODUCTION="false",
               SHARD_BATCH_SIZE="1", SHARD_POLL_EVERY="1", SHARD_LEASE_TTL="30",
               ESCALATION_POLL_SECONDS="1", HEARTBEAT_RETENTION_DAYS="0")
    if args.applied_outbox:
        key_file = output / "probe-secret.key"
        with key_file.open("xb") as key:
            key.write(secrets.token_bytes(32))
        key_file.chmod(0o600)
        env.update(PROBE_SECRET_KEY_FILE=str(key_file.resolve()),
                   PUBLIC_URL=f"http://127.0.0.1:{args.port}")
    else:
        env.pop("PROBE_SECRET_KEY_FILE", None)
    processes = {}
    logs = []
    passed = []
    token = None

    def api(method, path, body=None, port=None):
        req = urllib.request.Request(f"http://127.0.0.1:{port or args.port}" + path,
                                     method=method, headers={"Content-Type": "application/json"})
        if token:
            req.add_header("Authorization", "Bearer " + token)
        with urllib.request.urlopen(req, json.dumps(body).encode() if body is not None else None,
                                    timeout=5) as response:
            raw = response.read()
            return json.loads(raw) if raw else None

    def wait_for(label, predicate, timeout=20):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                passed.append(label)
                print("PASS", label, flush=True)
                return
            time.sleep(0.2)
        raise AssertionError("timed out: " + label)

    def start(worker, port):
        log = (output / f"{worker}.log").open("ab")
        logs.append(log)
        processes[worker] = subprocess.Popen([str(args.app_binary.resolve())],
                                            env=dict(env, WORKER_ID=worker, PORT=str(port)),
                                            stdout=log, stderr=subprocess.STDOUT)

        def ready():
            if processes[worker].poll() is not None:
                raise AssertionError(f"{worker} exited; inspect {output}")
            try:
                api("GET", "/api/health/ready", port=port)
                return True
            except (urllib.error.URLError, TimeoutError):
                return False
        wait_for(worker + " ready", ready, 40)

    def stop(worker):
        process = processes.pop(worker)
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)

    def hook_count(key):
        with lock:
            return len(hooks[key])

    def beats(monitor):
        return api("GET", f"/api/monitors/{monitor}/heartbeats?limit=100&order=desc")

    def alert(monitor):
        rows = api("GET", f"/api/alerts?monitor_id={monitor}")
        assert len(rows) <= 1, f"duplicate alert lifecycle for monitor {monitor}"
        return rows[0] if rows else {}

    try:
        start("smoke-a", args.port)
        login = api("POST", "/api/auth/login", {"username": "smoke-admin", "password": password})
        token = login.get("token") or login["access_token"]
        assert api("GET", "/api/monitors") == [], "requires a fresh smoke database"
        notifications = {}
        for key in hooks:
            notifications[key] = api("POST", "/api/notifications", {
                "name": key, "type": "webhook", "active": True, "include_ack_url": True,
                "config": {"url": sink_url + "/hook/" + key}})["id"]
        monitors = {}
        for key in target_status:
            monitors[key] = api("POST", "/api/monitors", {
                "name": "colima-" + key, "type": "http", "active": True,
                "interval": 1, "retry_interval": 1, "max_retries": 1,
                "timeout": 2, "resend_interval": 1,
                "config": {"url": sink_url + "/target/" + key}})["id"]
            api("POST", f"/api/notifications/{notifications['direct-' + key]}/monitor/{monitors[key]}")
        policy = api("POST", "/api/escalation-policies", {
            "name": "smoke-ladder", "enabled": True,
            "steps": [{"wait_minutes": 0, "notification_ids": [notifications["escalation"]]},
                      {"wait_minutes": 1, "notification_ids": [notifications["later"]]}]})
        api("PUT", f"/api/monitors/{monitors['first']}/escalation-policy", {"policy_id": policy["id"]})
        # A claims the first row; batch size one leaves the second for B.
        wait_for("worker A schedules first monitor", lambda: bool(beats(monitors["first"])))
        assert beats(monitors["second"]) == [], "worker A exceeded its lease set"
        start("smoke-b", args.port + 2)
        wait_for("both workers persist healthy checks", lambda: all(
            beats(mid) and beats(mid)[0]["status"] == "up" for mid in monitors.values()))
        with lock:
            before = dict(requests)
        time.sleep(5)
        with lock:
            counts = {key: requests[key] - before[key] for key in requests}
        assert all(3 <= n <= 6 for n in counts.values()), f"duplicate/missing scheduled checks: {counts}"
        passed.append("two workers execute only their own leases")
        print("PASS two workers execute only their own leases", counts, flush=True)
        with lock:
            target_status.update(first=503, second=503)
        wait_for("one initial DOWN webhook per monitor", lambda: all(
            hook_count("direct-" + key) == 1 for key in monitors))
        wait_for("first escalation delivered", lambda: hook_count("escalation") == 1)
        for mid in monitors.values():
            assert {"up", "pending", "down"}.issubset({row["status"] for row in beats(mid)})
            assert alert(mid)["status"] == "firing"
        if args.applied_outbox:
            with lock:
                first_message = hooks["direct-first"][0]["message"]
            assert f"http://127.0.0.1:{args.port}/ack/" in first_message, first_message
            passed.append("real bootstrap supplies PublicURL to outbox consumer")
        first_alert = alert(monitors["first"])
        api("POST", f"/api/alerts/{first_alert['id']}/ack")
        wait_for("acknowledgement cancels remaining escalation", lambda: (
            alert(monitors["first"]).get("status") == "acked" and
            alert(monitors["first"]).get("escalation", {}).get("status") == "canceled"))
        previous_ids = {key: beats(mid)[0]["id"] for key, mid in monitors.items()}
        stop("smoke-b")
        stop("smoke-a")
        start("smoke-a", args.port)
        start("smoke-b", args.port + 2)
        wait_for("both monitors record new DOWN checks after restart", lambda: all(
            beats(mid)[0]["id"] > previous_ids[key] and beats(mid)[0]["status"] == "down"
            for key, mid in monitors.items()))
        time.sleep(4)
        assert hook_count("direct-first") == hook_count("direct-second") == 1
        assert hook_count("escalation") == 1 and hook_count("later") == 0
        assert alert(monitors["second"])["status"] == "firing"
        passed.append("durable resend throttle survives process restart")
        print("PASS durable resend throttle survives process restart", flush=True)
        with lock:
            target_status.update(first=200, second=200)
        wait_for("both recoveries resolve their alerts and deliver webhooks", lambda: all(
            alert(mid).get("status") == "resolved" and hook_count("direct-" + key) == 2
            for key, mid in monitors.items()))
        for key, mid in monitors.items():
            with lock:
                payloads = list(hooks["direct-" + key])
            assert [p["status"] for p in payloads] == [0, 1]
            assert all(p["monitor"]["id"] == mid for p in payloads)
        assert hook_count("later") == 0
        report = {"applied_outbox": args.applied_outbox, "passed": passed, "monitor_ids": monitors, "request_counts": counts,
                  "webhook_counts": {key: hook_count(key) for key in hooks}}
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print("Smoke passed. Evidence:", output, flush=True)
    finally:
        for worker in list(processes):
            stop(worker)
        sink.shutdown()
        sink.server_close()
        for log in logs:
            log.close()


if __name__ == "__main__":
    main()
