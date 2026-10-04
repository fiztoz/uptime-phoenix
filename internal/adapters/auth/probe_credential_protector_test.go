package auth

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeCredentialProtectionBindsCompleteScope(t *testing.T) {
	p, err := NewProbeConfigProtector(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	m := domain.ProbeCredentialMetadata{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", StreamID: "31111111-2222-4333-8444-555555555555", EnrollmentID: "41111111-2222-4333-8444-555555555555", CredentialVersion: 1, Endpoint: "wss://edge.example/ws/probe/v1", Fingerprint: strings.Repeat("a", 64)}
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	cipher, err := p.SealCredential(t.Context(), m, token)
	if err != nil || bytes.Contains(cipher, []byte(token)) {
		t.Fatalf("credential was not protected: %v", err)
	}
	if got, err := p.OpenCredential(t.Context(), m, cipher); err != nil || got != token {
		t.Fatal("credential round trip failed")
	}
	for name, mutate := range map[string]func(*domain.ProbeCredentialMetadata){
		"hub": func(m *domain.ProbeCredentialMetadata) { m.HubID = m.ProbeID }, "probe": func(m *domain.ProbeCredentialMetadata) { m.ProbeID = m.HubID }, "stream": func(m *domain.ProbeCredentialMetadata) { m.StreamID = m.HubID }, "enrollment": func(m *domain.ProbeCredentialMetadata) { m.EnrollmentID = m.HubID }, "version": func(m *domain.ProbeCredentialMetadata) { m.CredentialVersion++ }, "endpoint": func(m *domain.ProbeCredentialMetadata) { m.Endpoint = "wss://other.example/ws/probe/v1" }, "pin": func(m *domain.ProbeCredentialMetadata) { m.Fingerprint = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := m
			mutate(&changed)
			if got, err := p.OpenCredential(t.Context(), changed, cipher); err == nil || got != "" {
				t.Fatal("foreign scope exposed token")
			}
		})
	}
	wrong, err := NewProbeConfigProtector(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wrong.OpenCredential(t.Context(), m, cipher); err == nil || got != "" {
		t.Fatal("wrong key exposed token")
	}
	if _, err := p.SealCredential(t.Context(), m, "phx_probe_enroll_"+strings.TrimPrefix(token, "phx_probe_")); err == nil {
		t.Fatal("enrollment token became runtime credential")
	}
}
