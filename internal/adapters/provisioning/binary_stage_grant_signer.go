package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"reflect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Ed25519ProbeStageGrantSigner owns an injected copy of a dedicated authority
// key. It performs no file, environment, credential-store or network access.
// Signing alone is not DB authority; only the consuming use case may call it.
type Ed25519ProbeStageGrantSigner struct {
	privateKey ed25519.PrivateKey
	publicKey  [32]byte
}

var _ ports.ProbeBinaryStageGrantSigner = (*Ed25519ProbeStageGrantSigner)(nil)

// NewEd25519ProbeStageGrantSigner validates all expanded private-key bytes and
// copies them. No persistent key is generated and no heap-zeroization is claimed.
func NewEd25519ProbeStageGrantSigner(privateKey []byte) (*Ed25519ProbeStageGrantSigner, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, domain.ErrValidation
	}
	derived := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	if !bytes.Equal(derived, privateKey) {
		return nil, domain.ErrValidation
	}
	var publicKey [32]byte
	copy(publicKey[:], derived[ed25519.SeedSize:])
	return &Ed25519ProbeStageGrantSigner{privateKey: derived, publicKey: publicKey}, nil
}

// PublicKey returns a defensive value copy, never private-key bytes.
func (s *Ed25519ProbeStageGrantSigner) PublicKey() [32]byte {
	if s == nil {
		return [32]byte{}
	}
	return s.publicKey
}

// SignStageAdmission signs only the fixed valid admission message. It neither
// queries authority nor starts detached work, and errors never contain key bytes.
func (s *Ed25519ProbeStageGrantSigner) SignStageAdmission(ctx context.Context, admission domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
	var zero domain.ProbeBinaryStageGrant
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return zero, domain.ErrInternal
	}
	if ctx == nil || (reflect.ValueOf(ctx).Kind() == reflect.Pointer && reflect.ValueOf(ctx).IsNil()) {
		return zero, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !admission.Valid() || admission.Request.Challenge.AuthorityKeyID != domain.ProbeBinaryStageAuthorityKeyID(s.publicKey) {
		return zero, domain.ErrValidation
	}
	grant := domain.ProbeBinaryStageGrant{Request: admission.Request}
	copy(grant.Signature[:], ed25519.Sign(s.privateKey, admission.Request.SigningMessage()))
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return grant, nil
}
