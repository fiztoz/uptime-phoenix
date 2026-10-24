package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// SSHProbeBinaryStageCommand is the single fixed remote helper invocation.
// There is no caller command, argument, destination path, shell fragment,
// environment request, PTY, subsystem, forwarding, sudo, password, SSH agent,
// fallback, upload or remote installation of the helper. The SSH server may
// implement exec through root's shell; nothing here claims a direct execve.
//
// The operator must already trust and install the intended helper at this
// exact path and protect its ancestor chain, root's shell/startup
// configuration and the SSH server/account/key authorization. A pinned SSH
// host key authenticates the endpoint only: not those files, the helper
// binary's provenance, a physical host, current database authority or
// permission for a particular journal reservation. No SFTP probe or label is
// substituted for these preconditions, and root/namespace metadata is not a
// hostile-root or mount fence.
const SSHProbeBinaryStageCommand = "/usr/local/bin/phoenix-probe-admin stage-binary-host"

// ErrSSHBinaryStageInput reports invalid caller input: a missing or typed-nil
// context, an invalid intent, a non-root SSH user, a typed-nil prepared binary,
// or a pin binding that matches neither the intent nor the configured public
// fingerprints. It is never returned after the uncertainty boundary.
var ErrSSHBinaryStageInput = errors.New("invalid SSH binary staging input")

// ErrSSHBinaryStageUnknown reports every failure after the uncertainty
// boundary: the remote helper may or may not have staged bytes. SSH disconnect
// can itself produce process stdin EOF, so last-byte disconnect may cause
// remote staging. Nothing may be retried, redispatched or deleted remotely
// from this result, and it never claims remote termination.
var ErrSSHBinaryStageUnknown = errors.New("SSH binary staging acknowledgement unknown")

// SSHBinaryStageAck means only: the authenticated endpoint returned one
// complete helper receipt matching this intent, the SSH channel reported a
// normal exit status 0, all local transport and source resources joined, and
// the context was still live. It is not a local seal, independent filesystem
// observation, ownership, artifact_directory application, committed
// reservation, fence, installed state or runtime identity. Its shape can be
// constructed by callers and grants nothing.
type SSHBinaryStageAck struct {
	Intent domain.ProbeBinaryStageIntent
}

const (
	stageSendRootUser   = "root"
	stageSendTimeout    = 2 * time.Minute
	stageSendStdoutMax  = MaxSSHBinaryStageReceiptSize
	stageSendStderrMax  = 4096
	stageSendCopyBuf    = 32 * 1024
	stageSendMaxEmpty   = 100
	stageSendMaxUnwrap  = 32
	stageSendStreamName = "ssh binary stage stream"
)

// Internal stream failures are private diagnostics. Every post-exec failure
// collapses to ErrSSHBinaryStageUnknown, so none of these is ever returned;
// raw channel, library or remote text is never exposed or persisted.
var (
	errStageSendSource   = errors.New(stageSendStreamName + ": source failed")
	errStageSendSize     = errors.New(stageSendStreamName + ": size mismatch")
	errStageSendProgress = errors.New(stageSendStreamName + ": no progress")
	errStageSendOutput   = errors.New(stageSendStreamName + ": output bound exceeded")
	errStageSendInternal = errors.New(stageSendStreamName + ": contract violated")
)

// SSHBinaryStageSender delivers one prepared probe binary snapshot to the
// fixed helper over one-shot pinned SSH exec. It holds only immutable
// per-instance trust state; all transport state is per Send call. The snapshot
// is borrowed: it is never prepared, changed or closed. A future concrete
// executor may call this adapter only after separately solving its actual
// host-authority and fencing protocol; this adapter grants no journal,
// installation or runtime authority and wires into no runner or CLI.
type SSHBinaryStageSender struct {
	insp *SSHBinaryStageInspector
}

// NewSSHBinaryStageSender parses one ephemeral client key, checks the public
// client fingerprint before any network I/O, and retains the unchanged
// fingerprint-only inspector for its validation and host connection. The
// privateKey buffer is borrowed only for parsing and is not retained.
// Credential and trust input failures return the fixed SSH input sentinel.
func NewSSHBinaryStageSender(ctx context.Context, privateKey []byte, hostFingerprint, clientFingerprint string, policy probe.EndpointPolicy) (*SSHBinaryStageSender, error) {
	insp, err := NewSSHBinaryStageInspector(ctx, privateKey, hostFingerprint, clientFingerprint, policy)
	if err != nil {
		return nil, err
	}
	if insp == nil {
		return nil, domain.ErrProbeSSHInput
	}
	return &SSHBinaryStageSender{insp: insp}, nil
}

// Send streams the unchanged header followed by exactly the pinned raw bytes
// and a real source EOF to one fixed exec request, then accepts exactly one
// complete matching receipt with a normal exit status 0.
//
// Before any I/O it requires a live non-typed-nil context, a valid systemd
// intent, SSH user exactly root, a non-typed-nil prepared binary with an exact
// matching Pin(), and both intent pins matching the configured public
// fingerprints. Invalid intent/source/root/pin bindings return the zero ack
// and ErrSSHBinaryStageInput. Pre-exec failures return only the fixed SSH
// input/destination/trust/connect, artifact storage and context sentinels.
//
// The reader is opened before exec so a known unavailable source never starts
// a helper; partial and failed opens are owned and closed exactly once.
// Immediately before Session.Start the call crosses the uncertainty boundary:
// every later failure, including Start, source, close, cancellation, SSH and
// nonzero-exit failures, returns the zero ack and ErrSSHBinaryStageUnknown.
// Cancellation closes the transport before any normal stdin EOF and joins the
// source Read/Close and the cancellation callback; no hard-stop guarantee for
// uncooperative implementations is claimed. All local resources join before
// returning; that is not remote process reaping or remote termination.
func (s *SSHBinaryStageSender) Send(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (SSHBinaryStageAck, error) {
	var zero SSHBinaryStageAck
	if s == nil || s.insp == nil || s.insp.host == nil || s.insp.host.signer == nil ||
		s.insp.host.trust == nil || s.insp.hostPin == "" || s.insp.clientPin == "" ||
		imageSnapshotNil(ctx) || imageSnapshotNil(binary) {
		return zero, ErrSSHBinaryStageInput
	}
	if err := ctx.Err(); err != nil {
		return zero, stageSendBareContext(err)
	}
	if !intent.Valid() || intent.Identity.Host.User != stageSendRootUser {
		return zero, ErrSSHBinaryStageInput
	}
	if err := s.insp.validateStageRequest(intent); err != nil {
		return zero, ErrSSHBinaryStageInput
	}
	pin := binary.Pin()
	if !pin.Valid() || pin != intent.Identity.Binary {
		return zero, ErrSSHBinaryStageInput
	}
	header, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil || len(header) == 0 {
		return zero, ErrSSHBinaryStageInput
	}
	// A fixed two-minute per-call budget, shortened by the caller. This is
	// cooperative local work budgeting, not a remote termination guarantee.
	tctx, cancel := context.WithTimeout(ctx, stageSendTimeout)
	defer cancel()
	call := &stageSendCall{
		ctx:          ctx,
		tctx:         tctx,
		intent:       intent,
		pin:          pin,
		header:       header,
		src:          &stageSendSource{},
		cancelJoined: make(chan struct{}),
		command:      SSHProbeBinaryStageCommand,
	}
	return call.run(s.insp, binary)
}

// stageSendCall owns one Send call's transport and borrowed snapshot stream.
type stageSendCall struct {
	ctx    context.Context
	tctx   context.Context
	intent domain.ProbeBinaryStageIntent
	pin    domain.ProbeBinaryPin
	header []byte
	src    *stageSendSource

	conn         net.Conn
	client       *ssh.Client
	session      *ssh.Session
	stdin        io.WriteCloser
	stdout       *stageSendBounded
	stderr       *stageSendBounded
	stop         func() bool
	cancelJoined chan struct{}

	// Authorized mode only: command is the fixed authorized helper, authorize
	// obtains one grant for the helper's challenge, and nothing is staged
	// until the first stdin byte is attempted.
	command   string
	grantPlan func(keyID string, nonce [32]byte) (func(context.Context) ([]byte, error), error)
	// hostAction, when set, replaces the binary header/payload/receipt with
	// one action frame, no payload and the canonical action receipt.
	hostAction   *domain.ProbeHostActionIntent
	hostReceipt  func(body []byte) bool
	challenge    []byte
	grantFrame   []byte
	stdinStarted bool
	waited       chan struct{}

	attempted bool
	eofSent   bool
	waitOnce  sync.Once
	waitErr   error
	joinOnce  sync.Once
}

// run performs the whole call and joins every local resource before returning.
// The deferred join precedes the caller's observation of the result; the
// context is rechecked only after that join and before a successful ack.
func (c *stageSendCall) run(insp *SSHBinaryStageInspector, binary ports.PreparedProbeBinary) (ack SSHBinaryStageAck, err error) {
	defer func() {
		if closeErr := c.join(); closeErr != nil {
			ack = SSHBinaryStageAck{}
			switch {
			case c.notStaged():
				err = ErrSSHBinaryStageNotStaged
			case c.attempted:
				err = ErrSSHBinaryStageUnknown
			default:
				err = domain.ErrProbeArtifactStorage
			}
			return
		}
		if err != nil {
			ack = SSHBinaryStageAck{}
			switch {
			case c.notStaged():
				// Authorized mode wrote no stdin byte: no stage input exists.
				err = ErrSSHBinaryStageNotStaged
			case c.attempted:
				// Past the uncertainty boundary every failure is unknown.
				err = ErrSSHBinaryStageUnknown
			}
			return
		}
		if c.ctx.Err() != nil || c.tctx.Err() != nil {
			// Context revocation never acknowledges.
			ack = SSHBinaryStageAck{}
			err = ErrSSHBinaryStageUnknown
		}
	}()
	if c.hostAction == nil {
		if err = c.openSource(binary); err != nil {
			return SSHBinaryStageAck{}, err
		}
	}
	if err = c.connect(insp); err != nil {
		return SSHBinaryStageAck{}, err
	}
	if err = c.execute(); err != nil {
		return SSHBinaryStageAck{}, err
	}
	body := c.stdout.snapshot()
	if c.grantPlan != nil {
		// The challenge line is the only output before the receipt.
		if len(c.challenge) == 0 || !bytes.HasPrefix(body, c.challenge) {
			return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
		}
		body = body[len(c.challenge):]
	}
	if c.hostAction != nil {
		if c.hostReceipt == nil || !c.hostReceipt(body) || c.ctx.Err() != nil {
			return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
		}
		return SSHBinaryStageAck{Intent: c.intent}, nil
	}
	parsed, perr := parseSSHBinaryStageAck(c.ctx, body, c.intent)
	if perr != nil {
		return SSHBinaryStageAck{}, perr
	}
	if parsed.Intent != c.intent {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageUnknown
	}
	return parsed, nil
}

// openSource opens the single owned snapshot reader before any remote exec.
// Partial and error readers are owned and closed exactly once. Any pre-exec
// reader Open/Close failure is the bare artifact storage sentinel.
func (c *stageSendCall) openSource(binary ports.PreparedProbeBinary) error {
	reader, err := binary.Open(c.tctx)
	c.src.reader = reader
	if err != nil || imageSnapshotNil(reader) {
		_ = c.src.close()
		if err == nil {
			err = domain.ErrProbeArtifactStorage
		}
		return stageSendBareStorage(err)
	}
	return nil
}

// connect resolves once, dials only a numeric IP, verifies the pinned host key
// before client authentication, and prepares the fixed session streams. The
// cancellation callback closes the transport first so no cleanup can send a
// normal stdin EOF, then closes the owned reader to unblock an active Read.
func (c *stageSendCall) connect(insp *SSHBinaryStageInspector) error {
	conn, err := insp.host.connect(c.tctx, c.intent.Identity.Host.Address)
	if err != nil {
		return stageSendBareTransport(c.ctx, c.tctx, err)
	}
	c.conn = conn
	c.stop = context.AfterFunc(c.tctx, func() {
		defer close(c.cancelJoined)
		_ = conn.Close()
		_ = c.src.close()
	})
	deadline, _ := c.tctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return stageSendBareTransport(c.ctx, c.tctx, domain.ErrProbeSSHConnect)
	}
	trustRejected := false
	config := &ssh.ClientConfig{
		User: c.intent.Identity.Host.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(insp.host.signer)},
		HostKeyCallback: func(name string, addr net.Addr, key ssh.PublicKey) error {
			if err := insp.host.trust(name, addr, key); err != nil {
				trustRejected = true
				return domain.ErrProbeSSHTrust
			}
			return nil
		},
	}
	cc, channels, requests, err := ssh.NewClientConn(conn, c.intent.Identity.Host.Address, config)
	if err != nil {
		if trustRejected {
			return stageSendBareTransport(c.ctx, c.tctx, domain.ErrProbeSSHTrust)
		}
		return stageSendBareTransport(c.ctx, c.tctx, domain.ErrProbeSSHConnect)
	}
	c.client = ssh.NewClient(cc, channels, requests)
	session, err := c.client.NewSession()
	if err != nil {
		return stageSendBareTransport(c.ctx, c.tctx, domain.ErrProbeSSHConnect)
	}
	c.session = session
	if c.stdin, err = session.StdinPipe(); err != nil {
		return stageSendBareTransport(c.ctx, c.tctx, domain.ErrProbeSSHConnect)
	}
	c.stdout = &stageSendBounded{max: stageSendStdoutMax, abort: func() { _ = conn.Close() }}
	if c.grantPlan != nil {
		c.stdout.max += ProbeBinaryStageChallengeLineSize
		c.stdout.line = make(chan struct{})
	}
	c.stderr = &stageSendBounded{max: stageSendStderrMax, abort: func() { _ = conn.Close() }}
	session.Stdout = c.stdout
	session.Stderr = c.stderr
	return nil
}

// execute crosses the uncertainty boundary immediately before the one fixed
// exec request. After full source EOF and a successful owned-reader Close it
// closes SSH stdin to send the channel EOF, then joins the library's stream
// copies through Wait. Wait is not remote process reaping.
func (c *stageSendCall) execute() error {
	if err := c.tctx.Err(); err != nil {
		return stageSendBareContext(err)
	}
	if c.command != SSHProbeBinaryStageCommand && c.command != SSHProbeBinaryStageAuthorizedCommand && c.command != SSHProbeHostActionCommand {
		return ErrSSHBinaryStageInput // fails closed before any exec attempt
	}
	c.attempted = true
	if err := c.session.Start(c.command); err != nil {
		return err
	}
	if c.grantPlan != nil {
		if err := c.authorize(); err != nil {
			return err
		}
	}
	if err := c.stream(); err != nil {
		return err
	}
	if err := c.src.close(); err != nil {
		return err
	}
	if err := c.tctx.Err(); err != nil {
		return err
	}
	if err := c.stdin.Close(); err != nil {
		return err
	}
	c.eofSent = true
	if err := c.wait(); err != nil {
		return err
	}
	if c.stdout.exceeded() || c.stderr.exceeded() {
		return errStageSendOutput
	}
	return nil
}

// stream writes the unchanged header, then exactly the pinned raw bytes and a
// real source EOF. Counts are checked before slicing, empty reads are bounded
// and reset on progress, exact size and SHA256 are verified, and a multi-cause
// EOF is refused by the bounded single-wrapper terminal classifier. There is
// no second source open, body prefetch, compression, trailer,
// LimitReader-generated EOF or generic io.Copy path.
func (c *stageSendCall) stream() error {
	if err := c.tctx.Err(); err != nil {
		return err
	}
	c.stdinStarted = true
	if err := stageSendWriteAll(c.stdin, c.header); err != nil {
		return err
	}
	if c.grantFrame != nil {
		if err := stageSendWriteAll(c.stdin, c.grantFrame); err != nil {
			return err
		}
	}
	if c.hostAction != nil {
		// A host action has no payload: real EOF follows the grant.
		return nil
	}
	hash := sha256.New()
	buf := make([]byte, stageSendCopyBuf)
	var written int64
	empty := 0
	confirmed := false
	for written < c.pin.Size {
		if err := c.tctx.Err(); err != nil {
			return err
		}
		want := c.pin.Size - written
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, readErr := c.src.reader.Read(buf[:want])
		if n < 0 || int64(n) > want {
			return errStageSendInternal
		}
		if n == 0 && readErr == nil {
			empty++
			if empty >= stageSendMaxEmpty {
				return errStageSendProgress
			}
			continue
		}
		empty = 0
		if n > 0 {
			if writeErr := stageSendWriteAll(c.stdin, buf[:n]); writeErr != nil {
				return writeErr
			}
			_, _ = hash.Write(buf[:n])
			written += int64(n)
		}
		if readErr != nil {
			if stageSendTerminal(readErr, io.EOF) {
				confirmed = true
				break
			}
			if stageSendTerminal(readErr, io.ErrUnexpectedEOF) {
				return errStageSendSize
			}
			if err := c.tctx.Err(); err != nil {
				return err
			}
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readErr
			}
			return errStageSendSource
		}
	}
	if written != c.pin.Size {
		return errStageSendSize
	}
	var extra [1]byte
	for attempts := 0; !confirmed && attempts < stageSendMaxEmpty; attempts++ {
		if err := c.tctx.Err(); err != nil {
			return err
		}
		n, readErr := c.src.reader.Read(extra[:])
		if n < 0 || n > len(extra) {
			return errStageSendInternal
		}
		if n > 0 {
			return errStageSendSize
		}
		if readErr == nil {
			continue
		}
		if stageSendTerminal(readErr, io.EOF) {
			confirmed = true
			break
		}
		if stageSendTerminal(readErr, io.ErrUnexpectedEOF) {
			return errStageSendSize
		}
		if err := c.tctx.Err(); err != nil {
			return err
		}
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return readErr
		}
		return errStageSendSource
	}
	if !confirmed {
		return errStageSendProgress
	}
	if hex.EncodeToString(hash.Sum(nil)) != c.pin.SHA256 {
		return errStageSendSource
	}
	return nil
}

// wait runs the SSH library Wait exactly once. It joins the library's stdout
// and stderr stream copies before returning and reports only a normal exit
// status 0 as success; missing, nonzero and signal exits are failures.
func (c *stageSendCall) wait() error {
	c.waitOnce.Do(func() { c.waitErr = c.session.Wait() })
	return c.waitErr
}

// join closes and joins every local transport and source resource exactly
// once. On failure the transport is closed first so cleanup cannot send
// another normal stdin EOF; SSH disconnect can still produce remote stdin EOF
// and the uncertainty is preserved, never resolved. Joining local resources
// does not claim remote termination.
func (c *stageSendCall) join() error {
	c.joinOnce.Do(func() {
		if !c.eofSent && c.conn != nil {
			_ = c.conn.Close()
		}
		if c.session != nil {
			_ = c.session.Close()
		}
		if c.client != nil {
			_ = c.client.Close()
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if !c.eofSent && c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.session != nil {
			_ = c.wait()
		}
		if c.client != nil {
			_ = c.client.Wait()
		}
		if c.stop != nil && !c.stop() {
			<-c.cancelJoined
		}
		_ = c.src.close()
	})
	return c.src.close()
}

// stageSendSource owns the single opened snapshot reader and closes it exactly
// once. Closing must unblock an active Read per the prepared-binary contract;
// callers join a concurrent close through the same once.
type stageSendSource struct {
	reader io.ReadCloser
	once   sync.Once
	err    error
}

func (s *stageSendSource) close() error {
	if s == nil || imageSnapshotNil(s.reader) {
		return nil
	}
	s.once.Do(func() {
		if s.reader.Close() != nil {
			s.err = domain.ErrProbeArtifactStorage
		}
	})
	return s.err
}

// stageSendBounded captures at most max bytes of one remote stream. Excess
// output aborts the transport and fails the call; raw channel diagnostics are
// never returned or persisted.
type stageSendBounded struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	max   int
	over  bool
	abort func()
	// line, when set, is closed once the first LF arrives or output overflows.
	line     chan struct{}
	lineOnce sync.Once
}

func (w *stageSendBounded) signalLine() {
	if w.line != nil {
		w.lineOnce.Do(func() { close(w.line) })
	}
}

func (w *stageSendBounded) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over || len(p) > w.max-w.buf.Len() {
		if !w.over {
			w.over = true
			if w.abort != nil {
				w.abort()
			}
		}
		w.signalLine()
		return 0, errStageSendOutput
	}
	_, _ = w.buf.Write(p)
	if bytes.IndexByte(p, '\n') >= 0 {
		w.signalLine()
	}
	return len(p), nil
}

func (w *stageSendBounded) snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

func (w *stageSendBounded) exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.over
}

// stageSendWriteAll writes the whole slice or fails; it never trusts a short
// write and never reports more bytes than were attempted.
func stageSendWriteAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if n < 0 || n > len(p) {
		return errStageSendInternal
	}
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// stageSendTerminal is the bounded single-wrapper terminal classifier: a
// single wrapping chain may preserve an EOF status, but a multi-cause error
// containing EOF and an independent failure is not successful completion.
func stageSendTerminal(err, terminal error) bool {
	for range stageSendMaxUnwrap {
		if err == nil {
			return false
		}
		if err == terminal {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// stageSendBareContext keeps only the two standard context sentinels; a custom
// context error is invalid input and must not leak private text.
func stageSendBareContext(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrSSHBinaryStageInput
	}
}

// stageSendBareStorage maps a pre-exec owned-reader failure to the bare
// artifact storage sentinel, keeping only the two standard context sentinels.
func stageSendBareStorage(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeArtifactStorage
	}
}

// stageSendBareTransport preserves only the fixed SSH input/destination/trust/
// connect and context sentinels for pre-exec transport failures. A live caller
// or transport cancellation wins over the library error, which may only report
// a closed connection.
func stageSendBareTransport(ctx, tctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return stageSendBareContext(cerr)
	}
	if cerr := tctx.Err(); cerr != nil {
		return stageSendBareContext(cerr)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, domain.ErrProbeSSHInput):
		return domain.ErrProbeSSHInput
	case errors.Is(err, domain.ErrProbeSSHTrust):
		return domain.ErrProbeSSHTrust
	case errors.Is(err, domain.ErrProbeSSHDestination):
		return domain.ErrProbeSSHDestination
	default:
		return domain.ErrProbeSSHConnect
	}
}
