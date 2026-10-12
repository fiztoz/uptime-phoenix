package services

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Pure completeness tests for the unwired M7 source-cutover evidence proposal.
// Evidence is synthetic and self-consistent: nothing here proves database
// provenance, capability negotiation or runtime mode application. A complete
// decision is never an activation, a send permit or a suppression permit.

const (
	cutoverHubID          = "11111111-1111-4111-8111-111111111111"
	cutoverForeignID      = "99999999-9999-4999-8999-999999999999"
	cutoverProbeB         = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cutoverProbeC         = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cutoverStreamLocal    = "12345678-1234-4234-8234-123456789abc"
	cutoverStreamB        = "87654321-4321-4321-8321-cba987654321"
	cutoverStreamC        = "abcdefab-cdef-4abc-8def-abcdefabcdef"
	cutoverStreamRollover = "aaaa0000-bbbb-4ccc-8ddd-eeee0000ffff"
	cutoverHashLocal      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cutoverHashB          = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	cutoverHashC          = "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	cutoverHashOther      = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// deliveryCutoverSource builds one fully self-consistent source: active pointer
// (config revision/hash in the probe-document space) plus a separate attestation
// (assignment-set revision in its own identity space). setRevision 7 and document
// revisions 42/57 differ on purpose: the spaces are independent.
func deliveryCutoverSource(now time.Time, member domain.ProbeAssignment, stream, hash string, configRevision, setRevision int64, mode domain.AlertDelivery) domain.DeliveryCutoverSource {
	target := domain.ProbeConfigTarget{HubID: cutoverHubID, ProbeID: member.ProbeID}
	return domain.DeliveryCutoverSource{
		ProbeID:                  member.ProbeID,
		StreamID:                 stream,
		ModeApplicationSupported: true,
		AcknowledgedConfig: &domain.ProbeActiveConfig{
			ProbeConfigTarget: target,
			Revision:          configRevision,
			SHA256:            hash,
			AppliedAt:         now.Add(-time.Minute),
			AssignmentCount:   2,
		},
		Assignment: &domain.AcknowledgedAssignmentDelivery{
			MonitorID:          member.MonitorID,
			Generation:         member.Generation,
			AssignmentRevision: setRevision,
			AlertDelivery:      mode,
		},
		ModeApplication: &domain.ProbeDeliveryModeApplication{
			ProbeConfigTarget:  target,
			MonitorID:          member.MonitorID,
			AssignmentRevision: setRevision,
			Generation:         member.Generation,
			ConfigRevision:     configRevision,
			SHA256:             hash,
			StreamID:           stream,
			AlertDelivery:      mode,
		},
	}
}

// deliveryCutoverInput builds a current proposal with one local and one remote
// member and complete evidence for both. Membership never excludes anything.
func deliveryCutoverInput(mode domain.AlertDelivery) domain.DeliveryCutoverInput {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	set := domain.MonitorProbeAssignments{
		MonitorID:     42,
		Revision:      7,
		HealthPolicy:  domain.HealthPolicyAnyDown,
		AlertDelivery: mode,
		Assignments: []domain.ProbeAssignment{
			{MonitorID: 42, ProbeID: domain.LocalProbeID, Generation: 1},
			{MonitorID: 42, ProbeID: cutoverProbeB, Generation: 3},
		},
	}
	return domain.DeliveryCutoverInput{
		Now:         now,
		SnapshotAt:  now,
		Current:     true,
		HubID:       cutoverHubID,
		Assignments: set,
		Sources: []domain.DeliveryCutoverSource{
			deliveryCutoverSource(now, set.Assignments[0], cutoverStreamLocal, cutoverHashLocal, 42, set.Revision, mode),
			deliveryCutoverSource(now, set.Assignments[1], cutoverStreamB, cutoverHashB, 57, set.Revision, mode),
		},
	}
}

func deliveryCutoverItem(t *testing.T, decision domain.DeliveryCutoverDecision, probeID string) domain.DeliveryCutoverSourceDecision {
	t.Helper()
	found := false
	var item domain.DeliveryCutoverSourceDecision
	for _, candidate := range decision.Sources {
		if candidate.ProbeID != probeID {
			continue
		}
		if found {
			t.Fatalf("duplicate decision item for %s: %+v", probeID, decision.Sources)
		}
		found, item = true, candidate
	}
	if !found {
		t.Fatalf("missing decision item for %s: %+v", probeID, decision.Sources)
	}
	return item
}

func deliveryCutoverCloneInput(in domain.DeliveryCutoverInput) domain.DeliveryCutoverInput {
	out := in
	out.Assignments.Assignments = append([]domain.ProbeAssignment(nil), in.Assignments.Assignments...)
	out.Sources = make([]domain.DeliveryCutoverSource, len(in.Sources))
	for i, source := range in.Sources {
		out.Sources[i] = source
		if source.AcknowledgedConfig != nil {
			config := *source.AcknowledgedConfig
			out.Sources[i].AcknowledgedConfig = &config
		}
		if source.Assignment != nil {
			assignment := *source.Assignment
			out.Sources[i].Assignment = &assignment
		}
		if source.ModeApplication != nil {
			application := *source.ModeApplication
			out.Sources[i].ModeApplication = &application
		}
	}
	return out
}

func TestDeliveryCutoverRegionalAndLegacyNeverCutover(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode domain.AlertDelivery
	}{{"legacy", ""}, {"regional", domain.AlertDeliveryRegional}} {
		for _, evidence := range []string{"complete", "absent", "corrupt"} {
			t.Run(tc.name+"/"+evidence, func(t *testing.T) {
				input := deliveryCutoverInput(tc.mode)
				switch evidence {
				case "absent":
					input.Sources = nil
				case "corrupt":
					input.Sources = []domain.DeliveryCutoverSource{{ProbeID: domain.LocalProbeID}}
				}
				got, err := EvaluateDeliveryCutover(input)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != domain.DeliveryCutoverRegional || got.DesiredMode != domain.AlertDeliveryRegional || got.Reason != "" {
					t.Fatalf("regional must never request a cutover: %+v", got)
				}
				if len(got.Sources) != len(input.Assignments.Assignments) {
					t.Fatalf("membership: %+v", got.Sources)
				}
				for _, item := range got.Sources {
					if item.State != domain.DeliveryCutoverRegional || item.Reason != "" {
						t.Fatalf("regional item must carry no cutover reason: %+v", item)
					}
				}
			})
		}
	}
}

func TestDeliveryCutoverHistoricalRegionalStillNeedsNoCutover(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{"", domain.AlertDeliveryRegional} {
		input := deliveryCutoverInput(mode)
		input.Current = false
		input.SnapshotAt = input.Now.Add(-time.Minute)
		input.Sources = nil
		got, err := EvaluateDeliveryCutover(input)
		if err != nil || got.State != domain.DeliveryCutoverRegional || got.DesiredMode != domain.AlertDeliveryRegional || got.Reason != "" {
			t.Fatalf("historical regional configuration requests no cutover: %+v %v", got, err)
		}
	}
}

func TestDeliveryCutoverCompletesOnlyWithEveryMemberAttested(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode), func(t *testing.T) {
			// Set revision 7 must bind independently of document revisions 42/57.
			input := deliveryCutoverInput(mode)
			got, err := EvaluateDeliveryCutover(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.DeliveryCutoverEvidenceComplete || got.DesiredMode != mode || got.Reason != "" {
				t.Fatalf("got %+v", got)
			}
			if len(got.Sources) != 2 {
				t.Fatalf("want one item per assigned member: %+v", got.Sources)
			}
			for _, probeID := range []string{domain.LocalProbeID, cutoverProbeB} {
				item := deliveryCutoverItem(t, got, probeID)
				if item.State != domain.DeliveryCutoverEvidenceComplete || item.Reason != "" {
					t.Fatalf("item %+v", item)
				}
			}
		})
	}
}

func TestDeliveryCutoverConfigAckAndParsingSupportNeverComplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason domain.DeliveryCutoverReason
		change func(*domain.DeliveryCutoverSource)
	}{
		{"ack without attestation", domain.DeliveryCutoverApplicationUnreported,
			func(s *domain.DeliveryCutoverSource) { s.ModeApplication = nil }},
		{"parsing support only", domain.DeliveryCutoverApplicationUnsupported,
			func(s *domain.DeliveryCutoverSource) { s.ModeApplicationSupported = false }},
		{"ack without any application evidence", domain.DeliveryCutoverApplicationUnsupported,
			func(s *domain.DeliveryCutoverSource) {
				s.ModeApplicationSupported = false
				s.ModeApplication = nil
			}},
	} {
		for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				input := deliveryCutoverInput(mode)
				tc.change(&input.Sources[1])
				got, err := EvaluateDeliveryCutover(input)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != domain.DeliveryCutoverPending {
					t.Fatalf("config acknowledgement must never complete a cutover: %+v", got)
				}
				item := deliveryCutoverItem(t, got, cutoverProbeB)
				if item.State != domain.DeliveryCutoverPending || item.Reason != tc.reason {
					t.Fatalf("got %+v want %q", item, tc.reason)
				}
				if other := deliveryCutoverItem(t, got, domain.LocalProbeID); other.State != domain.DeliveryCutoverEvidenceComplete {
					t.Fatalf("other member evidence must be evaluated independently: %+v", other)
				}
			})
		}
	}
}

func TestDeliveryCutoverMissingMemberNeverShrinksMembership(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode), func(t *testing.T) {
			input := deliveryCutoverInput(mode)
			input.Sources = input.Sources[:1] // remote member is silent
			got, err := EvaluateDeliveryCutover(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.DeliveryCutoverPending || got.Reason != "" {
				t.Fatalf("got %+v", got)
			}
			if len(got.Sources) != 2 {
				t.Fatalf("missing evidence must not shrink membership: %+v", got.Sources)
			}
			missing := deliveryCutoverItem(t, got, cutoverProbeB)
			if missing.State != domain.DeliveryCutoverPending || missing.Reason != domain.DeliveryCutoverSourceUnreported {
				t.Fatalf("got %+v", missing)
			}
			if local := deliveryCutoverItem(t, got, domain.LocalProbeID); local.State != domain.DeliveryCutoverEvidenceComplete {
				t.Fatalf("got %+v", local)
			}
		})
	}
}

func TestDeliveryCutoverHistoricalAndEmptyMembershipStayPending(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode)+"/historical", func(t *testing.T) {
			input := deliveryCutoverInput(mode)
			input.Current = false
			input.SnapshotAt = input.Now.Add(-time.Second)
			got, err := EvaluateDeliveryCutover(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.DeliveryCutoverPending || got.Reason != domain.DeliveryCutoverHistoricalSnapshot {
				t.Fatalf("historical evidence must never complete: %+v", got)
			}
			for _, item := range got.Sources {
				if item.State != domain.DeliveryCutoverPending || item.Reason != domain.DeliveryCutoverHistoricalSnapshot {
					t.Fatalf("got %+v", item)
				}
			}
		})
		t.Run(string(mode)+"/historical_empty", func(t *testing.T) {
			input := deliveryCutoverInput(mode)
			input.Current = false
			input.SnapshotAt = input.Now.Add(-time.Second)
			input.Assignments.Assignments, input.Sources = nil, nil
			got, err := EvaluateDeliveryCutover(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.DeliveryCutoverPending || got.Reason != domain.DeliveryCutoverHistoricalSnapshot {
				t.Fatalf("historical reason must win over empty membership: %+v", got)
			}
		})
		t.Run(string(mode)+"/empty", func(t *testing.T) {
			input := deliveryCutoverInput(mode)
			input.Assignments.Assignments, input.Sources = nil, nil
			got, err := EvaluateDeliveryCutover(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != domain.DeliveryCutoverPending || got.Reason != domain.DeliveryCutoverNoAssignments {
				t.Fatalf("empty membership must never be vacuously complete: %+v", got)
			}
			if len(got.Sources) != 0 {
				t.Fatalf("got %+v", got.Sources)
			}
		})
	}
}

func TestDeliveryCutoverSourceEvidenceReasons(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		for _, tc := range []struct {
			name   string
			reason domain.DeliveryCutoverReason
			change func(*domain.DeliveryCutoverInput)
		}{
			{"source unreported", domain.DeliveryCutoverSourceUnreported,
				func(i *domain.DeliveryCutoverInput) { i.Sources = i.Sources[:1] }},
			{"mode application unsupported", domain.DeliveryCutoverApplicationUnsupported,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplicationSupported = false }},
			{"config unreported", domain.DeliveryCutoverConfigUnreported,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig = nil }},
			{"config foreign probe", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig.ProbeID = cutoverProbeC }},
			{"config foreign hub", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig.HubID = cutoverForeignID }},
			{"config hash corrupt", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) {
					i.Sources[1].AcknowledgedConfig.SHA256 = strings.ToUpper(cutoverHashB)
				}},
			{"config revision zero", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig.Revision = 0 }},
			{"config applied time zero", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig.AppliedAt = time.Time{} }},
			{"config applied after now", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) {
					i.Sources[1].AcknowledgedConfig.AppliedAt = i.Now.Add(time.Nanosecond)
				}},
			{"config assignment count zero", domain.DeliveryCutoverConfigUnverified,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].AcknowledgedConfig.AssignmentCount = 0 }},
			{"document assignment missing", domain.DeliveryCutoverAssignmentMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment = nil }},
			{"assignment monitor mismatch", domain.DeliveryCutoverAssignmentMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.MonitorID++ }},
			{"assignment generation readd", domain.DeliveryCutoverAssignmentMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.Generation++ }},
			{"document assignment revision absent", domain.DeliveryCutoverAssignmentRevisionMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.AssignmentRevision = 0 }},
			{"document assignment revision stale", domain.DeliveryCutoverAssignmentRevisionMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.AssignmentRevision-- }},
			{"document assignment revision future", domain.DeliveryCutoverAssignmentRevisionMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.AssignmentRevision++ }},
			{"document mode invalid", domain.DeliveryCutoverDocumentModeMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].Assignment.AlertDelivery = "bogus" }},
			{"document mode differs", domain.DeliveryCutoverDocumentModeMismatch,
				func(i *domain.DeliveryCutoverInput) {
					i.Sources[1].Assignment.AlertDelivery = domain.AlertDeliveryRegional
				}},
			{"application unreported", domain.DeliveryCutoverApplicationUnreported,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication = nil }},
			{"application malformed", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.SHA256 = "corrupt" }},
			{"application foreign hub", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.HubID = cutoverForeignID }},
			{"application foreign probe", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.ProbeID = cutoverProbeC }},
			{"application monitor mismatch", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.MonitorID++ }},
			{"application stale set revision", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.AssignmentRevision = 5 }},
			{"application generation readd", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.Generation++ }},
			{"application config revision", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.ConfigRevision++ }},
			{"application hash swap", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.SHA256 = cutoverHashOther }},
			{"application stream rollover", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) { i.Sources[1].ModeApplication.StreamID = cutoverStreamRollover }},
			{"application mode mismatch", domain.DeliveryCutoverApplicationMismatch,
				func(i *domain.DeliveryCutoverInput) {
					i.Sources[1].ModeApplication.AlertDelivery = domain.AlertDeliveryRegional
				}},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				input := deliveryCutoverInput(mode)
				tc.change(&input)
				got, err := EvaluateDeliveryCutover(input)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != domain.DeliveryCutoverPending {
					t.Fatalf("incomplete evidence must stay pending: %+v", got)
				}
				if len(got.Sources) != 2 {
					t.Fatalf("membership: %+v", got.Sources)
				}
				item := deliveryCutoverItem(t, got, cutoverProbeB)
				if item.State != domain.DeliveryCutoverPending || item.Reason != tc.reason {
					t.Fatalf("got %+v want %q", item, tc.reason)
				}
				if other := deliveryCutoverItem(t, got, domain.LocalProbeID); other.State != domain.DeliveryCutoverEvidenceComplete ||
					other.Reason != "" {
					t.Fatalf("got %+v", other)
				}
			})
		}
	}
}

func TestDeliveryCutoverAssignmentAndDocumentRevisionSpacesAreIndependent(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode), func(t *testing.T) {
			// Set revision 7 with document revisions 42/57 must complete when each
			// attestation binds its own space.
			input := deliveryCutoverInput(mode)
			got, err := EvaluateDeliveryCutover(input)
			if err != nil || got.State != domain.DeliveryCutoverEvidenceComplete {
				t.Fatalf("distinct revision spaces must complete: %+v %v", got, err)
			}
			for _, tc := range []struct {
				name   string
				change func(*domain.ProbeDeliveryModeApplication)
			}{
				{"crossed spaces", func(a *domain.ProbeDeliveryModeApplication) {
					a.AssignmentRevision, a.ConfigRevision = 57, 7
				}},
				{"document revision in set space", func(a *domain.ProbeDeliveryModeApplication) {
					a.AssignmentRevision = 57
				}},
				{"set revision in document space", func(a *domain.ProbeDeliveryModeApplication) {
					a.ConfigRevision = 7
				}},
				{"stale set revision proof", func(a *domain.ProbeDeliveryModeApplication) {
					a.AssignmentRevision = 5
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					i := deliveryCutoverInput(mode)
					tc.change(i.Sources[1].ModeApplication)
					got, err := EvaluateDeliveryCutover(i)
					if err != nil {
						t.Fatal(err)
					}
					if got.State != domain.DeliveryCutoverPending {
						t.Fatalf("mismatched identity space must stay pending: %+v", got)
					}
					item := deliveryCutoverItem(t, got, cutoverProbeB)
					if item.State != domain.DeliveryCutoverPending || item.Reason != domain.DeliveryCutoverApplicationMismatch {
						t.Fatalf("got %+v", item)
					}
				})
			}
		})
	}
}

func TestDeliveryCutoverInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.DeliveryCutoverInput)
	}{
		{"zero now", func(i *domain.DeliveryCutoverInput) { i.Now = time.Time{} }},
		{"zero snapshot", func(i *domain.DeliveryCutoverInput) { i.SnapshotAt = time.Time{} }},
		{"future snapshot", func(i *domain.DeliveryCutoverInput) { i.SnapshotAt = i.Now.Add(time.Nanosecond) }},
		{"stale current snapshot", func(i *domain.DeliveryCutoverInput) { i.SnapshotAt = i.Now.Add(-time.Nanosecond) }},
		{"empty hub", func(i *domain.DeliveryCutoverInput) { i.HubID = "" }},
		{"non-uuid hub", func(i *domain.DeliveryCutoverInput) { i.HubID = "hub" }},
		{"zero uuid hub", func(i *domain.DeliveryCutoverInput) { i.HubID = "00000000-0000-0000-0000-000000000000" }},
		{"monitor zero", func(i *domain.DeliveryCutoverInput) { i.Assignments.MonitorID = 0 }},
		{"monitor negative", func(i *domain.DeliveryCutoverInput) { i.Assignments.MonitorID = -1 }},
		{"member monitor mismatch", func(i *domain.DeliveryCutoverInput) { i.Assignments.Assignments[1].MonitorID = 43 }},
		{"revision zero", func(i *domain.DeliveryCutoverInput) { i.Assignments.Revision = 0 }},
		{"revision negative", func(i *domain.DeliveryCutoverInput) { i.Assignments.Revision = -1 }},
		{"mode invalid", func(i *domain.DeliveryCutoverInput) { i.Assignments.AlertDelivery = "other" }},
		{"policy empty", func(i *domain.DeliveryCutoverInput) { i.Assignments.HealthPolicy = "" }},
		{"policy invalid", func(i *domain.DeliveryCutoverInput) { i.Assignments.HealthPolicy = "other" }},
		{"member generation zero", func(i *domain.DeliveryCutoverInput) { i.Assignments.Assignments[1].Generation = 0 }},
		{"member probe empty", func(i *domain.DeliveryCutoverInput) { i.Assignments.Assignments[1].ProbeID = "" }},
		{"member probe non-uuid", func(i *domain.DeliveryCutoverInput) { i.Assignments.Assignments[1].ProbeID = "probe-b" }},
		{"member probe zero uuid", func(i *domain.DeliveryCutoverInput) {
			i.Assignments.Assignments[1].ProbeID = "00000000-0000-0000-0000-000000000000"
		}},
		{"duplicate member", func(i *domain.DeliveryCutoverInput) {
			i.Assignments.Assignments[1].ProbeID = i.Assignments.Assignments[0].ProbeID
		}},
		{"foreign source", func(i *domain.DeliveryCutoverInput) { i.Sources[1].ProbeID = cutoverProbeC }},
		{"empty source identity", func(i *domain.DeliveryCutoverInput) { i.Sources[0].ProbeID = "" }},
		{"duplicate source", func(i *domain.DeliveryCutoverInput) {
			i.Sources[1].ProbeID = i.Sources[0].ProbeID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := deliveryCutoverInput(domain.AlertDeliveryAggregate)
			tc.change(&i)
			got, err := EvaluateDeliveryCutover(i)
			if !errors.Is(err, ErrInvalidDeliveryCutover) {
				t.Fatalf("got %+v %v", got, err)
			}
			if !reflect.DeepEqual(got, domain.DeliveryCutoverDecision{State: domain.DeliveryCutoverPending}) {
				t.Fatalf("invalid input must return only the empty pending decision: %+v", got)
			}
		})
	}
}

func TestDeliveryCutoverOrderingDeterministicAndInputsUnchanged(t *testing.T) {
	input := deliveryCutoverInput(domain.AlertDeliveryAggregate)
	localMember, remoteMember := input.Assignments.Assignments[0], input.Assignments.Assignments[1]
	thirdMember := domain.ProbeAssignment{MonitorID: 42, ProbeID: cutoverProbeC, Generation: 9}
	// Deliberately shuffled members and sources; only output ordering may be sorted.
	input.Assignments.Assignments = []domain.ProbeAssignment{thirdMember, remoteMember, localMember}
	input.Sources = []domain.DeliveryCutoverSource{
		deliveryCutoverSource(input.Now, remoteMember, cutoverStreamB, cutoverHashB, 57, input.Assignments.Revision, domain.AlertDeliveryAggregate),
		deliveryCutoverSource(input.Now, thirdMember, cutoverStreamC, cutoverHashC, 63, input.Assignments.Revision, domain.AlertDeliveryAggregate),
		deliveryCutoverSource(input.Now, localMember, cutoverStreamLocal, cutoverHashLocal, 42, input.Assignments.Revision, domain.AlertDeliveryAggregate),
	}
	input.Assignments.Revision = math.MaxInt64
	for n := range input.Sources {
		input.Sources[n].AcknowledgedConfig.Revision = math.MaxInt64
		input.Sources[n].Assignment.AssignmentRevision = math.MaxInt64
		input.Sources[n].ModeApplication.AssignmentRevision = math.MaxInt64
		input.Sources[n].ModeApplication.ConfigRevision = math.MaxInt64
	}
	loc := time.FixedZone("UTC+7", 7*3600)
	input.Now, input.SnapshotAt = input.Now.In(loc), input.SnapshotAt.In(loc)

	configRefs := make([]*domain.ProbeActiveConfig, len(input.Sources))
	assignmentRefs := make([]*domain.AcknowledgedAssignmentDelivery, len(input.Sources))
	applicationRefs := make([]*domain.ProbeDeliveryModeApplication, len(input.Sources))
	for n, source := range input.Sources {
		configRefs[n], assignmentRefs[n], applicationRefs[n] = source.AcknowledgedConfig, source.Assignment, source.ModeApplication
	}
	before := deliveryCutoverCloneInput(input)

	var first domain.DeliveryCutoverDecision
	for run := 0; run < 25; run++ {
		got, err := EvaluateDeliveryCutover(input)
		if err != nil || got.State != domain.DeliveryCutoverEvidenceComplete {
			t.Fatalf("run %d: got %+v %v", run, got, err)
		}
		if run == 0 {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d unstable: %+v vs %+v", run, got, first)
		}
	}
	ids := make([]string, len(first.Sources))
	for n, item := range first.Sources {
		ids[n] = item.ProbeID
	}
	if want := []string{cutoverProbeB, cutoverProbeC, domain.LocalProbeID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("output order must be deterministic: got %v want %v", ids, want)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("mutated input")
	}
	for n := range input.Sources {
		if input.Sources[n].AcknowledgedConfig != configRefs[n] ||
			input.Sources[n].Assignment != assignmentRefs[n] ||
			input.Sources[n].ModeApplication != applicationRefs[n] {
			t.Fatal("replaced caller evidence pointers")
		}
	}
}

func TestDeliveryCutoverDecisionShapeDisclosesOnlyBoundedCodes(t *testing.T) {
	fieldNames := func(value any) []string {
		kind := reflect.TypeOf(value)
		names := make([]string, kind.NumField())
		for n := range names {
			names[n] = kind.Field(n).Name
		}
		return names
	}
	decisionFields := fieldNames(domain.DeliveryCutoverDecision{})
	if !reflect.DeepEqual(decisionFields, []string{"State", "DesiredMode", "Reason", "Sources"}) {
		t.Fatalf("decision shape changed: %v", decisionFields)
	}
	sourceFields := fieldNames(domain.DeliveryCutoverSourceDecision{})
	if !reflect.DeepEqual(sourceFields, []string{"ProbeID", "State", "Reason"}) {
		t.Fatalf("source decision shape changed: %v", sourceFields)
	}
	forbidden := regexp.MustCompile(`(?i)suppress|send|active|ready|eligible|permit|hash|sha|stream|applied|time`)
	for _, name := range append(append([]string{}, decisionFields...), sourceFields...) {
		if forbidden.MatchString(name) {
			t.Fatalf("decision must not disclose %q", name)
		}
	}
	input := deliveryCutoverInput(domain.AlertDeliveryBoth)
	got, err := EvaluateDeliveryCutover(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.DeliveryCutoverEvidenceComplete {
		t.Fatalf("got %+v", got)
	}
	rendered := fmt.Sprint(got)
	for _, fact := range []string{
		cutoverHashLocal, cutoverHashB,
		cutoverStreamLocal, cutoverStreamB,
		"2026-10-12", "12:00",
	} {
		if strings.Contains(rendered, fact) {
			t.Fatalf("decision leaks evidence facts %q: %s", fact, rendered)
		}
	}
}
