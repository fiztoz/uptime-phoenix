#!/usr/bin/env python3
"""Run a public 0.4.5 -> working-tree upgrade on disposable MariaDB.

No external database, deployed cluster, or host-bound MariaDB port is used.
Requires cached Docker images and cached Go dependencies. All data is synthetic.
"""

import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
RELEASE = "ghcr.io/fiztoz/uptime-phoenix:0.4.5"
BASE = "gcr.io/distroless/static-debian12:nonroot"
ROWS = 100_000
DATABASE = "phoenix"  # Chart-managed MariaDB default.


def docker(*args, input_text=None):
    p = subprocess.run(["docker", *args], input=input_text, text=True,
                       capture_output=True, timeout=180, check=False)
    if p.returncode:
        raise RuntimeError(f"docker {args[0]} failed (exit {p.returncode})")
    return p.stdout.strip()


def sql(db, password, statement):
    return docker("exec", "-i", "-e", "MYSQL_PWD=" + password, db,
                  "mariadb", "-uphoenix", "--batch", "--skip-column-names",
                  "--database=" + DATABASE, input_text=statement)


def wait_ready(container):
    port = int(docker("port", container, "3000/tcp").split(":")[-1])
    url = f"http://127.0.0.1:{port}/api/health/ready"
    start = time.monotonic()
    for _ in range(120):
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    base = f"http://127.0.0.1:{port}"
                    return base, round(time.monotonic() - start, 2)
        except (OSError, urllib.error.HTTPError):
            pass
        if docker("inspect", "--format", "{{.State.Running}}", container) != "true":
            raise RuntimeError("app exited before readiness")
        time.sleep(1)
    raise RuntimeError("app did not become ready")


def api(base, method, path, body=None, bearer=None):
    payload = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if bearer is not None:
        headers["Authorization"] = "Bearer " + bearer
    request = urllib.request.Request(base + path, data=payload,
                                     method=method, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as exc:
        return exc.code, None  # Never echo the token or response body.


def seed(db, password):
    sql(db, password, "INSERT INTO monitors (user_id,name,type,config,active) VALUES " + ",".join(
        f"(1,'upgrade-{n:03d}','http','{{}}',0)" for n in range(100)
    ))
    for table in ("heartbeat_1m", "heartbeat_1h", "heartbeat_1d"):
        for start in (0, 500):
            vals = [f"({n % 100 + 1},'2026-09-15 12:{n // 100:02d}:00',1,1)"
                    for n in range(start, start + 500)]
            sql(db, password, f"INSERT INTO {table} (monitor_id,bucket,up_count,total_checks) VALUES "
                + ",".join(vals))
    for start in range(0, ROWS, 500):
        vals = []
        for n in range(start, start + 500):
            stamp = "2026-08-15 12:00:00" if n % 2 else "2026-09-15 12:00:00"
            vals.append(f"({n % 100 + 1},1,'{stamp}','upgrade-fixture',12,12)")
        sql(db, password, "INSERT INTO heartbeats (monitor_id,status,time,msg,ping,duration) VALUES "
            + ",".join(vals))


def snapshot(db, password):
    queries = {
        "ledger": "SELECT COUNT(*),MIN(filename),MAX(filename) FROM _migrations",
        "users": "SELECT COUNT(*) FROM users",
        "monitors": "SELECT COUNT(*),SUM(id) FROM monitors",
        "heartbeats": "SELECT COUNT(*),MIN(id),MAX(id),SUM(id) FROM heartbeats",
        "august": "SELECT COUNT(*) FROM heartbeats WHERE time < '2026-09-01'",
        "one_minute": "SELECT COUNT(*),SUM(id) FROM heartbeat_1m",
        "one_hour": "SELECT COUNT(*),SUM(id) FROM heartbeat_1h",
        "one_day": "SELECT COUNT(*),SUM(id) FROM heartbeat_1d",
        "partitions": "SELECT GROUP_CONCAT(PARTITION_NAME ORDER BY PARTITION_ORDINAL_POSITION) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='heartbeats'",
        "sample": "SELECT id,monitor_id,status,time FROM heartbeats ORDER BY id,time LIMIT 4",
    }
    return {key: sql(db, password, statement) for key, statement in queries.items()}


def main():
    host = os.environ.get("DOCKER_HOST") or docker(
        "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"
    )
    if not host.startswith("unix://"):
        raise RuntimeError("local Unix Docker socket required")
    for image in (RELEASE, BASE, "mariadb:11"):
        docker("image", "inspect", image)  # No image pull on demand.
    arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(
        docker("info", "--format", "{{.Architecture}}"))
    if arch is None:
        raise RuntimeError("unsupported Docker architecture")
    prefix = "phoenix-m6-upgrade-" + uuid.uuid4().hex[:8]
    net, db, old, new = (prefix + suffix for suffix in ("-net", "-db", "-old", "-new"))
    network_created = False
    # Disposable app-user auth mirrors the chart; never read operator secrets.
    password = secrets.token_urlsafe(24)
    connection = f"phoenix:{password}@tcp({db}:3306)/{DATABASE}?parseTime=true&loc=UTC&multiStatements=true"
    app_env = ["-e", "DB_ENGINE=mariadb", "-e", "DB_DSN=" + connection,
               "-e", "MODE=all", "-e", "PROBES_ENABLED=false"]
    try:
        docker("network", "create", net)
        network_created = True
        docker("run", "-d", "--name", db, "--network", net, "--network-alias", db,
               "--memory", "1g", "--cpus", "2", "-e", "MARIADB_RANDOM_ROOT_PASSWORD=yes",
               "-e", "MARIADB_DATABASE=" + DATABASE, "-e", "MARIADB_USER=phoenix",
               "-e", "MARIADB_PASSWORD=" + password, "mariadb:11",
               "--character-set-server=utf8mb4", "--collation-server=utf8mb4_unicode_ci",
               "--default-time-zone=+00:00", "--skip-name-resolve",
               "--max-connections=200", "--innodb-buffer-pool-size=256M")
        for _ in range(90):
            probe = subprocess.run(["docker", "exec", "-e", "MYSQL_PWD=" + password,
                                    db, "mariadb", "-uphoenix", "-N", "-e",
                                    "SELECT 1", DATABASE],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   check=False)
            if probe.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError("disposable MariaDB did not become ready")
        docker("run", "-d", "--name", old, "--network", net, "--memory", "512m",
               "-p", "127.0.0.1::3000", *app_env, RELEASE)
        release_url, release_boot = wait_ready(old)
        if sql(db, password, "SELECT COUNT(*) FROM _migrations") != "34":
            raise RuntimeError("release did not apply exactly migrations 001-034")
        ephemeral = uuid.uuid4().hex + "-local-only"
        status, registered = api(release_url, "POST", "/api/auth/register", {
            "username": "upgrade-fixture", "password": ephemeral,
        })
        if (status != 201 or not isinstance(registered, dict) or not registered.get("token")
                or not isinstance(registered.get("user"), dict) or registered["user"].get("id") != 1):
            raise RuntimeError(f"old release registration failed: HTTP {status}")
        token = registered["token"]
        seed(db, password)
        status, old_monitors = api(release_url, "GET", "/api/monitors", bearer=token)
        if status != 200 or not isinstance(old_monitors, list) or len(old_monitors) != 100:
            raise RuntimeError(f"release could not serve seeded monitors: HTTP {status}")
        before = snapshot(db, password)
        if not before["heartbeats"].startswith(f"{ROWS}\t"):
            raise RuntimeError("release schema did not retain 100k historical rows")
        before_disk = int(docker("exec", db, "du", "-sk", "/var/lib/mysql").split()[0])
        docker("stop", "-t", "10", old)
        with tempfile.TemporaryDirectory(prefix="phoenix-m6-upgrade-") as tmp:
            binary = Path(tmp) / "uptime-phoenix-head"
            allowed = {"PATH", "HOME", "TMPDIR", "GOPATH", "GOMODCACHE",
                       "GOCACHE", "LANG", "LC_ALL", "XDG_CACHE_HOME"}
            env = {key: value for key, value in os.environ.items() if key in allowed}
            env.update(GOTOOLCHAIN="local", GOOS="linux", GOARCH=arch,
                       CGO_ENABLED="0", GOPROXY="off", GOSUMDB="off")
            built = subprocess.run(["go", "build", "-trimpath", "-o", str(binary),
                                    "./cmd/app"], cwd=ROOT, env=env, capture_output=True,
                                   text=True, check=False, timeout=600)
            if built.returncode:
                raise RuntimeError("working-tree app did not build offline")
            binary_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
            docker("create", "--name", new, "--network", net, "--memory", "512m",
                   "-p", "127.0.0.1::3000", *app_env,
                   "--entrypoint", "/uptime-phoenix-head", BASE)
            docker("cp", str(binary), f"{new}:/uptime-phoenix-head")
            docker("start", new)
            next_url, upgrade_boot = wait_ready(new)
            after = snapshot(db, password)
            if after["ledger"].split("\t")[0] != "74" or not after["ledger"].endswith("074_hub_worker_capabilities.up.sql"):
                raise RuntimeError("app did not apply migrations 035-074")
            for key in ("users", "monitors", "heartbeats", "august", "one_minute",
                        "one_hour", "one_day", "partitions", "sample"):
                if before[key] != after[key]:
                    raise RuntimeError(f"upgrade changed persisted {key}")
            if sql(db, password, "SELECT COUNT(*) FROM heartbeats WHERE probe_id <> 'local'") != "0":
                raise RuntimeError("legacy heartbeats were not backfilled as local")
            status, login = api(next_url, "POST", "/api/auth/login", {
                "username": "upgrade-fixture", "password": ephemeral,
            })
            if status != 200 or not isinstance(login, dict) or not login.get("token"):
                raise RuntimeError(f"upgraded app could not authenticate release user: HTTP {status}")
            status, monitors = api(next_url, "GET", "/api/monitors", bearer=login["token"])
            if status != 200 or not isinstance(monitors, list) or {m.get("id") for m in monitors} != {m.get("id") for m in old_monitors}:
                raise RuntimeError(f"upgraded app could not serve release monitors: HTTP {status}")
            # Old and new regions must now share a bucket without overwriting
            # an old rollup or reusing an AUTO_INCREMENT identity.
            for table in ("heartbeat_1m", "heartbeat_1h", "heartbeat_1d"):
                index = sql(db, password, "SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) "
                            "FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() "
                            f"AND TABLE_NAME='{table}' AND INDEX_NAME='uq_monitor_probe_bucket' "
                            "AND NON_UNIQUE=0")
                if index != "monitor_id,probe_id,bucket":
                    raise RuntimeError(f"{table} lost the regional unique index")
                previous = int(sql(db, password, f"SELECT MAX(id) FROM {table}"))
                sql(db, password, f"INSERT INTO {table} (monitor_id,probe_id,bucket,up_count,total_checks) "
                        f"SELECT monitor_id,'remote-test',bucket,1,1 FROM {table} ORDER BY id LIMIT 1")
                if (sql(db, password, f"SELECT COUNT(*) FROM {table} WHERE probe_id='local'") != "1000"
                        or int(sql(db, password, f"SELECT MAX(id) FROM {table}")) <= previous):
                    raise RuntimeError(f"{table} did not preserve legacy IDs/rows")
            after_remote = snapshot(db, password)
            docker("stop", "-t", "10", new)
            docker("start", new)
            wait_ready(new)
            if snapshot(db, password) != after_remote:
                raise RuntimeError("upgrade reran or rows changed on restart")
            after_disk = int(docker("exec", db, "du", "-sk", "/var/lib/mysql").split()[0])
            print(json.dumps({
                "result": "passed", "from": "published 0.4.5 image",
                "to": "working-tree cmd/app Linux binary", "ledger": "34 -> 74",
                "app_migrations_applied": 40, "monitors": 100, "heartbeats": ROWS,
                "release_boot_seconds": release_boot, "upgrade_boot_seconds": upgrade_boot,
                "data_dir_kib_before": before_disk, "data_dir_kib_after": after_disk,
                "peak_disk_measured": False, "restart_noop": True,
                "legacy_probe_id_local": True, "binary_sha256": binary_hash,
                "release_user_login_and_monitors_after_upgrade": True,
                "three_rollup_index_and_remote_insert_checks": True,
                "canary_deployed": False, "production_sized": False,
            }, indent=2))
    finally:
        # An unsuccessful Docker run/create may still have created a named
        # container. Inspect our own unique names, not just successful calls.
        for container in (new, old, db):
            if subprocess.run(["docker", "container", "inspect", container],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              check=False).returncode == 0:
                subprocess.run(["docker", "rm", "-f", container],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               check=False)
        if network_created:
            subprocess.run(["docker", "network", "rm", net],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           check=False)
        for name in (db, old, new):
            if subprocess.run(["docker", "container", "inspect", name],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              check=False).returncode == 0:
                raise RuntimeError("disposable container cleanup failed")
        if network_created and subprocess.run(
            ["docker", "network", "inspect", net], stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, check=False
        ).returncode == 0:
            raise RuntimeError("disposable network cleanup failed")


if __name__ == "__main__":
    main()
