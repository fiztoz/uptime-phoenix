//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Real-effect lifecycle tests for the authorized host-action helper. Each test
// drives runAuthorizedProbeHostActionHelper end to end like the remote sender
// does: read the challenge from stdout first, sign it with a generated key,
// send the action frame and one grant frame, then close stdin (real EOF). The
// applied effect is the real provisioning.ApplyProbe* code against t.TempDir()
// directories standing in for /var/lib and /etc/systemd/system. systemctl is a
// fake that records its calls; nothing outside t.TempDir() is touched.

// hactFakeSystemctl records every invocation and models only the verbs the
// service action may use. Any other verb (notably start) fails loudly.
type hactFakeSystemctl struct {
	mu      sync.Mutex
	calls   [][]string
	enabled bool
}

func (s *hactFakeSystemctl) Run(_ context.Context, args ...string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(args))
	if len(args) == 0 {
		return "", -1, errors.New("systemctl without a verb")
	}
	switch args[0] {
	case "is-enabled":
		if s.enabled {
			return "enabled\n", 0, nil
		}
		return "disabled\n", 1, nil
	case "daemon-reload":
		return "", 0, nil
	case "enable":
		s.enabled = true
		return "", 0, nil
	}
	return "", -1, errors.New("unexpected systemctl verb " + args[0])
}

func (s *hactFakeSystemctl) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// hactTree is a fake host: a /var/lib stand-in, a /etc/systemd/system
// stand-in and a recording systemctl.
type hactTree struct {
	varLib  string
	unitDir string
	sysctl  *hactFakeSystemctl
}

func newHactTree(t *testing.T) *hactTree {
	t.Helper()
	return &hactTree{varLib: t.TempDir(), unitDir: t.TempDir(), sysctl: &hactFakeSystemctl{}}
}

// apply is the injected action: the real provisioning appliers beneath the
// stand-in directories, as applyHostProbeAction does beneath the real ones.
func (tr *hactTree) apply(ctx context.Context, intent domain.ProbeHostActionIntent) error {
	dir := tr.unitDir
	if intent.Kind == domain.ProbeHostActionDataDirectory {
		dir = tr.varLib
	}
	parent, err := os.Open(dir)
	if err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	defer func() { _ = parent.Close() }()
	switch intent.Kind {
	case domain.ProbeHostActionDataDirectory:
		return provisioning.ApplyProbeDataDirectory(ctx, parent, intent)
	case domain.ProbeHostActionUnitFile:
		return provisioning.ApplyProbeUnitFile(ctx, parent, intent)
	case domain.ProbeHostActionService:
		return provisioning.ApplyProbeService(ctx, parent, intent, tr.sysctl)
	}
	return domain.ErrProbeBinaryStageWriteInput
}

func (tr *hactTree) dataPath(intent domain.ProbeHostActionIntent) string {
	return filepath.Join(tr.varLib, "uptime-phoenix-probe-"+intent.Identity.OperationID)
}

func (tr *hactTree) unitPath(intent domain.ProbeHostActionIntent) string {
	return filepath.Join(tr.unitDir, "uptime-phoenix-probe-"+intent.Identity.OperationID+".service")
}

func hactWantEmptyDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 0 {
			t.Fatalf("host directory is not empty (the helper created something): %v %v", entries, err)
		}
	}
}

func (tr *hactTree) wantEmpty(t *testing.T) {
	t.Helper()
	hactWantEmptyDirs(t, tr.varLib, tr.unitDir)
	if calls := tr.sysctl.recorded(); len(calls) != 0 {
		t.Fatalf("systemctl was invoked: %v", calls)
	}
}

func hactOnlyEntry(t *testing.T, dir, name string) os.FileInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("directory must hold exactly %q, got %v (%v)", name, entries, err)
	}
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func hactWantOwnedByUs(t *testing.T, info os.FileInfo) {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		t.Fatal("entry is not owned by the helper's effective uid")
	}
}

// hactSenderFrames is the sender's view: only the parsed challenge and the
// intent it wants applied.
func hactSenderFrames(t *testing.T, priv ed25519.PrivateKey, keyID string, nonce [32]byte, intent domain.ProbeHostActionIntent) (action, grant []byte) {
	t.Helper()
	request := domain.ProbeHostActionAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
	}
	message := request.SigningMessage()
	if message == nil {
		t.Fatal("sender cannot sign an inadmissible request")
	}
	var sig [64]byte
	copy(sig[:], ed25519.Sign(priv, message))
	frame, err := provisioning.EncodeProbeHostActionGrantFrame(domain.ProbeHostActionGrant{Request: request, Signature: sig})
	if err != nil {
		t.Fatal(err)
	}
	action, err = provisioning.EncodeProbeHostActionFrame(intent)
	if err != nil {
		t.Fatal(err)
	}
	return action, frame
}

type hactRunOptions struct {
	keys     authzKeys // the authority the helper trusts
	nonce    [32]byte  // the deterministic entropy the helper turns into its challenge
	apply    func(context.Context, domain.ProbeHostActionIntent) error
	osPipe   bool // use a real os.Pipe for stdin instead of the counting io.Pipe
	hostKeys func(context.Context) ([]string, error)
}

// hactRun is one helper process: a goroutine running the helper with injected
// dependencies, plus the sender's ends of its stdin and stdout.
type hactRun struct {
	t        *testing.T
	o        hactRunOptions
	cancel   context.CancelFunc
	stdout   *authzStdout
	stderr   bytes.Buffer
	stdin    *authzStdin // nil with osPipe
	pipeIn   *os.File    // helper's end of an osPipe
	w        io.WriteCloser
	done     chan int
	finished bool
	applies  atomic.Int32
}

func startHactRun(t *testing.T, o hactRunOptions) *hactRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &hactRun{t: t, o: o, cancel: cancel, stdout: newAuthzStdout(), done: make(chan int, 1)}
	var input io.ReadCloser
	if o.osPipe {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		r.pipeIn, r.w, input = pr, pw, pr
		t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	} else {
		pr, pw := io.Pipe()
		r.stdin, r.w = &authzStdin{pr: pr}, pw
		input = r.stdin
	}
	loadKey := func(context.Context) ([32]byte, error) { return o.keys.pub, nil }
	hostKeys := authzIntentHost
	if o.hostKeys != nil {
		hostKeys = o.hostKeys
	}
	apply := func(c context.Context, intent domain.ProbeHostActionIntent) error {
		r.applies.Add(1)
		return o.apply(c, intent)
	}
	entropy := bytes.NewReader(append([]byte(nil), o.nonce[:]...))
	go func() {
		r.done <- runAuthorizedProbeHostActionHelper(ctx, nil, input, r.stdout, &r.stderr, loadKey, hostKeys, entropy, probeHostActionWithoutReceipt(apply))
	}()
	t.Cleanup(func() {
		cancel()
		_ = r.w.Close()
		if !r.finished {
			select {
			case <-r.done:
			case <-time.After(authzIOTimeout):
				t.Error("helper goroutine did not stop")
			}
		}
	})
	return r
}

// waitChallenge blocks until the helper emitted its challenge, which it must do
// before reading any stdin, and returns the sender's parsed view of it.
func (r *hactRun) waitChallenge() (keyID string, nonce [32]byte, line []byte) {
	r.t.Helper()
	select {
	case <-r.stdout.ready:
	case code := <-r.done:
		r.finished = true
		r.t.Fatalf("helper exited %d before any challenge; stderr %q", code, r.stderr.String())
	case <-time.After(authzIOTimeout):
		r.t.Fatal("helper never emitted a challenge before reading stdin")
	}
	all := r.stdout.Bytes()
	if len(all) != provisioning.ProbeBinaryStageChallengeLineSize {
		r.t.Fatalf("stdout before any stdin byte is %d bytes, want exactly the challenge line", len(all))
	}
	keyID, nonce, err := provisioning.ParseProbeBinaryStageChallengeLine(all)
	if err != nil {
		r.t.Fatal("challenge is not canonical", err)
	}
	if keyID != r.o.keys.id || nonce != r.o.nonce {
		r.t.Fatal("challenge does not name the trusted authority and the entropy nonce")
	}
	return keyID, nonce, all
}

// write sends data to the helper's stdin, optionally in small chunks. A closed
// pipe is returned, not fatal: a helper that refuses early stops reading.
func (r *hactRun) write(data []byte, chunk int) (int, error) {
	r.t.Helper()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		total := 0
		for len(data) > 0 {
			size := len(data)
			if chunk > 0 && size > chunk {
				size = chunk
			}
			n, err := r.w.Write(data[:size])
			total += n
			if err != nil {
				ch <- result{total, err}
				return
			}
			data = data[size:]
		}
		ch <- result{total, nil}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(authzIOTimeout):
		r.t.Fatal("stdin write blocked: the helper stopped reading without closing stdin")
		return 0, nil
	}
}

// finish closes the sender's stdin (EOF) and waits for the helper's exit code.
func (r *hactRun) finish() int {
	r.t.Helper()
	_ = r.w.Close()
	return r.waitExit()
}

func (r *hactRun) waitExit() int {
	r.t.Helper()
	var code int
	select {
	case code = <-r.done:
		r.finished = true
	case <-time.After(authzIOTimeout):
		r.t.Fatal("helper did not exit")
	}
	r.cancel()
	if r.pipeIn != nil {
		if _, err := r.pipeIn.Stat(); !errors.Is(err, os.ErrClosed) {
			r.t.Fatal("helper did not release its owned stdin", err)
		}
	}
	return code
}

// wantRefused asserts a refusal after the challenge: exit 1, the one sanitized
// stderr line, stdout holding only the challenge, apply never run, and (when
// counted) an exact stdin byte count with a single close.
func (r *hactRun) wantRefused(code int, want error, challenge []byte, consumed int) {
	r.t.Helper()
	if code != 1 || r.stderr.String() != want.Error()+"\n" {
		r.t.Fatalf("code=%d stderr=%q, want exit 1 with %q", code, r.stderr.String(), want.Error())
	}
	if got := r.stdout.Bytes(); !bytes.Equal(got, challenge) {
		r.t.Fatalf("stdout %q, want only the challenge line", got)
	}
	if n := r.applies.Load(); n != 0 {
		r.t.Fatalf("apply ran %d times for a refused grant", n)
	}
	if r.stdin != nil {
		if consumed >= 0 && r.stdin.consumed.Load() != int64(consumed) {
			r.t.Fatalf("helper consumed %d stdin bytes, want exactly %d", r.stdin.consumed.Load(), consumed)
		}
		if r.stdin.closes.Load() != 1 {
			r.t.Fatalf("stdin closed %d times, want 1", r.stdin.closes.Load())
		}
	}
}

// hactApplyOnce runs a complete, valid attempt for intent against tree and
// returns the sender's frames and the helper's challenge line.
func hactApplyOnce(t *testing.T, o hactRunOptions, intent domain.ProbeHostActionIntent, chunk int) (action, grant, challenge []byte, r *hactRun) {
	t.Helper()
	r = startHactRun(t, o)
	keyID, nonce, challenge := r.waitChallenge()
	action, grant = hactSenderFrames(t, o.keys.priv, keyID, nonce, intent)
	if n, err := r.write(hactJoin(action, grant), chunk); err != nil || n != len(action)+len(grant) {
		t.Fatalf("action+grant write n=%d err=%v", n, err)
	}
	return action, grant, challenge, r
}

func hactWantReceiptOnWire(t *testing.T, r *hactRun, challenge []byte, intent domain.ProbeHostActionIntent) {
	t.Helper()
	want := append(append([]byte(nil), challenge...), hactWantReceipt(intent)...)
	if got := r.stdout.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("stdout %q, want challenge line then the receipt line", got)
	}
	if lines := bytes.Count(r.stdout.Bytes(), []byte{'\n'}); lines != 2 {
		t.Fatalf("stdout has %d lines, want 2", lines)
	}
}

func TestRunAuthorizedProbeHostActionHelperCreatesDataDirectoryMode0700UnderVerifiedGrant(t *testing.T) {
	intent := hactIntent(t, domain.ProbeHostActionDataDirectory)
	keys := authzGeneratedKeys(t)
	for name, tc := range map[string]struct {
		chunk  int
		osPipe bool
	}{
		"whole writes":         {0, false},
		"fragmented frames":    {5, false},
		"real os stdin pipe":   {0, true},
		"fragmented real pipe": {3, true},
	} {
		t.Run(name, func(t *testing.T) {
			tree := newHactTree(t)
			o := hactRunOptions{keys: keys, nonce: authzNonce("data/" + name), apply: tree.apply, osPipe: tc.osPipe}
			action, grant, challenge, r := hactApplyOnce(t, o, intent, tc.chunk)

			// Without a real EOF the verified grant still applies nothing.
			select {
			case code := <-r.done:
				r.finished = true
				t.Fatalf("helper exited %d before stdin reached EOF", code)
			case <-time.After(150 * time.Millisecond):
			}
			hactWantEmptyDirs(t, tree.varLib, tree.unitDir)
			if r.applies.Load() != 0 {
				t.Fatal("apply ran before the sender closed stdin")
			}

			if code := r.finish(); code != 0 || r.stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%q", code, r.stderr.String())
			}
			hactWantReceiptOnWire(t, r, challenge, intent)
			if r.applies.Load() != 1 {
				t.Fatalf("apply ran %d times, want exactly once", r.applies.Load())
			}

			info := hactOnlyEntry(t, tree.varLib, "uptime-phoenix-probe-"+intent.Identity.OperationID)
			if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky|os.ModeSymlink) != 0 {
				t.Fatalf("data directory mode %v, want a plain directory with 0700", info.Mode())
			}
			hactWantOwnedByUs(t, info)
			hactWantEmptyDirs(t, tree.dataPath(intent), tree.unitDir)
			if calls := tree.sysctl.recorded(); len(calls) != 0 {
				t.Fatalf("data_directory invoked systemctl: %v", calls)
			}
			if r.stdin != nil {
				if got, want := r.stdin.consumed.Load(), int64(len(action)+len(grant)); got != want || r.stdin.closes.Load() != 1 {
					t.Fatalf("stdin consumed=%d want=%d closes=%d", got, want, r.stdin.closes.Load())
				}
			}
		})
	}
}

func TestRunAuthorizedProbeHostActionHelperWritesExactUnitFileMode0644UnderVerifiedGrant(t *testing.T) {
	intent := hactIntent(t, domain.ProbeHostActionUnitFile)
	keys := authzGeneratedKeys(t)
	for name, tc := range map[string]struct {
		chunk  int
		osPipe bool
	}{
		"whole writes":         {0, false},
		"fragmented frames":    {7, false},
		"real os stdin pipe":   {0, true},
		"fragmented real pipe": {1, true},
	} {
		t.Run(name, func(t *testing.T) {
			tree := newHactTree(t)
			o := hactRunOptions{keys: keys, nonce: authzNonce("unit/" + name), apply: tree.apply, osPipe: tc.osPipe}
			_, _, challenge, r := hactApplyOnce(t, o, intent, tc.chunk)
			if code := r.finish(); code != 0 || r.stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%q", code, r.stderr.String())
			}
			hactWantReceiptOnWire(t, r, challenge, intent)

			info := hactOnlyEntry(t, tree.unitDir, "uptime-phoenix-probe-"+intent.Identity.OperationID+".service")
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				t.Fatalf("unit file mode %v, want a regular file with 0644", info.Mode())
			}
			hactWantOwnedByUs(t, info)
			if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
				t.Fatal("unit file must have exactly one link")
			}
			got, err := os.ReadFile(tree.unitPath(intent))
			want := []byte(domain.ProbeSystemdUnit(intent.Identity))
			if err != nil || len(want) == 0 || !bytes.Equal(got, want) {
				t.Fatalf("unit file bytes differ from domain.ProbeSystemdUnit: %v\n got %q\nwant %q", err, got, want)
			}
			// What the grant signed covers exactly the installed bytes.
			sum := sha256.Sum256(got)
			if !strings.Contains(intent.Marker(), "\nunit_sha256="+hex.EncodeToString(sum[:])+"\n") {
				t.Fatal("the signed marker does not bind the installed unit bytes")
			}
			hactWantEmptyDirs(t, tree.varLib)
			if calls := tree.sysctl.recorded(); len(calls) != 0 {
				t.Fatalf("unit_file invoked systemctl: %v", calls)
			}
		})
	}
}

func TestRunAuthorizedProbeHostActionHelperReplayedGrantFromAnEarlierRunCreatesNothing(t *testing.T) {
	keys := authzGeneratedKeys(t)
	for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile} {
		intent := hactIntent(t, kind)
		for _, target := range []string{"fresh host", "same host"} {
			t.Run(kind+"/"+target, func(t *testing.T) {
				first := newHactTree(t)
				o1 := hactRunOptions{keys: keys, nonce: authzNonce("replay-first/" + kind + target), apply: first.apply}
				action1, grant1, _, r1 := hactApplyOnce(t, o1, intent, 0)
				if code := r1.finish(); code != 0 {
					t.Fatalf("first attempt code=%d stderr=%q", code, r1.stderr.String())
				}

				// The second helper has its own nonce; the captured frames are
				// replayed verbatim, as a wire observer could.
				second := first
				if target == "fresh host" {
					second = newHactTree(t)
				}
				before := entryNames(t, second.varLib, second.unitDir)
				o2 := hactRunOptions{keys: keys, nonce: authzNonce("replay-second/" + kind + target), apply: second.apply}
				r2 := startHactRun(t, o2)
				_, nonce2, challenge2 := r2.waitChallenge()
				if nonce2 == o1.nonce {
					t.Fatal("the two helper processes must have distinct nonces")
				}
				if _, err := r2.write(hactJoin(action1, grant1), 0); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal(err)
				}
				code := r2.finish()
				r2.wantRefused(code, domain.ErrProbeBinaryStageWriteUnauthorized, challenge2, len(action1)+len(grant1))
				if target == "fresh host" {
					second.wantEmpty(t)
				}
				if after := entryNames(t, second.varLib, second.unitDir); !slices.Equal(before, after) {
					t.Fatalf("replay changed the host: %v -> %v", before, after)
				}
			})
		}
	}
}

func entryNames(t *testing.T, dirs ...string) []string {
	t.Helper()
	var names []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			names = append(names, filepath.Join(d, e.Name()))
		}
	}
	return names
}

func TestRunAuthorizedProbeHostActionHelperFreshGrantNeverAdoptsOrFollowsExistingEntries(t *testing.T) {
	keys := authzGeneratedKeys(t)
	conflict := domain.ErrProbeBinaryStageWriteConflict

	runFresh := func(t *testing.T, tree *hactTree, intent domain.ProbeHostActionIntent, label string) {
		t.Helper()
		o := hactRunOptions{keys: keys, nonce: authzNonce("fresh/" + label), apply: tree.apply}
		action, grant, challenge, r := hactApplyOnce(t, o, intent, 0)
		code := r.finish()
		if code != 1 || r.stderr.String() != conflict.Error()+"\n" {
			t.Fatalf("code=%d stderr=%q, want exit 1 with the conflict refusal", code, r.stderr.String())
		}
		if got := r.stdout.Bytes(); !bytes.Equal(got, challenge) {
			t.Fatalf("stdout %q, want only the challenge (no receipt for a conflict)", got)
		}
		if r.applies.Load() != 1 {
			t.Fatalf("apply ran %d times, want once: the grant was valid and fresh", r.applies.Load())
		}
		if r.stdin.consumed.Load() != int64(len(action)+len(grant)) {
			t.Fatal("a valid fresh grant must be fully consumed before the conflict is reported")
		}
	}

	t.Run("data directory already exists", func(t *testing.T) {
		intent := hactIntent(t, domain.ProbeHostActionDataDirectory)
		tree := newHactTree(t)
		path := tree.dataPath(intent)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "canary"), []byte("pre-existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		runFresh(t, tree, intent, "dir")
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("the existing directory was adopted or re-moded: %v %v", info, err)
		}
		if body, err := os.ReadFile(filepath.Join(path, "canary")); err != nil || string(body) != "pre-existing" {
			t.Fatal("the existing directory's contents were disturbed", err)
		}
	})

	t.Run("unit file already exists", func(t *testing.T) {
		intent := hactIntent(t, domain.ProbeHostActionUnitFile)
		tree := newHactTree(t)
		if err := os.WriteFile(tree.unitPath(intent), []byte("pre-existing unit"), 0o600); err != nil {
			t.Fatal(err)
		}
		runFresh(t, tree, intent, "unit")
		info, err := os.Lstat(tree.unitPath(intent))
		body, rerr := os.ReadFile(tree.unitPath(intent))
		if err != nil || rerr != nil || string(body) != "pre-existing unit" || info.Mode().Perm() != 0o600 {
			t.Fatalf("the existing unit file was adopted or overwritten: %q %v %v", body, info, err)
		}
	})

	t.Run("data directory name is a symlink to a decoy directory", func(t *testing.T) {
		intent := hactIntent(t, domain.ProbeHostActionDataDirectory)
		tree := newHactTree(t)
		decoy := t.TempDir()
		if err := os.Chmod(decoy, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoy, tree.dataPath(intent)); err != nil {
			t.Fatal(err)
		}
		runFresh(t, tree, intent, "dir-symlink")
		info, err := os.Stat(decoy)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("the decoy directory behind the symlink was changed: %v %v", info, err)
		}
		hactWantEmptyDirs(t, decoy)
		if link, err := os.Readlink(tree.dataPath(intent)); err != nil || link != decoy {
			t.Fatal("the symlink was replaced", err)
		}
	})

	t.Run("unit file name is a symlink to a decoy file", func(t *testing.T) {
		intent := hactIntent(t, domain.ProbeHostActionUnitFile)
		tree := newHactTree(t)
		decoy := filepath.Join(t.TempDir(), "decoy")
		if err := os.WriteFile(decoy, []byte("decoy"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoy, tree.unitPath(intent)); err != nil {
			t.Fatal(err)
		}
		runFresh(t, tree, intent, "unit-symlink")
		if body, err := os.ReadFile(decoy); err != nil || string(body) != "decoy" {
			t.Fatalf("the file behind the symlink was written through: %q %v", body, err)
		}
	})

	t.Run("unit file name is a dangling symlink", func(t *testing.T) {
		intent := hactIntent(t, domain.ProbeHostActionUnitFile)
		tree := newHactTree(t)
		missing := filepath.Join(t.TempDir(), "never-created")
		if err := os.Symlink(missing, tree.unitPath(intent)); err != nil {
			t.Fatal(err)
		}
		runFresh(t, tree, intent, "unit-dangling")
		if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the dangling symlink's target was created through the link: %v", err)
		}
	})
}

// hactE2ERefusal is one sender stream that must create nothing on the host.
type hactE2ERefusal struct {
	name     string
	hostKeys []string
	stream   func(t *testing.T, k authzKeys, keyID string, nonce [32]byte, intent domain.ProbeHostActionIntent) []byte
	want     error
	// consumed is the exact stdin byte count the helper may read; -1 skips.
	consumed func(action, grant []byte) int
}

func TestRunAuthorizedProbeHostActionHelperUnauthorizedStreamsCreateNothingOnTheHost(t *testing.T) {
	unauthorized, input := domain.ErrProbeBinaryStageWriteUnauthorized, domain.ErrProbeBinaryStageWriteInput
	all := func(action, grant []byte) int { return len(action) + len(grant) }
	cases := []hactE2ERefusal{
		{
			name:     "foreign host key",
			hostKeys: []string{hactOtherFingerprint},
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, g := hactSenderFrames(t, k.priv, id, n, i)
				return hactJoin(a, g)
			},
			want:     unauthorized,
			consumed: func(action, _ []byte) int { return len(action) },
		},
		{
			name: "bit-flipped signature",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, g := hactSenderFrames(t, k.priv, id, n, i)
				g[20] ^= 0x01
				return hactJoin(a, g)
			},
			want:     unauthorized,
			consumed: all,
		},
		{
			name: "grant signed by an untrusted key",
			stream: func(t *testing.T, _ authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, g := hactSenderFrames(t, authzGeneratedKeys(t).priv, id, n, i)
				return hactJoin(a, g)
			},
			want:     unauthorized,
			consumed: all,
		},
		{
			name: "binary-stage grant for the same key and nonce",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, _ := hactSenderFrames(t, k.priv, id, n, i)
				stage := authzRootIntent()
				return hactJoin(a, authzRawFrame(authzSignRaw(k, authzRequest(k, stage, n).SigningMessage())))
			},
			want:     unauthorized,
			consumed: all,
		},
		{
			name: "grant for a different kind",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, _ := hactSenderFrames(t, k.priv, id, n, i)
				other := hactVariant(t, i, func(v *domain.ProbeHostActionIntent) { v.Kind = domain.ProbeHostActionService })
				_, g := hactSenderFrames(t, k.priv, id, n, other)
				return hactJoin(a, g)
			},
			want:     unauthorized,
			consumed: all,
		},
		{
			name: "non-root intent",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				_, g := hactSenderFrames(t, k.priv, id, n, i)
				deploy := helperIntent()
				header := hactStageHeader(t, domain.ProbeHostActionIntent{Identity: deploy.Identity, OwnerID: deploy.OwnerID, AttemptID: deploy.AttemptID})
				return hactJoin(hactRawAction("PHXACT1\n", byte(len(i.Kind)), i.Kind, header), g)
			},
			want:     input,
			consumed: nil,
		},
		{
			name: "one extra byte after the grant",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, g := hactSenderFrames(t, k.priv, id, n, i)
				return hactJoin(a, g, []byte("x"))
			},
			want:     input,
			consumed: func(action, grant []byte) int { return len(action) + len(grant) + 1 },
		},
		{
			name: "truncated grant",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, g := hactSenderFrames(t, k.priv, id, n, i)
				return hactJoin(a, g[:40])
			},
			want:     input,
			consumed: func(action, _ []byte) int { return len(action) + 40 },
		},
		{
			name: "missing grant",
			stream: func(t *testing.T, k authzKeys, id string, n [32]byte, i domain.ProbeHostActionIntent) []byte {
				a, _ := hactSenderFrames(t, k.priv, id, n, i)
				return a
			},
			want:     input,
			consumed: func(action, _ []byte) int { return len(action) },
		},
	}
	keys := authzGeneratedKeys(t)
	for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile} {
		intent := hactIntent(t, kind)
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				tree := newHactTree(t)
				o := hactRunOptions{keys: keys, nonce: authzNonce("e2e-refusal/" + kind + tc.name), apply: tree.apply}
				if tc.hostKeys != nil {
					list := tc.hostKeys
					o.hostKeys = func(context.Context) ([]string, error) { return list, nil }
				}
				r := startHactRun(t, o)
				keyID, nonce, challenge := r.waitChallenge()
				stream := tc.stream(t, keys, keyID, nonce, intent)
				action, grant := hactSenderFrames(t, keys.priv, keyID, nonce, intent)
				if _, err := r.write(stream, 0); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal(err)
				}
				code := r.finish()
				consumed := -1
				if tc.consumed != nil {
					consumed = tc.consumed(action, grant)
				}
				r.wantRefused(code, tc.want, challenge, consumed)
				tree.wantEmpty(t)
			})
		}
	}
}

func TestRunAuthorizedProbeHostActionHelperAppliesDataDirectoryUnitFileAndServiceAcrossThreeProcesses(t *testing.T) {
	keys := authzGeneratedKeys(t)
	tree := newHactTree(t)
	base := hactIntent(t, domain.ProbeHostActionDataDirectory)

	// Each action is its own helper process, challenge and one-shot grant, in
	// the fixed layout order the hub uses.
	for _, kind := range hactKinds {
		intent := base
		if kind != base.Kind {
			intent = hactVariant(t, base, func(i *domain.ProbeHostActionIntent) { i.Kind = kind })
		}
		o := hactRunOptions{keys: keys, nonce: authzNonce("lifecycle/" + kind), apply: tree.apply}
		_, _, challenge, r := hactApplyOnce(t, o, intent, 0)
		if code := r.finish(); code != 0 || r.stderr.Len() != 0 {
			t.Fatalf("%s: code=%d stderr=%q", kind, code, r.stderr.String())
		}
		hactWantReceiptOnWire(t, r, challenge, intent)
	}

	unit := "uptime-phoenix-probe-" + base.Identity.OperationID + ".service"
	if info, err := os.Lstat(tree.dataPath(base)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("data directory after the lifecycle: %v %v", info, err)
	}
	if info, err := os.Lstat(tree.unitPath(base)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Fatalf("unit file after the lifecycle: %v %v", info, err)
	}
	if got, err := os.ReadFile(tree.unitPath(base)); err != nil || string(got) != domain.ProbeSystemdUnit(base.Identity) {
		t.Fatalf("unit file bytes after the lifecycle differ: %v", err)
	}
	// The service is enabled, never started: the only systemctl calls are the
	// fixed sequence, and no other verb was possible (the fake rejects it).
	want := [][]string{{"is-enabled", unit}, {"daemon-reload"}, {"enable", unit}, {"is-enabled", unit}}
	if got := tree.sysctl.recorded(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("systemctl calls %v, want %v", got, want)
	}
	if entries := entryNames(t, tree.varLib, tree.unitDir); len(entries) != 2 {
		t.Fatalf("host holds %v, want exactly the data directory and the unit file", entries)
	}

	// A second service grant is a conflict: the unit is already enabled, and
	// nothing more is reloaded or enabled.
	service := hactVariant(t, base, func(i *domain.ProbeHostActionIntent) { i.Kind = domain.ProbeHostActionService })
	o := hactRunOptions{keys: keys, nonce: authzNonce("lifecycle/service-again"), apply: tree.apply}
	_, _, challenge, r := hactApplyOnce(t, o, service, 0)
	code := r.finish()
	if code != 1 || r.stderr.String() != domain.ErrProbeBinaryStageWriteConflict.Error()+"\n" || !bytes.Equal(r.stdout.Bytes(), challenge) {
		t.Fatalf("second service attempt code=%d stderr=%q stdout=%q", code, r.stderr.String(), r.stdout.Bytes())
	}
	if got := tree.sysctl.recorded(); len(got) != len(want)+1 || got[len(want)][0] != "is-enabled" {
		t.Fatalf("the conflicting attempt must only probe is-enabled: %v", got)
	}
}
