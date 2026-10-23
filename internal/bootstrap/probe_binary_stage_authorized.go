package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeBinaryStageAuthorityKeyPath is the fixed trusted public-key file the
// authorized helper verifies grants against. The operator installs it; its
// root ownership and mode are checked, but its provenance is not.
const ProbeBinaryStageAuthorityKeyPath = "/etc/phoenix-probe/stage-authority.pub"

// probeBinaryStageHostKeyNames are the sshd host public keys, under /etc/ssh,
// whose fingerprints bind a grant's intent to this host. A host whose sshd
// uses other HostKey paths refuses every intent: it fails closed.
var probeBinaryStageHostKeyNames = []string{"ssh_host_ed25519_key.pub", "ssh_host_ecdsa_key.pub", "ssh_host_rsa_key.pub"}

// probeBinaryStageAuthorizedUsage is the only text emitted for a non-empty
// argument list. It is exit 2, stdout stays empty, and arguments are never echoed.
const probeBinaryStageAuthorizedUsage = "Usage: phoenix-probe-admin stage-binary-authorized\nNo flags, paths, or arguments. Linux uid 0 only; trusted key " + ProbeBinaryStageAuthorityKeyPath + "; fixed root-owned /opt parent.\nEmits one challenge line, then stages only under a verified one-shot grant.\nThis is NOT installation, enrollment, cleanup, or continuous host authority.\n"

// RunAuthorizedProbeBinaryStageHelper is the host-local launcher with a
// one-shot challenge/grant check. Before reading stdin it loads the trusted
// authority key, acquires the fixed parent and emits one canonical challenge
// line naming that authority and a fresh process nonce. Stdin is the stage
// header, one grant frame, then the raw pinned binary. The grant must verify
// under the trusted key for exactly this nonce and the header's intent marker
// before any payload byte is consumed or anything is created; otherwise the
// definite pre-creation refusal is reported.
//
// The intent's pinned SSH host-key fingerprint must also name one of this
// host's own sshd host keys, so a grant relayed from another host's attempt is
// refused even when both hosts trust the same authority.
//
// The nonce makes a grant usable by at most this one process. A verified grant
// shows that the authority signed this exact attempt once; it is not continuous
// authority, a host-identity proof beyond the sender's pinned SSH host key,
// installation, enrollment, cleanup or proof of remote termination.
func RunAuthorizedProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	return runAuthorizedProbeBinaryStageHelper(ctx, args, input, out, stderr, loadHostProbeBinaryStageAuthority, openHostProbeBinaryStageParent, rand.Reader, loadHostProbeBinaryStageHostKeys)
}

// runAuthorizedProbeBinaryStageHelper injects key loading, parent acquisition
// and entropy so refusal paths are testable without a filesystem.
func runAuthorizedProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer,
	loadKey func(context.Context) ([32]byte, error), acquire func(context.Context) (*os.File, error), entropy io.Reader,
	hostKeys func(context.Context) ([]string, error),
) int {
	if len(args) != 0 {
		if err := probeBinaryStageHelperCloseInput(input); err != nil {
			return probeBinaryStageHelperExit(stderr, err)
		}
		if !probeBinaryStageHelperNil(stderr) {
			_, _ = io.WriteString(stderr, probeBinaryStageAuthorizedUsage)
		}
		return 2
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(stderr) ||
		probeBinaryStageHelperNil(loadKey) || probeBinaryStageHelperNil(acquire) || probeBinaryStageHelperNil(entropy) || probeBinaryStageHelperNil(hostKeys) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(err))
	}
	// Every local prerequisite is checked before the challenge, so a sender
	// never consumes a one-shot DB admission for a helper that cannot stage.
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
	parent, err := acquire(ctx)
	if err != nil || parent == nil {
		if parent != nil {
			if cerr := parent.Close(); cerr != nil {
				return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeArtifactStorage)
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(cerr))
		}
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHostAcquireError(err))
	}
	ownedInput, err := inheritedProbeBinaryStageInput(ctx, input)
	if err != nil {
		if parent.Close() != nil {
			err = domain.ErrProbeArtifactStorage
		}
		return probeBinaryStageHelperExit(stderr, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = parent.Close()
		}
	}()
	// The challenge is the only stdout before the receipt. A failed write
	// leaves nothing staged: no grant can reach this process afterwards.
	if err := ctx.Err(); err != nil {
		return probeBinaryStageAuthorizedRefuse(parent, &closed, ownedInput, stderr, probeBinaryStageHelperContextError(err))
	}
	if err := writeProbeBinaryStageHelperReceipt(out, challenge); err != nil {
		return probeBinaryStageAuthorizedRefuse(parent, &closed, ownedInput, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	authorize := func(ctx context.Context, intent domain.ProbeBinaryStageIntent, r io.Reader) error {
		// Checked before the grant frame is read: an intent pinned to another
		// host's key is refused without consuming anything further.
		if !probeBinaryStageHostOwnsIntent(fingerprints, intent) {
			return domain.ErrProbeBinaryStageWriteUnauthorized
		}
		return verifyProbeBinaryStageGrant(ctx, r, key, nonce, intent)
	}
	var buf, diagnostic bytes.Buffer
	code := runProbeBinaryStageHelperEntry(ctx, nil, parent, ownedInput, &buf, &diagnostic, authorize)
	closeErr := parent.Close()
	closed = true
	return finishInheritedProbeBinaryStageHelper(ctx, code, closeErr, buf.Bytes(), diagnostic.String(), out, stderr)
}

// probeBinaryStageAuthorizedRefuse closes the owned input and parent before
// any stdin byte is read. A close failure is the louder storage fact.
func probeBinaryStageAuthorizedRefuse(parent *os.File, closed *bool, input io.ReadCloser, stderr io.Writer, reason error) int {
	*closed = true
	if parent.Close() != nil {
		reason = domain.ErrProbeArtifactStorage
	}
	return probeBinaryStageHelperRefuse(input, stderr, reason)
}

// verifyProbeBinaryStageGrant reads exactly one grant frame and verifies its
// signature over this process's nonce, the trusted authority and the decoded
// intent's marker. Only the fixed frame is consumed; the payload is untouched.
// A short stream is an input refusal; a bad frame or signature is the definite
// pre-creation Unauthorized refusal.
func verifyProbeBinaryStageGrant(ctx context.Context, r io.Reader, key, nonce [32]byte, intent domain.ProbeBinaryStageIntent) error {
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(r) || key == ([32]byte{}) || nonce == ([32]byte{}) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	frame := make([]byte, provisioning.ProbeBinaryStageGrantFrameSize)
	st := probeBinaryStageHeaderRead{ctx: ctx, r: r}
	if err := st.exact(frame); err != nil {
		return err
	}
	signature, err := provisioning.DecodeProbeBinaryStageGrantFrame(frame)
	if err != nil {
		return domain.ErrProbeBinaryStageWriteUnauthorized
	}
	grant := domain.ProbeBinaryStageGrant{
		Request: domain.ProbeBinaryStageAdmissionRequest{
			Intent: intent,
			Challenge: domain.ProbeBinaryStageChallenge{
				AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(key),
				Nonce:          nonce,
				MarkerSHA256:   intent.MarkerSHA256(),
			},
		},
		Signature: signature,
	}
	if !grant.ValidFor(key) {
		return domain.ErrProbeBinaryStageWriteUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperContextError(err)
	}
	return nil
}

// probeBinaryStageHostOwnsIntent reports whether the intent's pinned SSH host
// key is one of this host's own. Empty fingerprints never match.
func probeBinaryStageHostOwnsIntent(fingerprints []string, intent domain.ProbeBinaryStageIntent) bool {
	want := intent.Identity.HostKeyFingerprint
	return want != "" && slices.Contains(fingerprints, want)
}

// probeBinaryStageHostKeyFingerprints parses bounded sshd public-key file
// bodies into SHA256 fingerprints. Any malformed body refuses the whole set.
func probeBinaryStageHostKeyFingerprints(bodies [][]byte) ([]string, error) {
	out := make([]string, 0, len(bodies))
	for _, body := range bodies {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(body)
		if err != nil || len(bytes.TrimSpace(rest)) != 0 {
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
		out = append(out, ssh.FingerprintSHA256(key))
	}
	if len(out) == 0 {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	return out, nil
}

// probeBinaryStageAuthorityReadKey reads one bounded key file body and parses
// it. A larger file is refused rather than truncated.
func probeBinaryStageAuthorityReadKey(r io.Reader) ([32]byte, error) {
	body := make([]byte, provisioning.ProbeBinaryStageAuthorityKeyFileSize+1)
	n, err := io.ReadFull(r, body)
	if n > provisioning.ProbeBinaryStageAuthorityKeyFileSize || (err != nil && !errors.Is(err, io.ErrUnexpectedEOF)) {
		return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable
	}
	key, err := provisioning.ParseProbeBinaryStageAuthorityKey(body[:n])
	if err != nil {
		return [32]byte{}, domain.ErrProbeBinaryStageWriteUnavailable
	}
	return key, nil
}
