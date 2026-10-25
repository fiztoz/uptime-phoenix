package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"slices"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeHostActionAuthorizedUsage is the only text for a non-empty argument list.
const probeHostActionAuthorizedUsage = "Usage: phoenix-probe-admin stage-host-action-authorized\nNo flags, paths, or arguments. Linux uid 0 only; trusted key " + ProbeBinaryStageAuthorityKeyPath + ".\nEmits one challenge line, then applies one data_directory, unit_file, service, probe_init, service_start or cleanup action only under a verified one-shot grant.\nThis is NOT installation, enrollment, or continuous host authority.\n"

// probeHostActionApply performs one verified host action. Definite refusals
// before any effect return Input, Conflict or Unavailable; anything after a
// possible effect returns Unknown. A nil receipt means the standard
// host_action_applied line; probe_init returns its own receipt, which is the
// only place its one-time enrollment token ever appears.
type probeHostActionApply func(ctx context.Context, intent domain.ProbeHostActionIntent) ([]byte, error)

// probeHostActionWithoutReceipt adapts an error-only action to the standard
// receipt.
func probeHostActionWithoutReceipt(apply func(context.Context, domain.ProbeHostActionIntent) error) probeHostActionApply {
	if apply == nil {
		return nil
	}
	return func(ctx context.Context, intent domain.ProbeHostActionIntent) ([]byte, error) {
		return nil, apply(ctx, intent)
	}
}

// RunAuthorizedProbeHostActionHelper applies one authorized host action. The
// prerequisites, challenge, host binding and grant check match
// RunAuthorizedProbeBinaryStageHelper; the grant is a host-action grant (its
// own signing domain), stdin then carries real EOF, and the action runs only
// after input is closed. Exit 0 is the only acknowledgement of the receipt.
func RunAuthorizedProbeHostActionHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	return runAuthorizedProbeHostActionHelper(ctx, args, input, out, stderr,
		loadHostProbeBinaryStageAuthority, loadHostProbeBinaryStageHostKeys, rand.Reader, applyHostProbeAction)
}

func runAuthorizedProbeHostActionHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer,
	loadKey func(context.Context) ([32]byte, error), hostKeys func(context.Context) ([]string, error), entropy io.Reader, apply probeHostActionApply,
) int {
	if len(args) != 0 {
		if err := probeBinaryStageHelperCloseInput(input); err != nil {
			return probeBinaryStageHelperExit(stderr, err)
		}
		if !probeBinaryStageHelperNil(stderr) {
			_, _ = io.WriteString(stderr, probeHostActionAuthorizedUsage)
		}
		return 2
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(stderr) ||
		probeBinaryStageHelperNil(loadKey) || probeBinaryStageHelperNil(hostKeys) || probeBinaryStageHelperNil(entropy) || probeBinaryStageHelperNil(apply) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(err))
	}
	key, err := loadKey(ctx)
	if err != nil || key == ([32]byte{}) {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHostAcquireError(err))
	}
	fingerprints, err := hostKeys(ctx)
	if err != nil || len(fingerprints) == 0 {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHostAcquireError(err))
	}
	fingerprints = slices.Clone(fingerprints)
	var nonce [32]byte
	if n, err := io.ReadFull(entropy, nonce[:]); err != nil || n != len(nonce) || nonce == ([32]byte{}) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	challenge := provisioning.EncodeProbeBinaryStageChallengeLine(domain.ProbeBinaryStageAuthorityKeyID(key), nonce)
	if challenge == nil {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	ownedInput, err := inheritedProbeBinaryStageInput(ctx, input)
	if err != nil {
		return probeBinaryStageHelperExit(stderr, err)
	}
	opCtx, cancel := context.WithTimeout(ctx, probeBinaryStageHelperTimeout)
	defer cancel()
	guard, err := newProbeBinaryStageHelperInput(opCtx, ownedInput)
	if err != nil {
		return probeBinaryStageHelperRefuse(ownedInput, stderr, err)
	}
	defer func() { _ = guard.Close() }()
	// A canceled helper must never emit a challenge: a sender that sees one
	// consumes its one-shot admission.
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, probeBinaryStageHelperContextError(err))
	}
	if err := writeProbeBinaryStageHelperReceipt(out, challenge); err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	intent, err := readAuthorizedProbeHostAction(opCtx, guard, key, nonce, fingerprints)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	// Input is closed before any effect: nothing more can be read.
	if err := guard.Close(); err != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeArtifactStorage)
	}
	if err := opCtx.Err(); err != nil {
		return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperContextError(err))
	}
	receipt, err := apply(opCtx, intent)
	if err != nil {
		// Unknown wins: a joined cause is never reported as a definite refusal.
		reason := domain.ErrProbeBinaryStageWriteUnknown
		if !errors.Is(err, domain.ErrProbeBinaryStageWriteUnknown) {
			for _, definite := range []error{domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteUnavailable} {
				if errors.Is(err, definite) {
					reason = definite
					break
				}
			}
		}
		return probeBinaryStageHelperExit(stderr, reason)
	}
	if ctx.Err() != nil || opCtx.Err() != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	if receipt == nil {
		receipt = provisioning.EncodeProbeHostActionReceipt(intent)
	}
	if err := writeProbeBinaryStageHelperReceipt(out, receipt); err != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	return 0
}

// readAuthorizedProbeHostAction reads the action frame, binds the intent to
// this host, verifies the host-action grant for this process's nonce, and
// requires real EOF. Nothing is applied here.
func readAuthorizedProbeHostAction(ctx context.Context, r io.Reader, key, nonce [32]byte, fingerprints []string) (domain.ProbeHostActionIntent, error) {
	var zero domain.ProbeHostActionIntent
	st := probeBinaryStageHeaderRead{ctx: ctx, r: r}
	prefix := make([]byte, provisioning.ProbeHostActionPrefixSize)
	if err := st.exact(prefix); err != nil {
		return zero, err
	}
	n, err := provisioning.ProbeHostActionKindLength(prefix)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	kind := make([]byte, n)
	if err := st.exact(kind); err != nil {
		return zero, err
	}
	docker := provisioning.ProbeHostActionFrameIsDocker(prefix)
	header, err := readProbeHostActionHeader(ctx, r, docker)
	if err != nil {
		return zero, err
	}
	intent, err := provisioning.DecodeProbeHostActionIntent(kind, header, docker)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	if !slices.Contains(fingerprints, intent.Identity.HostKeyFingerprint) || intent.Identity.HostKeyFingerprint == "" {
		return zero, domain.ErrProbeBinaryStageWriteUnauthorized
	}
	frame := make([]byte, provisioning.ProbeBinaryStageGrantFrameSize)
	if err := st.exact(frame); err != nil {
		return zero, err
	}
	signature, err := provisioning.DecodeProbeBinaryStageGrantFrame(frame)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteUnauthorized
	}
	grant := domain.ProbeHostActionGrant{
		Request: domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(key), Nonce: nonce, MarkerSHA256: intent.MarkerSHA256(),
		}},
		Signature: signature,
	}
	if !grant.ValidFor(key) {
		return zero, domain.ErrProbeBinaryStageWriteUnauthorized
	}
	if err := probeHostActionRequireEOF(ctx, r); err != nil {
		return zero, err
	}
	return intent, nil
}

// probeHostActionRequireEOF reads until real EOF; any byte is an input refusal.
func probeHostActionRequireEOF(ctx context.Context, r io.Reader) error {
	var one [1]byte
	for empty := 0; empty < probeBinaryStageHelperMaxEmptyReads; {
		if err := ctx.Err(); err != nil {
			return probeBinaryStageHelperContextError(err)
		}
		n, err := r.Read(one[:])
		if n > 0 {
			return domain.ErrProbeBinaryStageWriteInput
		}
		if err == nil {
			empty++
			continue
		}
		if probeBinaryStageHelperSingle(err, io.EOF) {
			return nil
		}
		return probeBinaryStageHelperPreError(err)
	}
	return domain.ErrProbeArtifactStorage
}

// readProbeHostActionHeader reads the identity header that follows the kind:
// the v1 binary-stage header, or for a v2 frame the Docker header.
func readProbeHostActionHeader(ctx context.Context, r io.Reader, docker bool) ([]byte, error) {
	if !docker {
		return readProbeBinaryStageHeader(ctx, r)
	}
	st := probeBinaryStageHeaderRead{ctx: ctx, r: r}
	prefix := make([]byte, provisioning.ProbeDockerHeaderPrefixSize)
	if err := st.exact(prefix); err != nil {
		return nil, err
	}
	n, err := provisioning.ProbeHostActionHeaderLength(prefix, true)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	header := make([]byte, provisioning.ProbeDockerHeaderPrefixSize+n)
	copy(header, prefix)
	if err := st.exact(header[provisioning.ProbeDockerHeaderPrefixSize:]); err != nil {
		return nil, err
	}
	return header, nil
}
