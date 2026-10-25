package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Tests for the single forced command of a restricted staging key. The two
// real verbs cannot run here (they need uid 0 on Linux and the host's key and
// sshd files), but off Linux, and on Linux without uid 0, both refuse before
// any challenge with the Unavailable sentinel. That is observably different
// from the dispatch usage (exit 2), which is what proves a command was routed
// to a verb and not refused by the dispatcher. No test here reads or writes a
// real host path.

var dispatchExactCommands = []string{provisioning.SSHProbeBinaryStageAuthorizedCommand, provisioning.SSHProbeHostActionCommand}

// dispatchSkipIfRealVerbWouldTouchHost skips only the cases that would run a
// real verb past its uid check, which would read /etc/phoenix-probe and /etc/ssh.
func dispatchSkipIfRealVerbWouldTouchHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("uid 0 on linux would let the real verbs read the host's /etc/phoenix-probe and /etc/ssh")
	}
}

type dispatchRun struct {
	input  *hostMockInput
	out    bytes.Buffer
	stderr bytes.Buffer
	code   int
}

func runDispatch(ctx context.Context, args []string, command string, closeErr error) *dispatchRun {
	r := &dispatchRun{input: &hostMockInput{closeErr: closeErr}}
	r.code = RunAuthorizedProbeDispatch(ctx, args, command, r.input, &r.out, &r.stderr)
	return r
}

// wantUsageRefusal asserts the dispatcher's whole refusal contract: exit 2, the
// exact fixed usage as the only output, nothing on stdout, the input closed
// exactly once and never read.
func (r *dispatchRun) wantUsageRefusal(t *testing.T, label string) {
	t.Helper()
	if r.code != 2 || r.out.Len() != 0 || r.stderr.String() != probeAuthorizedDispatchUsage {
		t.Fatalf("%s: code=%d stdout=%q stderr=%q, want exit 2 with only the fixed usage", label, r.code, r.out.String(), r.stderr.String())
	}
	if r.input.closes.Load() != 1 || r.input.reads.Load() != 0 {
		t.Fatalf("%s: input closes=%d reads=%d, want 1/0", label, r.input.closes.Load(), r.input.reads.Load())
	}
}

// wantVerbRefusal asserts the command reached a real verb, which refused
// before any challenge: exit 1, the one Unavailable line (never the dispatch
// usage), no stdout, input closed once and never read.
func (r *dispatchRun) wantVerbRefusal(t *testing.T, label string, want error) {
	t.Helper()
	if r.code != 1 || r.out.Len() != 0 || r.stderr.String() != want.Error()+"\n" {
		t.Fatalf("%s: code=%d stdout=%q stderr=%q, want exit 1 with %q", label, r.code, r.out.String(), r.stderr.String(), want.Error())
	}
	if strings.Contains(r.stderr.String(), "Usage") || strings.Contains(r.stderr.String(), "authorized_keys") {
		t.Fatalf("%s: a routed command was answered with dispatch usage: %q", label, r.stderr.String())
	}
	if r.input.closes.Load() != 1 || r.input.reads.Load() != 0 {
		t.Fatalf("%s: input closes=%d reads=%d, want 1/0", label, r.input.closes.Load(), r.input.reads.Load())
	}
}

func TestAuthorizedProbeDispatchPinsTheSSHContract(t *testing.T) {
	if provisioning.SSHProbeBinaryStageAuthorizedCommand != "/usr/local/bin/phoenix-probe-admin stage-binary-authorized" ||
		provisioning.SSHProbeHostActionCommand != "/usr/local/bin/phoenix-probe-admin stage-host-action-authorized" {
		t.Fatalf("the fixed SSH commands drifted: %q %q", provisioning.SSHProbeBinaryStageAuthorizedCommand, provisioning.SSHProbeHostActionCommand)
	}
	if provisioning.SSHProbeBinaryStageAuthorizedCommand == provisioning.SSHProbeHostActionCommand {
		t.Fatal("the two verbs must have distinct commands")
	}
	for _, c := range dispatchExactCommands {
		if c != strings.TrimSpace(c) || strings.Contains(c, "  ") || strings.ContainsAny(c, "\t\r\n\x00;&|$`'\"\\") {
			t.Fatalf("command %q must be a single-spaced plain word list", c)
		}
	}
	if !strings.Contains(probeAuthorizedDispatchUsage, `command="/usr/local/bin/phoenix-probe-admin authorized-dispatch"`) ||
		!strings.Contains(probeAuthorizedDispatchUsage, "restrict") || !strings.Contains(probeAuthorizedDispatchUsage, "SSH_ORIGINAL_COMMAND") ||
		!strings.HasSuffix(probeAuthorizedDispatchUsage, "\n") || strings.Count(probeAuthorizedDispatchUsage, "\n") != 2 {
		t.Fatalf("usage text drifted: %q", probeAuthorizedDispatchUsage)
	}
	// The usage never names a secret-bearing path or either verb's full command.
	for _, c := range dispatchExactCommands {
		if strings.Contains(probeAuthorizedDispatchUsage, c) {
			t.Fatalf("usage names the command %q", c)
		}
	}
}

func TestRunAuthorizedProbeDispatchRoutesExactCommandsToAVerbNotTheUsage(t *testing.T) {
	dispatchSkipIfRealVerbWouldTouchHost(t)
	for _, command := range dispatchExactCommands {
		t.Run(command, func(t *testing.T) {
			r := runDispatch(context.Background(), nil, command, nil)
			r.wantVerbRefusal(t, "nil args", domain.ErrProbeBinaryStageWriteUnavailable)
			// An empty, non-nil argument list is still no arguments.
			r = runDispatch(context.Background(), []string{}, command, nil)
			r.wantVerbRefusal(t, "empty args", domain.ErrProbeBinaryStageWriteUnavailable)
		})
	}
}

func TestRunAuthorizedProbeDispatchVerbsStillOwnTheirOwnRefusals(t *testing.T) {
	// These refuse before any host access, so they also run as root on Linux.
	for _, command := range dispatchExactCommands {
		t.Run(command, func(t *testing.T) {
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			runDispatch(canceled, nil, command, nil).wantVerbRefusal(t, "canceled ctx", context.Canceled)

			// A nil input reaches the verb's own nil guard: exit 1, not the usage.
			var out, stderr bytes.Buffer
			if code := RunAuthorizedProbeDispatch(context.Background(), nil, command, nil, &out, &stderr); code != 1 || out.Len() != 0 ||
				stderr.String() != domain.ErrProbeBinaryStageWriteInput.Error()+"\n" {
				t.Fatalf("nil input code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
			}
			// A nil stderr is tolerated by the verb as well.
			input := &hostMockInput{}
			if code := RunAuthorizedProbeDispatch(canceled, nil, command, input, &out, nil); code != 1 || out.Len() != 0 || input.closes.Load() != 1 {
				t.Fatalf("nil stderr code=%d stdout=%q closes=%d", code, out.String(), input.closes.Load())
			}
		})
	}
}

func TestRunAuthorizedProbeDispatchNonEmptyArgsAreRefusedEvenForAnExactCommand(t *testing.T) {
	commands := append([]string{"", "stage-binary-host", "sensitive-canary"}, dispatchExactCommands...)
	for _, args := range [][]string{{"--help"}, {""}, {"a", "b"}, {"stage-binary-authorized"}, {"sensitive-canary"}} {
		for _, command := range commands {
			r := runDispatch(context.Background(), args, command, nil)
			r.wantUsageRefusal(t, "args "+strings.Join(args, " ")+" command "+command)
			if strings.Contains(r.stderr.String(), "canary") || strings.Contains(r.stderr.String(), "--help") {
				t.Fatalf("args %q leaked into the usage: %q", args, r.stderr.String())
			}
		}
	}
}

func TestRunAuthorizedProbeDispatchRefusesEveryOtherCommandWithoutEchoingIt(t *testing.T) {
	binary, hostAction := provisioning.SSHProbeBinaryStageAuthorizedCommand, provisioning.SSHProbeHostActionCommand
	const canary = "sensitive-canary"
	commands := map[string]string{
		"empty":                            "",
		"one space":                        " ",
		"newline only":                     "\n",
		"binary trailing space":            binary + " ",
		"binary leading space":             " " + binary,
		"binary trailing spaces":           binary + "  ",
		"binary trailing tab":              binary + "\t",
		"binary trailing LF":               binary + "\n",
		"binary trailing CR":               binary + "\r",
		"binary trailing NUL":              binary + "\x00",
		"binary NUL inside":                strings.Replace(binary, " ", "\x00", 1),
		"binary double space":              strings.Replace(binary, " ", "  ", 1),
		"binary tab separator":             strings.Replace(binary, " ", "\t", 1),
		"binary appended flag":             binary + " --help",
		"binary appended argument":         binary + " " + canary,
		"binary semicolon":                 binary + "; echo " + canary,
		"binary and-and":                   binary + " && echo " + canary,
		"binary pipe":                      binary + " | sh -c " + canary,
		"binary command substitution":      binary + " $(touch " + canary + ")",
		"binary backticks":                 binary + " `touch " + canary + "`",
		"binary redirect":                  binary + " > /tmp/" + canary,
		"binary environment prefix":        "FOO=" + canary + " " + binary,
		"binary relative":                  "phoenix-probe-admin stage-binary-authorized",
		"binary dot dot path":              "/usr/local/bin/../bin/phoenix-probe-admin stage-binary-authorized",
		"binary uppercased":                strings.ToUpper(binary),
		"binary fullwidth space":           strings.Replace(binary, " ", "　", 1),
		"binary prefix only":               binary[:len(binary)-1],
		"host action trailing space":       hostAction + " ",
		"host action leading space":        " " + hostAction,
		"host action appended argument":    hostAction + " " + canary,
		"host action semicolon":            hostAction + "; echo " + canary,
		"host action command substitution": hostAction + " $(touch " + canary + ")",
		"host action double space":         strings.Replace(hostAction, " ", "  ", 1),
		"host action uppercased":           strings.ToUpper(hostAction),
		"host action prefix only":          hostAction[:len(hostAction)-1],
		"both verbs semicolon":             binary + "; " + hostAction,
		"both verbs and-and":               binary + " && " + hostAction,
		"both verbs newline":               binary + "\n" + hostAction,
		"stage-binary-host bare":           "stage-binary-host",
		"stage-binary-host full":           "/usr/local/bin/phoenix-probe-admin stage-binary-host",
		"stage-binary full":                "/usr/local/bin/phoenix-probe-admin stage-binary",
		"stage-binary-authorized bare":     "stage-binary-authorized",
		"stage-host-action bare":           "stage-host-action-authorized",
		"authorized-dispatch recursion":    "/usr/local/bin/phoenix-probe-admin authorized-dispatch",
		"stage-authority":                  "/usr/local/bin/phoenix-probe-admin stage-authority",
		"verify-artifact":                  "/usr/local/bin/phoenix-probe-admin verify-artifact",
		"inspect-host":                     "/usr/local/bin/phoenix-probe-admin inspect-host",
		"shell canary":                     "sh -c '" + canary + "'",
		"interactive shell":                "/bin/sh",
		"scp sink":                         "scp -t /tmp/" + canary,
		"sftp subsystem":                   "internal-sftp",
		"id":                               "id",
		"oversized":                        strings.Repeat(canary, 64*1024/len(canary)),
		"oversized with valid prefix":      binary + strings.Repeat(" "+canary, 4096),
	}
	for name, command := range commands {
		if command == binary || command == hostAction {
			t.Fatalf("%s: fixture is accidentally an exact command", name)
		}
		t.Run(name, func(t *testing.T) {
			r := runDispatch(context.Background(), nil, command, nil)
			r.wantUsageRefusal(t, name)
			if strings.Contains(r.stderr.String(), canary) || strings.Contains(r.out.String(), canary) {
				t.Fatalf("the requested command was echoed: %q", r.stderr.String())
			}
			// The fixed usage names the dispatcher itself; any other command text is
			// absent from it, which the exact-equality assertion above already proves.
			if strings.TrimSpace(command) != "" && !strings.Contains(probeAuthorizedDispatchUsage, command) && strings.Contains(r.stderr.String(), command) {
				t.Fatalf("the requested command was echoed into the usage: %q", r.stderr.String())
			}
		})
	}
}

func TestRunAuthorizedProbeDispatchInputCloseFailureOutranksTheUsage(t *testing.T) {
	canary := errors.New("sensitive-close-canary")
	for name, tc := range map[string]struct {
		args    []string
		command string
	}{
		"unknown command":      {nil, "id"},
		"empty command":        {nil, ""},
		"exact with arguments": {[]string{"--help"}, provisioning.SSHProbeBinaryStageAuthorizedCommand},
	} {
		r := runDispatch(context.Background(), tc.args, tc.command, canary)
		if r.code != 1 || r.out.Len() != 0 || r.stderr.String() != domain.ErrProbeArtifactStorage.Error()+"\n" ||
			strings.Contains(r.stderr.String(), "canary") || strings.Contains(r.stderr.String(), "Usage") {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q, want exit 1 with the bare storage refusal", name, r.code, r.out.String(), r.stderr.String())
		}
		if r.input.closes.Load() != 1 || r.input.reads.Load() != 0 {
			t.Fatalf("%s: input closes=%d reads=%d, want 1/0", name, r.input.closes.Load(), r.input.reads.Load())
		}
	}
}

func TestRunAuthorizedProbeDispatchRefusalToleratesNilDependencies(t *testing.T) {
	var typedInput io.ReadCloser = (*hostMockInput)(nil)
	var typedCtx context.Context = (*hostMockCtx)(nil)
	var untypedCtx context.Context

	// A typed-nil or nil input: nothing to close, still a usage refusal.
	for name, input := range map[string]io.ReadCloser{"typed-nil input": typedInput, "nil input": nil} {
		var out, stderr bytes.Buffer
		if code := RunAuthorizedProbeDispatch(context.Background(), nil, "id", input, &out, &stderr); code != 2 || out.Len() != 0 || stderr.String() != probeAuthorizedDispatchUsage {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", name, code, out.String(), stderr.String())
		}
	}
	// A nil or typed-nil stderr is never written to; the exit code still says refused.
	for name, stderr := range map[string]io.Writer{"nil stderr": nil, "typed-nil stderr": (*bytes.Buffer)(nil)} {
		input := &hostMockInput{}
		var out bytes.Buffer
		if code := RunAuthorizedProbeDispatch(context.Background(), nil, "id", input, &out, stderr); code != 2 || out.Len() != 0 || input.closes.Load() != 1 || input.reads.Load() != 0 {
			t.Fatalf("%s: code=%d stdout=%q closes=%d reads=%d", name, code, out.String(), input.closes.Load(), input.reads.Load())
		}
	}
	// stdout is never touched, so a nil stdout is harmless on a refusal.
	for name, out := range map[string]io.Writer{"nil stdout": nil, "typed-nil stdout": (*bytes.Buffer)(nil)} {
		input := &hostMockInput{}
		var stderr bytes.Buffer
		if code := RunAuthorizedProbeDispatch(context.Background(), nil, "id", input, out, &stderr); code != 2 || stderr.String() != probeAuthorizedDispatchUsage || input.closes.Load() != 1 {
			t.Fatalf("%s: code=%d stderr=%q closes=%d", name, code, stderr.String(), input.closes.Load())
		}
	}
	// A nil context does not matter to a refusal that never uses it.
	for name, ctx := range map[string]context.Context{"nil ctx": untypedCtx, "typed-nil ctx": typedCtx} {
		input := &hostMockInput{}
		var out, stderr bytes.Buffer
		if code := RunAuthorizedProbeDispatch(ctx, nil, "id", input, &out, &stderr); code != 2 || stderr.String() != probeAuthorizedDispatchUsage || input.closes.Load() != 1 {
			t.Fatalf("%s: code=%d stderr=%q closes=%d", name, code, stderr.String(), input.closes.Load())
		}
	}
}
