package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeConfigProtection(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{42}, 32)
	p, err := NewProbeConfigProtector(key)
	if err != nil {
		t.Fatal(err)
	}
	m := domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"},
		Revision: 1, SchemaVersion: 1, SHA256: strings.Repeat("a", 64), CreatedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC()}
	secret := []byte("confidential-provider-credential")
	first, err := p.Seal(ctx, m, secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Seal(ctx, m, secret)
	if err != nil || bytes.Equal(first, second) || bytes.Contains(first, secret) || len(first) != len(secret)+domain.ProbeConfigProtectionOverhead {
		t.Fatal("protection did not use fresh bounded ciphertext")
	}
	key[0] ^= 1
	plain, err := p.Open(ctx, m, first)
	if err != nil || !bytes.Equal(plain, secret) {
		t.Fatal("key alias changed protection")
	}
	wrong, err := NewProbeConfigProtector(key)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := wrong.Open(ctx, m, first); err == nil || plain != nil {
		t.Fatal("wrong key returned plaintext")
	}
	for name, mutate := range map[string]func(*domain.ProbeConfigMetadata){
		"hub":       func(m *domain.ProbeConfigMetadata) { m.HubID = "22222222-2222-4222-8222-222222222222" },
		"probe":     func(m *domain.ProbeConfigMetadata) { m.ProbeID = "22222222-2222-4222-8222-222222222222" },
		"revision":  func(m *domain.ProbeConfigMetadata) { m.Revision++ },
		"schema":    func(m *domain.ProbeConfigMetadata) { m.SchemaVersion++ },
		"hash":      func(m *domain.ProbeConfigMetadata) { m.SHA256 = strings.Repeat("b", 64) },
		"created":   func(m *domain.ProbeConfigMetadata) { m.CreatedAt = m.CreatedAt.Add(time.Second) },
		"effective": func(m *domain.ProbeConfigMetadata) { m.EffectiveAt = m.EffectiveAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := m
			mutate(&changed)
			if plain, err := p.Open(ctx, changed, first); err == nil || plain != nil {
				t.Fatal("metadata substitution returned plaintext")
			}
		})
	}
	for i := range first {
		changed := bytes.Clone(first)
		changed[i] ^= 1
		if plain, err := p.Open(ctx, m, changed); err == nil || plain != nil {
			t.Fatal("tampered ciphertext returned plaintext")
		}
	}
	for _, size := range []int{0, 16, 24, 31, 33} {
		if _, err := NewProbeConfigProtector(make([]byte, size)); err == nil {
			t.Fatal("invalid key size accepted")
		}
	}
	if _, err := p.Seal(ctx, m, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("empty plaintext accepted")
	}
	if _, err := p.Seal(ctx, m, make([]byte, domain.MaxProbeConfigBytes+1)); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("oversized plaintext accepted")
	}
	for _, payload := range [][]byte{nil, first[:5], append(bytes.Clone(first), 0)} {
		if plain, err := p.Open(ctx, m, payload); err == nil || plain != nil {
			t.Fatal("malformed ciphertext accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Seal(canceled, m, secret); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation")
	}
	if _, err := p.Open(canceled, m, first); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation")
	}
}
