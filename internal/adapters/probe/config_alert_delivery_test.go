package probe

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Assignment alert_delivery is optional. Absence is the legacy regional mode;
// presence must be exactly one supported string. A struct tag alone cannot
// enforce that because null would silently decode to an empty string.
func TestConfigAssignmentAlertDeliveryDecodes(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-http.json")
	for _, test := range []struct {
		name string
		set  bool
		mode any
		want domain.AlertDelivery
	}{
		{name: "absent is regional", set: false, want: domain.AlertDeliveryRegional},
		{name: "regional", set: true, mode: "regional", want: domain.AlertDeliveryRegional},
		{name: "aggregate", set: true, mode: "aggregate", want: domain.AlertDeliveryAggregate},
		{name: "both", set: true, mode: "both", want: domain.AlertDeliveryBoth},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := mutateJSON(t, fixture, func(root map[string]any) {
				if test.set {
					root["assignments"].([]any)[0].(map[string]any)["alert_delivery"] = test.mode
				}
			})
			snapshot, err := DecodeConfigSnapshot(frame)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Assignments) != 1 || snapshot.Assignments[0].AlertDelivery != test.want {
				t.Fatalf("decoded alert delivery %q, want %q", snapshot.Assignments[0].AlertDelivery, test.want)
			}
		})
	}
}

func TestConfigAssignmentAlertDeliveryRejectsInvalid(t *testing.T) {
	fixture := readFixture(t, "valid", "config-snapshot-http.json")
	for _, test := range []struct {
		name string
		mode any
	}{
		{name: "unknown string", mode: "bogus"},
		{name: "empty string", mode: ""},
		{name: "uppercase", mode: "Regional"},
		{name: "null", mode: nil},
		{name: "number", mode: 42},
		{name: "bool", mode: true},
		{name: "object", mode: map[string]any{"mode": "both"}},
		{name: "array", mode: []any{"both"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := mutateJSON(t, fixture, func(root map[string]any) {
				root["assignments"].([]any)[0].(map[string]any)["alert_delivery"] = test.mode
			})
			snapshot, err := DecodeConfigSnapshot(frame)
			if err == nil {
				t.Fatalf("accepted invalid alert delivery %v (%q)", test.mode, snapshot.Assignments[0].AlertDelivery)
			}
		})
	}
}

// Regional and empty stay omitted so existing regional documents do not
// change. Only aggregate and both travel on the wire.
func TestLocalConfigEncoderAlertDeliveryOmission(t *testing.T) {
	for _, test := range []struct {
		name string
		mode domain.AlertDelivery
		want string
	}{
		{name: "empty omitted", mode: ""},
		{name: "regional omitted", mode: domain.AlertDeliveryRegional},
		{name: "aggregate emitted", mode: domain.AlertDeliveryAggregate, want: `"alert_delivery":"aggregate"`},
		{name: "both emitted", mode: domain.AlertDeliveryBoth, want: `"alert_delivery":"both"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := localDefinitionFixture()
			for i := range d.Assignments {
				d.Assignments[i].AlertDelivery = test.mode
			}
			doc, err := (LocalConfigEncoder{}).EncodeLocal(d)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if bytes.Contains(doc, []byte(`"alert_delivery"`)) {
					t.Fatal("legacy regional document changed")
				}
			} else if !bytes.Contains(doc, []byte(test.want)) {
				t.Fatalf("document is missing %s", test.want)
			}
			snapshot, err := DecodeLocalConfigSnapshot(doc)
			if err != nil {
				t.Fatal(err)
			}
			want := test.mode
			if want == "" {
				want = domain.AlertDeliveryRegional
			}
			for _, assignment := range snapshot.Assignments {
				if assignment.AlertDelivery != want {
					t.Fatalf("round trip decoded %q, want %q", assignment.AlertDelivery, want)
				}
			}
		})
	}
	t.Run("invalid source mode rejects", func(t *testing.T) {
		d := localDefinitionFixture()
		d.Assignments[0].AlertDelivery = domain.AlertDelivery("bogus")
		if _, err := (LocalConfigEncoder{}).EncodeLocal(d); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid source mode accepted: %v", err)
		}
	})
}

// Advertising paging.aggregate.v1 is informational until a later activation
// barrier. The probe binary must advertise it; enrollment frames stay snapshot.v1.
func TestAggregatePagingCapabilityAdvertised(t *testing.T) {
	if AggregatePagingCapability != "paging.aggregate.v1" {
		t.Fatalf("capability spelling changed: %q", AggregatePagingCapability)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "cmd", "probe", "runtime.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("probe.AggregatePagingCapability")) {
		t.Fatal("cmd/probe runtime does not advertise the aggregate paging capability")
	}
}
