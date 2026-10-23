package provisioning

import (
	"bytes"
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Independent literals: a changed constant must not silently change what these
// tests accept.
const (
	hostActFrameMagicLiteral    = "PHXACT1\n"
	hostActFrameStageMagic      = "PHXSTG1\n"
	hostActFrameGrantMagic      = "PHXGRT1\n"
	hostActFramePrefixSize      = 9
	hostActFrameMaxKind         = 32
	hostActFrameReceiptPrefix   = `{"state":"host_action_applied","kind":"`
	hostActFrameReceiptMiddle   = `","marker_sha256":"`
	hostActFrameOperationID     = "11111111-1111-4111-8111-111111111111"
	hostActFrameOwnerID         = "22222222-2222-4222-8222-222222222222"
	hostActFrameAttemptID       = "33333333-3333-4333-8333-333333333333"
	hostActFrameOtherOperation  = "44444444-4444-4444-8444-444444444444"
	hostActFrameKindData        = "data_directory"
	hostActFrameKindUnit        = "unit_file"
	hostActFrameKindService     = "service"
	hostActFrameKindArtifactDir = "artifact_directory"
)

var hostActFrameKinds = []string{hostActFrameKindData, hostActFrameKindUnit, hostActFrameKindService}

func hostActFrameFingerprint(b byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func hostActFrameIdentity() domain.ProbeInstallIdentity {
	return domain.ProbeInstallIdentity{
		OperationID:          hostActFrameOperationID,
		Host:                 domain.ProbeSSHHost{Address: "example.test:22", User: "root"},
		HostKeyFingerprint:   hostActFrameFingerprint(0),
		ClientKeyFingerprint: hostActFrameFingerprint(1),
		RequestedBy:          7,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 17},
	}
}

func hostActFrameIntent(kind string) domain.ProbeHostActionIntent {
	return domain.ProbeHostActionIntent{Identity: hostActFrameIdentity(), OwnerID: hostActFrameOwnerID, AttemptID: hostActFrameAttemptID, Kind: kind}
}

func hostActFrameStageHeader(t *testing.T, intent domain.ProbeHostActionIntent) []byte {
	t.Helper()
	header, err := EncodeProbeBinaryStageHeader(domain.ProbeBinaryStageIntent{Identity: intent.Identity, OwnerID: intent.OwnerID, AttemptID: intent.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// hostActFrameSystemctl proves the ProbeSystemctl port has the documented shape.
type hostActFrameSystemctl struct{}

func (hostActFrameSystemctl) Run(context.Context, ...string) (string, int, error) { return "", 0, nil }

var _ ProbeSystemctl = hostActFrameSystemctl{}

// hostActFrameParse mirrors what the host helper does with the action frame:
// exact 9-byte prefix, exact kind bytes, a 12-byte stage prefix with its
// bounded length, then the exact header. It returns the intent and whatever
// follows the header (the grant frame and EOF in the real protocol).
func hostActFrameParse(frame []byte) (domain.ProbeHostActionIntent, []byte, error) {
	var zero domain.ProbeHostActionIntent
	if len(frame) < ProbeHostActionPrefixSize {
		return zero, nil, domain.ErrProbeBinaryStageWriteInput
	}
	n, err := ProbeHostActionKindLength(frame[:ProbeHostActionPrefixSize])
	if err != nil {
		return zero, nil, err
	}
	rest := frame[ProbeHostActionPrefixSize:]
	if len(rest) < n+ProbeBinaryStagePrefixSize {
		return zero, nil, domain.ErrProbeBinaryStageWriteInput
	}
	kind, rest := rest[:n], rest[n:]
	hl, err := ProbeBinaryStageHeaderLength(rest[:ProbeBinaryStagePrefixSize])
	if err != nil {
		return zero, nil, err
	}
	if len(rest) < ProbeBinaryStagePrefixSize+hl {
		return zero, nil, domain.ErrProbeBinaryStageWriteInput
	}
	header, tail := rest[:ProbeBinaryStagePrefixSize+hl], rest[ProbeBinaryStagePrefixSize+hl:]
	intent, err := DecodeProbeHostActionIntent(kind, header)
	return intent, tail, err
}

func TestProbeHostActionFrameConstants(t *testing.T) {
	if ProbeHostActionPrefixSize != hostActFramePrefixSize || MaxProbeHostActionKindSize != hostActFrameMaxKind {
		t.Fatalf("frame size constants drifted: %d %d", ProbeHostActionPrefixSize, MaxProbeHostActionKindSize)
	}
	if hostActionMagic != hostActFrameMagicLiteral || len(hostActionMagic)+1 != ProbeHostActionPrefixSize {
		t.Fatalf("action magic drifted: %q", hostActionMagic)
	}
	if hostActFrameMagicLiteral == hostActFrameStageMagic || hostActFrameMagicLiteral == hostActFrameGrantMagic {
		t.Fatal("action magic must differ from the stage header and grant magics")
	}
	ctl := reflect.TypeOf((*ProbeSystemctl)(nil)).Elem()
	run, ok := ctl.MethodByName("Run")
	if !ok || ctl.NumMethod() != 1 || !run.Type.IsVariadic() || run.Type.NumIn() != 2 || run.Type.NumOut() != 3 {
		t.Fatalf("ProbeSystemctl shape drifted: %v", run.Type)
	}
}

func TestEncodeProbeHostActionFrameRoundTrip(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		intent := hostActFrameIntent(kind)
		frame, err := EncodeProbeHostActionFrame(intent)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		header := hostActFrameStageHeader(t, intent)
		want := append([]byte(hostActFrameMagicLiteral), byte(len(kind)))
		want = append(want, kind...)
		want = append(want, header...)
		if !bytes.Equal(frame, want) {
			t.Fatalf("%s: frame bytes changed (%d vs %d bytes)", kind, len(frame), len(want))
		}
		if !bytes.HasPrefix(frame, []byte(hostActFrameMagicLiteral)) || bytes.HasPrefix(frame, []byte(hostActFrameStageMagic)) {
			t.Fatalf("%s: frame starts with the wrong magic", kind)
		}
		n, err := ProbeHostActionKindLength(frame[:ProbeHostActionPrefixSize])
		if err != nil || n != len(kind) {
			t.Fatalf("%s: kind length %d %v", kind, n, err)
		}
		got, err := DecodeProbeHostActionIntent(frame[ProbeHostActionPrefixSize:ProbeHostActionPrefixSize+n], frame[ProbeHostActionPrefixSize+n:])
		if err != nil || got != intent {
			t.Fatalf("%s: round trip %+v %v", kind, got, err)
		}
		// The header after the kind is the unchanged stage header.
		if !bytes.Equal(frame[ProbeHostActionPrefixSize+n:], header) {
			t.Fatalf("%s: header is not the unchanged stage header", kind)
		}
		parsed, tail, err := hostActFrameParse(append(append([]byte(nil), frame...), "TAIL"...))
		if err != nil || parsed != intent || string(tail) != "TAIL" {
			t.Fatalf("%s: helper-style parse %+v %q %v", kind, parsed, tail, err)
		}
	}
	// Distinct kinds give distinct frames, so a data_directory frame cannot be
	// replayed to apply a service.
	seen := map[string]string{}
	for _, kind := range hostActFrameKinds {
		frame, _ := EncodeProbeHostActionFrame(hostActFrameIntent(kind))
		if other, dup := seen[string(frame)]; dup {
			t.Fatalf("%s and %s encode to the same frame", kind, other)
		}
		seen[string(frame)] = kind
	}
}

func TestEncodeProbeHostActionFrameRefusesInvalidIntents(t *testing.T) {
	cases := map[string]func(*domain.ProbeHostActionIntent){
		"artifact_directory":  func(i *domain.ProbeHostActionIntent) { i.Kind = hostActFrameKindArtifactDir },
		"container":           func(i *domain.ProbeHostActionIntent) { i.Kind = "container" },
		"empty kind":          func(i *domain.ProbeHostActionIntent) { i.Kind = "" },
		"kind with newline":   func(i *domain.ProbeHostActionIntent) { i.Kind = "service\n" },
		"upper kind":          func(i *domain.ProbeHostActionIntent) { i.Kind = "SERVICE" },
		"docker mode":         func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
		"non-root":            func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
		"empty owner":         func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed attempt":   func(i *domain.ProbeHostActionIntent) { i.AttemptID = "../../x" },
		"invalid binary pin":  func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
		"bad operation id":    func(i *domain.ProbeHostActionIntent) { i.Identity.OperationID = "BAD" },
		"bad layout":          func(i *domain.ProbeHostActionIntent) { i.Identity.LayoutVersion = 99 },
		"bad host key pin":    func(i *domain.ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = "SHA256:short" },
		"address with inject": func(i *domain.ProbeHostActionIntent) { i.Identity.Host.Address = "example.test:22\"}" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			intent := hostActFrameIntent(hostActFrameKindService)
			mutate(&intent)
			frame, err := EncodeProbeHostActionFrame(intent)
			if err != domain.ErrProbeBinaryStageWriteInput || frame != nil {
				t.Fatalf("frame %d bytes, err %v; want nil and the input sentinel", len(frame), err)
			}
		})
	}
	var zero domain.ProbeHostActionIntent
	if frame, err := EncodeProbeHostActionFrame(zero); err != domain.ErrProbeBinaryStageWriteInput || frame != nil {
		t.Fatalf("zero intent %v %v", frame, err)
	}
}

func TestProbeHostActionKindLengthAcceptsOnlyExactPrefix(t *testing.T) {
	prefix := func(n byte) []byte { return append([]byte(hostActFrameMagicLiteral), n) }
	for _, n := range []byte{1, 7, 14, 31, 32} {
		got, err := ProbeHostActionKindLength(prefix(n))
		if err != nil || got != int(n) {
			t.Fatalf("length %d: %d %v", n, got, err)
		}
	}
	for _, n := range []int{0, 33, 34, 64, 128, 255} {
		got, err := ProbeHostActionKindLength(prefix(byte(n)))
		if err != domain.ErrProbeBinaryStageWriteInput || got != 0 {
			t.Fatalf("kind length %d accepted: %d %v", n, got, err)
		}
	}
	// Every magic byte is load-bearing.
	for i := range len(hostActFrameMagicLiteral) {
		bad := prefix(5)
		bad[i] ^= 0x01
		if got, err := ProbeHostActionKindLength(bad); err != domain.ErrProbeBinaryStageWriteInput || got != 0 {
			t.Fatalf("magic byte %d flipped, accepted: %d %v", i, got, err)
		}
	}
	// The stage header and grant magics are not an action prefix.
	for _, magic := range []string{hostActFrameStageMagic, hostActFrameGrantMagic, "PHXACT2\n", "phxact1\n", "PHXACT1 "} {
		if got, err := ProbeHostActionKindLength(append([]byte(magic), 5)); err != domain.ErrProbeBinaryStageWriteInput || got != 0 {
			t.Fatalf("magic %q accepted: %d %v", magic, got, err)
		}
	}
	// Only an exact 9-byte prefix is inspected.
	good := prefix(5)
	for name, bad := range map[string][]byte{
		"nil":            nil,
		"empty":          {},
		"magic only":     []byte(hostActFrameMagicLiteral),
		"eight bytes":    good[:8],
		"ten bytes":      append(append([]byte(nil), good...), 'x'),
		"prefix + kind":  append(append([]byte(nil), good...), "service"...),
		"oversize frame": bytes.Repeat([]byte{'P'}, 4096),
	} {
		if got, err := ProbeHostActionKindLength(bad); err != domain.ErrProbeBinaryStageWriteInput || got != 0 {
			t.Fatalf("%s accepted: %d %v", name, got, err)
		}
	}
}

func TestDecodeProbeHostActionIntentRefusals(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	header := hostActFrameStageHeader(t, intent)

	if got, err := DecodeProbeHostActionIntent([]byte(hostActFrameKindService), header); err != nil || got != intent {
		t.Fatalf("control decode %+v %v", got, err)
	}

	refused := func(name string, kind, hdr []byte) {
		t.Helper()
		got, err := DecodeProbeHostActionIntent(kind, hdr)
		if err != domain.ErrProbeBinaryStageWriteInput || got != (domain.ProbeHostActionIntent{}) {
			t.Fatalf("%s: %+v %v, want the zero intent and the input sentinel", name, got, err)
		}
	}

	// Kind: only the three exact names decode.
	for _, kind := range []string{
		hostActFrameKindArtifactDir, "container", "image_cache", "", "Service", "SERVICE", "service ", " service", "service\n",
		"service\x00", "data-directory", "unit_file/", "../service", strings.Repeat("a", 32), strings.Repeat("a", 33),
	} {
		refused("kind "+kind, []byte(kind), header)
	}
	refused("nil kind", nil, header)

	// Header: truncated, extended, empty, wrong magic, wrong length field.
	refused("empty header", []byte(hostActFrameKindService), nil)
	refused("nil header", []byte(hostActFrameKindService), []byte{})
	refused("prefix only", []byte(hostActFrameKindService), header[:ProbeBinaryStagePrefixSize])
	for _, cut := range []int{1, 2, 10, len(header) - ProbeBinaryStagePrefixSize} {
		refused("header truncated", []byte(hostActFrameKindService), header[:len(header)-cut])
	}
	refused("header with an extra byte", []byte(hostActFrameKindService), append(append([]byte(nil), header...), 0))
	refused("header with an extra LF", []byte(hostActFrameKindService), append(append([]byte(nil), header...), '\n'))
	refused("header followed by a grant frame", []byte(hostActFrameKindService), append(append([]byte(nil), header...), []byte(hostActFrameGrantMagic+strings.Repeat("s", 64))...))
	refused("header repeated", []byte(hostActFrameKindService), append(append([]byte(nil), header...), header...))
	for i := range 8 {
		bad := append([]byte(nil), header...)
		bad[i] ^= 0x01
		refused("header magic byte flipped", []byte(hostActFrameKindService), bad)
	}
	bad := append([]byte(nil), header...)
	bad[ProbeBinaryStagePrefixSize] = '['
	refused("header body not an object", []byte(hostActFrameKindService), bad)
	// The action prefix is not a stage header.
	refused("action prefix as header", []byte(hostActFrameKindService), append([]byte(hostActFrameMagicLiteral), header[8:]...))

	// A header the stage codec accepts for a non-root user still never decodes
	// as a host action: the intent must be root.
	deploy := intent
	deploy.Identity.Host.User = "deploy"
	deployHeader := hostActFrameStageHeader(t, deploy)
	refused("non-root header", []byte(hostActFrameKindService), deployHeader)
}

// Every proper prefix of a frame and every frame with trailing garbage inside
// the header must fail to decode; extra bytes after the header are the next
// frame's business (the grant), never part of the intent.
func TestProbeHostActionFrameTruncationAndExtension(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		frame, err := EncodeProbeHostActionFrame(hostActFrameIntent(kind))
		if err != nil {
			t.Fatal(err)
		}
		for cut := range len(frame) {
			if got, _, err := hostActFrameParse(frame[:cut]); err == nil {
				t.Fatalf("%s: truncated frame (%d of %d bytes) decoded to %+v", kind, cut, len(frame), got)
			}
		}
		n := len(kind)
		// Growing the header inside the exact slice is refused.
		if _, err := DecodeProbeHostActionIntent(frame[ProbeHostActionPrefixSize:ProbeHostActionPrefixSize+n], append(frame[ProbeHostActionPrefixSize+n:len(frame):len(frame)], 'x')); err == nil {
			t.Fatalf("%s: header plus one byte decoded", kind)
		}
		// Shrinking the kind length byte makes the kind a strict prefix of the
		// real name, and the remainder shifts into the header: refused.
		short := append([]byte(nil), frame...)
		short[len(hostActFrameMagicLiteral)] = byte(n - 1)
		if got, _, err := hostActFrameParse(short); err == nil {
			t.Fatalf("%s: shortened kind length decoded to %+v", kind, got)
		}
		long := append([]byte(nil), frame...)
		long[len(hostActFrameMagicLiteral)] = byte(n + 1)
		if got, _, err := hostActFrameParse(long); err == nil {
			t.Fatalf("%s: lengthened kind length decoded to %+v", kind, got)
		}
	}
}

func TestEncodeProbeHostActionGrantFrame(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	var signature [64]byte
	for i := range signature {
		signature[i] = byte(i + 1)
	}
	request := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: strings.Repeat("ab", 32), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
	grant := domain.ProbeHostActionGrant{Request: request, Signature: signature}
	frame, err := EncodeProbeHostActionGrantFrame(grant)
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]byte(hostActFrameGrantMagic), signature[:]...); !bytes.Equal(frame, want) || len(frame) != 72 {
		t.Fatalf("grant frame %d bytes", len(frame))
	}
	if ProbeBinaryStageGrantFrameSize != 72 {
		t.Fatalf("grant frame size constant %d", ProbeBinaryStageGrantFrameSize)
	}
	decoded, err := DecodeProbeBinaryStageGrantFrame(frame)
	if err != nil || decoded != signature {
		t.Fatalf("decode %v %v", decoded, err)
	}
	// The frame is shared with binary-stage grants; the signing domain, not the
	// frame, separates them.
	stage := hostActFrameStageGrant(request, signature)
	stageFrame, err := EncodeProbeBinaryStageGrantFrame(stage)
	if err != nil || !bytes.Equal(stageFrame, frame) {
		t.Fatalf("frame differs from the binary-stage grant frame: %v", err)
	}

	for name, bad := range map[string]domain.ProbeHostActionGrant{
		"zero grant":     {},
		"zero signature": {Request: request},
		"no request":     {Signature: signature},
		"marker mismatch": {Request: func() domain.ProbeHostActionAdmissionRequest {
			r := request
			r.Challenge.MarkerSHA256 = strings.Repeat("0", 64)
			return r
		}(), Signature: signature},
		"zero nonce": {Request: func() domain.ProbeHostActionAdmissionRequest {
			r := request
			r.Challenge.Nonce = [32]byte{}
			return r
		}(), Signature: signature},
		"artifact_directory kind": {Request: func() domain.ProbeHostActionAdmissionRequest {
			r := request
			r.Intent.Kind = hostActFrameKindArtifactDir
			return r
		}(), Signature: signature},
		"non-root": {Request: func() domain.ProbeHostActionAdmissionRequest {
			r := request
			r.Intent.Identity.Host.User = "deploy"
			r.Challenge.MarkerSHA256 = r.Intent.MarkerSHA256()
			return r
		}(), Signature: signature},
	} {
		if got, err := EncodeProbeHostActionGrantFrame(bad); err != domain.ErrProbeBinaryStageWriteInput || got != nil {
			t.Fatalf("%s: frame %d bytes, err %v", name, len(got), err)
		}
	}
}

func hostActFrameStageGrant(request domain.ProbeHostActionAdmissionRequest, signature [64]byte) domain.ProbeBinaryStageGrant {
	stage := domain.ProbeBinaryStageIntent{Identity: request.Intent.Identity, OwnerID: request.Intent.OwnerID, AttemptID: request.Intent.AttemptID}
	return domain.ProbeBinaryStageGrant{
		Request: domain.ProbeBinaryStageAdmissionRequest{Intent: stage, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: request.Challenge.AuthorityKeyID, Nonce: request.Challenge.Nonce, MarkerSHA256: stage.MarkerSHA256(),
		}},
		Signature: signature,
	}
}

func TestEncodeProbeHostActionReceiptCanonicalLine(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		intent := hostActFrameIntent(kind)
		receipt := EncodeProbeHostActionReceipt(intent)
		want := hostActFrameReceiptPrefix + kind + hostActFrameReceiptMiddle + intent.MarkerSHA256() + "\"}\n"
		if string(receipt) != want {
			t.Fatalf("%s receipt %q, want %q", kind, receipt, want)
		}
		if !bytes.HasSuffix(receipt, []byte("\"}\n")) || bytes.Count(receipt, []byte("\n")) != 1 || len(intent.MarkerSHA256()) != 64 {
			t.Fatalf("%s receipt is not one LF-terminated line: %q", kind, receipt)
		}
	}
	// A canonical line carries no secrets: only the kind and the marker digest.
	line := string(EncodeProbeHostActionReceipt(hostActFrameIntent(hostActFrameKindUnit)))
	for _, leak := range []string{"example.test", "root", hostActFrameOperationID, hostActFrameOwnerID, hostActFrameAttemptID, "SHA256:", "/var/lib", "/etc/systemd"} {
		if strings.Contains(line, leak) {
			t.Fatalf("receipt leaks %q: %q", leak, line)
		}
	}
	for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
		"artifact_directory": func(i *domain.ProbeHostActionIntent) { i.Kind = hostActFrameKindArtifactDir },
		"empty kind":         func(i *domain.ProbeHostActionIntent) { i.Kind = "" },
		"docker":             func(i *domain.ProbeHostActionIntent) { i.Identity.Mode = domain.ProbeInstallDocker },
		"non-root":           func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
		"bad owner":          func(i *domain.ProbeHostActionIntent) { i.OwnerID = "x" },
	} {
		intent := hostActFrameIntent(hostActFrameKindService)
		mutate(&intent)
		if got := EncodeProbeHostActionReceipt(intent); got != nil {
			t.Fatalf("%s: invalid intent rendered a receipt %q", name, got)
		}
	}
	if got := EncodeProbeHostActionReceipt(domain.ProbeHostActionIntent{}); got != nil {
		t.Fatalf("zero intent rendered %q", got)
	}
}

func TestProbeHostActionReceiptMatchesExactLineOnly(t *testing.T) {
	for _, kind := range hostActFrameKinds {
		intent := hostActFrameIntent(kind)
		line := EncodeProbeHostActionReceipt(intent)
		if !ProbeHostActionReceiptMatches(line, intent) {
			t.Fatalf("%s: canonical receipt refused", kind)
		}
		if !ProbeHostActionReceiptMatches(append([]byte(nil), line...), intent) {
			t.Fatalf("%s: a copy of the canonical receipt refused", kind)
		}

		with := func(edit func([]byte) []byte) []byte { return edit(append([]byte(nil), line...)) }
		bad := map[string][]byte{
			"extra byte":          append(append([]byte(nil), line...), 'x'),
			"extra LF":            append(append([]byte(nil), line...), '\n'),
			"receipt twice":       append(append([]byte(nil), line...), line...),
			"leading space":       append([]byte(" "), line...),
			"leading LF":          append([]byte("\n"), line...),
			"missing LF":          line[:len(line)-1],
			"CRLF":                with(func(b []byte) []byte { return append(b[:len(b)-1], '\r', '\n') }),
			"truncated":           line[:len(line)/2],
			"empty":               {},
			"nil":                 nil,
			"upper marker":        []byte(strings.ToUpper(string(line[:len(line)-3])) + "\"}\n"),
			"short marker":        with(func(b []byte) []byte { return append(b[:len(b)-4], "\"}\n"...) }),
			"extra json key":      []byte(strings.Replace(string(line), `"}`+"\n", `","x":"y"}`+"\n", 1)),
			"keys reordered":      []byte(`{"kind":"` + kind + `","state":"host_action_applied","marker_sha256":"` + intent.MarkerSHA256() + `"}` + "\n"),
			"spaces after colons": []byte(strings.ReplaceAll(string(line), `":"`, `": "`)),
			"challenge line":      EncodeProbeBinaryStageChallengeLine(strings.Repeat("ab", 32), [32]byte{1}),
		}
		for name, got := range bad {
			if ProbeHostActionReceiptMatches(got, intent) {
				t.Fatalf("%s: %s accepted: %q", kind, name, got)
			}
		}

		// Every other kind's receipt for the same operation is refused.
		for _, other := range hostActFrameKinds {
			if other == kind {
				continue
			}
			if ProbeHostActionReceiptMatches(EncodeProbeHostActionReceipt(hostActFrameIntent(other)), intent) {
				t.Fatalf("%s receipt accepted for %s", other, kind)
			}
		}
		// A receipt with this kind but another marker (other operation, owner,
		// attempt, host, binary) is refused.
		for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
			"operation": func(i *domain.ProbeHostActionIntent) { i.Identity.OperationID = hostActFrameOtherOperation },
			"owner":     func(i *domain.ProbeHostActionIntent) { i.OwnerID = hostActFrameOtherOperation },
			"attempt":   func(i *domain.ProbeHostActionIntent) { i.AttemptID = hostActFrameOtherOperation },
			"address":   func(i *domain.ProbeHostActionIntent) { i.Identity.Host.Address = "other.test:22" },
			"binary":    func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size++ },
			"actor":     func(i *domain.ProbeHostActionIntent) { i.Identity.RequestedBy++ },
		} {
			other := intent
			mutate(&other)
			if !other.Valid() {
				t.Fatalf("%s/%s: fixture mutation invalid", kind, name)
			}
			if ProbeHostActionReceiptMatches(EncodeProbeHostActionReceipt(other), intent) {
				t.Fatalf("%s: a receipt for another %s was accepted", kind, name)
			}
		}
	}

	// An invalid intent matches nothing, including an empty line and the line
	// its own fields would render.
	for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
		"artifact_directory": func(i *domain.ProbeHostActionIntent) { i.Kind = hostActFrameKindArtifactDir },
		"empty kind":         func(i *domain.ProbeHostActionIntent) { i.Kind = "" },
		"non-root":           func(i *domain.ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
	} {
		intent := hostActFrameIntent(hostActFrameKindService)
		mutate(&intent)
		for _, line := range [][]byte{nil, {}, []byte("\n"), []byte(hostActFrameReceiptPrefix + intent.Kind + hostActFrameReceiptMiddle + strings.Repeat("0", 64) + "\"}\n")} {
			if ProbeHostActionReceiptMatches(line, intent) {
				t.Fatalf("%s: invalid intent matched %q", name, line)
			}
		}
	}
}
