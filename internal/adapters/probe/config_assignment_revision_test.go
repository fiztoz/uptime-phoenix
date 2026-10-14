package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The optional assignment_revision binds a document to its immutable
// assignment-set revision. Absence is the legacy unbound document (zero, never
// "current" or revision one); presence must be exactly one canonical positive
// decimal string whatever the desired mode. A struct tag alone cannot enforce
// that because null would silently decode to zero.
func TestConfigAssignmentRevisionDecodesCanonicalPositive(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-http.json")
	for _, delivery := range []struct {
		name string
		set  bool
		mode any
	}{
		{name: "legacy mode", set: false},
		{name: "regional", set: true, mode: "regional"},
		{name: "aggregate", set: true, mode: "aggregate"},
		{name: "both", set: true, mode: "both"},
	} {
		for _, test := range []struct {
			name string
			set  bool
			rev  any
			want int64
		}{
			{name: "absent is unbound", set: false, want: 0},
			{name: "one", set: true, rev: "1", want: 1},
			{name: "above float precision", set: true, rev: "9007199254740993", want: 9007199254740993},
			{name: "max int64", set: true, rev: "9223372036854775807", want: math.MaxInt64},
		} {
			t.Run(delivery.name+"/"+test.name, func(t *testing.T) {
				frame := mutateJSON(t, fixture, func(root map[string]any) {
					assignment := root["assignments"].([]any)[0].(map[string]any)
					if delivery.set {
						assignment["alert_delivery"] = delivery.mode
					}
					if test.set {
						assignment["assignment_revision"] = test.rev
					}
				})
				snapshot, err := DecodeConfigSnapshot(frame)
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Assignments) != 1 {
					t.Fatalf("decoded %d assignments, want 1", len(snapshot.Assignments))
				}
				if got := int64(snapshot.Assignments[0].AssignmentRevision); got != test.want {
					t.Fatalf("decoded assignment revision %d, want %d", got, test.want)
				}
			})
		}
	}
	t.Run("unrelated optional fields stay ignored", func(t *testing.T) {
		frame := mutateJSON(t, fixture, func(root map[string]any) {
			assignment := root["assignments"].([]any)[0].(map[string]any)
			assignment["assignment_revision"] = "5"
			assignment["future_optional"] = map[string]any{"extension": []any{1, false, nil}}
		})
		snapshot, err := DecodeConfigSnapshot(frame)
		if err != nil || len(snapshot.Assignments) != 1 || int64(snapshot.Assignments[0].AssignmentRevision) != 5 {
			t.Fatalf("unknown optional field interfered with the binding: %v %v", snapshot.Assignments, err)
		}
	})
}

func TestConfigAssignmentRevisionRejectsInvalid(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-http.json")
	for _, test := range []struct {
		name string
		rev  any
	}{
		{name: "null", rev: nil},
		{name: "number", rev: json.Number("1")},
		{name: "negative number", rev: json.Number("-1")},
		{name: "zero", rev: "0"},
		{name: "double zero", rev: "00"},
		{name: "leading zero", rev: "01"},
		{name: "plus sign", rev: "+1"},
		{name: "minus sign", rev: "-1"},
		{name: "overflow", rev: "9223372036854775808"},
		{name: "huge", rev: "184467440737095516160"},
		{name: "empty string", rev: ""},
		{name: "leading space", rev: " 1"},
		{name: "trailing space", rev: "1 "},
		{name: "fraction", rev: "1.0"},
		{name: "exponent", rev: "1e3"},
		{name: "hex", rev: "0x1"},
		{name: "non-ascii digits", rev: "١٢"},
		{name: "bool", rev: true},
		{name: "object", rev: map[string]any{"revision": "1"}},
		{name: "array", rev: []any{"1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := mutateJSON(t, fixture, func(root map[string]any) {
				root["assignments"].([]any)[0].(map[string]any)["assignment_revision"] = test.rev
			})
			if snapshot, err := DecodeConfigSnapshot(frame); err == nil {
				t.Fatalf("accepted invalid assignment revision %v (%d assignments)", test.rev, len(snapshot.Assignments))
			}
		})
	}
}

// encodeTestLocal and encodeTestRemote keep the dialect tables below on single
// lines and route through the production encoders unchanged.
func encodeTestLocal(d domain.LocalProbeConfigDefinition) ([]byte, error) {
	return (LocalConfigEncoder{}).EncodeLocal(d)
}

func encodeTestRemote(d domain.LocalProbeConfigDefinition) ([]byte, error) {
	return (RemoteConfigEncoder{}).EncodeRemote(d)
}

// Zero omits the binding so legacy definitions keep their exact bytes; a
// positive revision always travels and round trips exactly for every desired
// mode, in both the local and the remote dialect.
func TestConfigEncoderAssignmentRevisionBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		def    func() domain.LocalProbeConfigDefinition
		encode func(domain.LocalProbeConfigDefinition) ([]byte, error)
		decode func([]byte) (ConfigSnapshot, error)
	}{
		{name: "local", def: localDefinitionFixture, encode: encodeTestLocal, decode: DecodeLocalConfigSnapshot},
		{name: "remote", def: remoteDefinitionFixture, encode: encodeTestRemote, decode: DecodeConfigSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, delivery := range []struct {
				name string
				mode domain.AlertDelivery
			}{
				{name: "legacy mode", mode: ""},
				{name: "regional", mode: domain.AlertDeliveryRegional},
				{name: "aggregate", mode: domain.AlertDeliveryAggregate},
				{name: "both", mode: domain.AlertDeliveryBoth},
			} {
				for _, rev := range []struct {
					name  string
					set   bool
					value int64
					want  string
				}{
					{name: "unbound omits", set: false},
					{name: "positive emits", set: true, value: 5, want: `"assignment_revision":"5"`},
					{name: "above float precision emits", set: true, value: 9007199254740993, want: `"assignment_revision":"9007199254740993"`},
					{name: "max int64 emits", set: true, value: math.MaxInt64, want: `"assignment_revision":"9223372036854775807"`},
				} {
					t.Run(delivery.name+"/"+rev.name, func(t *testing.T) {
						d := test.def()
						for i := range d.Assignments {
							d.Assignments[i].AlertDelivery = delivery.mode
							if rev.set {
								d.Assignments[i].AssignmentRevision = rev.value
							}
						}
						doc, err := test.encode(d)
						if err != nil {
							t.Fatal(err)
						}
						if rev.want == "" {
							if bytes.Contains(doc, []byte(`"assignment_revision"`)) {
								t.Fatal("legacy unbound document changed")
							}
						} else if !bytes.Contains(doc, []byte(rev.want)) {
							t.Fatalf("document is missing %s", rev.want)
						}
						snapshot, err := test.decode(doc)
						if err != nil {
							t.Fatal(err)
						}
						want := int64(0)
						if rev.set {
							want = rev.value
						}
						for _, assignment := range snapshot.Assignments {
							if int64(assignment.AssignmentRevision) != want {
								t.Fatalf("round trip decoded %v, want %d", assignment.AssignmentRevision, want)
							}
						}
					})
				}
			}
		})
	}
}

func TestConfigEncoderRejectsNegativeAssignmentRevision(t *testing.T) {
	for _, test := range []struct {
		name   string
		def    func() domain.LocalProbeConfigDefinition
		encode func(domain.LocalProbeConfigDefinition) ([]byte, error)
	}{
		{name: "local", def: localDefinitionFixture, encode: encodeTestLocal},
		{name: "remote", def: remoteDefinitionFixture, encode: encodeTestRemote},
	} {
		for _, value := range []int64{-1, -math.MaxInt64} {
			t.Run(test.name+"/"+strconv.FormatInt(value, 10), func(t *testing.T) {
				d := test.def()
				for i := range d.Assignments {
					d.Assignments[i].AssignmentRevision = value
				}
				if doc, err := test.encode(d); err == nil || !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("negative assignment revision %d accepted: %q %v", value, doc, err)
				}
			})
		}
	}
}

// Binding changes participate in normal snapshot change detection even when
// desired mode, generation and settings return to their previous values.
func TestConfigEncoderAssignmentRevisionChangesOnlyBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		def    func() domain.LocalProbeConfigDefinition
		encode func(domain.LocalProbeConfigDefinition) ([]byte, error)
		decode func([]byte) (ConfigSnapshot, error)
	}{
		{name: "local", def: localDefinitionFixture, encode: encodeTestLocal, decode: DecodeLocalConfigSnapshot},
		{name: "remote", def: remoteDefinitionFixture, encode: encodeTestRemote, decode: DecodeConfigSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			first, second := test.def(), test.def()
			first.Assignments[0].AssignmentRevision = 17
			second.Assignments[0].AssignmentRevision = 23
			firstDoc, err := test.encode(first)
			if err != nil {
				t.Fatal(err)
			}
			secondDoc, err := test.encode(second)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(firstDoc, secondDoc) {
				t.Fatal("binding change left the protected bytes unchanged")
			}
			normalized := bytes.ReplaceAll(secondDoc, []byte(`"assignment_revision":"23"`), []byte(`"assignment_revision":"17"`))
			if !bytes.Equal(firstDoc, normalized) {
				t.Fatal("binding change altered more than the assignment revision")
			}
			a, err := test.decode(firstDoc)
			if err != nil {
				t.Fatal(err)
			}
			b, err := test.decode(secondDoc)
			if err != nil {
				t.Fatal(err)
			}
			if a.Revision != b.Revision {
				t.Fatal("config revision moved with the binding")
			}
			for i := range a.Assignments {
				if a.Assignments[i].Generation != b.Assignments[i].Generation {
					t.Fatal("assignment generation moved with the binding")
				}
			}
		})
	}
}

// The live edge decoder and the hub's reference-only decoder share one
// projection path. The exact document binding survives into runtime data and
// zero stays legacy/unbound, whatever the desired mode.
func TestEdgeConfigDecoderRetainsAssignmentRevision(t *testing.T) {
	for _, delivery := range []struct {
		name string
		mode domain.AlertDelivery
	}{
		{name: "legacy mode", mode: ""},
		{name: "regional", mode: domain.AlertDeliveryRegional},
		{name: "aggregate", mode: domain.AlertDeliveryAggregate},
		{name: "both", mode: domain.AlertDeliveryBoth},
	} {
		for _, rev := range []struct {
			name  string
			value int64
		}{
			{name: "unbound", value: 0},
			{name: "above float precision", value: 9007199254740993},
			{name: "max int64", value: math.MaxInt64},
		} {
			t.Run(delivery.name+"/"+rev.name, func(t *testing.T) {
				s := m2Config(t)
				s.Assignments[0].AlertDelivery = delivery.mode
				s.Assignments[0].AssignmentRevision = Decimal(rev.value)
				document := configBytes(t, s)
				target := domain.ProbeConfigTarget{HubID: s.HubID, ProbeID: s.ProbeID}
				check := func(name string, decoder *EdgeConfigDecoder) {
					resolved, err := decoder.DecodeEdge(t.Context(), document, target)
					if err != nil || len(resolved.Assignments) != 1 || resolved.Assignments[0].AssignmentRevision != rev.value {
						t.Fatalf("%s decoder lost the assignment revision %d: %+v %v", name, rev.value, resolved, err)
					}
				}
				check("live", NewEdgeConfigDecoder(checker.Get, notifier.Get))
				check("hub", NewHubConfigDecoder(checker.Get, notifier.Get))
			})
		}
	}
}

// The nonsecret acknowledged projection keeps the exact document binding for
// every desired mode; legacy documents stay unbound instead of claiming a
// current or one revision.
func TestLocalDeliveryConfigDecoderRetainsAssignmentRevision(t *testing.T) {
	for _, delivery := range []struct {
		name string
		mode domain.AlertDelivery
	}{
		{name: "legacy mode", mode: ""},
		{name: "regional", mode: domain.AlertDeliveryRegional},
		{name: "aggregate", mode: domain.AlertDeliveryAggregate},
		{name: "both", mode: domain.AlertDeliveryBoth},
	} {
		for _, rev := range []struct {
			name  string
			bound bool
			want  map[int64]int64
		}{
			{name: "unbound", bound: false, want: map[int64]int64{1: 0, 2: 0}},
			{name: "bound", bound: true, want: map[int64]int64{1: 9007199254740993, 2: math.MaxInt64}},
		} {
			t.Run(delivery.name+"/"+rev.name, func(t *testing.T) {
				d := localDefinitionFixture()
				// Keep local push and opaque ACK preferences. Remove the deliberately
				// incomplete SMTP config used by the encoder-only fixture.
				d.Notifications = d.Notifications[1:]
				d.Policies[0].Steps = nil
				for i := range d.Assignments {
					d.Assignments[i].AlertDelivery = delivery.mode
					if rev.bound {
						d.Assignments[i].AssignmentRevision = rev.want[d.Assignments[i].Monitor.ID]
					}
				}
				document, err := (LocalConfigEncoder{}).EncodeLocal(d)
				if err != nil {
					t.Fatal(err)
				}
				metadata, err := (ConfigInspector{}).Inspect(document, d.Target)
				if err != nil {
					t.Fatal(err)
				}
				result, err := NewLocalDeliveryConfigDecoder(checker.Get, notifier.Get).DecodeLocalDelivery(t.Context(), document, metadata)
				if err != nil || len(result.Assignments) != 2 {
					t.Fatal("local document was rejected or lost assignments", err)
				}
				for _, assignment := range result.Assignments {
					if assignment.AssignmentRevision != rev.want[assignment.MonitorID] {
						t.Fatalf("projection lost the binding for %d: %d, want %d", assignment.MonitorID, assignment.AssignmentRevision, rev.want[assignment.MonitorID])
					}
				}
			})
		}
	}
}
