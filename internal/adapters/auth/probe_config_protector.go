package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeConfigProtector protects snapshot bytes with AES-256-GCM random nonces.
// The caller owns durable key provisioning and backup; keys are never persisted
// with snapshots. Construction does not generate or silently replace a key.
type ProbeConfigProtector struct {
	aead    cipher.AEAD
	keyHash func(hubID string) string
}

var _ ports.ProbeConfigProtector = (*ProbeConfigProtector)(nil)

// NewProbeConfigProtector requires a dedicated 32-byte installation key.
func NewProbeConfigProtector(key []byte) (*ProbeConfigProtector, error) {
	if len(key) != 32 {
		return nil, errors.New("configuration protection requires a 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	keyCopy := bytes.Clone(key)
	keyHashFn := func(hubID string) string {
		mac := hmac.New(sha256.New, keyCopy)
		mac.Write([]byte("phoenix-probe-key-v1:" + hubID))
		return hex.EncodeToString(mac.Sum(nil))
	}
	return &ProbeConfigProtector{aead: aead, keyHash: keyHashFn}, nil
}

// KeyHash returns the hex-encoded HMAC-SHA256 of the key bound to the trusted hubID.
func (p *ProbeConfigProtector) KeyHash(hubID string) string {
	if p == nil || p.keyHash == nil || !domain.ValidHubID(hubID) {
		return ""
	}
	return p.keyHash(hubID)
}

// Seal returns a format byte followed by a fresh nonce and authenticated content.
func (p *ProbeConfigProtector) Seal(ctx context.Context, metadata domain.ProbeConfigMetadata, plaintext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeConfigMetadata(metadata) || len(plaintext) == 0 || len(plaintext) > domain.MaxProbeConfigBytes {
		return nil, domain.ErrValidation
	}
	aad := configAssociatedData(metadata)
	return p.aead.Seal([]byte{1}, nil, plaintext, aad), nil
}

// Open authenticates metadata and ciphertext before returning any plaintext.
func (p *ProbeConfigProtector) Open(ctx context.Context, metadata domain.ProbeConfigMetadata, ciphertext []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeConfigMetadata(metadata) || len(ciphertext) <= domain.ProbeConfigProtectionOverhead ||
		len(ciphertext) > domain.MaxProbeConfigBytes+domain.ProbeConfigProtectionOverhead || ciphertext[0] != 1 {
		return nil, domain.ErrValidation
	}
	plaintext, err := p.aead.Open(nil, nil, ciphertext[1:], configAssociatedData(metadata))
	if err != nil {
		return nil, errors.New("configuration authentication failed")
	}
	return plaintext, nil
}

func configAssociatedData(metadata domain.ProbeConfigMetadata) []byte {
	m := domain.NormalizeProbeConfigMetadata(metadata)
	// A fixed positional array avoids ambiguous concatenation and domain serialization.
	encoded, _ := json.Marshal([]any{"phoenix.config.aes256gcm.v1", m.HubID, m.ProbeID, m.Revision, m.SchemaVersion,
		m.SHA256, m.CreatedAt.Format("2006-01-02T15:04:05.000000Z"), m.EffectiveAt.Format("2006-01-02T15:04:05.000000Z")})
	return encoded
}
