package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// RequireSafeOperativeStartup refuses downgrade into uninstrumented direct
// delivery while operative authority or unknown provider I/O remains durable.
// Removing the configuration key or connector runtime is not source resume.
func RequireSafeOperativeStartup(ctx context.Context, db *bun.DB, protected, probes bool) error {
	var unresolved int
	if !protected {
		if err := db.NewRaw("SELECT COUNT(*) FROM operative_provider_attempts WHERE state IN ('in_flight','ambiguous')").Scan(ctx, &unresolved); err != nil {
			return err
		}
		if unresolved != 0 {
			return ports.ErrConflict
		}
	}
	if !protected || !probes {
		if err := db.NewRaw("SELECT COUNT(*) FROM operative_cutover_state WHERE barrier_id IS NOT NULL AND phase NOT IN ('preparing','released')").Scan(ctx, &unresolved); err != nil {
			return err
		}
		if unresolved != 0 {
			return ports.ErrConflict
		}
		if err := db.NewRaw("SELECT COUNT(*) FROM operative_source_gates WHERE state = 'held'").Scan(ctx, &unresolved); err != nil {
			return err
		}
		if unresolved != 0 {
			return ports.ErrConflict
		}
	}
	return nil
}
