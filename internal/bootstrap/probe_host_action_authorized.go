package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// probeHostActionAuthorizedUsage is the only text for a non-empty argument list.
const probeHostActionAuthorizedUsage = "Usage: phoenix-probe-admin stage-host-action-authorized\nNo flags, paths, or arguments. Linux uid 0 only; trusted key " + ProbeBinaryStageAuthorityKeyPath + ".\nEmits one challenge line, then applies one image_cache, data_directory, unit_file, service, container, probe_init, service_start or cleanup action only under a verified one-shot grant.\nThis is NOT installation, enrollment, or continuous host authority.\n"

const (
	// probeHostActionImageCacheTimeout bounds an image_cache call end to end:
	// streaming the closed image archive and the Docker load. It matches the
	// hub sender's image_cache timeout. Every other action keeps the standard
	// probeBinaryStageHelperTimeout, which also bounds reading its frame.
	probeHostActionImageCacheTimeout = 15 * time.Minute
	// probeImagePayloadChunk is the largest read taken from the payload stream.
	probeImagePayloadChunk = 64 * 1024
)

// probeHostActionApply performs one verified host action. stagedArchive is the
// path of the verified, root-only staged image archive for image_cache and ""
// for every other kind. Definite refusals before any effect return Input,
// Conflict or Unavailable; anything after a possible effect returns Unknown. A
// nil receipt means the standard host_action_applied line; probe_init returns
// its own receipt, which is the only place its one-time enrollment token ever
// appears.
type probeHostActionApply func(ctx context.Context, intent domain.ProbeHostActionIntent, stagedArchive string) ([]byte, error)

// probeHostActionWithoutReceipt adapts an error-only action that takes no
// staged archive to the standard receipt; it ignores any staged path.
func probeHostActionWithoutReceipt(apply func(context.Context, domain.ProbeHostActionIntent) error) probeHostActionApply {
	if apply == nil {
		return nil
	}
	return func(ctx context.Context, intent domain.ProbeHostActionIntent, _ string) ([]byte, error) {
		return nil, apply(ctx, intent)
	}
}

// probeStagedArchive is one exclusive, root-only staged image archive. Close
// releases the data descriptor (the file stays on disk for docker load);
// Remove deletes it, is idempotent and releases everything.
type probeStagedArchive interface {
	io.Writer
	Sync() error
	Close() error
	Path() string
	Remove() error
}

// probeImageStager creates the exclusive staging file for one image_cache
// attempt. It runs only after the action's grant has been verified.
type probeImageStager func(ctx context.Context, intent domain.ProbeHostActionIntent) (probeStagedArchive, error)

// RunAuthorizedProbeHostActionHelper applies one authorized host action. The
// prerequisites, challenge, host binding and grant check match
// RunAuthorizedProbeBinaryStageHelper; the grant is a host-action grant (its
// own signing domain), stdin then carries real EOF (after the verified image
// payload, for image_cache only), and the action runs only after input is
// closed. Exit 0 is the only acknowledgement of the receipt.
func RunAuthorizedProbeHostActionHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	return runAuthorizedProbeHostActionHelperStaged(ctx, args, input, out, stderr,
		loadHostProbeBinaryStageAuthority, loadHostProbeBinaryStageHostKeys, rand.Reader, applyHostProbeAction, stageHostProbeImage)
}

// runAuthorizedProbeHostActionHelper is the helper with the host's production
// image stager; actions that stream no payload never touch it.
func runAuthorizedProbeHostActionHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer,
	loadKey func(context.Context) ([32]byte, error), hostKeys func(context.Context) ([]string, error), entropy io.Reader, apply probeHostActionApply,
) int {
	return runAuthorizedProbeHostActionHelperStaged(ctx, args, input, out, stderr, loadKey, hostKeys, entropy, apply, stageHostProbeImage)
}

func runAuthorizedProbeHostActionHelperStaged(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer,
	loadKey func(context.Context) ([32]byte, error), hostKeys func(context.Context) ([]string, error), entropy io.Reader, apply probeHostActionApply,
	stage probeImageStager,
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
		probeBinaryStageHelperNil(loadKey) || probeBinaryStageHelperNil(hostKeys) || probeBinaryStageHelperNil(entropy) || probeBinaryStageHelperNil(apply) ||
		probeBinaryStageHelperNil(stage) {
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
	// opCtx is the ceiling for an image_cache call. Every other action runs
	// under headCtx, the standard budget, which also cuts off a stalled sender
	// by closing the input; image_cache disarms that cut-off once its grant
	// has verified, so the payload may stream for as long as opCtx allows.
	opCtx, cancel := context.WithTimeout(ctx, probeHostActionImageCacheTimeout)
	defer cancel()
	headCtx, headCancel := context.WithTimeout(opCtx, probeBinaryStageHelperTimeout)
	defer headCancel()
	guard, err := newProbeBinaryStageHelperInput(opCtx, ownedInput)
	if err != nil {
		return probeBinaryStageHelperRefuse(ownedInput, stderr, err)
	}
	defer func() { _ = guard.Close() }()
	stopHead := context.AfterFunc(headCtx, func() { guard.closeRaw() })
	defer stopHead()
	// A canceled helper must never emit a challenge: a sender that sees one
	// consumes its one-shot admission.
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, probeBinaryStageHelperContextError(err))
	}
	if err := writeProbeBinaryStageHelperReceipt(out, challenge); err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	// The staged archive exists only between a verified payload and the end of
	// apply; every return path removes it.
	var staged probeStagedArchive
	removeStaged := func() error {
		s := staged
		staged = nil
		if s == nil {
			return nil
		}
		return s.Remove()
	}
	defer func() { _ = removeStaged() }()
	receive := func(intent domain.ProbeHostActionIntent) error {
		if !stopHead() {
			return probeBinaryStageHelperContextError(headCtx.Err())
		}
		var err error
		staged, err = receiveProbeHostActionPayload(opCtx, guard, intent, stage)
		return err
	}
	intent, err := readAuthorizedProbeHostAction(headCtx, guard, key, nonce, fingerprints, receive)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	applyCtx, stagedPath := headCtx, ""
	if intent.Kind == domain.ProbeHostActionImageCache {
		if probeBinaryStageHelperNil(staged) || staged.Path() == "" {
			return probeBinaryStageHelperBeforeReceive(guard, stderr, domain.ErrProbeBinaryStageWriteInput)
		}
		applyCtx, stagedPath = opCtx, staged.Path()
	}
	// Input is closed before any effect: nothing more can be read.
	if err := guard.Close(); err != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeArtifactStorage)
	}
	if err := applyCtx.Err(); err != nil {
		return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperContextError(err))
	}
	receipt, err := apply(applyCtx, intent, stagedPath)
	// The staged archive is gone before any receipt, so a receipt never
	// outlives its payload; a file that cannot be removed leaves the outcome
	// unconfirmed rather than silently leaking the archive.
	if removeErr := removeStaged(); removeErr != nil && err == nil {
		err = domain.ErrProbeBinaryStageWriteUnknown
	}
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
	if ctx.Err() != nil || applyCtx.Err() != nil {
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
// this host and verifies the host-action grant for this process's nonce. Only
// then does the stream continue: an image_cache intent hands the stream to
// payload (which must consume the PHXIMG1 payload and require real EOF), and
// every other kind, or a nil payload, requires real EOF immediately. Nothing
// is applied here, and an unverified grant never reaches payload.
func readAuthorizedProbeHostAction(ctx context.Context, r io.Reader, key, nonce [32]byte, fingerprints []string,
	payload func(domain.ProbeHostActionIntent) error,
) (domain.ProbeHostActionIntent, error) {
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
	if intent.Kind == domain.ProbeHostActionImageCache && payload != nil {
		if err := payload(intent); err != nil {
			return zero, err
		}
		return intent, nil
	}
	if err := probeHostActionRequireEOF(ctx, r); err != nil {
		return zero, err
	}
	return intent, nil
}

// receiveProbeHostActionPayload reads the PHXIMG1 prefix (its size must equal
// the intent's payload size), streams exactly that many bytes into a new
// staging file while hashing them, requires the SHA-256 to equal the
// intent's payload digest and then real EOF, and syncs and closes the file.
// Any mismatch, short read or trailing byte is a definite refusal and the
// file is removed before the error returns; on success the caller owns the
// staged archive and must Remove it. It runs only after the grant verified.
func receiveProbeHostActionPayload(ctx context.Context, r io.Reader, intent domain.ProbeHostActionIntent, stage probeImageStager) (probeStagedArchive, error) {
	st := probeBinaryStageHeaderRead{ctx: ctx, r: r}
	prefix := make([]byte, provisioning.ProbeImagePayloadPrefixSize)
	if err := st.exact(prefix); err != nil {
		return nil, err
	}
	size, err := provisioning.ProbeImagePayloadSize(prefix, intent.Payload.Size)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	staged, err := stage(ctx, intent)
	if err != nil {
		return nil, err
	}
	if probeBinaryStageHelperNil(staged) {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	ok := false
	defer func() {
		if !ok {
			_ = staged.Remove()
		}
	}()
	digest, err := probeHostActionStreamPayload(ctx, r, staged, size)
	if err != nil {
		return nil, err
	}
	if digest != intent.Payload.SHA256 {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := probeHostActionRequireEOF(ctx, r); err != nil {
		return nil, err
	}
	if err := staged.Sync(); err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := staged.Close(); err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	ok = true
	return staged, nil
}

// probeHostActionStreamPayload copies exactly size bytes from r to dst and
// returns their SHA-256 in lowercase hex. It never reads beyond size, so a
// following real-EOF check sees any trailing byte; a stream that ends early is
// an Input refusal and a dst write failure is Unavailable.
func probeHostActionStreamPayload(ctx context.Context, r io.Reader, dst io.Writer, size int64) (string, error) {
	hash := sha256.New()
	buf := make([]byte, probeImagePayloadChunk)
	empty := 0
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return "", probeBinaryStageHelperContextError(err)
		}
		want := min(int64(len(buf)), remaining)
		n, err := r.Read(buf[:want])
		if n < 0 || int64(n) > want {
			return "", domain.ErrProbeArtifactStorage
		}
		if n > 0 {
			empty = 0
			_, _ = hash.Write(buf[:n])
			if written, werr := dst.Write(buf[:n]); werr != nil || written != n {
				return "", domain.ErrProbeBinaryStageWriteUnavailable
			}
			remaining -= int64(n)
		} else if err == nil {
			empty++
			if empty >= probeBinaryStageHelperMaxEmptyReads {
				return "", domain.ErrProbeArtifactStorage
			}
			continue
		}
		if err == nil || (remaining == 0 && probeBinaryStageHelperSingle(err, io.EOF)) {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", probeBinaryStageHelperContextError(cerr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", probeBinaryStageHelperContextError(err)
		}
		if probeBinaryStageHelperSingle(err, io.EOF) || probeBinaryStageHelperSingle(err, io.ErrUnexpectedEOF) {
			return "", domain.ErrProbeBinaryStageWriteInput
		}
		return "", probeBinaryStageHelperPreError(err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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

// applyProbeDockerHostAction routes one verified Docker-mode intent to its
// applier. The data directory is the same applier the systemd layout uses; every
// other kind runs docker, which is resolved only then (newDocker) so a host
// action that never runs docker does not need the CLI. withVarLib opens the
// protected /var/lib parent for one call. stagedArchive is the image_cache
// payload path and must be empty for every other kind. Only probe_init returns
// its own receipt.
func applyProbeDockerHostAction(ctx context.Context, intent domain.ProbeHostActionIntent, stagedArchive string,
	newDocker func() (provisioning.ProbeDocker, error), withVarLib func(func(*os.File) error) error,
) ([]byte, error) {
	if intent.Identity.Mode != domain.ProbeInstallDocker || (intent.Kind == domain.ProbeHostActionImageCache) != (stagedArchive != "") {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		return nil, withVarLib(func(parent *os.File) error {
			return provisioning.ApplyProbeDataDirectory(ctx, parent, intent)
		})
	case domain.ProbeHostActionImageCache, domain.ProbeHostActionContainer, domain.ProbeActivationProbeInit,
		domain.ProbeActivationServiceStart, domain.ProbeHostActionCleanup:
	default:
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	docker, err := newDocker()
	if err != nil || probeBinaryStageHelperNil(docker) {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	switch intent.Kind {
	case domain.ProbeHostActionImageCache:
		return nil, provisioning.ApplyProbeImageCache(ctx, stagedArchive, intent, docker)
	case domain.ProbeHostActionContainer:
		return nil, provisioning.ApplyProbeContainer(ctx, intent, docker)
	case domain.ProbeActivationServiceStart:
		return nil, provisioning.ApplyProbeDockerStart(ctx, intent, docker)
	case domain.ProbeActivationProbeInit:
		var receipt []byte
		err := withVarLib(func(data *os.File) error {
			result, err := provisioning.ApplyProbeDockerInit(ctx, data, intent, docker)
			if err != nil {
				return err
			}
			if receipt = provisioning.EncodeProbeInitReceipt(intent, result); receipt == nil {
				return domain.ErrProbeBinaryStageWriteUnknown
			}
			return nil
		})
		return receipt, err
	default: // cleanup
		return nil, withVarLib(func(data *os.File) error {
			return provisioning.ApplyProbeDockerCleanup(ctx, data, intent, docker)
		})
	}
}
