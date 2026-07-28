// Package ports defines the interfaces that adapters must implement.
// No implementations, no external imports — only stdlib and domain types.
package ports

import "errors"

// Storage-level error sentinels.
//
// Adapters return these so service-layer code can use errors.Is to react
// to specific failure modes (e.g. translate ErrConflict into HTTP 409).
// Domain-level duplicates live in github.com/fiztoz/uptime-phoenix/internal/core/domain/errors.go
// — the two sets are distinct on purpose: domain.ErrXxx represents
// business semantics ("this user is not allowed to do that"), while
// ports.ErrXxx represents storage mechanics ("the row already exists").
var (
	// ErrNotFound is returned by repository lookups that miss.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned by repository writes that violate a
	// uniqueness constraint (e.g. duplicate username, slug, or domain).
	ErrConflict = errors.New("conflict")
	// ErrMonitorAlreadyLinked is a specific conflict returned when a
	// monitor is already assigned to a status page. This is more precise
	// than ErrConflict and produces a clearer user-facing message than
	// the generic "slug or custom domain already in use".
	ErrMonitorAlreadyLinked = errors.New("monitor already linked to status page")
)
