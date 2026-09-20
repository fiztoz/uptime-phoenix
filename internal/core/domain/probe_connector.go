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
