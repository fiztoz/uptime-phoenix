package probe

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestLocalDeliveryConfigDecoderDialectAndModes(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{"", domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode), func(t *testing.T) {
			d := localDefinitionFixture()
			d.Revision = 9007199254740993
			// Keep local push and opaque ACK preferences. Remove the deliberately
			// incomplete SMTP config used by the encoder-only fixture.
			d.Notifications = d.Notifications[1:]
			d.Policies[0].Steps = nil
			d.Assignments = append(d.Assignments, domain.ProbeConfigAssignment{Monitor: &domain.Monitor{ID: 3, Name: "local docker", Type: "docker", Interval: 60, Timeout: 3, Config: map[string]any{"container": "fixture", "docker_daemon": "unix:///nonexistent-fixture.sock"}}, Generation: 2})
			for i := range d.Assignments {
				d.Assignments[i].AlertDelivery = mode
			}
			document, err := (LocalConfigEncoder{}).EncodeLocal(d)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := (ConfigInspector{}).Inspect(document, d.Target)
			if err != nil {
				t.Fatal(err)
			}
			decoder := NewLocalDeliveryConfigDecoder(checker.Get, notifier.Get)
			result, err := decoder.DecodeLocalDelivery(t.Context(), document, metadata)
			want, _ := domain.CanonicalAlertDelivery(mode)
			if err != nil || result.Target != d.Target || result.Revision != d.Revision || len(result.Assignments) != 3 {
				t.Fatal("local dialect was rejected or identity lost", err)
			}
			for _, assignment := range result.Assignments {
				if assignment.AlertDelivery != want || assignment.Generation <= 0 {
					t.Fatal("exact local document mode lost")
				}
			}
			data, err := json.Marshal(result) // Test-only containment check, not an HTTP View.
			if err != nil || strings.Contains(string(data), "fixture-provider-secret") || strings.Contains(string(data), "fixture-check-secret") || strings.Contains(string(data), "example.test") || strings.Contains(string(data), "SHA256") {
				t.Fatal("projection retained confidential settings")
			}
			wrong := metadata
			wrong.Revision++
			if result, err := decoder.DecodeLocalDelivery(t.Context(), document, wrong); result != nil || !errors.Is(err, ports.ErrConflict) {
				t.Fatal("metadata disagreement accepted")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if result, err := decoder.DecodeLocalDelivery(ctx, document, metadata); result != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation ignored")
			}
		})
	}
}
