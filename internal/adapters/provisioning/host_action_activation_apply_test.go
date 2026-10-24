//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests exercise ApplyProbeInit and ApplyProbeServiceStart against
// t.TempDir() parents standing in for the borrowed /var/lib, /opt and
// /etc/systemd/system descriptors, a recording fake in place of the probe
// binary runner and a scripted fake in place of systemctl. No real probe,
// systemctl, privileged path or network is involved, and the owner check is
// against the test process's own euid, so root ownership on a real host is not
// established here. The runner receives the intent-derived absolute /opt path,
// which these tests assert as a string; nothing is executed from the temp dirs.

const (
	actApplyBinaryContent = "phoenix pinned probe binary bytes, fixture only"
	actApplyDataPath      = "/var/lib/" + hostActApplyDataName
	actApplyArtifactPath  = "/opt/" + hostActApplyDataName
)

// ---- fixtures ---------------------------------------------------------------

type actApplyEnv struct {
	intent      domain.ProbeHostActionIntent
	content     []byte
	dataRoot    string
	optRoot     string
	dataParent  *os.File
	optParent   *os.File
	dataDir     string
	artifactDir string
	binary      string
}

// newActApplyEnv builds the host layout a successful install leaves behind: an
// empty 0700 data directory and a 0700 artifact directory holding the pinned
// binary as a single-link 0555 file. Modes are set explicitly, never by umask.
func newActApplyEnv(t *testing.T) *actApplyEnv {
	t.Helper()
	e := &actApplyEnv{content: []byte(actApplyBinaryContent)}
	e.intent = hostActFrameIntent(domain.ProbeActivationProbeInit)
	e.intent.Identity.Binary = sendTPin(e.content)
	e.dataRoot, e.dataParent = hostActApplyParent(t)
	e.optRoot, e.optParent = hostActApplyParent(t)
	e.dataDir = filepath.Join(e.dataRoot, hostActApplyDataName)
	e.artifactDir = filepath.Join(e.optRoot, hostActApplyDataName)
	e.binary = filepath.Join(e.artifactDir, e.intent.Identity.Binary.ReleaseName())
	for _, dir := range []string{e.dataDir, e.artifactDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.writeBinary(t, e.content)
	return e
}

func (e *actApplyEnv) writeBinary(t *testing.T, data []byte) {
	t.Helper()
	_ = os.Chmod(e.binary, 0o600)
	if err := os.WriteFile(e.binary, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.binary, 0o555); err != nil {
		t.Fatal(err)
	}
}

func (e *actApplyEnv) wantBinaryPath() string {
	return actApplyArtifactPath + "/" + e.intent.Identity.Binary.ReleaseName()
}

func (e *actApplyEnv) wantArgs() []string {
	return []string{"init", "--data-dir", actApplyDataPath}
}

type actApplyRunnerCall struct {
	ctx  context.Context
	path string
	args []string
}

// actApplyRunner records every invocation; the script decides each answer.
type actApplyRunner struct {
	mu     sync.Mutex
	calls  []actApplyRunnerCall
	stdout []byte
	script func(ctx context.Context, call int, path string, args []string) ([]byte, int, error)
}

func newActApplyRunner(t *testing.T) *actApplyRunner {
	t.Helper()
	return &actApplyRunner{stdout: probeInitRealBytes(t, nil)}
}

func (r *actApplyRunner) Run(ctx context.Context, path string, args ...string) ([]byte, int, error) {
	r.mu.Lock()
	call := len(r.calls)
	r.calls = append(r.calls, actApplyRunnerCall{ctx: ctx, path: path, args: slices.Clone(args)})
	script, stdout := r.script, r.stdout
	r.mu.Unlock()
	if script != nil {
		return script(ctx, call, path, args)
	}
	return slices.Clone(stdout), 0, nil
}

func (r *actApplyRunner) recorded() []actApplyRunnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *actApplyRunner) requireNoCalls(t *testing.T) {
	t.Helper()
	if got := r.recorded(); len(got) != 0 {
		t.Fatalf("refusal ran the binary: %+v", got)
	}
}

type actApplyRunnerFunc func(ctx context.Context, path string, args ...string) ([]byte, int, error)

func (f actApplyRunnerFunc) Run(ctx context.Context, path string, args ...string) ([]byte, int, error) {
	return f(ctx, path, args...)
}

func actApplyChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Skipf("cannot set mode %v on this platform: %v", mode, err)
	}
	if info, err := os.Lstat(path); err != nil || hostActApplyBits(info) != mode {
		t.Skipf("mode %v is not representable here: %v %v", mode, info, err)
	}
}

// requireDirUntouched asserts the data directory and the staged binary are
// exactly as the fixture left them.
func (e *actApplyEnv) requireBinaryUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(e.binary)
	if err != nil || !bytes.Equal(got, e.content) {
		t.Fatalf("binary changed or unreadable: %v", err)
	}
	if info, err := os.Lstat(e.binary); err != nil || hostActApplyBits(info) != 0o555 {
		t.Fatalf("binary mode %v %v", info, err)
	}
}

type actApplyFlipContext struct {
	context.Context
	calls  atomic.Int32
	flipAt int32
}

func (c *actApplyFlipContext) Err() error {
	if c.calls.Add(1) >= c.flipAt {
		return context.Canceled
	}
	return nil
}

// ---- ApplyProbeInit ---------------------------------------------------------

func TestApplyProbeInitRunsTheExactPinnedBinaryOnce(t *testing.T) {
	for name, stdout := range map[string]func(*testing.T) []byte{
		"stdout with trailing LF": func(t *testing.T) []byte { return probeInitRealBytes(t, nil) },
		"stdout without trailing LF": func(t *testing.T) []byte {
			return bytes.TrimSuffix(probeInitRealBytes(t, nil), []byte("\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newActApplyEnv(t)
			runner := newActApplyRunner(t)
			runner.stdout = stdout(t)
			ctx := context.WithValue(t.Context(), hostActApplyCtxKey{}, "sentinel")

			got, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, e.intent, runner)
			hostActApplyRequireErr(t, err, nil)
			if got != probeInitTestResult() {
				t.Fatalf("result %+v, want %+v", got, probeInitTestResult())
			}

			calls := runner.recorded()
			if len(calls) != 1 {
				t.Fatalf("runner calls = %d, want exactly 1", len(calls))
			}
			if calls[0].path != e.wantBinaryPath() {
				t.Fatalf("binary path %q, want %q", calls[0].path, e.wantBinaryPath())
			}
			if !slices.Equal(calls[0].args, e.wantArgs()) {
				t.Fatalf("args %q, want %q", calls[0].args, e.wantArgs())
			}
			if calls[0].ctx.Value(hostActApplyCtxKey{}) != "sentinel" {
				t.Fatal("runner did not receive the caller's context")
			}
			// The executed path comes from the intent, never from the borrowed
			// descriptors, and carries no shell syntax.
			for _, s := range append([]string{calls[0].path}, calls[0].args...) {
				if strings.Contains(s, e.dataRoot) || strings.Contains(s, e.optRoot) || strings.ContainsAny(s, " \t\n;|&$`'\"<>*") {
					t.Fatalf("unexpected argument %q", s)
				}
			}

			// Applying only reads: nothing was created, changed or removed.
			hostActApplyRequireEntries(t, e.dataDir)
			hostActApplyRequireEntries(t, e.artifactDir, e.intent.Identity.Binary.ReleaseName())
			e.requireBinaryUntouched(t)
			hostActApplyRequireBorrowedParentOpen(t, e.dataParent)
			hostActApplyRequireBorrowedParentOpen(t, e.optParent)
		})
	}
}

func TestApplyProbeInitRefusesDataDirectoryProblemsBeforeRunning(t *testing.T) {
	cases := []struct {
		name  string
		want  error
		setup func(t *testing.T, e *actApplyEnv)
	}{
		{"missing", domain.ErrProbeBinaryStageWriteUnavailable, func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.dataDir); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular file instead of a directory", domain.ErrProbeBinaryStageWriteUnavailable, func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.dataDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.dataDir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to a correct empty directory", domain.ErrProbeBinaryStageWriteUnavailable, func(t *testing.T, e *actApplyEnv) {
			real := filepath.Join(e.dataRoot, "real")
			if err := os.Rename(e.dataDir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, e.dataDir); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", domain.ErrProbeBinaryStageWriteUnavailable, func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.dataDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(e.dataRoot, "nowhere"), e.dataDir); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, mode := range []os.FileMode{0o755, 0o750, 0o770, 0o777, 0o500, 0o600, 0o701, 0o710, 0o000, os.ModeSetgid | 0o700, os.ModeSticky | 0o700} {
		cases = append(cases, struct {
			name  string
			want  error
			setup func(t *testing.T, e *actApplyEnv)
		}{"mode " + mode.String(), domain.ErrProbeBinaryStageWriteUnavailable, func(t *testing.T, e *actApplyEnv) {
			actApplyChmod(t, e.dataDir, mode)
		}})
	}
	for name, populate := range map[string]func(t *testing.T, dir string){
		"a file":         func(t *testing.T, dir string) { actApplyMustWrite(t, filepath.Join(dir, "identity.db"), "x") },
		"an empty file":  func(t *testing.T, dir string) { actApplyMustWrite(t, filepath.Join(dir, "empty"), "") },
		"a dotfile":      func(t *testing.T, dir string) { actApplyMustWrite(t, filepath.Join(dir, ".hidden"), "x") },
		"a subdirectory": func(t *testing.T, dir string) { actApplyMustMkdir(t, filepath.Join(dir, "sub")) },
		"a symlink":      func(t *testing.T, dir string) { actApplyMustSymlink(t, "/nonexistent", filepath.Join(dir, "link")) },
		"a fifo":         func(t *testing.T, dir string) { actApplyMustMkfifo(t, filepath.Join(dir, "fifo")) },
		"many entries": func(t *testing.T, dir string) {
			actApplyMustWrite(t, filepath.Join(dir, "a"), "1")
			actApplyMustWrite(t, filepath.Join(dir, "b"), "2")
		},
		"a nested secret": func(t *testing.T, dir string) {
			actApplyMustMkdir(t, filepath.Join(dir, "tls"))
			actApplyMustWrite(t, filepath.Join(dir, "tls", "key"), "x")
		},
	} {
		cases = append(cases, struct {
			name  string
			want  error
			setup func(t *testing.T, e *actApplyEnv)
		}{"not empty: " + name, domain.ErrProbeBinaryStageWriteConflict, func(t *testing.T, e *actApplyEnv) {
			populate(t, e.dataDir)
		}})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newActApplyEnv(t)
			tc.setup(t, e)
			before := hostActApplyEntries(t, e.dataRoot)
			runner := newActApplyRunner(t)

			got, err := actApplyWithinInit(t, func() (ProbeInitResult, error) {
				return ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner)
			})
			hostActApplyRequireErr(t, err, tc.want)
			if got != (ProbeInitResult{}) {
				t.Fatalf("refusal returned %+v", got)
			}
			runner.requireNoCalls(t)
			if after := hostActApplyEntries(t, e.dataRoot); !slices.Equal(before, after) {
				t.Fatalf("data parent entries changed: %v -> %v", before, after)
			}
			e.requireBinaryUntouched(t)
		})
	}
}

func TestApplyProbeInitRefusesBinaryProblemsBeforeRunning(t *testing.T) {
	type tcase struct {
		name  string
		setup func(t *testing.T, e *actApplyEnv)
	}
	cases := []tcase{
		{"artifact directory missing", func(t *testing.T, e *actApplyEnv) {
			if err := os.RemoveAll(e.artifactDir); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary missing", func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.binary); err != nil {
				t.Fatal(err)
			}
		}},
		{"artifact directory is a file", func(t *testing.T, e *actApplyEnv) {
			if err := os.RemoveAll(e.artifactDir); err != nil {
				t.Fatal(err)
			}
			actApplyMustWrite(t, e.artifactDir, string(e.content))
		}},
		{"artifact directory is a symlink to the correct directory", func(t *testing.T, e *actApplyEnv) {
			real := filepath.Join(e.optRoot, "real")
			if err := os.Rename(e.artifactDir, real); err != nil {
				t.Fatal(err)
			}
			actApplyMustSymlink(t, real, e.artifactDir)
		}},
		{"binary is a symlink to the correct file", func(t *testing.T, e *actApplyEnv) {
			real := filepath.Join(e.artifactDir, "real-binary")
			if err := os.Rename(e.binary, real); err != nil {
				t.Fatal(err)
			}
			actApplyMustSymlink(t, real, e.binary)
		}},
		{"binary is a symlink to an outside file", func(t *testing.T, e *actApplyEnv) {
			outside := filepath.Join(e.dataRoot, "outside")
			actApplyMustWrite(t, outside, string(e.content))
			actApplyChmod(t, outside, 0o555)
			if err := os.Remove(e.binary); err != nil {
				t.Fatal(err)
			}
			actApplyMustSymlink(t, outside, e.binary)
		}},
		{"binary is a directory", func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.binary); err != nil {
				t.Fatal(err)
			}
			actApplyMustMkdir(t, e.binary)
		}},
		{"binary is a fifo", func(t *testing.T, e *actApplyEnv) {
			if err := os.Remove(e.binary); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(e.binary, 0o555); err != nil {
				t.Skipf("no fifo here: %v", err)
			}
		}},
		{"binary is hardlinked", func(t *testing.T, e *actApplyEnv) {
			if err := os.Link(e.binary, filepath.Join(e.dataRoot, "alias")); err != nil {
				t.Skipf("no hardlink here: %v", err)
			}
		}},
		{"binary is hardlinked inside the artifact directory", func(t *testing.T, e *actApplyEnv) {
			if err := os.Link(e.binary, filepath.Join(e.artifactDir, "alias")); err != nil {
				t.Skipf("no hardlink here: %v", err)
			}
		}},
		{"size one byte larger", func(t *testing.T, e *actApplyEnv) { e.writeBinary(t, append(slices.Clone(e.content), 'x')) }},
		{"size one byte smaller", func(t *testing.T, e *actApplyEnv) { e.writeBinary(t, e.content[:len(e.content)-1]) }},
		{"empty binary", func(t *testing.T, e *actApplyEnv) { e.writeBinary(t, nil) }},
		{"same size, last byte differs", func(t *testing.T, e *actApplyEnv) {
			data := slices.Clone(e.content)
			data[len(data)-1] ^= 0x01
			e.writeBinary(t, data)
		}},
		{"same size, first byte differs", func(t *testing.T, e *actApplyEnv) {
			data := slices.Clone(e.content)
			data[0] ^= 0x01
			e.writeBinary(t, data)
		}},
		{"much larger binary", func(t *testing.T, e *actApplyEnv) { e.writeBinary(t, bytes.Repeat([]byte("A"), 1<<20)) }},
		{"intent pin hash differs from the file", func(t *testing.T, e *actApplyEnv) { e.intent.Identity.Binary.SHA256 = strings.Repeat("a", 64) }},
		{"intent pin size differs from the file", func(t *testing.T, e *actApplyEnv) { e.intent.Identity.Binary.Size++ }},
		{"intent pin names another release", func(t *testing.T, e *actApplyEnv) { e.intent.Identity.Binary.Version = "9.9.9" }},
		{"intent pin names another architecture", func(t *testing.T, e *actApplyEnv) { e.intent.Identity.Binary.Architecture = "arm64" }},
	}
	for _, mode := range []os.FileMode{0o755, 0o557, 0o556, 0o554, 0o455, 0o444, 0o500, 0o775, 0o777, 0o600, 0o000, os.ModeSetuid | 0o555, os.ModeSetgid | 0o555, os.ModeSticky | 0o555, os.ModeSetuid | 0o755} {
		cases = append(cases, tcase{"mode " + mode.String(), func(t *testing.T, e *actApplyEnv) { actApplyChmod(t, e.binary, mode) }})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newActApplyEnv(t)
			tc.setup(t, e)
			runner := newActApplyRunner(t)

			got, err := actApplyWithinInit(t, func() (ProbeInitResult, error) {
				return ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner)
			})
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnavailable)
			if got != (ProbeInitResult{}) {
				t.Fatalf("refusal returned %+v", got)
			}
			runner.requireNoCalls(t)
			hostActApplyRequireEntries(t, e.dataDir)
		})
	}
}

func TestApplyProbeInitRefusesInvalidCallsWithoutRunning(t *testing.T) {
	e := newActApplyEnv(t)
	runner := newActApplyRunner(t)
	refused := func(name string, got ProbeInitResult, err, want error) {
		t.Helper()
		if err != want || got != (ProbeInitResult{}) {
			t.Fatalf("%s: %+v %v, want %v", name, got, err, want)
		}
	}
	input := domain.ErrProbeBinaryStageWriteInput

	for name, intent := range hostActApplyWrongKindIntents() {
		got, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, intent, runner)
		refused("intent "+name, got, err, input)
	}
	for _, kind := range []string{domain.ProbeActivationServiceStart, domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "Probe_Init", "probe_init "} {
		intent := e.intent
		intent.Kind = kind
		got, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, intent, runner)
		refused("kind "+kind, got, err, input)
	}
	for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
		"empty owner":       func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed attempt": func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		"invalid pin":       func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
	} {
		intent := e.intent
		mutate(&intent)
		got, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, intent, runner)
		refused(name, got, err, input)
	}

	got, err := ApplyProbeInit(t.Context(), nil, e.optParent, e.intent, runner)
	refused("nil data parent", got, err, input)
	got, err = ApplyProbeInit(t.Context(), e.dataParent, nil, e.intent, runner)
	refused("nil opt parent", got, err, input)
	got, err = ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, nil)
	refused("nil runner", got, err, input)
	var typedNilRunner *actApplyRunner
	got, err = ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, typedNilRunner)
	refused("typed nil runner", got, err, input)
	var nilFunc actApplyRunnerFunc
	got, err = ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, nilFunc)
	refused("typed nil func runner", got, err, input)
	var nilCtx *sendTNilContext
	got, err = ApplyProbeInit(nilCtx, e.dataParent, e.optParent, e.intent, runner)
	refused("typed nil ctx", got, err, input)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = ApplyProbeInit(canceled, e.dataParent, e.optParent, e.intent, runner)
	refused("canceled ctx", got, err, context.Canceled)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	got, err = ApplyProbeInit(expired, e.dataParent, e.optParent, e.intent, runner)
	refused("expired ctx", got, err, context.DeadlineExceeded)

	runner.requireNoCalls(t)
	hostActApplyRequireEntries(t, e.dataDir)
	e.requireBinaryUntouched(t)
	hostActApplyRequireBorrowedParentOpen(t, e.dataParent)
	hostActApplyRequireBorrowedParentOpen(t, e.optParent)
}

// A cancellation observed after every pre-check passed but before the binary
// runs must still run nothing.
func TestApplyProbeInitRechecksCancellationImmediatelyBeforeRunning(t *testing.T) {
	e := newActApplyEnv(t)
	runner := newActApplyRunner(t)
	ctx := &actApplyFlipContext{Context: t.Context(), flipAt: 2}

	got, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, e.intent, runner)
	hostActApplyRequireErr(t, err, context.Canceled)
	if got != (ProbeInitResult{}) {
		t.Fatalf("result %+v", got)
	}
	runner.requireNoCalls(t)
	if ctx.calls.Load() < 2 {
		t.Fatalf("context checked %d times; the pre-run recheck is gone", ctx.calls.Load())
	}
}

func TestApplyProbeInitFailuresAfterTheBinaryRanAreUnknown(t *testing.T) {
	valid := probeInitRealBytes(t, nil)
	otherLine := probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = probeInitTestMakeToken(9) })
	type outcome struct {
		stdout []byte
		code   int
		err    error
		cancel bool
	}
	cases := map[string]outcome{
		"runner error":                        {err: errors.New("exec failed " + probeInitTestToken)},
		"runner error with valid stdout":      {stdout: valid, err: errors.New("killed")},
		"context error from runner":           {err: context.DeadlineExceeded},
		"exit 1 with valid stdout":            {stdout: valid, code: 1},
		"exit 2 with valid stdout":            {stdout: valid, code: 2},
		"exit 126":                            {stdout: valid, code: 126},
		"exit 127":                            {stdout: valid, code: 127},
		"exit 255":                            {stdout: valid, code: 255},
		"exit -1 (signal)":                    {stdout: valid, code: -1},
		"exit 1 without stdout":               {code: 1},
		"empty stdout":                        {},
		"only LF":                             {stdout: []byte("\n")},
		"not json":                            {stdout: []byte("enrollment token could not be issued\n")},
		"two lines":                           {stdout: append(slices.Clone(valid), otherLine...)},
		"invalid token":                       {stdout: probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = "short" })},
		"missing token":                       {stdout: probeInitRealBytes(t, func(o *probeInitRealOutput) { o.EnrollmentToken = "" })},
		"already bound (hub_id)":              {stdout: probeInitRealBytes(t, func(o *probeInitRealOutput) { o.HubID = probeInitTestOtherUID })},
		"local probe id":                      {stdout: probeInitRealBytes(t, func(o *probeInitRealOutput) { o.ProbeID = "local" })},
		"invalid fingerprint":                 {stdout: probeInitRealBytes(t, func(o *probeInitRealOutput) { o.Fingerprint = "abc" })},
		"unknown member":                      {stdout: []byte(strings.Replace(string(valid), `"last_created_seq"`, `"private_key":"x","last_created_seq"`, 1))},
		"valid output but ctx canceled in it": {stdout: valid, cancel: true},
		"valid output, ctx canceled, code 1":  {stdout: valid, code: 1, cancel: true},
	}
	for name, oc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newActApplyEnv(t)
			runner := newActApplyRunner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runner.script = func(context.Context, int, string, []string) ([]byte, int, error) {
				if oc.cancel {
					cancel()
				}
				return oc.stdout, oc.code, oc.err
			}

			got, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, e.intent, runner)
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnknown)
			if got != (ProbeInitResult{}) {
				t.Fatalf("a failed init returned %+v, so the token would escape", got)
			}
			if strings.Contains(err.Error(), probeInitTestToken) || strings.Contains(err.Error(), probeInitTestProbeID) {
				t.Fatalf("error leaks init material: %v", err)
			}
			calls := runner.recorded()
			if len(calls) != 1 || calls[0].path != e.wantBinaryPath() || !slices.Equal(calls[0].args, e.wantArgs()) {
				t.Fatalf("runner calls %+v, want exactly one exact call (no retry)", calls)
			}
		})
	}
}

// Once the binary ran, identity material may exist: the data directory is no
// longer empty, so a second application is a definite Conflict and runs nothing.
func TestApplyProbeInitIsOnceOnly(t *testing.T) {
	t.Run("after success", func(t *testing.T) {
		e := newActApplyEnv(t)
		runner := newActApplyRunner(t)
		runner.script = func(context.Context, int, string, []string) ([]byte, int, error) {
			actApplyMustWrite(t, filepath.Join(e.dataDir, "identity.db"), "identity")
			return probeInitRealBytes(t, nil), 0, nil
		}
		got, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner)
		hostActApplyRequireErr(t, err, nil)
		if got != probeInitTestResult() {
			t.Fatalf("first result %+v", got)
		}

		got, err = ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner)
		hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
		if got != (ProbeInitResult{}) {
			t.Fatalf("replay returned %+v", got)
		}
		if n := len(runner.recorded()); n != 1 {
			t.Fatalf("runner calls = %d, want 1: the binary must not run twice", n)
		}
		hostActApplyRequireEntries(t, e.dataDir, "identity.db")
	})

	t.Run("after an unknown outcome", func(t *testing.T) {
		e := newActApplyEnv(t)
		runner := newActApplyRunner(t)
		runner.script = func(context.Context, int, string, []string) ([]byte, int, error) {
			actApplyMustWrite(t, filepath.Join(e.dataDir, "identity.db"), "identity")
			return []byte("garbage\n"), 0, nil
		}
		if _, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner); err != domain.ErrProbeBinaryStageWriteUnknown {
			t.Fatalf("first call %v, want Unknown", err)
		}
		if _, err := ApplyProbeInit(t.Context(), e.dataParent, e.optParent, e.intent, runner); err != domain.ErrProbeBinaryStageWriteConflict {
			t.Fatalf("retry %v, want Conflict: an automatic retry could orphan a probe identity", err)
		}
		if n := len(runner.recorded()); n != 1 {
			t.Fatalf("runner calls = %d, want 1", n)
		}
	})
}

// ---- ApplyProbeServiceStart -------------------------------------------------

type actApplyAnswer struct {
	out  string
	code int
	err  error
}

// actApplyStartScript answers the exact start sequence: is-enabled, is-active
// before the start, start, then is-active polls (the last answer repeats).
// Any other verb, or a verb out of order, fails loudly.
func actApplyStartScript(enabled, preActive, start actApplyAnswer, polls ...actApplyAnswer) func(int, []string) (string, int, error) {
	return func(call int, args []string) (string, int, error) {
		answer := func(a actApplyAnswer) (string, int, error) { return a.out, a.code, a.err }
		switch {
		case call == 0 && len(args) == 2 && args[0] == "is-enabled" && args[1] == hostActApplyUnitName:
			return answer(enabled)
		case call == 1 && len(args) == 2 && args[0] == "is-active" && args[1] == hostActApplyUnitName:
			return answer(preActive)
		case call == 2 && len(args) == 2 && args[0] == "start" && args[1] == hostActApplyUnitName:
			return answer(start)
		case call >= 3 && len(args) == 2 && args[0] == "is-active" && args[1] == hostActApplyUnitName && len(polls) > 0:
			return answer(polls[min(call-3, len(polls)-1)])
		}
		return "", 127, errors.New("unexpected systemctl invocation")
	}
}

var (
	actApplyEnabled    = actApplyAnswer{out: "enabled\n"}
	actApplyInactive   = actApplyAnswer{out: "inactive\n", code: 3}
	actApplyStarted    = actApplyAnswer{}
	actApplyActive     = actApplyAnswer{out: "active\n"}
	actApplyActivating = actApplyAnswer{out: "activating\n", code: 3}
)

func actApplyStartIntent() domain.ProbeHostActionIntent {
	intent := hostActFrameIntent(domain.ProbeActivationServiceStart)
	intent.Identity.Binary = sendTPin([]byte(actApplyBinaryContent))
	return intent
}

func actApplyStartCalls(verbs ...string) [][]string {
	out := make([][]string, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, []string{v, hostActApplyUnitName})
	}
	return out
}

func actApplyCountVerb(ctl *hostActApplySystemctl, verb string) int {
	n := 0
	for _, call := range ctl.recorded() {
		if call[0] == verb {
			n++
		}
	}
	return n
}

func TestApplyProbeServiceStartHappyPathCallsExactSequence(t *testing.T) {
	intent := actApplyStartIntent()
	dir, parent := hostActApplyParent(t)
	unit := hostActApplyWriteUnit(t, dir, intent)
	ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActive)}
	ctx := context.WithValue(t.Context(), hostActApplyCtxKey{}, "sentinel")

	began := time.Now()
	hostActApplyRequireErr(t, ApplyProbeServiceStart(ctx, parent, intent, ctl), nil)
	if elapsed := time.Since(began); elapsed >= probeServicePoll {
		t.Fatalf("an already active unit waited %v; the first poll must be immediate", elapsed)
	}
	hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active"))

	// Fixed verbs and the bare unit name only: never a path, a flag, or any verb
	// that would enable, restart, reset or force.
	for _, call := range ctl.recorded() {
		if !slices.Contains([]string{"is-enabled", "is-active", "start"}, call[0]) {
			t.Fatalf("unexpected systemctl verb %q", call[0])
		}
		for _, arg := range call {
			if strings.HasPrefix(arg, "-") || strings.Contains(arg, "/") || arg == "--now" || arg == "restart" || arg == "enable" {
				t.Fatalf("unexpected systemctl argument %q in %v", arg, call)
			}
		}
	}
	for i, c := range ctl.ctxs {
		if c.Value(hostActApplyCtxKey{}) != "sentinel" {
			t.Fatalf("call %d did not receive the caller's context", i)
		}
	}
	hostActApplyRequireUnitUntouched(t, unit, hostActApplyUnit(intent), 0o644)
	hostActApplyRequireEntries(t, dir, hostActApplyUnitName)
	hostActApplyRequireBorrowedParentOpen(t, parent)
}

func TestApplyProbeServiceStartPollsUntilActive(t *testing.T) {
	t.Run("two activating polls first", func(t *testing.T) {
		intent := actApplyStartIntent()
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActivating, actApplyActivating, actApplyActive)}
		began := time.Now()
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), nil)
		if elapsed := time.Since(began); elapsed < 2*probeServicePoll-100*time.Millisecond {
			t.Fatalf("returned after %v without waiting between polls", elapsed)
		}
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active", "is-active", "is-active"))
	})
	t.Run("a failed poll does not end the wait", func(t *testing.T) {
		intent := actApplyStartIntent()
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyAnswer{code: 1, err: errors.New("transient")}, actApplyActive)}
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), nil)
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active", "is-active"))
	})
	t.Run("a failed unit before start is restarted once", func(t *testing.T) {
		intent := actApplyStartIntent()
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyAnswer{out: "failed\n", code: 3}, actApplyStarted, actApplyActive)}
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), nil)
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active"))
	})
}

func TestApplyProbeServiceStartRefusesAUnitThatIsNotExactlyDerivedWithoutAnySystemctlCall(t *testing.T) {
	intent := actApplyStartIntent()
	want := hostActApplyUnit(intent)
	otherPin := intent
	// Only the release name (version, architecture) reaches ExecStart; a pin that
	// differs just in hash and size derives the very same unit.
	otherPin.Identity.Binary = sendTPin([]byte("another pinned binary"))
	otherPin.Identity.Binary.Version = "9.9.9"
	otherOp := intent
	otherOp.Identity.OperationID = hostActFrameOtherOperation

	type tcase struct {
		name  string
		write func(t *testing.T, dir string) string
	}
	raw := func(data []byte, mode os.FileMode) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string {
			p := filepath.Join(dir, hostActApplyUnitName)
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			actApplyChmod(t, p, mode)
			return p
		}
	}
	cases := []tcase{
		{"missing", func(t *testing.T, dir string) string { return "" }},
		{"empty file", raw(nil, 0o644)},
		{"one byte short", raw(want[:len(want)-1], 0o644)},
		{"one byte longer", raw(append(slices.Clone(want), '\n'), 0o644)},
		{"one byte flipped", raw(append(slices.Clone(want[:len(want)-2]), 'X', want[len(want)-1]), 0o644)},
		{"unit of another binary pin", raw([]byte(domain.ProbeSystemdUnit(otherPin.Identity)), 0o644)},
		{"unit of another operation", raw([]byte(domain.ProbeSystemdUnit(otherOp.Identity)), 0o644)},
		{"operator edited ExecStart", raw([]byte(strings.Replace(string(want), "ExecStart=", "ExecStart=/bin/sh -c id; ", 1)), 0o644)},
		{"exact bytes, mode 0600", raw(want, 0o600)},
		{"exact bytes, mode 0664", raw(want, 0o664)},
		{"exact bytes, mode 0666", raw(want, 0o666)},
		{"exact bytes, mode 0755", raw(want, 0o755)},
		{"exact bytes, setuid", raw(want, os.ModeSetuid|0o644)},
		{"oversize", raw(bytes.Repeat([]byte("x"), hostActionUnitMaxRead+1), 0o644)},
		{"directory", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, hostActApplyUnitName)
			actApplyMustMkdir(t, p)
			return p
		}},
		{"fifo", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, hostActApplyUnitName)
			actApplyMustMkfifo(t, p)
			return p
		}},
		{"symlink to the exact unit", func(t *testing.T, dir string) string {
			real := filepath.Join(dir, "real.service")
			if err := os.WriteFile(real, want, 0o600); err != nil {
				t.Fatal(err)
			}
			actApplyChmod(t, real, 0o644)
			p := filepath.Join(dir, hostActApplyUnitName)
			actApplyMustSymlink(t, real, p)
			return p
		}},
		{"hardlinked exact unit", func(t *testing.T, dir string) string {
			p := raw(want, 0o644)(t, dir)
			if err := os.Link(p, filepath.Join(t.TempDir(), "alias")); err != nil {
				t.Skipf("no hardlink here: %v", err)
			}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			tc.write(t, dir)
			ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActive)}
			err := hostActApplyWithin(t, "service start", func() error {
				return ApplyProbeServiceStart(t.Context(), parent, intent, ctl)
			})
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnavailable)
			hostActApplyRequireCalls(t, ctl, nil)
		})
	}
}

func TestApplyProbeServiceStartRequiresExactlyEnabled(t *testing.T) {
	for name, answer := range map[string]actApplyAnswer{
		"disabled":                {out: "disabled\n", code: 1},
		"enabled but exit 1":      {out: "enabled\n", code: 1},
		"enabled without LF":      {out: "enabled"},
		"enabled with two LF":     {out: "enabled\n\n"},
		"enabled-runtime":         {out: "enabled-runtime\n"},
		"static":                  {out: "static\n"},
		"alias":                   {out: "alias\n"},
		"masked":                  {out: "masked\n", code: 1},
		"linked":                  {out: "linked\n"},
		"empty output exit 0":     {},
		"not-found":               {out: "not-found\n", code: 4},
		"systemctl error":         {err: errors.New("systemctl unavailable")},
		"systemctl error exit 0":  {out: "enabled\n", err: errors.New("pipe closed")},
		"uppercase":               {out: "ENABLED\n"},
		"enabled plus unit state": {out: "enabled\nactive\n"},
	} {
		t.Run(name, func(t *testing.T) {
			intent := actApplyStartIntent()
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: actApplyStartScript(answer, actApplyInactive, actApplyStarted, actApplyActive)}
			hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
			hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled"))
		})
	}
}

func TestApplyProbeServiceStartActiveStateBeforeStart(t *testing.T) {
	t.Run("already active is Conflict and never starts", func(t *testing.T) {
		for name, answer := range map[string]actApplyAnswer{
			"active":     actApplyActive,
			"activating": {out: "activating\n"},
			"reloading":  {out: "reloading\n"},
			"bare exit0": {},
		} {
			intent := actApplyStartIntent()
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, answer, actApplyStarted, actApplyActive)}
			hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteConflict)
			if got := actApplyCountVerb(ctl, "start"); got != 0 {
				t.Fatalf("%s: start was called %d times for an active unit", name, got)
			}
			hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active"))
		}
	})
	t.Run("is-active error before start is Unavailable and never starts", func(t *testing.T) {
		intent := actApplyStartIntent()
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyAnswer{err: errors.New("bus down")}, actApplyStarted, actApplyActive)}
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active"))
	})
}

func TestApplyProbeServiceStartFailingStartIsUnknownWithoutPolling(t *testing.T) {
	for name, answer := range map[string]actApplyAnswer{
		"exit 1":        {code: 1},
		"exit 5":        {out: "Failed to start", code: 5},
		"signal":        {code: -1},
		"runner error":  {err: errors.New("dbus timeout")},
		"error, exit 0": {err: errors.New("lost reply")},
	} {
		t.Run(name, func(t *testing.T) {
			intent := actApplyStartIntent()
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, answer, actApplyActive)}
			began := time.Now()
			hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
			if elapsed := time.Since(began); elapsed >= probeServicePoll {
				t.Fatalf("a failed start waited %v", elapsed)
			}
			hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start"))
		})
	}
}

// Anything but "active\n" with exit 0 keeps polling, and a wait that ends
// (here by the caller's short deadline) is Unknown: the unit may still be coming up.
func TestApplyProbeServiceStartNeverActiveIsUnknown(t *testing.T) {
	for name, poll := range map[string]actApplyAnswer{
		"activating":              actApplyActivating,
		"inactive after start":    actApplyInactive,
		"failed":                  {out: "failed\n", code: 3},
		"active text but exit 3":  {out: "active\n", code: 3},
		"active without LF":       {out: "active"},
		"active with two LF":      {out: "active\n\n"},
		"uppercase active":        {out: "ACTIVE\n"},
		"reloading exit 0":        {out: "reloading\n"},
		"empty output exit 0":     {},
		"systemctl error":         {err: errors.New("bus down")},
		"error with active text":  {out: "active\n", err: errors.New("pipe closed")},
		"active plus extra state": {out: "active\nfailed\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			intent := actApplyStartIntent()
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, poll)}
			ctx, cancel := context.WithTimeout(t.Context(), 800*time.Millisecond)
			defer cancel()

			began := time.Now()
			err := ApplyProbeServiceStart(ctx, parent, intent, ctl)
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnknown)
			if elapsed := time.Since(began); elapsed > 5*time.Second {
				t.Fatalf("the deadline took %v to end the wait", elapsed)
			}
			if got := actApplyCountVerb(ctl, "start"); got != 1 {
				t.Fatalf("start called %d times, want exactly 1", got)
			}
			if polls := actApplyCountVerb(ctl, "is-active") - 1; polls < 2 || polls > 4 {
				t.Fatalf("polled %d times after start in 800ms", polls)
			}
			for _, call := range ctl.recorded()[3:] {
				if !slices.Equal(call, []string{"is-active", hostActApplyUnitName}) {
					t.Fatalf("unexpected poll %v", call)
				}
			}
		})
	}
}

// The production wait is 20 s. This runs it for real, once, and is skipped
// under -short. It is parallel so it overlaps the rest of the package.
func TestApplyProbeServiceStartGivesUpAfterTheTwentySecondWait(t *testing.T) {
	if testing.Short() {
		t.Skip("takes the real 20 second wait")
	}
	t.Parallel()
	intent := actApplyStartIntent()
	dir, parent := hostActApplyParent(t)
	hostActApplyWriteUnit(t, dir, intent)
	ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActivating)}

	// The caller's deadline is only a fail-safe: if the production wait were ever
	// lost this test would fail on elapsed time instead of hanging the suite.
	ctx, cancel := context.WithTimeout(t.Context(), 2*probeServiceStartWait)
	defer cancel()
	began := time.Now()
	err := ApplyProbeServiceStart(ctx, parent, intent, ctl)
	elapsed := time.Since(began)
	hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnknown)
	if elapsed < probeServiceStartWait-time.Second || elapsed > probeServiceStartWait+5*time.Second {
		t.Fatalf("gave up after %v, want about %v", elapsed, probeServiceStartWait)
	}
	polls := actApplyCountVerb(ctl, "is-active") - 1
	if polls < 20 || polls > int(probeServiceStartWait/probeServicePoll)+3 {
		t.Fatalf("polled %d times in %v", polls, elapsed)
	}
	if got := actApplyCountVerb(ctl, "start"); got != 1 {
		t.Fatalf("start called %d times", got)
	}
}

func TestApplyProbeServiceStartCancellationIsUnknownAndPrompt(t *testing.T) {
	setup := func(t *testing.T, script func(int, []string) (string, int, error)) (*os.File, domain.ProbeHostActionIntent, *hostActApplySystemctl) {
		intent := actApplyStartIntent()
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		return parent, intent, &hostActApplySystemctl{script: script}
	}

	t.Run("canceled during the first poll", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		base := actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActivating)
		parent, intent, ctl := setup(t, func(call int, args []string) (string, int, error) {
			if call == 3 {
				cancel()
			}
			return base(call, args)
		})
		began := time.Now()
		hostActApplyRequireErr(t, ApplyProbeServiceStart(ctx, parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
		if elapsed := time.Since(began); elapsed >= probeServicePoll {
			t.Fatalf("returned after %v", elapsed)
		}
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active"))
	})

	t.Run("canceled while waiting between polls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		parent, intent, ctl := setup(t, actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActivating))
		time.AfterFunc(150*time.Millisecond, cancel)
		began := time.Now()
		hostActApplyRequireErr(t, ApplyProbeServiceStart(ctx, parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
		if elapsed := time.Since(began); elapsed >= probeServicePoll {
			t.Fatalf("a cancel during the wait took %v to be honored", elapsed)
		}
		if got := actApplyCountVerb(ctl, "is-active"); got != 2 {
			t.Fatalf("is-active calls = %d, want the pre-start check and one poll", got)
		}
	})

	t.Run("canceled right after start with a context-aware systemctl", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		base := actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActive)
		parent, intent, ctl := setup(t, func(call int, args []string) (string, int, error) {
			if ctx.Err() != nil {
				return "", -1, ctx.Err()
			}
			out, code, err := base(call, args)
			if call == 2 {
				cancel()
			}
			return out, code, err
		})
		hostActApplyRequireErr(t, ApplyProbeServiceStart(ctx, parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
		hostActApplyRequireCalls(t, ctl, actApplyStartCalls("is-enabled", "is-active", "start", "is-active"))
	})
}

func TestApplyProbeServiceStartRefusesInvalidCallsWithoutAnySystemctlCall(t *testing.T) {
	intent := actApplyStartIntent()
	dir, parent := hostActApplyParent(t)
	hostActApplyWriteUnit(t, dir, intent)
	ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActive)}
	input := domain.ErrProbeBinaryStageWriteInput

	for _, bad := range hostActApplyWrongKindIntents() {
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, bad, ctl), input)
	}
	for _, kind := range []string{domain.ProbeActivationProbeInit, hostActFrameKindService, domain.ProbeActivationRegistration, domain.ProbeActivationEnrollment, "Service_Start", "service_start "} {
		bad := intent
		bad.Kind = kind
		hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, bad, ctl), input)
	}
	for name, mutate := range map[string]func(*domain.ProbeHostActionIntent){
		"empty owner":       func(i *domain.ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed attempt": func(i *domain.ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		"invalid pin":       func(i *domain.ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
	} {
		bad := intent
		mutate(&bad)
		if err := ApplyProbeServiceStart(t.Context(), parent, bad, ctl); err != input {
			t.Fatalf("%s: %v", name, err)
		}
	}
	hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), nil, intent, ctl), input)
	hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, nil), input)
	var typedNil *hostActApplySystemctl
	hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, typedNil), input)
	var nilCtx *sendTNilContext
	hostActApplyRequireErr(t, ApplyProbeServiceStart(nilCtx, parent, intent, ctl), input)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	hostActApplyRequireErr(t, ApplyProbeServiceStart(canceled, parent, intent, ctl), context.Canceled)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	hostActApplyRequireErr(t, ApplyProbeServiceStart(expired, parent, intent, ctl), context.DeadlineExceeded)

	hostActApplyRequireCalls(t, ctl, nil)
	hostActApplyRequireBorrowedParentOpen(t, parent)
}

func TestApplyProbeServiceStartWithAnUnusableParentIsUnavailable(t *testing.T) {
	intent := actApplyStartIntent()
	dir, parent := hostActApplyParent(t)
	hostActApplyWriteUnit(t, dir, intent)
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	ctl := &hostActApplySystemctl{script: actApplyStartScript(actApplyEnabled, actApplyInactive, actApplyStarted, actApplyActive)}
	hostActApplyRequireErr(t, ApplyProbeServiceStart(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
	hostActApplyRequireCalls(t, ctl, nil)
}

// ---- lifecycle --------------------------------------------------------------

// actApplyUnitHost is a stateful fake of the one unit's systemd state.
type actApplyUnitHost struct {
	mu      sync.Mutex
	enabled bool
	active  bool
	calls   [][]string
}

func (s *actApplyUnitHost) Run(_ context.Context, args ...string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(args))
	switch {
	case len(args) == 2 && args[0] == "is-enabled":
		if s.enabled {
			return "enabled\n", 0, nil
		}
		return "disabled\n", 1, nil
	case len(args) == 1 && args[0] == "daemon-reload":
		return "", 0, nil
	case len(args) == 2 && args[0] == "enable":
		s.enabled = true
		return "", 0, nil
	case len(args) == 2 && args[0] == "is-active":
		if s.active {
			return "active\n", 0, nil
		}
		return "inactive\n", 3, nil
	case len(args) == 2 && args[0] == "start":
		if !s.enabled {
			return "", 5, nil
		}
		s.active = true
		return "", 0, nil
	}
	return "", 127, errors.New("unexpected systemctl invocation")
}

func (s *actApplyUnitHost) count(verb string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c[0] == verb {
			n++
		}
	}
	return n
}

// The whole host side of an install followed by an activation, with the real
// appliers over temp parents: out of order steps are refused before they act,
// and every replay is a definite Conflict that runs nothing.
func TestActivationHostLifecycleInitThenStart(t *testing.T) {
	e := newActApplyEnv(t)
	if err := os.Remove(e.dataDir); err != nil { // the install applier creates it below
		t.Fatal(err)
	}
	unitRoot, unitParent := hostActApplyParent(t)
	identity := e.intent.Identity
	intentFor := func(kind string) domain.ProbeHostActionIntent {
		return domain.ProbeHostActionIntent{Identity: identity, OwnerID: hostActFrameOwnerID, AttemptID: hostActFrameAttemptID, Kind: kind}
	}
	host := &actApplyUnitHost{}
	runner := newActApplyRunner(t)
	runner.script = func(context.Context, int, string, []string) ([]byte, int, error) {
		actApplyMustWrite(t, filepath.Join(e.dataDir, "identity.db"), "identity")
		return probeInitRealBytes(t, nil), 0, nil
	}
	ctx := t.Context()
	initIntent, startIntent := intentFor(domain.ProbeActivationProbeInit), intentFor(domain.ProbeActivationServiceStart)

	// Out of order: nothing exists yet.
	if _, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, initIntent, runner); err != domain.ErrProbeBinaryStageWriteUnavailable {
		t.Fatalf("init without a data directory: %v", err)
	}
	if err := ApplyProbeServiceStart(ctx, unitParent, startIntent, host); err != domain.ErrProbeBinaryStageWriteUnavailable {
		t.Fatalf("start without a unit: %v", err)
	}

	// The install's host steps.
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(ctx, e.dataParent, intentFor(hostActFrameKindData)), nil)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(ctx, unitParent, intentFor(hostActFrameKindUnit)), nil)
	if err := ApplyProbeServiceStart(ctx, unitParent, startIntent, host); err != domain.ErrProbeBinaryStageWriteUnavailable {
		t.Fatalf("start before the unit is enabled: %v", err)
	}
	hostActApplyRequireErr(t, ApplyProbeService(ctx, unitParent, intentFor(hostActFrameKindService), host), nil)
	if host.count("start") != 0 {
		t.Fatal("a premature start reached systemd")
	}

	// Activation: init, then start.
	got, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, initIntent, runner)
	hostActApplyRequireErr(t, err, nil)
	if got != probeInitTestResult() {
		t.Fatalf("init result %+v", got)
	}
	hostActApplyRequireErr(t, ApplyProbeServiceStart(ctx, unitParent, startIntent, host), nil)
	if host.count("start") != 1 || !host.active {
		t.Fatalf("start calls %d active %v", host.count("start"), host.active)
	}

	// Every replay is a definite Conflict and does nothing.
	for name, err := range map[string]error{
		"data directory": ApplyProbeDataDirectory(ctx, e.dataParent, intentFor(hostActFrameKindData)),
		"unit file":      ApplyProbeUnitFile(ctx, unitParent, intentFor(hostActFrameKindUnit)),
		"service":        ApplyProbeService(ctx, unitParent, intentFor(hostActFrameKindService), host),
		"service start":  ApplyProbeServiceStart(ctx, unitParent, startIntent, host),
	} {
		if err != domain.ErrProbeBinaryStageWriteConflict {
			t.Fatalf("replay of %s: %v, want Conflict", name, err)
		}
	}
	if _, err := ApplyProbeInit(ctx, e.dataParent, e.optParent, initIntent, runner); err != domain.ErrProbeBinaryStageWriteConflict {
		t.Fatalf("replay of probe init: %v, want Conflict", err)
	}
	if n := len(runner.recorded()); n != 1 || host.count("start") != 1 {
		t.Fatalf("replays acted: runner calls %d, start calls %d", n, host.count("start"))
	}
	hostActApplyRequireEntries(t, e.dataDir, "identity.db")
	hostActApplyRequireEntries(t, unitRoot, hostActApplyUnitName)
}

// ---- small helpers ----------------------------------------------------------

func actApplyMustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func actApplyMustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func actApplyMustSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("no symlink here: %v", err)
	}
}

func actApplyMustMkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
}

// actApplyWithinInit is hostActApplyWithin for calls that return a result.
func actApplyWithinInit(t *testing.T, fn func() (ProbeInitResult, error)) (ProbeInitResult, error) {
	t.Helper()
	var result ProbeInitResult
	err := hostActApplyWithin(t, "probe init", func() error {
		var err error
		result, err = fn()
		return err
	})
	return result, err
}
