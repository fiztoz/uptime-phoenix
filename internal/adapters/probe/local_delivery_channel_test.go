package probe

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestLocalDeliveryChannelComparison(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.LocalProbeConfigDefinition)
		match  bool
	}{
		{"unrelated monitor", func(d *domain.LocalProbeConfigDefinition) { d.Assignments[1].Monitor.Description = "changed" }, true},
		{"other channel", func(d *domain.LocalProbeConfigDefinition) { d.Notifications[0].Name = "changed" }, true},
		{"provider settings", func(d *domain.LocalProbeConfigDefinition) {
			d.Notifications[1].Config["url"] = "https://changed.example.test"
		}, false},
		{"channel disabled", func(d *domain.LocalProbeConfigDefinition) { d.Notifications[1].Active = false }, false},
		{"channel removed", func(d *domain.LocalProbeConfigDefinition) {
			d.Assignments[1].NotificationLinks = nil
			d.Notifications = d.Notifications[:1]
			d.Templates = nil
		}, false},
		{"template body", func(d *domain.LocalProbeConfigDefinition) { d.Templates[0].BodyTemplate = "changed {{monitor.name}}" }, false},
		{"template settings", func(d *domain.LocalProbeConfigDefinition) { d.Templates[0].Config = map[string]any{"custom": true} }, false},
		{"ack URL", func(d *domain.LocalProbeConfigDefinition) { d.Notifications[1].IncludeAckURL = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoder := LocalConfigEncoder{}
			before, err := encoder.EncodeLocal(localDefinitionFixture())
			if err != nil {
				t.Fatal(err)
			}
			next := localDefinitionFixture()
			next.Revision++
			tc.change(&next)
			match, err := encoder.SameLocalDeliveryChannel(before, next, 101)
			if err != nil || match != tc.match {
				t.Fatalf("match=%v err=%v; want %v", match, err, tc.match)
			}
		})
	}
}
