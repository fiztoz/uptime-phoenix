# Phoenix Load Test — Sprint D (R3.6)

**Run date:** 2026-07-25
**Result:** the `<1 s` p95 / `<2 s` p99 heartbeat fan-out target is now **met at 100, 1,000 and
10,000 monitors**. The 10,000-monitor stage ran in split API/worker mode and passed every
threshold. One unrelated threshold, `ws_connect_time`, is intermittently flaky — see
"Known flakiness" below; it is recorded as a failure where it failed.

This supersedes the Sprint B run, which measured a WS event p95 of 5.44 s at 100 monitors and
60 s at 1,000. That was not a capacity ceiling — it was the quadratic fan-out defect R3.6, now
fixed. See "What changed in the harness" for why the old and new numbers are not directly
comparable.

## What changed in the harness

`tests/load/k6-load-test.js` gained a `MONITOR_INTERVAL` environment variable (default `60`,
unchanged).

This mattered more than it sounds. With the previous fixed 60 s interval and a 30 s scenario,
**the entire sample window could fall between two check rounds**: zero heartbeat events were
observed and `ws_event_latency` reported a meaningless p95 of `0s` while every threshold
"passed". The first post-fix run hit exactly that and looked like a triumph; it was measuring
nothing. Setting the interval below the scenario duration guarantees every monitor checks at
least once while the WebSocket clients are connected.

Because of this, **every number below was re-measured on BOTH the pre-fix and the post-fix
binary under identical settings.** A post-fix figure compared against the Sprint B table would
prove nothing.

## Results

All stages: fresh `phoenix_load` database per run, isolated MariaDB 11 on a 1 GB tmpfs.
Host: 10-core Apple Silicon laptop / 16 GB RAM (Phoenix API/worker binaries run natively here).
Containers (MariaDB, Redis, k6) run under **Colima** at **2 CPUs / 4 GiB** — that VM
allocation, not the host's full 16 GB, is the binding limit for the Docker-side stack.
MariaDB is further capped by its 1 GB tmpfs. `api_response_time` and `http_req_failed`
passed in every stage and are omitted except where notable.

### In-process mode (`MODE=all`, memory EventBus)

| Stage | Monitors | WS clients | Interval | Event p95 | Event p99 | Event max | Connect p95 | HB events delivered | Result |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| **pre-fix** | 100 | 10 | 20 s | **2.08 s** | — | 2.57 s | 13 ms | 3,420 | **FAIL** (p95 > 1 s) |
| post-fix | 100 | 10 | 20 s | **108 ms** | — | 116 ms | 19 ms | 2,500 | PASS |
| **pre-fix** | 1,000 | 50 | 20 s | **49.88 s** | — | 53.85 s | 9 ms | 20,600 | **FAIL** (p95 > 1 s) |
| post-fix run 1 | 1,000 | 50 | 20 s | **84 ms** | — | 133 ms | 25.5 ms | 142,500 | PASS |
| post-fix run 2 | 1,000 | 50 | 20 s | **147 ms** | 188 ms | 394 ms | 1.05 s | 147,622 | FAIL (`ws_connect_time` only) |
| post-fix run 3 | 1,000 | 50 | 20 s | **144 ms** | 258 ms | 461 ms | 37.5 ms | 147,500 | PASS |

### Split API/worker mode (separate `cmd/api` + `cmd/worker`, Redis EventBus)

| Stage | Monitors | WS clients | Interval | Event p95 | Event max | Connect p95 | API p95 | HB events delivered | Result |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| split | 1,000 | 50 | 20 s | **113 ms** | 169 ms | 37 ms | 37.5 ms | 142,500 | PASS |
| split | **10,000** | 50 | 30 s | **405 ms** | 7.77 s | 23 ms | 369 ms | **1,342,628** @ 4,498/s | PASS |

At the 10,000-monitor peak the API process used 220.6 MiB RSS and the worker 78.3 MiB at a
sampled 47% CPU; 69,450 heartbeat rows were written during the run and no worker errors were
logged. Routing events through Redis costs roughly 30 ms of p95 versus the in-process bus
(113 ms vs 84 ms at 1,000 monitors), which is the expected price of cross-pod fan-out.

### The delivered-event counts are the other half of the story

At 1,000 monitors the pre-fix binary delivered **20,600** heartbeat events to its clients; the
post-fix binary delivered **~147,000** from a comparable ~4,000 heartbeat rows. That is not a
7× throughput improvement — it is 7× **less silent loss**. The pre-fix hub stalled long enough
that the EventBus subscriber buffer overflowed and discarded events on a bare `default:`
branch, with no log line and no metric. The Sprint B table's "delivered heartbeat events/sec"
therefore overstated what clients actually received.

Both drop paths are now counted (`phoenix_eventbus_events_dropped_total{event_type}` and
`phoenix_ws_frames_dropped_total`), so this failure mode is visible on `/metrics` rather than
invisible. Note `/metrics` is API-key protected; an unauthenticated `curl` returns nothing and
must not be read as "no drops".

Coalescing is visible too: at 1,000 monitors the pre-fix run sent 40,150 total WS messages of
which only 20,600 were heartbeats — the remaining ~19,550 were one `stats.update` per event.
Post-fix the same stage sent 142,600 messages of which 142,500 were heartbeats: the badge
recompute collapsed to ~100 frames.

## Known flakiness — `ws_connect_time`

`ws_connect_time` breached its `p95 < 1 s` threshold in one of three post-fix runs at 1,000
monitors (p95 = 1.05 s), and passed at 25.5 ms and 37.5 ms in the other two. **This is recorded
as a failure, not smoothed away.**

It is not caused by the R3.6 fix:

- the **pre-fix** baseline at 1,000 monitors shows the same spike (max = 1.04 s) while its p95
  stayed at 9 ms — the tail was already there;
- the value clusters at ~1.04–1.06 s, a fixed delay rather than a load-dependent curve;
- every WebSocket upgrade succeeded in every run (checks 100%), so it is not the 50 req/s
  ingress limiter rejecting `/ws`;
- it affects a handful of the 50 simultaneously-connecting virtual users.

A separate, real connect-path defect WAS found and fixed during this work: `sendMonitorList`
resolved each monitor's status with a per-monitor `GetLatest`, so a client connecting to a
1,000-monitor install issued 1,000 serialized queries — 50,000 across 50 clients. That pushed
connect p95 to 1.06 s *consistently*. It now uses the batched lookup. The residual ~1.05 s
outlier above is what remains after that fix and is most likely harness- or OS-side
(simultaneous-accept scheduling); it has not been root-caused and should not be assumed
resolved.

## Reproduction

Run only against a throwaway database. Never substitute the development database named
`phoenix` or its volume.

```bash
docker run -d \
  --name phoenix-load-mariadb \
  --tmpfs /var/lib/mysql:rw,size=1g \
  -p 127.0.0.1:13307:3306 \
  -e MARIADB_ROOT_PASSWORD=load_root \
  -e MARIADB_DATABASE=phoenix_load \
  -e MARIADB_USER=phoenix \
  -e MARIADB_PASSWORD=load_app \
  mariadb:11
```

Recreate the database between stages so the first-check storm is part of the measurement:

```bash
docker exec phoenix-load-mariadb mariadb -uroot -pload_root \
  -e "DROP DATABASE IF EXISTS phoenix_load; CREATE DATABASE phoenix_load;
      GRANT ALL ON phoenix_load.* TO 'phoenix'@'%'; FLUSH PRIVILEGES;"
```

### In-process stage

```bash
go build -o /tmp/phoenix-load-app ./cmd/app

DB_ENGINE=mariadb \
DB_DSN='phoenix:load_app@tcp(127.0.0.1:13307)/phoenix_load?charset=utf8mb4&parseTime=true&loc=UTC' \
PORT=3102 JWT_SECRET=load_secret \
BOOTSTRAP_USERNAME=admin BOOTSTRAP_PASSWORD='ChangeMe123!' \
/tmp/phoenix-load-app
```

```bash
docker run --rm -v "$PWD/tests/load:/scripts:ro" grafana/k6:0.57.0 run \
  --summary-trend-stats "avg,min,med,max,p(90),p(95),p(99)" \
  --env BASE_URL=http://host.docker.internal:3102 \
  --env MONITOR_TARGET=http://127.0.0.1:3102/api/health/live \
  --env ADMIN_USERNAME=admin --env ADMIN_PASSWORD='ChangeMe123!' \
  --env MONITOR_COUNT=1000 --env API_VUS=5 --env WS_CLIENTS=50 \
  --env TEST_DURATION=60s --env WS_HOLD_MS=60000 --env WS_MAX_DURATION=90s \
  --env MONITOR_INTERVAL=20 \
  /scripts/k6-load-test.js
```

`MONITOR_INTERVAL` **must** be below `TEST_DURATION`, or the sample window can fall between
check rounds and `ws_event_latency` will report a vacuous `0s`.

### Split API/worker stage (10,000 monitors)

```bash
docker run -d --name phoenix-load-redis -p 127.0.0.1:16380:6379 redis:7-alpine
go build -o /tmp/phoenix-api ./cmd/api
go build -o /tmp/phoenix-worker ./cmd/worker

DSN='phoenix:load_app@tcp(127.0.0.1:13307)/phoenix_load?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true'

# API tier owns migrations — wait for /api/health/ready before starting the worker.
DB_ENGINE=mariadb DB_DSN="$DSN" REDIS_URL='redis://127.0.0.1:16380/0' \
PORT=3102 JWT_SECRET=load_secret \
BOOTSTRAP_USERNAME=admin BOOTSTRAP_PASSWORD='ChangeMe123!' /tmp/phoenix-api &

until curl -sf http://127.0.0.1:3102/api/health/ready >/dev/null; do sleep 1; done

DB_ENGINE=mariadb DB_DSN="$DSN" REDIS_URL='redis://127.0.0.1:16380/0' /tmp/phoenix-worker &
```

Then run k6 with `MONITOR_COUNT=10000 WS_CLIENTS=50 TEST_DURATION=90s MONITOR_INTERVAL=30`.
Seeding 10,000 monitors takes several minutes; `setupTimeout` is already 15 m.

Tear down only the disposable containers:

```bash
docker rm -f phoenix-load-mariadb phoenix-load-redis
```

## What is still not covered

- A rolling-deploy WebSocket soak (reconnect/resume across a restart) — Track C.
- Sustained multi-hour running; every stage here is 60–90 s.
- The `ws_connect_time` outlier above has not been root-caused.
