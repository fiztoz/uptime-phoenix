package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeCommandProtectionBindsImmutableRequest(t *testing.T) {
	p, err := NewProbeConfigProtector(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"private_operator_note":"do not expose command payload"}`)
	sum := sha256.Sum256(plain)
	source, generation := "51111111-2222-4333-8444-555555555555", int64(7)
	at := time.Now().UTC().Truncate(time.Microsecond)
	m := domain.ProbeCommandMetadata{CommandID: "41111111-2222-4333-8444-555555555555", HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555", Kind: "alert.ack", SourceAlertID: &source, AssignmentGeneration: &generation, CreatedAt: at, ExpiresAt: at.Add(time.Hour), PayloadSHA256: hex.EncodeToString(sum[:])}
	cipher, err := p.SealCommand(t.Context(), m, plain)
	if err != nil || bytes.Contains(cipher, plain) {
		t.Fatal("command not protected", err)
	}
	got, err := p.OpenCommand(t.Context(), m, cipher)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("request round trip failed", err)
	}
	for name, mutate := range map[string]func(*domain.ProbeCommandMetadata){
		"command":    func(m *domain.ProbeCommandMetadata) { m.CommandID = m.HubID },
		"hub":        func(m *domain.ProbeCommandMetadata) { m.HubID = m.ProbeID },
		"probe":      func(m *domain.ProbeCommandMetadata) { m.ProbeID = m.HubID },
		"stream":     func(m *domain.ProbeCommandMetadata) { m.StreamID = m.HubID },
		"kind":       func(m *domain.ProbeCommandMetadata) { m.Kind = "history.clear"; m.SourceAlertID = nil },
		"source":     func(m *domain.ProbeCommandMetadata) { v := m.HubID; m.SourceAlertID = &v },
		"generation": func(m *domain.ProbeCommandMetadata) { v := int64(8); m.AssignmentGeneration = &v },
		"created":    func(m *domain.ProbeCommandMetadata) { m.CreatedAt = m.CreatedAt.Add(-time.Second) },
		"expires":    func(m *domain.ProbeCommandMetadata) { m.ExpiresAt = m.ExpiresAt.Add(time.Second) },
		"hash":       func(m *domain.ProbeCommandMetadata) { m.PayloadSHA256 = hex.EncodeToString(make([]byte, 32)) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := m
			mutate(&changed)
			got, err := p.OpenCommand(t.Context(), changed, cipher)
			if err == nil || len(got) != 0 {
				t.Fatal("foreign scope exposed request")
			}
		})
	}
	wrong, err := NewProbeConfigProtector(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wrong.OpenCommand(t.Context(), m, cipher); err == nil || len(got) > 0 {
		t.Fatal("wrong key exposed request")
	}
	if _, err := p.SealCommand(t.Context(), m, []byte("different bytes")); err == nil {
		t.Fatal("digest mismatch sealed")
	}
	if _, err := p.SealCommand(t.Context(), m, make([]byte, domain.MaxProbeCommandBytes+1)); err == nil {
		t.Fatal("unbounded payload sealed")
	}
}
