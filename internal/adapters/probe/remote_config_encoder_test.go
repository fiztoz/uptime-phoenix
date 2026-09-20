package probe

import (
	"bytes"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func remoteDefinitionFixture() domain.LocalProbeConfigDefinition {
	d := localDefinitionFixture()
	d.Target.ProbeID = "22222222-2222-4222-8222-222222222222"
	d.Assignments[0].Monitor.Type = "tcp"
	d.Assignments[0].Monitor.Config = map[string]any{"host": "example.test", "port": 443}
	return d
}

func TestRemoteConfigEncoderPreservesScopeAndLocalPreferences(t *testing.T) {
	d := remoteDefinitionFixture()
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeConfigSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProbeID != d.Target.ProbeID || s.Revision != 7 || s.Assignments[1].Active || s.Assignments[1].Generation != 3 || s.Assignments[0].NotificationLinks[0].IncludeTarget || s.NotificationChannels[0].IncludeAckURL {
		t.Fatal("remote encoding changed identity, pause, generation or notification scope")
	}
	if s.NotificationChannels[0].Version != 7 || s.NotificationTemplates[0].Version != 7 || s.ProxyBindings[0].Version != 7 || s.EscalationPolicies[0].Version != 7 || s.EscalationPolicies[0].Enabled || s.Watchdog.Enabled || time.Time(s.CreatedAt).Location() != time.UTC {
		t.Fatal("dependency versions, disabled entries or UTC timestamps changed")
	}
	if !d.Notifications[1].IncludeAckURL {
		t.Fatal("remote encoder mutated hub-owned preference")
	}
	d.Assignments[0], d.Assignments[1] = d.Assignments[1], d.Assignments[0]
	d.Notifications[0], d.Notifications[1] = d.Notifications[1], d.Notifications[0]
	reordered, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil || !bytes.Equal(document, reordered) {
		t.Fatal("source ordering changed remote protected bytes")
	}
	d.Target.ProbeID = domain.LocalProbeID
	local, err := (LocalConfigEncoder{}).EncodeLocal(d)
	if err != nil {
		t.Fatal(err)
	}
	localSnapshot, err := DecodeLocalConfigSnapshot(local)
	if err != nil || !localSnapshot.NotificationChannels[0].IncludeAckURL {
		t.Fatal("local ACK-link preference changed")
	}
}

func TestRemoteConfigEncoderRejectsUnsupportedWork(t *testing.T) {
	for name, change := range map[string]func(*domain.LocalProbeConfigDefinition){
		"local identity":        func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = domain.LocalProbeID },
		"invalid identity":      func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = "edge" },
		"push":                  func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "push" },
		"unimplemented checker": func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "grpc" },
		"certificate paging":    func(d *domain.LocalProbeConfigDefinition) { d.Assignments[1].Monitor.CertExpiryNotify = true },
		"active escalation":     func(d *domain.LocalProbeConfigDefinition) { d.Policies[0].Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			d := remoteDefinitionFixture()
			change(&d)
			if document, err := (RemoteConfigEncoder{}).EncodeRemote(d); err == nil || document != nil {
				t.Fatal("unsupported work silently omitted or published")
			}
		})
	}
}
