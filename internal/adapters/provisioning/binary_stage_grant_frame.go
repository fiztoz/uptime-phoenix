package provisioning

import (
	"bytes"
	"encoding/hex"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	// ProbeBinaryStageGrantFrameSize is the fixed PHXGRT1 newline magic plus
	// one raw 64-byte Ed25519 signature. It follows the stage header and
	// precedes the raw pinned binary on the authorized helper's stdin.
	ProbeBinaryStageGrantFrameSize = 8 + 64
	// ProbeBinaryStageChallengeLineSize is the exact canonical challenge line,
	// including its single terminal LF.
	ProbeBinaryStageChallengeLineSize = len(stageChallengePrefix) + 64 + len(stageChallengeMiddle) + 64 + len(stageChallengeSuffix)
	// ProbeBinaryStageAuthorityKeyFileSize is the exact trusted public-key file:
	// 64 lowercase hex characters encoding raw Ed25519 bytes, then one LF.
	ProbeBinaryStageAuthorityKeyFileSize = 64 + 1
)

const (
	stageGrantMagic      = "PHXGRT1\n"
	stageChallengePrefix = `{"state":"stage_challenge","authority_key_id":"`
	stageChallengeMiddle = `","nonce":"`
	stageChallengeSuffix = "\"}\n"
)

// EncodeProbeBinaryStageChallengeLine renders the one canonical challenge line
// an authorized helper emits before it reads stdin. It binds a helper-chosen
// nonce to the helper's trusted authority; it carries no marker, because the
// helper has not yet read the intent. Invalid input returns nil.
func EncodeProbeBinaryStageChallengeLine(authorityKeyID string, nonce [32]byte) []byte {
	if !stageGrantLowerHex(authorityKeyID) || nonce == ([32]byte{}) {
		return nil
	}
	line := make([]byte, 0, ProbeBinaryStageChallengeLineSize)
	line = append(line, stageChallengePrefix...)
	line = append(line, authorityKeyID...)
	line = append(line, stageChallengeMiddle...)
	line = append(line, hex.EncodeToString(nonce[:])...)
	return append(line, stageChallengeSuffix...)
}

// ParseProbeBinaryStageChallengeLine accepts only the exact canonical bytes
// EncodeProbeBinaryStageChallengeLine produces: no whitespace, key order,
// escaping, case or trailing variation. A zero nonce is refused. The parsed
// values are an unauthenticated request for a grant, never a grant.
func ParseProbeBinaryStageChallengeLine(line []byte) (string, [32]byte, error) {
	var nonce [32]byte
	if len(line) != ProbeBinaryStageChallengeLineSize ||
		!bytes.HasPrefix(line, []byte(stageChallengePrefix)) || !bytes.HasSuffix(line, []byte(stageChallengeSuffix)) {
		return "", nonce, domain.ErrProbeBinaryStageWriteInput
	}
	body := line[len(stageChallengePrefix) : len(line)-len(stageChallengeSuffix)]
	keyID, middle, nonceHex := string(body[:64]), body[64:64+len(stageChallengeMiddle)], string(body[64+len(stageChallengeMiddle):])
	if string(middle) != stageChallengeMiddle || !stageGrantLowerHex(keyID) || !stageGrantLowerHex(nonceHex) {
		return "", nonce, domain.ErrProbeBinaryStageWriteInput
	}
	if _, err := hex.Decode(nonce[:], []byte(nonceHex)); err != nil || nonce == ([32]byte{}) {
		return "", [32]byte{}, domain.ErrProbeBinaryStageWriteInput
	}
	return keyID, nonce, nil
}

// EncodeProbeBinaryStageGrantFrame writes the fixed magic and raw signature of
// a structurally valid grant. It does not verify the signature: the helper's
// own trusted key is the only verification authority. The frame carries no
// request fields; the helper rebuilds them from its nonce and decoded header.
func EncodeProbeBinaryStageGrantFrame(grant domain.ProbeBinaryStageGrant) ([]byte, error) {
	if !grant.Request.Valid() || grant.Signature == ([64]byte{}) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	frame := make([]byte, 0, ProbeBinaryStageGrantFrameSize)
	frame = append(frame, stageGrantMagic...)
	return append(frame, grant.Signature[:]...), nil
}

// DecodeProbeBinaryStageGrantFrame returns the raw signature of one exact
// frame. A decoded signature is unverified until the helper checks it.
func DecodeProbeBinaryStageGrantFrame(frame []byte) ([64]byte, error) {
	var signature [64]byte
	if len(frame) != ProbeBinaryStageGrantFrameSize || string(frame[:len(stageGrantMagic)]) != stageGrantMagic {
		return signature, domain.ErrProbeBinaryStageWriteInput
	}
	copy(signature[:], frame[len(stageGrantMagic):])
	if signature == ([64]byte{}) {
		return [64]byte{}, domain.ErrProbeBinaryStageWriteInput
	}
	return signature, nil
}

// EncodeProbeBinaryStageAuthorityKey renders the canonical trusted public-key
// file an operator installs on the host. It encodes public bytes only.
func EncodeProbeBinaryStageAuthorityKey(publicKey [32]byte) []byte {
	if publicKey == ([32]byte{}) {
		return nil
	}
	return append([]byte(hex.EncodeToString(publicKey[:])), '\n')
}

// ParseProbeBinaryStageAuthorityKey accepts only the exact canonical file:
// 64 lowercase hex characters and one LF. A zero key is refused. File
// ownership and mode are the caller's responsibility; content alone never
// establishes that the operator installed it.
func ParseProbeBinaryStageAuthorityKey(body []byte) ([32]byte, error) {
	var key [32]byte
	if len(body) != ProbeBinaryStageAuthorityKeyFileSize || body[64] != '\n' || !stageGrantLowerHex(string(body[:64])) {
		return key, domain.ErrProbeBinaryStageWriteInput
	}
	if _, err := hex.Decode(key[:], body[:64]); err != nil || key == ([32]byte{}) {
		return [32]byte{}, domain.ErrProbeBinaryStageWriteInput
	}
	return key, nil
}

// stageGrantLowerHex reports whether value is exactly 64 lowercase hexadecimal
// digits: the shape of an authority key ID and of a grant nonce.
func stageGrantLowerHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
