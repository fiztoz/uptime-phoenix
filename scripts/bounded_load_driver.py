#!/usr/bin/env python3
"""Bounded, paced load driver for the multi-region single-probe-N phases (#270).

It creates N HTTP monitors through the hub API, assigns each to one probe,
then polls the probe's config snapshot until the probe has applied the
published revision. Every request goes through one client-side pacer kept
below the hub ingress limiter (RATE_LIMIT_RPS / RATE_LIMIT_BURST, which run
before auth and are keyed per client IP).

Evidence is fixed-field by construction. A per-request record holds only
sequence, method, endpoint class, HTTP status, a UTC timestamp and the
trailing request rate. Bodies, headers, URLs, hosts, IDs, credentials and
exception text are never stored or printed. The first non-2xx (or transport
error) stops the run immediately, with no retry, and the evidence file is
still written.

Modes:
  paced           create/assign N monitors, then poll the snapshot to convergence
  unpaced-control deliberately exceed the ingress limit with sequential
                  unpaced reads; passes only when the hub answers 429

Standard library only, so it runs from a plain checkout at a pinned commit.
See scripts/BOUNDED_LOAD_DRIVER.md.
"""
import argparse
import collections
import dataclasses
import datetime
import json
import os
import re
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

DRIVER_VERSION = "1"
EVIDENCE_SCHEMA = "uptime-phoenix.bounded-load-evidence/v1"

DEFAULT_HUB_RPS = 50.0
DEFAULT_HUB_BURST = 100
DEFAULT_TARGET_RPS = 25.0
MAX_MONITORS = 2000
RATE_WINDOW_SECONDS = 5.0  # failure context: requests in the preceding 5 s
CURRENT_RATE_WINDOW_SECONDS = 1.0  # per-record rate: requests started in the last second
SNAPSHOT_POLL_SECONDS = 2.0
DEFAULT_CONVERGE_TIMEOUT = 120.0
REQUEST_TIMEOUT = 30.0

# The only methods and endpoint classes a record can carry.
METHODS = frozenset({"GET", "POST", "PUT"})
ENDPOINT_CLASSES = frozenset({
    "auth.login",
    "monitor.create",
    "monitor.list",
    "assignment.read",
    "assignment.replace",
    "probe.snapshot",
    "peer.snapshot",
})
RECORD_FIELDS = ("seq", "method", "endpoint_class", "status", "ts_utc", "rate_rps")

# Failure categories. A 500 is kept apart from other 5xx because a snapshot
# path can collapse several distinct internal errors into a 500, while the
# ingress limiter answers 429.
CATEGORIES = frozenset({
    "http_429_rate_limited",
    "http_500_server_error",
    "http_5xx_other",
    "http_4xx_client_error",
    "http_other_non_2xx",
    "transport_timeout",
    "transport_connection",
    "transport_other",
    "invalid_response",
    "not_converged",
    "control_no_429",
})

PHASE_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,39}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")


class UsageError(Exception):
    """Invalid configuration; nothing was sent."""


@dataclasses.dataclass(frozen=True)
class RequestRecord:
    """One request. The field set is the allowlist; nothing else is logged."""
    seq: int
    method: str
    endpoint_class: str
    status: object  # int HTTP status, or None for a transport error
    ts_utc: str
    rate_rps: float

    def __post_init__(self):
        if not isinstance(self.seq, int) or self.seq < 1:
            raise ValueError("seq must be a positive int")  # also rejects non-int
        if self.method not in METHODS:
            raise ValueError("method not allowlisted")
        if self.endpoint_class not in ENDPOINT_CLASSES:
            raise ValueError("endpoint class not allowlisted")
        if self.status is not None and (not isinstance(self.status, int) or isinstance(self.status, bool)):
            raise TypeError("status must be an int or None")
        if not isinstance(self.rate_rps, float):
            raise TypeError("rate_rps must be a float")

    def to_dict(self):
        return {name: getattr(self, name) for name in RECORD_FIELDS}


class DriverFailure(Exception):
    """Stops the run. Carries only allowlisted classification fields."""

    def __init__(self, category, record=None):
        if category not in CATEGORIES:
            raise ValueError("unknown failure category")
        super().__init__(category)
        self.category = category
        self.record = record


def classify_status(status):
    if status == 429:
        return "http_429_rate_limited"
    if status == 500:
        return "http_500_server_error"
    if 500 <= status <= 599:
        return "http_5xx_other"
    if 400 <= status <= 499:
        return "http_4xx_client_error"
    return "http_other_non_2xx"


def classify_transport(exc):
    if isinstance(exc, (socket.timeout, TimeoutError)):
        return "transport_timeout"
    if isinstance(exc, urllib.error.URLError):
        reason = exc.reason
        if isinstance(reason, (socket.timeout, TimeoutError)):
            return "transport_timeout"
        if isinstance(reason, (ConnectionError, OSError)):
            return "transport_connection"
        return "transport_other"
    if isinstance(exc, (ConnectionError, OSError)):
        return "transport_connection"
    return "transport_other"


class Clock:
    """Real clocks; tests substitute a fake with the same three methods."""

    def monotonic(self):
        return time.monotonic()

    def sleep(self, seconds):
        if seconds > 0:
            time.sleep(seconds)

    def utc_now(self):
        return datetime.datetime.now(datetime.timezone.utc)


class Pacer:
    """Sequential spacing of 1/rps between request starts; None means unpaced."""

    def __init__(self, clock, rps):
        self.clock = clock
        self.interval = None if rps is None else 1.0 / rps
        self.next_at = None

    def wait(self):
        if self.interval is None:
            return
        now = self.clock.monotonic()
        if self.next_at is not None and now < self.next_at:
            self.clock.sleep(self.next_at - now)
            now = self.clock.monotonic()
        self.next_at = max(now, self.next_at or now) + self.interval


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def _iso(dt):
    return dt.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"


class Client:
    """Paced HTTP client that keeps the URL, headers and bodies to itself."""

    def __init__(self, base_url, clock, pacer, sink):
        self._base = base_url.rstrip("/")
        self._token = None
        self.clock = clock
        self.pacer = pacer
        self.sink = sink
        self.records = []
        self._starts = collections.deque()
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())

    def set_token(self, token):
        self._token = token

    def trailing(self, now, window=RATE_WINDOW_SECONDS):
        """Requests started in the half-open window (now - window, now] and their rate."""
        while self._starts and now - self._starts[0] >= RATE_WINDOW_SECONDS:
            self._starts.popleft()
        count = sum(1 for start in self._starts if now - start < window - 1e-9)  # tolerate float drift
        return count, round(count / window, 1)

    def _record(self, method, endpoint_class, status):
        now = self.clock.monotonic()
        _, rate = self.trailing(now, CURRENT_RATE_WINDOW_SECONDS)
        record = RequestRecord(seq=len(self.records) + 1, method=method, endpoint_class=endpoint_class,
                               status=status, ts_utc=_iso(self.clock.utc_now()), rate_rps=float(rate))
        self.records.append(record)
        self.sink(record)
        return record

    def call(self, method, endpoint_class, url, body=None, auth=True, absolute=False):
        if method not in METHODS or endpoint_class not in ENDPOINT_CLASSES:
            raise ValueError("method or endpoint class not allowlisted")
        self.pacer.wait()
        self._starts.append(self.clock.monotonic())
        headers = {"Accept": "application/json"}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if auth and self._token:
            headers["Authorization"] = "Bearer " + self._token
        request = urllib.request.Request(url if absolute else self._base + url, data=data,
                                         method=method, headers=headers)
        try:
            with self._opener.open(request, timeout=REQUEST_TIMEOUT) as response:
                status = response.status
                raw = response.read()
        except urllib.error.HTTPError as exc:
            status = exc.code
            exc.close()
            record = self._record(method, endpoint_class, status)
            raise DriverFailure(classify_status(status), record) from None
        except Exception as exc:  # noqa: BLE001 - classified, text never kept
            category = classify_transport(exc)
            record = self._record(method, endpoint_class, None)
            raise DriverFailure(category, record) from None
        record = self._record(method, endpoint_class, status)
        if not 200 <= status <= 299:
            raise DriverFailure(classify_status(status), record)
        try:
            return json.loads(raw) if raw else None
        except ValueError:
            raise DriverFailure("invalid_response", record) from None


def _require(value, kind, record):
    if not isinstance(value, kind):
        raise DriverFailure("invalid_response", record)
    return value


def login(client, username, password):
    out = client.call("POST", "auth.login", "/api/auth/login",
                      {"username": username, "password": password}, auth=False)
    token = _require(out, dict, client.records[-1]).get("token")
    client.set_token(_require(token, str, client.records[-1]))


def create_and_assign(client, phase, index, probe_id, target_url):
    created = client.call("POST", "monitor.create", "/api/monitors", {
        "name": f"bl-{phase}-{index:04d}", "type": "http", "interval": 5, "timeout": 4,
        "retry_interval": 5, "config": {"url": target_url, "method": "GET"},
    })
    created = _require(created, dict, client.records[-1])
    monitor_id = created.get("id")
    if monitor_id is None and isinstance(created.get("monitor"), dict):
        monitor_id = created["monitor"].get("id")
    _require(monitor_id, int, client.records[-1])
    current = _require(client.call("GET", "assignment.read", f"/api/monitors/{monitor_id}/probes"),
                       dict, client.records[-1])
    revision = _require(current.get("revision"), str, client.records[-1])
    client.call("PUT", "assignment.replace", f"/api/monitors/{monitor_id}/probes", {
        "expected_revision": revision, "probe_ids": [probe_id],
        "health_policy": "any_down", "alert_delivery": "regional",
    })


def snapshot_config(client, probe_id):
    """Read the hub's config snapshot for the probe: (sync_status, desired, applied, count)."""
    detail = _require(client.call("GET", "probe.snapshot", f"/api/probes/{probe_id}"),
                      dict, client.records[-1])
    diagnostics = _require(detail.get("diagnostics"), dict, client.records[-1])
    config = _require(diagnostics.get("config"), dict, client.records[-1])
    desired = config.get("desired") if isinstance(config.get("desired"), dict) else {}
    applied = config.get("applied") if isinstance(config.get("applied"), dict) else {}
    count = applied.get("assignment_count", 0)
    if not isinstance(count, int) or isinstance(count, bool):
        raise DriverFailure("invalid_response", client.records[-1])
    return config.get("sync_status"), desired.get("revision"), applied.get("revision"), count


def poll_until_converged(client, probe_id, peer_snapshot_url, timeout, expected_count):
    """Poll the probe snapshot (and optional peer snapshot) every 2 s.

    The hub publishes probe config asynchronously, so desired == applied can
    hold for the previous revision right after the last write. Convergence
    therefore also requires the applied assignment count to reach the
    baseline plus the monitors this run assigned.
    """
    started = client.clock.monotonic()
    polls = 0
    while True:
        polls += 1
        status, desired, applied, count = snapshot_config(client, probe_id)
        if peer_snapshot_url:
            client.call("GET", "peer.snapshot", peer_snapshot_url, auth=False, absolute=True)
        elapsed = client.clock.monotonic() - started
        if (status == "applied" and isinstance(desired, str) and desired and desired == applied
                and count >= expected_count):
            return {"converged": True, "polls": polls, "seconds": round(elapsed, 1),
                    "applied_assignment_count": count}
        if elapsed >= timeout:
            raise DriverFailure("not_converged", client.records[-1])
        client.clock.sleep(SNAPSHOT_POLL_SECONDS)


def unpaced_control(client, max_requests):
    """Sequential unpaced reads. Only a 429 passes; any other end fails."""
    monitors = _require(client.call("GET", "monitor.list", "/api/monitors"), list, client.records[-1])
    ids = [m.get("id") for m in monitors if isinstance(m, dict) and isinstance(m.get("id"), int)]
    if not ids:
        raise DriverFailure("invalid_response", client.records[-1])
    sent = 0
    while sent < max_requests:
        for monitor_id in ids:
            if sent >= max_requests:
                break
            sent += 1
            try:
                client.call("GET", "assignment.read", f"/api/monitors/{monitor_id}/probes")
            except DriverFailure as failure:
                if failure.category == "http_429_rate_limited":
                    return failure.record
                raise
    raise DriverFailure("control_no_429", client.records[-1])


def driver_commit():
    try:
        out = subprocess.run(["git", "rev-parse", "HEAD"], cwd=Path(__file__).resolve().parent,
                             capture_output=True, text=True, timeout=10, check=False)
    except (OSError, subprocess.SubprocessError):
        return "unknown"
    value = out.stdout.strip()
    return value if COMMIT_RE.match(value) else "unknown"


def resolve_limits(args, env):
    def pick(flag, name, default, cast):
        if flag is not None:
            return cast(flag), "flag"
        if env.get(name):
            try:
                return cast(env[name]), "env"
            except ValueError:
                raise UsageError(f"{name} must be a number") from None
        return default, "default"

    rps, rps_src = pick(args.hub_rate_limit_rps, "RATE_LIMIT_RPS", DEFAULT_HUB_RPS, float)
    burst, burst_src = pick(args.hub_rate_limit_burst, "RATE_LIMIT_BURST", DEFAULT_HUB_BURST, int)
    if rps <= 0 or burst <= 0:
        raise UsageError("hub rate limit values must be positive")
    return rps, burst, {"rps": rps_src, "burst": burst_src}


def failure_view(client, failure):
    record = failure.record
    view = {"category": failure.category, "seq": None, "method": None, "endpoint_class": None,
            "status": None, "requests_prev_5s": None, "rate_prev_5s_rps": None}
    if record is not None:
        view.update({"seq": record.seq, "method": record.method,
                     "endpoint_class": record.endpoint_class, "status": record.status})
        count, rate = client.trailing(client.clock.monotonic())
        view.update({"requests_prev_5s": count, "rate_prev_5s_rps": float(rate)})
    return view


def write_evidence(path, evidence):
    path = Path(path)
    tmp = path.with_name(path.name + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as handle:
        json.dump(evidence, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.replace(tmp, path)


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--mode", choices=("paced", "unpaced-control"), required=True)
    p.add_argument("--phase", required=True, help="phase label recorded in evidence, e.g. single-probe-100")
    p.add_argument("--monitors", type=int, default=100)
    p.add_argument("--target-rps", type=float, default=None,
                   help=f"paced request rate (default min({DEFAULT_TARGET_RPS:g}, hub rps / 2))")
    p.add_argument("--hub-rate-limit-rps", type=float, default=None,
                   help="the hub's RATE_LIMIT_RPS (default: $RATE_LIMIT_RPS, else 50)")
    p.add_argument("--hub-rate-limit-burst", type=int, default=None,
                   help="the hub's RATE_LIMIT_BURST (default: $RATE_LIMIT_BURST, else 100)")
    p.add_argument("--control-max-requests", type=int, default=None,
                   help="unpaced-control request cap (default 3 x burst)")
    p.add_argument("--converge-timeout", type=float, default=DEFAULT_CONVERGE_TIMEOUT)
    p.add_argument("--hub-commit", default=None, help="40-hex commit the hub was built from (recorded)")
    p.add_argument("--evidence", required=True, help="evidence JSON path (written 0600)")
    p.add_argument("--quiet", action="store_true", help="do not print per-request records")
    return p.parse_args(argv)


def build_config(args, env):
    """Validate everything before the first request. Secrets stay in env."""
    if not PHASE_RE.match(args.phase):
        raise UsageError("--phase must be lowercase letters, digits and dashes (max 40)")
    if args.hub_commit is not None and not COMMIT_RE.match(args.hub_commit):
        raise UsageError("--hub-commit must be a full 40-hex commit")
    hub_rps, hub_burst, sources = resolve_limits(args, env)
    for name in ("LOAD_HUB_URL", "BOOTSTRAP_USERNAME", "BOOTSTRAP_PASSWORD"):
        if not env.get(name):
            raise UsageError(f"{name} must be set in the environment")
    if not env["LOAD_HUB_URL"].startswith(("http://", "https://")):
        raise UsageError("LOAD_HUB_URL must be an http(s) URL")
    config = {"hub_rps": hub_rps, "hub_burst": hub_burst, "sources": sources}
    if args.mode == "paced":
        if not 1 <= args.monitors <= MAX_MONITORS:
            raise UsageError(f"--monitors must be between 1 and {MAX_MONITORS}")
        for name in ("LOAD_PROBE_ID", "LOAD_TARGET_URL"):
            if not env.get(name):
                raise UsageError(f"{name} must be set in the environment for paced mode")
        target = args.target_rps if args.target_rps is not None else min(DEFAULT_TARGET_RPS, hub_rps / 2)
        if not 0 < target < hub_rps:
            raise UsageError("--target-rps must be positive and below the hub RATE_LIMIT_RPS")
        if args.converge_timeout <= 0:
            raise UsageError("--converge-timeout must be positive")
        config["target_rps"] = float(target)
    else:
        if args.target_rps is not None:
            raise UsageError("unpaced-control does not pace; omit --target-rps")
        cap = args.control_max_requests if args.control_max_requests is not None else 3 * hub_burst
        if cap <= hub_burst:
            raise UsageError("--control-max-requests must exceed the hub burst")
        config["target_rps"] = None
        config["control_max_requests"] = cap
    return config


def run(argv, env=None, clock=None, out=None, err=None):
    env = os.environ if env is None else env
    clock = clock or Clock()
    out = out or sys.stdout
    err = err or sys.stderr
    try:
        args = parse_args(argv)
        config = build_config(args, env)
    except UsageError as exc:
        print(f"usage error: {exc}", file=err)
        return 2

    def sink(record):
        if not args.quiet:
            out.write(json.dumps(record.to_dict(), sort_keys=True) + "\n")
            out.flush()

    client = Client(env["LOAD_HUB_URL"], clock, Pacer(clock, config["target_rps"]), sink)
    evidence = {
        "schema": EVIDENCE_SCHEMA,
        "driver_version": DRIVER_VERSION,
        "driver_commit": driver_commit(),
        "hub_commit": args.hub_commit,
        "mode": args.mode,
        "phase": args.phase,
        "monitors_requested": args.monitors if args.mode == "paced" else None,
        "pacing": {
            "target_rps": config["target_rps"],
            "hub_rate_limit_rps": config["hub_rps"],
            "hub_rate_limit_burst": config["hub_burst"],
            "hub_rate_limit_source": config["sources"],
            "concurrency": 1,
            "control_max_requests": config.get("control_max_requests"),
        },
        "started_utc": _iso(clock.utc_now()),
    }
    result, failure, convergence, monitors_done, control = "failed", None, None, 0, None
    try:
        login(client, env["BOOTSTRAP_USERNAME"], env["BOOTSTRAP_PASSWORD"])
        if args.mode == "paced":
            baseline = snapshot_config(client, env["LOAD_PROBE_ID"])[3]
            for index in range(1, args.monitors + 1):
                create_and_assign(client, args.phase, index, env["LOAD_PROBE_ID"], env["LOAD_TARGET_URL"])
                monitors_done = index
            convergence = poll_until_converged(client, env["LOAD_PROBE_ID"],
                                               env.get("LOAD_PEER_SNAPSHOT_URL") or None,
                                               args.converge_timeout, baseline + args.monitors)
            convergence.update({"baseline_assignment_count": baseline,
                                "expected_min_assignment_count": baseline + args.monitors})
            result = "passed"
        else:
            hit = unpaced_control(client, config["control_max_requests"])
            control = {"expected": "http_429_rate_limited", "observed": "http_429_rate_limited",
                       "first_429_seq": hit.seq, "endpoint_class": hit.endpoint_class}
            result = "control_passed"
    except DriverFailure as exc:
        failure = failure_view(client, exc)
        if args.mode == "unpaced-control":
            control = {"expected": "http_429_rate_limited", "observed": exc.category,
                       "first_429_seq": None, "endpoint_class": failure["endpoint_class"]}
    counts = collections.Counter(f"{r.endpoint_class} {r.status}" for r in client.records)
    evidence.update({
        "finished_utc": _iso(clock.utc_now()),
        "result": result,
        "failure": failure,
        "monitors_assigned": monitors_done if args.mode == "paced" else None,
        "convergence": convergence,
        "control": control,
        "request_count": len(client.records),
        "max_rate_rps": max((r.rate_rps for r in client.records), default=0.0),
        "counts": dict(sorted(counts.items())),
        "requests": [r.to_dict() for r in client.records],
    })
    write_evidence(args.evidence, evidence)
    summary = {"result": result, "requests": len(client.records),
               "failure": failure["category"] if failure else None}
    out.write(json.dumps({"summary": summary}, sort_keys=True) + "\n")
    return 0 if result in ("passed", "control_passed") else 1


if __name__ == "__main__":
    sys.exit(run(sys.argv[1:]))
