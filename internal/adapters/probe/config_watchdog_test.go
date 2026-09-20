package probe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
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

func TestEnabledWatchdogRequiresMetadataAndCapability(t *testing.T) {
	s := m2Config(t)
	s.Watchdog.Enabled = true
	target := domain.ProbeConfigTarget{HubID: s.HubID, ProbeID: s.ProbeID}
	decoder := NewEdgeConfigDecoder(checker.Get, notifier.Get)
	if _, err := decoder.DecodeEdge(t.Context(), configBytes(t, s), target); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("watchdog accepted without probe metadata", err)
	}
	s.Probe = &ConfigProbeDisplay{Name: "Bangkok edge", Location: "TH"}
	document := configBytes(t, s)
	resolved, err := decoder.DecodeEdge(t.Context(), document, target)
	if err != nil || !resolved.Watchdog.Enabled || resolved.Probe.Name != s.Probe.Name {
		t.Fatal("enabled watchdog graph not resolved", err)
	}
	begin, chunks, commit := configTestFrames(t, document)
	transferTarget := configTestTarget()
	transferTarget.Capabilities = []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"}
	if _, err := NewConfigTransfer(begin, time.Now(), transferTarget); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatal("old edge accepted watchdog transfer", err)
	}
	transferTarget.Capabilities = append(transferTarget.Capabilities, "watchdog.v1")
	// Use the complete inventory for unrelated disabled dependencies too.
	transferTarget.Capabilities = append(transferTarget.Capabilities, "notifier.discord.v1", "notifier.smtp.v1")
	transfer := filledConfigTransfer(t, begin, chunks, time.Now(), transferTarget)
	if _, err := transfer.Commit(commit, time.Now()); err != nil {
		t.Fatal("capable edge rejected complete watchdog graph", err)
	}
}
