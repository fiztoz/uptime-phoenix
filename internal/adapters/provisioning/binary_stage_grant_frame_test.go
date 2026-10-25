package provisioning

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The wire format is restated here independently of the code under test, so a
// change to the canonical bytes has to be made twice and cannot drift silently.
const (
	gfChallengePrefix = `{"state":"stage_challenge","authority_key_id":"`
	gfChallengeMiddle = `","nonce":"`
	gfChallengeSuffix = "\"}\n"
	gfHexDigits       = "0123456789abcdef"
	gfGrantMagic      = "PHXGRT1\n"
	// gfBackslash lets the fixtures spell JSON escapes without a literal
	// backslash-u sequence in this source file.
	gfBackslash = "\x5c"
)

// gfAuthority derives a deterministic generated Ed25519 authority from a
// one-byte seed pattern. No key file, environment or shared secret is read.
func gfAuthority(seed byte) (ed25519.PrivateKey, [32]byte) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	var public [32]byte
	copy(public[:], key[ed25519.SeedSize:])
	return key, public
}

// gfNonce returns a nonzero nonce whose bytes are start, start+1, ...
func gfNonce(start byte) [32]byte {
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = start + byte(i)
	}
	return nonce
}

func gfFingerprint(b byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// gfIntent is a valid systemd intent for the root user, which the admission
// request requires.
func gfIntent() domain.ProbeBinaryStageIntent {
	return domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: "example.test:22", User: "root"},
			HostKeyFingerprint:   gfFingerprint(0),
			ClientKeyFingerprint: gfFingerprint(0),
			RequestedBy:          1,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version:      "1.2.3",
				Architecture: "amd64",
				SHA256:       strings.Repeat("a", 64),
				Size:         17,
			},
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
}

// gfRequest builds the admission request exactly as the hub does: the helper's
// authority key id and nonce plus the marker digest of the intent.
func gfRequest(t *testing.T, intent domain.ProbeBinaryStageIntent, public [32]byte, nonce [32]byte) domain.ProbeBinaryStageAdmissionRequest {
	t.Helper()
	request := domain.ProbeBinaryStageAdmissionRequest{
		Intent: intent,
		Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(public),
			Nonce:          nonce,
			MarkerSHA256:   intent.MarkerSHA256(),
		},
	}
	if !request.Valid() {
		t.Fatal("fixture admission request is invalid")
	}
	return request
}

// gfSignedGrant signs request.SigningMessage() with a real Ed25519 key.
func gfSignedGrant(t *testing.T, key ed25519.PrivateKey, request domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageGrant {
	t.Helper()
	message := request.SigningMessage()
	if message == nil {
		t.Fatal("fixture signing message is nil")
	}
	grant := domain.ProbeBinaryStageGrant{Request: request}
	copy(grant.Signature[:], ed25519.Sign(key, message))
	if grant.Signature == ([64]byte{}) {
		t.Fatal("fixture signature is zero")
	}
	return grant
}

func gfWantInputError(t *testing.T, err error) {
	t.Helper()
	if err != domain.ErrProbeBinaryStageWriteInput || !errors.Is(err, domain.ErrProbeBinaryStageWriteInput) {
		t.Fatalf("error = %v, want exactly domain.ErrProbeBinaryStageWriteInput", err)
	}
}

func gfChallengeLine(keyID, nonceHex string) []byte {
	return []byte(gfChallengePrefix + keyID + gfChallengeMiddle + nonceHex + gfChallengeSuffix)
}

func gfWantChallengeRefused(t *testing.T, line []byte) {
	t.Helper()
	keyID, nonce, err := ParseProbeBinaryStageChallengeLine(line)
	gfWantInputError(t, err)
	if keyID != "" || nonce != ([32]byte{}) {
		t.Fatalf("refused challenge line leaked key id %q / nonce %x", keyID, nonce)
	}
}

func gfWantKeyRefused(t *testing.T, body []byte) {
	t.Helper()
	key, err := ParseProbeBinaryStageAuthorityKey(body)
	gfWantInputError(t, err)
	if key != ([32]byte{}) {
		t.Fatalf("refused key file returned key bytes %x", key)
	}
}

func gfWantFrameRefused(t *testing.T, frame []byte) {
	t.Helper()
	signature, err := DecodeProbeBinaryStageGrantFrame(frame)
	gfWantInputError(t, err)
	if signature != ([64]byte{}) {
		t.Fatalf("refused frame returned signature bytes %x", signature)
	}
}

// gfNextHex returns a different lowercase hex digit, so the field stays
// well-formed while its value changes.
func gfNextHex(c byte) byte {
	return gfHexDigits[(strings.IndexByte(gfHexDigits, c)+1)%len(gfHexDigits)]
}

// gfUpperOneLetter upper-cases only the first a-f letter, leaving the rest of
// the lowercase digest intact.
func gfUpperOneLetter(t *testing.T, value string) string {
	t.Helper()
	for i := 0; i < len(value); i++ {
		if value[i] >= 'a' && value[i] <= 'f' {
			return value[:i] + strings.ToUpper(value[i:i+1]) + value[i+1:]
		}
	}
	t.Fatal("fixture hex has no letter to upper-case")
	return ""
}

func TestProbeBinaryStageChallengeLineRoundTrip(t *testing.T) {
	_, public := gfAuthority(1)
	keyID := domain.ProbeBinaryStageAuthorityKeyID(public)

	wantSize := len(gfChallengePrefix) + 64 + len(gfChallengeMiddle) + 64 + len(gfChallengeSuffix)
	gotSize := ProbeBinaryStageChallengeLineSize
	if gotSize != wantSize {
		t.Fatalf("challenge line size = %d, independently derived %d", gotSize, wantSize)
	}
	if wantSize != 189 {
		t.Fatalf("challenge line size = %d, want the pinned 189 bytes", wantSize)
	}

	var firstOnly, lastOnly, allOnes, letters [32]byte
	firstOnly[0] = 1
	lastOnly[31] = 1
	allOnes = [32]byte(bytes.Repeat([]byte{0xff}, 32))
	letters = [32]byte(bytes.Repeat([]byte{0xab}, 32))
	nonces := map[string][32]byte{
		"sequential": gfNonce(1),
		"first only": firstOnly,
		"last only":  lastOnly,
		"all ff":     allOnes,
		"letters":    letters,
	}
	for name, nonce := range nonces {
		t.Run(name, func(t *testing.T) {
			line := EncodeProbeBinaryStageChallengeLine(keyID, nonce)
			want := gfChallengeLine(keyID, hex.EncodeToString(nonce[:]))
			if !bytes.Equal(line, want) {
				t.Fatalf("encoded line differs from the independently built canonical bytes")
			}
			if len(line) != ProbeBinaryStageChallengeLineSize {
				t.Fatalf("line length = %d, want %d", len(line), ProbeBinaryStageChallengeLineSize)
			}
			if line[len(line)-1] != '\n' || bytes.Count(line, []byte{'\n'}) != 1 {
				t.Fatal("line must end in exactly one LF")
			}

			pristine := bytes.Clone(line)
			gotKey, gotNonce, err := ParseProbeBinaryStageChallengeLine(line)
			if err != nil || gotKey != keyID || gotNonce != nonce {
				t.Fatalf("round trip = (%q, %x, %v), want (%q, %x, nil)", gotKey, gotNonce, err, keyID, nonce)
			}
			if !bytes.Equal(line, pristine) {
				t.Fatal("parse modified its input")
			}

			// Encode returns a fresh slice: scribbling on one result cannot
			// corrupt the next.
			line[0] = 'X'
			if again := EncodeProbeBinaryStageChallengeLine(keyID, nonce); !bytes.Equal(again, want) {
				t.Fatal("encode result aliases shared state")
			}
		})
	}
}

func TestProbeBinaryStageChallengeLineEncodeRefusesInvalidInput(t *testing.T) {
	_, public := gfAuthority(1)
	keyID := domain.ProbeBinaryStageAuthorityKeyID(public)
	nonce := gfNonce(1)
	if got := EncodeProbeBinaryStageChallengeLine(keyID, nonce); got == nil {
		t.Fatal("control encode refused a valid key id")
	}

	cases := []struct {
		name  string
		keyID string
		nonce [32]byte
	}{
		{"empty key id", "", nonce},
		{"uppercase key id", strings.ToUpper(keyID), nonce},
		{"one uppercase letter", gfUpperOneLetter(t, keyID), nonce},
		{"short by one", keyID[:63], nonce},
		{"long by one", keyID + "a", nonce},
		{"long by LF", keyID + "\n", nonce},
		{"trailing space", keyID[:63] + " ", nonce},
		{"leading space", " " + keyID[1:], nonce},
		{"non-hex last", keyID[:63] + "g", nonce},
		{"non-hex first", "x" + keyID[1:], nonce},
		{"hex prefix", "0x" + keyID[2:], nonce},
		{"non-ASCII bytes", keyID[:62] + "é", nonce},
		{"quote injection", keyID[:62] + `"}`, nonce},
		{"zero nonce", keyID, [32]byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeProbeBinaryStageChallengeLine(tc.keyID, tc.nonce); got != nil {
				t.Fatalf("encode accepted invalid input and produced %d bytes", len(got))
			}
		})
	}
}

func TestProbeBinaryStageChallengeLineParseRefusesNonCanonical(t *testing.T) {
	_, public := gfAuthority(1)
	keyID := domain.ProbeBinaryStageAuthorityKeyID(public)
	nonceHex := strings.Repeat("ab", 32)
	if strings.ToUpper(keyID) == keyID {
		t.Fatal("fixture key id has no letters; pick another seed")
	}
	valid := gfChallengeLine(keyID, nonceHex)
	if len(valid) != ProbeBinaryStageChallengeLineSize {
		t.Fatalf("fixture line length = %d", len(valid))
	}
	if _, _, err := ParseProbeBinaryStageChallengeLine(valid); err != nil {
		t.Fatalf("control line refused: %v", err)
	}

	lastLF := len(valid) - 1
	shifted := gfChallengePrefix + keyID + "a" + gfChallengeMiddle + nonceHex[:63] + gfChallengeSuffix
	escaped := `{"` + gfBackslash + `u0073tate":"stage_challenge","authority_key_id":"` + keyID + gfChallengeMiddle + nonceHex[:59] + gfChallengeSuffix
	cases := []struct {
		name string
		line []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"zero nonce", gfChallengeLine(keyID, strings.Repeat("0", 64))},
		{"uppercase key id", gfChallengeLine(strings.ToUpper(keyID), nonceHex)},
		{"one uppercase letter in key id", gfChallengeLine(gfUpperOneLetter(t, keyID), nonceHex)},
		{"uppercase nonce", gfChallengeLine(keyID, strings.ToUpper(nonceHex))},
		{"one uppercase letter in nonce", gfChallengeLine(keyID, gfUpperOneLetter(t, nonceHex))},
		{"non-hex key id", gfChallengeLine(keyID[:63]+"g", nonceHex)},
		{"non-hex nonce", gfChallengeLine(keyID, nonceHex[:63]+"g")},
		{"short key id", gfChallengeLine(keyID[:63], nonceHex)},
		{"short nonce", gfChallengeLine(keyID, nonceHex[:63])},
		{"long key id", gfChallengeLine(keyID+"a", nonceHex)},
		{"long nonce", gfChallengeLine(keyID, nonceHex+"a")},
		{"field boundary shifted at equal total length", []byte(shifted)},
		{"swapped key order", []byte(`{"state":"stage_challenge","nonce":"` + nonceHex + `","authority_key_id":"` + keyID + gfChallengeSuffix)},
		{"missing LF", valid[:lastLF]},
		{"CRLF", append(bytes.Clone(valid[:lastLF]), '\r', '\n')},
		{"CR instead of LF", append(bytes.Clone(valid[:lastLF]), '\r')},
		{"extra trailing byte", append(bytes.Clone(valid), 'x')},
		{"extra trailing LF", append(bytes.Clone(valid), '\n')},
		{"extra trailing NUL", append(bytes.Clone(valid), 0)},
		{"truncated by one trailing byte", valid[:lastLF]},
		{"truncated by one leading byte", valid[1:]},
		{"truncated by one nonce digit keeping LF", gfChallengeLine(keyID, nonceHex[:63])},
		{"wrong state value at equal length", []byte(strings.Replace(string(valid), "stage_challenge", "stage_challengX", 1))},
		{"wrong state value capitalised", []byte(strings.Replace(string(valid), "stage_challenge", "Stage_challenge", 1))},
		{"stage grant state", []byte(strings.Replace(string(valid), "stage_challenge", "stage_grant____", 1))},
		{"escaped state key spelling", []byte(strings.Replace(string(valid), `"state"`, `"`+gfBackslash+`u0073tate"`, 1))},
		{"escaped state key at equal total length", []byte(escaped)},
		{"escaped authority key spelling", []byte(strings.Replace(string(valid), `authority_key_id`, `authority`+gfBackslash+`u005fkey_id`, 1))},
		{"escaped underscore in value", []byte(strings.Replace(string(valid), `"stage_challenge"`, `"stage`+gfBackslash+`u005fchallenge"`, 1))},
		{"space after opening brace", []byte(strings.Replace(string(valid), `{"state"`, `{ "state"`, 1))},
		{"space after colon", []byte(strings.Replace(string(valid), `"state":`, `"state": `, 1))},
		{"space after comma", []byte(strings.Replace(string(valid), `","nonce"`, `", "nonce"`, 1))},
		{"space before LF", append(bytes.Clone(valid[:lastLF]), ' ', '\n')},
		{"leading space", append([]byte{' '}, valid...)},
		{"leading LF", append([]byte{'\n'}, valid...)},
		{"two lines", append(bytes.Clone(valid), valid...)},
		{"other helper state line", []byte("{\"state\":\"stage_ready\"}\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gfWantChallengeRefused(t, tc.line)
		})
	}

	t.Run("one byte replaced by non-canonical byte", func(t *testing.T) {
		for _, replacement := range []byte{' ', '\t', '\r', '\n', 0, 0xff} {
			for i := range valid {
				if valid[i] == replacement {
					continue
				}
				line := bytes.Clone(valid)
				line[i] = replacement
				gfWantChallengeRefused(t, line)
			}
		}
	})
	t.Run("whitespace inserted at every position", func(t *testing.T) {
		for _, space := range []byte{' ', '\t', '\r', '\n'} {
			for i := 0; i <= len(valid); i++ {
				line := make([]byte, 0, len(valid)+1)
				line = append(line, valid[:i]...)
				line = append(line, space)
				line = append(line, valid[i:]...)
				gfWantChallengeRefused(t, line)
			}
		}
	})
	t.Run("one byte deleted at every position", func(t *testing.T) {
		for i := range valid {
			line := make([]byte, 0, len(valid)-1)
			line = append(line, valid[:i]...)
			line = append(line, valid[i+1:]...)
			gfWantChallengeRefused(t, line)
		}
	})
}

func TestProbeBinaryStageGrantFrameRoundTrip(t *testing.T) {
	key, public := gfAuthority(2)
	grant := gfSignedGrant(t, key, gfRequest(t, gfIntent(), public, gfNonce(7)))

	frameSize := ProbeBinaryStageGrantFrameSize
	if frameSize != 72 {
		t.Fatalf("grant frame size = %d, want the pinned 72 bytes", frameSize)
	}
	if want := len(gfGrantMagic) + ed25519.SignatureSize; frameSize != want {
		t.Fatalf("grant frame size = %d, want %d magic + signature bytes", frameSize, want)
	}
	frame, err := EncodeProbeBinaryStageGrantFrame(grant)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frame) != ProbeBinaryStageGrantFrameSize {
		t.Fatalf("frame length = %d, want %d", len(frame), ProbeBinaryStageGrantFrameSize)
	}
	if !bytes.HasPrefix(frame, []byte(gfGrantMagic)) || string(frame[:8]) != "PHXGRT1\n" {
		t.Fatalf("frame magic = %q, want %q", frame[:8], gfGrantMagic)
	}
	if !bytes.Equal(frame[8:], grant.Signature[:]) {
		t.Fatal("frame body is not the raw signature")
	}

	signature, err := DecodeProbeBinaryStageGrantFrame(frame)
	if err != nil || signature != grant.Signature {
		t.Fatalf("decode = (%x, %v), want the exact signature", signature, err)
	}
	// The frame carries no request fields: the helper rebuilds them.
	if !(domain.ProbeBinaryStageGrant{Request: grant.Request, Signature: signature}).ValidFor(public) {
		t.Fatal("decoded signature does not verify against the original request")
	}

	// Neither direction shares storage with its input or a previous result.
	frame[0] = 'X'
	if again, err := EncodeProbeBinaryStageGrantFrame(grant); err != nil || string(again[:8]) != gfGrantMagic {
		t.Fatal("encode result aliases shared state")
	}
	if signature != grant.Signature {
		t.Fatal("decoded signature aliases the frame")
	}
}

func TestProbeBinaryStageGrantFrameEncodeRefusesInvalidGrant(t *testing.T) {
	key, public := gfAuthority(2)
	valid := gfSignedGrant(t, key, gfRequest(t, gfIntent(), public, gfNonce(7)))
	other := gfIntent()
	other.AttemptID = "44444444-4444-4444-8444-444444444444"

	cases := []struct {
		name string
		// intentStillValid asserts the mutated intent is individually valid, so
		// the refusal is attributable to the admission rule under test alone.
		intentStillValid bool
		mutate           func(g *domain.ProbeBinaryStageGrant)
	}{
		{"non-root user with matching marker", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Intent.Identity.Host.User = "deploy"
			g.Request.Challenge.MarkerSHA256 = g.Request.Intent.MarkerSHA256()
		}},
		{"marker of a different intent", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Challenge.MarkerSHA256 = other.MarkerSHA256()
		}},
		{"intent changed after marker was bound", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Intent.AttemptID = other.AttemptID
		}},
		{"uppercase marker", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Challenge.MarkerSHA256 = strings.ToUpper(g.Request.Challenge.MarkerSHA256)
		}},
		{"empty marker", true, func(g *domain.ProbeBinaryStageGrant) { g.Request.Challenge.MarkerSHA256 = "" }},
		{"zero nonce", true, func(g *domain.ProbeBinaryStageGrant) { g.Request.Challenge.Nonce = [32]byte{} }},
		{"uppercase authority key id", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Challenge.AuthorityKeyID = strings.ToUpper(g.Request.Challenge.AuthorityKeyID)
		}},
		{"short authority key id", true, func(g *domain.ProbeBinaryStageGrant) {
			g.Request.Challenge.AuthorityKeyID = g.Request.Challenge.AuthorityKeyID[:63]
		}},
		{"empty authority key id", true, func(g *domain.ProbeBinaryStageGrant) { g.Request.Challenge.AuthorityKeyID = "" }},
		{"docker mode intent", false, func(g *domain.ProbeBinaryStageGrant) { g.Request.Intent.Identity.Mode = domain.ProbeInstallDocker }},
		{"empty owner id", false, func(g *domain.ProbeBinaryStageGrant) { g.Request.Intent.OwnerID = "" }},
		{"zero value intent", false, func(g *domain.ProbeBinaryStageGrant) { g.Request.Intent = domain.ProbeBinaryStageIntent{} }},
		{"zero signature", true, func(g *domain.ProbeBinaryStageGrant) { g.Signature = [64]byte{} }},
		{"zero value grant", false, func(g *domain.ProbeBinaryStageGrant) { *g = domain.ProbeBinaryStageGrant{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := valid
			tc.mutate(&grant)
			if tc.intentStillValid && !grant.Request.Intent.Valid() {
				t.Fatal("case is meant to isolate an admission rule but the intent itself is invalid")
			}
			frame, err := EncodeProbeBinaryStageGrantFrame(grant)
			gfWantInputError(t, err)
			if frame != nil {
				t.Fatalf("refused grant still produced %d frame bytes", len(frame))
			}
		})
	}
}

// Encode is structural only: the helper's own trusted key is the verification
// authority, so a well-formed request with an unverifiable signature frames.
func TestProbeBinaryStageGrantFrameEncodeDoesNotVerifySignature(t *testing.T) {
	key, public := gfAuthority(2)
	grant := gfSignedGrant(t, key, gfRequest(t, gfIntent(), public, gfNonce(7)))
	grant.Signature[63] ^= 0x01
	if grant.ValidFor(public) {
		t.Fatal("tampered signature unexpectedly verifies")
	}
	frame, err := EncodeProbeBinaryStageGrantFrame(grant)
	if err != nil || len(frame) != ProbeBinaryStageGrantFrameSize || !bytes.Equal(frame[8:], grant.Signature[:]) {
		t.Fatalf("structurally valid grant refused: %v", err)
	}
}

func TestProbeBinaryStageGrantFrameDecodeRefusesMalformed(t *testing.T) {
	key, public := gfAuthority(2)
	grant := gfSignedGrant(t, key, gfRequest(t, gfIntent(), public, gfNonce(7)))
	valid, err := EncodeProbeBinaryStageGrantFrame(grant)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if _, err := DecodeProbeBinaryStageGrantFrame(valid); err != nil {
		t.Fatalf("control frame refused: %v", err)
	}

	withMagic := func(magic string) []byte {
		return append([]byte(magic), grant.Signature[:]...)
	}
	cases := []struct {
		name  string
		frame []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"magic only", []byte(gfGrantMagic)},
		{"signature only", bytes.Clone(grant.Signature[:])},
		{"short by one", valid[:len(valid)-1]},
		{"short by one leading byte", valid[1:]},
		{"long by one NUL", append(bytes.Clone(valid), 0)},
		{"long by trailing LF", append(bytes.Clone(valid), '\n')},
		{"two frames", append(bytes.Clone(valid), valid...)},
		{"signature before magic", append(bytes.Clone(grant.Signature[:]), gfGrantMagic...)},
		{"challenge line as frame", gfChallengeLine(strings.Repeat("a", 64), strings.Repeat("b", 64))},
		{"wrong version magic", withMagic("PHXGRT2\n")},
		{"older version magic", withMagic("PHXGRT0\n")},
		{"lowercase magic", withMagic("phxgrt1\n")},
		{"magic without LF", withMagic("PHXGRT1 ")},
		{"magic with NUL terminator", withMagic("PHXGRT1\x00")},
		{"magic with CR terminator", withMagic("PHXGRT1\r")},
		{"CRLF magic with truncated signature", append([]byte("PHXGRT1\r\n"), grant.Signature[:63]...)},
		{"zero signature", append([]byte(gfGrantMagic), make([]byte, 64)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gfWantFrameRefused(t, tc.frame)
		})
	}

	t.Run("every magic byte is significant", func(t *testing.T) {
		for i := range len(gfGrantMagic) {
			frame := bytes.Clone(valid)
			frame[i] ^= 0x01
			gfWantFrameRefused(t, frame)
		}
	})
	t.Run("every truncation length", func(t *testing.T) {
		for n := range len(valid) {
			gfWantFrameRefused(t, valid[:n])
		}
	})
}

// Decode is structural only: it must hand back any nonzero 64 bytes untouched
// and leave verification to the helper's trusted key.
func TestProbeBinaryStageGrantFrameDecodeIsUnverified(t *testing.T) {
	_, public := gfAuthority(2)
	for _, index := range []int{0, 31, 63} {
		var want [64]byte
		want[index] = 1
		frame := append([]byte(gfGrantMagic), want[:]...)
		got, err := DecodeProbeBinaryStageGrantFrame(frame)
		if err != nil || got != want {
			t.Fatalf("index %d: decode = (%x, %v), want the exact bytes", index, got, err)
		}
		request := gfRequest(t, gfIntent(), public, gfNonce(7))
		if (domain.ProbeBinaryStageGrant{Request: request, Signature: got}).ValidFor(public) {
			t.Fatalf("index %d: arbitrary decoded bytes verified as a grant", index)
		}
	}
}

func TestProbeBinaryStageAuthorityKeyRoundTrip(t *testing.T) {
	_, generated := gfAuthority(3)
	var lastOnly, firstOnly [32]byte
	lastOnly[31] = 1
	firstOnly[0] = 1
	keys := map[string][32]byte{
		"generated":  generated,
		"sequential": gfNonce(0xf0),
		"all ff":     [32]byte(bytes.Repeat([]byte{0xff}, 32)),
		"first only": firstOnly,
		"last only":  lastOnly,
		"letters":    [32]byte(bytes.Repeat([]byte{0xcd}, 32)),
	}
	if fileSize := ProbeBinaryStageAuthorityKeyFileSize; fileSize != 65 {
		t.Fatalf("authority key file size = %d, want 64 hex + LF", fileSize)
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			body := EncodeProbeBinaryStageAuthorityKey(key)
			if len(body) != ProbeBinaryStageAuthorityKeyFileSize {
				t.Fatalf("encoded length = %d, want %d", len(body), ProbeBinaryStageAuthorityKeyFileSize)
			}
			if string(body) != hex.EncodeToString(key[:])+"\n" || bytes.Count(body, []byte{'\n'}) != 1 || body[64] != '\n' {
				t.Fatal("encoded key is not lowercase hex plus a single LF")
			}
			pristine := bytes.Clone(body)
			got, err := ParseProbeBinaryStageAuthorityKey(body)
			if err != nil || got != key {
				t.Fatalf("round trip = (%x, %v), want (%x, nil)", got, err, key)
			}
			if !bytes.Equal(body, pristine) {
				t.Fatal("parse modified its input")
			}
			body[0] = 'X'
			if again := EncodeProbeBinaryStageAuthorityKey(key); !bytes.Equal(again, pristine) {
				t.Fatal("encode result aliases shared state")
			}
		})
	}
}

func TestProbeBinaryStageAuthorityKeyEncodeRefusesZeroKey(t *testing.T) {
	if got := EncodeProbeBinaryStageAuthorityKey([32]byte{}); got != nil {
		t.Fatalf("zero key encoded to %q", got)
	}
}

func TestProbeBinaryStageAuthorityKeyParseRefusesNonCanonical(t *testing.T) {
	_, public := gfAuthority(3)
	keyHex := hex.EncodeToString(public[:])
	if strings.ToUpper(keyHex) == keyHex {
		t.Fatal("fixture key has no hex letters; pick another seed")
	}
	valid := keyHex + "\n"
	if _, err := ParseProbeBinaryStageAuthorityKey([]byte(valid)); err != nil {
		t.Fatalf("control file refused: %v", err)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"zero key", []byte(strings.Repeat("0", 64) + "\n")},
		{"uppercase hex", []byte(strings.ToUpper(keyHex) + "\n")},
		{"one uppercase letter", []byte(gfUpperOneLetter(t, keyHex) + "\n")},
		{"missing LF", []byte(keyHex)},
		{"CRLF", []byte(keyHex + "\r\n")},
		{"CR instead of LF", []byte(keyHex + "\r")},
		{"trailing space instead of LF", []byte(keyHex + " ")},
		{"trailing space before LF", []byte(keyHex + " \n")},
		{"trailing extra LF", []byte(keyHex + "\n\n")},
		{"trailing extra byte", []byte(valid + "x")},
		{"leading space", []byte(" " + keyHex + "\n")},
		{"leading space at 65 bytes", []byte(" " + keyHex[:63] + "\n")},
		{"leading LF", []byte("\n" + keyHex)},
		{"63 hex digits", []byte(keyHex[:63] + "\n")},
		{"63 hex digits and two LF", []byte(keyHex[:63] + "\n\n")},
		{"65 hex digits", []byte(keyHex + "a\n")},
		{"65 hex digits without LF", []byte(keyHex + "a")},
		{"32 hex digits", []byte(keyHex[:32] + "\n")},
		{"non-hex first", []byte("g" + keyHex[1:] + "\n")},
		{"non-hex middle", []byte(keyHex[:31] + "x" + keyHex[32:] + "\n")},
		{"non-hex last", []byte(keyHex[:63] + "z\n")},
		{"hex prefix", []byte("0x" + keyHex[2:] + "\n")},
		{"dash separated byte", []byte(keyHex[:2] + "-" + keyHex[3:] + "\n")},
		{"non-ASCII byte", append([]byte(keyHex[:63]), 0xff, '\n')},
		{"embedded NUL", []byte(keyHex[:20] + "\x00" + keyHex[21:] + "\n")},
		{"two keys", []byte(valid + valid)},
		{"base64 spelling", []byte(base64.StdEncoding.EncodeToString(public[:]) + "\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gfWantKeyRefused(t, tc.body)
		})
	}

	t.Run("one byte replaced by non-canonical byte", func(t *testing.T) {
		for _, replacement := range []byte{' ', '\t', '\r', 0, 0xff, 'G'} {
			for i := range valid {
				body := []byte(valid)
				body[i] = replacement
				gfWantKeyRefused(t, body)
			}
		}
	})
}

// This documents the helper-side verification contract: a grant is only valid
// against the request the helper itself rebuilds from the challenge line it
// emitted, the intent it decoded and that intent's marker digest.
func TestProbeBinaryStageGrantFrameVerifiesOnlyRebuiltRequest(t *testing.T) {
	key, public := gfAuthority(4)
	_, otherPublic := gfAuthority(5)
	intent := gfIntent()
	nonce := gfNonce(9)

	// Helper side: trusted key file in, challenge line out.
	trusted, err := ParseProbeBinaryStageAuthorityKey(EncodeProbeBinaryStageAuthorityKey(public))
	if err != nil || trusted != public {
		t.Fatalf("trusted key did not survive its file: %v", err)
	}
	line := EncodeProbeBinaryStageChallengeLine(domain.ProbeBinaryStageAuthorityKeyID(trusted), nonce)
	if line == nil {
		t.Fatal("challenge line refused")
	}

	// Hub side: parse the line, build the request, sign, frame.
	lineKeyID, lineNonce, err := ParseProbeBinaryStageChallengeLine(line)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	hubRequest := domain.ProbeBinaryStageAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: lineKeyID, Nonce: lineNonce, MarkerSHA256: intent.MarkerSHA256()},
	}
	if !hubRequest.Valid() {
		t.Fatal("hub request invalid")
	}
	frame, err := EncodeProbeBinaryStageGrantFrame(gfSignedGrant(t, key, hubRequest))
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}

	// Helper side again: rebuild from only what the helper itself knows.
	rebuild := func(t *testing.T, keyID string, nonce [32]byte, in domain.ProbeBinaryStageIntent, frame []byte) domain.ProbeBinaryStageGrant {
		t.Helper()
		signature, err := DecodeProbeBinaryStageGrantFrame(frame)
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		return domain.ProbeBinaryStageGrant{
			Request: domain.ProbeBinaryStageAdmissionRequest{
				Intent:    in,
				Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: in.MarkerSHA256()},
			},
			Signature: signature,
		}
	}

	t.Run("matching rebuild verifies under the trusted key", func(t *testing.T) {
		grant := rebuild(t, lineKeyID, lineNonce, intent, frame)
		if !grant.ValidFor(trusted) {
			t.Fatal("grant over the rebuilt request did not verify")
		}
	})

	t.Run("any single nonce digit changed on the line", func(t *testing.T) {
		offset := len(gfChallengePrefix) + 64 + len(gfChallengeMiddle)
		for i := range 64 {
			tampered := bytes.Clone(line)
			tampered[offset+i] = gfNextHex(tampered[offset+i])
			keyID, nonce, err := ParseProbeBinaryStageChallengeLine(tampered)
			if err != nil {
				t.Fatalf("digit %d: tampered line unexpectedly malformed: %v", i, err)
			}
			if nonce == lineNonce {
				t.Fatalf("digit %d: tamper did not change the nonce", i)
			}
			grant := rebuild(t, keyID, nonce, intent, frame)
			if !grant.Request.Valid() {
				t.Fatalf("digit %d: rebuilt request invalid; failure would not prove the signature binds the nonce", i)
			}
			if grant.ValidFor(trusted) {
				t.Fatalf("digit %d: grant verified against a different nonce", i)
			}
		}
	})

	t.Run("any single nonce byte flipped before encoding", func(t *testing.T) {
		for i := range lineNonce {
			flipped := lineNonce
			flipped[i] ^= 0x01
			grant := rebuild(t, lineKeyID, flipped, intent, frame)
			if !grant.Request.Valid() || grant.ValidFor(trusted) {
				t.Fatalf("byte %d: flipped nonce verified or was invalid", i)
			}
		}
	})

	t.Run("different intent never verifies", func(t *testing.T) {
		mutations := []struct {
			name   string
			mutate func(in *domain.ProbeBinaryStageIntent)
		}{
			{"owner id", func(in *domain.ProbeBinaryStageIntent) { in.OwnerID = "44444444-4444-4444-8444-444444444444" }},
			{"attempt id", func(in *domain.ProbeBinaryStageIntent) { in.AttemptID = "44444444-4444-4444-8444-444444444444" }},
			{"operation id", func(in *domain.ProbeBinaryStageIntent) {
				in.Identity.OperationID = "44444444-4444-4444-8444-444444444444"
			}},
			{"ssh address", func(in *domain.ProbeBinaryStageIntent) { in.Identity.Host.Address = "other.example.test:22" }},
			{"host key fingerprint", func(in *domain.ProbeBinaryStageIntent) { in.Identity.HostKeyFingerprint = gfFingerprint(1) }},
			{"client key fingerprint", func(in *domain.ProbeBinaryStageIntent) { in.Identity.ClientKeyFingerprint = gfFingerprint(1) }},
			{"requested by", func(in *domain.ProbeBinaryStageIntent) { in.Identity.RequestedBy = 2 }},
			{"binary version", func(in *domain.ProbeBinaryStageIntent) { in.Identity.Binary.Version = "1.2.4" }},
			{"binary architecture", func(in *domain.ProbeBinaryStageIntent) { in.Identity.Binary.Architecture = "arm64" }},
			{"binary sha256", func(in *domain.ProbeBinaryStageIntent) { in.Identity.Binary.SHA256 = strings.Repeat("b", 64) }},
			{"binary size", func(in *domain.ProbeBinaryStageIntent) { in.Identity.Binary.Size = 18 }},
		}
		for _, tc := range mutations {
			t.Run(tc.name, func(t *testing.T) {
				changed := intent
				tc.mutate(&changed)
				if !changed.Valid() || changed.MarkerSHA256() == intent.MarkerSHA256() {
					t.Fatal("mutation must yield a valid intent with a different marker")
				}
				grant := rebuild(t, lineKeyID, lineNonce, changed, frame)
				if !grant.Request.Valid() {
					t.Fatal("rebuilt request invalid; failure would not prove the signature binds the intent")
				}
				if grant.ValidFor(trusted) {
					t.Fatal("grant verified against a different intent")
				}
			})
		}

		t.Run("non-root user", func(t *testing.T) {
			changed := intent
			changed.Identity.Host.User = "deploy"
			if !changed.Valid() {
				t.Fatal("non-root intent must itself be valid")
			}
			if rebuild(t, lineKeyID, lineNonce, changed, frame).ValidFor(trusted) {
				t.Fatal("grant verified for a non-root intent")
			}
		})
	})

	t.Run("marker digest not derived from the intent never verifies", func(t *testing.T) {
		grant := rebuild(t, lineKeyID, lineNonce, intent, frame)
		other := intent
		other.AttemptID = "44444444-4444-4444-8444-444444444444"
		grant.Request.Challenge.MarkerSHA256 = other.MarkerSHA256()
		if grant.ValidFor(trusted) {
			t.Fatal("grant verified with a marker that does not match its intent")
		}
	})

	t.Run("wrong or zero trusted key never verifies", func(t *testing.T) {
		grant := rebuild(t, lineKeyID, lineNonce, intent, frame)
		if grant.ValidFor(otherPublic) {
			t.Fatal("grant verified under a different authority")
		}
		if grant.ValidFor([32]byte{}) {
			t.Fatal("grant verified under the zero key")
		}
	})

	t.Run("challenge line naming another authority never verifies", func(t *testing.T) {
		otherLine := EncodeProbeBinaryStageChallengeLine(domain.ProbeBinaryStageAuthorityKeyID(otherPublic), lineNonce)
		keyID, nonce, err := ParseProbeBinaryStageChallengeLine(otherLine)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		grant := rebuild(t, keyID, nonce, intent, frame)
		if grant.ValidFor(trusted) || grant.ValidFor(otherPublic) {
			t.Fatal("grant verified for a request naming another authority")
		}
	})

	t.Run("any single signature bit flipped in the frame", func(t *testing.T) {
		for i := len(gfGrantMagic); i < len(frame); i++ {
			tampered := bytes.Clone(frame)
			tampered[i] ^= 0x01
			grant := rebuild(t, lineKeyID, lineNonce, intent, tampered)
			if grant.ValidFor(trusted) {
				t.Fatalf("frame byte %d: tampered signature verified", i)
			}
		}
	})
}
