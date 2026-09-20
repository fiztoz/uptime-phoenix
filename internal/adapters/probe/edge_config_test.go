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
		"watchdog":    func(s *ConfigSnapshot) { s.Watchdog.Enabled = true },
		"certificate": func(s *ConfigSnapshot) { s.Assignments[0].Monitor.CertExpiryNotify = true },
		"unsupported checker": func(s *ConfigSnapshot) {
			s.Assignments[0].Monitor.Type = "ping"
			s.Assignments[0].RequiredCapabilities = []string{"checker.ping.v1"}
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

func TestEdgeConfigActivationRetainsExactBytesAndColdValidation(t *testing.T) {
	snapshot := m2Config(t)
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
	svc := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
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
	cold := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	loaded, err := cold.Load(t.Context())
	if err != nil || !domain.SameProbeConfigMetadata(loaded.Metadata, applied.Metadata) || loaded.Assignments[0].Monitor.ID != 42 {
		t.Fatalf("cold read changed config: %+v %v", loaded, err)
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
	if _, err := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get), wrongKey).Load(t.Context()); err == nil {
		t.Fatal("cold load accepted foreign protection key")
	}
}
