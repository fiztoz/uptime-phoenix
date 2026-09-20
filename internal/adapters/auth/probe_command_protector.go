package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeCommandProtector = (*ProbeConfigProtector)(nil)

// SealCommand protects exact immutable request bytes under a distinct purpose.
func (p *ProbeConfigProtector) SealCommand(ctx context.Context, m domain.ProbeCommandMetadata, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeCommandMetadata(m) || !commandPayloadMatches(m, payload) {
		return nil, domain.ErrValidation
	}
	return p.aead.Seal([]byte{1}, nil, payload, commandAssociatedData(m)), nil
}

// OpenCommand authenticates scope and exact digest before exposing a request.
func (p *ProbeConfigProtector) OpenCommand(ctx context.Context, m domain.ProbeCommandMetadata, ciphertext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeCommandMetadata(m) || len(ciphertext) < 30 || len(ciphertext) > domain.MaxProbeCommandBytes+64 || ciphertext[0] != 1 {
		return nil, domain.ErrValidation
	}
	plain, err := p.aead.Open(nil, nil, ciphertext[1:], commandAssociatedData(m))
	if err != nil {
		return nil, errors.New("probe command authentication failed")
	}
	if !commandPayloadMatches(m, plain) {
		clear(plain)
		return nil, domain.ErrValidation
	}
	return plain, nil
}

func commandPayloadMatches(m domain.ProbeCommandMetadata, plain []byte) bool {
	if len(plain) == 0 || len(plain) > domain.MaxProbeCommandBytes {
		return false
	}
	sum := sha256.Sum256(plain)
	return hex.EncodeToString(sum[:]) == m.PayloadSHA256
}

func commandAssociatedData(m domain.ProbeCommandMetadata) []byte {
	data, _ := json.Marshal([]any{"phoenix.command.aes256gcm.v1", m.CommandID, m.HubID, m.ProbeID, m.StreamID, m.Kind, m.SourceAlertID, m.AssignmentGeneration, m.CreatedAt.UTC().Format(time.RFC3339Nano), m.ExpiresAt.UTC().Format(time.RFC3339Nano), m.PayloadSHA256})
	return data
}
