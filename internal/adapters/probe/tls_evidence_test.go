package probe

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestTLSObservationReplayAndSnapshotWire(t *testing.T) {
	at := time.Now().UTC()
	tls := &domain.TLSObservation{NotAfter: at.Add(7 * 24 * time.Hour), DaysRemaining: 7, Issuer: "CN=Public CA"}
	observation := domain.RegionalObservation{MonitorID: 1, ProbeID: uuid.NewString(), StreamID: uuid.NewString(), AssignmentGeneration: 1, ConfigRevision: 1, Seq: 1, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: at, TLS: tls}
	payload, err := (EdgeTelemetryEncoder{}).EncodeObservation(observation)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := buildReplayBatchFrame(observation.StreamID, 1, &domain.EdgeReplayBatch{StreamID: observation.StreamID, FirstSeq: 1, LastSeq: 1, Items: []domain.EdgeReplayItem{{Seq: 1, Kind: "observation", ObservedAt: at, Payload: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReplayBatch(frame, observation.ProbeID)
	if err != nil || len(decoded.Events) != 1 || decoded.Events[0].Observation == nil || !domain.SameTLSObservation(decoded.Events[0].Observation.TLS, tls) {
		t.Fatalf("TLS observation rejected or changed during replay: %+v %v", decoded, err)
	}
	_, document, err := encodeCurrentSnapshot(domain.EdgeCurrentSnapshot{Identity: domain.EdgeIdentity{StreamID: observation.StreamID, ConfigRevision: 1, LastCreatedSeq: 1}, CreatedAt: at, States: []domain.EdgeCurrentEvidence{{MonitorID: 1, AssignmentGeneration: 1, Seq: 1, Payload: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeStateSnapshot(document)
	if err != nil || len(snapshot.States) != 1 || !domain.SameTLSObservation(sourceTLS(snapshot.States[0].TLS), tls) {
		t.Fatal("current state discarded exact TLS", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data["tls"], &fields); err != nil || len(fields) != 3 || fields["not_after"] == nil || fields["days_remaining"] == nil || fields["issuer"] == nil {
		t.Fatal("TLS wire shape is not the explicit three-field whitelist", err)
	}
}
