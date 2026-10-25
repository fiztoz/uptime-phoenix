package domain

import "errors"

// Snapshot errors are fixed and never carry local paths or storage diagnostics.
var (
	ErrProbeImageSnapshotStorage = errors.New("probe image snapshot storage unavailable")
	ErrProbeImageSnapshotClosed  = errors.New("probe image snapshot closed")
	ErrProbeImageSnapshotBusy    = errors.New("probe image snapshot bundle already open")
)
