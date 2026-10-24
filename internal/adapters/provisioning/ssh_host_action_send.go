package provisioning

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// SSHProbeHostActionCommand is the single fixed authorized host-action helper
// invocation, under the same operator trust preconditions as the stage helper.
const SSHProbeHostActionCommand = "/usr/local/bin/phoenix-probe-admin stage-host-action-authorized"

// ProbeHostActionAuthorizer obtains one grant for an exact host-action
// request. services.ProbeHostActionAuthorizationService satisfies it.
type ProbeHostActionAuthorizer interface {
	Authorize(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error)
}

// SSHHostActionAck means only that the helper returned the exact canonical
// receipt for this intent with exit 0 and joined local resources.
type SSHHostActionAck struct {
	Intent domain.ProbeHostActionIntent
}

// SendHostAction runs one authorized data_directory, unit_file or service
// action: challenge, one Authorize call, then the action frame and grant
// frame with no payload. Outcomes follow SendAuthorized exactly:
// ErrSSHBinaryStageNotStaged before the first stdin byte,
// ErrSSHBinaryStageUnknown after it.
func (s *SSHBinaryStageSender) SendHostAction(ctx context.Context, intent domain.ProbeHostActionIntent, authorizer ProbeHostActionAuthorizer) (SSHHostActionAck, error) {
	if intent.Kind == domain.ProbeActivationProbeInit {
		// probe_init has its own receipt; SendProbeInit returns its result.
		return SSHHostActionAck{}, ErrSSHBinaryStageInput
	}
	return s.sendHostAction(ctx, intent, authorizer, func(body []byte) bool {
		return ProbeHostActionReceiptMatches(body, intent)
	})
}

// SendProbeInit runs the authorized probe_init action and returns its
// parsed result: public facts plus the one-time enrollment token. Outcomes
// follow SendHostAction. The token exists only in the returned value; the
// caller must pass it straight to enrollment and never store or log it.
func (s *SSHBinaryStageSender) SendProbeInit(ctx context.Context, intent domain.ProbeHostActionIntent, authorizer ProbeHostActionAuthorizer) (ProbeInitResult, error) {
	if intent.Kind != domain.ProbeActivationProbeInit {
		return ProbeInitResult{}, ErrSSHBinaryStageInput
	}
	var result ProbeInitResult
	ack, err := s.sendHostAction(ctx, intent, authorizer, func(body []byte) bool {
		parsed, ok := ParseProbeInitReceipt(body, intent)
		if ok {
			result = parsed
		}
		return ok
	})
	if err != nil || ack.Intent != intent || !result.Valid() {
		if err == nil {
			err = ErrSSHBinaryStageUnknown
		}
		return ProbeInitResult{}, err
	}
	return result, nil
}

func (s *SSHBinaryStageSender) sendHostAction(ctx context.Context, intent domain.ProbeHostActionIntent, authorizer ProbeHostActionAuthorizer, receipt func([]byte) bool) (SSHHostActionAck, error) {
	var zero SSHHostActionAck
	if imageSnapshotNil(authorizer) || s == nil || s.insp == nil || s.insp.host == nil || s.insp.host.signer == nil ||
		s.insp.host.trust == nil || s.insp.hostPin == "" || s.insp.clientPin == "" || imageSnapshotNil(ctx) {
		return zero, ErrSSHBinaryStageInput
	}
	if err := ctx.Err(); err != nil {
		return zero, stageSendBareContext(err)
	}
	if !intent.Valid() {
		return zero, ErrSSHBinaryStageInput
	}
	stage := hostActionStageIntent(intent)
	if err := s.insp.validateStageRequest(stage); err != nil {
		return zero, ErrSSHBinaryStageInput
	}
	frame, err := EncodeProbeHostActionFrame(intent)
	if err != nil {
		return zero, ErrSSHBinaryStageInput
	}
	tctx, cancel := context.WithTimeout(ctx, stageSendTimeout)
	defer cancel()
	action := intent
	call := &stageSendCall{
		ctx:          ctx,
		tctx:         tctx,
		intent:       stage,
		header:       frame,
		src:          &stageSendSource{},
		cancelJoined: make(chan struct{}),
		command:      SSHProbeHostActionCommand,
		grantPlan:    hostActionGrantPlan(intent, authorizer),
		hostAction:   &action,
		hostReceipt:  receipt,
		waited:       make(chan struct{}),
	}
	ack, err := call.run(s.insp, nil)
	if err != nil || ack.Intent != stage {
		if err == nil {
			err = ErrSSHBinaryStageUnknown
		}
		return zero, err
	}
	return SSHHostActionAck{Intent: intent}, nil
}

func hostActionGrantPlan(intent domain.ProbeHostActionIntent, authorizer ProbeHostActionAuthorizer) func(string, [32]byte) (func(context.Context) ([]byte, error), error) {
	return func(keyID string, nonce [32]byte) (func(context.Context) ([]byte, error), error) {
		request := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
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
			return EncodeProbeHostActionGrantFrame(grant)
		}, nil
	}
}
