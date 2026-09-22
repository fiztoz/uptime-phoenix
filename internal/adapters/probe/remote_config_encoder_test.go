package probe

import (
	"bytes"
	"slices"
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
		"local identity":           func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = domain.LocalProbeID },
		"invalid identity":         func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = "edge" },
		"push":                     func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "push" },
		"docker resource bindings": func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "docker" },
		"certificate paging":       func(d *domain.LocalProbeConfigDefinition) { d.Assignments[1].Monitor.CertExpiryNotify = true },
		"active escalation":        func(d *domain.LocalProbeConfigDefinition) { d.Policies[0].Enabled = true },
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

// remotePullCheckerConfigs pairs every remotely executable pull type with
// minimal settings its checker validates.
var remotePullCheckerConfigs = map[string]map[string]any{
	"http":      {"url": "https://example.test"},
	"tcp":       {"hostname": "example.test", "port": 443},
	"ping":      {"hostname": "example.test"},
	"dns":       {"hostname": "example.test", "resolve_type": "A"},
	"websocket": {"url": "wss://example.test/socket"},
	"mqtt":      {"broker": "mqtt://example.test:1883"},
	"rabbitmq":  {"url": "amqp://example.test:5672"},
	"grpc":      {"url": "example.test:50051"},
	"snmp":      {"hostname": "example.test", "oid": "1.3.6.1.2.1.1.3.0"},
	"database":  {"engine": "postgres", "connection_string": "postgres://user:pass@example.test:5432/db"},
	"s3":        {"bucket": "example-bucket", "access_key": "fixture-key", "secret_key": "fixture-secret"},
}

// TestRemoteConfigEncoderPublishesEveryPullCheckerType proves the hub can
// publish each pull type without a probe-local resource binding. Docker is
// covered by the unsupported-work test until assignments can carry binding
// keys.
func TestRemoteConfigEncoderPublishesEveryPullCheckerType(t *testing.T) {
	for kind, config := range remotePullCheckerConfigs {
		t.Run(kind, func(t *testing.T) {
			d := remoteDefinitionFixture()
			d.Assignments[0].Monitor.Type = kind
			d.Assignments[0].Monitor.Config = config
			document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
			if err != nil {
				t.Fatalf("pull checker publication failed: %v", err)
			}
			s, err := DecodeConfigSnapshot(document)
			if err != nil {
				t.Fatalf("published snapshot undecodable: %v", err)
			}
			// Assignments are sorted by monitor ID; find the mutated monitor.
			var found *ConfigAssignment
			for i := range s.Assignments {
				if s.Assignments[i].MonitorID == 2 {
					found = &s.Assignments[i]
					break
				}
			}
			if found == nil || found.Monitor.Type != kind || !slices.Contains(found.RequiredCapabilities, "checker."+kind+".v1") {
				t.Fatalf("assignment type or capability changed: %+v", found)
			}
		})
	}
}
