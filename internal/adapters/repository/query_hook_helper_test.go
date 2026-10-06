package repository_test

import "github.com/uptrace/bun"

// attachInPlaceQueryHook attaches hook to db in place, mutating db itself.
//
// Bun 1.3 deprecated AddQueryHook in favor of WithQueryHook, but WithQueryHook
// returns a CLONE of the *bun.DB and leaves the original unmodified. The
// repository adapters under test hold a reference to the original *bun.DB, so
// a cloned handle would silently leave every barrier and diagnostic hook inert
// while the test still passed. The hook must therefore be attached to the same
// *bun.DB the adapters were constructed with, which is what this helper pins.
func attachInPlaceQueryHook(db *bun.DB, hook bun.QueryHook) {
	db.AddQueryHook(hook) //nolint:staticcheck // in-place attach required: WithQueryHook clones the DB and would leave the adapters' handle un-hooked
}
