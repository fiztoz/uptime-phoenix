package probe

import (
	"bytes"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/scheduler"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
	"github.com/google/uuid"
)

func TestEdgeMaintenanceRecordingAfterColdConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name, cron, at string
		active         bool
	}{
		{"spring before", "0 1 * * *", "2026-03-08T05:59:59Z", false},
		{"spring start", "0 1 * * *", "2026-03-08T06:00:00Z", true},
		{"spring active", "0 1 * * *", "2026-03-08T06:14:59Z", true},
		{"spring end", "0 1 * * *", "2026-03-08T06:15:00Z", false},
		{"spring skipped hour", "0 2 * * *", "2026-03-08T07:05:00Z", false},
		{"fall first hour", "0 1 * * *", "2026-11-01T05:05:00Z", true},
		{"fall first end", "0 1 * * *", "2026-11-01T05:15:00Z", false},
		{"fall second hour", "0 1 * * *", "2026-11-01T06:05:00Z", true},
		{"fall second end", "0 1 * * *", "2026-11-01T06:15:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := m2Config(t)
			snapshot.MaintenanceWindows[0].Timezone = "America/New_York"
			snapshot.MaintenanceWindows[0].CronExpr = tc.cron
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			identity := domain.EdgeIdentity{ProbeID: snapshot.ProbeID, StreamID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}
			store, err := edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			now := time.Now().UTC()
			hash := sha256.Sum256([]byte("maintenance-test"))
			if err := store.IssueEnrollmentToken(t.Context(), domain.EdgeEnrollmentToken{Hash: hash, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if err := store.CommitEnrollment(t.Context(), hash, domain.EdgeEnrollment{HubID: snapshot.HubID, ProbeID: snapshot.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1, TokenHash: hash, AppliedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.AcceptConnectionGeneration(t.Context(), snapshot.HubID, 1); err != nil {
				t.Fatal(err)
			}
			protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x73}, 32))
			if err != nil {
				t.Fatal(err)
			}
			decoder := NewEdgeConfigDecoder(checker.Get, notifier.Get)
			if _, err := services.NewEdgeConfigService(store, store, decoder, protector).Apply(t.Context(), configBytes(t, snapshot), 1, now); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
			if err != nil {
				t.Fatal(err)
			}
			config, err := services.NewEdgeConfigService(store, store, decoder, protector).Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			// The production scheduler skips the checker and supplies UP in maintenance.
			recorder := services.NewEdgeRecordingService(store, store, scheduler.NewCronEvaluator())
			for _, raw := range []domain.Status{domain.StatusUp, domain.StatusDown} {
				observation, err := recorder.Record(t.Context(), config, config.Assignments[0], ports.CheckResult{Status: raw}, at)
				if err != nil || observation.Seq == 0 {
					t.Fatalf("record raw %v: %+v %v", raw, observation, err)
				}
				if tc.active && (observation.Status != domain.StatusMaintenance || observation.RawStatus != domain.StatusMaintenance || observation.DownCount != 0) {
					t.Fatalf("invalid maintenance evidence: %+v", observation)
				}
				if !tc.active && observation.Status == domain.StatusMaintenance {
					t.Fatalf("outside window suppressed: %+v", observation)
				}
				if _, err := (EdgeTelemetryEncoder{}).EncodeObservation(observation); err != nil {
					t.Fatal(err)
				}
			}
			evidence, err := store.ReadEdgeEvidence(t.Context(), 42, 3)
			if err != nil || evidence.State == nil || evidence.State.Seq != 2 {
				t.Fatalf("lost committed state: %+v %v", evidence, err)
			}
			if tc.active && evidence.Incident != nil {
				t.Fatalf("maintenance opened incident: %+v", evidence.Incident)
			}
			deliveries, err := store.ClaimDeliveries(t.Context(), snapshot.ProbeID, now, time.Minute, 10)
			if err != nil || len(deliveries) != 0 {
				t.Fatalf("maintenance/retry emitted notification: %+v %v", deliveries, err)
			}
		})
	}
}
