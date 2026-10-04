package probe

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func wireTestCondition(state domain.ConditionState, percent float64, at time.Time) domain.ConditionObservation {
	threshold := 80.0
	return domain.ConditionObservation{
		Kind: domain.MonitorConditionStorage, State: state, Used: &percent, Limit: floatPtr2(100), Percent: &percent, Threshold: &threshold,
		Unit: "bytes", Resource: "Database size", Scope: "database", Source: "fixed engine query",
		Message: "sampled", ObservedAt: at, StaleAfter: at.Add(3 * time.Minute),
	}
}

func floatPtr2(value float64) *float64 { return &value }

func TestConditionWireRoundTrip(t *testing.T) {
	at := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	raw := wireTestCondition(domain.ConditionStateWarning, 84, at)

	t.Run("ObservationCarriesRawEvidence", func(t *testing.T) {
		o := domain.RegionalObservation{MonitorID: 7, AssignmentGeneration: 2, ConfigRevision: 3, Seq: 4, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: at, Conditions: []domain.ConditionObservation{raw}}
		payload, err := (EdgeTelemetryEncoder{}).EncodeObservation(o)
		if err != nil {
			t.Fatal(err)
		}
		event, err := decodeTelemetryEvent(payload)
		if err != nil {
			t.Fatal(err)
		}
		observation, ok := event.Data.(Observation)
		if !ok || len(observation.Conditions) != 1 {
			t.Fatalf("raw evidence lost: %+v", event.Data)
		}
		if got := sourceConditionObservation(observation.Conditions[0]); got.State != domain.ConditionStateWarning || got.Percent == nil || *got.Percent != 84 || !got.ObservedAt.Equal(at) {
			t.Fatalf("raw evidence changed: %+v", got)
		}
	})

	t.Run("EmptyConditionsSerializeExplicitly", func(t *testing.T) {
		o := domain.RegionalObservation{MonitorID: 7, AssignmentGeneration: 2, ConfigRevision: 3, Seq: 4, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: at}
		payload, err := (EdgeTelemetryEncoder{}).EncodeObservation(o)
		if err != nil {
			t.Fatal(err)
		}
		event, err := decodeTelemetryEvent(payload)
		if err != nil {
			t.Fatal(err)
		}
		if observation, ok := event.Data.(Observation); !ok || len(observation.Conditions) != 0 {
			t.Fatal("empty condition list must decode as an explicit empty list")
		}
	})

	t.Run("ConditionTransitionRoundTrip", func(t *testing.T) {
		previous := domain.ConditionStateWarning
		transition := domain.ConditionTransition{MonitorID: 7, AssignmentGeneration: 2, ConfigRevision: 3, Kind: domain.MonitorConditionStorage, PreviousState: &previous, State: domain.ConditionStateOK, Message: "recovered to 10.0%"}
		payload, err := (EdgeTelemetryEncoder{}).EncodeConditionTransition(5, at, transition)
		if err != nil {
			t.Fatal(err)
		}
		event, err := decodeTelemetryEvent(payload)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind != "condition.transition" {
			t.Fatalf("unexpected kind %q", event.Kind)
		}
		got := sourceConditionTransition(event.Data.(ConditionTransition))
		if !domain.ValidConditionTransition(&got) || got.State != domain.ConditionStateOK || got.PreviousState == nil || *got.PreviousState != domain.ConditionStateWarning {
			t.Fatalf("transition changed: %+v", got)
		}
		same := domain.ConditionStateOK
		if _, err := (EdgeTelemetryEncoder{}).EncodeConditionTransition(5, at, domain.ConditionTransition{MonitorID: 7, AssignmentGeneration: 2, ConfigRevision: 3, Kind: domain.MonitorConditionStorage, PreviousState: &same, State: domain.ConditionStateOK}); err == nil {
			t.Fatal("transition whose previous state equals its state must be rejected")
		}
		if _, err := (EdgeTelemetryEncoder{}).EncodeConditionTransition(5, at, domain.ConditionTransition{}); err == nil {
			t.Fatal("identity-less transition must be rejected")
		}
	})

	t.Run("SnapshotStateMatchesPayloadOrFails", func(t *testing.T) {
		o := domain.RegionalObservation{MonitorID: 7, AssignmentGeneration: 2, ConfigRevision: 3, Seq: 4, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: at, Conditions: []domain.ConditionObservation{raw}}
		payload, err := (EdgeTelemetryEncoder{}).EncodeObservation(o)
		if err != nil {
			t.Fatal(err)
		}
		effective := domain.ConditionStateWarning
		lastSuccess := at
		evidence := domain.ConditionEvidence{ConditionObservation: raw, EffectiveState: &effective, ConsecutiveState: domain.ConditionStateWarning, ConsecutiveCount: 2, LastSuccessAt: &lastSuccess}
		source := domain.EdgeCurrentSnapshot{
			Identity:  domain.EdgeIdentity{ProbeID: "11111111-1111-4111-8111-111111111111", StreamID: "22222222-2222-4222-8222-222222222222", ConfigRevision: 3, LastCreatedSeq: 4},
			CreatedAt: at,
			States: []domain.EdgeCurrentEvidence{{
				MonitorID: 7, AssignmentGeneration: 2, Seq: 4, Payload: payload,
				Conditions: []domain.ConditionEvidence{evidence},
			}},
		}
		snapshot, _, err := encodeCurrentSnapshot(source)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.States) != 1 || len(snapshot.States[0].Conditions) != 1 {
			t.Fatalf("evaluated condition state lost: %+v", snapshot.States)
		}
		if snapshot.States[0].Conditions[0].EffectiveState == nil || *snapshot.States[0].Conditions[0].EffectiveState != "warning" {
			t.Fatalf("promoted state lost: %+v", snapshot.States[0].Conditions[0])
		}
		source.States[0].Conditions = nil
		if _, _, err := encodeCurrentSnapshot(source); err == nil {
			t.Fatal("snapshot evidence must not disagree with the immutable payload")
		}
		forged := evidence
		forged.EffectiveState = nil
		source.States[0].Conditions = []domain.ConditionEvidence{forged}
		if _, _, err := encodeCurrentSnapshot(source); err == nil {
			t.Fatal("unconfirmed state must not masquerade as a different evaluation")
		}
	})
}
