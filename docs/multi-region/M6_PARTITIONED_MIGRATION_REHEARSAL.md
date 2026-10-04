# M6 — first populated partitioned MariaDB migration rehearsal

Status: **bounded engineering rehearsal, not the full M6 production-sized migration gate**. This record covers the most disruptive M1 upgrade (`037_probe_heartbeat.up.sql`): new regional columns/index on the partitioned `heartbeats` table and replacement of all three legacy rollup unique keys. A [later rehearsal](M6_MIGRATION_RUNNER_REHEARSAL.md) additionally exercises the application's Go runner for `038`–`074` on the populated schema. It does not authorize applying the upgrade to a running deployment. Stop all writers for a real schema migration; MariaDB DDL auto-commits.

## Reproduction and isolation

From the repository root with a cached `mariadb:11` image and a working Docker daemon:

```sh
python3 -B scripts/rehearse_probe_heartbeat_migration.py --rows 100000 --hold-reader-seconds 0
python3 -B scripts/rehearse_probe_heartbeat_migration.py --rows 100000 --hold-reader-seconds 3
```

The harness creates a **new random-name disposable container**, `--network none`, no published ports, 2 vCPU / 2 GiB limit, and a 1.5 GiB tmpfs for MariaDB data. It starts a fresh schema by applying the checked-in `001`–`036` MariaDB SQL files through the MariaDB client, loads 100,000 synthetic legacy heartbeats (half in each of two monthly partitions) and 1,000 legacy rollups *per resolution*, then executes the checked-in `037` SQL in order. The root password is empty **inside this isolated no-network container only**; no external DSN, database or credential is read. The script removes only its own container in `finally` (and did so after the recorded runs).

The second run holds a read transaction on `heartbeats` for three seconds before executing `037`. It polls `information_schema.PROCESSLIST` for a metadata-lock wait during the migration. The first run gives a no-reader baseline on the same synthetic data. The disk sampler uses `du -sk /var/lib/mysql` at ~200 ms intervals during DDL plus before/after endpoints. Its observed peak is a **lower bound**, not a guaranteed true peak or a volume-sizing figure: short-lived temporary files outside the data directory or between samples are not measured.

## Executed evidence (2026-09-29 UTC)

Image: `mariadb:11`, resolved server version `11.8.9-MariaDB-ubu2404`; Colima Docker VM has 2 CPUs / 4 GiB RAM. Both runs exited 0. The target table had **14 range partitions**, retained `PARTITION BY RANGE (UNIX_TIMESTAMP(time))` and `PRIMARY KEY(id,time)`, and kept every heartbeat ID and both month counts. All 3,000 legacy rollups retained `(count, SUM(id))`; all existing rows backfilled to `probe_id=local`, with zero unknown counts. The exact ordered, unique `(monitor_id,probe_id,bucket)` key exists on all three rollups, with the obsolete `(monitor_id,bucket)` unique key gone; the new heartbeat index has `(monitor_id,probe_id,time,id)`. A second probe was inserted into a legacy minute bucket with a *new* auto-increment ID. A `037 down` with that remote rollup was refused by the database CHECK guard before dropping the heartbeat columns; the remote row remained.

| Run | Full `037` client elapsed | Metadata-lock wait seen | Data-dir before → after | Observed data-dir peak | Disk samples |
|---|---:|---|---:|---:|---:|
| No held reader | 0.163 s | no | 208,776 → 209,304 KiB | 209,304 KiB | 1 |
| 3 s held read transaction | 3.109 s | yes | 208,776 → 209,304 KiB | 209,304 KiB | 14 |

Observed data-dir growth was **528 KiB**. The ~2.95 s difference between the two elapsed times is consistent with the deliberately held reader, not a claim that DDL is generally lock-free. A queued DDL can also block later readers; this test only observes the writer's metadata-lock wait, **not** reader latency or live application impact. The 0.163 s result is a tmpfs/small-dataset result, not a production estimate.

Hashes for the **original c939db4 run** (SHA-256; the Python harness has since gained an optional `--run-tail` path, with its new hash in the later rehearsal):

- `scripts/rehearse_probe_heartbeat_migration.py`: `a9d46c77348c4d17a5ef5a4cbd09076b18963aa41bf1a79d8d8baf509b4a6c8a`
- `037_probe_heartbeat.up.sql`: `8a779becef619d1d52e9ff4341d19e133308ae6288240ff4080f6d9d599a07ac`
- `037_probe_heartbeat.down.sql`: `56557828d9158a9ac28597673e8566f52ec2731fcae188b0a85956e9c85cda5f`

## Supporting verification

Executed `GOTOOLCHAIN=go1.26.6 go build ./...`, `go test -race -count=1 -timeout 2400s -p 4 ./...` (22 packages passed; **MariaDB test legs were not enabled**), `~/go/bin/golangci-lint run ./internal/adapters/repository/...` (0 issues), and the focused `TestHeartbeatProbeIDAndRollupUniqueKey` regression with race detection (SQLite passed). A separate JSON-audited focused run reported `sqlite: pass`, `mariadb: skip` because `TEST_MARIADB_DSN` was unset; do **not** count that as a real-engine Go test. The MariaDB evidence in this record comes from the isolated SQL rehearsal, not from the Go adapter test. No Svelte/Helm files were changed.

## What remains unverified

- This original run executed **checked-in SQL via the MariaDB client**, not `repository.RunMigrations` or a whole in-place application upgrade. Earlier migrations ran on an *empty* schema. The [follow-up](M6_MIGRATION_RUNNER_REHEARSAL.md) uses `repository.RunMigrations` for `038`–`074` on the populated schema, but neither run measures a production-sized application-led upgrade from a real legacy snapshot or an operational rollback through dependent newer schema.
- 100,000 synthetic heartbeats and 3,000 rollups are not a copy of a production distribution, disk layout, retention policy, concurrency pattern, or representative payloads. No real writer raced the migration. Peak total disk growth (including temp space) and reader/write latency during the metadata-lock queue are **not** established.
- Rollback refusal was checked for a **remote rollup**. Other remote/scoped identities and other migrations' downgrade guards have separate existing engine tests; this rehearsal does not replace those tests.

For the M6 checkbox, repeat against an operator-approved **disposable copy** of a suitably large partitioned deployment on comparable storage, include every applicable upgrade/guard, time each step, sample all relevant disk locations and reader/metadata-lock impact, and record the rollback refusal paths. Do not use the rehearsal's empty-password/no-network container configuration outside this self-contained script. The full section-13 matrix, distributed load/backlog/kill tests and canary also remain open.
