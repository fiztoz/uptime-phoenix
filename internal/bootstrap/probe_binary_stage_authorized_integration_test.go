//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Real-effect lifecycle tests for the authorized helper. Each test drives
// runAuthorizedProbeBinaryStageHelper end to end like the remote sender does:
// read the challenge from stdout first, sign it with a generated key, then feed
// stage header, one grant frame and the raw binary. The staged graph is a real
// directory under a temp parent; nothing outside t.TempDir() is touched.

const authzIOTimeout = 20 * time.Second

var (
	authzFixtureMu   sync.Mutex
	authzFixtureData []byte
)

// authzEffectData builds the tiny Linux ELF once per test binary run.
func authzEffectData(t *testing.T) []byte {
	t.Helper()
	authzFixtureMu.Lock()
	defer authzFixtureMu.Unlock()
	if authzFixtureData == nil {
		authzFixtureData = helperEffectFixture(t, "amd64")
	}
	if len(authzFixtureData) == 0 {
		t.Fatal("effect fixture is empty")
	}
	return authzFixtureData
}

// authzEffectIntent derives a valid stage intent from helperEffectIntent and sets
// the only host user admission accepts.
func authzEffectIntent(t *testing.T, data []byte) domain.ProbeBinaryStageIntent {
	t.Helper()
	intent := helperEffectIntent(t, data, "amd64")
	intent.Identity.Host.User = "root"
	if !intent.Valid() || !authzRequest(authzNewKeys(t, "probe"), intent, authzNonce("probe")).Valid() {
		t.Fatal("effect intent is not an admissible request")
	}
	return intent
}

// authzGeneratedKeys returns a freshly generated authority; the private half
// never leaves the test.
func authzGeneratedKeys(t *testing.T) authzKeys {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil || len(pub) != 32 {
		t.Fatal("generate authority key", err)
	}
	k := authzKeys{priv: priv}
	copy(k.pub[:], pub)
	k.id = domain.ProbeBinaryStageAuthorityKeyID(k.pub)
	return k
}

// authzSignChallenge is the sender's view: it knows only what the parsed
// challenge line said and the intent it wants staged.
func authzSignChallenge(t *testing.T, priv ed25519.PrivateKey, keyID string, nonce [32]byte, intent domain.ProbeBinaryStageIntent) [64]byte {
	t.Helper()
	message := domain.ProbeBinaryStageAdmissionRequest{
		Intent:    intent,
		Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
	}.SigningMessage()
	if message == nil {
		t.Fatal("sender cannot sign an inadmissible request")
	}
	return authzSignRaw(authzKeys{priv: priv}, message)
}

// authzStdout is the helper's stdout. ready closes as soon as one full challenge
// line has been written, so the sender can react while the helper still runs.
type authzStdout struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newAuthzStdout() *authzStdout { return &authzStdout{ready: make(chan struct{})} }

func (w *authzStdout) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.buf.Len() >= provisioning.ProbeBinaryStageChallengeLineSize {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func (w *authzStdout) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// authzStdin is a synchronous pipe that counts exactly how many bytes the
// helper consumed; io.Pipe writes block until read, so unconsumed payload shows
// up as a write that never completed.
type authzStdin struct {
	pr       *io.PipeReader
	consumed atomic.Int64
	closes   atomic.Int32
}

func (s *authzStdin) Read(p []byte) (int, error) {
	n, err := s.pr.Read(p)
	s.consumed.Add(int64(n))
	return n, err
}

func (s *authzStdin) Close() error {
	s.closes.Add(1)
	return s.pr.Close()
}

type authzOptions struct {
	keys   authzKeys // the authority the helper trusts
	nonce  [32]byte  // the deterministic entropy the helper turns into its challenge
	parent string    // directory the injected acquirer opens fresh on every call
	osPipe bool      // use a real os.Pipe for stdin instead of the counting io.Pipe
	// hostKeys overrides the local sshd fingerprints; nil trusts the intent's host.
	hostKeys func(context.Context) ([]string, error)
}

// authzRun is one helper process: a goroutine running the wrapper with injected
// dependencies, plus the sender's ends of its stdin and stdout.
type authzRun struct {
	t        *testing.T
	o        authzOptions
	cancel   context.CancelFunc
	stdout   *authzStdout
	stderr   bytes.Buffer
	stdin    *authzStdin // nil with osPipe
	pipeIn   *os.File    // helper's end of an osPipe
	w        io.WriteCloser
	done     chan int
	finished bool
	acquires atomic.Int32

	mu      sync.Mutex
	parents []*os.File
}

func startAuthzRun(t *testing.T, o authzOptions) *authzRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &authzRun{t: t, o: o, cancel: cancel, stdout: newAuthzStdout(), done: make(chan int, 1)}
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
	acquire := func(context.Context) (*os.File, error) {
		r.acquires.Add(1)
		f, err := os.Open(o.parent)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.parents = append(r.parents, f)
		r.mu.Unlock()
		return f, nil
	}
	entropy := bytes.NewReader(append([]byte(nil), o.nonce[:]...))
	hostKeys := authzIntentHost
	if o.hostKeys != nil {
		hostKeys = o.hostKeys
	}
	go func() {
		r.done <- runAuthorizedProbeBinaryStageHelper(ctx, nil, input, r.stdout, &r.stderr, loadKey, acquire, entropy, hostKeys)
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
func (r *authzRun) waitChallenge() (keyID string, nonce [32]byte, line []byte) {
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
	if !bytes.Equal(all, provisioning.EncodeProbeBinaryStageChallengeLine(keyID, nonce)) {
		r.t.Fatal("challenge is not the canonical encoding")
	}
	return keyID, nonce, all
}

// write sends data to the helper's stdin, optionally in small chunks, and
// reports how many bytes the helper consumed before it stopped reading.
func (r *authzRun) write(data []byte, chunk int) (int, error) {
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
func (r *authzRun) finish() int {
	r.t.Helper()
	_ = r.w.Close()
	return r.waitExit()
}

// waitExit waits for the helper to exit on its own, without closing stdin, and
// checks that it released the parent it acquired.
func (r *authzRun) waitExit() int {
	r.t.Helper()
	var code int
	select {
	case code = <-r.done:
		r.finished = true
	case <-time.After(authzIOTimeout):
		r.t.Fatal("helper did not exit")
	}
	r.cancel()
	r.wantParentsReleased()
	return code
}

func (r *authzRun) wantParentsReleased() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.parents) != int(r.acquires.Load()) || len(r.parents) != 1 {
		r.t.Fatalf("parents handed out=%d acquires=%d, want exactly one", len(r.parents), r.acquires.Load())
	}
	for _, p := range r.parents {
		if _, err := p.Stat(); !errors.Is(err, os.ErrClosed) {
			r.t.Fatal("helper leaked its acquired parent descriptor", err)
		}
	}
}

// wantRefusedAfterChallenge asserts a pre-creation refusal that happens after the
// challenge was emitted: exit 1, the one sanitized stderr line, stdout holding
// only the challenge, nothing created or spooled, and an exact stdin byte count.
func (r *authzRun) wantRefusedAfterChallenge(code int, want error, challenge []byte, consumed int, parent, scratch string) {
	r.t.Helper()
	if code != 1 || r.stderr.String() != want.Error()+"\n" {
		r.t.Fatalf("code=%d stderr=%q, want exit 1 with %q", code, r.stderr.String(), want.Error())
	}
	if got := r.stdout.Bytes(); !bytes.Equal(got, challenge) {
		r.t.Fatalf("stdout %q, want only the challenge line", got)
	}
	helperEffectEmpty(r.t, parent, scratch)
	if r.stdin != nil {
		if got := r.stdin.consumed.Load(); got != int64(consumed) {
			r.t.Fatalf("helper consumed %d stdin bytes, want exactly %d", got, consumed)
		}
		if r.stdin.closes.Load() != 1 {
			r.t.Fatalf("stdin closed %d times, want 1", r.stdin.closes.Load())
		}
	}
}

func authzHeader(t *testing.T, intent domain.ProbeBinaryStageIntent) []byte {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// authzSignedFrame frames the grant exactly as the real sender would.
func authzSignedFrame(t *testing.T, priv ed25519.PrivateKey, keyID string, nonce [32]byte, intent domain.ProbeBinaryStageIntent) []byte {
	t.Helper()
	grant := domain.ProbeBinaryStageGrant{
		Request: domain.ProbeBinaryStageAdmissionRequest{
			Intent:    intent,
			Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: intent.MarkerSHA256()},
		},
		Signature: authzSignChallenge(t, priv, keyID, nonce, intent),
	}
	frame, err := provisioning.EncodeProbeBinaryStageGrantFrame(grant)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func authzWantReceipt(t *testing.T, intent domain.ProbeBinaryStageIntent) []byte {
	t.Helper()
	payload, err := probeBinaryStageHelperReceipt(domain.ProbeBinaryStageSeal{Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// authzStageOnce runs a full successful attempt and returns the sender's frame.
func authzStageOnce(t *testing.T, o authzOptions, intent domain.ProbeBinaryStageIntent, data []byte, chunk int) (frame []byte, r *authzRun, header []byte, challenge []byte) {
	t.Helper()
	r = startAuthzRun(t, o)
	keyID, nonce, challenge := r.waitChallenge()
	header = authzHeader(t, intent)
	frame = authzSignedFrame(t, o.keys.priv, keyID, nonce, intent)
	if n, err := r.write(append(append([]byte(nil), header...), frame...), chunk); err != nil || n != len(header)+len(frame) {
		t.Fatalf("header+grant write n=%d err=%v", n, err)
	}
	if n, err := r.write(data, 0); err != nil || n != len(data) {
		t.Fatalf("payload write n=%d err=%v", n, err)
	}
	return frame, r, header, challenge
}

func TestRunAuthorizedProbeBinaryStageHelperStagesUnderVerifiedGrant(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)
	for name, tc := range map[string]struct {
		chunk  int
		osPipe bool
	}{
		"whole writes":         {0, false},
		"fragmented grant":     {5, false},
		"real os stdin pipe":   {0, true},
		"fragmented real pipe": {3, true},
	} {
		t.Run(name, func(t *testing.T) {
			parent, _, scratch := helperEffectParent(t)
			o := authzOptions{keys: keys, nonce: authzNonce("success/" + name), parent: parent, osPipe: tc.osPipe}
			_, r, header, challenge := authzStageOnce(t, o, intent, data, tc.chunk)
			if code := r.finish(); code != 0 || r.stderr.Len() != 0 {
				t.Fatalf("code=%d stderr=%q", code, r.stderr.String())
			}
			receipt := authzWantReceipt(t, intent)
			if got, want := r.stdout.Bytes(), append(append([]byte(nil), challenge...), receipt...); !bytes.Equal(got, want) {
				t.Fatalf("stdout %q, want challenge line then the receipt line", got)
			}
			var view probeBinaryStageHelperReceiptView
			if err := json.Unmarshal(receipt, &view); err != nil {
				t.Fatal(err)
			}
			seal := domain.ProbeBinaryStageSeal{Intent: intent}
			if view.State != "local_binary_staged" || view.DirectoryName != seal.DirectoryName() ||
				view.MarkerSHA256 != intent.MarkerSHA256() || view.SHA256 != intent.Identity.Binary.SHA256 ||
				view.Size != int64(len(data)) || view.OS != "linux" || view.Architecture != "amd64" || view.VersionSource != "operator_pin" {
				t.Fatalf("receipt fields drifted: %+v", view)
			}
			// The challenge precedes the receipt as two separate LF-terminated lines.
			if lines := bytes.Count(r.stdout.Bytes(), []byte{'\n'}); lines != 2 {
				t.Fatalf("stdout has %d lines, want 2", lines)
			}
			helperEffectGraph(t, parent, intent, data)
			helperEffectEmpty(t, scratch)
			if r.stdin != nil {
				if got, want := r.stdin.consumed.Load(), int64(len(header)+provisioning.ProbeBinaryStageGrantFrameSize+len(data)); got != want || r.stdin.closes.Load() != 1 {
					t.Fatalf("stdin consumed=%d want=%d closes=%d", got, want, r.stdin.closes.Load())
				}
			}
			if r.pipeIn != nil {
				if _, err := r.pipeIn.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("helper did not release its owned stdin", err)
				}
			}
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperFreshGrantDoesNotAdoptExistingStage(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)
	parent, _, scratch := helperEffectParent(t)

	_, first, _, _ := authzStageOnce(t, authzOptions{keys: keys, nonce: authzNonce("first"), parent: parent}, intent, data, 0)
	if code := first.finish(); code != 0 {
		t.Fatalf("first attempt code=%d stderr=%q", code, first.stderr.String())
	}

	// A new process with its own nonce and a correctly signed fresh grant is
	// authorized, yet it still refuses to adopt or overwrite the existing graph.
	second := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("second"), parent: parent})
	keyID, nonce, challenge := second.waitChallenge()
	header := authzHeader(t, intent)
	frame := authzSignedFrame(t, keys.priv, keyID, nonce, intent)
	if n, err := second.write(append(append([]byte(nil), header...), frame...), 0); err != nil || n != len(header)+len(frame) {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	_, _ = second.write(data, 0)
	code := second.finish()
	if code != 1 || second.stderr.String() != domain.ErrProbeBinaryStageWriteConflict.Error()+"\n" ||
		!bytes.Equal(second.stdout.Bytes(), challenge) {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, second.stderr.String(), second.stdout.Bytes())
	}
	helperEffectGraph(t, parent, intent, data)
	helperEffectEmpty(t, scratch)
}

func TestRunAuthorizedProbeBinaryStageHelperRefusesReplayedGrantForDifferentNonce(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)

	// Run A is a legitimate attempt; its grant frame is what an eavesdropper keeps.
	parentA, _, _ := helperEffectParent(t)
	replayed, a, _, _ := authzStageOnce(t, authzOptions{keys: keys, nonce: authzNonce("run-A"), parent: parentA}, intent, data, 0)
	if code := a.finish(); code != 0 {
		t.Fatalf("run A code=%d stderr=%q", code, a.stderr.String())
	}
	helperEffectGraph(t, parentA, intent, data)

	parentB, _, scratchB := helperEffectParent(t)
	b := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("run-B"), parent: parentB})
	keyID, nonce, challenge := b.waitChallenge()
	if bytes.Equal(replayed, authzSignedFrame(t, keys.priv, keyID, nonce, intent)) {
		t.Fatal("fixture: the replayed grant equals the grant for the new nonce")
	}
	header := authzHeader(t, intent)
	if n, err := b.write(append(append([]byte(nil), header...), replayed...), 0); err != nil || n != len(header)+len(replayed) {
		t.Fatalf("header+grant write n=%d err=%v", n, err)
	}
	// The binary the attacker also wants staged is never consumed: the helper has
	// already closed stdin, so not one payload byte is accepted.
	n, werr := b.write(data, 0)
	if n != 0 || werr == nil {
		t.Fatalf("payload write n=%d err=%v, want 0 bytes consumed and a closed pipe", n, werr)
	}
	code := b.finish()
	b.wantRefusedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnauthorized, challenge, len(header)+provisioning.ProbeBinaryStageGrantFrameSize, parentB, scratchB)
	// The legitimate run's graph is untouched by the replay.
	helperEffectGraph(t, parentA, intent, data)
}

func TestRunAuthorizedProbeBinaryStageHelperRefusesTamperedGrantsWithoutEffects(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)
	attacker := authzGeneratedKeys(t)

	otherAttempt := intent
	otherAttempt.AttemptID = "77777777-7777-4777-8777-777777777777"
	type grantFn func(t *testing.T, keyID string, nonce [32]byte) []byte
	exact := func(keyID string, nonce [32]byte, in domain.ProbeBinaryStageIntent) []byte {
		return domain.ProbeBinaryStageAdmissionRequest{
			Intent:    in,
			Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: in.MarkerSHA256()},
		}.SigningMessage()
	}
	cases := map[string]grantFn{
		"forged signature by a different key over the exact message": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			return authzRawFrame(authzSignRaw(attacker, exact(keyID, nonce, intent)))
		},
		"attacker signs for its own key id": func(t *testing.T, _ string, nonce [32]byte) []byte {
			return authzRawFrame(authzSignChallenge(t, attacker.priv, attacker.id, nonce, intent))
		},
		"bit flipped signature": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			frame := authzSignedFrame(t, keys.priv, keyID, nonce, intent)
			frame[len(frame)-1] ^= 0x01
			return frame
		},
		"zero signature": func(*testing.T, string, [32]byte) []byte { return authzRawFrame([64]byte{}) },
		"wrong frame magic": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			frame := authzSignedFrame(t, keys.priv, keyID, nonce, intent)
			copy(frame, "PHXGRT2\n")
			return frame
		},
		"signature for a different marker": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			return authzSignedFrame(t, keys.priv, keyID, nonce, otherAttempt)
		},
		"signature over a different nonce": func(t *testing.T, keyID string, _ [32]byte) []byte {
			return authzSignedFrame(t, keys.priv, keyID, authzNonce("some-other-process"), intent)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			parent, _, scratch := helperEffectParent(t)
			r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("tamper/" + name), parent: parent})
			keyID, nonce, challenge := r.waitChallenge()
			header, frame := authzHeader(t, intent), build(t, keyID, nonce)
			if len(frame) != provisioning.ProbeBinaryStageGrantFrameSize {
				t.Fatalf("fixture frame is %d bytes", len(frame))
			}
			if n, err := r.write(append(append([]byte(nil), header...), frame...), 0); err != nil || n != len(header)+len(frame) {
				t.Fatalf("header+grant write n=%d err=%v", n, err)
			}
			if n, err := r.write(data, 0); n != 0 || err == nil {
				t.Fatalf("payload write n=%d err=%v, want nothing consumed", n, err)
			}
			code := r.finish()
			r.wantRefusedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnauthorized, challenge, len(header)+len(frame), parent, scratch)
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperRefusesNonRootIntentEvenWhenCorrectlySigned(t *testing.T) {
	data := authzEffectData(t)
	rootIntent := authzEffectIntent(t, data)
	nonRoot := helperEffectIntent(t, data, "amd64") // helperIntent's Host.User is "deploy"
	if nonRoot.Identity.Host.User == "root" || !nonRoot.Valid() {
		t.Fatalf("fixture: want a valid non-root intent, got user %q", nonRoot.Identity.Host.User)
	}
	keys := authzGeneratedKeys(t)

	for name, build := range map[string]func(t *testing.T, keyID string, nonce [32]byte) []byte{
		"non-root intent signed over the exact documented message": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			k := authzKeys{priv: keys.priv, pub: keys.pub, id: keyID}
			sig := authzSignRaw(k, authzManualMessage(k, nonce, nonRoot.MarkerSHA256()))
			// The sender-side codec refuses to even frame this grant.
			grant := domain.ProbeBinaryStageGrant{
				Request: domain.ProbeBinaryStageAdmissionRequest{
					Intent:    nonRoot,
					Challenge: domain.ProbeBinaryStageChallenge{AuthorityKeyID: keyID, Nonce: nonce, MarkerSHA256: nonRoot.MarkerSHA256()},
				},
				Signature: sig,
			}
			if _, err := provisioning.EncodeProbeBinaryStageGrantFrame(grant); err == nil {
				t.Fatal("grant codec framed a non-root request")
			}
			return authzRawFrame(sig)
		},
		"root intent signed but a non-root header sent": func(t *testing.T, keyID string, nonce [32]byte) []byte {
			return authzSignedFrame(t, keys.priv, keyID, nonce, rootIntent)
		},
	} {
		t.Run(name, func(t *testing.T) {
			parent, _, scratch := helperEffectParent(t)
			r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("nonroot/" + name), parent: parent})
			keyID, nonce, challenge := r.waitChallenge()
			header, frame := authzHeader(t, nonRoot), build(t, keyID, nonce)
			if n, err := r.write(append(append([]byte(nil), header...), frame...), 0); err != nil || n != len(header)+len(frame) {
				t.Fatalf("header+grant write n=%d err=%v", n, err)
			}
			if n, err := r.write(data, 0); n != 0 || err == nil {
				t.Fatalf("payload write n=%d err=%v, want nothing consumed", n, err)
			}
			code := r.finish()
			r.wantRefusedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnauthorized, challenge, len(header)+len(frame), parent, scratch)
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperRefusesMissingOrTruncatedGrant(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)

	for name, build := range map[string]func(t *testing.T, header, frame []byte) []byte{
		"stdin closed right after the challenge": func(*testing.T, []byte, []byte) []byte { return nil },
		"stdin closed after the header":          func(t *testing.T, header, _ []byte) []byte { return header },
		"stdin closed one byte short of the grant": func(t *testing.T, header, frame []byte) []byte {
			return append(append([]byte(nil), header...), frame[:len(frame)-1]...)
		},
		"stdin closed inside the grant magic": func(t *testing.T, header, frame []byte) []byte {
			return append(append([]byte(nil), header...), frame[:4]...)
		},
		"stdin closed inside the header": func(t *testing.T, header, _ []byte) []byte { return header[:len(header)-1] },
	} {
		t.Run(name, func(t *testing.T) {
			parent, _, scratch := helperEffectParent(t)
			r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("missing/" + name), parent: parent})
			keyID, nonce, challenge := r.waitChallenge()
			sent := build(t, authzHeader(t, intent), authzSignedFrame(t, keys.priv, keyID, nonce, intent))
			if len(sent) > 0 {
				if n, err := r.write(sent, 0); err != nil || n != len(sent) {
					t.Fatalf("write n=%d err=%v", n, err)
				}
			}
			code := r.finish()
			// Even a perfectly signed grant never helps a stream that ends early.
			r.wantRefusedAfterChallenge(code, domain.ErrProbeBinaryStageWriteInput, challenge, len(sent), parent, scratch)
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperValidGrantStillRequiresExactPayload(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)

	for name, tc := range map[string]struct {
		payload []byte
		want    error
	}{
		"one byte short": {data[:len(data)-1], domain.ErrProbeArtifactSize},
		"one byte extra": {append(append([]byte(nil), data...), 0), domain.ErrProbeArtifactSize},
		"empty":          {nil, domain.ErrProbeArtifactSize},
		"wrong content":  {append(append([]byte(nil), data[:len(data)-1]...), data[len(data)-1]^0xff), domain.ErrProbeArtifactDigest},
	} {
		t.Run(name, func(t *testing.T) {
			parent, _, scratch := helperEffectParent(t)
			r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("payload/" + name), parent: parent})
			keyID, nonce, challenge := r.waitChallenge()
			header, frame := authzHeader(t, intent), authzSignedFrame(t, keys.priv, keyID, nonce, intent)
			if n, err := r.write(append(append([]byte(nil), header...), frame...), 0); err != nil || n != len(header)+len(frame) {
				t.Fatalf("header+grant write n=%d err=%v", n, err)
			}
			if len(tc.payload) > 0 {
				_, _ = r.write(tc.payload, 0) // the helper may stop reading as soon as it can refuse
			}
			code := r.finish()
			// A verified grant authorizes an attempt, not arbitrary bytes: the
			// pinned size/digest refusal still fires and leaves nothing behind.
			if code != 1 || r.stderr.String() != tc.want.Error()+"\n" || !bytes.Equal(r.stdout.Bytes(), challenge) {
				t.Fatalf("code=%d stderr=%q stdout=%q, want exit 1, %q and only the challenge", code, r.stderr.String(), r.stdout.Bytes(), tc.want.Error())
			}
			helperEffectEmpty(t, parent, scratch)
		})
	}
}

func TestRunAuthorizedProbeBinaryStageHelperCancelWhileAwaitingGrant(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)
	parent, _, scratch := helperEffectParent(t)

	r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("stalled-sender"), parent: parent})
	_, _, challenge := r.waitChallenge()
	// The sender goes silent after the challenge, mid-header, then the caller's
	// context ends. The helper must give up, release everything and create nothing.
	header := authzHeader(t, intent)
	half := header[:len(header)/2]
	if n, err := r.write(half, 0); err != nil || n != len(half) {
		t.Fatalf("partial header write n=%d err=%v", n, err)
	}
	// Stdin is deliberately left open: cancellation alone must unblock the read.
	r.cancel()
	code := r.waitExit()
	r.wantRefusedAfterChallenge(code, context.Canceled, challenge, len(half), parent, scratch)
}

// A grant validly signed for this helper's own nonce is still refused when the
// intent pins another host's SSH key: this is the cross-host relay where a
// compromised host feeds a second host's challenge through the hub for its own
// attempt. The grant frame itself is never read.
func TestRunAuthorizedProbeBinaryStageHelperRefusesIntentForAnotherHost(t *testing.T) {
	data := authzEffectData(t)
	intent := authzEffectIntent(t, data)
	keys := authzGeneratedKeys(t)
	parent, _, scratch := helperEffectParent(t)
	otherHost := func(context.Context) ([]string, error) {
		return []string{"SHA256:" + strings.Repeat("B", 43)}, nil
	}
	r := startAuthzRun(t, authzOptions{keys: keys, nonce: authzNonce("other-host"), parent: parent, hostKeys: otherHost})
	keyID, nonce, challenge := r.waitChallenge()
	header := authzHeader(t, intent)
	if n, err := r.write(header, 0); err != nil || n != len(header) {
		t.Fatalf("header write n=%d err=%v", n, err)
	}
	grant := authzSignedFrame(t, keys.priv, keyID, nonce, intent)
	if n, err := r.write(grant, 0); n != 0 || err == nil {
		t.Fatalf("grant write n=%d err=%v, want 0 bytes consumed and a closed pipe", n, err)
	}
	code := r.finish()
	r.wantRefusedAfterChallenge(code, domain.ErrProbeBinaryStageWriteUnauthorized, challenge, len(header), parent, scratch)
}
