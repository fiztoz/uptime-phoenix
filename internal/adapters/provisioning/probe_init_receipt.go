package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	probeInitReceiptState = "probe_initialized"
	probeEnrollmentPrefix = "phx_probe_enroll_"
	// MaxProbeInitReceiptSize bounds the probe_init receipt line.
	MaxProbeInitReceiptSize = 2048
)

// ProbeCommandRunner runs one fixed absolute executable with fixed arguments,
// an empty environment and bounded output, returning stdout and the exit code.
// A nonzero exit is not an error.
type ProbeCommandRunner interface {
	Run(ctx context.Context, path string, args ...string) (stdout []byte, exitCode int, err error)
}

// ProbeInitResult is what probe_init reports: public facts plus the one-time
// enrollment token. The token must only ever be passed to enrollment.
type ProbeInitResult struct {
	Facts           domain.ProbeActivationFacts
	EnrollmentToken string
}

// Valid checks the facts and the token's structural shape. The hub validates
// the token again before using it.
func (r ProbeInitResult) Valid() bool {
	return r.Facts.Valid() && validProbeEnrollmentToken(r.EnrollmentToken)
}

// probeInitReceiptDTO is the canonical receipt in its fixed member order.
type probeInitReceiptDTO struct {
	State                  string `json:"state"`
	Kind                   string `json:"kind"`
	MarkerSHA256           string `json:"marker_sha256"`
	ProbeID                string `json:"probe_id"`
	StreamID               string `json:"stream_id"`
	CertificateFingerprint string `json:"certificate_fingerprint"`
	EnrollmentToken        string `json:"enrollment_token"`
}

// EncodeProbeInitReceipt renders the one canonical probe_init receipt line.
func EncodeProbeInitReceipt(intent domain.ProbeHostActionIntent, result ProbeInitResult) []byte {
	if intent.Kind != domain.ProbeActivationProbeInit || !result.Valid() {
		return nil
	}
	marker := intent.MarkerSHA256()
	if marker == "" {
		return nil
	}
	body, err := json.Marshal(probeInitReceiptDTO{
		State: probeInitReceiptState, Kind: intent.Kind, MarkerSHA256: marker,
		ProbeID: result.Facts.ProbeID, StreamID: result.Facts.StreamID,
		CertificateFingerprint: result.Facts.CertificateFingerprint, EnrollmentToken: result.EnrollmentToken,
	})
	if err != nil || len(body)+1 > MaxProbeInitReceiptSize {
		return nil
	}
	return append(body, '\n')
}

// ParseProbeInitReceipt accepts only the exact canonical line for this intent:
// it decodes strictly, then requires a byte-identical canonical re-encoding,
// so no alternative spelling, order, escape or whitespace is accepted.
func ParseProbeInitReceipt(line []byte, intent domain.ProbeHostActionIntent) (ProbeInitResult, bool) {
	var zero ProbeInitResult
	if intent.Kind != domain.ProbeActivationProbeInit || len(line) == 0 || len(line) > MaxProbeInitReceiptSize || line[len(line)-1] != '\n' {
		return zero, false
	}
	var dto probeInitReceiptDTO
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&dto) != nil || dto.State != probeInitReceiptState {
		return zero, false
	}
	result := ProbeInitResult{
		Facts:           domain.ProbeActivationFacts{ProbeID: dto.ProbeID, StreamID: dto.StreamID, CertificateFingerprint: dto.CertificateFingerprint},
		EnrollmentToken: dto.EnrollmentToken,
	}
	want := EncodeProbeInitReceipt(intent, result)
	if want == nil || !bytes.Equal(want, line) {
		return zero, false
	}
	return result, true
}

// parseProbeInitOutput reads the JSON `probe init` prints. Unknown members
// are refused; only the documented ones are accepted.
func parseProbeInitOutput(stdout []byte) (ProbeInitResult, bool) {
	var zero ProbeInitResult
	var out struct {
		ProbeID         string           `json:"probe_id"`
		StreamID        string           `json:"stream_id"`
		Fingerprint     string           `json:"certificate_fingerprint"`
		HubID           string           `json:"hub_id"`
		Sequence        json.RawMessage  `json:"last_created_seq"`
		Revision        json.RawMessage  `json:"config_revision"`
		EnrollmentToken string           `json:"enrollment_token"`
		ExpiresAt       *json.RawMessage `json:"expires_at"`
	}
	trimmed := bytes.TrimSuffix(stdout, []byte("\n"))
	if len(trimmed) == 0 || bytes.Contains(trimmed, []byte("\n")) {
		return zero, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || out.HubID != "" {
		return zero, false
	}
	// Exactly one object: any trailing byte, even a stray closing brace that
	// Decoder.More would not report, is refused.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return zero, false
	}
	result := ProbeInitResult{
		Facts:           domain.ProbeActivationFacts{ProbeID: out.ProbeID, StreamID: out.StreamID, CertificateFingerprint: out.Fingerprint},
		EnrollmentToken: out.EnrollmentToken,
	}
	if !result.Valid() {
		return zero, false
	}
	return result, true
}

func validProbeEnrollmentToken(token string) bool {
	if len(token) > 256 || !strings.HasPrefix(token, probeEnrollmentPrefix) || len(token)-len(probeEnrollmentPrefix) < 43 {
		return false
	}
	for _, c := range token[len(probeEnrollmentPrefix):] {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
