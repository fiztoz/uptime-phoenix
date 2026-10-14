package edge

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestEdgeRegionalApplicationResetLifecycle crosses the actual authenticated
// archive/reset/reopen machinery. The application is seeded in this storage
// fixture; live document decoding/production is covered by the session contract.
func TestEdgeRegionalApplicationResetLifecycle(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "retained_old_stream"
		if corrupt {
			name = "corrupt_present_fact_refuses_reset"
		}
		t.Run(name, func(t *testing.T) {
			s, dir, protector, plan := streamResetFixture(t)
			active, err := s.ReadActiveConfig(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			fact := regionalFact(active, 17, 1, exactSetRevision)
			if corrupt {
				fact.SHA256 = regionalForeignTarget + regionalForeignTarget[:28]
			}
			if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_regional_delivery_applications
			 (config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`, fact.ConfigRevision, fact.MonitorID, fact.StreamID, fact.AssignmentRevision, fact.Generation, fact.SHA256, fact.AlertDelivery); err != nil {
				t.Fatal(err)
			}
			result, err := s.ResetStream(t.Context(), plan)
			if corrupt {
				if !errors.Is(err, ports.ErrConflict) {
					t.Fatal("reset accepted corrupt present application", err)
				}
				assertResetSource(t, s, plan.PreviousStreamID, 2)
				return
			}
			if err != nil || result.State != "applied" {
				t.Fatal("fact-bearing reset failed", err)
			}
			assertResetSource(t, s, plan.StreamID, 0)
			if got := regionalFacts(t, s); len(got) != 0 {
				t.Fatal("reset rebound old-stream application")
			}
			if n := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE stream_id = ?", plan.PreviousStreamID); n != 1 {
				t.Fatal("reset erased original stream identity")
			}
			archive := immutableResetDB(t, filepath.Join(dir, resetArchiveDir, plan.ResetID, "edge.db"))
			var binding int64
			if err := archive.NewRaw("SELECT assignment_revision FROM edge_regional_delivery_applications WHERE stream_id = ?", plan.PreviousStreamID).Scan(t.Context(), &binding); err != nil || binding != exactSetRevision {
				t.Fatal("archive lost immutable application", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			current, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(protector))
			if err != nil {
				t.Fatal("reopen after reset", err)
			}
			defer func() { _ = current.Close() }()
			if len(regionalFacts(t, current)) != 0 {
				t.Fatal("cold reopen resurrected old-stream application")
			}
		})
	}
}
