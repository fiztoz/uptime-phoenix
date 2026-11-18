# Bounded load driver (`single-probe-N`)

`scripts/bounded_load_driver.py` drives the multi-region `single-probe-100` phase and larger
`single-probe-N` phases against one hub from one client, at a bounded rate. It was added for
issue #270, where a run stopped on an HTTP error whose request and status were never recorded.

It uses only the Python 3 standard library (3.9 or newer), so a plain checkout at a pinned commit is enough.
Nothing needs to be uploaded to the test hosts.

## What it does

- **`--mode paced`**: logs in, reads a baseline config snapshot for the probe, then for each of
  N monitors does `monitor.create` (HTTP, interval 5 s, timeout 4 s), `assignment.read` and
  `assignment.replace` onto the probe. It then polls the probe config snapshot
  (`GET /api/probes/{id}`, class `probe.snapshot`) every 2 s, plus an optional operator-side
  peer snapshot (class `peer.snapshot`). It stops polling when the snapshot reports
  `sync_status=applied`, the desired revision equals the applied revision, and the applied
  assignment count has reached the baseline plus N. The count check matters because the hub
  publishes probe config asynchronously, so the previous revision can look fully applied right
  after the last write.
- **`--mode unpaced-control`**: a deliberate failing control. It sends sequential, unpaced
  `assignment.read` requests over the existing monitors. It passes only if the hub answers
  **429**, which shows that the driver captures the failing request and its status. Any other
  non-2xx, or no 429 within `--control-max-requests` (default 3 × burst), fails.

**Pacing.** The hub's ingress limiter (`RATE_LIMIT_RPS`, default 50, and `RATE_LIMIT_BURST`,
default 100) runs before auth and is keyed per client IP, so every request from the driver
counts against one bucket. Paced mode sends one request at a time, spaced at `--target-rps`.
The default is `min(25, RATE_LIMIT_RPS / 2)`, and the driver refuses a rate at or above
`RATE_LIMIT_RPS`. Set the hub's limits explicitly and pass the same values (as flags, or as the
`RATE_LIMIT_RPS` / `RATE_LIMIT_BURST` environment variables). The evidence records the values
and whether each came from a flag, the environment or the default.

**Fail fast.** The first non-2xx, redirect, transport error, malformed response or
non-convergence stops the run. Nothing is retried. The evidence file is still written.

**Classification.** `http_429_rate_limited`, `http_500_server_error` (kept apart from other
5xx because a snapshot path can collapse several internal errors into a 500),
`http_5xx_other`, `http_4xx_client_error`, `http_other_non_2xx`, `transport_timeout`,
`transport_connection`, `transport_other`, `invalid_response`, `not_converged`,
`control_no_429`.

## Evidence (allowlisted fields only)

Each request produces one record with exactly these fields: `seq`, `method`,
`endpoint_class`, `status` (`null` for a transport error), `ts_utc`, and `rate_rps` (requests
started in the last second). Records are printed as JSON lines and kept in the evidence file.
The record type rejects any other field, method or endpoint class, and the tests check that
no URL, host, port, ID, credential, token, header or body text reaches stdout or the file.

The evidence file (written with mode 0600) also holds the driver version and commit, the hub
commit if given, the pacing settings, the result, counts by `endpoint_class status`, the
convergence summary, and for a failure its category, sequence number, method, endpoint class,
status and the request count and rate over the preceding 5 s.

Keep evidence files private. They contain no addresses or IDs, but they are run artifacts and
do not belong in the repository.

## Build on the instance from the public repo

Run this on each host. Pin full 40-character commits, and record what was actually checked out.

```sh
HUB_COMMIT=<40-hex commit of the hub candidate>
DRIVER_COMMIT=<40-hex commit that contains scripts/bounded_load_driver.py>

git clone https://github.com/fiztoz/uptime-phoenix.git
cd uptime-phoenix
git checkout --detach "$HUB_COMMIT"
git rev-parse HEAD
go version            # must match the go directive in go.mod
CGO_ENABLED=0 go build -tags noweb -o ./bin/ ./cmd/app ./cmd/probe ./cmd/phoenix-probe-admin ./cmd/phoenix-probe-key
sha256sum bin/*

# The driver can come from a different commit than the hub.
git worktree add --detach ../driver "$DRIVER_COMMIT"
git -C ../driver rev-parse HEAD
```

The Go toolchain and modules come from their public sources. No compiled binary, archive or
evidence file moves between hosts.

## Run

Keep credentials and addresses in the environment. They are never written to evidence.

```sh
export LOAD_HUB_URL=...          # hub base URL as seen from this host
export BOOTSTRAP_USERNAME=... BOOTSTRAP_PASSWORD=...
export LOAD_PROBE_ID=...         # the enrolled probe to assign to
export LOAD_TARGET_URL=...       # URL the monitors check (allowed by the probe endpoint policy)
# export LOAD_PEER_SNAPSHOT_URL=...  # optional operator-side snapshot endpoint, polled with no credentials

python3 -B ../driver/scripts/bounded_load_driver.py --mode paced --phase single-probe-100 \
  --monitors 100 --hub-rate-limit-rps 50 --hub-rate-limit-burst 100 \
  --hub-commit "$HUB_COMMIT" --evidence single-probe-100.json

# Failing control, after the paced phase and the hold:
python3 -B ../driver/scripts/bounded_load_driver.py --mode unpaced-control --phase unpaced-control \
  --hub-rate-limit-rps 50 --hub-rate-limit-burst 100 --evidence unpaced-control.json
```

Exit status: 0 for a passed paced run or a control that got its 429, 1 for a failed run, 2 for
a usage error (nothing sent). For larger phases, raise `--monitors` (up to 2000) and give the
phase a matching `--phase` label.

## Tests

```sh
python3 -B -m unittest discover -s scripts -p test_bounded_load_driver.py -v
```

The tests run against a fake hub on loopback that models the endpoints, the ingress token
bucket on a shared fake clock, asynchronous config publication and injected faults. They make
no external network calls.
