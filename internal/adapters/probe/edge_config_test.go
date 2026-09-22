package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func m2Config(t *testing.T) ConfigSnapshot {
	t.Helper()
	s, err := DecodeConfigSnapshot(readFixture(t, "valid", "config-snapshot-http.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Watchdog.Enabled = false
	s.Assignments[0].Monitor.CertExpiryNotify = false
	return s
}

func configBytes(t *testing.T, s ConfigSnapshot) []byte {
	t.Helper()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestEdgeConfigDecoderRejectsUnsupportedWithoutIO(t *testing.T) {
	decoder := NewEdgeConfigDecoder(checker.Get, notifier.Get)
	base := m2Config(t)
	target := domain.ProbeConfigTarget{HubID: base.HubID, ProbeID: base.ProbeID}
	resolved, err := decoder.DecodeEdge(t.Context(), configBytes(t, base), target)
	if err != nil || len(resolved.Assignments) != 1 || resolved.Assignments[0].Proxy == nil || resolved.Assignments[0].Proxy.Password != "fixture-secret" || resolved.Assignments[0].NotificationLinks[0].IncludeTarget || resolved.Assignments[0].EffectiveOwner != "Operations" || resolved.Maintenance[20].Timezone != "Asia/Bangkok" {
		t.Fatalf("lost accepted execution context: %+v %v", resolved, err)
	}
	for name, mutate := range map[string]func(*ConfigSnapshot){
		"certificate": func(s *ConfigSnapshot) { s.Assignments[0].Monitor.CertExpiryNotify = true },
		"docker resource binding": func(s *ConfigSnapshot) {
			s.Assignments[0].Monitor.Type = "docker"
			s.Assignments[0].Monitor.Config = json.RawMessage(`{"container":"phoenix"}`)
			s.Assignments[0].RequiredCapabilities = []string{"checker.docker.v1"}
			s.Assignments[0].ResourceBindings = []ResourceBinding{{BindingKey: "docker", Kind: "docker_socket"}}
		},
		"enabled escalation": func(s *ConfigSnapshot) {
			s.EscalationPolicies[0].Enabled = true
			s.EscalationPolicies[0].Steps = []ConfigEscalationStep{{Step: 1, DelaySeconds: 60, NotificationIDs: []int64{10}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := m2Config(t)
			mutate(&s)
			got, err := decoder.DecodeEdge(t.Context(), configBytes(t, s), target)
			if !errors.Is(err, ErrUnsupportedCapability) || got != nil {
				t.Fatalf("unsupported config accepted: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "example.test") {
				t.Fatal("secret in rejection")
			}
		})
	}
	// The protocol itself forbids remote ACK links, before capability dispatch.
	ack := m2Config(t)
	ack.NotificationChannels[0].IncludeAckURL = true
	if got, err := decoder.DecodeEdge(t.Context(), configBytes(t, ack), target); got != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("protocol-invalid remote ACK link accepted: %v", err)
	}
}

// TestEdgeConfigDecoderAcceptsEveryPullCheckerType proves edge activation
// accepts each pull checker the build installs, through the same installed
// validators the edge runtime uses. Docker requires an advertised resource
// binding and is exercised by the separate resource binding tests.
func TestEdgeConfigDecoderAcceptsEveryPullCheckerType(t *testing.T) {
	decoder := NewEdgeConfigDecoder(checker.Get, notifier.Get)
	for kind, config := range remotePullCheckerConfigs {
		t.Run(kind, func(t *testing.T) {
			s := m2Config(t)
			s.Assignments[0].Monitor.Type = kind
			s.Assignments[0].RequiredCapabilities = []string{"checker." + kind + ".v1"}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			s.Assignments[0].Monitor.Config = raw
			target := domain.ProbeConfigTarget{HubID: s.HubID, ProbeID: s.ProbeID}
			resolved, err := decoder.DecodeEdge(t.Context(), configBytes(t, s), target)
			if err != nil || len(resolved.Assignments) != 1 {
				t.Fatalf("pull checker assignment rejected: %+v %v", resolved, err)
			}
			if resolved.Assignments[0].Monitor.Type != kind || len(resolved.Assignments[0].Monitor.Config) != len(config) {
				t.Fatalf("execution settings lost: %s %+v", resolved.Assignments[0].Monitor.Type, resolved.Assignments[0].Monitor.Config)
			}
		})
	}
}

func TestEdgeConfigActivationRetainsExactBytesAndColdValidation(t *testing.T) {
	for _, kind := range []string{"http", "docker"} {
		t.Run(kind, func(t *testing.T) { testEdgeConfigColdValidation(t, kind) })
	}
}

func testEdgeConfigColdValidation(t *testing.T, kind string) {
	snapshot := m2Config(t)
	var resources *LocalResourceBindings
	if kind == "docker" {
		var err error
		resources, err = LoadResourceBindings(t.Context(), resourceFile(t, `[{"binding_key":"docker","kind":"docker_socket","endpoint":"unix:///tmp/probe.sock"}]`))
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Assignments[0].Monitor.Type = "docker"
		snapshot.Assignments[0].Monitor.Config = json.RawMessage(`{"container":"phoenix"}`)
		snapshot.Assignments[0].RequiredCapabilities = []string{"checker.docker.v1"}
		snapshot.Assignments[0].ResourceBindings = resources.Inventory()
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity := domain.EdgeIdentity{ProbeID: snapshot.ProbeID, StreamID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}
	store, err := edge.Open(t.Context(), dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	at := time.Now().UTC()
	hash := sha256.Sum256([]byte("local-test-authorization"))
	if err := store.IssueEnrollmentToken(t.Context(), domain.EdgeEnrollmentToken{Hash: hash, IssuedAt: at, ExpiresAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitEnrollment(t.Context(), hash, domain.EdgeEnrollment{HubID: snapshot.HubID, ProbeID: snapshot.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1, TokenHash: sha256.Sum256([]byte("runtime-test")), AppliedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptConnectionGeneration(t.Context(), snapshot.HubID, 7); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get, resources), protector)
	document := configBytes(t, snapshot)
	wantHash := sha256.Sum256(document)
	if _, err := svc.Apply(t.Context(), document, 6, at); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale session applied: %v", err)
	}
	applied, err := svc.Apply(t.Context(), document, 7, at)
	if err != nil || applied.Metadata.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("exact document not applied: %+v %v", applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := edge.Open(t.Context(), dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	cold := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get, resources), protector)
	loaded, err := cold.Load(t.Context())
	if err != nil || !domain.SameProbeConfigMetadata(loaded.Metadata, applied.Metadata) || loaded.Assignments[0].Monitor.ID != 42 {
		t.Fatalf("cold read changed config: %+v %v", loaded, err)
	}
	if kind == "docker" {
		missing := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
		if _, err := missing.Load(t.Context()); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatal("cold load accepted missing binding", err)
		}
		rejected := snapshot
		rejected.Revision++
		for i := range rejected.NotificationChannels {
			rejected.NotificationChannels[i].Version = rejected.Revision
		}
		for i := range rejected.NotificationTemplates {
			rejected.NotificationTemplates[i].Version = rejected.Revision
		}
		for i := range rejected.ProxyBindings {
			rejected.ProxyBindings[i].Version = rejected.Revision
		}
		for i := range rejected.EscalationPolicies {
			rejected.EscalationPolicies[i].Version = rejected.Revision
		}
		rejected.Assignments[0].ResourceBindings = []ResourceBinding{{BindingKey: "missing", Kind: "docker_socket"}}
		if _, err := cold.Apply(t.Context(), configBytes(t, rejected), 7, at); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatal("unresolved replacement applied", err)
		}
		if still, err := reopened.ReadActiveConfig(t.Context()); err != nil || still.Snapshot.Revision != int64(snapshot.Revision) {
			t.Fatal("rejection changed durable config", err)
		}
	}
	active, err := reopened.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(t.Context(), active.Snapshot.ProbeConfigMetadata, active.Snapshot.ProtectedPayload)
	if err != nil || !bytes.Equal(plain, document) {
		t.Fatalf("original bytes not retained: %v", err)
	}
	wrongKey, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x74}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get, resources), wrongKey).Load(t.Context()); err == nil {
		t.Fatal("cold load accepted foreign protection key")
	}
}
