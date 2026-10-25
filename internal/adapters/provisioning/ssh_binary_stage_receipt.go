package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MaxSSHBinaryStageReceiptSize bounds the complete helper receipt, including its
// single terminal LF. A transport that reads a longer channel fails the attempt
// instead of truncating it; nothing here accepts a partial or padded object
// beyond this bound.
const MaxSSHBinaryStageReceiptSize = 2048

const (
	sshBinaryStageReceiptKeyCount = 9

	sshBinaryStageReceiptState         = "local_binary_staged"
	sshBinaryStageReceiptVersionSource = "operator_pin"
	sshBinaryStageReceiptOS            = "linux"
	sshBinaryStageReceiptUser          = "root"
)

const (
	stageReceiptBitState uint16 = 1 << iota
	stageReceiptBitDirectoryName
	stageReceiptBitMarkerSHA256
	stageReceiptBitPinnedVersion
	stageReceiptBitVersionSource
	stageReceiptBitOS
	stageReceiptBitArchitecture
	stageReceiptBitSHA256
	stageReceiptBitSize
	stageReceiptBitAll = (1 << sshBinaryStageReceiptKeyCount) - 1
)

// sshBinaryStageReceiptDTO is the flat wire object. It is decoded field by field
// from canonical key spellings, never unmarshalled directly: encoding/json would
// accept case aliases, escaped spellings and a last-duplicate winner.
type sshBinaryStageReceiptDTO struct {
	State         string
	DirectoryName string
	MarkerSHA256  string
	PinnedVersion string
	VersionSource string
	OS            string
	Architecture  string
	SHA256        string
	Size          int64
}

// parseSSHBinaryStageAck validates the single bounded receipt line that one
// helper invocation may return for exactly one staging attempt.
//
// It reports only the exact acknowledgment for intent or the transport's Unknown
// sentinel with a zero acknowledgment, including for a missing or finished
// context and for an intent this root-only delivery could never attempt. The
// returned value is a shape callers can forge; it is not a local seal, a
// filesystem observation, ownership, a committed reservation, a fence or an
// installed state.
func parseSSHBinaryStageAck(ctx context.Context, body []byte, intent domain.ProbeBinaryStageIntent) (SSHBinaryStageAck, error) {
	if !sshBinaryStageReceiptCallable(ctx, intent) {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
	}
	object, ok := sshBinaryStageReceiptObject(body)
	if !ok {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
	}
	parsed, ok := parseSSHBinaryStageReceipt(object)
	if !ok || !sshBinaryStageReceiptMatches(parsed, intent) {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
	}
	if ctx.Err() != nil {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
	}
	return SSHBinaryStageAck{Intent: intent}, nil
}

// sshBinaryStageReceiptCallable refuses a typed-nil or finished context and any
// intent the root-only delivery contract could never attempt: one complete
// systemd artifact for the SSH user root. Byte validation happens only
// afterwards, so nothing is interpreted without a live, authorized attempt.
func sshBinaryStageReceiptCallable(ctx context.Context, intent domain.ProbeBinaryStageIntent) bool {
	if imageSnapshotNil(ctx) || ctx.Err() != nil {
		return false
	}
	return intent.Valid() && intent.Identity.Host.User == sshBinaryStageReceiptUser
}

// sshBinaryStageReceiptObject enforces the line shape: bounded valid UTF-8, one
// flat object whose braces are the first and last bytes, one LF immediately
// after that object, and no internal LF/CR that could make a multi-line or
// concatenated receipt look like one object. Whitespace between members may
// vary; whitespace outside the braces may not.
func sshBinaryStageReceiptObject(body []byte) ([]byte, bool) {
	if len(body) == 0 || len(body) > MaxSSHBinaryStageReceiptSize || !utf8.Valid(body) {
		return nil, false
	}
	if body[len(body)-1] != '\n' {
		return nil, false
	}
	object := body[:len(body)-1]
	if len(object) < 2 || object[0] != '{' || object[len(object)-1] != '}' {
		return nil, false
	}
	if bytes.IndexByte(object, '\n') >= 0 || bytes.IndexByte(object, '\r') >= 0 {
		return nil, false
	}
	return object, true
}

// parseSSHBinaryStageReceipt requires one flat object with exactly the nine
// canonical key spellings. Key order and JSON whitespace may vary; duplicate,
// escaped, case-aliased, unknown, null, nested, mistyped, missing and trailing
// members are refused. Nested values are refused at their first delimiter.
func parseSSHBinaryStageReceipt(object []byte) (sshBinaryStageReceiptDTO, bool) {
	dec := json.NewDecoder(bytes.NewReader(object))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return sshBinaryStageReceiptDTO{}, false
	}
	var parsed sshBinaryStageReceiptDTO
	var seen uint16
	for dec.More() {
		before := dec.InputOffset()
		keyTok, err := dec.Token()
		after := dec.InputOffset()
		if err != nil {
			return sshBinaryStageReceiptDTO{}, false
		}
		key, ok := keyTok.(string)
		if !ok || !canonicalStageHeaderKeyLiteral(object, before, after, key) {
			return sshBinaryStageReceiptDTO{}, false
		}
		val, err := dec.Token()
		if err != nil {
			return sshBinaryStageReceiptDTO{}, false
		}
		bit, ok := stageReceiptBit(key)
		if !ok || seen&bit != 0 || !applySSHBinaryStageReceiptToken(&parsed, key, val) {
			return sshBinaryStageReceiptDTO{}, false
		}
		seen |= bit
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || seen != stageReceiptBitAll {
		return sshBinaryStageReceiptDTO{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return sshBinaryStageReceiptDTO{}, false
	}
	return parsed, true
}

func stageReceiptBit(key string) (uint16, bool) {
	switch key {
	case "state":
		return stageReceiptBitState, true
	case "directory_name":
		return stageReceiptBitDirectoryName, true
	case "marker_sha256":
		return stageReceiptBitMarkerSHA256, true
	case "pinned_version":
		return stageReceiptBitPinnedVersion, true
	case "version_source":
		return stageReceiptBitVersionSource, true
	case "os":
		return stageReceiptBitOS, true
	case "architecture":
		return stageReceiptBitArchitecture, true
	case "sha256":
		return stageReceiptBitSHA256, true
	case "size":
		return stageReceiptBitSize, true
	default:
		return 0, false
	}
}

func applySSHBinaryStageReceiptToken(dst *sshBinaryStageReceiptDTO, key string, tok json.Token) bool {
	switch key {
	case "state":
		return takeStageString(&dst.State, tok)
	case "directory_name":
		return takeStageString(&dst.DirectoryName, tok)
	case "marker_sha256":
		return takeStageString(&dst.MarkerSHA256, tok)
	case "pinned_version":
		return takeStageString(&dst.PinnedVersion, tok)
	case "version_source":
		return takeStageString(&dst.VersionSource, tok)
	case "os":
		return takeStageString(&dst.OS, tok)
	case "architecture":
		return takeStageString(&dst.Architecture, tok)
	case "sha256":
		return takeStageString(&dst.SHA256, tok)
	case "size":
		return takeStageInt64(&dst.Size, tok)
	default:
		return false
	}
}

// sshBinaryStageReceiptMatches binds every value to this intent. The relative
// directory name is derived from the intent, never from a caller path.
func sshBinaryStageReceiptMatches(receipt sshBinaryStageReceiptDTO, intent domain.ProbeBinaryStageIntent) bool {
	pin := intent.Identity.Binary
	return receipt.State == sshBinaryStageReceiptState &&
		receipt.DirectoryName == (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName() &&
		receipt.MarkerSHA256 == intent.MarkerSHA256() &&
		receipt.PinnedVersion == pin.Version &&
		receipt.VersionSource == sshBinaryStageReceiptVersionSource &&
		receipt.OS == sshBinaryStageReceiptOS &&
		receipt.Architecture == pin.Architecture &&
		receipt.SHA256 == pin.SHA256 &&
		receipt.Size == pin.Size
}
