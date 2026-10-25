package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestBinaryStageFramingAndAttrs(t *testing.T) {
	rw := &memRW{r: bytes.NewReader(testPkt(sftpVersion, testU32(3)))}
	pipe := &sftpPipe{ctx: context.Background(), r: rw, w: rw}
	if err := pipe.init(); err != nil {
		t.Fatal(err)
	}
	wantInit := []byte{0, 0, 0, 5, sftpInit, 0, 0, 0, 3}
	if !bytes.Equal(rw.w.Bytes(), wantInit) {
		t.Fatalf("INIT = %x", rw.w.Bytes())
	}

	var pairs []byte
	pairs = append(pairs, testU32(3)...)
	for i := 0; i < binaryStageExtMax; i++ {
		pairs = append(pairs, testU32(0)...)
		pairs = append(pairs, testU32(0)...)
	}
	if err := (&sftpPipe{ctx: context.Background(), r: bytes.NewReader(testPkt(sftpVersion, pairs)), w: &bytes.Buffer{}}).init(); err != nil {
		t.Fatal(err)
	}
	pairs = append(pairs, testU32(0)...)
	pairs = append(pairs, testU32(0)...)
	if err := (&sftpPipe{ctx: context.Background(), r: bytes.NewReader(testPkt(sftpVersion, pairs)), w: &bytes.Buffer{}}).init(); err == nil {
		t.Fatal("accepted 33 extensions")
	}
	if err := (&sftpPipe{ctx: context.Background(), r: bytes.NewReader(testPkt(sftpVersion, testU32(4))), w: &bytes.Buffer{}}).init(); err == nil {
		t.Fatal("accepted version 4")
	}
	if _, _, err := (&sftpPipe{ctx: context.Background(), r: bytes.NewReader([]byte{0, 0, 0, 0})}).readPacket(); err == nil {
		t.Fatal("accepted zero length")
	}
	over := []byte{0, 0, 0xff, 0xff}
	binary.BigEndian.PutUint32(over, binaryStagePacketMax+1)
	capR := &capReader{t: t, r: bytes.NewReader(over), max: 4}
	if _, _, err := (&sftpPipe{ctx: context.Background(), r: capR}).readPacket(); err == nil {
		t.Fatal("accepted oversize length")
	}

	if _, _, err := parseAttrs(testU32(0)); err == nil {
		t.Fatal("missing permissions")
	}
	if _, _, err := parseAttrs(append(testU32(0x10), testU32(0o100644)...)); err == nil {
		t.Fatal("unknown flag")
	}
	if _, _, err := parseAttrs(append(testU32(0x80000004), testU32(0o100644)...)); err == nil {
		t.Fatal("extended flag")
	}
	attrs, rest, err := parseAttrs(append(append(testU32(sftpAttrSize|sftpAttrPerms), testU64(4)...), testU32(0o100644)...))
	if err != nil || len(rest) != 0 || !attrs.hasSize || attrs.size != 4 || attrs.mode&posixIFMT != posixIFREG {
		t.Fatalf("attrs %+v %v %d", attrs, err, len(rest))
	}
	if _, err := parseStatus(append(testU32(sftpFXNoSuchFile), testU32(0)...)); err == nil {
		t.Fatal("status missing language")
	}
	if _, err := parseHandle(testU32(0)); err == nil {
		t.Fatal("empty handle")
	}
	handle, err := parseHandle(append(testU32(binaryStageHandleMax), bytes.Repeat([]byte{'h'}, binaryStageHandleMax)...))
	if err != nil || len(handle) != binaryStageHandleMax {
		t.Fatal(err)
	}
	if _, err := parseHandle(append(testU32(binaryStageHandleMax+1), bytes.Repeat([]byte{'h'}, binaryStageHandleMax+1)...)); err == nil {
		t.Fatal("oversized handle")
	}
	if _, err := parseData(append(testU32(2), 'a', 'b'), 1); err == nil {
		t.Fatal("accepted more data than requested")
	}
	if _, err := parseData(testU32(0), 1); err == nil {
		t.Fatal("accepted zero-length data")
	}
	if err := (&sftpPipe{ctx: context.Background(), w: &bytes.Buffer{}}).writePacket(6, nil); err == nil {
		t.Fatal("WRITE request permitted")
	}
	for _, typ := range []byte{9, 12, 13, 14, 18, 200} {
		if allowedStageRequest(typ) {
			t.Fatalf("type %d permitted", typ)
		}
	}
}

func TestBinaryStageScriptedOutcomes(t *testing.T) {
	intent := stageTestIntent(t, nil)
	marker := []byte(intent.Marker())
	payload := []byte("abcd")
	dir := intent.Directory()
	markerPath := dir + "/" + domain.ProbeBinaryStageMarkerName
	binaryPath := dir + "/" + intent.Identity.Binary.ReleaseName()

	absent := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testStatus(1, sftpFXNoSuchFile))
	if absent.obs.State != domain.ProbeBinaryStageAbsent || absent.err != nil || absent.obs.Intent != intent {
		t.Fatalf("absent %+v %v", absent.obs, absent.err)
	}
	pkts := splitPackets(t, absent.writes)
	if len(pkts) != 2 || pkts[0][0] != sftpInit || pkts[1][0] != sftpLstat || packetString(t, pkts[1], 5) != stageOptPath {
		t.Fatalf("absent writes %#v", pkts)
	}
	wantLstat := []byte{0, 0, 0, 13, sftpLstat, 0, 0, 0, 1, 0, 0, 0, 4, '/', 'o', 'p', 't'}
	if !bytes.Equal(absent.writes[9:], wantLstat) {
		t.Fatalf("LSTAT bytes %x", absent.writes[9:])
	}

	opMissing := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testStatus(2, sftpFXNoSuchFile))
	if opMissing.obs.State != domain.ProbeBinaryStageAbsent || opMissing.err != nil {
		t.Fatalf("op absent %+v %v", opMissing.obs, opMissing.err)
	}

	link := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testModeAttrs(1, 0o120777))
	if link.obs.State != domain.ProbeBinaryStageConflict || link.err != nil || countType(splitPackets(t, link.writes), sftpOpen) != 0 {
		t.Fatalf("symlink %+v %v", link.obs, link.err)
	}

	denied := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testStatus(1, 3))
	if denied.err != domain.ErrProbeBinaryStageInspection || denied.obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("permission %+v %v", denied.obs, denied.err)
	}
	unsupported := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testStatus(1, 8))
	if unsupported.err != domain.ErrProbeBinaryStageInspection || unsupported.obs.State == domain.ProbeBinaryStageAbsent {
		t.Fatalf("unsupported %+v %v", unsupported.obs, unsupported.err)
	}

	missingMarker := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testDirAttrs(2), testStatus(3, sftpFXNoSuchFile))
	if missingMarker.obs.State != domain.ProbeBinaryStageConflict || missingMarker.err != nil || countType(splitPackets(t, missingMarker.writes), sftpOpen) != 0 {
		t.Fatalf("missing marker %+v %v", missingMarker.obs, missingMarker.err)
	}
	markerDenied := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testDirAttrs(2), testStatus(3, 3))
	if markerDenied.err != domain.ErrProbeBinaryStageInspection || markerDenied.obs != (domain.ProbeBinaryStageObservation{}) || countType(splitPackets(t, markerDenied.writes), sftpOpen) != 0 {
		t.Fatalf("marker permission %+v %v", markerDenied.obs, markerDenied.err)
	}
	markerLink := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testDirAttrs(2), testModeAttrs(3, 0o120777))
	if markerLink.obs.State != domain.ProbeBinaryStageConflict || markerLink.err != nil || countType(splitPackets(t, markerLink.writes), sftpOpen) != 0 {
		t.Fatalf("marker symlink %+v %v", markerLink.obs, markerLink.err)
	}

	wrongHash := append([]byte(nil), marker...)
	wrongHash[len(wrongHash)-2] ^= 1
	hashed := runStage(t, intent, matchPrefix(marker, wrongHash)...)
	if hashed.obs.State != domain.ProbeBinaryStageConflict || hashed.err != nil || countType(splitPackets(t, hashed.writes), sftpClose) != 1 {
		t.Fatalf("hash %+v %v", hashed.obs, hashed.err)
	}

	over := runStage(t, intent,
		testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testDirAttrs(2), testFileAttrs(3, uint64(len(marker))),
		testHandle(4, "m"), testFileAttrs(5, uint64(len(marker))), testData(6, append(append([]byte(nil), marker...), 'x')), testStatus(7, sftpFXOK))
	if over.obs.State != domain.ProbeBinaryStageConflict || over.err != nil || countType(splitPackets(t, over.writes), sftpClose) != 1 {
		t.Fatalf("oversize %+v %v", over.obs, over.err)
	}

	badClose := runStage(t, intent, append(matchUntilEOF(marker, marker), testStatus(8, 4))...)
	if badClose.err != domain.ErrProbeBinaryStageInspection || badClose.obs != (domain.ProbeBinaryStageObservation{}) || countType(splitPackets(t, badClose.writes), sftpClose) != 1 {
		t.Fatalf("close %+v %v", badClose.obs, badClose.err)
	}

	matched := runStage(t, intent, fullMatch(marker, payload)...)
	if matched.err != nil || matched.obs.State != domain.ProbeBinaryStageContentMatches || matched.obs.Intent != intent {
		t.Fatalf("match %+v %v", matched.obs, matched.err)
	}
	seen := splitPackets(t, matched.writes)
	var ids []uint32
	for _, pkt := range seen {
		if pkt[0] == sftpInit {
			continue
		}
		if !allowedStageRequest(pkt[0]) {
			t.Fatalf("mutating type %d", pkt[0])
		}
		ids = append(ids, binary.BigEndian.Uint32(pkt[1:5]))
		if pkt[0] == sftpOpen {
			path, next := packetStringAt(t, pkt, 5)
			flags := binary.BigEndian.Uint32(pkt[next : next+4])
			attrFlags := binary.BigEndian.Uint32(pkt[next+4 : next+8])
			if flags != sftpFXFRead || attrFlags != 0 {
				t.Fatalf("OPEN flags %d %d", flags, attrFlags)
			}
			if path != markerPath && path != binaryPath {
				t.Fatalf("OPEN path %s", path)
			}
		}
	}
	for i, id := range ids {
		if id != uint32(i+1) {
			t.Fatalf("ids %v", ids)
		}
	}

	wrongID := runStage(t, intent, testPkt(sftpVersion, testU32(3)), testStatus(9, sftpFXNoSuchFile))
	if wrongID.err != domain.ErrProbeBinaryStageInspection || wrongID.obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("id %+v %v", wrongID.obs, wrongID.err)
	}
}

func TestBinaryStageCancelAndBudget(t *testing.T) {
	intent := stageTestIntent(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	obs, err := inspectBinaryStageSFTP(ctx, &memRW{r: bytes.NewReader(nil)}, intent, nil)
	if err != context.Canceled || obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("precanceled %+v %v", obs, err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	rw := &blockRW{ctx: ctx, started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		obs, err = inspectBinaryStageSFTP(ctx, rw, intent, nil)
	}()
	<-rw.started
	cancel()
	<-done
	if err != context.Canceled || obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("mid-read %+v %v", obs, err)
	}

	lim := &stderrLimit{close: func() error { return nil }}
	lim.add(binaryStageStderrMax)
	if lim.exceeded() {
		t.Fatal("budget boundary")
	}
	lim.add(1)
	if !lim.exceeded() {
		t.Fatal("expected overflow")
	}
	pr, pw := io.Pipe()
	closed := make(chan struct{})
	live := &stderrLimit{close: func() error {
		select {
		case <-closed:
		default:
			close(closed)
		}
		return pw.Close()
	}}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		discardStderr(pr, live)
	}()
	if _, err := pw.Write(bytes.Repeat([]byte{'x'}, binaryStageStderrMax+1)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	<-finished
	if !live.exceeded() {
		t.Fatal("stream did not trip budget")
	}
}

func TestSSHBinaryStageInspectorPinsBeforeDial(t *testing.T) {
	key, signer := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	_, otherSigner := deploymentKey(t)
	hostFP := ssh.FingerprintSHA256(hostSigner.PublicKey())
	clientFP := ssh.FingerprintSHA256(signer.PublicKey())
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	ctx := context.Background()

	var nilCtx context.Context // deliberately nil: the refusal under test
	if _, err := NewSSHBinaryStageInspector(nilCtx, key, hostFP, clientFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput {
		t.Fatalf("nil ctx %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := NewSSHBinaryStageInspector(canceled, key, hostFP, clientFP, probe.EndpointPolicy{}); err != context.Canceled {
		t.Fatalf("canceled ctor %v", err)
	}
	if insp, err := NewSSHBinaryStageInspector(ctx, key, hostFP, otherFP, probe.EndpointPolicy{}); err != domain.ErrProbeSSHInput || insp != nil {
		t.Fatalf("client pin ctor %+v %v", insp, err)
	}
	insp, err := NewSSHBinaryStageInspector(ctx, key, hostFP, clientFP, probe.EndpointPolicy{})
	if err != nil || insp.host == nil || insp.hostPin != hostFP || insp.clientPin != clientFP {
		t.Fatal(err)
	}
	insp.host.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("lookup")
		return nil, errors.New("lookup")
	}
	insp.host.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial")
		return nil, errors.New("dial")
	}
	intent := stageTestIntent(t, func(in *domain.ProbeBinaryStageIntent) {
		in.Identity.HostKeyFingerprint = hostFP
		in.Identity.ClientKeyFingerprint = clientFP
	})
	badHost := intent
	badHost.Identity.HostKeyFingerprint = otherFP
	obs, err := insp.InspectBinaryStage(ctx, badHost)
	if err != domain.ErrProbeSSHInput || obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("host pin %+v %v", obs, err)
	}
	badClient := intent
	badClient.Identity.ClientKeyFingerprint = otherFP
	obs, err = insp.InspectBinaryStage(ctx, badClient)
	if err != domain.ErrProbeSSHInput || obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("client pin %+v %v", obs, err)
	}
	badClient.Identity.Mode = domain.ProbeInstallDocker
	obs, err = insp.InspectBinaryStage(ctx, badClient)
	if err != domain.ErrProbeBinaryStageInput || obs != (domain.ProbeBinaryStageObservation{}) {
		t.Fatalf("invalid %+v %v", obs, err)
	}
	var nilInspector *SSHBinaryStageInspector
	if _, err := nilInspector.InspectBinaryStage(ctx, intent); err != domain.ErrProbeBinaryStageInput {
		t.Fatal(err)
	}
	past, cancelPast := context.WithDeadline(ctx, time.Unix(0, 0))
	defer cancelPast()
	if _, err := insp.InspectBinaryStage(past, intent); err != context.DeadlineExceeded {
		t.Fatalf("deadline %v", err)
	}
}

type stageRun struct {
	obs    domain.ProbeBinaryStageObservation
	err    error
	writes []byte
}

type memRW struct {
	r *bytes.Reader
	w bytes.Buffer
}

func (m *memRW) Read(p []byte) (int, error)  { return m.r.Read(p) }
func (m *memRW) Write(p []byte) (int, error) { return m.w.Write(p) }

type blockRW struct {
	ctx     context.Context
	started chan struct{}
	w       bytes.Buffer
	once    bool
}

func (b *blockRW) Write(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.started)
	}
	return b.w.Write(p)
}

func (b *blockRW) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

type capReader struct {
	t   *testing.T
	r   io.Reader
	max int
}

func (c *capReader) Read(p []byte) (int, error) {
	if len(p) > c.max {
		c.t.Fatalf("read %d exceeds %d", len(p), c.max)
	}
	return c.r.Read(p)
}

func runStage(t *testing.T, intent domain.ProbeBinaryStageIntent, replies ...[]byte) stageRun {
	t.Helper()
	rw := &memRW{r: bytes.NewReader(bytes.Join(replies, nil))}
	obs, err := inspectBinaryStageSFTP(context.Background(), rw, intent, nil)
	return stageRun{obs: obs, err: err, writes: append([]byte(nil), rw.w.Bytes()...)}
}

func stageTestIntent(t *testing.T, mutate func(*domain.ProbeBinaryStageIntent)) domain.ProbeBinaryStageIntent {
	t.Helper()
	sum := sha256.Sum256([]byte("abcd"))
	raw := bytes.Repeat([]byte{1}, 32)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(raw)
	intent := domain.ProbeBinaryStageIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          "11111111-1111-4111-8111-111111111111",
			Host:                 domain.ProbeSSHHost{Address: "203.0.113.10:22", User: "root"},
			HostKeyFingerprint:   fp,
			ClientKeyFingerprint: fp,
			RequestedBy:          7,
			LayoutVersion:        domain.ProbeInstallLayoutVersion,
			Mode:                 domain.ProbeInstallSystemd,
			Binary: domain.ProbeBinaryPin{
				Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: 4,
			},
		},
		OwnerID:   "22222222-2222-4222-8222-222222222222",
		AttemptID: "33333333-3333-4333-8333-333333333333",
	}
	if mutate != nil {
		mutate(&intent)
	}
	if mutate == nil && !intent.Valid() {
		t.Fatal("intent invalid")
	}
	return intent
}

func fullMatch(marker, binary []byte) [][]byte {
	return append(matchPrefix(marker, marker),
		testFileAttrs(9, uint64(len(binary))), testHandle(10, "b"), testFileAttrs(11, uint64(len(binary))),
		testData(12, binary), testStatus(13, sftpFXEOF), testStatus(14, sftpFXOK))
}

func matchPrefix(expected, served []byte) [][]byte {
	return append(matchUntilEOF(expected, served), testStatus(8, sftpFXOK))
}

func matchUntilEOF(expected, served []byte) [][]byte {
	return [][]byte{
		testPkt(sftpVersion, testU32(3)), testDirAttrs(1), testDirAttrs(2), testFileAttrs(3, uint64(len(expected))),
		testHandle(4, "m"), testFileAttrs(5, uint64(len(expected))), testData(6, served), testStatus(7, sftpFXEOF),
	}
}

func testDirAttrs(id uint32) []byte { return testModeAttrs(id, 0o040755) }

func testModeAttrs(id, mode uint32) []byte {
	return testPkt(sftpAttrs, append(append(testU32(id), testU32(sftpAttrPerms)...), testU32(mode)...))
}

func testFileAttrs(id uint32, size uint64) []byte {
	payload := append(testU32(id), testU32(sftpAttrSize|sftpAttrPerms)...)
	payload = append(payload, testU64(size)...)
	payload = append(payload, testU32(0o100644)...)
	return testPkt(sftpAttrs, payload)
}

func testStatus(id, code uint32) []byte {
	payload := append(testU32(id), testU32(code)...)
	payload = append(payload, testU32(0)...)
	payload = append(payload, testU32(0)...)
	return testPkt(sftpStatus, payload)
}

func testHandle(id uint32, handle string) []byte {
	return testPkt(sftpHandle, append(testU32(id), testString(handle)...))
}

func testData(id uint32, data []byte) []byte {
	payload := append(testU32(id), testU32(uint32(len(data)))...)
	return testPkt(sftpData, append(payload, data...))
}

func testPkt(typ byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out, uint32(1+len(payload)))
	out[4] = typ
	copy(out[5:], payload)
	return out
}

func testU32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func testU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func testString(s string) []byte {
	b := testU32(uint32(len(s)))
	return append(b, s...)
}

func splitPackets(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 4 {
			t.Fatalf("short framing header")
		}
		n := binary.BigEndian.Uint32(b[:4])
		if n == 0 || int(n) > len(b)-4 {
			t.Fatalf("bad framing length %d", n)
		}
		out = append(out, append([]byte(nil), b[4:4+n]...))
		b = b[4+n:]
	}
	return out
}

func packetString(t *testing.T, pkt []byte, off int) string {
	t.Helper()
	s, end := packetStringAt(t, pkt, off)
	if end != len(pkt) {
		t.Fatalf("trailing path bytes")
	}
	return s
}

func packetStringAt(t *testing.T, pkt []byte, off int) (string, int) {
	t.Helper()
	if len(pkt) < off+4 {
		t.Fatal("short string")
	}
	n := binary.BigEndian.Uint32(pkt[off : off+4])
	end := off + 4 + int(n)
	if end > len(pkt) {
		t.Fatal("string exceeds packet")
	}
	return string(pkt[off+4 : end]), end
}

func countType(pkts [][]byte, typ byte) int {
	n := 0
	for _, pkt := range pkts {
		if len(pkt) > 0 && pkt[0] == typ {
			n++
		}
	}
	return n
}
