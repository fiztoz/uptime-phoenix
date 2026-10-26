#!/usr/bin/env python3
"""M8 real-host acceptance matrix through the local hub's REST API.

Usage: m8_aws_matrix.py <run_dir> <run_id> <arm64_ip> <amd64_ip> [systemd|docker|docker-preflight]

docker runs the Docker-path cases; it needs <run_dir>/image_pin.json
({"repository","version","index_sha256","index_size"}) and the OCI layout
under <run_dir>/artifacts/. docker-preflight runs only the "no Docker on
the host" refusal and must run before Docker is installed.

<run_dir> is a private operator directory holding the hub's admin.password,
the staging_key and unauthorized_key, hub.db and artifacts/ with the pinned
probe releases (see docs/TESTING.md). <run_id> is the
scripts/m8_aws_acceptance.py run whose private/ dir holds the SSH identity and
the console-proven known_hosts.

Every case drives the real hub (installer preflight, install, activation,
cleanup) against two Ubuntu hosts prepared with the helper, the stage
authority key and the restricted staging key. Host-side setup for a case
(firewall rule, helper removal) and host-side verification use the harness's
ubuntu SSH identity. Secrets are read from private files and never printed.
Prints one JSON line per case and exits nonzero if any case fails.
"""
import hashlib
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

R = ""  # the run directory, set from argv
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HUB = "http://127.0.0.1:3900"
VERSION = "0.9.2-m8matrix"
RESULTS = []


def call(method, path, token=None, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(HUB + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return resp.status, json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read() or b"{}")


class Host:
    def __init__(self, run_id, arch, ip):
        self.arch, self.ip = arch, ip
        private = os.path.join(ROOT, "tmp", f"m8-aws-{run_id}", "private")
        self.known_hosts = os.path.join(private, "known_hosts")
        self.ssh_opts = ["-F", "/dev/null", "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={self.known_hosts}",
                         "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
                         "-i", os.path.join(private, f"m8-{run_id}.pem")]

    def fingerprint(self, kind):
        out = subprocess.run(["ssh-keygen", "-lf", self.known_hosts], capture_output=True, text=True, check=True).stdout
        for line in out.splitlines():
            parts = line.split()
            if parts[2] == self.ip and parts[-1] == f"({kind})":
                return parts[1]
        raise SystemExit(f"no {kind} host key pinned for {self.ip}")

    def sudo(self, script):
        """Run a root shell script on the host as test setup/verification."""
        # The script travels on stdin so no remote shell expands it first.
        res = subprocess.run(["ssh", *self.ssh_opts, f"ubuntu@{self.ip}", "sudo sh -eu"], input=script,
                             capture_output=True, text=True, timeout=120)
        return res.returncode, res.stdout.strip()


def artifact(arch):
    path = os.path.join(R, "artifacts", f"uptime-phoenix-probe_{VERSION}_linux_{arch}")
    data = open(path, "rb").read()
    return hashlib.sha256(data).hexdigest(), len(data)


def install_body(host, op, pin_arch=None, fingerprint=None, key_file="staging_key"):
    arch = pin_arch or host.arch
    digest, size = artifact(arch)
    return {
        "operation_id": op, "ssh_address": f"{host.ip}:22", "ssh_user": "root",
        "host_key_fingerprint": fingerprint or host.fingerprint("ED25519"),
        "private_key": open(os.path.join(R, key_file)).read(),
        "binary_version": VERSION, "binary_architecture": arch, "binary_sha256": digest, "binary_size": size,
    }


def poll(path, token, done, limit=420):
    deadline, view = time.time() + limit, {}
    while time.time() < deadline:
        code, view = call("GET", path, token)
        if code == 200 and view.get("status") in done:
            return view
        time.sleep(3)
    raise RuntimeError(f"poll timeout on {path}: {view}")


def record(case, host, ok, **facts):
    line = {"case": case, "host": host.arch, "result": "pass" if ok else "FAIL", **facts}
    RESULTS.append(line)
    print(json.dumps(line), flush=True)


def expect_refusal(case, token, host, body, code):
    status, view = call("POST", "/api/probe-installs", token, body)
    _, observed = call("GET", f"/api/probe-installs/{body['operation_id']}", token)
    ok = status == 422 and view.get("code") == code and observed.get("code") == "install_not_found"
    record(case, host, ok, http=status, code=view.get("code"), receipt=observed.get("code"))


def install_ready(token, host, op):
    status, view = call("POST", "/api/probe-installs", token, install_body(host, op))
    if status != 202:
        raise RuntimeError(f"install {op}: {status} {view}")
    return poll(f"/api/probe-installs/{op}", token, {"resources_ready", "failed", "canceled", "interrupted"})


def activate(token, host, op, limit=420):
    body = {"private_key": open(os.path.join(R, "staging_key")).read(), "probe_key": f"m8-{host.arch}-{op[:8]}",
            "name": f"M8 matrix {host.arch}", "location": "ap-southeast-1", "listen_port": 8443}
    status, view = call("POST", f"/api/probe-installs/{op}/activate", token, body)
    if status != 202:
        raise RuntimeError(f"activate {op}: {status} {view}")
    return poll(f"/api/probe-installs/{op}/activation", token, {"active", "failed", "canceled", "interrupted"}, limit)


def cleanup(token, host, op):
    body = {"cleanup_id": str(uuid.uuid4()), "private_key": open(os.path.join(R, "staging_key")).read()}
    status, view = call("POST", f"/api/probe-installs/{op}/cleanup", token, body)
    if status != 202:
        raise RuntimeError(f"cleanup {op}: {status} {view}")
    return poll(f"/api/probe-installs/{op}/cleanup", token, {"cleaned", "failed", "interrupted"})


def host_resources(host, op):
    """Count the operation's resources and whether its unit is loaded or anything listens on 8443."""
    base = f"uptime-phoenix-probe-{op}"
    _, out = host.sudo(
        f"n=0; for p in /var/lib/{base} /opt/{base} /etc/systemd/system/{base}.service; do test -e $p && n=$((n+1)); done; "
        f"a=$(systemctl is-active {base}.service 2>/dev/null || true); "
        f"l=$(ss -ltnH 'sport = :8443' | wc -l); echo $n $a $l")
    count, active, listeners = out.split()
    return int(count), active, int(listeners)


def image_pin(arch):
    pin = json.load(open(os.path.join(R, "image_pin.json")))
    return {"image_repository": pin["repository"], "image_version": pin["version"], "image_architecture": arch,
            "image_index_sha256": pin["index_sha256"], "image_index_size": pin["index_size"]}


def docker_body(host, op, arch=None, key_file="staging_key"):
    body = {"operation_id": op, "ssh_address": f"{host.ip}:22", "ssh_user": "root",
            "host_key_fingerprint": host.fingerprint("ED25519"),
            "private_key": open(os.path.join(R, key_file)).read(), "mode": "docker"}
    body.update(image_pin(arch or host.arch))
    return body


def docker_resources(host, op):
    """Data dir present, container state (or "absent"), listeners on 8443 and whether the image tag exists."""
    base = f"uptime-phoenix-probe-{op}"
    _, out = host.sudo(
        f"d=0; test -e /var/lib/{base} && d=1; "
        f"c=$(docker container inspect --format '{{{{.State.Status}}}}' {base} 2>/dev/null || echo absent); "
        f"i=$(docker image ls --quiet uptime-phoenix-probe | wc -l); "
        f"l=$(ss -ltnH 'sport = :8443' | wc -l); echo $d $c $l $i")
    data, state, listeners, images = out.split()
    return int(data), state, int(listeners), int(images)


def docker_install_ready(token, host, op):
    status, view = call("POST", "/api/probe-installs", token, docker_body(host, op))
    if status != 202:
        raise RuntimeError(f"docker install {op}: {status} {view}")
    return poll(f"/api/probe-installs/{op}", token, {"resources_ready", "failed", "canceled", "interrupted"}, 900)


def main_docker(token, arm, amd, preflight_only):
    new = lambda: str(uuid.uuid4())  # noqa: E731
    if preflight_only:
        status, view = call("POST", "/api/probe-installs", token, docker_body(amd, new()))
        record("docker_unavailable", amd, status == 422 and view.get("code") == "host_docker_unavailable",
               http=status, code=view.get("code"))
        return
    status, view = call("POST", "/api/probe-installs", token, docker_body(amd, new(), arch="arm64"))
    record("docker_wrong_architecture", amd, status == 422 and view.get("code") == "host_architecture_mismatch",
           http=status, code=view.get("code"))
    for host in (amd, arm):
        op = new()
        view = docker_install_ready(token, host, op)
        act = activate(token, host, op) if view["status"] == "resources_ready" else {}
        state = docker_resources(host, op)
        record("docker_clean_install_and_activate", host, view["status"] == "resources_ready" and act.get("status") == "active"
               and state[0] == 1 and state[1] == "running" and state[2] >= 1 and state[3] >= 1,
               install=view["status"], failed_phase=view.get("failed_phase"), activation=act.get("status"),
               probe_id=act.get("probe_id"), host_state=list(state))
        if host is amd:
            status, refusal = call("POST", "/api/probe-installs", token, docker_body(host, new()))
            record("docker_existing_install", host, status == 422 and refusal.get("code") == "host_existing_install",
                   http=status, code=refusal.get("code"))
        cv = cleanup(token, host, op)
        state = docker_resources(host, op)
        record("docker_cleanup_active_probe", host, cv["status"] == "cleaned" and state[0] == 0 and state[1] == "absent"
               and state[2] == 0 and state[3] >= 1, cleanup=cv["status"], host_state=list(state))


def main():
    global R
    R, run_id, arm_ip, amd_ip = os.path.abspath(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
    arm, amd = Host(run_id, "arm64", arm_ip), Host(run_id, "amd64", amd_ip)
    password = open(os.path.join(R, "admin.password")).read().strip()
    code, login = call("POST", "/api/auth/login", body={"username": "m8admin", "password": password})
    if code != 200:
        raise SystemExit(f"login failed {code}")
    token = login["token"]
    new = lambda: str(uuid.uuid4())  # noqa: E731
    mode = sys.argv[5] if len(sys.argv) > 5 else "systemd"

    # Leftovers of an aborted earlier attempt are removed first, by the product.
    leftovers = subprocess.run(["sqlite3", os.path.join(R, "hub.db"),
                                "select o.operation_id, o.ssh_address from probe_install_operations o where o.state in "
                                "('resources_ready','failed','canceled','interrupted') and not exists (select 1 from "
                                "probe_install_cleanups c where c.operation_id = o.operation_id and c.state = 'cleaned')"],
                               capture_output=True, text=True, check=True).stdout.split()
    for row in leftovers:
        op, address = row.split("|")
        host = amd if address.startswith(amd.ip) else arm
        cv = cleanup(token, host, op)
        print(json.dumps({"pre_cleanup": op, "host": host.arch, "status": cv["status"]}), flush=True)

    if mode in ("docker", "docker-preflight"):
        main_docker(token, arm, amd, mode == "docker-preflight")
        failed = [r for r in RESULTS if r["result"] != "pass"]
        print(json.dumps({"cases": len(RESULTS), "failed": len(failed)}))
        raise SystemExit(1 if failed else 0)

    # --- Preflight refusals: nothing is created anywhere. ---
    expect_refusal("bad_host_key", token, amd, install_body(amd, new(), fingerprint=arm.fingerprint("ED25519")), "host_trust_failed")
    expect_refusal("wrong_architecture", token, amd, install_body(amd, new(), pin_arch="arm64"), "host_architecture_mismatch")
    expect_refusal("missing_root_access", token, amd, install_body(amd, new(), key_file="unauthorized_key"), "host_auth_failed")
    rc, _ = arm.sudo("mv /usr/local/bin/phoenix-probe-admin /usr/local/bin/phoenix-probe-admin.hidden")
    try:
        expect_refusal("missing_helper", token, arm, install_body(arm, new()), "host_helper_unavailable")
    finally:
        arm.sudo("mv /usr/local/bin/phoenix-probe-admin.hidden /usr/local/bin/phoenix-probe-admin")

    # --- Clean host, systemd path, ED25519 pin, through activation. ---
    op_a = new()
    view = install_ready(token, amd, op_a)
    act = activate(token, amd, op_a) if view["status"] == "resources_ready" else {}
    count, active, listeners = host_resources(amd, op_a)
    record("clean_host_install_and_activate", amd, view["status"] == "resources_ready" and act.get("status") == "active"
           and count == 3 and active == "active" and listeners == 1,
           install=view["status"], activation=act.get("status"), probe_id=act.get("probe_id"), host_state=[count, active, listeners])

    # --- Existing install refused; then cleanup of an active probe frees the host. ---
    expect_refusal("existing_install", token, amd, install_body(amd, new()), "host_existing_install")
    cv = cleanup(token, amd, op_a)
    count, active, listeners = host_resources(amd, op_a)
    record("cleanup_active_probe", amd, cv["status"] == "cleaned" and count == 0 and active != "active" and listeners == 0,
           cleanup=cv["status"], host_state=[count, active, listeners])
    op_a2 = new()
    view = install_ready(token, amd, op_a2)
    cv = cleanup(token, amd, op_a2) if view["status"] == "resources_ready" else {}
    record("reinstall_after_cleanup", amd, view["status"] == "resources_ready" and cv.get("status") == "cleaned",
           install=view["status"], cleanup=cv.get("status"))

    # --- Interrupted install: cancel after the first resource applied. ---
    op_b = new()
    status, view = call("POST", "/api/probe-installs", token, install_body(arm, op_b))
    deadline = time.time() + 120
    while time.time() < deadline:
        _, view = call("GET", f"/api/probe-installs/{op_b}", token)
        if view.get("resources") and view["resources"][0]["state"] == "applied":
            break
        time.sleep(0.2)
    call("POST", f"/api/probe-installs/{op_b}/cancel", token)
    view = poll(f"/api/probe-installs/{op_b}", token, {"resources_ready", "failed", "canceled", "interrupted"})
    partial, _, _ = host_resources(arm, op_b)
    refused, refusal = call("POST", "/api/probe-installs", token, install_body(arm, new()))
    cv = cleanup(token, arm, op_b)
    after, _, _ = host_resources(arm, op_b)
    record("interrupted_install", arm, view["status"] in ("canceled", "interrupted") and 1 <= partial < 3
           and refused == 422 and refusal.get("code") == "host_existing_install" and cv["status"] == "cleaned" and after == 0,
           install=view["status"], failed_phase=view.get("failed_phase"), partial_resources=partial,
           second_install=[refused, refusal.get("code")], cleanup=cv["status"], after=after)

    # --- Inaccessible runtime port: enrollment never sends the token. ---
    op_c = new()
    view = install_ready(token, arm, op_c)
    arm.sudo("iptables -I INPUT -p tcp --dport 8443 -j DROP")
    try:
        act = activate(token, arm, op_c)
    finally:
        arm.sudo("iptables -D INPUT -p tcp --dport 8443 -j DROP")
    steps = {s["kind"]: s["state"] for s in act.get("steps", [])}
    record("inaccessible_runtime_port", arm, act.get("status") == "failed" and steps.get("enrollment") == "not_started"
           and act.get("failed_phase") == "enrollment",
           activation=act.get("status"), failed_phase=act.get("failed_phase"), steps=steps)
    cv = cleanup(token, arm, op_c)
    count, active, listeners = host_resources(arm, op_c)
    record("cleanup_after_failed_activation", arm, cv["status"] == "cleaned" and count == 0 and listeners == 0,
           cleanup=cv["status"], host_state=[count, active, listeners])

    # --- Clean arm64 host through activation with an ECDSA pin. ---
    op_d = new()
    body = install_body(arm, op_d, fingerprint=arm.fingerprint("ECDSA"))
    status, view = call("POST", "/api/probe-installs", token, body)
    view = poll(f"/api/probe-installs/{op_d}", token, {"resources_ready", "failed", "canceled", "interrupted"})
    act = activate(token, arm, op_d) if view["status"] == "resources_ready" else {}
    count, active, listeners = host_resources(arm, op_d)
    record("clean_host_install_and_activate", arm, view["status"] == "resources_ready" and act.get("status") == "active"
           and count == 3 and active == "active" and listeners == 1,
           install=view["status"], activation=act.get("status"), probe_id=act.get("probe_id"), host_state=[count, active, listeners])
    cv = cleanup(token, arm, op_d)
    record("final_cleanup", arm, cv["status"] == "cleaned", cleanup=cv["status"])

    failed = [r for r in RESULTS if r["result"] != "pass"]
    print(json.dumps({"cases": len(RESULTS), "failed": len(failed)}))
    raise SystemExit(1 if failed else 0)


if __name__ == "__main__":
    main()
