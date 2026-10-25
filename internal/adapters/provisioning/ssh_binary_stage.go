package provisioning

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	binaryStageTimeout   = 30 * time.Second
	binaryStagePacketMax = 64 * 1024
	binaryStageReadMax   = 32 * 1024
	binaryStageHandleMax = 256
	binaryStageStderrMax = 4096
	binaryStageExtMax    = 32
	sftpSubsystem        = "sftp"
	stageOptPath         = "/opt"

	sftpInit    = 1
	sftpVersion = 2
	sftpOpen    = 3
	sftpClose   = 4
	sftpRead    = 5
	sftpLstat   = 7
	sftpFstat   = 8
	sftpStatus  = 101
	sftpHandle  = 102
	sftpData    = 103
	sftpAttrs   = 105

	sftpFXOK         = 0
	sftpFXEOF        = 1
	sftpFXNoSuchFile = 2

	sftpAttrSize   = 0x1
	sftpAttrUIDGID = 0x2
	sftpAttrPerms  = 0x4
	sftpAttrACMod  = 0x8
	sftpAttrKnown  = sftpAttrSize | sftpAttrUIDGID | sftpAttrPerms | sftpAttrACMod
	sftpFXFRead    = 1

	posixIFMT  = 0o170000
	posixIFDIR = 0o040000
	posixIFREG = 0o100000
)

// errBinaryStage is an internal framing failure. It is never returned.
var errBinaryStage = errors.New("binary stage protocol")

// SSHBinaryStageInspector reads one operation-derived marker and binary over a
// fixed SFTP subsystem. Observations are not host authority or journal grants.
// host is the unchanged preflight inspector; tests may replace its per-instance
// lookup and dial. hostPin and clientPin are configured public fingerprints.
type SSHBinaryStageInspector struct {
	host      *SSHHostInspector
	hostPin   string
	clientPin string
}

var _ ports.ProbeBinaryStageInspector = (*SSHBinaryStageInspector)(nil)

// NewSSHBinaryStageInspector parses one ephemeral client key and checks the
// public client fingerprint before any network I/O. The private-key buffer is
// borrowed only for the existing constructor and is not retained.
func NewSSHBinaryStageInspector(ctx context.Context, privateKey []byte, hostFingerprint, clientFingerprint string, policy probe.EndpointPolicy) (*SSHBinaryStageInspector, error) {
	if imageSnapshotNil(ctx) {
		return nil, domain.ErrProbeSSHInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validFingerprint(clientFingerprint) {
		return nil, domain.ErrProbeSSHInput
	}
	host, err := NewSSHHostInspector(ctx, privateKey, SSHHostTrust{Fingerprint: hostFingerprint}, policy)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(host.signer.PublicKey())), []byte(clientFingerprint)) != 1 {
		return nil, domain.ErrProbeSSHInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &SSHBinaryStageInspector{host: host, hostPin: hostFingerprint, clientPin: clientFingerprint}, nil
}

// InspectBinaryStage reads the derived marker and binary with SFTP v3 LSTAT,
// read-only OPEN, FSTAT, READ and CLOSE. Cancellation and transport failure
// discard every observation. A match is not an atomic snapshot or a fence.
func (s *SSHBinaryStageInspector) InspectBinaryStage(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error) {
	obs, err := s.inspectBinaryStageOnce(ctx, intent)
	if errors.Is(err, domain.ErrProbeSSHTrust) && ctx != nil && s != nil && s.host != nil && s.host.learnHostKeyAlgorithms(ctx, intent.Identity.Host.Address) {
		return s.inspectBinaryStageOnce(ctx, intent)
	}
	return obs, err
}

func (s *SSHBinaryStageInspector) inspectBinaryStageOnce(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error) {
	var zero domain.ProbeBinaryStageObservation
	if s == nil || imageSnapshotNil(ctx) || s.host == nil || s.host.signer == nil || s.host.trust == nil || s.hostPin == "" || s.clientPin == "" {
		return zero, domain.ErrProbeBinaryStageInput
	}
	ctx, cancel := context.WithTimeout(ctx, binaryStageTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.validateStageRequest(intent); err != nil {
		return zero, err
	}
	session, err := s.openSFTP(ctx, intent)
	if err != nil {
		return zero, err
	}
	defer session.join()
	obs, err := inspectBinaryStageSFTP(ctx, &stageRW{r: session.stdout, w: session.stdin}, intent, session.budget.exceeded)
	session.join()
	if cerr := ctx.Err(); cerr != nil {
		return zero, cerr
	}
	if session.budget.exceeded() {
		return zero, domain.ErrProbeBinaryStageInspection
	}
	if err != nil {
		return zero, err
	}
	return obs, nil
}

func (s *SSHBinaryStageInspector) validateStageRequest(intent domain.ProbeBinaryStageIntent) error {
	if subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(s.host.signer.PublicKey())), []byte(s.clientPin)) != 1 {
		return domain.ErrProbeSSHInput
	}
	if !intent.Valid() {
		return domain.ErrProbeBinaryStageInput
	}
	if _, _, _, err := stagePaths(intent); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(intent.Identity.HostKeyFingerprint), []byte(s.hostPin)) != 1 ||
		subtle.ConstantTimeCompare([]byte(intent.Identity.ClientKeyFingerprint), []byte(s.clientPin)) != 1 {
		return domain.ErrProbeSSHInput
	}
	return nil
}

type stageRW struct {
	r io.Reader
	w io.Writer
}

func (s *stageRW) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stageRW) Write(p []byte) (int, error) { return s.w.Write(p) }

type stageSession struct {
	conn         net.Conn
	client       *ssh.Client
	session      *ssh.Session
	stdin        io.WriteCloser
	stdout       io.Reader
	stop         func() bool
	cancelJoined chan struct{}
	wg           sync.WaitGroup
	once         sync.Once
	budget       *stderrLimit
}

func (s *stageSession) join() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.session != nil {
			_ = s.session.Close()
		}
		if s.client != nil {
			_ = s.client.Close()
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.wg.Wait()
		if s.client != nil {
			_ = s.client.Wait()
		}
		if s.stop != nil && !s.stop() {
			<-s.cancelJoined
		}
	})
}

func (s *SSHBinaryStageInspector) openSFTP(ctx context.Context, intent domain.ProbeBinaryStageIntent) (out *stageSession, err error) {
	conn, err := s.host.connect(ctx, intent.Identity.Host.Address)
	if err != nil {
		return nil, err
	}
	ss := &stageSession{conn: conn, cancelJoined: make(chan struct{}), budget: &stderrLimit{close: conn.Close}}
	defer func() {
		if err != nil {
			ss.join()
		}
	}()
	ss.stop = context.AfterFunc(ctx, func() {
		defer close(ss.cancelJoined)
		_ = conn.Close()
	})
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, domain.ErrProbeSSHConnect
	}
	trustRejected := false
	config := &ssh.ClientConfig{
		User:              intent.Identity.Host.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(s.host.signer)},
		HostKeyAlgorithms: s.host.hostKeyAlgorithms(intent.Identity.Host.Address),
		HostKeyCallback: func(name string, addr net.Addr, key ssh.PublicKey) error {
			if err := s.host.trust(name, addr, key); err != nil {
				trustRejected = true
				return domain.ErrProbeSSHTrust
			}
			return nil
		},
	}
	cc, channels, requests, err := ssh.NewClientConn(conn, intent.Identity.Host.Address, config)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if trustRejected {
			return nil, domain.ErrProbeSSHTrust
		}
		return nil, domain.ErrProbeSSHConnect
	}
	ss.client = ssh.NewClient(cc, channels, requests)
	ss.session, err = ss.client.NewSession()
	if err != nil {
		return nil, stagePublic(ctx, err)
	}
	ss.stdin, err = ss.session.StdinPipe()
	if err != nil {
		return nil, stagePublic(ctx, err)
	}
	ss.stdout, err = ss.session.StdoutPipe()
	if err != nil {
		return nil, stagePublic(ctx, err)
	}
	stderr, err := ss.session.StderrPipe()
	if err != nil {
		return nil, stagePublic(ctx, err)
	}
	ss.wg.Add(1)
	go func() {
		defer ss.wg.Done()
		discardStderr(stderr, ss.budget)
	}()
	if err = ss.session.RequestSubsystem(sftpSubsystem); err != nil {
		return nil, stagePublic(ctx, err)
	}
	return ss, nil
}

type stderrLimit struct {
	mu    sync.Mutex
	n     int
	over  bool
	close func() error
}

func (s *stderrLimit) add(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += n
	if s.n > binaryStageStderrMax && !s.over {
		s.over = true
		if s.close != nil {
			_ = s.close()
		}
	}
}

func (s *stderrLimit) exceeded() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.over
}

func discardStderr(r io.Reader, lim *stderrLimit) {
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			lim.add(n)
			if lim.exceeded() {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func stagePaths(intent domain.ProbeBinaryStageIntent) (dir, marker, binary string, err error) {
	if !intent.Valid() {
		return "", "", "", domain.ErrProbeBinaryStageInput
	}
	body := intent.Marker()
	if len(body) == 0 || len(body) > domain.MaxProbeBinaryStageMarkerSize || intent.MarkerSHA256() == "" {
		return "", "", "", domain.ErrProbeBinaryStageInput
	}
	dir = intent.Directory()
	wantDir := "/opt/uptime-phoenix-probe-" + intent.Identity.OperationID
	name := intent.Identity.Binary.ReleaseName()
	if dir != wantDir || name == "" {
		return "", "", "", domain.ErrProbeBinaryStageInput
	}
	return dir, dir + "/" + domain.ProbeBinaryStageMarkerName, dir + "/" + name, nil
}

func inspectBinaryStageSFTP(ctx context.Context, rw io.ReadWriter, intent domain.ProbeBinaryStageIntent, exceeded func() bool) (domain.ProbeBinaryStageObservation, error) {
	var zero domain.ProbeBinaryStageObservation
	if ctx == nil || rw == nil {
		return zero, domain.ErrProbeBinaryStageInput
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	dir, marker, binary, err := stagePaths(intent)
	if err != nil {
		return zero, err
	}
	pipe := &sftpPipe{ctx: ctx, r: rw, w: rw, exceeded: exceeded}
	if err := pipe.init(); err != nil {
		return zero, stagePublic(ctx, err)
	}
	state, err := pipe.requireDir(stageOptPath)
	if err != nil || state != "" {
		return stageObservation(ctx, intent, state, err)
	}
	state, err = pipe.requireDir(dir)
	if err != nil || state != "" {
		return stageObservation(ctx, intent, state, err)
	}
	body := intent.Marker()
	matched, err := pipe.readPinned(marker, int64(len(body)), intent.MarkerSHA256())
	if err != nil || !matched {
		return stageObservation(ctx, intent, domain.ProbeBinaryStageConflict, err)
	}
	matched, err = pipe.readPinned(binary, intent.Identity.Binary.Size, intent.Identity.Binary.SHA256)
	if err != nil || !matched {
		return stageObservation(ctx, intent, domain.ProbeBinaryStageConflict, err)
	}
	return stageObservation(ctx, intent, domain.ProbeBinaryStageContentMatches, nil)
}

func stageObservation(ctx context.Context, intent domain.ProbeBinaryStageIntent, state string, err error) (domain.ProbeBinaryStageObservation, error) {
	var zero domain.ProbeBinaryStageObservation
	if ctx != nil && ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err != nil {
		return zero, stagePublic(ctx, err)
	}
	obs := domain.ProbeBinaryStageObservation{Intent: intent, State: state}
	if !obs.Valid() || obs.Intent != intent {
		return zero, domain.ErrProbeBinaryStageInspection
	}
	return obs, nil
}

func stagePublic(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	switch err {
	case nil, context.Canceled, context.DeadlineExceeded,
		domain.ErrProbeBinaryStageInput, domain.ErrProbeBinaryStageInspection,
		domain.ErrProbeSSHInput, domain.ErrProbeSSHTrust, domain.ErrProbeSSHDestination, domain.ErrProbeSSHConnect:
		return err
	default:
		return domain.ErrProbeBinaryStageInspection
	}
}

type sftpPipe struct {
	ctx      context.Context
	r        io.Reader
	w        io.Writer
	id       uint32
	exceeded func() bool
}

type fileAttrs struct {
	size    uint64
	mode    uint32
	hasSize bool
	hasMode bool
}

type lstatResult struct {
	attrs fileAttrs
	code  uint32
	ok    bool
}

func (p *sftpPipe) init() error {
	if err := p.writePacket(sftpInit, appendU32(nil, 3)); err != nil {
		return err
	}
	typ, payload, err := p.readPacket()
	if err != nil {
		return err
	}
	if typ != sftpVersion {
		return errBinaryStage
	}
	ver, rest, ok := takeUint32(payload)
	if !ok || ver != 3 {
		return errBinaryStage
	}
	pairs := 0
	for len(rest) > 0 {
		if pairs == binaryStageExtMax {
			return errBinaryStage
		}
		_, rest, ok = takeString(rest)
		if !ok {
			return errBinaryStage
		}
		_, rest, ok = takeString(rest)
		if !ok {
			return errBinaryStage
		}
		pairs++
	}
	return nil
}

func (p *sftpPipe) requireDir(path string) (string, error) {
	res, err := p.lstat(path)
	if err != nil {
		return "", err
	}
	if !res.ok {
		if res.code == sftpFXNoSuchFile {
			return domain.ProbeBinaryStageAbsent, nil
		}
		return "", errBinaryStage
	}
	if !res.attrs.hasMode || res.attrs.mode&posixIFMT != posixIFDIR {
		return domain.ProbeBinaryStageConflict, nil
	}
	return "", nil
}

func (p *sftpPipe) readPinned(path string, size int64, digest string) (bool, error) {
	if size <= 0 || size > domain.MaxProbeBinarySize || len(digest) != 64 {
		return false, errBinaryStage
	}
	res, err := p.lstat(path)
	if err != nil {
		return false, err
	}
	if !res.ok {
		if res.code == sftpFXNoSuchFile {
			return false, nil
		}
		return false, errBinaryStage
	}
	if !res.attrs.hasMode || res.attrs.mode&posixIFMT != posixIFREG {
		return false, nil
	}
	if !res.attrs.hasSize {
		return false, errBinaryStage
	}
	if res.attrs.size != uint64(size) {
		return false, nil
	}
	handle, err := p.openRead(path)
	if err != nil {
		return false, err
	}
	matched, rerr := p.confirmAndRead(handle, size, digest)
	cerr := p.closeHandle(handle)
	if p.ctx.Err() != nil {
		return false, p.ctx.Err()
	}
	if rerr != nil {
		return false, rerr
	}
	if cerr != nil {
		return false, cerr
	}
	return matched, nil
}

func (p *sftpPipe) confirmAndRead(handle []byte, size int64, digest string) (bool, error) {
	attrs, err := p.fstat(handle)
	if err != nil {
		return false, err
	}
	if !attrs.hasMode || attrs.mode&posixIFMT != posixIFREG {
		return false, nil
	}
	if !attrs.hasSize {
		return false, errBinaryStage
	}
	if attrs.size != uint64(size) {
		return false, nil
	}
	return p.readExact(handle, size, digest)
}

func (p *sftpPipe) readExact(handle []byte, size int64, digest string) (bool, error) {
	sum := sha256.New()
	var hashed int64
	for hashed <= size {
		if err := p.ctx.Err(); err != nil {
			return false, err
		}
		n := size + 1 - hashed
		if n > binaryStageReadMax {
			n = binaryStageReadMax
		}
		if n <= 0 {
			break
		}
		data, eof, err := p.readChunk(handle, uint64(hashed), uint32(n))
		if err != nil {
			return false, err
		}
		if eof {
			if hashed != size {
				return false, nil
			}
			break
		}
		if int64(len(data)) > n {
			return false, errBinaryStage
		}
		if _, err := sum.Write(data); err != nil {
			return false, errBinaryStage
		}
		hashed += int64(len(data))
		if hashed > size {
			return false, nil
		}
	}
	if hashed != size {
		return false, nil
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(digest)) != 1 {
		return false, nil
	}
	return true, nil
}

func (p *sftpPipe) lstat(path string) (lstatResult, error) {
	typ, rest, err := p.exchange(sftpLstat, appendString([]byte(path)))
	if err != nil {
		return lstatResult{}, err
	}
	return p.attrsOrStatus(typ, rest)
}

func (p *sftpPipe) fstat(handle []byte) (fileAttrs, error) {
	typ, rest, err := p.exchange(sftpFstat, appendString(handle))
	if err != nil {
		return fileAttrs{}, err
	}
	if typ != sftpAttrs {
		return fileAttrs{}, errBinaryStage
	}
	attrs, rest, err := parseAttrs(rest)
	if err != nil || len(rest) != 0 {
		return fileAttrs{}, errBinaryStage
	}
	return attrs, nil
}

func (p *sftpPipe) openRead(path string) ([]byte, error) {
	body := appendString([]byte(path))
	body = appendU32(body, sftpFXFRead)
	body = appendU32(body, 0)
	typ, rest, err := p.exchange(sftpOpen, body)
	if err != nil {
		return nil, err
	}
	if typ != sftpHandle {
		return nil, errBinaryStage
	}
	return parseHandle(rest)
}

func (p *sftpPipe) closeHandle(handle []byte) error {
	typ, rest, err := p.exchange(sftpClose, appendString(handle))
	if err != nil {
		return err
	}
	if typ != sftpStatus {
		return errBinaryStage
	}
	code, err := parseStatus(rest)
	if err != nil || code != sftpFXOK {
		return errBinaryStage
	}
	return nil
}

func (p *sftpPipe) readChunk(handle []byte, offset uint64, n uint32) ([]byte, bool, error) {
	if n == 0 || n > binaryStageReadMax {
		return nil, false, errBinaryStage
	}
	body := appendString(handle)
	body = appendU64(body, offset)
	body = appendU32(body, n)
	typ, rest, err := p.exchange(sftpRead, body)
	if err != nil {
		return nil, false, err
	}
	switch typ {
	case sftpData:
		data, err := parseData(rest, int(n))
		return data, false, err
	case sftpStatus:
		code, err := parseStatus(rest)
		if err != nil {
			return nil, false, err
		}
		if code == sftpFXEOF {
			return nil, true, nil
		}
		return nil, false, errBinaryStage
	default:
		return nil, false, errBinaryStage
	}
}

func (p *sftpPipe) attrsOrStatus(typ byte, rest []byte) (lstatResult, error) {
	switch typ {
	case sftpAttrs:
		attrs, rest, err := parseAttrs(rest)
		if err != nil || len(rest) != 0 {
			return lstatResult{}, errBinaryStage
		}
		return lstatResult{attrs: attrs, ok: true}, nil
	case sftpStatus:
		code, err := parseStatus(rest)
		if err != nil {
			return lstatResult{}, err
		}
		return lstatResult{code: code}, nil
	default:
		return lstatResult{}, errBinaryStage
	}
}

func (p *sftpPipe) exchange(typ byte, body []byte) (byte, []byte, error) {
	if p.id == ^uint32(0) {
		return 0, nil, errBinaryStage
	}
	p.id++
	payload := appendU32(nil, p.id)
	payload = append(payload, body...)
	if err := p.writePacket(typ, payload); err != nil {
		return 0, nil, err
	}
	got, rest, err := p.readPacket()
	if err != nil {
		return 0, nil, err
	}
	id, rest, ok := takeUint32(rest)
	if !ok || id != p.id {
		return 0, nil, errBinaryStage
	}
	return got, rest, nil
}

func (p *sftpPipe) writePacket(typ byte, payload []byte) error {
	if !allowedStageRequest(typ) || len(payload) > binaryStagePacketMax-1 {
		return errBinaryStage
	}
	if err := p.preIO(); err != nil {
		return err
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(1+len(payload)))
	hdr[4] = typ
	if n, err := p.w.Write(hdr[:]); err != nil {
		return p.ioErr(err)
	} else if n != len(hdr) {
		return errBinaryStage
	}
	if len(payload) > 0 {
		if n, err := p.w.Write(payload); err != nil {
			return p.ioErr(err)
		} else if n != len(payload) {
			return errBinaryStage
		}
	}
	return nil
}

func (p *sftpPipe) readPacket() (byte, []byte, error) {
	if err := p.preIO(); err != nil {
		return 0, nil, err
	}
	var hdr [4]byte
	if err := p.readFull(hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > binaryStagePacketMax {
		return 0, nil, errBinaryStage
	}
	buf := make([]byte, n)
	if err := p.readFull(buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func (p *sftpPipe) readFull(buf []byte) error {
	if err := p.preIO(); err != nil {
		return err
	}
	_, err := io.ReadFull(p.r, buf)
	return p.ioErr(err)
}

func (p *sftpPipe) preIO() error {
	if p.ctx != nil && p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.exceeded != nil && p.exceeded() {
		return errBinaryStage
	}
	return nil
}

func (p *sftpPipe) ioErr(err error) error {
	if err == nil {
		return nil
	}
	if p.ctx != nil && p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.exceeded != nil && p.exceeded() {
		return errBinaryStage
	}
	return errBinaryStage
}

func allowedStageRequest(typ byte) bool {
	switch typ {
	case sftpInit, sftpOpen, sftpClose, sftpRead, sftpLstat, sftpFstat:
		return true
	default:
		return false
	}
}

func parseAttrs(b []byte) (fileAttrs, []byte, error) {
	flags, rest, ok := takeUint32(b)
	if !ok || flags&^uint32(sftpAttrKnown) != 0 || flags&sftpAttrPerms == 0 {
		return fileAttrs{}, nil, errBinaryStage
	}
	var attrs fileAttrs
	if flags&sftpAttrSize != 0 {
		attrs.size, rest, ok = takeUint64(rest)
		if !ok {
			return fileAttrs{}, nil, errBinaryStage
		}
		attrs.hasSize = true
	}
	if flags&sftpAttrUIDGID != 0 {
		if _, rest, ok = takeUint32(rest); !ok {
			return fileAttrs{}, nil, errBinaryStage
		}
		if _, rest, ok = takeUint32(rest); !ok {
			return fileAttrs{}, nil, errBinaryStage
		}
	}
	attrs.mode, rest, ok = takeUint32(rest)
	if !ok {
		return fileAttrs{}, nil, errBinaryStage
	}
	attrs.hasMode = true
	if flags&sftpAttrACMod != 0 {
		if _, rest, ok = takeUint32(rest); !ok {
			return fileAttrs{}, nil, errBinaryStage
		}
		if _, rest, ok = takeUint32(rest); !ok {
			return fileAttrs{}, nil, errBinaryStage
		}
	}
	return attrs, rest, nil
}

func parseStatus(b []byte) (uint32, error) {
	code, rest, ok := takeUint32(b)
	if !ok {
		return 0, errBinaryStage
	}
	if _, rest, ok = takeString(rest); !ok {
		return 0, errBinaryStage
	}
	if _, rest, ok = takeString(rest); !ok || len(rest) != 0 {
		return 0, errBinaryStage
	}
	return code, nil
}

func parseHandle(b []byte) ([]byte, error) {
	h, rest, ok := takeString(b)
	if !ok || len(h) == 0 || len(h) > binaryStageHandleMax || len(rest) != 0 {
		return nil, errBinaryStage
	}
	out := make([]byte, len(h))
	copy(out, h)
	return out, nil
}

func parseData(b []byte, max int) ([]byte, error) {
	data, rest, ok := takeString(b)
	if !ok || len(rest) != 0 || len(data) == 0 || len(data) > max {
		return nil, errBinaryStage
	}
	return data, nil
}

func takeUint32(b []byte) (uint32, []byte, bool) {
	if len(b) < 4 {
		return 0, b, false
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], true
}

func takeUint64(b []byte) (uint64, []byte, bool) {
	if len(b) < 8 {
		return 0, b, false
	}
	return binary.BigEndian.Uint64(b[:8]), b[8:], true
}

func takeString(b []byte) ([]byte, []byte, bool) {
	if len(b) < 4 {
		return nil, b, false
	}
	n := binary.BigEndian.Uint32(b[:4])
	if n > uint32(len(b)-4) {
		return nil, b, false
	}
	end := 4 + int(n)
	return b[4:end], b[end:], true
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func appendU64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

func appendString(s []byte) []byte {
	return append(appendU32(nil, uint32(len(s))), s...)
}
