"""Status-page monitor reorder smoke — asserts EFFECTS on real MariaDB.

Covers the code paths no unit test touches: the raw ON DUPLICATE KEY UPDATE
upsert, the destructive replace-set semantics, and the new 409 sentinel.
"""
import json
import urllib.request
import urllib.error

BASE = "http://127.0.0.1:3100"
results = []


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


def check(label, got, want):
    ok = got == want
    results.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {label}: got={got!r} want={want!r}")


st, r = call("POST", "/api/auth/login", body={"username": "admin", "password": "ChangeMe123!"})
tok = r.get("token") or r.get("access_token")

# status page + three monitors
st, sp = call("POST", "/api/status-pages", tok, {"title": "SP", "slug": "sp-reorder", "published": True})
sp_id = sp.get("id") or (sp.get("status_page") or {}).get("id")

mids = []
for n in ("m1", "m2", "m3"):
    st, m = call("POST", "/api/monitors", tok, {
        "name": n, "type": "http", "interval": 60, "timeout": 10,
        "config": {"url": "https://example.com"}})
    mids.append(m["id"])
m1, m2, m3 = mids

for mid in mids:
    call("POST", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_id": mid})

st, links = call("GET", f"/api/status-pages/{sp_id}/monitors", tok)
check("3 monitors assigned", len(links), 3)

# --- the NEW 409 sentinel (was "slug or custom domain already in use") ---
st, body = call("POST", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_id": m1})
check("duplicate add -> 409", st, 409)
check("409 body names the monitor conflict",
      (body or {}).get("error"), "monitor is already linked to this status page")

# --- reorder: raw ON DUPLICATE KEY UPDATE path on MariaDB ---
st, _ = call("PUT", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_ids": [m3, m1, m2]})
check("reorder -> 200", st, 200)

st, links = call("GET", f"/api/status-pages/{sp_id}/monitors", tok)
order = [(l["monitor_id"], l["display_order"]) for l in links]
check("persisted order is m3,m1,m2 with 10/20/30", order, [(m3, 10), (m1, 20), (m2, 30)])

# --- ordering actually reaches the PUBLIC page (the thing users see) ---
st, pub = call("GET", "/api/status/sp-reorder")
pub_names = [m.get("name") for m in (pub.get("monitors") or [])]
check("public page honors the order", pub_names, ["m3", "m1", "m2"])

# --- destructive replace-set semantics: an omitted monitor is REMOVED ---
st, _ = call("PUT", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_ids": [m1, m2]})
st, links = call("GET", f"/api/status-pages/{sp_id}/monitors", tok)
check("omitting m3 removes it (documented replace-set)",
      sorted(l["monitor_id"] for l in links), sorted([m1, m2]))

# --- empty list clears all (the branch where sqlite/mariadb diverge structurally) ---
st, _ = call("PUT", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_ids": []})
st, links = call("GET", f"/api/status-pages/{sp_id}/monitors", tok)
check("empty list clears every assignment", len(links), 0)

# --- re-add after clear still works (no orphaned unique-key rows left behind) ---
st, _ = call("POST", f"/api/status-pages/{sp_id}/monitors", tok, {"monitor_id": m1})
check("re-add after clear -> 204 (AddMonitor returns No Content)", st, 204)

print(f"\n{sum(results)}/{len(results)} assertions passed")
