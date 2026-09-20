package probe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigProbeMetadataUsesExactBoundedFields(t *testing.T) {
	base := readFixture(t, "valid", "config-snapshot-empty.json")
	for name, raw := range map[string]string{
		"valid": `{"name":"Bangkok", "location":"TH", "NAME":"alias", "endpoint":"secret"}`,
		"null":  `null`, "missing": `{"name":"Bangkok"}`, "case": `{"NAME":"Bangkok", "location":"TH"}`,
		"empty": `{"name":" ", "location":"TH"}`,
		"long":  `{"name":"` + strings.Repeat("ก", 201) + `", "location":"TH"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(base, &fields); err != nil {
				t.Fatal(err)
			}
			fields["probe"] = json.RawMessage(raw)
			document, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			s, err := DecodeConfigSnapshot(document)
			if name != "valid" {
				if err == nil {
					t.Fatal("invalid metadata accepted")
				}
				return
			}
			if err != nil || s.Probe == nil || s.Probe.Name != "Bangkok" || s.Probe.Location != "TH" {
				t.Fatal("metadata alias replaced canonical fields", err)
			}
			encoded, err := json.Marshal(s.Probe)
			if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "alias") {
				t.Fatal("metadata escaped whitelist", err)
			}
		})
	}
}
