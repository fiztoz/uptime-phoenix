# M6 — application-runner migration tail on populated partitioned MariaDB

**Status: bounded extension of the [migration-037 rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md), NOT the full M6 production-sized upgrade gate.** No production data, deployment or canary was touched. The current milestone stays unchecked in [the plan](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation).

## Scope and reproduction

```sh
python3 -B scripts/rehearse_probe_heartbeat_migration.py --rows 100000 --hold-reader-seconds 0 --run-tail
python3 -B scripts/rehearse_probe_heartbeat_migration.py --rows 100000 --hold-reader-seconds 3 --run-tail
```

As in the first rehearsal, the Python harness uses a fresh random-name **no-network/no-published-port** `mariadb:11` container (2 CPU, 2 GiB RAM, 1.5 GiB data tmpfs), applies the checked-in SQL for `001`–`036` to an initially empty schema, seeds 100,000 legacy heartbeats over two of the 14 monthly partitions and 1,000 legacy rollups per resolution, then executes checked-in `037` SQL against those rows. It tests the metadata-lock wait with an optional three-second read transaction, verifies original IDs/backfill/indexes/partition expression, adds a second probe's rollup into a legacy minute, and checks `037 down` fails closed with that remote row intact.

**New step:** only after the checked-in `037` succeeds, the harness creates a matching `_migrations` ledger with the *already applied* `001`–`037` filenames. It cross-compiles `scripts/rehearse_tail_migrations.go` with `CGO_ENABLED=0` for the container's Linux architecture and copies it inside. The build uses the installed local Go toolchain (Go 1.27.1 on this host) with `GOPROXY=off`/`GOSUMDB=off`, and the script first verifies a local Unix Docker socket and cached `mariadb:11` image: missing prerequisites fail instead of pulling anything. The helper has a fixed Unix-socket DSN for *this throwaway schema only*, refuses to run outside the tagged container, and calls the **production `repository.RunMigrations(db, "mariadb")`**. It applies exactly `038_probe_incidents.up.sql` through `074_hub_worker_capabilities.up.sql`, then repeats the call to assert idempotence. It does **not** claim that the Go runner applied `001`–`037`; the ledger was explicitly backfilled in the disposable schema. Neither process reads an operator DSN; the database has no published port or Docker network, and the helper connects through a local Unix socket. The harness removes its own container on exit.

## Executed evidence (2026-09-29 UTC)

Both final 100k-row commands exited 0 on MariaDB `11.8.9-MariaDB-ubu2404` (Colima VM 2 CPU/4 GiB). The runner added **37** real migrations and the ledger ended with **74** rows; its second invocation returned successfully with no new ledger rows. The `hub_worker_capabilities` table from `074` exists. All 100 monitors, 100,000 legacy heartbeats and IDs, their two-month split and the partition list survived the tail. All local rollup `(count, SUM(id))` pairs survived each resolution, and the distinct remote minute rollup survived. The `037` key and heartbeat index shapes were checked again **after** the tail; the guarded downgrade was checked before it. These are persisted-state checks, not solely exit-code checks.

| 100k-row run | `037` elapsed | `037` lock wait observed | Runner `038`–`074` elapsed | Data-dir KiB before → after tail | Tail sampled peak / samples |
|---|---:|---|---:|---:|---:|
| No held reader | 0.155 s | no | 0.065 s | 209,488 → 212,856 | 212,856 / 1 |
| Three-second reader | 3.039 s | yes | 0.063 s | 209,488 → 212,856 | 212,856 / 1 |

The extra three seconds are attributable only to the deliberately held read transaction. `du -sk /var/lib/mysql` sampling at ~200 ms is **not true peak disk growth**; only one sample landed during the short tail, and temporary space outside the data directory is omitted. The numeric wall times on tmpfs and synthetic rows do not predict production execution. Neither reader latency behind queued DDL nor concurrent writer impact was measured. Two preliminary 2k-row harness runs stopped after `037` but before the Go runner applied any tail migration: the helper rejected an untagged container, then offline compilation rejected Go's downloadable-toolchain checksum with `GOSUMDB=off`. Tagging only the newly created container and selecting the already installed local Go toolchain corrected both harness failures. The final 2k-row dry run and both 100k-row runs passed offline.

SHA-256 for the final executed inputs:

- `scripts/rehearse_probe_heartbeat_migration.py`: `17a05664dcac460f754cd6f863f788cf81f0a5144b2a37eb5128b7d9eee1bf88`
- `scripts/rehearse_tail_migrations.go`: `aaad4219cb5b3bef74d1a5000c3f1266692c6ecef715fa3c0159414f25b74166`
- Sorted `001`–`074` `*.up.sql` filenames and raw bytes, each delimited by `\0`, SHA-256: `126f7b8ab4c643100f78a14b1618dc4dac3d01b10591f678b4bb7807c6751adf`
- `037_probe_heartbeat.down.sql`: `56557828d9158a9ac28597673e8566f52ec2731fcae188b0a85956e9c85cda5f`

## Supporting gates and evidence limits

Executed `GOTOOLCHAIN=go1.26.6 go build ./...`, `GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -p 4 ./...` (22 packages passed), `GOTOOLCHAIN=go1.26.6 ~/go/bin/golangci-lint run ./...` (0 issues after correcting one preliminary goimports failure), `gofmt -l scripts/rehearse_tail_migrations.go` (empty), Python AST parse and a 2k-row run **without** `--run-tail` (pass). The two final 100k-row `--run-tail` runs passed against the *real* MariaDB engine, not a fake. Separate preflight checks confirmed that `DOCKER_HOST=tcp://…` fails before Docker use and that the Go helper without its isolated-container tag fails before opening a database. No `TEST_MARIADB_DSN` was set for the Go test command, so it does **not** establish that any Go MariaDB integration subtest executed. The full 40-row section-13 matrix was neither authored nor executed here. No frontend or Helm file changed.

## Remaining release boundary

The production Go runner was used for the **tail only**; there is no application-led upgrade on a representative snapshot through every version. Most early schema changes happened before loading data. Synthetic 100k/3k rows, a single controlled reader and tmpfs do not measure peak disk growth or real-reader/write lock impact on a production-sized partitioned copy. Only the `037` downgrade guard with a remote rollup was rehearsed in this flow. Other downgrade refusals and real dual-engine repository tests are separate. The full section-13 matrix, distributed load/backlog, kill-point coverage and canary all remain unverified by this rehearsal. Rollback after remote evidence is a roll-forward operation until an operator-approved export/drain; do not downgrade a live installation based on these results.
