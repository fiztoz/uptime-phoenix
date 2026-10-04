package probe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func codecResetReceipt() domain.EdgeStreamResetRecord {
	p := domain.ProbeStreamResetPlan{ResetID: "11111111-1111-4111-8111-111111111111", HubID: "22222222-2222-4222-8222-222222222222", ProbeID: "33333333-3333-4333-8333-333333333333", EnrollmentID: "44444444-4444-4444-8444-444444444444", PreviousStreamID: "55555555-5555-4555-8555-555555555555", StreamID: "66666666-6666-4666-8666-666666666666", Fingerprint: strings.Repeat("a", 64), CredentialVersion: 2, CertificateVersion: 3, HubCommittedSeq: 9007199254740993, ConnectionGeneration: 9, PreparedAt: time.Date(2026, 9, 21, 0, 0, 0, 123000, time.UTC)}
	return domain.EdgeStreamResetRecord{Plan: p, InitialStreamID: p.PreviousStreamID, Source: domain.EdgeIdentity{ProbeID: p.ProbeID, HubID: p.HubID, StreamID: p.PreviousStreamID, Fingerprint: strings.Repeat("b", 64), LastCreatedSeq: 3, CommittedSeq: 2, ConnectionGeneration: 8, ConfigRevision: 4}, State: "applied", ReservedAt: p.PreparedAt, AppliedAt: p.PreparedAt.Add(time.Second), ArchiveBytes: 4096, ArchiveSHA256: strings.Repeat("c", 64)}
}

func TestStreamResetCodecClosedContract(t *testing.T) {
	c := StreamResetCodec{}
	want := codecResetReceipt()
	plan, err := c.EncodeStreamResetPlan(t.Context(), want.Plan)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.DecodeStreamResetPlan(t.Context(), plan)
	if err != nil || got != want.Plan {
		t.Fatal("plan lost exact identity/counters", err)
	}
	data, err := c.EncodeStreamResetReceipt(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.DecodeStreamResetReceipt(t.Context(), data)
	if err != nil || !reflect.DeepEqual(receipt, want) {
		t.Fatal("source bounds changed to fabricate coverage", err)
	}
	if !bytes.Contains(data, []byte(`"hub_committed_seq":"9007199254740993"`)) || !bytes.Contains(data, []byte(`"unobserved_coverage":"unknown"`)) {
		t.Fatal("receipt lost precision or unknown coverage")
	}
	for name, mutate := range map[string]func(map[string]json.RawMessage){
		"unknown":               func(m map[string]json.RawMessage) { m["surprise"] = json.RawMessage(`true`) },
		"missing":               func(m map[string]json.RawMessage) { delete(m, "state") },
		"null":                  func(m map[string]json.RawMessage) { m["state"] = json.RawMessage(`null`) },
		"wrong phase":           func(m map[string]json.RawMessage) { m["state"] = json.RawMessage(`"complete"`) },
		"invented coverage":     func(m map[string]json.RawMessage) { m["unobserved_coverage"] = json.RawMessage(`"complete"`) },
		"JSON number":           func(m map[string]json.RawMessage) { m["archive_bytes"] = json.RawMessage(`4096`) },
		"leading zero":          func(m map[string]json.RawMessage) { m["archive_bytes"] = json.RawMessage(`"04096"`) },
		"overflow":              func(m map[string]json.RawMessage) { m["archive_bytes"] = json.RawMessage(`"9223372036854775808"`) },
		"generation not fenced": func(m map[string]json.RawMessage) { m["source_connection_generation"] = json.RawMessage(`"9"`) },
		"cursor past source":    func(m map[string]json.RawMessage) { m["source_committed_seq"] = json.RawMessage(`"4"`) },
		"offset clock": func(m map[string]json.RawMessage) {
			m["source_applied_at"] = json.RawMessage(`"2026-09-21T07:00:00+07:00"`)
		},
		"submicro clock": func(m map[string]json.RawMessage) {
			m["source_applied_at"] = json.RawMessage(`"2026-09-21T00:00:00.000000001Z"`)
		},
		"unknown nested":   func(m map[string]json.RawMessage) { m["plan"] = append([]byte(`{"surprise":true,`), plan[1:]...) },
		"duplicate nested": func(m map[string]json.RawMessage) { m["plan"] = append([]byte(`{"schema_version":1,`), plan[1:]...) },
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			mutate(m)
			bad, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.DecodeStreamResetReceipt(t.Context(), bad); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
	for _, bad := range [][]byte{append([]byte(`{"state":"source_applied",`), data[1:]...), append(bytes.Clone(data), []byte(` {}`)...), bytes.Repeat([]byte(" "), 8193)} {
		if _, err := c.DecodeStreamResetReceipt(t.Context(), bad); err == nil {
			t.Fatal("ambiguous/oversize receipt accepted")
		}
	}
}
