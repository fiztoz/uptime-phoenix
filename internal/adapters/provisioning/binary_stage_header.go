package provisioning

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	// ProbeBinaryStagePrefixSize is the fixed PHXSTG1 newline magic plus a
	// big-endian uint32 JSON length. It is not a payload bound.
	ProbeBinaryStagePrefixSize = 12
	// MaxProbeBinaryStageHeaderSize is the maximum accepted JSON object size.
	// The raw pinned binary is never part of this codec.
	MaxProbeBinaryStageHeaderSize = 4096

	probeBinaryStageHeaderKeyCount = 14
)

const (
	stageBitOperationID uint16 = 1 << iota
	stageBitOwnerID
	stageBitAttemptID
	stageBitSSHAddress
	stageBitSSHUser
	stageBitHostKey
	stageBitClientKey
	stageBitRequestedBy
	stageBitLayout
	stageBitMode
	stageBitVersion
	stageBitArchitecture
	stageBitSHA256
	stageBitSize
	stageBitAll = (1 << probeBinaryStageHeaderKeyCount) - 1
)

// probeBinaryStageHeaderDTO is the flat wire object. Domain structs stay
// untagged; callers assign validated fields after this decode.
type probeBinaryStageHeaderDTO struct {
	OperationID          string `json:"operation_id"`
	OwnerID              string `json:"owner_id"`
	AttemptID            string `json:"attempt_id"`
	SSHAddress           string `json:"ssh_address"`
	SSHUser              string `json:"ssh_user"`
	HostKeyFingerprint   string `json:"host_key_fingerprint"`
	ClientKeyFingerprint string `json:"client_key_fingerprint"`
	RequestedBy          int64  `json:"requested_by"`
	LayoutVersion        int    `json:"layout_version"`
	Mode                 string `json:"mode"`
	Version              string `json:"version"`
	Architecture         string `json:"architecture"`
	SHA256               string `json:"sha256"`
	Size                 int64  `json:"size"`
}

// ProbeBinaryStageHeaderLength reads an exact 12-byte prefix. N must be
// 1..4096. It does not allocate from or inspect any following payload.
func ProbeBinaryStageHeaderLength(prefix []byte) (int, error) {
	if len(prefix) != ProbeBinaryStagePrefixSize || !probeBinaryStageMagic(prefix) {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	n := binary.BigEndian.Uint32(prefix[8:12])
	if n == 0 || n > MaxProbeBinaryStageHeaderSize {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	return int(n), nil
}

// DecodeProbeBinaryStageHeader parses one bounded header into a complete
// systemd stage intent. The slice must be exactly the prefix plus N JSON
// bytes: no payload, trailer, or partial intent is accepted.
func DecodeProbeBinaryStageHeader(header []byte) (domain.ProbeBinaryStageIntent, error) {
	if len(header) < ProbeBinaryStagePrefixSize || len(header) > ProbeBinaryStagePrefixSize+MaxProbeBinaryStageHeaderSize {
		return domain.ProbeBinaryStageIntent{}, domain.ErrProbeBinaryStageWriteInput
	}
	n, err := ProbeBinaryStageHeaderLength(header[:ProbeBinaryStagePrefixSize])
	if err != nil || len(header) != ProbeBinaryStagePrefixSize+n {
		return domain.ProbeBinaryStageIntent{}, domain.ErrProbeBinaryStageWriteInput
	}
	intent, ok := decodeProbeBinaryStageJSON(header[ProbeBinaryStagePrefixSize:])
	if !ok {
		return domain.ProbeBinaryStageIntent{}, domain.ErrProbeBinaryStageWriteInput
	}
	return intent, nil
}

// EncodeProbeBinaryStageHeader writes the canonical prefix and flat JSON for
// a complete valid intent. Invalid intents return nil. The binary payload is
// not encoded.
func EncodeProbeBinaryStageHeader(intent domain.ProbeBinaryStageIntent) ([]byte, error) {
	if !intent.Valid() {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	payload, err := json.Marshal(probeBinaryStageHeaderFromIntent(intent))
	if err != nil || len(payload) == 0 || len(payload) > MaxProbeBinaryStageHeaderSize {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	out := make([]byte, ProbeBinaryStagePrefixSize+len(payload))
	out[0], out[1], out[2], out[3], out[4], out[5], out[6], out[7] = 'P', 'H', 'X', 'S', 'T', 'G', '1', '\n'
	binary.BigEndian.PutUint32(out[8:12], uint32(len(payload)))
	copy(out[12:], payload)
	return out, nil
}

func probeBinaryStageMagic(prefix []byte) bool {
	return prefix[0] == 'P' && prefix[1] == 'H' && prefix[2] == 'X' && prefix[3] == 'S' &&
		prefix[4] == 'T' && prefix[5] == 'G' && prefix[6] == '1' && prefix[7] == '\n'
}

func probeBinaryStageHeaderFromIntent(intent domain.ProbeBinaryStageIntent) probeBinaryStageHeaderDTO {
	id, pin := intent.Identity, intent.Identity.Binary
	return probeBinaryStageHeaderDTO{
		OperationID:          id.OperationID,
		OwnerID:              intent.OwnerID,
		AttemptID:            intent.AttemptID,
		SSHAddress:           id.Host.Address,
		SSHUser:              id.Host.User,
		HostKeyFingerprint:   id.HostKeyFingerprint,
		ClientKeyFingerprint: id.ClientKeyFingerprint,
		RequestedBy:          id.RequestedBy,
		LayoutVersion:        id.LayoutVersion,
		Mode:                 id.Mode,
		Version:              pin.Version,
		Architecture:         pin.Architecture,
		SHA256:               pin.SHA256,
		Size:                 pin.Size,
	}
}

func decodeProbeBinaryStageJSON(body []byte) (domain.ProbeBinaryStageIntent, bool) {
	if len(body) == 0 || len(body) > MaxProbeBinaryStageHeaderSize || !utf8.Valid(body) {
		return domain.ProbeBinaryStageIntent{}, false
	}
	parsed, ok := parseProbeBinaryStageHeader(body)
	if !ok {
		return domain.ProbeBinaryStageIntent{}, false
	}
	var dto probeBinaryStageHeaderDTO
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&dto) != nil {
		return domain.ProbeBinaryStageIntent{}, false
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF || dto != parsed {
		return domain.ProbeBinaryStageIntent{}, false
	}
	intent := domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          dto.OperationID,
			Host:                 domain.ProbeSSHHost{Address: dto.SSHAddress, User: dto.SSHUser},
			HostKeyFingerprint:   dto.HostKeyFingerprint,
			ClientKeyFingerprint: dto.ClientKeyFingerprint,
			RequestedBy:          dto.RequestedBy,
			LayoutVersion:        dto.LayoutVersion,
			Mode:                 dto.Mode,
			Binary: domain.ProbeBinaryPin{
				Version:      dto.Version,
				Architecture: dto.Architecture,
				SHA256:       dto.SHA256,
				Size:         dto.Size,
			},
		},
		OwnerID:   dto.OwnerID,
		AttemptID: dto.AttemptID,
	}
	if !intent.Valid() {
		return domain.ProbeBinaryStageIntent{}, false
	}
	return intent, true
}

// parseProbeBinaryStageHeader requires one flat object with exactly the 14
// canonical key spellings. encoding/json struct binding is not enough: it
// accepts case aliases and keeps the last duplicate, including a duplicate
// written with escapes. Escapes are not canonical spellings. Nested values are
// refused at the first delimiter, without walking them.
func parseProbeBinaryStageHeader(body []byte) (probeBinaryStageHeaderDTO, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return probeBinaryStageHeaderDTO{}, false
	}
	var parsed probeBinaryStageHeaderDTO
	var seen uint16
	for dec.More() {
		before := dec.InputOffset()
		keyTok, err := dec.Token()
		after := dec.InputOffset()
		if err != nil {
			return probeBinaryStageHeaderDTO{}, false
		}
		key, ok := keyTok.(string)
		if !ok || !canonicalStageHeaderKeyLiteral(body, before, after, key) {
			return probeBinaryStageHeaderDTO{}, false
		}
		val, err := dec.Token()
		if err != nil {
			return probeBinaryStageHeaderDTO{}, false
		}
		bit, ok := stageHeaderBit(key)
		if !ok || seen&bit != 0 || !applyStageHeaderToken(&parsed, key, val) {
			return probeBinaryStageHeaderDTO{}, false
		}
		seen |= bit
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || seen != stageBitAll {
		return probeBinaryStageHeaderDTO{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return probeBinaryStageHeaderDTO{}, false
	}
	return parsed, true
}

func canonicalStageHeaderKeyLiteral(body []byte, before, after int64, key string) bool {
	if before < 0 || after < before || after > int64(len(body)) {
		return false
	}
	raw := bytes.TrimSpace(body[before:after])
	// Token consumes the separator before every key after the first one.
	// InputOffset before that Token still points before the comma.
	if len(raw) > 0 && raw[0] == ',' {
		raw = bytes.TrimSpace(raw[1:])
	}
	if len(raw) != len(key)+2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	return string(raw[1:len(raw)-1]) == key
}

func stageHeaderBit(key string) (uint16, bool) {
	switch key {
	case "operation_id":
		return stageBitOperationID, true
	case "owner_id":
		return stageBitOwnerID, true
	case "attempt_id":
		return stageBitAttemptID, true
	case "ssh_address":
		return stageBitSSHAddress, true
	case "ssh_user":
		return stageBitSSHUser, true
	case "host_key_fingerprint":
		return stageBitHostKey, true
	case "client_key_fingerprint":
		return stageBitClientKey, true
	case "requested_by":
		return stageBitRequestedBy, true
	case "layout_version":
		return stageBitLayout, true
	case "mode":
		return stageBitMode, true
	case "version":
		return stageBitVersion, true
	case "architecture":
		return stageBitArchitecture, true
	case "sha256":
		return stageBitSHA256, true
	case "size":
		return stageBitSize, true
	default:
		return 0, false
	}
}

func applyStageHeaderToken(dst *probeBinaryStageHeaderDTO, key string, tok json.Token) bool {
	switch key {
	case "operation_id":
		return takeStageString(&dst.OperationID, tok)
	case "owner_id":
		return takeStageString(&dst.OwnerID, tok)
	case "attempt_id":
		return takeStageString(&dst.AttemptID, tok)
	case "ssh_address":
		return takeStageString(&dst.SSHAddress, tok)
	case "ssh_user":
		return takeStageString(&dst.SSHUser, tok)
	case "host_key_fingerprint":
		return takeStageString(&dst.HostKeyFingerprint, tok)
	case "client_key_fingerprint":
		return takeStageString(&dst.ClientKeyFingerprint, tok)
	case "mode":
		return takeStageString(&dst.Mode, tok)
	case "version":
		return takeStageString(&dst.Version, tok)
	case "architecture":
		return takeStageString(&dst.Architecture, tok)
	case "sha256":
		return takeStageString(&dst.SHA256, tok)
	case "requested_by":
		return takeStageInt64(&dst.RequestedBy, tok, 64)
	case "size":
		return takeStageInt64(&dst.Size, tok, 64)
	case "layout_version":
		var n int64
		if !takeStageInt64(&n, tok, strconv.IntSize) {
			return false
		}
		dst.LayoutVersion = int(n)
		return true
	default:
		return false
	}
}

func takeStageString(dst *string, tok json.Token) bool {
	s, ok := tok.(string)
	if !ok {
		return false
	}
	*dst = s
	return true
}

func takeStageInt64(dst *int64, tok json.Token, bits int) bool {
	number, ok := tok.(json.Number)
	if !ok || !canonicalStageInteger(number.String()) {
		return false
	}
	n, err := strconv.ParseInt(number.String(), 10, bits)
	if err != nil {
		return false
	}
	*dst = n
	return true
}

func canonicalStageInteger(s string) bool {
	if s == "" || s[0] == '+' {
		return false
	}
	i := 0
	if s[0] == '-' {
		if s == "-0" {
			return false
		}
		i = 1
	}
	if i >= len(s) || s[i] < '0' || s[i] > '9' || (s[i] == '0' && i != len(s)-1) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
