# M6: local-scale migration rehearsal, v0.4.5 → main (20M, 50M and 259M heartbeats)

Status: **local-scale engineering evidence, not the production-sized gate.** These runs used a box's local disk, not storage comparable to a deployment, and synthetic rows. The production size and storage are owner decisions that are still open. The M6 checkbox in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) §9 stays unticked.

Runs r1–r3: 2026-11-09, 22:29–23:02 ICT, at main `8e14335`. The 259M runs are in their own section below. No cloud resources were used.

## Setup

- **Server.** MariaDB 11.8.3 (official tarball) in user space on loopback. It had its own data directory and its own `tmpdir`/`innodb_tmpdir`, so both could be sampled. Buffer pool 2 GiB, `--default-time-zone=+00:00`. Host: 8 vCPU, 15 GiB RAM, local virtio disk.
- **Old schema.** A small Go main inside a `v0.4.5` worktree calls `repository.RunMigrations(db, "mariadb")`. That produces the released v0.4.5 schema: 34 migrations, with `heartbeats` partitioned by `RANGE (UNIX_TIMESTAMP(time))` in 14 partitions.
- **Seed.** 500 `http` monitors and N heartbeats spread over 150 days (Jun 12 – Nov 9 2026). Statuses are 98% up, 1% down and 1% pending, with a short or long `msg`. Rollups: `heartbeat_1m` has 2 days × 500 monitors (1.44M rows), `heartbeat_1h` has 150 days (1.8M) and `heartbeat_1d` has 150 days (75k).
- **Upgrade.** The same Go main inside a `main` worktree runs the real runner: 60 migrations, `035` → `094`, including the resumable `037` path. Per-migration time comes from the runner's own `Applying database migration` log lines.
- **Load during the upgrade.** One writer inserts a heartbeat every 200 ms. One reader runs the dashboard-shaped query (`WHERE monitor_id=? ORDER BY time DESC LIMIT 50`) every 200 ms. `information_schema.PROCESSLIST` is sampled every 250 ms, and the data dir plus tmpdir every 1 s. Run r2 also holds a read transaction on `heartbeats` for the first 30 s. The upgrade starts at t = 10 s.
- **Checks after the upgrade.**
  - `COUNT(*)` and `SUM(id)` of the seeded rows (`id ≤` the max seeded id).
  - Per-month count and `SUM(id)`.
  - Rollup counts and `SUM(id)`.
  - Partition count and expression.
  - Every index and its columns.
  - `CHECK TABLE … EXTENDED` on all four tables.
- **Rollback refusal.** Insert one remote `heartbeat_1m` row and one remote heartbeat, then apply `037_probe_heartbeat.down.sql` with the MariaDB client.

## Results

| Run | Heartbeats | Upgrade (60 migrations) | `037` | `042` | Writer / reader p95 | Writer / reader max | Errors | Data dir before → after |
|---|---:|---:|---:|---:|---|---|---:|---|
| r1 | 20M | 50.5 s | 40.6 s | 9.2 s | 19 / 22 ms | 486 / 510 ms | 0 | 3,296 → 4,007 MiB (+711) |
| r2, 30 s held reader | 20M | 59.4 s | 52.2 s | 6.8 s | 8 / 14 ms | **19,855 / 19,857 ms** | 0 | 3,305 → 4,013 MiB (+708) |
| r3 | 50M | 113.8 s | 92.1 s | 21.0 s | 13 / 46 ms | 248 / 327 ms | 0 | 6,753 → 8,345 MiB (+1,592) |

Every other migration took under 0.3 s.

`tmpdir` stayed at 0 bytes in every sample, so the growth is the new index and columns, written in place. Disk was sampled once a second. The peak is a lower bound, but it equals the final size, and no shrink was seen.

**Data and index validity: pass in all three runs.**
- The seeded heartbeat count and `SUM(id)` are identical before and after: 20,000,000 / 200000010000000, and 50,000,000 / 1250000025000000.
- Every month before November is identical. November differs only by the writer's rows: +351, +299 and +669.
- Every row has `probe_id='local'` and `stream_id IS NULL`.
- The table keeps 14 partitions, `RANGE unix_timestamp(time)` and `PRIMARY KEY (id,time)`.
- `idx_hb_monitor_probe_time (monitor_id,probe_id,time,id)` exists.
- Each rollup has `uq_monitor_probe_bucket` and no `uq_monitor_bucket`.
- `CHECK TABLE EXTENDED` returns OK for all four tables.

**Rollback refusal: pass.** The `037` down fails at its guard with `ERROR 4025 … CONSTRAINT probe_heartbeat_down_guard.state_count failed`. That took 6.5 s at 20M and 20.2 s at 50M, because the guard counts rows with full scans. Afterwards, the remote rows, all six new heartbeat columns, `uq_monitor_probe_bucket` and `idx_hb_monitor_probe_time` are still there.

Scaling from 20M to 50M is close to linear: about 2.3 s per million heartbeats for the whole upgrade, and about 32 MiB per million of disk growth. That is an extrapolation, not a measurement. Storage comparable to a deployment (network block storage) can be much slower.

## 259M heartbeats (D3 row count), MariaDB and SQLite

Run 2026-11-10, 06:26–10:49 ICT, on the same box, with the same seed shape (500 monitors, 150 days, same rollups) and the same load (writer + reader at 5 Hz each, upgrade starts at t = 10 s). The upgrade runner is main `8e14335`. No migration file or migration runner code changed between `8e14335` and main `3759028`. The DB work was pinned to 4 of the 8 vCPUs.

| Engine | Seeded size | Upgrade (60 migrations) | `037` | `042` | Writer ok / attempts | Writer p95 / max | Reader ok / attempts | Reader p95 / max | Size after |
|---|---:|---:|---:|---:|---:|---|---:|---|---:|
| MariaDB 11.8.3 | 30.8 GB data dir | 601.6 s | 479.5 s | 120.6 s | 3,101 / 3,101 | 17 / 1,008 ms | 3,102 / 3,102 | 53 / 874 ms | 46.3 GB (peak 53.8 GB) |
| SQLite (WAL, `busy_timeout` 10 s) | 36.0 GB file | 521.3 s | 487.0 s | 23.0 s | **265 / 313** | 29 / 4,266 ms | 2,704 / 2,704 | 6 / 114 ms | 49.0 GB (peak 42.0 GB file + 13.4 GB WAL) |

Every other migration took under 1.1 s on MariaDB. On SQLite, `057` took 10.2 s and the rest under 1 s. Both upgrades exited 0.

**MariaDB validity: pass.**
- Seeded `COUNT(*)` and `SUM(id)` are identical before and after: 259,000,000 / 33540500129500000.
- June to October are identical per month. November differs only by the writer's 3,101 rows.
- Every row has `probe_id='local'` and `stream_id IS NULL`. The table keeps 14 partitions, `RANGE unix_timestamp(time)`.
- `idx_hb_monitor_probe_time` exists. Each rollup has `uq_monitor_probe_bucket` and no `uq_monitor_bucket`.
- `CHECK TABLE heartbeats` (default mode) returns OK in 687 s. `CHECK TABLE … EXTENDED` returns OK on the three rollups in 16 s. On `heartbeats`, `EXTENDED` was stopped after 105 min, partway through the second of six populated partitions. Its only error is 188 (`HA_ERR_ABORTED_BY_USER`) from that stop. It would need more than 5 h on this box.
- Rollback refusal: pass. The `037` down fails with `ERROR 4025 … probe_heartbeat_down_guard.state_count` after 183.3 s. The remote rows, the six columns and the new indexes are all still there.
- Disk: 30.8 → 53.8 GB at peak (+23 GB transient, `tmpdir` still 0) → 46.3 GB. Minimum free space on the host was 32 GB. Minimum `MemAvailable` was 2.3 GiB.

**SQLite validity: pass on every check below that completed.**
- Seeded `COUNT(*)` and `SUM(id)` are identical before and after: 259,000,000 / 33540500129500000.
- All six months are identical per month for seeded ids. There are 265 extra rows, equal to the writer's committed rows.
- The rollup counts and `SUM(id)` are identical: 1,440,000 / 1,800,000 / 75,000.
- All six new `heartbeats` columns exist with their defaults. `idx_hb_monitor_probe_time` and `idx_*_monitor_probe_bucket` exist on all three rollups. `_migrations` holds 94 rows, the last being `094_settle_orphaned_aggregate_attempts`.
- `PRAGMA quick_check` returned `ok` at 13:41 ICT on 2026-11-10.
- Disk: the file went from 36.0 GB to 49.0 GB. At peak, the file was 42.0 GB plus a 13.4 GB WAL; the WAL was checkpointed to 0 at the end. Minimum `MemAvailable` was 2.4 GiB.

**SQLite: heartbeat writes stop for most of the upgrade (finding 3).**

At 259M rows, no write committed from t = 0 to t = 451 s after the runner started, and again for 30 s around `042`. In total, 48 of 313 writer attempts failed with `database is locked`. Each of those waited the full 10 s `busy_timeout`. WAL readers were not affected.

The cause is in the runner:
- `applyMigrationWithExecutor` wraps each SQLite migration file in one transaction.
- `037` runs `CREATE INDEX idx_hb_monitor_probe_time` over all 259M rows inside that transaction, which holds SQLite's single write lock for the whole build (about 451 s).
- The `COMMIT` then checkpoints the 13 GB WAL (about 36 s). Writers can commit again during that checkpoint.
- `042` holds the write lock while it scans all of `heartbeats` for `MAX(source_seq) WHERE stream_id = …`, which has no usable index (30 s).

MariaDB did not stop writes at this size, because its `037` `ALTER` is online.

The owner chose to treat this as a **known limitation**: document downtime, and stop the process before a SQLite upgrade. That is now in [RUNBOOK](../RUNBOOK.md) §2.4. With a single SQLite process there is no separate writer anyway; the real effect is that the new binary does not serve or record checks until migrations finish. Plan for about 2 s of write downtime per million `heartbeats` rows on hardware like this box. Two other options were considered and not taken: a cheaper `042` (the `MAX(source_seq)` scan could skip the full table when every `source_seq` is still `NULL`), and moving the index build out of the upgrade path (SQLite has no online `CREATE INDEX`).

## Findings

1. **A long transaction on `heartbeats` stalls every heartbeat read and write during `037`.** In r2, the `ALTER TABLE heartbeats` waited for the metadata lock behind the held reader, from t ≈ 10 s to t = 30 s. While it waited, every new writer and reader queued behind the `ALTER`, and both maxed out at 19.9 s. That's standard MariaDB metadata-lock behaviour.
   - The runner sets no `lock_wait_timeout`, so the server default of 86,400 s applies.
   - [RUNBOOK](../RUNBOOK.md) §2.1 keeps one migration-owning API pod, but doesn't say to stop the old pods.
   - The chart's `RollingUpdate` with `maxUnavailable: 0` keeps the old pods serving while the new pod migrates.

   Without a long transaction (r1, r3), the worst stall was 0.5 s.
2. **An older binary does not refuse a newer schema.** The v0.4.5 runner against the upgraded `094` schema exits 0 and applies nothing. The RUNBOOK says never to mix an old binary with a partially upgraded schema, but nothing enforces that at startup.

3. **On SQLite at 259M rows, heartbeat writes stop for about 451 s during `037` and 30 s during `042`.** See the SQLite part of the 259M section for the cause. The owner chose to document this as a known limitation (downtime during the upgrade) rather than change `042` or move the index build.

Finding 1 is addressed by draft PR #254 (runbook scale-to-0, plus a capped migration lock wait with retry). Finding 2 is addressed by draft PR #255 (startup refusal of a newer schema). Finding 3 is recorded as a known limitation in [RUNBOOK](../RUNBOOK.md) §2.4 and in the section below.

## Known limitations

**SQLite upgrades need downtime.** Scale to 0 or stop the process before starting the new binary. At 259 million heartbeat rows, writes were refused for about 451 s during the `037` index rebuild and for another 30 s during `042`. Reads stayed fine. MariaDB was not affected. See [RUNBOOK](../RUNBOOK.md) §2.4.

## Not covered

- Storage comparable to a deployment, a production-shaped data distribution, payload sizes or retention. The 259M runs match the D3 row count, but only on local disk.
- A completed `CHECK TABLE heartbeats EXTENDED` at 259M (stopped after 105 min, see above), and SQLite `PRAGMA integrity_check` (only `quick_check` ran).
- Real application traffic. The load was one synthetic writer and one reader.
- A mixed-version rollout with real old pods.
- Down migrations other than `037`.
