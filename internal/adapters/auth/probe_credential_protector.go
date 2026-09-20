package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeCredentialProtector = (*ProbeConfigProtector)(nil)

// SealCredential uses a separate authenticated purpose and complete connection
// identity, keeping credentials distinct from the snapshot encryption purpose.
func (p *ProbeConfigProtector) SealCredential(ctx context.Context, m domain.ProbeCredentialMetadata, token string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeCredentialMetadata(m) || !validProtectedRuntimeToken(token) {
		return nil, domain.ErrValidation
	}
	return p.aead.Seal([]byte{1}, nil, []byte(token), credentialAssociatedData(m)), nil
}

// OpenCredential returns plaintext only after authenticating its immutable scope.
func (p *ProbeConfigProtector) OpenCredential(ctx context.Context, m domain.ProbeCredentialMetadata, ciphertext []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p == nil || p.aead == nil || !domain.ValidProbeCredentialMetadata(m) || len(ciphertext) < 30 || len(ciphertext) > 300 || ciphertext[0] != 1 {
		return "", domain.ErrValidation
	}
	plain, err := p.aead.Open(nil, nil, ciphertext[1:], credentialAssociatedData(m))
	if err != nil {
		return "", errors.New("probe credential authentication failed")
	}
	defer clear(plain)
	token := string(plain)
	if !validProtectedRuntimeToken(token) {
		return "", domain.ErrValidation
	}
	return token, nil
}

func validProtectedRuntimeToken(token string) bool {
	if len(token) > 256 || !strings.HasPrefix(token, "phx_probe_") || strings.HasPrefix(token, "phx_probe_enroll_") {
		return false
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, "phx_probe_"))
	return err == nil && len(data) >= 32
}

func credentialAssociatedData(m domain.ProbeCredentialMetadata) []byte {
	data, _ := json.Marshal([]any{"phoenix.credential.aes256gcm.v1", m.HubID, m.ProbeID, m.StreamID, m.EnrollmentID, m.CredentialVersion, m.Endpoint, m.Fingerprint})
	return data
}
