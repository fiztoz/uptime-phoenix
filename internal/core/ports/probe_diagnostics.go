package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeDiagnosticsRepository is the dedicated safe read port for administrative
// fleet diagnostics. It returns the complete nonsecret picture for registered
// probes read coherently across registration, enrollment, connection watchdog,
// runtime lease, config publication and applied receipts in one transaction on
// every supported engine.
//
// It never returns credential bytes, key hashes, configuration payloads or
// protected identities, and it never mutates state: no row is created, renewed
// or expired by a read. Lease liveness is observational; anything that writes
// under a lease must re-prove ownership inside its own transaction.
//
// Rows are ordered by probe ID ascending. after is an exclusive cursor (empty
// starts at the beginning) and limit bounds the page; the store returns at most
// limit rows and the caller derives pagination from the last returned ID.
type ProbeDiagnosticsRepository interface {
	ListProbeDiagnostics(ctx context.Context, after string, limit int) ([]domain.ProbeDiagnostics, error)
	GetProbeDiagnostics(ctx context.Context, probeID string) (*domain.ProbeDiagnostics, error)
}
