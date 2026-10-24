package provisioning

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SSHProbeBinaryStageAuthorizedCommand is the single fixed authorized helper
// invocation. The same operator trust preconditions as
// SSHProbeBinaryStageCommand apply, plus the host's trusted authority key.
const SSHProbeBinaryStageAuthorizedCommand = "/usr/local/bin/phoenix-probe-admin stage-binary-authorized"

// stageSendAuthorizeMinBudget is the least remaining per-call budget with
// which the sender will consume a one-shot admission: enough for the bounded
// authorization call plus a typical payload stream. Less refuses NotStaged.
const stageSendAuthorizeMinBudget = 30 * time.Second

// ErrSSHBinaryStageNotStaged reports an authorized-mode failure after exec but
// before the first stdin byte was attempted: no header, grant or payload was
// sent, so no helper can have staged anything. It covers challenge failures,
// authorization refusals and lost authorization acknowledgements. A consumed
// one-shot DB admission is not replenished; a new attempt is required.
var ErrSSHBinaryStageNotStaged = errors.New("SSH binary staging refused before any stage input was sent")

// ProbeBinaryStageAuthorizer obtains one grant for an exact admission request.
// services.ProbeBinaryStageAuthorizationService satisfies it: each call
// irreversibly consumes the request's journal attempt before signing.
type ProbeBinaryStageAuthorizer interface {
	Authorize(ctx context.Context, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error)
}

// SendAuthorized is Send against the authorized helper. After exec it waits
// for the helper's one canonical challenge line, binds it to the intent's
// marker, obtains exactly one grant from authorizer, and only then streams the
// unchanged header, the grant frame and the pinned bytes. Pre-exec validation
// and failures are as for Send. Failures after exec but before the first stdin
// byte return ErrSSHBinaryStageNotStaged; every later failure is
// ErrSSHBinaryStageUnknown. An acknowledgement additionally requires the
// challenge line to precede the matching receipt. Authorization is never
// retried; the authorizer is called at most once.
func (s *SSHBinaryStageSender) SendAuthorized(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary, authorizer ProbeBinaryStageAuthorizer) (SSHBinaryStageAck, error) {
	if imageSnapshotNil(authorizer) {
		return SSHBinaryStageAck{}, ErrSSHBinaryStageInput
	}
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
	attempt := func() (SSHBinaryStageAck, error) {
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
			command:      SSHProbeBinaryStageAuthorizedCommand,
			grantPlan:    binaryStageGrantPlan(intent, authorizer),
			waited:       make(chan struct{}),
		}
		return call.run(s.insp, binary)
	}
	return s.withHostKeyRetry(ctx, intent.Identity.Host.Address, attempt)
}

// notStaged reports whether an authorized call failed before any stdin byte.
func (c *stageSendCall) notStaged() bool {
	return c.grantPlan != nil && c.attempted && !c.stdinStarted
}

// binaryStageGrantPlan validates the binary-stage request for a challenge and
// returns the single deferred Authorize call that builds the grant frame.
func binaryStageGrantPlan(intent domain.ProbeBinaryStageIntent, authorizer ProbeBinaryStageAuthorizer) func(string, [32]byte) (func(context.Context) ([]byte, error), error) {
	return func(keyID string, nonce [32]byte) (func(context.Context) ([]byte, error), error) {
		request := domain.ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256(),
		}}
		if !request.Valid() {
			return nil, errStageSendInternal
		}
		return func(ctx context.Context) ([]byte, error) {
			grant, err := authorizer.Authorize(ctx, request)
			if err != nil {
				return nil, err
			}
			if grant.Request != request {
				return nil, errStageSendInternal
			}
			return EncodeProbeBinaryStageGrantFrame(grant)
		}, nil
	}
}

// authorize waits for the challenge line, then obtains and encodes one grant.
// Session.Wait runs in the background so a helper that exits without a
// challenge ends the wait; the shared once keeps it a single Wait call.
func (c *stageSendCall) authorize() error {
	go func() {
		defer close(c.waited)
		_ = c.wait()
	}()
	select {
	case <-c.stdout.line:
	case <-c.waited:
	case <-c.tctx.Done():
		return c.tctx.Err()
	}
	if c.stdout.exceeded() {
		return errStageSendOutput
	}
	line := c.stdout.snapshot()
	// The helper writes nothing after the challenge until it has the grant.
	if len(line) != ProbeBinaryStageChallengeLineSize || bytes.IndexByte(line, '\n') != len(line)-1 {
		return errStageSendInternal
	}
	keyID, nonce, err := ParseProbeBinaryStageChallengeLine(line)
	if err != nil {
		return errStageSendInternal
	}
	obtain, err := c.grantPlan(keyID, nonce)
	if err != nil || obtain == nil {
		return errStageSendInternal
	}
	if err := c.tctx.Err(); err != nil {
		return err
	}
	// Never consume a one-shot admission without time left to deliver it.
	if deadline, ok := c.tctx.Deadline(); !ok || time.Until(deadline) < stageSendAuthorizeMinBudget {
		return errStageSendProgress
	}
	// Never consume a one-shot admission for a helper that already exited.
	select {
	case <-c.waited:
		return errStageSendInternal
	default:
	}
	frame, err := obtain(c.tctx)
	if err != nil {
		return err
	}
	if err := c.tctx.Err(); err != nil {
		return err
	}
	// A helper that exited during authorization cannot receive the grant;
	// report it before any stdin byte rather than as a post-write Unknown.
	select {
	case <-c.waited:
		return errStageSendInternal
	default:
	}
	if len(frame) != ProbeBinaryStageGrantFrameSize {
		return errStageSendInternal
	}
	c.challenge = line
	c.grantFrame = frame
	return nil
}
