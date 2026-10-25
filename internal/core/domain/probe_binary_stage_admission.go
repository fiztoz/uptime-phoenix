package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const probeBinaryStageSigningDomain = "phoenix-stage-admission-v1\x00"

// ProbeBinaryStageChallenge binds a future helper's nonce to one public authority
// and the existing canonical intent marker. It is not an authorization by itself.
type ProbeBinaryStageChallenge struct {
	AuthorityKeyID string
	Nonce          [32]byte
	MarkerSHA256   string
}

// Valid requires canonical digests and a nonzero process challenge.
func (c ProbeBinaryStageChallenge) Valid() bool {
	return stageAdmissionDigest(c.AuthorityKeyID) && stageAdmissionDigest(c.MarkerSHA256) && c.Nonce != ([32]byte{})
}

// ProbeBinaryStageAdmissionRequest names one root/systemd artifact-directory
// attempt. Its labels and challenge carry no proof of a stored reservation.
type ProbeBinaryStageAdmissionRequest struct {
	Intent    ProbeBinaryStageIntent
	Challenge ProbeBinaryStageChallenge
}

// Valid binds the whole immutable stage intent through its unchanged v1 marker.
func (r ProbeBinaryStageAdmissionRequest) Valid() bool {
	return r.Intent.Valid() && r.Intent.Identity.Host.User == "root" &&
		r.Challenge.Valid() && r.Challenge.MarkerSHA256 == r.Intent.MarkerSHA256()
}

// SigningMessage returns only the fixed domain-separated message, or nil for an
// invalid request. It performs no signing, serialization or authority lookup.
func (r ProbeBinaryStageAdmissionRequest) SigningMessage() []byte {
	if !r.Valid() {
		return nil
	}
	key, _ := hex.DecodeString(r.Challenge.AuthorityKeyID)
	marker, _ := hex.DecodeString(r.Challenge.MarkerSHA256)
	message := make([]byte, 0, len(probeBinaryStageSigningDomain)+96)
	message = append(message, probeBinaryStageSigningDomain...)
	message = append(message, key...)
	message = append(message, r.Challenge.Nonce[:]...)
	return append(message, marker...)
}

// ProbeBinaryStageAdmission is an acknowledged, irreversible DB consumption.
// It proves authority at admission only, not continuous authority or execution.
type ProbeBinaryStageAdmission struct {
	Request    ProbeBinaryStageAdmissionRequest
	AdmittedAt time.Time
}

// Valid checks the request and the journal's UTC microsecond storage contract.
// The repository independently checks the stored graph, deadline and clock.
func (a ProbeBinaryStageAdmission) Valid() bool {
	return a.Request.Valid() && installUTC(a.AdmittedAt)
}

// ProbeBinaryStageGrant is short-lived authorization material. Never serialize
// it in public views, log it, or persist its signature in the journal.
type ProbeBinaryStageGrant struct {
	Request   ProbeBinaryStageAdmissionRequest
	Signature [64]byte
}

// ValidFor verifies the exact message under the configured public authority.
// Verification is pure; it does not establish that any DB call actually occurred.
func (g ProbeBinaryStageGrant) ValidFor(publicKey [32]byte) bool {
	return publicKey != ([32]byte{}) && g.Request.Valid() &&
		g.Request.Challenge.AuthorityKeyID == ProbeBinaryStageAuthorityKeyID(publicKey) &&
		ed25519.Verify(ed25519.PublicKey(publicKey[:]), g.Request.SigningMessage(), g.Signature[:])
}

// ProbeBinaryStageAuthorityKeyID identifies raw Ed25519 public-key bytes only.
// It neither chooses nor loads a key and grants no authority.
func ProbeBinaryStageAuthorityKeyID(publicKey [32]byte) string {
	digest := sha256.Sum256(publicKey[:])
	return hex.EncodeToString(digest[:])
}

func stageAdmissionDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
