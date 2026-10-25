package provisioning

import (
	"context"
	"crypto/subtle"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
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
	if intent.Kind == domain.ProbeActivationProbeInit || intent.Kind == domain.ProbeHostActionImageCache {
		// probe_init has its own receipt (SendProbeInit); image_cache streams
		// a payload (SendImageCache).
		return SSHHostActionAck{}, ErrSSHBinaryStageInput
	}
	return s.sendHostAction(ctx, intent, authorizer, nil, func(body []byte) bool {
		return ProbeHostActionReceiptMatches(body, intent)
	})
}

// SendImageCache runs one authorized image_cache action: challenge, one
// Authorize call, the action frame and grant frame, then the PHXIMG1 prefix
// and exactly the closed archive's bytes. The archive must be the one the
// intent's payload names (SHA-256, size, layout index) for exactly the
// intent's image selection. Outcomes follow SendHostAction.
func (s *SSHBinaryStageSender) SendImageCache(ctx context.Context, intent domain.ProbeHostActionIntent, archive ports.PreparedProbeImageArchive, authorizer ProbeHostActionAuthorizer) (SSHHostActionAck, error) {
	if intent.Kind != domain.ProbeHostActionImageCache || imageSnapshotNil(archive) {
		return SSHHostActionAck{}, ErrSSHBinaryStageInput
	}
	d := archive.Descriptor()
	if !d.Valid() || d.Content.Selection != intent.Identity.Image || d.ArchiveSHA256 != intent.Payload.SHA256 ||
		d.ArchiveSize != intent.Payload.Size || d.LayoutIndexSHA256 != intent.Payload.LayoutIndexSHA256 {
		return SSHHostActionAck{}, ErrSSHBinaryStageInput
	}
	return s.sendHostAction(ctx, intent, authorizer, archive, func(body []byte) bool {
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
	ack, err := s.sendHostAction(ctx, intent, authorizer, nil, func(body []byte) bool {
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

func (s *SSHBinaryStageSender) sendHostAction(ctx context.Context, intent domain.ProbeHostActionIntent, authorizer ProbeHostActionAuthorizer, payload ports.PreparedProbeImageArchive, receipt func([]byte) bool) (SSHHostActionAck, error) {
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
	if err := s.insp.validateHostActionRequest(intent); err != nil {
		return zero, ErrSSHBinaryStageInput
	}
	var prefix []byte
	pin := domain.ProbeBinaryPin{}
	timeout := stageSendTimeout
	if payload != nil {
		if prefix = EncodeProbeImagePayloadPrefix(intent.Payload.Size); prefix == nil {
			return zero, ErrSSHBinaryStageInput
		}
		// The stream loop checks exact size and digest against pin.
		pin = domain.ProbeBinaryPin{SHA256: intent.Payload.SHA256, Size: intent.Payload.Size}
		timeout = stageSendImageTimeout
	}
	frame, err := EncodeProbeHostActionFrame(intent)
	if err != nil {
		return zero, ErrSSHBinaryStageInput
	}
	attempt := func() (SSHBinaryStageAck, error) {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		action := intent
		call := &stageSendCall{
			ctx:           ctx,
			tctx:          tctx,
			intent:        stage,
			pin:           pin,
			header:        frame,
			src:           &stageSendSource{},
			cancelJoined:  make(chan struct{}),
			command:       SSHProbeHostActionCommand,
			grantPlan:     hostActionGrantPlan(intent, authorizer),
			hostAction:    &action,
			payloadPrefix: prefix,
			hostReceipt:   receipt,
			waited:        make(chan struct{}),
		}
		if payload != nil {
			return call.run(s.insp, payload)
		}
		return call.run(s.insp, nil)
	}
	ack, err := s.withHostKeyRetry(ctx, intent.Identity.Host.Address, attempt)
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

// validateHostActionRequest binds a host-action intent to this sender: the
// parsed client key must be the pinned one, and the intent's host and client
// fingerprints must be the sender's pins. It works for every install mode.
func (s *SSHBinaryStageInspector) validateHostActionRequest(intent domain.ProbeHostActionIntent) error {
	if subtle.ConstantTimeCompare([]byte(ssh.FingerprintSHA256(s.host.signer.PublicKey())), []byte(s.clientPin)) != 1 {
		return domain.ErrProbeSSHInput
	}
	if !intent.Valid() {
		return domain.ErrProbeBinaryStageInput
	}
	if subtle.ConstantTimeCompare([]byte(intent.Identity.HostKeyFingerprint), []byte(s.hostPin)) != 1 ||
		subtle.ConstantTimeCompare([]byte(intent.Identity.ClientKeyFingerprint), []byte(s.clientPin)) != 1 {
		return domain.ErrProbeSSHInput
	}
	return nil
}
