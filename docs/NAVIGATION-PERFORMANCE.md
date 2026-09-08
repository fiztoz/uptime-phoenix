# Dashboard and Monitors navigation

The reported recording showed a hydrated monitor count alongside an empty
Monitors table, and Dashboard folder cards returning to skeletons on each visit.
Both routes kept the WebSocket monitor snapshot but recreated their folder
catalogs as empty arrays. Grouped monitors could not be placed in the tree until
the catalog request finished. The table's empty-state condition ignored that
pending request.

The folder endpoint also resolved statuses using one latest-heartbeat repository
call per grouped monitor. It now uses the existing `HeartbeatBatchReader` port
implemented by MariaDB and SQLite. The adapters retain their existing
`time DESC, id DESC` ordering. Nested rollups, missing heartbeats, maintenance,
and all-install versus owner-scoped selection keep the same semantics. A batch
database error is returned instead of presenting an incomplete status map.
Repositories without the optional batch port retain the sequential path.

The frontend retains folders, tags, and the five-row Dashboard Insights preview
in memory for up to 60 seconds as a first-paint aid. Every route visit still
revalidates; concurrent requests for the same resource share the pending read.
The exact session token scopes each resource, logout clears it, and catalog
writes invalidate it. Invalidation also prevents an older pending read from
repopulating the cache. Returned values are independent copies. No resource
data is written to browser storage, and a full reload starts cold.

Background refreshes preserve visible content. Initial Monitors loading and
folder errors cannot display the “No monitors found” message. Live availability
continues to come from the existing WebSocket store.

## Evidence

- `08-navigation-performance.spec.ts` reproduces the original failure by
  loading a folder on Dashboard, holding subsequent folder responses open,
  and navigating to Monitors. Before the change, the folder remained absent
  for the entire 1.5-second assertion window. The regression requires both
  routes to show it while the response is still held, and checks cold loading
  and mobile rendering too.
- `TestGroupStatusBatchMatchesSequential` compares nested rollup results on
  65 grouped monitors, including missing and maintenance heartbeats. It requires
  one batch invocation, zero individual latest-heartbeat calls, exclusion of an
  ungrouped monitor, and propagation of batch failures.
- `session-resource.test.ts` checks request coalescing, expiration, mutation
  isolation, session changes, invalidation during a pending read, retry after
  failure, and empty catalogs.

These checks establish removal of the navigation dependency and the N+1
repository call pattern. They are not production latency measurements. A cold
load still waits for authorized catalog data, and subsequent visits still make
fresh requests. Database latency and connection-pool contention remain relevant.

Validation completed with Go build/vet/race tests, golangci-lint (zero issues),
Svelte check (zero errors/warnings), frontend unit tests/build/Prettier/ESLint,
the 11 existing browser tests and the new navigation regression, Helm lint and
the render matrix, and govulncheck (no reachable vulnerabilities). The gate
initially stopped on an existing detached package comment in `assets.go`;
moving the build tag above the comment fixed it. Remaining gate steps were run
individually after that correction. The standalone lint retry used the same
Go 1.26.6 toolchain pinned by the Makefile.
