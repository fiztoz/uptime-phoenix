package domain

import "time"

// ProbeConnectorLease fences one hub worker's outbound probe connection. Generation
// survives release and increases on every acquisition, including same-owner retry.
type ProbeConnectorLease struct {
	ProbeID    string
	OwnerID    string
	Generation int64
	LeaseUntil time.Time
	Connected  bool
}

// ProbeRuntimeLease owns connector attempts across reconnect backoff. Epoch is
// independent of per-session generation and survives release and process restart.
// This fence also identifies the owner of the connection watchdog timer.
// LeaseUntil is an acquisition/renewal snapshot for diagnostics; repositories
// check current stored authority, never a caller's cached wall-clock deadline.
type ProbeRuntimeLease struct {
	ProbeID    string
	OwnerID    string
	Epoch      int64
	LeaseUntil time.Time
}
