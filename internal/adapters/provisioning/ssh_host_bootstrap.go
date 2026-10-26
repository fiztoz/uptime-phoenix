package provisioning

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Bounds of one bootstrap's SSH steps. The upload carries the helper release
// (tens of MiB); the other steps are small fixed scripts.
const (
	hostBootstrapUploadTimeout = 100 * time.Second
	hostBootstrapStepTimeout   = 30 * time.Second
	hostBootstrapMaxOutput     = 4096
)

// Fixed result lines of the two scripts.
const (
	hostBootstrapUploadDone = "phoenix-host-bootstrap-upload-v1\n"
	hostBootstrapStarted    = "phoenix-host-bootstrap-v1 start\n"
	hostBootstrapResultTag  = "phoenix-host-bootstrap-v1 "
)

// hostBootstrapUploadScript runs as the SSH login user. It refuses a non-Linux
// host or an architecture other than the pinned helper's BEFORE writing
// anything, then receives the helper into a fresh private directory whose name
// the hub chose ($1). mkdir fails on any pre-existing path, so a planted
// directory or symlink is never written through. Nothing here is privileged.
const hostBootstrapUploadScript = `set -eu
LC_ALL=C
export LC_ALL
[ "$(uname -s)" = Linux ] || exit 30
case "$(uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) arch=unknown ;;
esac
[ "$arch" = "$2" ] || exit 31
umask 077
mkdir -m 0700 -- "$1" || exit 32
cat >"$1/helper" || exit 33
printf "phoenix-host-bootstrap-upload-v1\n"
`

// hostBootstrapRootScript runs as root (directly, or via sudo -S). Arguments:
// $1 upload dir, $2 helper SHA-256, $3 stage-authority public key (hex),
// $4 deployment key type, $5 its base64 blob, $6 path prefix ("" in
// production), $7/$8 the owning uid/gid ("0"), $9 the ssh dir ("" = ~root/.ssh).
// Every argument is a hub-built, charset-validated value; none is free text.
//
// Each file is written to a private mktemp file in the destination directory,
// owned and moded, then renamed over the destination (atomic). Unchanged files
// (exact bytes, owner, mode, single link) are left alone, so a rerun is a
// no-op. It never edits or reorders unrelated authorized_keys lines and refuses
// (exit 21) when the deployment key already appears with different options:
// sshd uses the first matching line, so an unrestricted duplicate would defeat
// the forced command. A different stage-authority key is refused (exit 20),
// never replaced. Group/world-writable or non-root-owned destination
// directories and files are refused (exit 22), never "fixed". stdin is the
// remainder of the sudo password line, if any, and is never read.
const hostBootstrapRootScript = `set -eu
exec </dev/null
umask 077
LC_ALL=C
export LC_ALL
src=$1
sum=$2
authority=$3
ktype=$4
kblob=$5
prefix=$6
uid=$7
gid=$8
sshdir=$9
tmp=
cleanup() {
	if [ -n "$tmp" ]; then rm -f -- "$tmp"; fi
	rm -rf -- "$src"
}
trap cleanup EXIT
printf "phoenix-host-bootstrap-v1 start\n"
[ "$(id -u)" = "$uid" ] || exit 10
digest() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum <"$1" | cut -c1-64
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 <"$1" | cut -c1-64
	else openssl dgst -sha256 -r <"$1" | cut -c1-64
	fi
}
present() { [ -e "$1" ] || [ -L "$1" ]; }
exact() {
	[ -n "$(find "$1" -prune -type f -user "$uid" -group "$gid" -perm "$2" -links 1 2>/dev/null)" ]
}
safe() {
	[ -n "$(find "$1" -prune -type "$2" -user "$uid" ! -perm -0020 ! -perm -0002 2>/dev/null)" ]
}
number() {
	case "$1" in
	""|*[!0-9]*) exit 22 ;;
	esac
}
publish() {
	chown "$uid:$gid" "$tmp"
	chmod "$2" "$tmp"
	mv -f -- "$tmp" "$1"
	tmp=
	if command -v restorecon >/dev/null 2>&1; then restorecon -- "$1" >/dev/null 2>&1 || true; fi
}
upload=$src/helper
bindir=$prefix/usr/local/bin
helper=$bindir/phoenix-probe-admin
pdir=$prefix/etc/phoenix-probe
pub=$pdir/stage-authority.pub
if [ -z "$sshdir" ]; then
	home=~root
	case "$home" in
	/*) ;;
	*) exit 22 ;;
	esac
	sshdir=${home%/}/.ssh
fi
ak=$sshdir/authorized_keys
line="restrict,command=\"/usr/local/bin/phoenix-probe-admin authorized-dispatch\" $ktype $kblob"

# Decide everything before changing anything: every refusal below exits
# with the host exactly as it was (apart from the private upload).
[ -n "$(find "$upload" -prune -type f 2>/dev/null)" ] || exit 11
[ "$(digest "$upload")" = "$sum" ] || exit 23
if present "$bindir"; then safe "$bindir" d || exit 22; fi
if [ -d "$helper" ] && [ ! -L "$helper" ]; then exit 22; fi
helper_state=installed
if exact "$helper" 0755 && [ "$(digest "$helper")" = "$sum" ]; then helper_state=unchanged; fi
if present "$pdir"; then safe "$pdir" d || exit 22; fi
authority_state=installed
if present "$pub"; then
	safe "$pub" f || exit 22
	size=$(wc -c <"$pub" | tr -d " ")
	number "$size"
	[ "$size" -eq 65 ] || exit 20
	[ "$(cat -- "$pub")" = "$authority" ] || exit 20
	if exact "$pub" 0644; then authority_state=unchanged; fi
fi
if present "$sshdir"; then safe "$sshdir" d || exit 22; fi
key_state=added
if present "$ak"; then
	safe "$ak" f || exit 22
	blob=$(grep -c -F -e "$kblob" -- "$ak" || true)
	same=$(grep -c -x -F -e "$line" -- "$ak" || true)
	number "$blob"
	number "$same"
	[ "$blob" -eq "$same" ] || exit 21
	if [ "$same" -gt 0 ]; then key_state=unchanged; fi
fi

if [ "$helper_state" = installed ]; then
	if ! present "$bindir"; then
		umask 022
		mkdir -p -- "$bindir"
		umask 077
	fi
	tmp=$(mktemp "$bindir/.phoenix-probe-admin.XXXXXXXX")
	cat -- "$upload" >"$tmp"
	[ "$(digest "$tmp")" = "$sum" ] || exit 23
	publish "$helper" 0755
fi
if [ "$authority_state" = installed ]; then
	if ! present "$pdir"; then
		mkdir -m 0755 -- "$pdir"
		chown "$uid:$gid" "$pdir"
	fi
	tmp=$(mktemp "$pdir/.stage-authority.pub.XXXXXXXX")
	printf "%s\n" "$authority" >"$tmp"
	publish "$pub" 0644
fi
if [ "$key_state" = added ]; then
	if ! present "$sshdir"; then
		mkdir -m 0700 -- "$sshdir"
		chown "$uid:$gid" "$sshdir"
	fi
	tmp=$(mktemp "$sshdir/.authorized_keys.XXXXXXXX")
	if present "$ak"; then
		cat -- "$ak" >"$tmp"
		if [ -s "$ak" ]; then
			last=$(tail -c 1 -- "$ak" | wc -l | tr -d " ")
			number "$last"
			if [ "$last" -eq 0 ]; then printf "\n" >>"$tmp"; fi
		fi
	fi
	printf "%s\n" "$line" >>"$tmp"
	publish "$ak" 0600
fi
printf "phoenix-host-bootstrap-v1 helper=%s authority=%s authorized_key=%s\n" "$helper_state" "$authority_state" "$key_state"
`

// hostBootstrapLayout fixes where the root script writes. Production is the
// documented host layout; only package tests substitute a private prefix and
// their own uid/gid, because a test cannot write /usr/local/bin as root.
type hostBootstrapLayout struct {
	uploadBase string // parent of the private upload directory
	prefix     string // "" in production
	uid, gid   string // "0" in production
	sshDir     string // "" means root's home/.ssh
}

var productionHostBootstrapLayout = hostBootstrapLayout{uploadBase: "/tmp", uid: "0", gid: "0"}

// hostBootstrapArg is the charset every script argument must satisfy, so no
// argument can close the single quotes it is passed in.
var hostBootstrapArg = regexp.MustCompile(`^[A-Za-z0-9+/=@._:-]*$`)

// SSHProbeHostBootstrapper implements ports.ProbeHostBootstrapper over
// golang.org/x/crypto/ssh. It holds the operator artifact directory handle and
// the hub's stage-authority PUBLIC key; it never stores a password, deployment
// key or signer beyond one call.
type SSHProbeHostBootstrapper struct {
	policy    probe.EndpointPolicy
	artifacts *os.Root
	preparer  *ProbeBinaryPreparer
	authority []byte
	layout    hostBootstrapLayout
}

var _ ports.ProbeHostBootstrapper = (*SSHProbeHostBootstrapper)(nil)

// NewSSHProbeHostBootstrapper opens the artifact directory as a rooted handle
// (release names cannot escape it) and captures the canonical stage-authority
// public-key file and a private copy of the destination policy.
func NewSSHProbeHostBootstrapper(artifactDir string, policy probe.EndpointPolicy, authorityPublicKey [32]byte) (*SSHProbeHostBootstrapper, error) {
	if artifactDir == "" || authorityPublicKey == ([32]byte{}) {
		return nil, domain.ErrValidation
	}
	root, err := os.OpenRoot(artifactDir)
	if err != nil {
		return nil, errors.New("probe host bootstrap artifact directory unavailable")
	}
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &SSHProbeHostBootstrapper{
		policy: policy, artifacts: root, preparer: NewProbeHelperBinaryPreparer(""),
		authority: EncodeProbeBinaryStageAuthorityKey(authorityPublicKey), layout: productionHostBootstrapLayout,
	}, nil
}

// Close releases the artifact directory handle.
func (b *SSHProbeHostBootstrapper) Close() error {
	if b == nil || b.artifacts == nil {
		return nil
	}
	return b.artifacts.Close()
}

// Bootstrap performs the one-time host preparation. The host key is verified
// against the pin inside the SSH key exchange, before user authentication, so
// the password is never offered to an unverified host. One password login
// carries every step: upload (as the login user), the root script (directly or
// through sudo -S) and, on any failure after the upload, removal of the upload
// directory. The restricted key is then verified with the deployment key only.
func (b *SSHProbeHostBootstrapper) Bootstrap(ctx context.Context, req domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error) {
	var zero domain.ProbeHostBootstrapResult
	if b == nil || b.artifacts == nil || ctx == nil {
		return zero, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !req.Valid() {
		return zero, domain.ErrProbeSSHInput
	}
	insp, err := NewSSHHostInspector(ctx, req.PrivateKey, SSHHostTrust{Fingerprint: req.HostKeyFingerprint}, b.policy)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, domain.ErrProbeSSHInput
	}
	keyType, keyBlob, ok := authorizedKeyFields(insp.signer.PublicKey())
	if !ok {
		return zero, domain.ErrProbeSSHInput
	}
	prepared, err := b.prepareHelper(ctx, req.Helper)
	if err != nil {
		return zero, err
	}
	defer func() { _ = prepared.Close(context.WithoutCancel(ctx)) }()
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return zero, &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
	}
	upload := strings.TrimSuffix(b.layout.uploadBase, "/") + "/phoenix-bootstrap-" + hex.EncodeToString(token)

	result, err := b.prepareHost(ctx, insp, req, prepared, upload, keyType, keyBlob)
	if err != nil {
		return zero, err
	}
	if err := b.verify(ctx, insp, req); err != nil {
		return zero, err
	}
	result.Verified = true
	return result, nil
}

func (b *SSHProbeHostBootstrapper) prepareHelper(ctx context.Context, pin domain.ProbeBinaryPin) (ports.PreparedProbeBinary, error) {
	input, err := b.artifacts.Open(domain.HelperReleaseName(pin))
	if err != nil {
		return nil, domain.ErrProbeInstallArtifact
	}
	if info, err := input.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = input.Close()
		return nil, domain.ErrProbeInstallArtifact
	}
	prepared, err := b.preparer.PrepareBinary(ctx, pin, input) // consumes input
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, domain.ErrProbeInstallArtifact
	}
	return prepared, nil
}

// prepareHost runs the password-authenticated steps on one connection.
func (b *SSHProbeHostBootstrapper) prepareHost(ctx context.Context, insp *SSHHostInspector, req domain.ProbeHostBootstrapRequest, prepared ports.PreparedProbeBinary, upload, keyType, keyBlob string) (domain.ProbeHostBootstrapResult, error) {
	var zero domain.ProbeHostBootstrapResult
	client, closeClient, err := insp.dialPassword(ctx, req.Host, req.Password)
	if errors.Is(err, domain.ErrProbeSSHTrust) && insp.learnHostKeyAlgorithms(ctx, req.Host.Address) {
		client, closeClient, err = insp.dialPassword(ctx, req.Host, req.Password)
	}
	if err != nil {
		return zero, hostBootstrapDialError(ctx, err)
	}
	defer closeClient()

	uploadCommand, ok := hostBootstrapCommand(hostBootstrapUploadScript, "phoenix-host-bootstrap-upload", false, upload, req.Helper.Architecture)
	if !ok {
		return zero, &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
	}
	helper, err := prepared.Open(ctx)
	if err != nil {
		return zero, hostBootstrapContextOr(ctx, domain.ProbeHostBootstrapFailed)
	}
	out, exit, err := runHostBootstrapSession(ctx, client, hostBootstrapUploadTimeout, uploadCommand, helper)
	_ = helper.Close()
	if err != nil || exit != 0 || out != hostBootstrapUploadDone {
		code := domain.ProbeHostBootstrapFailed
		switch {
		case err == nil && exit == 30:
			code = domain.ProbeHostBootstrapOS
		case err == nil && exit == 31:
			code = domain.ProbeHostBootstrapArchitecture
		case err == nil && exit == 32:
			// mkdir refused: the path is not ours, so it is never removed.
		default:
			// Our private upload directory may exist; remove it as its owner.
			b.removeUpload(ctx, client, upload)
		}
		return zero, hostBootstrapContextOr(ctx, code)
	}

	sudo := req.Host.User != "root"
	l := b.layout
	rootCommand, ok := hostBootstrapCommand(hostBootstrapRootScript, "phoenix-host-bootstrap", sudo,
		upload, prepared.Pin().SHA256, strings.TrimSuffix(string(b.authority), "\n"), keyType, keyBlob,
		l.prefix, l.uid, l.gid, l.sshDir)
	if !ok {
		b.removeUpload(ctx, client, upload)
		return zero, &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
	}
	var stdin io.Reader = bytes.NewReader(nil)
	var passwordLine []byte
	if sudo {
		passwordLine = make([]byte, 0, len(req.Password)+1)
		passwordLine = append(append(passwordLine, req.Password...), '\n')
		stdin = bytes.NewReader(passwordLine)
	}
	out, exit, err = runHostBootstrapSession(ctx, client, hostBootstrapStepTimeout, rootCommand, stdin)
	clear(passwordLine)
	started := strings.HasPrefix(out, hostBootstrapStarted)
	if !started {
		// The root script never ran, so its cleanup trap did not either.
		b.removeUpload(ctx, client, upload)
	}
	if err != nil {
		return zero, hostBootstrapContextOr(ctx, domain.ProbeHostBootstrapFailed)
	}
	if exit != 0 || !started {
		return zero, hostBootstrapContextOr(ctx, hostBootstrapRootExit(exit, started, sudo))
	}
	result, ok := parseHostBootstrapResult(strings.TrimPrefix(out, hostBootstrapStarted))
	if !ok {
		return zero, &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapFailed}
	}
	return result, nil
}

// removeUpload is a best-effort removal of the login user's own upload
// directory; it is never reported as success or failure.
func (b *SSHProbeHostBootstrapper) removeUpload(ctx context.Context, client *ssh.Client, upload string) {
	command, ok := hostBootstrapCommand(`rm -rf -- "$1"`, "phoenix-host-bootstrap-cleanup", false, upload)
	if !ok {
		return
	}
	_, _, _ = runHostBootstrapSession(context.WithoutCancel(ctx), client, 10*time.Second, command, bytes.NewReader(nil))
}

// verify proves the restricted entry with the deployment key alone: root login
// through the forced authorized-dispatch command running the fixed,
// nonmutating install-preflight, which must report root and a loadable
// stage-authority key.
func (b *SSHProbeHostBootstrapper) verify(ctx context.Context, insp *SSHHostInspector, req domain.ProbeHostBootstrapRequest) error {
	report, err := insp.InspectInstallHost(ctx, domain.ProbeSSHHost{Address: req.Host.Address, User: "root"})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var refusal *domain.ProbeInstallPreflightError
		code := domain.ProbeHostBootstrapVerifyHelper
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case domain.ProbeInstallPreflightAuth:
				code = domain.ProbeHostBootstrapVerifyAuth
			case domain.ProbeInstallPreflightTrust:
				code = domain.ProbeHostBootstrapTrust
			case domain.ProbeInstallPreflightDestination:
				code = domain.ProbeHostBootstrapDestination
			case domain.ProbeInstallPreflightUnreachable:
				code = domain.ProbeHostBootstrapUnreachable
			}
		}
		return &domain.ProbeHostBootstrapError{Code: code}
	}
	switch {
	case report.Kernel != "Linux" || domain.ProbeInstallHostArchitecture(report.Machine) != req.Helper.Architecture || !report.Root:
		return &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapVerifyHelper}
	case !report.Authority:
		return &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapVerifyAuthority}
	}
	return nil
}

// authorizedKeyFields returns the public key's OpenSSH type and base64 blob.
func authorizedKeyFields(key ssh.PublicKey) (string, string, bool) {
	fields := strings.Fields(string(ssh.MarshalAuthorizedKey(key)))
	if len(fields) != 2 || fields[0] != key.Type() || !hostBootstrapArg.MatchString(fields[0]) || !hostBootstrapArg.MatchString(fields[1]) {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// hostBootstrapCommand renders `[sudo -S --] /bin/sh -c '<script>' name args…`.
// The script and every argument are hub-built constants or validated values
// that cannot contain a single quote; anything else refuses.
func hostBootstrapCommand(script, name string, sudo bool, args ...string) (string, bool) {
	if strings.Contains(script, "'") || !hostBootstrapArg.MatchString(name) {
		return "", false
	}
	var b strings.Builder
	if sudo {
		b.WriteString("sudo -S -- ")
	}
	b.WriteString("/bin/sh -c '" + script + "' " + name)
	for _, arg := range args {
		if !hostBootstrapArg.MatchString(arg) {
			return "", false
		}
		b.WriteString(" '" + arg + "'")
	}
	return b.String(), true
}

// hostBootstrapRootExit maps the root step's exit status. Without the start
// marker the script never ran: through sudo that is a privilege failure (wrong
// password, no sudo rights, requiretty).
func hostBootstrapRootExit(exit int, started, sudo bool) string {
	if !started {
		if sudo {
			return domain.ProbeHostBootstrapPrivilege
		}
		return domain.ProbeHostBootstrapFailed
	}
	switch exit {
	case 10:
		return domain.ProbeHostBootstrapPrivilege
	case 11, 23:
		return domain.ProbeHostBootstrapTransfer
	case 20:
		return domain.ProbeHostBootstrapAuthorityClash
	case 21:
		return domain.ProbeHostBootstrapKeyClash
	case 22:
		return domain.ProbeHostBootstrapUnsafeLayout
	}
	return domain.ProbeHostBootstrapFailed
}

// parseHostBootstrapResult accepts exactly the one canonical result line.
func parseHostBootstrapResult(text string) (domain.ProbeHostBootstrapResult, bool) {
	var zero domain.ProbeHostBootstrapResult
	line, ok := strings.CutPrefix(text, hostBootstrapResultTag)
	if !ok || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		return zero, false
	}
	fields := strings.Split(strings.TrimSuffix(line, "\n"), " ")
	if len(fields) != 3 {
		return zero, false
	}
	helper, ok1 := strings.CutPrefix(fields[0], "helper=")
	authority, ok2 := strings.CutPrefix(fields[1], "authority=")
	key, ok3 := strings.CutPrefix(fields[2], "authorized_key=")
	result := domain.ProbeHostBootstrapResult{Helper: helper, StageAuthority: authority, AuthorizedKey: key, Verified: true}
	if !ok1 || !ok2 || !ok3 || !result.Valid() {
		return zero, false
	}
	result.Verified = false
	return result, true
}

func hostBootstrapContextOr(ctx context.Context, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return &domain.ProbeHostBootstrapError{Code: code}
}

func hostBootstrapDialError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	code := domain.ProbeHostBootstrapUnreachable
	switch {
	case errors.Is(err, domain.ErrProbeSSHTrust):
		code = domain.ProbeHostBootstrapTrust
	case errors.Is(err, domain.ErrProbeSSHDestination):
		code = domain.ProbeHostBootstrapDestination
	case errors.Is(err, errSSHAuthRejected):
		code = domain.ProbeHostBootstrapAuth
	}
	return &domain.ProbeHostBootstrapError{Code: code}
}

// dialPassword verifies the host key against the pin during key exchange and
// only then authenticates with the password (password, or a single echo-off
// keyboard-interactive prompt). The returned close function closes the client
// and the context watcher. The password slice is only borrowed: the callbacks
// read it during authentication and the client never references it again.
func (s *SSHHostInspector) dialPassword(ctx context.Context, host domain.ProbeSSHHost, password []byte) (*ssh.Client, func(), error) {
	if s == nil || s.trust == nil || !host.Valid() || len(password) == 0 {
		return nil, nil, domain.ErrProbeSSHInput
	}
	dialCtx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	conn, err := s.connect(dialCtx, host.Address)
	if err != nil {
		return nil, nil, err
	}
	stopDial := context.AfterFunc(dialCtx, func() { _ = conn.Close() })
	deadline, _ := dialCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	trustRejected, trusted := false, false
	answered := false
	config := &ssh.ClientConfig{
		User: host.User,
		Auth: []ssh.AuthMethod{
			ssh.PasswordCallback(func() (string, error) { return string(password), nil }),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
				if len(questions) == 0 {
					return nil, nil
				}
				if answered || len(questions) != 1 || len(echos) != 1 || echos[0] {
					return nil, errors.New("unsupported keyboard-interactive challenge")
				}
				answered = true
				return []string{string(password)}, nil
			}),
		},
		HostKeyAlgorithms: s.hostKeyAlgorithms(host.Address),
		HostKeyCallback: func(name string, addr net.Addr, key ssh.PublicKey) error {
			if err := s.trust(name, addr, key); err != nil {
				trustRejected = true
				return domain.ErrProbeSSHTrust
			}
			trusted = true
			return nil
		},
	}
	cc, channels, requests, err := ssh.NewClientConn(conn, host.Address, config)
	dialFailed := !stopDial()
	if err != nil || dialFailed {
		_ = conn.Close()
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		switch {
		case trustRejected:
			return nil, nil, domain.ErrProbeSSHTrust
		case trusted && err != nil:
			return nil, nil, errSSHAuthRejected
		}
		return nil, nil, domain.ErrProbeSSHConnect
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, channels, requests)
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	var once sync.Once
	return client, func() {
		once.Do(func() {
			stop()
			_ = client.Close()
		})
	}, nil
}

// runHostBootstrapSession runs one fixed command on its own session with a
// bounded stdout capture; stderr (sudo prompts, tool diagnostics) is
// discarded and never surfaces. It returns stdout and the exit status, or an
// error when no exit status was received. Cancellation or the step timeout
// closes the session.
func runHostBootstrapSession(ctx context.Context, client *ssh.Client, timeout time.Duration, command string, stdin io.Reader) (string, int, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, err := client.NewSession()
	if err != nil {
		return "", -1, discoveryError(stepCtx)
	}
	defer func() { _ = session.Close() }()
	stop := context.AfterFunc(stepCtx, func() { _ = session.Close() })
	defer stop()
	output := &discoveryOutput{close: session.Close}
	session.Stdin = stdin
	session.Stdout = boundedBootstrapWriter{output: output}
	session.Stderr = io.Discard
	err = session.Run(command)
	if ctxErr := stepCtx.Err(); ctxErr != nil {
		return "", -1, ctxErr
	}
	output.mu.Lock()
	text, exceeded := output.stdout.String(), output.exceeded
	output.mu.Unlock()
	if exceeded {
		return "", -1, domain.ErrProbeSSHDiscovery
	}
	if err == nil {
		return text, 0, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return text, exitErr.ExitStatus(), nil
	}
	return "", -1, domain.ErrProbeSSHDiscovery
}

type boundedBootstrapWriter struct{ output *discoveryOutput }

func (w boundedBootstrapWriter) Write(p []byte) (int, error) {
	o := w.output
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(p) > hostBootstrapMaxOutput-o.size {
		o.exceeded = true
		_ = o.close()
		return 0, domain.ErrProbeSSHDiscovery
	}
	o.size += len(p)
	return o.stdout.Write(p)
}
