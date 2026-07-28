# Phoenix Load Tests

`k6-load-test.js` seeds a disposable Phoenix instance to `MONITOR_COUNT`,
loads the authenticated monitor inventory with `API_VUS`, and holds
`WS_CLIENTS` authenticated WebSocket connections open while counting heartbeat
fan-out and event latency.

The setup phase creates real HTTP monitors at 60-second intervals. It never
deletes them, so never point it at a development or production database.

## Environment variables

- `BASE_URL` — URL reachable by k6 (default `http://localhost:3000`).
- `MONITOR_TARGET` — health URL reachable by Phoenix's checker process.
- `AUTH_TOKEN` — optional JWT; otherwise the script logs in.
- `ADMIN_USERNAME` / `ADMIN_PASSWORD` — bootstrap credentials used to log in.
- `MONITOR_COUNT` — cumulative target number of monitors (default `100`).
- `API_VUS` — concurrent authenticated inventory readers (default `5`).
- `WS_CLIENTS` — concurrent authenticated WebSocket clients (default `10`).
- `TEST_DURATION` — API scenario duration (default `30s`).
- `WS_HOLD_MS` — how long each WebSocket stays connected (default `30000`).
- `WS_MAX_DURATION` — hard ceiling for the WS scenario (default `45s`; keep it
  above `WS_HOLD_MS`).

## Thresholds

- monitor-list API p95 below 500 ms and p99 below 1 s;
- HTTP failure rate below 1%;
- WebSocket connection p95 below 1 s;
- heartbeat fan-out latency p95 below 1 s and p99 below 2 s.

## Container runner

With Phoenix exposed on host port 3102, run k6 without installing it locally:

```bash
docker run --rm \
  -v "$PWD/tests/load:/scripts:ro" \
  grafana/k6:0.57.0 run \
  --env BASE_URL=http://host.docker.internal:3102 \
  --env MONITOR_TARGET=http://127.0.0.1:3102/api/health/live \
  --env ADMIN_USERNAME=admin \
  --env ADMIN_PASSWORD=ChangeMe123! \
  --env MONITOR_COUNT=100 \
  --env API_VUS=5 \
  --env WS_CLIENTS=10 \
  --env TEST_DURATION=30s \
  --env WS_HOLD_MS=30000 \
  /scripts/k6-load-test.js
```

The isolated database/app startup and measurement procedure used for Sprint B
is recorded in `docs/LOADTEST.md`.
