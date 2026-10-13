package edge

import (
	"context"
	"testing"

	"github.com/uptrace/bun"
)

// TestEdgeDeliveryPreparationMigration preserves populated old command receipts
// through both directions and enforces the kind-specific preparation success.
func TestEdgeDeliveryPreparationMigration(t *testing.T) {
	s, _, authority, c := ackFixture(t)
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), authority, c); err != nil {
		t.Fatal(err)
	}
	execMigration := func(name string) error {
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		return s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error { _, err := tx.ExecContext(ctx, string(data)); return err })
	}
	var before edgeCommandRow
	if err := s.db.NewSelect().Model(&before).Where("command_id=?", c.CommandID).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := execMigration("016_delivery_preparation.tx.down.sql"); err != nil {
		t.Fatal(err)
	}
	if err := execMigration("016_delivery_preparation.tx.up.sql"); err != nil {
		t.Fatal(err)
	}
	var after edgeCommandRow
	if err := s.db.NewSelect().Model(&after).Where("command_id=?", c.CommandID).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !sameMigrationReceipt(before, after) {
		t.Fatal("migration changed existing command receipt")
	}
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_applied_commands SET code='delivery_prepared' WHERE command_id=?", c.CommandID); err == nil {
		t.Fatal("old command acquired preparation success code")
	}
}
func sameMigrationReceipt(a, b edgeCommandRow) bool {
	return a.CommandID == b.CommandID && a.Kind == b.Kind && string(a.RequestHash) == string(b.RequestHash) && a.Status == b.Status && a.Code == b.Code && a.Message == b.Message && a.RetainUntil == b.RetainUntil && a.AppliedAt != nil && b.AppliedAt != nil && *a.AppliedAt == *b.AppliedAt
}
