"""Offline tests for the bounded load driver against a local fake hub.

The fake hub listens on loopback only and models the endpoints the driver
uses, the per-client ingress token bucket (shared fake clock), and injected
faults. No external network is touched.
"""
import datetime
import io
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bounded_load_driver as driver  # noqa: E402

# Distinctive markers that must never reach stdout or the evidence file.
PASSWORD = "pw-MARKER-7f3a9c"
USERNAME = "user-MARKER-51ab"
TOKEN = "tok-MARKER-c0ffee"
PROBE_ID = "probe-MARKER-0d15ea5e"
TARGET_URL = "http://target-marker.invalid:18090/health-MARKER"
BODY_MARKER = "body-MARKER-deadbeef"
HEADER_MARKER = "hdr-MARKER-feedface"


class FakeClock:
    def __init__(self):
        self.lock = threading.Lock()
        self.now = 1000.0
        self.slept = 0.0

    def monotonic(self):
        with self.lock:
            return self.now

    def sleep(self, seconds):
        if seconds > 0:
            with self.lock:
                self.now += seconds
                self.slept += seconds

    def advance(self, seconds):
        self.sleep(seconds)

    def utc_now(self):
        base = datetime.datetime(2026, 11, 18, tzinfo=datetime.timezone.utc)
        return base + datetime.timedelta(seconds=self.monotonic())


class FakeHub:
    """Minimal hub: login, monitors, assignments, probe snapshot, peer snapshot."""

    def __init__(self, clock, rps=50.0, burst=100, limiter=True, converge_after_polls=2,
                 per_request_seconds=0.0):
        self.clock = clock
        self.rps, self.burst, self.limiter = rps, burst, limiter
        self.tokens, self.last = float(burst), None
        self.converge_after_polls = converge_after_polls
        self.per_request_seconds = per_request_seconds
        self.monitors = {}
        self.create_bodies = []
        self.calls = []  # (method, class)
        self.faults = {}  # (class, nth) -> status
        self.redirect_class = None
        self.polls_since_write = 0
        self.dirty = False
        self.published = (1, 0)  # (revision, applied assignment count)
        self.lock = threading.Lock()
        hub = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                hub.handle(self, "GET")

            def do_POST(self):
                hub.handle(self, "POST")

            def do_PUT(self):
                hub.handle(self, "PUT")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def assigned(self):
        return sum(1 for m in self.monitors.values() if m["probes"])

    @property
    def url(self):
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()

    def classify(self, method, path):
        parts = path.split("?")[0].strip("/").split("/")
        if path == "/api/auth/login":
            return "auth.login"
        if path == "/peer/snapshot":
            return "peer.snapshot"
        if parts[:2] == ["api", "monitors"] and len(parts) == 2:
            return "monitor.create" if method == "POST" else "monitor.list"
        if parts[:2] == ["api", "monitors"] and len(parts) == 4 and parts[3] == "probes":
            return "assignment.read" if method == "GET" else "assignment.replace"
        if parts[:2] == ["api", "probes"] and len(parts) == 3:
            return "probe.snapshot"
        return "unknown"

    def allow(self):
        if not self.limiter:
            return True
        now = self.clock.monotonic()
        if self.last is not None:
            self.tokens = min(self.burst, self.tokens + (now - self.last) * self.rps)
        self.last = now
        if self.tokens >= 1:
            self.tokens -= 1
            return True
        return False

    def reply(self, handler, status, obj, extra_headers=None):
        raw = json.dumps(obj).encode()
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("X-Debug", HEADER_MARKER)
        for k, v in (extra_headers or {}).items():
            handler.send_header(k, v)
        handler.send_header("Content-Length", str(len(raw)))
        handler.end_headers()
        handler.wfile.write(raw)

    def handle(self, handler, method):
        length = int(handler.headers.get("Content-Length") or 0)
        body = json.loads(handler.rfile.read(length)) if length else None
        with self.lock:
            cls = self.classify(method, handler.path)
            self.calls.append((method, cls))
            nth = sum(1 for c in self.calls if c[1] == cls)
            self.clock.advance(self.per_request_seconds)
            if cls == "peer.snapshot":
                status = self.faults.get((cls, nth))
                if status:
                    return self.reply(handler, status, {"error": BODY_MARKER})
                return self.reply(handler, 200, {"ok": True, "note": BODY_MARKER})
            if not self.allow():
                return self.reply(handler, 429, {"error": "rate limit exceeded " + BODY_MARKER})
            status = self.faults.get((cls, nth))
            if status:
                return self.reply(handler, status, {"error": BODY_MARKER})
            if self.redirect_class == cls:
                return self.reply(handler, 302, {}, {"Location": "http://elsewhere.invalid/"})
            auth = handler.headers.get("Authorization")
            if cls != "auth.login" and auth != "Bearer " + TOKEN:
                return self.reply(handler, 401, {"error": "unauthenticated"})
            if cls == "auth.login":
                if body != {"username": USERNAME, "password": PASSWORD}:
                    return self.reply(handler, 401, {"error": "bad credentials"})
                return self.reply(handler, 200, {"token": TOKEN, "user": BODY_MARKER})
            if cls == "monitor.create":
                assert body["timeout"] < body["interval"]
                mid = len(self.monitors) + 1
                self.monitors[mid] = {"revision": 1, "probes": []}
                self.create_bodies.append(body)
                return self.reply(handler, 201, {"id": mid, "name": body["name"], "x": BODY_MARKER})
            if cls == "monitor.list":
                return self.reply(handler, 200, [{"id": i} for i in self.monitors])
            if cls in ("assignment.read", "assignment.replace"):
                mid = int(handler.path.split("/")[3])
                mon = self.monitors.get(mid)
                if mon is None:
                    return self.reply(handler, 404, {"error": "not found"})
                if cls == "assignment.replace":
                    if body["expected_revision"] != str(mon["revision"]) or body["probe_ids"] != [PROBE_ID]:
                        return self.reply(handler, 409, {"error": "stale"})
                    mon["revision"] += 1
                    mon["probes"] = body["probe_ids"]
                    self.dirty, self.polls_since_write = True, 0
                return self.reply(handler, 200, {"revision": str(mon["revision"]), "assignments": []})
            if cls == "probe.snapshot":
                if handler.path != "/api/probes/" + PROBE_ID:
                    return self.reply(handler, 404, {"error": "probe not found"})
                # Publication lags writes: until converge_after_polls snapshot
                # reads after the last write, the old revision is reported as
                # fully applied (desired == applied) with the old count.
                self.polls_since_write += 1
                if self.polls_since_write >= self.converge_after_polls:
                    self.published = (self.published[0] + (1 if self.dirty else 0), self.assigned())
                    self.dirty = False
                rev, count = self.published
                config = {"desired": {"revision": str(rev)},
                          "applied": {"revision": str(rev), "assignment_count": count},
                          "sync_status": "applied"}
                return self.reply(handler, 200, {"desired_config_revision": str(rev), "e": BODY_MARKER,
                                                 "diagnostics": {"config": config}})
            return self.reply(handler, 404, {"error": "unknown"})


class DriverTestCase(unittest.TestCase):
    def setUp(self):
        self.clock = FakeClock()
        self.tmp = tempfile.TemporaryDirectory()
        self.evidence_path = os.path.join(self.tmp.name, "evidence.json")
        self.hubs = []

    def tearDown(self):
        for hub in self.hubs:
            hub.close()
        self.tmp.cleanup()

    def hub(self, **kwargs):
        hub = FakeHub(self.clock, **kwargs)
        self.hubs.append(hub)
        return hub

    def env(self, hub, **extra):
        env = {"LOAD_HUB_URL": hub.url, "BOOTSTRAP_USERNAME": USERNAME, "BOOTSTRAP_PASSWORD": PASSWORD,
               "LOAD_PROBE_ID": PROBE_ID, "LOAD_TARGET_URL": TARGET_URL}
        env.update(extra)
        return env

    def run_driver(self, hub, *args, env=None):
        out = io.StringIO()
        argv = ["--evidence", self.evidence_path, *args]
        self.err = io.StringIO()
        code = driver.run(argv, env=env if env is not None else self.env(hub), clock=self.clock, out=out,
                          err=self.err)
        evidence = None
        if os.path.exists(self.evidence_path):
            with open(self.evidence_path) as handle:
                evidence = json.load(handle)
        return code, out.getvalue(), evidence

    def assert_clean(self, hub, *texts):
        port = str(hub.server.server_address[1])
        forbidden = [PASSWORD, USERNAME, TOKEN, PROBE_ID, TARGET_URL, "target-marker", BODY_MARKER,
                     HEADER_MARKER, "Bearer", "127.0.0.1", "localhost", "http://", "/api/", "MARKER",
                     "elsewhere.invalid", ":" + port]
        for text in texts:
            for needle in forbidden:
                self.assertNotIn(needle, text, f"leaked {needle!r}")


class RecordAllowlistTest(unittest.TestCase):
    def test_record_fields_are_exactly_the_allowlist(self):
        rec = driver.RequestRecord(seq=1, method="GET", endpoint_class="probe.snapshot", status=200,
                                   ts_utc="2026-11-18T00:00:00.000Z", rate_rps=1.0)
        self.assertEqual(tuple(rec.to_dict()), driver.RECORD_FIELDS)
        self.assertEqual(set(driver.RECORD_FIELDS),
                         {f.name for f in driver.dataclasses.fields(driver.RequestRecord)})

    def test_record_fields_match_the_hard_coded_allowlist(self):
        # Hard-coded on purpose: comparing against driver.RECORD_FIELDS would
        # let a field silently drop out of (or join) the allowlist.
        expected = {"seq", "method", "endpoint_class", "status", "ts_utc", "rate_rps"}
        self.assertEqual(set(driver.RECORD_FIELDS), expected)
        self.assertEqual({f.name for f in driver.dataclasses.fields(driver.RequestRecord)}, expected)
        rec = driver.RequestRecord(seq=1, method="GET", endpoint_class="probe.snapshot", status=200,
                                   ts_utc="2026-11-18T00:00:00.000Z", rate_rps=1.0)
        self.assertEqual(set(rec.to_dict()), expected)

    def test_record_rejects_unlisted_values(self):
        good = {"seq": 1, "method": "GET", "endpoint_class": "probe.snapshot", "status": 200,
                "ts_utc": "t", "rate_rps": 1.0}
        for field, bad in (("endpoint_class", "/api/monitors/1"), ("method", "DELETE"),
                           ("status", "500 Internal"), ("seq", 0), ("rate_rps", "fast")):
            with self.subTest(field=field), self.assertRaises((ValueError, TypeError)):
                driver.RequestRecord(**{**good, field: bad})
        with self.assertRaises(TypeError):
            driver.RequestRecord(**good, url="http://x")

    def test_classification(self):
        self.assertEqual(driver.classify_status(429), "http_429_rate_limited")
        self.assertEqual(driver.classify_status(500), "http_500_server_error")
        self.assertEqual(driver.classify_status(503), "http_5xx_other")
        self.assertEqual(driver.classify_status(409), "http_4xx_client_error")
        self.assertEqual(driver.classify_status(302), "http_other_non_2xx")


class PacedModeTest(DriverTestCase):
    def test_paced_single_probe_stays_under_limiter_and_converges(self):
        hub = self.hub(rps=50, burst=100, converge_after_polls=3)
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-100",
                                        "--monitors", "100")
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["result"], "passed")
        self.assertIsNone(ev["failure"])
        self.assertEqual(ev["monitors_assigned"], 100)
        self.assertEqual(ev["monitor_timing"], {"interval": 20, "retry_interval": 20, "timeout": 4})
        self.assertEqual(len(hub.create_bodies), 100)
        self.assertEqual({(b["interval"], b["retry_interval"], b["timeout"]) for b in hub.create_bodies},
                         {(20, 20, 4)})
        self.assertEqual(ev["pacing"]["target_rps"], 25.0)
        self.assertEqual(ev["pacing"]["hub_rate_limit_rps"], 50.0)
        self.assertEqual(ev["pacing"]["hub_rate_limit_burst"], 100)
        self.assertEqual(ev["pacing"]["hub_rate_limit_source"], {"rps": "default", "burst": "default"})
        # The first two convergence polls see the previous revision reported
        # as applied (desired == applied); only the count proves publication.
        self.assertEqual(ev["convergence"], {
            "converged": True, "polls": 3, "seconds": 4.0, "applied_assignment_count": 100,
            "baseline_assignment_count": 0, "expected_min_assignment_count": 100})
        self.assertEqual(ev["request_count"], 1 + 1 + 300 + 3)
        self.assertLessEqual(ev["max_rate_rps"], 25.0)
        self.assertEqual(ev["counts"]["monitor.create 201"], 100)
        self.assertEqual(ev["counts"]["assignment.replace 200"], 100)
        self.assertNotIn(429, {r["status"] for r in ev["requests"]})
        self.assertEqual([r["seq"] for r in ev["requests"]], list(range(1, 306)))
        for record in ev["requests"]:
            self.assertEqual(set(record), set(driver.RECORD_FIELDS))
        lines = [json.loads(line) for line in out.splitlines()]
        self.assertEqual(lines[-1]["summary"]["result"], "passed")
        for line in lines[:-1]:
            self.assertEqual(set(line), set(driver.RECORD_FIELDS))
        self.assertEqual(oct(os.stat(self.evidence_path).st_mode & 0o777), "0o600")
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_limits_from_env_are_recorded_and_bound_the_pace(self):
        hub = self.hub(rps=10, burst=20)
        env = self.env(hub, RATE_LIMIT_RPS="10", RATE_LIMIT_BURST="20")
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-5",
                                        "--monitors", "5", env=env)
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["pacing"]["target_rps"], 5.0)
        self.assertEqual(ev["pacing"]["hub_rate_limit_source"], {"rps": "env", "burst": "env"})

    def test_peer_snapshot_is_polled_and_its_500_is_classified(self):
        hub = self.hub(converge_after_polls=5)
        hub.faults[("peer.snapshot", 2)] = 500
        env = self.env(hub, LOAD_PEER_SNAPSHOT_URL=hub.url + "/peer/snapshot")
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-3",
                                        "--monitors", "3", env=env)
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "http_500_server_error")
        self.assertEqual(ev["failure"]["endpoint_class"], "peer.snapshot")
        self.assertEqual(ev["failure"]["status"], 500)
        self.assertEqual(hub.calls[-1], ("GET", "peer.snapshot"))
        self.assertEqual(sum(1 for c in hub.calls if c[1] == "peer.snapshot"), 2)
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_probe_snapshot_500_is_distinct_from_429(self):
        hub = self.hub(converge_after_polls=10)
        hub.faults[("probe.snapshot", 2)] = 500
        code, _, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-2", "--monitors", "2")
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "http_500_server_error")
        self.assertEqual(ev["failure"]["endpoint_class"], "probe.snapshot")
        self.assertEqual(ev["convergence"], None)

    def test_first_non_2xx_stops_with_no_retry(self):
        hub = self.hub()
        hub.faults[("monitor.create", 3)] = 503
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-10",
                                        "--monitors", "10")
        self.assertEqual(code, 1)
        self.assertEqual(ev["result"], "failed")
        self.assertEqual(ev["monitors_assigned"], 2)
        fail = ev["failure"]
        self.assertEqual((fail["category"], fail["method"], fail["endpoint_class"], fail["status"]),
                         ("http_5xx_other", "POST", "monitor.create", 503))
        self.assertEqual(fail["seq"], ev["request_count"])
        self.assertEqual(hub.calls[-1], ("POST", "monitor.create"))
        self.assertEqual(sum(1 for c in hub.calls if c[1] == "monitor.create"), 3)
        self.assertEqual(len(hub.calls), ev["request_count"])
        self.assertIsInstance(fail["requests_prev_5s"], int)
        self.assertIsInstance(fail["rate_prev_5s_rps"], float)
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_429_during_paced_run_is_recorded(self):
        hub = self.hub()
        hub.faults[("assignment.read", 4)] = 429
        code, _, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-9", "--monitors", "9")
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "http_429_rate_limited")
        self.assertEqual(ev["failure"]["endpoint_class"], "assignment.read")

    def test_redirect_is_not_followed(self):
        hub = self.hub()
        hub.redirect_class = "assignment.read"
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-1", "--monitors", "1")
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "http_other_non_2xx")
        self.assertEqual(ev["failure"]["status"], 302)
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_not_converged_after_timeout(self):
        hub = self.hub(converge_after_polls=10**6)
        code, _, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-1", "--monitors", "1",
                                      "--converge-timeout", "10")
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "not_converged")
        self.assertEqual(ev["failure"]["endpoint_class"], "probe.snapshot")
        self.assertEqual(ev["counts"]["probe.snapshot 200"], 1 + 6)

    def test_convergence_counts_from_existing_assignment_baseline(self):
        hub = self.hub(converge_after_polls=2)
        for i in range(1, 5):
            hub.monitors[i] = {"revision": 2, "probes": [PROBE_ID]}
        hub.published = (9, 4)
        code, _, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-3", "--monitors", "3")
        self.assertEqual(code, 0)
        self.assertEqual(ev["convergence"]["baseline_assignment_count"], 4)
        self.assertEqual(ev["convergence"]["expected_min_assignment_count"], 7)
        self.assertEqual(ev["convergence"]["applied_assignment_count"], 7)

    def test_transport_error_records_category_only(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        hub = self.hub()
        env = self.env(hub, LOAD_HUB_URL=f"http://127.0.0.1:{port}")
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-1",
                                        "--monitors", "1", env=env)
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "transport_connection")
        self.assertIsNone(ev["failure"]["status"])
        self.assertEqual(ev["requests"][0]["endpoint_class"], "auth.login")
        text = out + Path(self.evidence_path).read_text()
        self.assertNotIn(str(port), text.replace('"seq": ', ""))
        self.assertNotIn("refused", text.lower())
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())


    def test_explicit_interval_is_sent_and_recorded_in_evidence_only(self):
        hub = self.hub()
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-2",
                                        "--monitors", "2", "--interval", "15")
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["monitor_timing"], {"interval": 15, "retry_interval": 15, "timeout": 4})
        self.assertEqual(len(hub.create_bodies), 2)
        for body in hub.create_bodies:
            self.assertEqual(body["interval"], 15)
            self.assertEqual(body["retry_interval"], 15)
            self.assertEqual(body["timeout"], 4)
        for record in ev["requests"]:
            self.assertEqual(set(record), {"seq", "method", "endpoint_class", "status", "ts_utc", "rate_rps"})
            self.assertNotIn("interval", record)
            self.assertNotIn("retry_interval", record)
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_interval_min_5_is_accepted_and_recorded(self):
        hub = self.hub()
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-1",
                                        "--monitors", "1", "--interval", "5")
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["monitor_timing"], {"interval": 5, "retry_interval": 5, "timeout": 4})
        self.assertEqual(len(hub.create_bodies), 1)
        self.assertEqual(hub.create_bodies[0]["interval"], 5)
        self.assertEqual(hub.create_bodies[0]["retry_interval"], 5)
        self.assertEqual(hub.create_bodies[0]["timeout"], 4)

    def test_interval_max_2147483647_is_accepted_and_recorded(self):
        # The inclusive max is 2**31-1 (MariaDB INT / int32). A mutant that
        # changes "> MAX_INTERVAL" to ">=" would reject this value.
        self.assertEqual(driver.MAX_INTERVAL, 2147483647)
        hub = self.hub()
        code, out, ev = self.run_driver(hub, "--mode", "paced", "--phase", "single-probe-1",
                                        "--monitors", "1", "--interval", "2147483647")
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["monitor_timing"], {
            "interval": 2147483647, "retry_interval": 2147483647, "timeout": 4,
        })
        self.assertEqual(len(hub.create_bodies), 1)
        self.assertEqual(hub.create_bodies[0]["interval"], 2147483647)
        self.assertEqual(hub.create_bodies[0]["retry_interval"], 2147483647)
        self.assertEqual(hub.create_bodies[0]["timeout"], 4)


@unittest.skipUnless(hasattr(signal, "SIGINT") and os.name == "posix", "needs POSIX SIGINT")
class InterruptTest(unittest.TestCase):
    """SIGINT/SIGTERM mid-request still write evidence and exit 130/143."""

    def interrupt_mid_request(self, sig):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(4)
        listener.settimeout(20)
        port = listener.getsockname()[1]
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.addCleanup(listener.close)
        evidence_path = os.path.join(tmp.name, "evidence.json")
        env = {"PATH": os.environ.get("PATH", ""), "LOAD_HUB_URL": f"http://127.0.0.1:{port}",
               "BOOTSTRAP_USERNAME": USERNAME, "BOOTSTRAP_PASSWORD": PASSWORD,
               "LOAD_PROBE_ID": PROBE_ID, "LOAD_TARGET_URL": TARGET_URL}
        script = str(Path(__file__).resolve().parent / "bounded_load_driver.py")
        proc = subprocess.Popen([sys.executable, "-B", script, "--evidence", evidence_path,
                                 "--mode", "paced", "--phase", "single-probe-1", "--monitors", "1"],
                                env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(lambda: proc.poll() is None and proc.kill())
        # The hub accepts the login connection and never answers, so the
        # driver is blocked inside a request when the signal lands.
        conn, _ = listener.accept()
        self.addCleanup(conn.close)
        proc.send_signal(sig)
        out, err = proc.communicate(timeout=20)
        self.assertTrue(os.path.exists(evidence_path), "a stopped run must still write evidence")
        with open(evidence_path) as handle:
            ev = json.load(handle)
        self.assertEqual(ev["monitors_assigned"], 0)
        self.assertNotIn("Traceback", err)
        text = out + err + Path(evidence_path).read_text()
        for needle in (PASSWORD, USERNAME, PROBE_ID, TARGET_URL, "127.0.0.1", ":" + str(port)):
            self.assertNotIn(needle, text)
        return proc.returncode, ev, json.loads(out.strip().splitlines()[-1])["summary"]

    def test_sigint_during_request_writes_interrupted_evidence(self):
        code, ev, summary = self.interrupt_mid_request(signal.SIGINT)
        self.assertEqual(code, 130)
        self.assertEqual(ev["result"], "interrupted")
        self.assertEqual(ev["failure"]["category"], "interrupted")
        self.assertEqual(summary["failure"], "interrupted")

    def test_sigterm_during_request_writes_terminated_evidence(self):
        code, ev, summary = self.interrupt_mid_request(signal.SIGTERM)
        self.assertEqual(code, 143)
        self.assertEqual(ev["result"], "terminated")
        self.assertEqual(ev["failure"]["category"], "terminated")
        self.assertEqual(summary["failure"], "terminated")


class UnpacedControlTest(DriverTestCase):
    def seed(self, hub, n):
        for i in range(1, n + 1):
            hub.monitors[i] = {"revision": 1, "probes": []}

    def test_unpaced_control_hits_429_at_burst(self):
        hub = self.hub(rps=50, burst=100)
        self.seed(hub, 100)
        code, out, ev = self.run_driver(hub, "--mode", "unpaced-control", "--phase", "unpaced-control")
        self.assertEqual(code, 0, out)
        self.assertEqual(ev["result"], "control_passed")
        self.assertEqual(ev["control"]["observed"], "http_429_rate_limited")
        self.assertEqual(ev["control"]["endpoint_class"], "assignment.read")
        # login + list take two tokens; the 101st request overall is refused.
        self.assertEqual(ev["control"]["first_429_seq"], 101)
        self.assertEqual(ev["request_count"], 101)
        self.assertIsNone(ev["pacing"]["target_rps"])
        self.assertEqual(ev["requests"][-1]["status"], 429)
        self.assertGreater(ev["max_rate_rps"], 50.0)
        self.assert_clean(hub, out, Path(self.evidence_path).read_text())

    def test_unpaced_control_without_429_fails(self):
        hub = self.hub(limiter=False)
        self.seed(hub, 3)
        code, _, ev = self.run_driver(hub, "--mode", "unpaced-control", "--phase", "unpaced-control",
                                      "--control-max-requests", "120")
        self.assertEqual(code, 1)
        self.assertEqual(ev["failure"]["category"], "control_no_429")
        self.assertEqual(ev["control"]["observed"], "control_no_429")
        self.assertEqual(ev["request_count"], 2 + 120)

    def test_unpaced_control_other_status_fails(self):
        hub = self.hub(limiter=False)
        self.seed(hub, 3)
        hub.faults[("assignment.read", 5)] = 500
        code, _, ev = self.run_driver(hub, "--mode", "unpaced-control", "--phase", "unpaced-control")
        self.assertEqual(code, 1)
        self.assertEqual(ev["control"]["observed"], "http_500_server_error")


class ConfigValidationTest(DriverTestCase):
    def expect_usage(self, *args, env=None):
        hub = self.hub()
        code, _, ev = self.run_driver(hub, *args, env=env if env is not None else self.env(hub))
        self.assertEqual(code, 2)
        self.assertTrue(self.err.getvalue().startswith("usage error: "))
        self.assertIsNone(ev)
        self.assertEqual(hub.calls, [])

    def test_rejects_rate_at_or_above_hub_limit(self):
        self.expect_usage("--mode", "paced", "--phase", "p", "--target-rps", "50")
        self.expect_usage("--mode", "paced", "--phase", "p", "--target-rps", "12",
                          "--hub-rate-limit-rps", "10")

    def test_rejects_free_text_phase_and_bad_commit(self):
        self.expect_usage("--mode", "paced", "--phase", "http://host/x")
        self.expect_usage("--mode", "paced", "--phase", "p", "--hub-commit", "main")

    def test_requires_env_and_bounds(self):
        self.expect_usage("--mode", "paced", "--phase", "p", env={})
        self.expect_usage("--mode", "paced", "--phase", "p", "--monitors", str(driver.MAX_MONITORS + 1))
        self.expect_usage("--mode", "unpaced-control", "--phase", "c", "--target-rps", "5")
        self.expect_usage("--mode", "unpaced-control", "--phase", "c", "--control-max-requests", "100")

    def test_rejects_invalid_interval(self):
        self.expect_usage("--mode", "paced", "--phase", "p", "--interval", "0")
        self.expect_usage("--mode", "paced", "--phase", "p", "--interval", "-1")
        # timeout is 4 s; hub validateMonitorTiming requires timeout < interval
        self.expect_usage("--mode", "paced", "--phase", "p", "--interval", "4")
        self.expect_usage("--mode", "paced", "--phase", "p", "--interval", "3")
        self.expect_usage("--mode", "paced", "--phase", "p", "--interval", str(driver.MAX_INTERVAL + 1))
        # A usage error must not write evidence or send a request.
        hub = self.hub()
        code, _, ev = self.run_driver(hub, "--mode", "paced", "--phase", "p", "--interval", "0")
        self.assertEqual(code, 2)
        self.assertIn("usage error: --interval must be a positive integer", self.err.getvalue())
        self.assertIsNone(ev)
        self.assertEqual(hub.calls, [])


class PacerTest(unittest.TestCase):
    def test_spacing(self):
        clock = FakeClock()
        pacer = driver.Pacer(clock, 25.0)
        starts = []
        for _ in range(50):
            pacer.wait()
            starts.append(clock.monotonic())
        gaps = [b - a for a, b in zip(starts, starts[1:])]
        self.assertTrue(all(abs(g - 0.04) < 1e-9 for g in gaps), gaps[:5])

    def test_unpaced_never_sleeps(self):
        clock = FakeClock()
        pacer = driver.Pacer(clock, None)
        for _ in range(10):
            pacer.wait()
        self.assertEqual(clock.slept, 0.0)


if __name__ == "__main__":
    unittest.main()
