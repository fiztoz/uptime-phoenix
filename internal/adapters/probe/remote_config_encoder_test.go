package probe

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
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

func TestRemoteConfigEncoderPublishesEnabledEscalation(t *testing.T) {
	d := remoteDefinitionFixture()
	d.Policies[0].Enabled = true
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeConfigSnapshot(document)
	if err != nil || len(snapshot.EscalationPolicies) == 0 || !snapshot.EscalationPolicies[0].Enabled || len(snapshot.EscalationPolicies[0].Steps) == 0 {
		t.Fatalf("enabled ladder dropped: %+v %v", snapshot.EscalationPolicies, err)
	}
	if err := validateEdgeRuntimeSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteConfigEncoderRejectsUnsupportedWork(t *testing.T) {
	for name, change := range map[string]func(*domain.LocalProbeConfigDefinition){
		"local identity":           func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = domain.LocalProbeID },
		"invalid identity":         func(d *domain.LocalProbeConfigDefinition) { d.Target.ProbeID = "edge" },
		"push":                     func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "push" },
		"docker resource bindings": func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor.Type = "docker" },
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

// TestRemoteConfigEncoderPublishesCertificatePaging proves the hub keeps the
// monitor's opt-in flag in the remote dialect, so the source that owns the
// assignment is the one that decides when a threshold has been reached.
func TestRemoteConfigEncoderPublishesCertificatePaging(t *testing.T) {
	d := remoteDefinitionFixture()
	d.Assignments[1].Monitor.CertExpiryNotify = true
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil || document == nil {
		t.Fatalf("certificate paging rejected: %v", err)
	}
	snapshot, err := DecodeConfigSnapshot(document)
	if err != nil {
		t.Fatalf("decode published certificate paging graph: %v", err)
	}
	for _, a := range snapshot.Assignments {
		if a.MonitorID == d.Assignments[1].Monitor.ID && !a.Monitor.CertExpiryNotify {
			t.Fatal("certificate paging opt-in lost through the remote dialect")
		}
	}
	if err := validateEdgeRuntimeSnapshot(snapshot); err != nil {
		t.Fatalf("published certificate paging graph rejected by the edge: %v", err)
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

func TestRemoteConfigEncoderAdvertisesExecutableProxy(t *testing.T) {
	d := remoteDefinitionFixture()
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeConfigSnapshot(document)
	if err != nil {
		t.Fatal(err)
	}
	var httpAssignment *ConfigAssignment
	for i := range snapshot.Assignments {
		if snapshot.Assignments[i].Monitor.Type == "http" {
			httpAssignment = &snapshot.Assignments[i]
		}
		if snapshot.Assignments[i].Monitor.Type == "tcp" && snapshot.Assignments[i].ProxyBindingKey != nil {
			t.Fatal("tcp assignment gained a proxy")
		}
	}
	if httpAssignment == nil || httpAssignment.ProxyBindingKey == nil || !slices.Contains(httpAssignment.RequiredCapabilities, "proxy.http.v1") {
		t.Fatalf("http proxy was not required: %+v", httpAssignment)
	}
	for i := range snapshot.Assignments {
		if snapshot.Assignments[i].Monitor.Type == "http" {
			snapshot.Assignments[i].RequiredCapabilities = []string{"checker.http.v1"}
		}
	}
	if err := validateEdgeRuntimeSnapshot(snapshot); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatal("proxy without its protocol capability activated", err)
	}

	d.Assignments[0].Monitor.ProxyID = d.Assignments[1].Monitor.ProxyID
	if _, err := (RemoteConfigEncoder{}).EncodeRemote(d); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatal("tcp proxy assignment was published", err)
	}
}

func TestRemotePublicationRejectsUnsupportedDatabaseEngine(t *testing.T) {
	d := remoteDefinitionFixture()
	d.Assignments[0].Monitor.Type = "database"
	d.Assignments[0].Monitor.Config = map[string]any{"engine": "oracle", "connection_string": "oracle://example.test/db"}
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHubConfigDecoder(checker.Get, notifier.Get).DecodeEdge(t.Context(), document, d.Target); err == nil {
		t.Fatal("unsupported database engine reached activation")
	}
}

// TestRemoteConfigEncoderPublishesEveryPullCheckerType proves the hub can
// publish each pull type without a probe-local resource binding. Docker is
// covered separately by the resource binding publication tests.
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
