//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestMain performs the two explicitly approved root-inode fixture setups only
// as namespace PID1, then reexecutes this test binary after dropping capabilities.
// It is never a production helper path and never executes a probe artifact.
func TestMain(m *testing.M) {
	if os.Getenv("PHOENIX_M8_HOST_SANDBOX_SETUP") == "1" && os.Getenv("PHOENIX_M8_HOST_SANDBOX_SETUP_DONE") != "1" {
		nonce, err := os.ReadFile("/fixtures/sandbox-nonce")
		mounts, mountErr := os.ReadFile("/proc/self/mountinfo")
		scenario := os.Getenv("PHOENIX_M8_HOST_SANDBOX_SCENARIO")
		if os.Getpid() != 1 || os.Geteuid() != 0 || err != nil || len(nonce) < 16 || string(nonce) != os.Getenv("PHOENIX_M8_HOST_SANDBOX_NONCE") || mountErr != nil || !strings.Contains(string(mounts), " /fixtures ro,") || (scenario != "unsafe_root_owner" && scenario != "unsafe_root_mode") {
			_, _ = fmt.Fprintln(os.Stderr, "isolated root fixture setup refused")
			os.Exit(1)
		}
		runtime.LockOSThread()
		if scenario == "unsafe_root_owner" {
			err = os.Chown("/", 10001, 0)
		} else {
			err = os.Chmod("/", 0o777)
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "isolated root fixture mutation refused")
			os.Exit(1)
		}
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var data [2]unix.CapUserData
		if unix.Capset(&header, &data[0]) != nil {
			_, _ = fmt.Fprintln(os.Stderr, "fixture capability drop refused")
			os.Exit(1)
		}
		// Exec eliminates all other setup threads. no-new-privileges is sticky;
		// the fresh test checks actual zero capabilities before the helper.
		err = syscall.Exec("/fixtures/test", os.Args, append(os.Environ(), "PHOENIX_M8_HOST_SANDBOX_SETUP_DONE=1"))
		_, _ = fmt.Fprintln(os.Stderr, "isolated test reexec refused")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestProbeBinaryStageHostSandbox must run only in the dedicated disposable
// namespace created by the committed runner. The strict gate rejects this skip.
func TestProbeBinaryStageHostSandbox(t *testing.T) {
	if os.Getenv("PHOENIX_M8_HOST_SANDBOX") != "1" {
		t.Skip("requires isolated M8 host-launcher sandbox; run the dedicated gate")
	}
	nonce, err := os.ReadFile("/fixtures/sandbox-nonce")
	if err != nil || len(nonce) < 16 || string(nonce) != os.Getenv("PHOENIX_M8_HOST_SANDBOX_NONCE") {
		t.Fatal("sandbox identity absent or invalid")
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil || !strings.Contains(string(mounts), " /tmp ") || !strings.Contains(string(mounts), " - tmpfs ") {
		t.Fatal("private temporary filesystem not established")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal("capability status unavailable")
	}
	for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		if !strings.Contains(string(status), field+":\t0000000000000000") {
			t.Fatal("fixture retained active or inheritable capabilities")
		}
	}
	if !strings.Contains(string(status), "NoNewPrivs:\t1") {
		t.Fatal("no-new-privileges not established")
	}
	scenario := os.Getenv("PHOENIX_M8_HOST_SANDBOX_SCENARIO")
	if scenario != "positive" {
		allowed := map[string]bool{"unsafe_owner": true, "missing_opt": true, "symlink_opt": true, "regular_opt": true, "nonroot": true, "unsafe_root_owner": true, "unsafe_root_mode": true}
		if !allowed[scenario] {
			t.Fatal("unknown sandbox scenario")
		}
		t.Run("RefusesParent/"+scenario, func(t *testing.T) {
			hostSandboxRefusalFixture(t, scenario)
			// A deliberately invalid, complete header isolates parent refusal:
			// dropping a parent guard must yield Input, not another later
			// permission failure that happens to look like Unavailable.
			hostSandboxExpect(t, []byte("x"), nil, 1, domain.ErrProbeBinaryStageWriteUnavailable.Error()+"\n")
		})
		return
	}
	if os.Geteuid() != 0 || !strings.Contains(string(mounts), " /opt ") {
		t.Fatal("positive namespace is not the isolated uid0 opt fixture")
	}
	t.Run("ParentFDAttributes", func(t *testing.T) {
		parent, err := openHostProbeBinaryStageParent(t.Context())
		if err != nil || parent == nil {
			t.Fatal("protected fixed parent refused", err)
		}
		defer func() { _ = parent.Close() }()
		control, err := parent.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var flags int
		var info unix.Stat_t
		var inner error
		if err = control.Control(func(fd uintptr) {
			flags, inner = unix.FcntlInt(fd, unix.F_GETFD, 0)
			if inner == nil {
				inner = unix.Fstat(int(fd), &info)
			}
		}); err != nil || inner != nil || flags&unix.FD_CLOEXEC == 0 || !probeBinaryStageHostDirectory(&info) {
			t.Fatal("parent is not protected live CLOEXEC directory", err, inner)
		}
		var named unix.Stat_t
		if err := unix.Stat("/opt", &named); err != nil || named.Dev != info.Dev || named.Ino != info.Ino {
			t.Fatal("fixed parent does not identify this namespace opt", err)
		}
		for _, mode := range []os.FileMode{0o775, 0o707} {
			if err := os.Chmod("/opt", mode); err != nil {
				t.Fatal(err)
			}
			p, refuse := openHostProbeBinaryStageParent(t.Context())
			if p != nil {
				_ = p.Close()
			}
			if err := os.Chmod("/opt", 0o755); err != nil {
				t.Fatal(err)
			}
			if p != nil || refuse != domain.ErrProbeBinaryStageWriteUnavailable {
				t.Fatal("writable fixed parent accepted", refuse)
			}
		}
	})
	t.Run("ActualAdmin", func(t *testing.T) {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(arch, func(t *testing.T) {
				data, intent := hostSandboxFixture(t, arch)
				hostSandboxOwnGraph(t, intent)
				payload := hostSandboxExpect(t, helperEffectFrame(t, intent, data), nil, 0, "")
				hostSandboxReceipt(t, payload, intent)
				helperEffectGraph(t, "/opt", intent, data)
			})
		}
	})
	t.Run("ReplayPreservesForeign", func(t *testing.T) {
		data, intent := hostSandboxFixture(t, "arm64")
		hostSandboxOwnGraph(t, intent)
		hostSandboxExpect(t, helperEffectFrame(t, intent, data), nil, 0, "")
		marker := filepath.Join("/opt", (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName(), domain.ProbeBinaryStageMarkerName)
		if err := os.Chmod(marker, 0o644); err != nil {
			t.Fatal(err)
		}
		foreign := []byte("foreign retained marker\n")
		if err := os.WriteFile(marker, foreign, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(marker, 0o444); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(marker)
		if err != nil {
			t.Fatal(err)
		}
		hostSandboxExpect(t, helperEffectFrame(t, intent, data), nil, 1, domain.ErrProbeBinaryStageWriteConflict.Error()+"\n")
		after, err := os.Lstat(marker)
		got, readErr := os.ReadFile(marker)
		if err != nil || readErr != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(got, foreign) {
			t.Fatal("replay adopted or rewrote foreign marker")
		}
	})
	t.Run("HeaderRefusal", func(t *testing.T) {
		hostSandboxExpect(t, []byte("x"), nil, 1, domain.ErrProbeBinaryStageWriteInput.Error()+"\n")
		helperEffectEmpty(t, "/opt")
	})
	t.Run("BodyRefusal", func(t *testing.T) {
		data, intent := hostSandboxFixture(t, "arm64")
		hostSandboxExpect(t, helperEffectFrame(t, intent, data[:len(data)-1]), nil, 1, domain.ErrProbeArtifactSize.Error()+"\n")
		helperEffectEmpty(t, "/opt")
	})
	t.Run("WithheldEOFSignal", func(t *testing.T) {
		data, intent := hostSandboxFixture(t, "arm64")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd, scratch := hostSandboxCommand(t, ctx, nil)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(); _ = w.Close() }()
		cmd.Stdin = r
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		frame := helperEffectFrame(t, intent, data)
		written := make(chan error, 1)
		writerJoined := make(chan struct{})
		processJoined := make(chan struct{})
		waited := make(chan error, 1)
		go func() {
			defer close(writerJoined)
			_, err := w.Write(frame)
			written <- err
		}()
		go func() {
			defer close(processJoined)
			waited <- cmd.Wait()
		}()
		t.Cleanup(func() {
			_ = w.Close()
			cancel()
			<-writerJoined
			<-processJoined
		})
		select {
		case err := <-written:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("child did not consume bounded frame")
		}
		time.Sleep(100 * time.Millisecond)
		helperEffectEmpty(t, "/opt")
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		err = <-waited
		<-processJoined
		_ = w.Close()
		if err == nil || ctx.Err() != nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteUnknown.Error()+"\n" {
			t.Fatal("withheld EOF did not join normal cancellation refusal", err, stderr.String())
		}
		helperEffectEmpty(t, "/opt", scratch)
	})
	t.Run("UnknownOutputRetainsGraph", func(t *testing.T) {
		data, intent := hostSandboxFixture(t, "arm64")
		hostSandboxOwnGraph(t, intent)
		r := bytes.NewReader(helperEffectFrame(t, intent, data))
		input := &helperRC{readFn: r.Read}
		var parent *os.File
		acquire := func(ctx context.Context) (*os.File, error) {
			p, err := openHostProbeBinaryStageParent(ctx)
			parent = p
			return p, err
		}
		writes := 0
		out := helperEffectWriteFunc(func(p []byte) (int, error) {
			writes++
			if parent == nil {
				t.Fatal("output before acquisition")
			}
			if _, err := parent.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("parent not closed before acknowledgement", err)
			}
			return 0, io.ErrClosedPipe
		})
		var stderr bytes.Buffer
		code := runHostProbeBinaryStageHelper(t.Context(), nil, input, out, &stderr, acquire)
		if code != 1 || writes != 1 || input.closes.Load() != 1 || stderr.String() != domain.ErrProbeBinaryStageWriteUnknown.Error()+"\n" {
			t.Fatal("post-stage output failure lost uncertainty", code, stderr.String())
		}
		helperEffectGraph(t, "/opt", intent, data)
	})
	t.Run("UsageBeforeRoot", func(t *testing.T) {
		hostSandboxExpect(t, nil, []string{"--help"}, 2, probeBinaryStageHostUsage)
		helperEffectEmpty(t, "/opt")
	})
	t.Run("SourceCloseBeforeMutation", func(t *testing.T) {
		data, intent := hostSandboxFixture(t, "arm64")
		hostSandboxOwnGraph(t, intent)
		r := bytes.NewReader(helperEffectFrame(t, intent, data))
		input := &helperRC{readFn: r.Read, closeFn: func() error {
			helperEffectEmpty(t, "/opt")
			return nil
		}}
		var out, stderr bytes.Buffer
		code := RunHostProbeBinaryStageHelper(t.Context(), nil, input, &out, &stderr)
		if code != 0 || input.closes.Load() != 1 || stderr.Len() != 0 {
			t.Fatal("source close/real stage composition failed", code, stderr.String())
		}
		hostSandboxReceipt(t, out.Bytes(), intent)
		helperEffectGraph(t, "/opt", intent, data)
	})
}

func hostSandboxRefusalFixture(t *testing.T, scenario string) {
	t.Helper()
	var root unix.Stat_t
	if unix.Stat("/", &root) != nil || root.Mode&unix.S_IFMT != unix.S_IFDIR {
		t.Fatal("invalid namespace root fixture")
	}
	if scenario != "nonroot" && os.Geteuid() != 0 {
		t.Fatal("unrelated nonroot masks root or parent fixture")
	}
	if scenario == "unsafe_root_owner" {
		if root.Uid == 0 || root.Mode&0o022 != 0 {
			t.Fatal("unsafe root owner not isolated")
		}
		return
	}
	if scenario == "unsafe_root_mode" {
		if root.Uid != 0 || root.Mode&0o022 == 0 {
			t.Fatal("unsafe root mode not isolated")
		}
		return
	}
	if root.Uid != 0 || root.Mode&0o022 != 0 {
		t.Fatal("unrelated unsafe root masks intended refusal")
	}
	if scenario == "nonroot" {
		if os.Geteuid() == 0 {
			t.Fatal("nonroot credential not established")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("unrelated nonroot refusal masks fixture")
	}
	var info unix.Stat_t
	err := unix.Lstat("/opt", &info)
	switch scenario {
	case "missing_opt":
		if !errors.Is(err, unix.ENOENT) {
			t.Fatal("missing opt not established")
		}
	case "regular_opt":
		if err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG {
			t.Fatal("regular opt not established")
		}
	case "symlink_opt":
		var target unix.Stat_t
		if err != nil || info.Mode&unix.S_IFMT != unix.S_IFLNK || unix.Stat("/opt", &target) != nil || target.Mode&unix.S_IFMT != unix.S_IFDIR || target.Uid != 0 || target.Mode&0o022 != 0 {
			t.Fatal("live safe symlink target not established")
		}
	case "unsafe_owner":
		if err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Uid == 0 || info.Mode&0o022 != 0 {
			t.Fatal("unsafe opt owner not isolated")
		}
	default:
		t.Fatal("unknown refusal fixture")
	}
}

func hostSandboxFixture(t *testing.T, arch string) ([]byte, domain.ProbeBinaryStageIntent) {
	t.Helper()
	data, err := os.ReadFile("/fixtures/" + arch)
	if err != nil || len(data) == 0 {
		t.Fatal("pinned Linux fixture unavailable", err)
	}
	return data, helperEffectIntent(t, data, arch)
}

func hostSandboxOwnGraph(t *testing.T, intent domain.ProbeBinaryStageIntent) {
	t.Helper()
	name := (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName()
	if name == "" || strings.ContainsAny(name, "/\\") {
		t.Fatal("invalid fixture graph")
	}
	if _, err := os.Lstat("/opt/" + name); !os.IsNotExist(err) {
		t.Fatal("fixture operation already exists", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll("/opt/" + name); err != nil {
			t.Error("owned namespace fixture cleanup", err)
		}
	})
}

func hostSandboxCommand(t *testing.T, ctx context.Context, args []string) (*exec.Cmd, string) {
	t.Helper()
	scratch := t.TempDir()
	cmd := exec.CommandContext(ctx, "/fixtures/admin", append([]string{"stage-binary-host"}, args...)...)
	cmd.Env = []string{"PATH=/no-toolchain", "HOME=/tmp", "TMPDIR=" + scratch, "PROBES_ENABLED=not-a-boolean", "DB_TYPE=not-a-database"}
	cmd.Dir = scratch
	return cmd, scratch
}

func hostSandboxExpect(t *testing.T, frame []byte, args []string, want int, diagnostic string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd, scratch := hostSandboxCommand(t, ctx, args)
	cmd.Stdin = bytes.NewReader(frame)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil || cmd.ProcessState == nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != want || (err == nil) != (want == 0) || stderr.String() != diagnostic || (want != 0 && out.Len() != 0) {
		t.Fatal("actual fixed-parent command failed normal-exit contract", err, stderr.String())
	}
	helperEffectEmpty(t, scratch)
	return out.Bytes()
}

func hostSandboxReceipt(t *testing.T, payload []byte, intent domain.ProbeBinaryStageIntent) {
	t.Helper()
	var view map[string]json.RawMessage
	if len(payload) > 2048 || bytes.Count(payload, []byte{'\n'}) != 1 || !bytes.HasSuffix(payload, []byte{'\n'}) || json.Unmarshal(payload, &view) != nil || len(view) != 9 {
		t.Fatal("invalid bounded receipt shape")
	}
	expected, err := probeBinaryStageHelperReceipt(domain.ProbeBinaryStageSeal{Intent: intent})
	if err != nil || !bytes.Equal(payload, expected) {
		t.Fatal("receipt does not bind exact intended graph")
	}
}
