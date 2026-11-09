# M6: local-scale migration rehearsal, v0.4.5 → main (20M and 50M heartbeats)

Status: **local-scale engineering evidence, not the production-sized gate.** These runs used a box's local disk, not storage comparable to a deployment, and synthetic rows. The production size and storage are owner decisions that are still open. The M6 checkbox in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) §9 stays unticked.

Run 2026-11-09, 22:29–23:02 ICT, at main `8e14335`. No cloud resources were used.

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

## Findings

1. **A long transaction on `heartbeats` stalls every heartbeat read and write during `037`.** In r2, the `ALTER TABLE heartbeats` waited for the metadata lock behind the held reader, from t ≈ 10 s to t = 30 s. While it waited, every new writer and reader queued behind the `ALTER`, and both maxed out at 19.9 s. That's standard MariaDB metadata-lock behaviour.
   - The runner sets no `lock_wait_timeout`, so the server default of 86,400 s applies.
   - [RUNBOOK](../RUNBOOK.md) §2.1 keeps one migration-owning API pod, but doesn't say to stop the old pods.
   - The chart's `RollingUpdate` with `maxUnavailable: 0` keeps the old pods serving while the new pod migrates.

   Without a long transaction (r1, r3), the worst stall was 0.5 s.
2. **An older binary does not refuse a newer schema.** The v0.4.5 runner against the upgraded `094` schema exits 0 and applies nothing. The RUNBOOK says never to mix an old binary with a partially upgraded schema, but nothing enforces that at startup.

Neither finding is fixed here. Each needs an owner decision: a runbook step, a runner `lock_wait_timeout` with retry, and/or a startup guard.

## Not covered

- Storage comparable to a deployment, a production-shaped data distribution, payload sizes, retention, or production size (owner decision D3).
- Real application traffic. The load was one synthetic writer and one reader.
- A mixed-version rollout with real old pods.
- Down migrations other than `037`.
