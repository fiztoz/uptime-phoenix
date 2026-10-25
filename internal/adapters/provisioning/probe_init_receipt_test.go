package provisioning

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests cover the probe_init receipt codec, the strict reader of the
// real `probe init` stdout, and the activation host-action kinds' bindings.
// They are pure: no SSH, no filesystem, no real probe binary, and every token
// is generated for the test. Literals are independent of the production
// constants so a drifting constant cannot silently change what is accepted.

const (
	probeInitTestProbeID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	probeInitTestStreamID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	probeInitTestPrefix   = "phx_probe_enroll_"
	probeInitTestOtherUID = "55555555-5555-4555-8555-555555555555"
)

var (
	probeInitTestFingerprint = strings.Repeat("c0", 32)
	probeInitTestToken       = probeInitTestMakeToken(0)
	// probeInitTestNearPrefix is the real prefix with one letter dropped: a
	// look-alike that must be refused.
	probeInitTestNearPrefix = strings.Replace(probeInitTestPrefix, "ll", "l", 1)
)

// probeInitTestMakeToken builds a token shaped like the real one: the prefix
// plus base64url (no padding) of 32 bytes, which is exactly 43 characters. The
// first three bytes force both '-' and '_' into the alphabet under test.
func probeInitTestMakeToken(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i*37+11)
	}
	copy(raw, []byte{0xfb, 0xff, 0xbf})
	return probeInitTestPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func probeInitTestIntent() domain.ProbeHostActionIntent {
	return hostActFrameIntent(domain.ProbeActivationProbeInit)
}

func probeInitTestResult() ProbeInitResult {
	return ProbeInitResult{
		Facts:           domain.ProbeActivationFacts{ProbeID: probeInitTestProbeID, StreamID: probeInitTestStreamID, CertificateFingerprint: probeInitTestFingerprint},
		EnrollmentToken: probeInitTestToken,
	}
}

// probeInitTestMarker hashes the marker bytes independently of MarkerSHA256.
func probeInitTestMarker(intent domain.ProbeHostActionIntent) string {
	sum := sha256.Sum256([]byte(intent.Marker()))
	return hex.EncodeToString(sum[:])
}

// probeInitTestWire spells the canonical receipt by hand, member by member in
// the documented order, so it does not depend on the encoder under test.
func probeInitTestWire(marker, probeID, streamID, fingerprint, token string) string {
	return `{"state":"probe_initialized","kind":"probe_init","marker_sha256":"` + marker +
		`","probe_id":"` + probeID + `","stream_id":"` + streamID +
		`","certificate_fingerprint":"` + fingerprint + `","enrollment_token":"` + token + `"}` + "\n"
}

func probeInitTestCanonical(t *testing.T) (domain.ProbeHostActionIntent, ProbeInitResult, string) {
	t.Helper()
	intent, result := probeInitTestIntent(), probeInitTestResult()
	want := probeInitTestWire(probeInitTestMarker(intent), result.Facts.ProbeID, result.Facts.StreamID, result.Facts.CertificateFingerprint, result.EnrollmentToken)
	got := EncodeProbeInitReceipt(intent, result)
	if string(got) != want {
		t.Fatalf("encoder output differs from the documented canonical line:\n got %q\nwant %q", got, want)
	}
	return intent, result, want
}

func TestProbeInitReceiptConstants(t *testing.T) {
	if MaxProbeInitReceiptSize != 2048 {
		t.Fatalf("receipt bound drifted: %d", MaxProbeInitReceiptSize)
	}
	if probeInitReceiptState != "probe_initialized" || probeEnrollmentPrefix != probeInitTestPrefix {
		t.Fatalf("receipt state or token prefix drifted: %q %q", probeInitReceiptState, probeEnrollmentPrefix)
	}
	if domain.ProbeActivationProbeInit != "probe_init" || domain.ProbeActivationServiceStart != "service_start" {
		t.Fatal("activation host kinds drifted")
	}
	// The helper receipt rides on stdout beside the challenge line; both must
	// fit the sender's stdout bound for the largest legal token.
	challenge := EncodeProbeBinaryStageChallengeLine(strings.Repeat("0", 64), authSendNonce())
	biggest := probeInitTestResult()
	biggest.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 256-len(probeInitTestPrefix))
	line := EncodeProbeInitReceipt(probeInitTestIntent(), biggest)
	if line == nil || len(challenge)+len(line) > stageSendStdoutMax {
		t.Fatalf("largest legal receipt (%d bytes) plus challenge (%d) exceeds the sender's stdout bound %d", len(line), len(challenge), stageSendStdoutMax)
	}
}

func TestProbeInitReceiptRoundTripIsTheExactCanonicalLine(t *testing.T) {
	intent, result, canonical := probeInitTestCanonical(t)
	line := EncodeProbeInitReceipt(intent, result)

	if !strings.HasSuffix(canonical, "}\n") || strings.Count(canonical, "\n") != 1 || len(line) >= MaxProbeInitReceiptSize {
		t.Fatalf("not one LF-terminated bounded line: %q", line)
	}
	got, ok := ParseProbeInitReceipt(line, intent)
	if !ok || got != result {
		t.Fatalf("round trip = %+v %v, want %+v", got, ok, result)
	}
	// The encoder is deterministic.
	if again := EncodeProbeInitReceipt(intent, result); !bytes.Equal(again, line) {
		t.Fatal("encoding is not deterministic")
	}
	// The marker is the hash of the host-action marker bytes, which bind the
	// kind and the data-directory reference.
	marker := intent.Marker()
	if !strings.Contains(marker, "kind=probe_init\n") || !strings.Contains(marker, "reference=/var/lib/uptime-phoenix-probe-"+hostActFrameOperationID+"\n") {
		t.Fatalf("marker does not bind kind and reference:\n%s", marker)
	}
	// The token appears exactly once: in the receipt's last member.
	if strings.Count(canonical, probeInitTestToken) != 1 || !strings.Contains(canonical, `"enrollment_token":"`+probeInitTestToken+`"}`+"\n") {
		t.Fatal("token is not exactly the last member")
	}
}

func TestProbeInitReceiptIsBoundToTheWholeIntent(t *testing.T) {
	intent, result, _ := probeInitTestCanonical(t)
	line := EncodeProbeInitReceipt(intent, result)

	mutations := map[string]func(*domain.ProbeHostActionIntent){
		"operation":      func(i *domain.ProbeHostActionIntent) { i.Identity.OperationID = hostActFrameOtherOperation },
		"owner":          func(i *domain.ProbeHostActionIntent) { i.OwnerID = probeInitTestOtherUID },
		"attempt":        func(i *domain.ProbeHostActionIntent) { i.AttemptID = probeInitTestOtherUID },
		"ssh address":    func(i *domain.ProbeHostActionIntent) { i.Identity.Host.Address = "other.test:22" },
		"host key":       func(i *domain.ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = hostActFrameFingerprint(9) },
		"client key":     func(i *domain.ProbeHostActionIntent) { i.Identity.ClientKeyFingerprint = hostActFrameFingerprint(9) },
		"requested by":   func(i *domain.ProbeHostActionIntent) { i.Identity.RequestedBy = 8 },
		"binary version": func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Version = "1.2.4" },
		"binary sha256":  func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
		"binary size":    func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size++ },
	}
	for name, mutate := range mutations {
		other := intent
		mutate(&other)
		if !other.Valid() || other.MarkerSHA256() == intent.MarkerSHA256() {
			t.Fatalf("%s: not a distinct valid intent", name)
		}
		if got, ok := ParseProbeInitReceipt(line, other); ok || got != (ProbeInitResult{}) {
			t.Fatalf("%s: a receipt for another intent was accepted: %+v", name, got)
		}
	}

	// The marker differs from the one of every other host-action kind of the
	// same operation, so one receipt can never stand in for another kind.
	seen := map[string]string{}
	for _, kind := range []string{
		hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService,
		domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart,
	} {
		m := hostActFrameIntent(kind).MarkerSHA256()
		if m == "" {
			t.Fatalf("%s: no marker", kind)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("kinds %s and %s share a marker", prev, kind)
		}
		seen[m] = kind
	}
}

func TestEncodeProbeInitReceiptRefusals(t *testing.T) {
	intent, result := probeInitTestIntent(), probeInitTestResult()

	t.Run("non probe_init intents", func(t *testing.T) {
		for _, kind := range []string{
			hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService, domain.ProbeActivationServiceStart,
			hostActFrameKindArtifactDir, domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "", "Probe_Init", "probe_init ",
		} {
			if got := EncodeProbeInitReceipt(hostActFrameIntent(kind), result); got != nil {
				t.Fatalf("kind %q encoded: %q", kind, got)
			}
		}
	})

	t.Run("invalid intents with the probe_init kind", func(t *testing.T) {
		docker := intent
		docker.Identity.Mode = domain.ProbeInstallDocker
		deploy := intent
		deploy.Identity.Host.User = "deploy"
		noOwner := intent
		noOwner.OwnerID = ""
		badAttempt := intent
		badAttempt.AttemptID = "not-an-id"
		badPin := intent
		badPin.Identity.Binary.Size = 0
		for name, bad := range map[string]domain.ProbeHostActionIntent{
			"zero": {}, "docker mode": docker, "non-root": deploy, "empty owner": noOwner, "bad attempt": badAttempt, "bad pin": badPin,
		} {
			if got := EncodeProbeInitReceipt(bad, result); got != nil {
				t.Fatalf("%s encoded: %q", name, got)
			}
		}
	})

	t.Run("invalid results", func(t *testing.T) {
		with := func(mutate func(*ProbeInitResult)) ProbeInitResult {
			r := result
			mutate(&r)
			return r
		}
		for name, bad := range map[string]ProbeInitResult{
			"zero":                 {},
			"empty probe id":       with(func(r *ProbeInitResult) { r.Facts.ProbeID = "" }),
			"local probe id":       with(func(r *ProbeInitResult) { r.Facts.ProbeID = domain.LocalProbeID }),
			"uppercase probe id":   with(func(r *ProbeInitResult) { r.Facts.ProbeID = strings.ToUpper(probeInitTestProbeID) }),
			"nil uuid probe id":    with(func(r *ProbeInitResult) { r.Facts.ProbeID = "00000000-0000-0000-0000-000000000000" }),
			"short probe id":       with(func(r *ProbeInitResult) { r.Facts.ProbeID = probeInitTestProbeID[:35] }),
			"empty stream id":      with(func(r *ProbeInitResult) { r.Facts.StreamID = "" }),
			"non-uuid stream id":   with(func(r *ProbeInitResult) { r.Facts.StreamID = "stream-1" }),
			"empty fingerprint":    with(func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = "" }),
			"short fingerprint":    with(func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = strings.Repeat("c", 63) }),
			"long fingerprint":     with(func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = strings.Repeat("c", 65) }),
			"upper fingerprint":    with(func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = strings.Repeat("C", 64) }),
			"prefixed fingerprint": with(func(r *ProbeInitResult) { r.Facts.CertificateFingerprint = "sha256:" + strings.Repeat("c", 57) }),
			"missing token":        with(func(r *ProbeInitResult) { r.EnrollmentToken = "" }),
			"short token":          with(func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) }),
			"wrong prefix token":   with(func(r *ProbeInitResult) { r.EnrollmentToken = "phx_probe_other_" + strings.Repeat("A", 44) }),
			"bad character token":  with(func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) + "+" }),
			"oversize token": with(func(r *ProbeInitResult) {
				r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 256-len(probeInitTestPrefix)+1)
			}),
			"quote injection": with(func(r *ProbeInitResult) { r.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) + `"` }),
		} {
			if got := EncodeProbeInitReceipt(intent, bad); got != nil {
				t.Fatalf("%s encoded: %q", name, got)
			}
		}
	})
}

func TestParseProbeInitReceiptRefusesEveryNonCanonicalSpelling(t *testing.T) {
	intent, result, canonical := probeInitTestCanonical(t)
	marker := probeInitTestMarker(intent)
	body := strings.TrimSuffix(canonical, "\n")

	dto := probeInitReceiptDTO{
		State: "probe_initialized", Kind: "probe_init", MarkerSHA256: marker,
		ProbeID: result.Facts.ProbeID, StreamID: result.Facts.StreamID,
		CertificateFingerprint: result.Facts.CertificateFingerprint, EnrollmentToken: result.EnrollmentToken,
	}
	indented, err := json.MarshalIndent(dto, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	other := probeInitTestIntent()
	other.AttemptID = probeInitTestOtherUID

	// Dropping one member at a time.
	members := []string{
		`"state":"probe_initialized",`, `"kind":"probe_init",`, `"marker_sha256":"` + marker + `",`, `"probe_id":"` + result.Facts.ProbeID + `",`,
		`"stream_id":"` + result.Facts.StreamID + `",`, `"certificate_fingerprint":"` + result.Facts.CertificateFingerprint + `",`,
	}
	missing := map[string]string{
		"missing enrollment_token": strings.Replace(body, `,"enrollment_token":"`+result.EnrollmentToken+`"`, "", 1) + "\n",
	}
	for _, m := range members {
		missing["missing "+strings.SplitN(m, `"`, 3)[1]] = strings.Replace(canonical, m, "", 1)
	}

	cases := map[string]string{
		"empty":                      "",
		"only LF":                    "\n",
		"leading space":              " " + canonical,
		"leading LF":                 "\n" + canonical,
		"UTF-8 BOM":                  "\xef\xbb\xbf" + canonical,
		"space after colon":          strings.Replace(canonical, `"state":"`, `"state": "`, 1),
		"space after comma":          strings.Replace(canonical, `","kind"`, `", "kind"`, 1),
		"space before closing":       body[:len(body)-1] + " }\n",
		"space before LF":            body + " \n",
		"tab before LF":              body + "\t\n",
		"NUL before LF":              body + "\x00\n",
		"indented JSON":              string(indented) + "\n",
		"CRLF":                       body + "\r\n",
		"missing LF":                 body,
		"double LF":                  canonical + "\n",
		"two receipts":               canonical + canonical,
		"trailing byte after LF":     canonical + "x",
		"trailing second line":       canonical + probeInitTestWire(marker, result.Facts.ProbeID, result.Facts.StreamID, result.Facts.CertificateFingerprint, probeInitTestMakeToken(9)),
		"kind before state":          `{"kind":"probe_init","state":"probe_initialized","marker_sha256":"` + marker + `","probe_id":"` + result.Facts.ProbeID + `","stream_id":"` + result.Facts.StreamID + `","certificate_fingerprint":"` + result.Facts.CertificateFingerprint + `","enrollment_token":"` + result.EnrollmentToken + `"}` + "\n",
		"token before probe_id":      `{"state":"probe_initialized","kind":"probe_init","marker_sha256":"` + marker + `","enrollment_token":"` + result.EnrollmentToken + `","probe_id":"` + result.Facts.ProbeID + `","stream_id":"` + result.Facts.StreamID + `","certificate_fingerprint":"` + result.Facts.CertificateFingerprint + `"}` + "\n",
		"stream before probe":        `{"state":"probe_initialized","kind":"probe_init","marker_sha256":"` + marker + `","stream_id":"` + result.Facts.StreamID + `","probe_id":"` + result.Facts.ProbeID + `","certificate_fingerprint":"` + result.Facts.CertificateFingerprint + `","enrollment_token":"` + result.EnrollmentToken + `"}` + "\n",
		"escaped state":              strings.Replace(canonical, "probe_initialized", `probe_\u0069nitialized`, 1),
		"escaped key":                strings.Replace(canonical, `"state"`, `"stat\u0065"`, 1),
		"escaped hyphen in probe_id": strings.Replace(canonical, "-", `\u002d`, 1),
		"escaped token underscore":   strings.Replace(canonical, "enroll_", `enroll\u005f`, 1),
		"escaped first character":    strings.Replace(canonical, `{"state"`, `{"\u0073tate"`, 1),
		"key case variation":         strings.Replace(canonical, `"state"`, `"State"`, 1),
		"key case variation token":   strings.Replace(canonical, `"enrollment_token"`, `"Enrollment_Token"`, 1),
		"extra member":               strings.Replace(canonical, `,"enrollment_token"`, `,"extra":"x","enrollment_token"`, 1),
		"extra trailing member":      body[:len(body)-1] + `,"extra":1}` + "\n",
		"hub_id member":              body[:len(body)-1] + `,"hub_id":""}` + "\n",
		"expires_at member":          body[:len(body)-1] + `,"expires_at":"2026-01-01T00:00:00Z"}` + "\n",
		"duplicate probe_id":         body[:len(body)-1] + `,"probe_id":"` + result.Facts.ProbeID + `"}` + "\n",
		"duplicate token":            body[:len(body)-1] + `,"enrollment_token":"` + result.EnrollmentToken + `"}` + "\n",
		"duplicate with new token":   body[:len(body)-1] + `,"enrollment_token":"` + probeInitTestMakeToken(9) + `"}` + "\n",
		"wrong state applied":        strings.Replace(canonical, "probe_initialized", "host_action_applied", 1),
		"wrong state staged":         strings.Replace(canonical, "probe_initialized", "local_binary_staged", 1),
		"state trailing space":       strings.Replace(canonical, "probe_initialized", "probe_initialized ", 1),
		"state uppercase":            strings.Replace(canonical, "probe_initialized", "PROBE_INITIALIZED", 1),
		"empty state":                strings.Replace(canonical, "probe_initialized", "", 1),
		"wrong kind service_start":   strings.Replace(canonical, `"kind":"probe_init"`, `"kind":"service_start"`, 1),
		"wrong kind data":            strings.Replace(canonical, `"kind":"probe_init"`, `"kind":"data_directory"`, 1),
		"kind trailing space":        strings.Replace(canonical, `"kind":"probe_init"`, `"kind":"probe_init "`, 1),
		"kind uppercase":             strings.Replace(canonical, `"kind":"probe_init"`, `"kind":"PROBE_INIT"`, 1),
		"empty kind":                 strings.Replace(canonical, `"kind":"probe_init"`, `"kind":""`, 1),
		"wrong marker other hex":     strings.Replace(canonical, marker, strings.Repeat("ab", 32), 1),
		"marker of other attempt":    strings.Replace(canonical, marker, probeInitTestMarker(other), 1),
		"marker of data kind":        strings.Replace(canonical, marker, probeInitTestMarker(hostActFrameIntent(hostActFrameKindData)), 1),
		"marker of service_start":    strings.Replace(canonical, marker, probeInitTestMarker(hostActFrameIntent(domain.ProbeActivationServiceStart)), 1),
		"uppercase marker":           strings.Replace(canonical, marker, strings.ToUpper(marker), 1),
		"short marker":               strings.Replace(canonical, marker, marker[:63], 1),
		"empty marker":               strings.Replace(canonical, marker, "", 1),
		"null probe_id":              strings.Replace(canonical, `"probe_id":"`+result.Facts.ProbeID+`"`, `"probe_id":null`, 1),
		"numeric probe_id":           strings.Replace(canonical, `"probe_id":"`+result.Facts.ProbeID+`"`, `"probe_id":1`, 1),
		"local probe_id":             strings.Replace(canonical, result.Facts.ProbeID, "local", 1),
		"uppercase probe_id":         strings.Replace(canonical, result.Facts.ProbeID, strings.ToUpper(result.Facts.ProbeID), 1),
		"nil uuid probe_id":          strings.Replace(canonical, result.Facts.ProbeID, "00000000-0000-0000-0000-000000000000", 1),
		"empty stream_id":            strings.Replace(canonical, result.Facts.StreamID, "", 1),
		"short fingerprint":          strings.Replace(canonical, result.Facts.CertificateFingerprint, result.Facts.CertificateFingerprint[:63], 1),
		"uppercase fingerprint":      strings.Replace(canonical, result.Facts.CertificateFingerprint, strings.ToUpper(result.Facts.CertificateFingerprint), 1),
		"prefixed fingerprint":       strings.Replace(canonical, result.Facts.CertificateFingerprint, "SHA256:"+result.Facts.CertificateFingerprint[:57], 1),
		"empty token":                strings.Replace(canonical, result.EnrollmentToken, "", 1),
		"short token":                strings.Replace(canonical, result.EnrollmentToken, result.EnrollmentToken[:len(result.EnrollmentToken)-1], 1),
		"wrong prefix token":         strings.Replace(canonical, probeInitTestPrefix, probeInitTestNearPrefix, 1),
		"bad character token":        strings.Replace(canonical, result.EnrollmentToken, result.EnrollmentToken[:len(result.EnrollmentToken)-1]+"+", 1),
		"padded token":               strings.Replace(canonical, result.EnrollmentToken, result.EnrollmentToken+"=", 1),
		"oversize token":             strings.Replace(canonical, result.EnrollmentToken, probeInitTestPrefix+strings.Repeat("A", 240), 1),
		"top-level null":             "null\n",
		"top-level array":            "[" + body + "]\n",
		"top-level string":           `"` + probeInitTestToken + `"` + "\n",
		"token only":                 probeInitTestToken + "\n",
		"truncated JSON":             body[:len(body)-1] + "\n",
		"oversize padded":            body + strings.Repeat(" ", MaxProbeInitReceiptSize-len(body)) + "\n",
		"oversize exact bound":       body + strings.Repeat(" ", MaxProbeInitReceiptSize-len(body)-1) + "\n",
		"oversize garbage":           strings.Repeat("x", MaxProbeInitReceiptSize+1) + "\n",
	}
	for name, line := range missing {
		cases[name] = line
	}
	// The padding cases are the size guard's own: exactly at and just past it.
	if got := len(cases["oversize exact bound"]); got != MaxProbeInitReceiptSize {
		t.Fatalf("exact-bound fixture is %d bytes", got)
	}
	if got := len(cases["oversize padded"]); got != MaxProbeInitReceiptSize+1 {
		t.Fatalf("over-bound fixture is %d bytes", got)
	}

	for name, line := range cases {
		if line == canonical {
			t.Fatalf("%s: fixture equals the canonical line", name)
		}
		got, ok := ParseProbeInitReceipt([]byte(line), intent)
		if ok || got != (ProbeInitResult{}) {
			t.Fatalf("%s: accepted %+v: %q", name, got, line)
		}
	}
	if got, ok := ParseProbeInitReceipt(nil, intent); ok || got != (ProbeInitResult{}) {
		t.Fatalf("nil line accepted: %+v", got)
	}
	// Sanity: the untouched canonical line is still accepted.
	if got, ok := ParseProbeInitReceipt([]byte(canonical), intent); !ok || got != result {
		t.Fatalf("canonical line refused: %+v %v", got, ok)
	}
}

func TestParseProbeInitReceiptRequiresTheProbeInitIntent(t *testing.T) {
	intent, result, _ := probeInitTestCanonical(t)
	line := EncodeProbeInitReceipt(intent, result)
	for _, kind := range []string{
		hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService, domain.ProbeActivationServiceStart,
		hostActFrameKindArtifactDir, "", "probe_init ",
	} {
		if got, ok := ParseProbeInitReceipt(line, hostActFrameIntent(kind)); ok || got != (ProbeInitResult{}) {
			t.Fatalf("kind %q accepted a probe_init receipt: %+v", kind, got)
		}
	}
	invalid := intent
	invalid.OwnerID = "x"
	if got, ok := ParseProbeInitReceipt(line, invalid); ok || got != (ProbeInitResult{}) {
		t.Fatalf("invalid intent accepted a receipt: %+v", got)
	}
	if got, ok := ParseProbeInitReceipt(line, domain.ProbeHostActionIntent{}); ok || got != (ProbeInitResult{}) {
		t.Fatalf("zero intent accepted a receipt: %+v", got)
	}
}

func TestParseProbeInitReceiptRefusesEveryProperPrefix(t *testing.T) {
	intent, _, canonical := probeInitTestCanonical(t)
	for n := 0; n < len(canonical); n++ {
		if got, ok := ParseProbeInitReceipt([]byte(canonical[:n]), intent); ok || got != (ProbeInitResult{}) {
			t.Fatalf("a %d byte prefix was accepted: %+v", n, got)
		}
	}
}

// Whatever a single-byte corruption produces, a receipt is accepted only if it
// is itself the exact canonical encoding of what it reports, and no corruption
// of the state, kind or marker members is ever accepted.
func TestParseProbeInitReceiptSingleByteMutationsStayCanonical(t *testing.T) {
	intent, original, canonical := probeInitTestCanonical(t)
	guarded := strings.Index(canonical, `"probe_id"`)
	if guarded < 0 {
		t.Fatal("no probe_id member")
	}
	for i := 0; i < len(canonical); i++ {
		for _, repl := range []byte{0x00, ' ', '"', '\\', '\n', '}', 'X', '0', 0xff} {
			if canonical[i] == repl {
				continue
			}
			mutated := []byte(canonical)
			mutated[i] = repl
			got, ok := ParseProbeInitReceipt(mutated, intent)
			if !ok {
				if got != (ProbeInitResult{}) {
					t.Fatalf("refusal at %d/%#x returned a partial result %+v", i, repl, got)
				}
				continue
			}
			if i < guarded {
				t.Fatalf("a corrupted state/kind/marker byte at %d/%#x was accepted", i, repl)
			}
			if got == original {
				t.Fatalf("mutation at %d/%#x yielded the original result from different bytes", i, repl)
			}
			if re := EncodeProbeInitReceipt(intent, got); !bytes.Equal(re, mutated) {
				t.Fatalf("accepted non-canonical bytes at %d/%#x: %q", i, repl, mutated)
			}
		}
	}
}

func TestParseProbeInitReceiptSizeBound(t *testing.T) {
	intent := probeInitTestIntent()
	result := probeInitTestResult()

	// The largest token the validator allows still fits and round trips.
	result.EnrollmentToken = probeInitTestPrefix + strings.Repeat("Z", 256-len(probeInitTestPrefix))
	line := EncodeProbeInitReceipt(intent, result)
	if line == nil || len(line) > MaxProbeInitReceiptSize {
		t.Fatalf("largest legal receipt is %d bytes", len(line))
	}
	if got, ok := ParseProbeInitReceipt(line, intent); !ok || got != result {
		t.Fatalf("largest legal receipt refused: %+v %v", got, ok)
	}
	// One more byte of token is not a legal token, so it cannot be encoded.
	result.EnrollmentToken += "Z"
	if got := EncodeProbeInitReceipt(intent, result); got != nil {
		t.Fatalf("257 byte token encoded: %q", got)
	}
}

func FuzzParseProbeInitReceipt(f *testing.F) {
	intent := probeInitTestIntent()
	result := probeInitTestResult()
	canonical := EncodeProbeInitReceipt(intent, result)
	f.Add(canonical)
	f.Add([]byte(strings.Replace(string(canonical), "}", " }", 1)))
	f.Add([]byte(strings.TrimSuffix(string(canonical), "\n")))
	f.Add(append(append([]byte(nil), canonical...), canonical...))
	f.Add([]byte(`{"state":"probe_initialized"}` + "\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, line []byte) {
		got, ok := ParseProbeInitReceipt(line, intent)
		if !ok {
			if got != (ProbeInitResult{}) {
				t.Fatalf("refusal returned %+v", got)
			}
			return
		}
		if !got.Valid() {
			t.Fatalf("accepted an invalid result %+v", got)
		}
		if re := EncodeProbeInitReceipt(intent, got); !bytes.Equal(re, line) {
			t.Fatalf("accepted bytes are not the canonical encoding: %q", line)
		}
	})
}

func TestProbeInitResultValid(t *testing.T) {
	if !probeInitTestResult().Valid() {
		t.Fatal("the test result is not valid")
	}
	if (ProbeInitResult{}).Valid() {
		t.Fatal("zero result valid")
	}
	noToken := probeInitTestResult()
	noToken.EnrollmentToken = ""
	noFacts := probeInitTestResult()
	noFacts.Facts = domain.ProbeActivationFacts{}
	if noToken.Valid() || noFacts.Valid() {
		t.Fatal("a result missing its token or its facts is valid")
	}
}

func TestProbeActivationFactsValid(t *testing.T) {
	base := probeInitTestResult().Facts
	if !base.Valid() {
		t.Fatal("base facts invalid")
	}
	for name, mutate := range map[string]func(*domain.ProbeActivationFacts){
		"empty":               func(f *domain.ProbeActivationFacts) { *f = domain.ProbeActivationFacts{} },
		"local probe id":      func(f *domain.ProbeActivationFacts) { f.ProbeID = "local" },
		"probe id with space": func(f *domain.ProbeActivationFacts) { f.ProbeID = base.ProbeID[:35] + " " },
		"uppercase probe id":  func(f *domain.ProbeActivationFacts) { f.ProbeID = strings.ToUpper(base.ProbeID) },
		"nil uuid probe id":   func(f *domain.ProbeActivationFacts) { f.ProbeID = "00000000-0000-0000-0000-000000000000" },
		"unhyphenated probe":  func(f *domain.ProbeActivationFacts) { f.ProbeID = strings.ReplaceAll(base.ProbeID, "-", "") },
		"empty stream id":     func(f *domain.ProbeActivationFacts) { f.StreamID = "" },
		"stream id is a slug": func(f *domain.ProbeActivationFacts) { f.StreamID = "local" },
		"nil uuid stream id":  func(f *domain.ProbeActivationFacts) { f.StreamID = "00000000-0000-0000-0000-000000000000" },
		"empty fingerprint":   func(f *domain.ProbeActivationFacts) { f.CertificateFingerprint = "" },
		"63 hex":              func(f *domain.ProbeActivationFacts) { f.CertificateFingerprint = base.CertificateFingerprint[:63] },
		"65 hex":              func(f *domain.ProbeActivationFacts) { f.CertificateFingerprint += "0" },
		"uppercase hex": func(f *domain.ProbeActivationFacts) {
			f.CertificateFingerprint = strings.ToUpper(base.CertificateFingerprint)
		},
		"non-hex fingerprint":  func(f *domain.ProbeActivationFacts) { f.CertificateFingerprint = strings.Repeat("g", 64) },
		"colon separated hash": func(f *domain.ProbeActivationFacts) { f.CertificateFingerprint = strings.Repeat("c0:", 21) + "c0c" },
	} {
		f := base
		mutate(&f)
		if f.Valid() {
			t.Fatalf("%s: facts accepted: %+v", name, f)
		}
	}
}

func TestValidProbeEnrollmentToken(t *testing.T) {
	alphabet := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	good := map[string]string{
		"real shape":       probeInitTestToken,
		"all alphabet":     probeInitTestPrefix + alphabet[:43],
		"hyphens only":     probeInitTestPrefix + strings.Repeat("-", 43),
		"underscores only": probeInitTestPrefix + strings.Repeat("_", 43),
		"digits only":      probeInitTestPrefix + strings.Repeat("7", 43),
		"exactly 256":      probeInitTestPrefix + strings.Repeat("a", 256-len(probeInitTestPrefix)),
		"prefix inside":    probeInitTestPrefix + probeInitTestPrefix + strings.Repeat("a", 26),
	}
	for name, token := range good {
		if !validProbeEnrollmentToken(token) {
			t.Fatalf("%s: refused %q", name, token)
		}
	}
	if len(good["exactly 256"]) != 256 {
		t.Fatalf("256 fixture is %d bytes", len(good["exactly 256"]))
	}
	bad := map[string]string{
		"empty":             "",
		"prefix only":       probeInitTestPrefix,
		"42 characters":     probeInitTestPrefix + strings.Repeat("A", 42),
		"257 bytes":         probeInitTestPrefix + strings.Repeat("a", 256-len(probeInitTestPrefix)+1),
		"no prefix":         strings.Repeat("A", 60),
		"uppercase prefix":  "PHX_PROBE_ENROLL_" + strings.Repeat("A", 43),
		"no final underbar": "phx_probe_enroll-" + strings.Repeat("A", 43),
		"near prefix":       probeInitTestNearPrefix + strings.Repeat("A", 44),
		"leading space":     " " + probeInitTestPrefix + strings.Repeat("A", 43),
		"trailing LF":       probeInitTestPrefix + strings.Repeat("A", 43) + "\n",
		"trailing space":    probeInitTestPrefix + strings.Repeat("A", 43) + " ",
		"embedded space":    probeInitTestPrefix + strings.Repeat("A", 21) + " " + strings.Repeat("A", 21),
		"padding":           probeInitTestPrefix + strings.Repeat("A", 43) + "=",
		"plus":              probeInitTestPrefix + strings.Repeat("A", 42) + "+",
		"slash":             probeInitTestPrefix + strings.Repeat("A", 42) + "/",
		"dot":               probeInitTestPrefix + strings.Repeat("A", 42) + ".",
		"quote":             probeInitTestPrefix + strings.Repeat("A", 42) + `"`,
		"backslash":         probeInitTestPrefix + strings.Repeat("A", 42) + `\`,
		"NUL":               probeInitTestPrefix + strings.Repeat("A", 42) + "\x00",
		"tab":               probeInitTestPrefix + strings.Repeat("A", 42) + "\t",
		"latin e acute":     probeInitTestPrefix + strings.Repeat("A", 42) + "é",
		"cyrillic a":        probeInitTestPrefix + strings.Repeat("A", 42) + "а",
		"zero width space":  probeInitTestPrefix + strings.Repeat("A", 42) + "\u200b",
		"fullwidth digit":   probeInitTestPrefix + strings.Repeat("A", 42) + "７",
		"invalid utf8":      probeInitTestPrefix + strings.Repeat("A", 42) + "\xff",
		"bad char early":    probeInitTestPrefix + "!" + strings.Repeat("A", 42),
	}
	for name, token := range bad {
		if validProbeEnrollmentToken(token) {
			t.Fatalf("%s: accepted %q", name, token)
		}
	}
}

// ---- parseProbeInitOutput ---------------------------------------------------

// probeInitRealOutput mirrors the DTO printed by cmd/probe (the member names,
// order, omitempty rules and the decimal-as-string counters) and encodes it
// with json.Encoder, which terminates the line with an LF just like the CLI.
type probeInitRealOutput struct {
	ProbeID         string     `json:"probe_id"`
	StreamID        string     `json:"stream_id"`
	Fingerprint     string     `json:"certificate_fingerprint"`
	HubID           string     `json:"hub_id,omitempty"`
	Sequence        string     `json:"last_created_seq"`
	Revision        string     `json:"config_revision"`
	EnrollmentToken string     `json:"enrollment_token,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
}

func probeInitRealBytes(t *testing.T, mutate func(*probeInitRealOutput)) []byte {
	t.Helper()
	expires := time.Date(2026, 7, 1, 12, 10, 0, 123456789, time.UTC)
	out := probeInitRealOutput{
		ProbeID: probeInitTestProbeID, StreamID: probeInitTestStreamID, Fingerprint: probeInitTestFingerprint,
		Sequence: "0", Revision: "0", EnrollmentToken: probeInitTestToken, ExpiresAt: &expires,
	}
	if mutate != nil {
		mutate(&out)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseProbeInitOutputAcceptsTheRealShape(t *testing.T) {
	want := probeInitTestResult()
	line := probeInitRealBytes(t, nil)
	if !bytes.HasSuffix(line, []byte("}\n")) || bytes.Count(line, []byte("\n")) != 1 || bytes.Contains(line, []byte(`"hub_id"`)) {
		t.Fatalf("fixture is not the real shape: %q", line)
	}

	accepted := map[string][]byte{
		"with trailing LF":      line,
		"without trailing LF":   bytes.TrimSuffix(line, []byte("\n")),
		"explicit empty hub_id": []byte(strings.Replace(string(line), `"last_created_seq"`, `"hub_id":"","last_created_seq"`, 1)),
		"no expires_at":         probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ExpiresAt = nil }),
		"null expires_at":       []byte(strings.TrimSuffix(string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ExpiresAt = nil })), "}\n") + `,"expires_at":null}` + "\n"),
		"nonzero counters":      probeInitRealBytes(t, func(o *probeInitRealOutput) { o.Sequence, o.Revision = "41", "7" }),
	}
	for name, stdout := range accepted {
		got, ok := parseProbeInitOutput(stdout)
		if !ok || got != want {
			t.Fatalf("%s: %+v %v for %q", name, got, ok, stdout)
		}
	}
}

func TestParseProbeInitOutputRefusals(t *testing.T) {
	line := string(probeInitRealBytes(t, nil))
	body := strings.TrimSuffix(line, "\n")
	other := probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = probeInitTestMakeToken(9) })

	insert := func(member string) string {
		return strings.Replace(line, `"last_created_seq"`, member+`,"last_created_seq"`, 1)
	}
	cases := map[string]string{
		"empty":                        "",
		"only LF":                      "\n",
		"only spaces":                  "   \n",
		"two lines":                    line + string(other),
		"line then blank line":         line + "\n",
		"blank line then line":         "\n" + line,
		"line then garbage line":       line + "garbage\n",
		"two objects on one line":      body + body + "\n",
		"object then second object LF": body + " " + body,
		"trailing text":                body + " x\n",
		"split over two lines":         strings.Replace(line, `,"stream_id"`, ",\n\"stream_id\"", 1),
		"unknown member":               insert(`"extra":"x"`),
		"unknown nested member":        insert(`"extra":{"a":1}`),
		"unknown private key":          insert(`"private_key":"-----BEGIN"`),
		"unknown tls key":              insert(`"tls_private_key":"x"`),
		"unknown token member":         insert(`"token":"` + probeInitTestToken + `"`),
		"unknown hub key hash":         insert(`"key_hash":"` + probeInitTestFingerprint + `"`),
		"hub_id bound":                 insert(`"hub_id":"` + probeInitTestOtherUID + `"`),
		"hub_id garbage":               insert(`"hub_id":"x"`),
		"missing token":                string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = "" })),
		"explicit empty token":         strings.Replace(body, `"enrollment_token":"`+probeInitTestToken+`"`, `"enrollment_token":""`, 1) + "\n",
		"null token":                   strings.Replace(body, `"enrollment_token":"`+probeInitTestToken+`"`, `"enrollment_token":null`, 1) + "\n",
		"short token":                  string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) })),
		"wrong prefix token":           string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = "tok_" + strings.Repeat("A", 60) })),
		"bad char token":               string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 42) + "!" })),
		"oversize token":               string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = probeInitTestPrefix + strings.Repeat("A", 240) })),
		"local probe id":               string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ProbeID = "local" })),
		"non-uuid probe id":            string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ProbeID = "probe-1" })),
		"empty probe id":               string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ProbeID = "" })),
		"uppercase probe id":           string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ProbeID = strings.ToUpper(o.ProbeID) })),
		"empty stream id":              string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.StreamID = "" })),
		"non-uuid stream id":           string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.StreamID = "stream" })),
		"empty fingerprint":            string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.Fingerprint = "" })),
		"short fingerprint":            string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.Fingerprint = o.Fingerprint[:63] })),
		"uppercase fingerprint":        string(probeInitRealBytes(t, func(o *probeInitRealOutput) { o.Fingerprint = strings.ToUpper(o.Fingerprint) })),
		"probe id wrong type":          strings.Replace(body, `"probe_id":"`+probeInitTestProbeID+`"`, `"probe_id":1`, 1) + "\n",
		"token wrong type":             strings.Replace(body, `"enrollment_token":"`+probeInitTestToken+`"`, `"enrollment_token":["x"]`, 1) + "\n",
		"top-level null":               "null\n",
		"top-level array":              "[" + body + "]\n",
		"top-level string":             `"` + probeInitTestToken + `"` + "\n",
		"empty object":                 "{}\n",
		"truncated":                    body[:len(body)-1] + "\n",
		"plain text token":             probeInitTestToken + "\n",
		"stderr style text":            "Enrollment token could not be issued; an enrolled probe cannot be rebound\n",
	}
	for name, stdout := range cases {
		if got, ok := parseProbeInitOutput([]byte(stdout)); ok || got != (ProbeInitResult{}) {
			t.Fatalf("%s: accepted %+v for %q", name, got, stdout)
		}
	}
	if got, ok := parseProbeInitOutput(nil); ok || got != (ProbeInitResult{}) {
		t.Fatalf("nil stdout accepted: %+v", got)
	}
	// Sanity: the untouched output is still accepted.
	if got, ok := parseProbeInitOutput([]byte(line)); !ok || got != probeInitTestResult() {
		t.Fatalf("real output refused: %+v %v", got, ok)
	}
}

// Exactly one object: stray closers after it, which json.Decoder.More does
// not report, are refused like any other trailing data.
func TestParseProbeInitOutputRefusesStrayClosingBracesAfterTheObject(t *testing.T) {
	line := string(probeInitRealBytes(t, nil))
	body := strings.TrimSuffix(line, "\n")
	for name, stdout := range map[string]string{
		"one stray closing brace":   body + "}\n",
		"one stray closing bracket": body + "]\n",
		"several stray closers":     body + "}]}\n",
	} {
		if got, ok := parseProbeInitOutput([]byte(stdout)); ok || got != (ProbeInitResult{}) {
			t.Fatalf("%s: accepted %+v", name, got)
		}
	}
}

// ---- host-action kinds of the activation layout ----------------------------

func TestActivationHostActionIntentsMatchTheActivationLayout(t *testing.T) {
	identity := hostActFrameIdentity()
	activation := domain.ProbeActivationIdentity{
		Install: identity, ProbeKey: "edge-1", Name: "Edge 1", ListenPort: 8443,
		ClientKeyFingerprint: identity.ClientKeyFingerprint, RequestedBy: 7,
	}
	steps := activation.Steps()
	if len(steps) != 4 || steps[0].Kind != "probe_init" || steps[2].Kind != "service_start" {
		t.Fatalf("activation layout %+v", steps)
	}

	initSpec, ok := hostActFrameIntent(domain.ProbeActivationProbeInit).Resource()
	if !ok || initSpec != steps[0] || initSpec.Ordinal != 1 || !initSpec.Owned ||
		initSpec.Reference != "/var/lib/uptime-phoenix-probe-"+hostActFrameOperationID {
		t.Fatalf("probe_init spec %+v %v, want %+v", initSpec, ok, steps[0])
	}
	startSpec, ok := hostActFrameIntent(domain.ProbeActivationServiceStart).Resource()
	if !ok || startSpec != steps[2] || startSpec.Ordinal != 3 || !startSpec.Owned ||
		startSpec.Reference != "uptime-phoenix-probe-"+hostActFrameOperationID+".service" {
		t.Fatalf("service_start spec %+v %v, want %+v", startSpec, ok, steps[2])
	}

	// The hub-only steps are not host actions.
	for _, kind := range []string{domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "", "probe-init", "start"} {
		intent := hostActFrameIntent(kind)
		if intent.Valid() {
			t.Fatalf("kind %q is a valid host action", kind)
		}
		if _, ok := intent.Resource(); ok {
			t.Fatalf("kind %q has a resource", kind)
		}
	}
}

// A grant is signed over a message that binds the kind through the marker, so
// a signature for any other host action (or for the binary stage) never
// verifies for probe_init or service_start, and the other way round.
func TestActivationGrantsAreBoundToTheirKind(t *testing.T) {
	authority := newAuthSendAuthority(t)
	nonce := authSendNonce()
	request := func(kind string) domain.ProbeHostActionAdmissionRequest {
		intent := hostActFrameIntent(kind)
		return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: authority.keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256(),
		}}
	}
	kinds := []string{
		hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService,
		domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart,
	}
	for _, signed := range kinds {
		grant := hostActSendSign(authority, request(signed))
		if !grant.ValidFor(authority.raw) {
			t.Fatalf("%s: genuine grant does not verify", signed)
		}
		for _, used := range kinds {
			if used == signed {
				continue
			}
			forged := domain.ProbeHostActionGrant{Request: request(used), Signature: grant.Signature}
			if forged.ValidFor(authority.raw) {
				t.Fatalf("a %s grant verified as a %s grant", signed, used)
			}
		}
	}
}
