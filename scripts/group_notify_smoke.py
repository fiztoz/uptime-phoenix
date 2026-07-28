"""Group (folder) notification end-to-end smoke: assert EFFECTS, never a bare 2xx.

Run against a THROWAWAY MariaDB — see docs/HANDOFF-NEXT.md §5. Never point this at
the dev `phoenix` database.

What it proves, and why each one is here:

  * Migration 009 applies on real MariaDB (group_notifications + monitor_groups.last_status).
    SQLite silently accepts what MariaDB rejects, and the repo's tests are SQLite-only.
  * A notification attached to a FOLDER fires on the FOLDER's own derived status.
  * A notification flagged is_default ("auto-attach to new monitors") attaches to a new
    MONITOR and NEVER to a new FOLDER. This is the exact ask, and a version that got it
    wrong would still return 201 on every call.
  * An `ignore`-condition folder never alerts, even with a provider attached.
  * Two monitors in one folder going DOWN at the same instant produce ONE folder alert,
    not two — the compare-and-set on last_status. This is the MariaDB-specific claim
    (`last_status <=> :old`), and it is the whole reason the column is persisted.

Each notification points at its OWN path on a local sink, so an alert can be attributed
to the exact provider that received it — counting total webhooks would hide a folder
alert being sent to a monitor's provider (or to a sibling folder's).
"""
import json
import threading
import time
import urllib.error
import urllib.request
from collections import defaultdict
from http.server import BaseHTTPRequestHandler, HTTPServer

BASE = "http://127.0.0.1:3100"
SINK_PORT = 3199
SINK = f"http://127.0.0.1:{SINK_PORT}"

# path -> list of payloads received
received = defaultdict(list)
received_lock = threading.Lock()


class SinkHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length).decode() if length else ""
        with received_lock:
            received[self.path].append(raw)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *_args):
        pass  # keep the smoke output readable


sink = HTTPServer(("127.0.0.1", SINK_PORT), SinkHandler)
threading.Thread(target=sink.serve_forever, daemon=True).start()


def call(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw


results = []


def check(label, got, want):
    ok = got == want
    results.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {label}: got={got!r} want={want!r}")


def hook_count(path):
    """Alerts delivered to one provider. Waits briefly — dispatch is async."""
    for _ in range(30):
        with received_lock:
            n = len(received[path])
        if n:
            return n
        time.sleep(0.1)
    with received_lock:
        return len(received[path])


def hook_count_settled(path, settle=1.0):
    """Count after letting any *duplicate* alert arrive. A test for 'exactly one'
    must give a second, wrong alert time to show up, or it proves nothing."""
    time.sleep(settle)
    with received_lock:
        return len(received[path])


def push(token, status):
    st, _ = call("POST", f"/api/push/{token}", body={"status": status})
    return st


def make_push_monitor(name, group_id, token):
    """A push monitor lets the smoke drive status deterministically, instead of
    waiting out a check interval and hoping the network cooperates."""
    st, m = call("POST", "/api/monitors", admin, {
        "name": name,
        "type": "push",
        "interval": 60,
        "group_id": group_id,
        "config": {"push_token": token},
    })
    return st, m


# --- setup ---------------------------------------------------------------

st, r = call("POST", "/api/auth/login", body={"username": "admin", "password": "ChangeMe123!"})
admin = r.get("token") or r.get("access_token")
print(f"admin login -> {st}")


def make_notification(name, path, is_default=False):
    st, n = call("POST", "/api/notifications", admin, {
        "name": name,
        "type": "webhook",
        "config": {"url": f"{SINK}{path}"},
        "active": True,
        "is_default": is_default,
    })
    assert st in (200, 201), (st, n)
    return n["id"]


folder_pager = make_notification("folder-pager", "/folder")
sibling_pager = make_notification("sibling-pager", "/sibling")
default_pager = make_notification("default-pager", "/default", is_default=True)

# --- 1. THE ASK: a default notification must never auto-attach to a folder ---

st, payments = call("POST", "/api/monitor-groups", admin, {
    "name": "payments", "condition": "worst_of_children",
})
check("create folder", st, 201)
payments_id = payments["id"]

st, attached = call("GET", f"/api/monitor-groups/{payments_id}/notifications", admin)
check("new folder has ZERO notifications (is_default must not auto-attach to folders)",
      [n["name"] for n in attached], [])

# --- 2. attach a provider to the folder ---

st, _ = call("POST", f"/api/notifications/{folder_pager}/group/{payments_id}", admin)
check("attach notification to folder", st, 204)

st, attached = call("GET", f"/api/monitor-groups/{payments_id}/notifications", admin)
check("folder now lists exactly its own provider", [n["name"] for n in attached], ["folder-pager"])

# Idempotent: the UI toggles a checkbox and a double-click must not 500.
st, _ = call("POST", f"/api/notifications/{folder_pager}/group/{payments_id}", admin)
check("re-attaching the same pair is idempotent", st, 204)

# --- 3. a monitor inside it. The default DOES still auto-attach to monitors ---

api_token = "smoke-api"
st, api = make_push_monitor("api", payments_id, api_token)
check("create push monitor in folder", st, 201)
api_id = api["id"]

st, mon_notifs = call("GET", f"/api/monitors/{api_id}/notifications", admin)
check("is_default STILL auto-attaches to a new monitor (the control)",
      [n["name"] for n in mon_notifs], ["default-pager"])

# --- 4. the folder alerts on its own derived status ---

push(api_token, "up")
time.sleep(0.5)
check("healthy folder sent no alert", hook_count_settled("/folder", 0.5), 0)

push(api_token, "down")
check("folder trip alerted its provider", hook_count("/folder") >= 1, True)
check("folder trip alerted EXACTLY once", hook_count_settled("/folder"), 1)
check("a sibling folder's provider was NOT alerted", hook_count_settled("/sibling", 0), 0)

with received_lock:
    body = received["/folder"][0]
check("the alert is about the FOLDER, not the monitor", "payments" in body, True)

# Still down: a folder has no resend interval, so it must not re-alert.
push(api_token, "down")
check("still-DOWN folder does not re-alert", hook_count_settled("/folder"), 1)

# Recovery.
push(api_token, "up")
check("folder recovery alerted once more", hook_count_settled("/folder"), 2)

# --- 5. an ignore-condition folder never alerts ---

st, archive = call("POST", "/api/monitor-groups", admin, {"name": "archive", "condition": "ignore"})
archive_id = archive["id"]
call("POST", f"/api/notifications/{sibling_pager}/group/{archive_id}", admin)
old_token = "smoke-old"
make_push_monitor("old", archive_id, old_token)
push(old_token, "up")
push(old_token, "down")
check("ignore-condition folder never alerts", hook_count_settled("/sibling"), 0)

# --- 6. THE RACE: two monitors in one folder go DOWN at the same instant ------
# Exactly one folder alert may leave the building. Without the compare-and-set on
# monitor_groups.last_status, both workers would send.

st, pool = call("POST", "/api/monitor-groups", admin, {
    "name": "pool", "condition": "worst_of_children",
})
pool_id = pool["id"]
race_pager = make_notification("race-pager", "/race")
call("POST", f"/api/notifications/{race_pager}/group/{pool_id}", admin)

tokens = []
for name in ("a", "b"):
    token = f"smoke-race-{name}"
    make_push_monitor(name, pool_id, token)
    tokens.append(token)
    push(token, "up")

time.sleep(0.5)
threads = [threading.Thread(target=push, args=(t, "down")) for t in tokens]
for t in threads:
    t.start()
for t in threads:
    t.join()

check("concurrent folder trip alerted EXACTLY once (compare-and-set held)",
      hook_count_settled("/race", 1.5), 1)

# --- 7. detach ------------------------------------------------------------

st, _ = call("DELETE", f"/api/notifications/{folder_pager}/group/{payments_id}", admin)
check("detach notification from folder", st, 204)
st, attached = call("GET", f"/api/monitor-groups/{payments_id}/notifications", admin)
check("folder no longer lists the provider", attached, [])

print(f"\n{sum(results)}/{len(results)} passed")
sink.shutdown()
raise SystemExit(0 if all(results) else 1)
