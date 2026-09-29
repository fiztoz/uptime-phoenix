#!/usr/bin/env python3
"""Rehearse migration 037 on a populated, isolated MariaDB container.

Requires a cached mariadb:11 image and Docker. The container has no network and
no published ports. Never points at an operator-supplied database. Only removes
its own uniquely named container, and prints no credentials.
"""

import argparse
import json
from pathlib import Path
import subprocess
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
MIGRATIONS = ROOT / "internal/adapters/repository/mariadb/migrations"
DATABASE = "phoenix_m6_rehearsal"


def docker(*args, input_text=None, check=True):
    result = subprocess.run(
        ["docker", *args], input=input_text, text=True, capture_output=True, check=False
    )
    if check and result.returncode:
        raise RuntimeError(f"docker command failed ({args[0]}): {result.stderr[-1200:]}")
    return result


class Rehearsal:
    def __init__(self, name):
        self.name = name
        self.samples = []
        self.stop_sampling = threading.Event()
        self.sample_thread = None

    def sql(self, text, *, check=True):
        result = docker(
            "exec", "-i", self.name, "mariadb", "-uroot", f"--database={DATABASE}",
            "--batch", "--skip-column-names", input_text=text, check=False
        )
        if check and result.returncode:
            raise RuntimeError(f"SQL failed: {result.stderr[-1200:]}")
        return result

    def query(self, sql):
        return self.sql(sql).stdout.strip()

    def disk_kib(self):
        return int(docker("exec", self.name, "du", "-sk", "/var/lib/mysql").stdout.split()[0])

    def sample_disk(self):
        while not self.stop_sampling.is_set():
            try:
                self.samples.append(self.disk_kib())
            except (RuntimeError, ValueError):
                pass
            self.stop_sampling.wait(0.2)

    def start_sampler(self):
        self.stop_sampling.clear()
        self.sample_thread = threading.Thread(target=self.sample_disk, daemon=True)
        self.sample_thread.start()

    def stop_sampler(self):
        self.stop_sampling.set()
        self.sample_thread.join(timeout=5)

    def apply(self, path):
        # The MariaDB client handles semicolon-delimited SQL exactly as checked in.
        return self.sql(path.read_text())


def seed(r, rows):
    r.sql("INSERT INTO users (username, password_hash) VALUES ('rehearsal', 'test-only');")
    # Spread the sample across two existing monthly partitions; the monitor/user
    # rows and each of the three legacy rollup resolutions predate migration 037.
    r.sql("INSERT INTO monitors (user_id, name, type, config) VALUES " + ",".join(
        f"(1, 'sample-{i}', 'http', '{{}}')" for i in range(1, 101)
    ) + ";")
    base = "2026-09-15 12:00:00"
    for table in ("heartbeat_1m", "heartbeat_1h", "heartbeat_1d"):
        for start in range(0, 1000, 500):
            values = []
            for i in range(start, start + 500):
                # DISTINCT (monitor, bucket); rollups do not need 100k rows to
                # validate the index replacement and identity backfill.
                monitor = i % 100 + 1
                minute = i // 100
                values.append(f"({monitor}, '2026-09-15 12:{minute:02d}:00', 1, 1)")
            r.sql(f"INSERT INTO {table} (monitor_id, bucket, up_count, total_checks) VALUES "
                  + ",".join(values) + ";")
    for start in range(0, rows, 500):
        values = []
        for i in range(start, min(start + 500, rows)):
            stamp = "2026-08-15 12:00:00" if i % 2 else base
            values.append(f"({i % 100 + 1}, 1, '{stamp}', 'synthetic', 12, 12)")
        r.sql("INSERT INTO heartbeats (monitor_id, status, time, msg, ping, duration) VALUES "
              + ",".join(values) + ";")


def run(rows, hold_seconds):
    name = "phoenix-m6-rehearsal-" + uuid.uuid4().hex[:10]
    r = Rehearsal(name)
    # Empty root password is safe ONLY because this disposable container has no
    # network and no published ports. Never use this configuration on a host DB.
    docker("run", "-d", "--name", name, "--network", "none", "--cpus", "2",
           "--memory", "2g", "--tmpfs", "/var/lib/mysql:rw,size=1536m",
           "-e", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1", "mariadb:11")
    try:
        for _ in range(90):
            if docker("exec", name, "mariadb", "-uroot", "-N", "-e", "SELECT 1", check=False).returncode == 0:
                # Entrypoint first starts a temporary init server, then shuts
                # it down. Do not treat that transient server as ready.
                time.sleep(3)
                if docker("exec", name, "mariadb", "-uroot", "-N", "-e", "SELECT 1", check=False).returncode == 0:
                    break
            time.sleep(1)
        else:
            raise RuntimeError("disposable MariaDB did not become ready")
        version = docker("exec", name, "mariadb", "-uroot", "-N", "-e", "SELECT VERSION()").stdout.strip()
        docker("exec", name, "mariadb", "-uroot", "-e", f"CREATE DATABASE {DATABASE}")
        for path in sorted(MIGRATIONS.glob("*.up.sql")):
            if int(path.name[:3]) >= 37:
                break
            r.apply(path)
        seed(r, rows)
        before_count = int(r.query("SELECT COUNT(*) FROM heartbeats"))
        before_ids = r.query("SELECT MIN(id), MAX(id) FROM heartbeats")
        legacy_rollups = {table: r.query(f"SELECT COUNT(*), SUM(id) FROM {table}")
                          for table in ("heartbeat_1m", "heartbeat_1h", "heartbeat_1d")}
        before_disk = r.disk_kib()
        partition_before = r.query("SELECT GROUP_CONCAT(PARTITION_NAME ORDER BY PARTITION_ORDINAL_POSITION) "
                                   "FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() "
                                   "AND TABLE_NAME='heartbeats'")
        # Optionally hold a read transaction; this is a controlled metadata-
        # lock probe, NOT a concurrent-writer rehearsal. Stop production writers.
        holder = None
        try:
            if hold_seconds:
                holder = subprocess.Popen(
                    ["docker", "exec", "-i", name, "mariadb", "-uroot", f"--database={DATABASE}",
                     "--batch", "--skip-column-names"], stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1
                )
                holder.stdin.write("START TRANSACTION; SELECT COUNT(*) FROM heartbeats; ")
                holder.stdin.write(f"SELECT SLEEP({hold_seconds}); COMMIT;\n")
                holder.stdin.flush()
                # MariaDB CLI stdout can buffer when piped. Confirm via server
                # processlist that the sleeper is running before starting DDL.
                for _ in range(60):
                    state = r.query("SELECT COUNT(*) FROM information_schema.PROCESSLIST "
                                    f"WHERE INFO LIKE 'SELECT SLEEP({hold_seconds})%'")
                    if int(state):
                        break
                    time.sleep(0.05)
                else:
                    raise RuntimeError("metadata-lock holder never started")
            r.start_sampler()
            started = time.monotonic()
            migration = MIGRATIONS / "037_probe_heartbeat.up.sql"
            # One process runs all statements, matching their committed order.
            # Poll the server while the migration is executing, not afterwards.
            with subprocess.Popen(
                ["docker", "exec", "-i", name, "mariadb", "-uroot", f"--database={DATABASE}",
                 "--batch", "--skip-column-names"], stdin=subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True
            ) as ddl:
                ddl.stdin.write(migration.read_text())
                ddl.stdin.close()
                saw_wait = False
                while ddl.poll() is None:
                    waiting = r.query("SELECT COUNT(*) FROM information_schema.PROCESSLIST "
                                      "WHERE STATE LIKE '%metadata lock%'")
                    saw_wait |= int(waiting) > 0
                    time.sleep(0.1)
                error = ddl.stderr.read()
                if ddl.returncode:
                    raise RuntimeError(f"037 upgrade failed: {error[-1200:]}")
            elapsed = time.monotonic() - started
        finally:
            if r.sample_thread:
                r.stop_sampler()
            if holder is not None and holder.poll() is None:
                holder.communicate(timeout=hold_seconds + 12)
        assert saw_wait == (hold_seconds > 0), "unexpected metadata-lock behavior"
        after_disk = r.disk_kib()
        after_count = int(r.query("SELECT COUNT(*) FROM heartbeats"))
        assert before_count == rows == after_count
        assert r.query("SELECT MIN(id), MAX(id) FROM heartbeats") == before_ids
        assert r.query("SELECT COUNT(*) FROM heartbeats WHERE probe_id <> 'local' "
                       "OR assignment_generation <> 1 OR config_revision <> 0") == "0"
        assert r.query("SELECT COUNT(*) FROM heartbeats WHERE time < '2026-09-01'") == str(rows // 2)
        assert partition_before == r.query(
            "SELECT GROUP_CONCAT(PARTITION_NAME ORDER BY PARTITION_ORDINAL_POSITION) "
            "FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() "
            "AND TABLE_NAME='heartbeats'")
        ddl = r.query("SHOW CREATE TABLE heartbeats").replace("`", "").lower()
        assert "partition by range (unix_timestamp(time))" in ddl
        for table, expected in legacy_rollups.items():
            assert r.query(f"SELECT COUNT(*), SUM(id) FROM {table}") == expected
            assert r.query(f"SELECT COUNT(*) FROM {table} WHERE probe_id <> 'local' "
                           "OR unknown_count <> 0") == "0"
            keys = r.query("SELECT GROUP_CONCAT(DISTINCT INDEX_NAME ORDER BY INDEX_NAME) "
                           "FROM information_schema.STATISTICS "
                           f"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='{table}'")
            assert "uq_monitor_probe_bucket" in keys.split(",")
            assert "uq_monitor_bucket" not in keys.split(",")
            assert r.query("SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) "
                           "FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() "
                           f"AND TABLE_NAME='{table}' AND INDEX_NAME='uq_monitor_probe_bucket' "
                           "AND NON_UNIQUE=0") == "monitor_id,probe_id,bucket"
        assert r.query("SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) "
                       "FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() "
                       "AND TABLE_NAME='heartbeats' AND INDEX_NAME='idx_hb_monitor_probe_time'") == "monitor_id,probe_id,time,id"
        assert r.query("SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) "
                       "FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() "
                       "AND TABLE_NAME='heartbeats' AND INDEX_NAME='PRIMARY'") == "id,time"
        partition_count = len(partition_before.split(","))
        assert partition_count >= 2 and "pmax" in partition_before
        # A new probe can share a legacy bucket, without changing its old ID.
        r.sql("INSERT INTO heartbeat_1m (monitor_id, probe_id, bucket, up_count, total_checks) "
              "SELECT monitor_id, 'remote-test', bucket, 1, 1 FROM heartbeat_1m WHERE id=1")
        assert r.query("SELECT COUNT(DISTINCT probe_id), COUNT(DISTINCT id) FROM heartbeat_1m "
                       "WHERE monitor_id=1 AND bucket='2026-09-15 12:00:00'") == "2\t2"
        assert int(r.query("SELECT id FROM heartbeat_1m WHERE probe_id='remote-test'")) > 1000
        refusal = r.sql((MIGRATIONS / "037_probe_heartbeat.down.sql").read_text(), check=False)
        assert refusal.returncode != 0 and "CONSTRAINT" in refusal.stderr.upper(), refusal.stderr[-1200:]
        assert r.query("SELECT COUNT(*) FROM heartbeat_1m WHERE probe_id='remote-test'") == "1"
        assert r.query("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() "
                       "AND TABLE_NAME='heartbeats' AND COLUMN_NAME='probe_id'") == "1"
        report = {
            "migration": "037_probe_heartbeat", "image": "mariadb:11", "version": version,
            "rows": rows, "partitions": partition_count, "rollups_each": 1000,
            "container_limits": {"cpu": 2, "memory_bytes": 2 * 1024**3,
                                 "data_tmpfs_bytes": 1536 * 1024**2},
            "elapsed_seconds": round(elapsed, 3), "held_read_seconds": hold_seconds,
            "metadata_lock_wait_observed": saw_wait,
            "disk_kib_before": before_disk, "disk_kib_after": after_disk,
            "sampled_peak_disk_kib": max(before_disk, after_disk, *r.samples),
            "disk_samples": len(r.samples),
            "backfill_and_id_preservation": "passed", "indexes_and_partitioning": "passed",
            "remote_bucket_coexistence": "passed", "remote_downgrade_refused": "passed",
            "limitations": "Observed disk peak is sampled, not guaranteed true peak; controlled read lock only; synthetic data; raw checked-in SQL through MariaDB client, not Go RunMigrations; no production approval."
        }
        print(json.dumps(report, indent=2))
    finally:
        docker("rm", "-f", name, check=False)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rows", type=int, default=100_000)
    parser.add_argument("--hold-reader-seconds", type=int, default=3)
    args = parser.parse_args()
    if args.rows < 2 or args.rows % 2 or not 0 <= args.hold_reader_seconds <= 10:
        parser.error("--rows must be even and >= 2; --hold-reader-seconds must be 0..10")
    run(args.rows, args.hold_reader_seconds)
