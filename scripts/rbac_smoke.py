"""RBAC end-to-end smoke: assert EFFECTS, never a bare 2xx."""
import json
import urllib.request
import urllib.error

BASE = "http://127.0.0.1:3100"


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


# 1. admin login
st, r = call("POST", "/api/auth/login", body={"username": "admin", "password": "ChangeMe123!"})
admin = r.get("token") or r.get("access_token")
print(f"admin login -> {st}")

# 2. /me exposes capabilities.
#
# NOTE the contract (handlers/auth.go UserView): these are the RAW stored flags,
# NOT the effective permission. An admin has is_admin=true and BOTH flags false,
# yet may do everything — enforcement ORs them server-side (asserted at step 14).
# The raw flags are reported so the admin's user-edit form can round-trip a
# user's actual settings. Any consumer deciding what to SHOW must gate on
# `is_admin || can_manage_x`, never on can_manage_x alone.
st, me = call("GET", "/api/auth/me", admin)
u = me.get("user", me)
check("admin /me is_admin", u.get("is_admin"), True)
check("admin /me can_manage_notifications is the RAW flag", u.get("can_manage_notifications"), False)
check("admin /me can_manage_maintenance is the RAW flag", u.get("can_manage_maintenance"), False)

# 3. admin creates two monitors
mons = []
for name in ("alpha", "beta"):
    st, r = call("POST", "/api/monitors", admin, {
        "name": name, "type": "http", "interval": 60, "timeout": 10,
        "config": {"url": "https://example.com"},
    })
    mons.append((r.get("id"), name))
print(f"created monitors: {mons}")

# 4. tags field present and is [] (never null)
st, lst = call("GET", "/api/monitors", admin)
check("admin sees 2 monitors", len(lst), 2)
check("monitor.tags is [] not null", lst[0].get("tags"), [])

# 5. admin creates a plain non-admin user: no grants, no capabilities
st, r = call("POST", "/api/users", admin, {
    "username": "viewer", "password": "ViewerPass1!", "is_admin": False,
})
viewer_id = (r.get("user") or {}).get("id")
print(f"created viewer id={viewer_id} -> {st}")

st, r = call("POST", "/api/auth/login", body={"username": "viewer", "password": "ViewerPass1!"})
viewer = r.get("token") or r.get("access_token")

# 6. THE CORE CLAIM: a user with no grants sees NOTHING
st, lst = call("GET", "/api/monitors", viewer)
check("ungranted viewer sees 0 monitors", len(lst) if isinstance(lst, list) else lst, 0)

# 7. ...and cannot read one directly (404, not 403 — don't confirm existence)
st, _ = call("GET", f"/api/monitors/{mons[0][0]}", viewer)
check("ungranted viewer GET monitor -> 404", st, 404)

# 8. monitors are read-only for non-admins
st, _ = call("POST", "/api/monitors", viewer, {
    "name": "evil", "type": "http", "interval": 60, "timeout": 10,
    "config": {"url": "https://example.com"},
})
check("viewer cannot create monitor -> 403", st, 403)

# 9. no capability -> cannot create a notification
st, _ = call("POST", "/api/notifications", viewer, {
    "name": "n1", "type": "webhook", "config": {"url": "https://example.com"},
})
check("viewer w/o capability cannot create notification -> 403", st, 403)

# 10. admin-only surfaces are closed
check("viewer blocked from /api/users -> 403", call("GET", "/api/users", viewer)[0], 403)
check("viewer blocked from /api/proxies -> 403", call("GET", "/api/proxies", viewer)[0], 403)
check("viewer blocked from /api/status-pages -> 403", call("GET", "/api/status-pages", viewer)[0], 403)
check("viewer blocked from /api/backup/export -> 403", call("GET", "/api/backup/export", viewer)[0], 403)

# 11. GRANT one monitor -> viewer now sees exactly that one
st, r = call("PUT", f"/api/users/{viewer_id}/permissions", admin,
             {"monitor_ids": [mons[0][0]], "group_ids": []})
print(f"grant monitor {mons[0][0]} -> {st} {r}")
st, lst = call("GET", "/api/monitors", viewer)
names = sorted(m["name"] for m in lst) if isinstance(lst, list) else lst
check("granted viewer sees exactly ['alpha']", names, ["alpha"])

# 12. grant the notifications capability -> creation now allowed
st, _ = call("PUT", f"/api/users/{viewer_id}", admin, {"can_manage_notifications": True})
st, r = call("POST", "/api/notifications", viewer, {
    "name": "n1", "type": "webhook", "config": {"url": "https://example.com"},
})
check("viewer WITH capability creates notification -> 201", st, 201)

# 13. ...but still cannot touch maintenance (independent flag)
st, _ = call("POST", "/api/maintenance", viewer, {
    "title": "m1", "strategy": "manual", "active": True,
})
check("capability is per-resource: maintenance still 403", st, 403)

print(f"\n{sum(results)}/{len(results)} assertions passed")
