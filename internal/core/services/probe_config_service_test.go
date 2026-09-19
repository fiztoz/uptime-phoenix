package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type configServiceRepo struct {
	saved               domain.ProtectedProbeConfig
	err                 error
	saves, gets, latest int
	expected            int64
}

func (r *configServiceRepo) Save(_ context.Context, s domain.ProtectedProbeConfig, expected int64) (*domain.ProtectedProbeConfig, error) {
	r.saves++
	r.expected = expected
	if r.err != nil {
		return nil, r.err
	}
	r.saved = s
	copy := s
	return &copy, nil
}
func (r *configServiceRepo) Get(context.Context, string, int64) (*domain.ProtectedProbeConfig, error) {
	r.gets++
	copy := r.saved
	return &copy, r.err
}
func (r *configServiceRepo) Latest(context.Context, string) (*domain.ProtectedProbeConfig, error) {
	r.latest++
	copy := r.saved
	return &copy, r.err
}

type configServiceInspector struct{ err error }

func (i configServiceInspector) Inspect(doc []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
	hash := sha256.Sum256(doc)
	return domain.ProbeConfigMetadata{ProbeConfigTarget: target, Revision: 7, SchemaVersion: 1, SHA256: hex.EncodeToString(hash[:]), CreatedAt: time.Unix(100, 123456789), EffectiveAt: time.Unix(101, 123456789)}, i.err
}

type configServiceProtector struct {
	sealErr, openErr error
	seals, opens     int
	plain            []byte
}

func (p *configServiceProtector) Seal(_ context.Context, _ domain.ProbeConfigMetadata, plain []byte) ([]byte, error) {
	p.seals++
	p.plain = bytes.Clone(plain)
	return []byte("protected"), p.sealErr
}
func (p *configServiceProtector) Open(context.Context, domain.ProbeConfigMetadata, []byte) ([]byte, error) {
	p.opens++
	if p.openErr != nil {
		return nil, p.openErr
	}
	return bytes.Clone(p.plain), nil
}
func (p *configServiceProtector) KeyHash(hubID string) string {
	return strings.Repeat("a", 64)
}

func TestProbeConfigServiceConfidentialBoundary(t *testing.T) {
	ctx := context.Background()
	target := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "local"}
	doc := []byte("fixture-private-credential")
	r, p := &configServiceRepo{}, &configServiceProtector{}
	s := NewProbeConfigService(r, configServiceInspector{}, p)
	meta, err := s.Prepare(ctx, target, doc, 6)
	if err != nil || r.saves != 1 || p.seals != 1 || r.expected != 6 || bytes.Contains(r.saved.ProtectedPayload, doc) || meta.CreatedAt.Location() != time.UTC || meta.CreatedAt.Nanosecond()%1000 != 0 {
		t.Fatal("preparation boundary failed")
	}
	doc[0] = 'x'
	for _, revision := range []int64{0, 7} {
		plain, got, err := s.Read(ctx, target, revision)
		if err != nil || string(plain) != "fixture-private-credential" || !domain.SameProbeConfigMetadata(got, meta) {
			t.Fatal("read lost exact confidential bytes")
		}
	}
	if r.gets != 1 || r.latest != 1 {
		t.Fatal("exact/latest reads mixed")
	}
	for _, phase := range []string{"inspect", "seal", "store"} {
		t.Run(phase, func(t *testing.T) {
			repo, protector := &configServiceRepo{}, &configServiceProtector{}
			inspector := configServiceInspector{}
			switch phase {
			case "inspect":
				inspector.err = errors.New("secret diagnostic: fixture-private-credential")
			case "seal":
				protector.sealErr = errors.New("protection unavailable")
			case "store":
				repo.err = ports.ErrConflict
			}
			got, err := NewProbeConfigService(repo, inspector, protector).Prepare(ctx, target, []byte("fixture-private-credential"), 6)
			if err == nil || got.Revision != 0 || strings.Contains(err.Error(), "fixture-private-credential") {
				t.Fatal("failed preparation leaked metadata/secret")
			}
			if phase != "store" && repo.saves != 0 {
				t.Fatal("failed preparation wrote storage")
			}
			if phase == "store" && !errors.Is(err, ports.ErrConflict) {
				t.Fatal("lost conflict sentinel")
			}
		})
	}
	wrong := target
	wrong.HubID = "22222222-2222-4222-8222-222222222222"
	before := p.opens
	if plain, _, err := s.Read(ctx, wrong, 7); !errors.Is(err, ports.ErrConflict) || plain != nil || p.opens != before {
		t.Fatal("wrong authority decrypted data")
	}
	if plain, _, err := s.Read(ctx, target, 8); !errors.Is(err, ports.ErrConflict) || plain != nil {
		t.Fatal("wrong revision returned data")
	}
	beforeExactReads := r.gets
	p.plain = []byte("tampered-but-decrypted")
	if plain, _, err := s.Read(ctx, target, 0); err == nil || plain != nil || r.gets != beforeExactReads {
		t.Fatal("corrupt latest used old credential fallback")
	}
	for _, body := range [][]byte{nil, make([]byte, domain.MaxProbeConfigBytes+1)} {
		if _, err := s.Prepare(ctx, target, body, 6); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid size accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Prepare(canceled, target, doc, 6); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation")
	}
	if _, err := NewProbeConfigService(r, nil, p).Prepare(ctx, target, doc, 6); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("missing validator accepted")
	}
}
