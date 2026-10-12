package edge

// Effect-based verification for the bounded M7 edge regional application state.
// These tests drive the real SQLite store: effective regional facts are written
// only through the configuration activation transaction and consumed inside the
// actual availability recording, escalation enqueue and final send authorization
// transactions. Desired aggregate/both modes never authorize anything here: the
// edge runtime is regional-only, and absent legacy facts keep the existing
// regional behavior while supplying no application evidence.

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// exactSetRevision sits above JavaScript's exact integer range (2^53), so a
// counter carried through document decoding must survive storage and cold
// reopen without collapsing into a rounded value.
const exactSetRevision = int64(9007199254740993)

const (
	regionalForeignStream = "9c1b6f5a-2e4d-4a5b-9c8d-0e1f2a3b4c5d"
	regionalNextStream    = "7a6b5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c5e"
	regionalForeignTarget = "11111111-1111-4111-8111-111111111111"
)

// regionalFact builds one source-local effective fact bound to the exact
// authenticated document identity and the accepted stream, the same tuple the
// service factory derives from a resolved document.
func regionalFact(config domain.EdgeActiveConfig, monitorID, generation, setRevision int64) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  config.Snapshot.ProbeConfigTarget,
		MonitorID:          monitorID,
		AssignmentRevision: setRevision,
		Generation:         generation,
		ConfigRevision:     config.Snapshot.Revision,
		SHA256:             config.Snapshot.SHA256,
		StreamID:           testIdentity().StreamID,
		AlertDelivery:      domain.AlertDeliveryRegional,
	}
}

func regionalWithFacts(config domain.EdgeActiveConfig, facts ...domain.ProbeDeliveryModeApplication) domain.EdgeActiveConfig {
	config.RegionalDeliveryApplications = facts
	return config
}

// regionalTwoMonitorConfig mirrors protectedConfig(t, 1) with a second bound
// assignment so a rejected activation can be observed rolling back a first,
// already-inserted valid fact alongside the invalid one.
func regionalTwoMonitorConfig(t *testing.T) domain.EdgeActiveConfig {
	t.Helper()
	config := protectedConfig(t, 1)
	config.Assignments = append(config.Assignments, domain.EdgeAssignmentIdentity{MonitorID: 18, Generation: 1, Active: true})
	return config
}

func regionalEnrolledStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	return s, dir
}

func regionalStore(t *testing.T, config domain.EdgeActiveConfig) (*Store, string) {
	t.Helper()
	s, dir := regionalEnrolledStore(t)
	if err := s.ActivateConfig(t.Context(), config); err != nil {
		t.Fatalf("activate configuration: %v", err)
	}
	return s, dir
}

func regionalCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var value int
	if err := s.db.NewRaw(query, args...).Scan(t.Context(), &value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func regionalFacts(t *testing.T, s *Store) []domain.ProbeDeliveryModeApplication {
	t.Helper()
	active, err := s.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatalf("read active configuration: %v", err)
	}
	return active.RegionalDeliveryApplications
}

// regionalClaim copies the durable attempt fence out of stored work.
func regionalClaim(item domain.QueuedDelivery) domain.DeliveryClaim {
	lease := item.LeaseToken
	return domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: lease}
}

// assertNoRegionalEffects proves a rejected source write left no partial
// recording behind: no telemetry, incident, intent, state row or sequence.
func assertNoRegionalEffects(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"edge_telemetry_outbox", "edge_alerts", "edge_delivery_outbox", "edge_regional_state"} {
		if got := regionalCount(t, s, "SELECT COUNT(*) FROM "+table); got != 0 {
			t.Fatalf("%s escaped rollback: %d", table, got)
		}
	}
	if got := regionalCount(t, s, "SELECT last_created_seq FROM edge_identity WHERE id = 1"); got != 0 {
		t.Fatalf("sequence escaped rollback: %d", got)
	}
}

// TestEdgeRegionalApplicationExactFactsRetainedAcrossReopen asserts the exact
// fact tuple — including an assignment-set revision above JavaScript precision —
// survives a duplicate exact retry and a cold store reopen unchanged.
func TestEdgeRegionalApplicationExactFactsRetainedAcrossReopen(t *testing.T) {
	ctx := t.Context()
	config := regionalTwoMonitorConfig(t)
	facts := []domain.ProbeDeliveryModeApplication{
		regionalFact(config, 17, 1, exactSetRevision),
		regionalFact(config, 18, 1, exactSetRevision+2),
	}
	config = regionalWithFacts(config, facts...)
	s, dir := regionalStore(t, config)

	got, err := s.ReadActiveConfig(ctx)
	if err != nil || !slices.Equal(got.RegionalDeliveryApplications, facts) {
		t.Fatalf("exact facts not retained: %+v %v", got.RegionalDeliveryApplications, err)
	}

	// A duplicate exact retry must not rewrite ciphertext, activation time or
	// already-established facts.
	first := got
	if err := s.ActivateConfig(ctx, config); err != nil {
		t.Fatalf("duplicate exact retry: %v", err)
	}
	again, err := s.ReadActiveConfig(ctx)
	if err != nil || !again.AppliedAt.Equal(first.AppliedAt) ||
		!bytes.Equal(again.Snapshot.ProtectedPayload, first.Snapshot.ProtectedPayload) ||
		!slices.Equal(again.RegionalDeliveryApplications, facts) {
		t.Fatalf("exact retry rewrote protected state or facts: %+v %v", again, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	cold := regionalFacts(t, reopened)
	if len(cold) != 2 || cold[0].AssignmentRevision != exactSetRevision || cold[1].AssignmentRevision != exactSetRevision+2 {
		t.Fatalf("cold reopen lost the exact counter: %+v", cold)
	}
	if !slices.Equal(cold, facts) {
		t.Fatalf("cold reopen altered facts: %+v", cold)
	}
}

// TestEdgeRegionalApplicationUpgradeRetryEstablishesOnlyExactDocument asserts
// pre-upgrade absence is never backfilled, that only a retry of the exact
// authenticated document can establish its missing facts, and that established
// or corrupt rows are never rewritten or repaired.
func TestEdgeRegionalApplicationUpgradeRetryEstablishesOnlyExactDocument(t *testing.T) {
	ctx := t.Context()
	config := regionalTwoMonitorConfig(t)
	s, _ := regionalStore(t, config) // pre-upgrade shape: activated before facts existed

	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
		t.Fatalf("activation backfilled application facts: %d", got)
	}
	if facts := regionalFacts(t, s); len(facts) != 0 {
		t.Fatalf("legacy absence reported as application: %+v", facts)
	}
	legacy, err := s.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}

	fact17 := regionalFact(config, 17, 1, exactSetRevision)
	fact18 := regionalFact(config, 18, 1, exactSetRevision)
	if err := s.ActivateConfig(ctx, regionalWithFacts(config, fact17, fact18)); err != nil {
		t.Fatalf("same-document upgrade retry: %v", err)
	}
	established := regionalFacts(t, s)
	if !slices.Equal(established, []domain.ProbeDeliveryModeApplication{fact17, fact18}) {
		t.Fatalf("facts not established exactly: %+v", established)
	}
	after, err := s.ReadActiveConfig(ctx)
	if err != nil || !after.AppliedAt.Equal(legacy.AppliedAt) ||
		!bytes.Equal(after.Snapshot.ProtectedPayload, legacy.Snapshot.ProtectedPayload) {
		t.Fatalf("establishing facts rewrote ciphertext or activation time: %+v %v", after, err)
	}

	// A different set revision for an established monitor must never rewrite
	// the stored fact: identity is compared exactly or refused.
	conflicting := regionalFact(config, 17, 1, exactSetRevision+1)
	if err := s.ActivateConfig(ctx, regionalWithFacts(config, conflicting, fact18)); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("fact rewrite accepted: %v", err)
	}
	if got := regionalFacts(t, s); !slices.Equal(got, established) {
		t.Fatalf("refused retry altered facts: %+v", got)
	}

	// A retry that omits already-established facts is a no-op, never a deletion.
	if err := s.ActivateConfig(ctx, regionalWithFacts(config, fact18)); err != nil {
		t.Fatalf("subset retry: %v", err)
	}
	if got := regionalFacts(t, s); !slices.Equal(got, established) {
		t.Fatalf("subset retry altered facts: %+v", got)
	}

	// Corrupt present state is refused and never repaired by a retry.
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET sha256 = ? WHERE monitor_id = 17", strings.Repeat("g", 64)); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConfig(ctx, regionalWithFacts(config, fact17, fact18)); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("corrupt fact repaired or ignored: %v", err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE monitor_id = 17 AND sha256 = ?", strings.Repeat("g", 64)); got != 1 {
		t.Fatalf("corrupt row rewritten: %d", got)
	}
}

// TestEdgeRegionalApplicationActivationRejectsInvalidFactsAtomically asserts
// every broken binding — target, hash, document, stream, monitor, generation,
// duplicates, non-regional effective modes and non-positive set revisions — is
// refused before any fact or active pointer survives the transaction.
func TestEdgeRegionalApplicationActivationRejectsInvalidFactsAtomically(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name  string
		want  error
		facts func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication
	}{
		{"foreign hub target", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.ProbeConfigTarget.HubID = regionalForeignTarget
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"foreign probe target", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.ProbeConfigTarget.ProbeID = regionalForeignTarget
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"document hash", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.SHA256 = strings.Repeat("f", 64)
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"document revision", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.ConfigRevision = 2
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"foreign stream", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.StreamID = regionalForeignStream
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"unbound monitor", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 19, 1, exactSetRevision)
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"stale generation", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 2, exactSetRevision)
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"duplicate monitor", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), regionalFact(config, 17, 1, exactSetRevision)}
		}},
		{"aggregate effective mode", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.AlertDelivery = domain.AlertDeliveryAggregate
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"both effective mode", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, exactSetRevision)
			second.AlertDelivery = domain.AlertDeliveryBoth
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
		{"nonpositive assignment revision", domain.ErrValidation, func(config domain.EdgeActiveConfig) []domain.ProbeDeliveryModeApplication {
			second := regionalFact(config, 18, 1, 0)
			return []domain.ProbeDeliveryModeApplication{regionalFact(config, 17, 1, exactSetRevision), second}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := regionalEnrolledStore(t)
			config := regionalTwoMonitorConfig(t)
			if err := s.ActivateConfig(ctx, regionalWithFacts(config, tc.facts(config)...)); !errors.Is(err, tc.want) {
				t.Fatalf("invalid facts accepted: %v", err)
			}
			if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
				t.Fatalf("rejected facts escaped rollback: %d", got)
			}
			if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_config"); got != 0 {
				t.Fatalf("rejected activation escaped rollback: %d", got)
			}
			if _, err := s.ReadActiveConfig(ctx); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("rejected activation moved the pointer: %v", err)
			}
		})
	}

	t.Run("stale connection fence", func(t *testing.T) {
		s, _ := regionalEnrolledStore(t)
		if err := s.AcceptConnectionGeneration(ctx, testHubID, 2); err != nil {
			t.Fatal(err)
		}
		config := regionalTwoMonitorConfig(t)
		if err := s.ActivateConfig(ctx, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision))); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("stale connection generation accepted: %v", err)
		}
		if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
			t.Fatalf("stale activation escaped rollback: %d", got)
		}
		if _, err := s.ReadActiveConfig(ctx); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("stale activation moved the pointer: %v", err)
		}
	})
}

// TestEdgeRegionalApplicationStreamIsolation asserts facts stay bound to their
// source stream: rows of another stream never leak into reads or consumers, and
// a change of the accepted stream reports legacy absence instead of rebinding
// the old rows. The authenticated rollover lifecycle itself is covered by the
// stream reset tests; this exercises the same selection predicate directly.
func TestEdgeRegionalApplicationStreamIsolation(t *testing.T) {
	ctx := t.Context()
	config := protectedConfig(t, 1)
	current := regionalFact(config, 17, 1, exactSetRevision)
	s, _ := regionalStore(t, regionalWithFacts(config, current))

	if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
		(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
		VALUES (1, 17, ?, ?, 1, ?, 'regional')`, regionalForeignStream, exactSetRevision, config.Snapshot.SHA256); err != nil {
		t.Fatal(err)
	}
	if got := regionalFacts(t, s); !slices.Equal(got, []domain.ProbeDeliveryModeApplication{current}) {
		t.Fatalf("foreign stream fact leaked into reads: %+v", got)
	}

	// Corruption on another stream must neither fail current reads nor gate
	// current consumers, and must never be repaired away.
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET sha256 = ? WHERE stream_id = ?", strings.Repeat("g", 64), regionalForeignStream); err != nil {
		t.Fatal(err)
	}
	if got := regionalFacts(t, s); !slices.Equal(got, []domain.ProbeDeliveryModeApplication{current}) {
		t.Fatalf("foreign corruption reached current reads: %+v", got)
	}
	if got, err := s.CommitEdgeCheck(ctx, checkRecord()); err != nil || got.Seq != 1 {
		t.Fatalf("recording gated by foreign stream state: %+v %v", got, err)
	}

	// A later accepted stream must not inherit the old stream's facts.
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_identity SET stream_id = ? WHERE id = 1", regionalNextStream); err != nil {
		t.Fatal(err)
	}
	if got := regionalFacts(t, s); len(got) != 0 {
		t.Fatalf("stream change rebound old facts: %+v", got)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE stream_id = ?", testIdentity().StreamID); got != 1 {
		t.Fatalf("old stream fact lost after stream change: %d", got)
	}
}

// TestEdgeRegionalApplicationCorruptPresentStateRefused asserts present facts
// that no longer match the retained document fail explicitly everywhere they
// are read, with zero recorded effects, and are never silently repaired. The
// schema itself refuses to store anything but a regional effective mode.
func TestEdgeRegionalApplicationCorruptPresentStateRefused(t *testing.T) {
	ctx := t.Context()
	for name, corrupt := range map[string]string{
		"nonhex hash":       strings.Repeat("g", 64),
		"noncanonical hash": strings.Repeat("A", 64),
	} {
		t.Run(name, func(t *testing.T) {
			config := protectedConfig(t, 1)
			s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))
			if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET sha256 = ?", corrupt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadActiveConfig(ctx); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("corrupt present facts accepted by read: %v", err)
			}
			if _, err := s.CommitEdgeCheck(ctx, checkRecord()); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("corrupt present facts recorded anyway: %v", err)
			}
			assertNoRegionalEffects(t, s)
			if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE sha256 = ?", corrupt); got != 1 {
				t.Fatalf("corrupt row repaired: %d", got)
			}
		})
	}

	t.Run("schema refuses non-regional facts", func(t *testing.T) {
		config := protectedConfig(t, 1)
		s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))
		for _, mode := range []string{"aggregate", "both"} {
			if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET alert_delivery = ? WHERE monitor_id = 17", mode); err == nil {
				t.Fatalf("schema stored %s effective mode", mode)
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
				(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
				VALUES (1, 18, ?, ?, 1, ?, ?)`, testIdentity().StreamID, exactSetRevision, config.Snapshot.SHA256, mode); err == nil {
				t.Fatalf("schema accepted %s effective mode", mode)
			}
		}
	})
}

// TestEdgeRegionalApplicationGatesAvailabilityRecording asserts the real
// CommitEdgeCheck transaction consults the helper: valid facts record with full
// persisted effects, while a present fact bound to the wrong generation refuses
// the whole write without leaving a partial record behind.
func TestEdgeRegionalApplicationGatesAvailabilityRecording(t *testing.T) {
	ctx := t.Context()

	t.Run("valid facts record", func(t *testing.T) {
		config := protectedConfig(t, 1)
		s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))
		got, err := s.CommitEdgeCheck(ctx, checkRecord())
		if err != nil || got.Seq != 1 {
			t.Fatalf("valid facts refused recording: %+v %v", got, err)
		}
		for table, want := range map[string]int{"edge_telemetry_outbox": 2, "edge_alerts": 1, "edge_delivery_outbox": 1, "edge_regional_state": 1} {
			if got := regionalCount(t, s, "SELECT COUNT(*) FROM "+table); got != want {
				t.Fatalf("%s = %d, want %d", table, got, want)
			}
		}
		if got := regionalCount(t, s, "SELECT last_created_seq FROM edge_identity WHERE id = 1"); got != 2 {
			t.Fatalf("observation+incident not sequenced: %d", got)
		}
	})

	t.Run("stale generation fact refuses", func(t *testing.T) {
		config := protectedConfig(t, 1)
		s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))
		// Shape-valid and selected by the read, but bound to the wrong
		// assignment generation: only the consumer helper can refuse it.
		if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 2"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitEdgeCheck(ctx, checkRecord()); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("stale-generation fact recorded anyway: %v", err)
		}
		assertNoRegionalEffects(t, s)
		if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE generation = 2"); got != 1 {
			t.Fatalf("stale fact rewritten by refusal: %d", got)
		}
	})
}

// TestEdgeRegionalApplicationLegacyAbsenceStaysExecutable asserts missing
// pre-upgrade facts are reported as unreported and never suppress the regional
// runtime: recording and final send authorization keep working unchanged.
func TestEdgeRegionalApplicationLegacyAbsenceStaysExecutable(t *testing.T) {
	ctx := t.Context()
	config := protectedConfig(t, 1)
	s, _ := regionalStore(t, config)

	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
		t.Fatalf("legacy activation created facts: %d", got)
	}
	active, err := s.ReadActiveConfig(ctx)
	if err != nil || len(active.RegionalDeliveryApplications) != 0 {
		t.Fatalf("legacy absence reported as application: %+v %v", active.RegionalDeliveryApplications, err)
	}

	got, err := s.CommitEdgeCheck(ctx, checkRecord())
	if err != nil || got.Seq != 1 {
		t.Fatalf("legacy absence suppressed recording: %+v %v", got, err)
	}
	items, err := s.ClaimDeliveries(ctx, testIdentity().ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim delivery: %+v %v", items, err)
	}
	authorized, err := s.AuthorizeEdgeDelivery(ctx, regionalClaim(items[0]), config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil {
		t.Fatalf("legacy absence suppressed final send authorization: %+v %v", authorized, err)
	}
}

// TestEdgeRegionalApplicationGatesFinalSendAuthorization asserts the final
// provider authorization transaction re-consults present state per attempt:
// valid facts authorize the stored item, a stale-generation fact is refused by
// the helper, and corrupt facts fail explicitly instead of falling back.
func TestEdgeRegionalApplicationGatesFinalSendAuthorization(t *testing.T) {
	ctx := t.Context()
	config := protectedConfig(t, 1)
	s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))
	if _, err := s.CommitEdgeCheck(ctx, checkRecord()); err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimDeliveries(ctx, testIdentity().ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim delivery: %+v %v", items, err)
	}
	claim := regionalClaim(items[0])

	authorized, err := s.AuthorizeEdgeDelivery(ctx, claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil || authorized.DeliveryID != claim.DeliveryID {
		t.Fatalf("valid facts refused final send: %+v %v", authorized, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 2"); err != nil {
		t.Fatal(err)
	}
	if authorized, err := s.AuthorizeEdgeDelivery(ctx, claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second); !errors.Is(err, ports.ErrConflict) || authorized != nil {
		t.Fatalf("stale-generation fact authorized a send: %+v %v", authorized, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 1, sha256 = ?", strings.Repeat("g", 64)); err != nil {
		t.Fatal(err)
	}
	if authorized, err := s.AuthorizeEdgeDelivery(ctx, claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second); !errors.Is(err, ports.ErrConflict) || authorized != nil {
		t.Fatalf("corrupt fact authorized a send: %+v %v", authorized, err)
	}
}

// TestEdgeRegionalApplicationGatesEscalationEnqueue asserts the escalation
// transition transaction consults the helper before creating rung intents: a
// stale-generation fact refuses the enqueue with no persisted effect, while the
// restored exact fact commits the same rung.
func TestEdgeRegionalApplicationGatesEscalationEnqueue(t *testing.T) {
	ctx := t.Context()
	config := protectedConfig(t, 1)
	s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))

	rec := checkRecord()
	at := rec.Observation.ObservedAt
	step := int64(1)
	rec.Incident.EscalationPolicyID = 30
	rec.Incident.EscalationPolicyVersion = 1
	rec.Incident.EscalationStatus = domain.EscalationStatePending
	rec.Incident.EscalationNextStep = &step
	rec.Incident.EscalationNextRunAt = &at
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	policy := &domain.EscalationPolicy{ID: 30, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{10}}, {StepOrder: 2, NotificationIDs: []int64{10}}}}
	current := *rec.Incident
	if _, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at); !changed {
		t.Fatal("escalation ladder did not advance")
	}
	intent := domain.DeliveryIntent{DeliveryID: "11111111-1111-4111-8111-111111111111", SourceAlertID: current.SourceAlertID, SourceTransitionVersion: 2, ProbeID: current.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at, EscalationPolicyID: 30, EscalationStep: 1}

	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitEscalationAdvance(ctx, 1, current, []domain.DeliveryIntent{intent}, "step one", at); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale-generation fact enqueued escalation work: %v", err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_delivery_outbox"); got != 1 {
		t.Fatalf("refused enqueue persisted intents: %d", got)
	}
	if got := regionalCount(t, s, "SELECT transition_version FROM edge_alerts"); got != 1 {
		t.Fatalf("refused enqueue advanced the incident: %d", got)
	}
	if got := regionalCount(t, s, "SELECT last_created_seq FROM edge_identity WHERE id = 1"); got != 2 {
		t.Fatalf("refused enqueue consumed sequence: %d", got)
	}

	// Restoring the exact fact (test-side, never a production repair path)
	// proves the refusal was the application gate alone.
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitEscalationAdvance(ctx, 1, current, []domain.DeliveryIntent{intent}, "step one", at); err != nil {
		t.Fatalf("valid fact refused escalation enqueue: %v", err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_delivery_outbox"); got != 2 {
		t.Fatalf("escalation rung not persisted: %d", got)
	}
}

// TestEdgeRegionalApplicationLeavesCertificateAuthorizationUnchanged asserts
// the certificate subject keeps its own authorization identity: the regional
// availability helper is deliberately not consulted for it, so application
// state bound to a stale generation cannot block a certificate send.
func TestEdgeRegionalApplicationLeavesCertificateAuthorizationUnchanged(t *testing.T) {
	ctx := t.Context()
	config := protectedConfig(t, 1)
	s, _ := regionalStore(t, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision)))

	c, a := certAssignment()
	active, err := s.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Metadata = active.Snapshot.ProbeConfigMetadata
	start := time.Now().UTC().Truncate(time.Microsecond)
	certRecord(t, s, c, a, start, 5, start.AddDate(0, 0, 5))

	items, err := s.ClaimDeliveries(ctx, testIdentity().ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim certificate work: %+v %v", items, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE edge_regional_delivery_applications SET generation = 2"); err != nil {
		t.Fatal(err)
	}
	authorized, err := s.AuthorizeEdgeDelivery(ctx, regionalClaim(items[0]), c.Metadata, 10*time.Second)
	if err != nil || authorized == nil || authorized.Certificate == nil {
		t.Fatalf("certificate send gated by regional application state: %+v %v", authorized, err)
	}
}

// TestEdgeRegionalApplicationMigrationLifecycleAndCleanup asserts the shipped
// migration pair: creation backfills nothing, facts exist only under a retained
// document, retirement cascades exactly the retired document's facts, and the
// down/up companions round-trip the exact constraints without fabricating rows.
func TestEdgeRegionalApplicationMigrationLifecycleAndCleanup(t *testing.T) {
	ctx := t.Context()
	s, _ := regionalEnrolledStore(t)

	if got := regionalCount(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'edge_regional_delivery_applications'"); got != 1 {
		t.Fatalf("regional application table missing: %d", got)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
		t.Fatalf("migration backfilled facts: %d", got)
	}

	// Neither does a pre-upgrade style activation of a legacy document.
	config := protectedConfig(t, 1)
	if err := s.ActivateConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
		t.Fatalf("legacy activation backfilled facts: %d", got)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
		(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
		VALUES (999, 17, ?, ?, 1, ?, 'regional')`, testIdentity().StreamID, exactSetRevision, config.Snapshot.SHA256); err == nil {
		t.Fatal("orphan application fact accepted")
	}

	// Establish facts under revision 1 via the exact-document retry, then a
	// newer document's own facts under revision 2.
	if err := s.ActivateConfig(ctx, regionalWithFacts(config, regionalFact(config, 17, 1, exactSetRevision))); err != nil {
		t.Fatal(err)
	}
	next := protectedConfig(t, 2)
	next.Assignments = []domain.EdgeAssignmentIdentity{{MonitorID: 17, Generation: 2, Active: true}}
	next = regionalWithFacts(next, regionalFact(next, 17, 2, exactSetRevision))
	if err := s.ActivateConfig(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE config_revision = 1"); got != 1 {
		t.Fatalf("revision 1 facts missing: %d", got)
	}

	// Retiring an old document cascades exactly its own facts.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM edge_config WHERE revision = 1"); err != nil {
		t.Fatal(err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE config_revision = 1"); got != 0 {
		t.Fatalf("retired document kept facts: %d", got)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE config_revision = 2"); got != 1 {
		t.Fatalf("current document lost facts to cascade: %d", got)
	}

	// The shipped down companion drops only its table; the up companion
	// rebuilds the exact constraints and backfills nothing.
	down, err := fs.ReadFile(migrations, "migrations/015_regional_delivery_application.tx.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("down migration: %v", err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'edge_regional_delivery_applications'"); got != 0 {
		t.Fatalf("down migration left table behind: %d", got)
	}
	up, err := fs.ReadFile(migrations, "migrations/015_regional_delivery_application.tx.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(up)); err != nil {
		t.Fatalf("up migration: %v", err)
	}
	if got := regionalCount(t, s, "SELECT COUNT(*) FROM edge_regional_delivery_applications"); got != 0 {
		t.Fatalf("up migration backfilled facts: %d", got)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
		(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
		VALUES (2, 18, ?, ?, 1, ?, 'aggregate')`, testIdentity().StreamID, exactSetRevision, next.Snapshot.SHA256); err == nil {
		t.Fatal("rebuilt schema lost the regional-only CHECK")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
		(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
		VALUES (999, 18, ?, ?, 1, ?, 'regional')`, testIdentity().StreamID, exactSetRevision, next.Snapshot.SHA256); err == nil {
		t.Fatal("rebuilt schema lost its document FK")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO edge_regional_delivery_applications
		(config_revision, monitor_id, stream_id, assignment_revision, generation, sha256, alert_delivery)
		VALUES (2, 18, ?, ?, 1, ?, 'regional')`, testIdentity().StreamID, exactSetRevision, next.Snapshot.SHA256); err != nil {
		t.Fatalf("rebuilt schema rejected a valid fact: %v", err)
	}
}
