//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests exercise the three Linux/darwin host-action appliers against
// t.TempDir() parents standing in for the borrowed /var/lib and
// /etc/systemd/system descriptors, and a recording fake in place of systemctl.
// No real systemctl, no privileged path, no network and no key material is
// involved; the owner check is against the test process's own euid, so the
// root-ownership requirement of a real host is not established here.

const (
	hostActApplyDataName = "uptime-phoenix-probe-" + hostActFrameOperationID
	hostActApplyUnitName = "uptime-phoenix-probe-" + hostActFrameOperationID + ".service"
)

// hostActApplyUmask sets the process umask for one test and restores it.
// The tests that use it never run in parallel.
func hostActApplyUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// hostActApplyParent returns a fresh temp directory and a borrowed descriptor.
func hostActApplyParent(t *testing.T) (string, *os.File) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return dir, f
}

func hostActApplyEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func hostActApplyRequireEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := hostActApplyEntries(t, dir); !slices.Equal(got, want) {
		t.Fatalf("directory entries %v, want %v", got, want)
	}
}

// hostActApplyBits is the permission, setuid, setgid and sticky bits.
func hostActApplyBits(info os.FileInfo) os.FileMode {
	return info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func hostActApplyNlink(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no stat_t")
	}
	return uint64(st.Nlink)
}

func hostActApplyRequireBorrowedParentOpen(t *testing.T, parent *os.File) {
	t.Helper()
	if _, err := parent.Stat(); err != nil {
		t.Fatalf("the borrowed parent was closed: %v", err)
	}
}

func hostActApplyRequireErr(t *testing.T, got, want error) {
	t.Helper()
	if got != want {
		t.Fatalf("error %v, want %v", got, want)
	}
}

func hostActApplyUnit(intent domain.ProbeHostActionIntent) []byte {
	return []byte(domain.ProbeSystemdUnit(intent.Identity))
}

// hostActApplyWriteUnit writes the exact unit bytes with the exact mode, with
// no help from the code under test.
func hostActApplyWriteUnit(t *testing.T, dir string, intent domain.ProbeHostActionIntent) string {
	t.Helper()
	p := filepath.Join(dir, hostActApplyUnitName)
	if err := os.WriteFile(p, hostActApplyUnit(intent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hostActApplyRequireUnitUntouched(t *testing.T, p string, want []byte, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("unit content changed or unreadable: %v", err)
	}
	if info, err := os.Lstat(p); err != nil || hostActApplyBits(info) != mode {
		t.Fatalf("unit mode %v %v, want %v", info, err, mode)
	}
}

// hostActApplyWithin runs fn and fails the test if it does not return, so a
// blocking open of a FIFO is a failure rather than a hung suite.
func hostActApplyWithin(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("%s blocked", what)
		return nil
	}
}

func hostActApplyWrongKindIntents() map[string]domain.ProbeHostActionIntent {
	docker := hostActFrameIntent(hostActFrameKindData)
	docker.Identity.Mode = domain.ProbeInstallDocker
	deploy := hostActFrameIntent(hostActFrameKindData)
	deploy.Identity.Host.User = "deploy"
	return map[string]domain.ProbeHostActionIntent{
		"data_directory":     hostActFrameIntent(hostActFrameKindData),
		"unit_file":          hostActFrameIntent(hostActFrameKindUnit),
		"service":            hostActFrameIntent(hostActFrameKindService),
		"artifact_directory": hostActFrameIntent(hostActFrameKindArtifactDir),
		"empty kind":         hostActFrameIntent(""),
		"docker mode":        docker,
		"non-root":           deploy,
		"zero":               {},
	}
}

// ---- ApplyProbeDataDirectory ------------------------------------------------

func TestApplyProbeDataDirectoryCreatesExactMode0700(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)
	// Whatever the process umask, the directory ends up exactly 0700.
	for _, umask := range []int{0o000, 0o022, 0o077, 0o277, 0o377} {
		t.Run(fmt.Sprintf("umask-%03o", umask), func(t *testing.T) {
			dir, parent := hostActApplyParent(t) // before the umask: a TempDir needs the default one
			hostActApplyUmask(t, umask)
			hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), nil)

			p := filepath.Join(dir, hostActApplyDataName)
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("not a real directory: %v", info.Mode())
			}
			if got := hostActApplyBits(info); got != 0o700 {
				t.Fatalf("mode %#o, want exactly 0700 with umask %#o", got, umask)
			}
			if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
				t.Fatalf("directory owner %v, want the euid %d", st, os.Geteuid())
			}
			hostActApplyRequireEntries(t, dir, hostActApplyDataName)
			hostActApplyRequireEntries(t, p) // created empty
			hostActApplyRequireBorrowedParentOpen(t, parent)
		})
	}
}

func TestApplyProbeDataDirectoryIsFreshOnly(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)
	hostActApplyUmask(t, 0o022)
	dir, parent := hostActApplyParent(t)
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), nil)

	// Make the first directory distinguishable: another mode and a child.
	p := filepath.Join(dir, hostActApplyDataName)
	child := filepath.Join(p, "operator-data")
	if err := os.WriteFile(child, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}

	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)

	after, err := os.Lstat(p)
	if err != nil || hostActApplyBits(after) != 0o750 || !os.SameFile(before, after) {
		t.Fatalf("existing directory was adopted or changed: %v %v", after, err)
	}
	if got, err := os.ReadFile(child); err != nil || string(got) != "keep me" {
		t.Fatalf("existing directory contents changed: %q %v", got, err)
	}
	hostActApplyRequireEntries(t, dir, hostActApplyDataName)
	hostActApplyRequireBorrowedParentOpen(t, parent)
}

func TestApplyProbeDataDirectoryRefusesExistingEntriesWithoutFollowing(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)
	hostActApplyUmask(t, 0o022)

	t.Run("regular file", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		p := filepath.Join(dir, hostActApplyDataName)
		if err := os.WriteFile(p, []byte("not a directory"), 0o640); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		hostActApplyRequireUnitUntouched(t, p, []byte("not a directory"), 0o640)
		hostActApplyRequireEntries(t, dir, hostActApplyDataName)
	})

	t.Run("pre-existing directory of another mode is not adopted", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		p := filepath.Join(dir, hostActApplyDataName)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		if info, err := os.Lstat(p); err != nil || hostActApplyBits(info) != 0o755 {
			t.Fatalf("pre-existing directory was chmodded: %v %v", info, err)
		}
	})

	t.Run("symlink to an existing directory", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, hostActApplyDataName)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		if info, err := os.Lstat(p); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink replaced: %v %v", info, err)
		}
		if info, err := os.Stat(target); err != nil || hostActApplyBits(info) != 0o755 {
			t.Fatalf("the symlink target was chmodded through the link: %v %v", info, err)
		}
		hostActApplyRequireEntries(t, target)
	})

	t.Run("dangling symlink", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		target := filepath.Join(t.TempDir(), "never-created")
		p := filepath.Join(dir, hostActApplyDataName)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the dangling symlink target was created: %v", err)
		}
		if info, err := os.Lstat(p); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink replaced: %v %v", info, err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		p := filepath.Join(dir, hostActApplyDataName)
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		err := hostActApplyWithin(t, "data directory over a fifo", func() error { return ApplyProbeDataDirectory(t.Context(), parent, intent) })
		hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
	})
}

func TestApplyProbeDataDirectoryRefusesWrongKindAndInput(t *testing.T) {
	var nilCtx *sendTNilContext
	for name, intent := range hostActApplyWrongKindIntents() {
		if name == "data_directory" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteInput)
			hostActApplyRequireEntries(t, dir)
		})
	}
	dir, parent := hostActApplyParent(t)
	valid := hostActFrameIntent(hostActFrameKindData)
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), nil, valid), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(nilCtx, parent, valid), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(nil, parent, valid), domain.ErrProbeBinaryStageWriteInput) //nolint:staticcheck // a nil context is the refusal under test
	hostActApplyRequireEntries(t, dir)
}

func TestApplyProbeDataDirectoryHonorsCanceledContext(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)
	dir, parent := hostActApplyParent(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(canceled, parent, intent), context.Canceled)
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(expired, parent, intent), context.DeadlineExceeded)
	hostActApplyRequireEntries(t, dir) // nothing was created
}

func TestApplyProbeDataDirectoryFailureBeforeCreationIsUnavailable(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)

	t.Run("parent is not a directory", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "plain")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), f, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir, "plain")
	})

	t.Run("parent already closed", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		if err := parent.Close(); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir)
	})

	t.Run("parent not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir, parent := hostActApplyParent(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir)
	})
}

func TestApplyProbeDataDirectoryConcurrentCallersCreateOnce(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindData)
	dir, parent := hostActApplyParent(t)
	const callers = 12
	results := make(chan error, callers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			results <- ApplyProbeDataDirectory(t.Context(), parent, intent)
		}()
	}
	start.Done()
	done.Wait()
	close(results)
	created, conflicts := 0, 0
	for err := range results {
		switch err {
		case nil:
			created++
		case domain.ErrProbeBinaryStageWriteConflict:
			conflicts++
		default:
			t.Fatalf("unexpected result %v", err)
		}
	}
	if created != 1 || conflicts != callers-1 {
		t.Fatalf("created %d conflicts %d, want exactly one creator", created, conflicts)
	}
	hostActApplyRequireEntries(t, dir, hostActApplyDataName)
}

// ---- ApplyProbeUnitFile -----------------------------------------------------

func TestApplyProbeUnitFileWritesExactBytesMode0644(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	want := hostActApplyUnit(intent)
	if len(want) == 0 || want[len(want)-1] != '\n' {
		t.Fatal("fixture unit is empty or not newline terminated")
	}
	for _, umask := range []int{0o000, 0o022, 0o077, 0o277} {
		t.Run(fmt.Sprintf("umask-%03o", umask), func(t *testing.T) {
			dir, parent := hostActApplyParent(t) // before the umask: a TempDir needs the default one
			hostActApplyUmask(t, umask)
			hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), nil)

			p := filepath.Join(dir, hostActApplyUnitName)
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("not a regular file: %v", info.Mode())
			}
			if got := hostActApplyBits(info); got != 0o644 {
				t.Fatalf("mode %#o, want exactly 0644 with umask %#o", got, umask)
			}
			if hostActApplyNlink(t, info) != 1 {
				t.Fatal("unit file has extra links")
			}
			if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
				t.Fatalf("owner %v, want euid %d", st, os.Geteuid())
			}
			got, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(got, want) || info.Size() != int64(len(want)) {
				t.Fatalf("unit bytes differ from ProbeSystemdUnit (%d vs %d bytes): %v", len(got), len(want), err)
			}
			hostActApplyRequireEntries(t, dir, hostActApplyUnitName)
			hostActApplyRequireBorrowedParentOpen(t, parent)
		})
	}
}

func TestApplyProbeUnitFileIsFreshOnly(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	hostActApplyUmask(t, 0o022)
	dir, parent := hostActApplyParent(t)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), nil)

	// A second call never rewrites, repairs or re-chmods what exists.
	p := filepath.Join(dir, hostActApplyUnitName)
	tampered := []byte("[Service]\nExecStart=/bin/sh\n")
	if err := os.WriteFile(p, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
	hostActApplyRequireUnitUntouched(t, p, tampered, 0o600)
	hostActApplyRequireEntries(t, dir, hostActApplyUnitName)

	// Even an identical, correctly moded file is a Conflict: no adoption.
	if err := os.WriteFile(p, hostActApplyUnit(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
}

func TestApplyProbeUnitFileRefusesExistingEntriesWithoutFollowing(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	hostActApplyUmask(t, 0o022)

	t.Run("symlink to an existing file is not written through", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		target := filepath.Join(t.TempDir(), "sentinel")
		if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, hostActApplyUnitName)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		hostActApplyRequireUnitUntouched(t, target, []byte("sentinel"), 0o600)
		if info, err := os.Lstat(p); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink replaced: %v %v", info, err)
		}
	})

	t.Run("dangling symlink is not created through", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		target := filepath.Join(t.TempDir(), "never-created")
		if err := os.Symlink(target, filepath.Join(dir, hostActApplyUnitName)); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the dangling target was created: %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		if err := os.Mkdir(filepath.Join(dir, hostActApplyUnitName), 0o755); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
	})

	t.Run("fifo does not block", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		if err := unix.Mkfifo(filepath.Join(dir, hostActApplyUnitName), 0o600); err != nil {
			t.Fatal(err)
		}
		err := hostActApplyWithin(t, "unit file over a fifo", func() error { return ApplyProbeUnitFile(t.Context(), parent, intent) })
		hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
	})

	t.Run("hardlink to the unit of another name", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		other := filepath.Join(dir, "other")
		if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, filepath.Join(dir, hostActApplyUnitName)); err != nil {
			t.Fatal(err)
		}
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteConflict)
		hostActApplyRequireUnitUntouched(t, other, []byte("other"), 0o600)
	})
}

func TestApplyProbeUnitFileRefusesWrongKindAndInput(t *testing.T) {
	var nilCtx *sendTNilContext
	for name, intent := range hostActApplyWrongKindIntents() {
		if name == "unit_file" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteInput)
			hostActApplyRequireEntries(t, dir)
		})
	}
	dir, parent := hostActApplyParent(t)
	valid := hostActFrameIntent(hostActFrameKindUnit)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), nil, valid), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(nilCtx, parent, valid), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireEntries(t, dir)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	hostActApplyRequireErr(t, ApplyProbeUnitFile(canceled, parent, valid), context.Canceled)
	hostActApplyRequireEntries(t, dir)
}

func TestApplyProbeUnitFileFailureBeforeCreationIsUnavailable(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	t.Run("parent is not a directory", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "plain")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), f, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir, "plain")
	})
	t.Run("parent already closed", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		_ = parent.Close()
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir)
	})
	t.Run("parent not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir, parent := hostActApplyParent(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), parent, intent), domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir)
	})
}

func TestApplyProbeUnitFileConcurrentCallersCreateOnce(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindUnit)
	dir, parent := hostActApplyParent(t)
	const callers = 12
	results := make(chan error, callers)
	var start, done sync.WaitGroup
	start.Add(1)
	for range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			results <- ApplyProbeUnitFile(t.Context(), parent, intent)
		}()
	}
	start.Done()
	done.Wait()
	close(results)
	created, conflicts := 0, 0
	for err := range results {
		switch err {
		case nil:
			created++
		case domain.ErrProbeBinaryStageWriteConflict:
			conflicts++
		default:
			t.Fatalf("unexpected result %v", err)
		}
	}
	if created != 1 || conflicts != callers-1 {
		t.Fatalf("created %d conflicts %d, want exactly one creator", created, conflicts)
	}
	hostActApplyRequireUnitUntouched(t, filepath.Join(dir, hostActApplyUnitName), hostActApplyUnit(intent), 0o644)
}

// ---- ApplyProbeService ------------------------------------------------------

type hostActApplyCtxKey struct{}

// hostActApplySystemctl records every invocation. The script decides each
// answer from the zero-based call index and the arguments.
type hostActApplySystemctl struct {
	mu     sync.Mutex
	calls  [][]string
	ctxs   []context.Context
	script func(call int, args []string) (string, int, error)
}

func (s *hostActApplySystemctl) Run(ctx context.Context, args ...string) (string, int, error) {
	s.mu.Lock()
	call := len(s.calls)
	s.calls = append(s.calls, slices.Clone(args))
	s.ctxs = append(s.ctxs, ctx)
	script := s.script
	s.mu.Unlock()
	if script == nil {
		script = hostActApplyHappyScript
	}
	return script(call, args)
}

func (s *hostActApplySystemctl) recorded() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	for i := range s.calls {
		out[i] = slices.Clone(s.calls[i])
	}
	return out
}

// hostActApplyHappyScript answers a fresh, disabled unit that enables cleanly.
func hostActApplyHappyScript(call int, args []string) (string, int, error) {
	switch {
	case call == 0 && len(args) == 2 && args[0] == "is-enabled":
		return "disabled\n", 1, nil
	case len(args) == 1 && args[0] == "daemon-reload":
		return "", 0, nil
	case len(args) == 2 && args[0] == "enable":
		return "Created symlink /etc/systemd/system/multi-user.target.wants/" + args[1] + ".\n", 0, nil
	case len(args) == 2 && args[0] == "is-enabled":
		return "enabled\n", 0, nil
	}
	return "", 127, errors.New("unexpected systemctl invocation")
}

// hostActApplyScript overrides single steps of the happy path.
func hostActApplyScript(override map[int]func(args []string) (string, int, error)) func(int, []string) (string, int, error) {
	return func(call int, args []string) (string, int, error) {
		if fn, ok := override[call]; ok {
			return fn(args)
		}
		return hostActApplyHappyScript(call, args)
	}
}

func hostActApplyResult(out string, code int, err error) func([]string) (string, int, error) {
	return func([]string) (string, int, error) { return out, code, err }
}

// hostActApplyHook is a scripted systemctl reply that runs side first (the
// test's interference with the host) and then succeeds with out.
func hostActApplyHook(side func(), out string) func([]string) (string, int, error) {
	reply := hostActApplyResult(out, 0, nil)
	return func(args []string) (string, int, error) {
		side()
		return reply(args)
	}
}

func hostActApplyWantCalls(name string, n int) [][]string {
	all := [][]string{{"is-enabled", name}, {"daemon-reload"}, {"enable", name}, {"is-enabled", name}}
	return all[:n]
}

func hostActApplyRequireCalls(t *testing.T, ctl *hostActApplySystemctl, want [][]string) {
	t.Helper()
	got := ctl.recorded()
	if len(got) != len(want) {
		t.Fatalf("systemctl calls %v, want %v", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("systemctl calls %v, want %v", got, want)
		}
	}
}

func TestApplyProbeServiceHappyPathCallsExactSequence(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	dir, parent := hostActApplyParent(t)
	unit := hostActApplyWriteUnit(t, dir, intent)
	ctl := &hostActApplySystemctl{}
	ctx := context.WithValue(t.Context(), hostActApplyCtxKey{}, "sentinel")

	hostActApplyRequireErr(t, ApplyProbeService(ctx, parent, intent, ctl), nil)
	hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 4))

	// Fixed arguments only: the bare unit name, never a path, and never any
	// flag that would start, force or only runtime-enable the unit.
	for _, call := range ctl.recorded() {
		if !slices.Contains([]string{"is-enabled", "daemon-reload", "enable"}, call[0]) {
			t.Fatalf("unexpected systemctl verb %q", call[0])
		}
		for _, arg := range call {
			if strings.HasPrefix(arg, "-") || strings.Contains(arg, "/") || arg == "start" || arg == "restart" || arg == "--now" {
				t.Fatalf("unexpected systemctl argument %q in %v", arg, call)
			}
		}
	}
	for i, c := range ctl.ctxs {
		if c.Value(hostActApplyCtxKey{}) != "sentinel" {
			t.Fatalf("call %d did not receive the caller's context", i)
		}
	}
	// The unit file is only read.
	hostActApplyRequireUnitUntouched(t, unit, hostActApplyUnit(intent), 0o644)
	hostActApplyRequireEntries(t, dir, hostActApplyUnitName)
	hostActApplyRequireBorrowedParentOpen(t, parent)
}

func TestApplyProbeServiceRefusesUnitThatIsNotExactlyDerivedWithoutAnySystemctlCall(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	want := hostActApplyUnit(intent)
	flipped := slices.Clone(want)
	flipped[len(flipped)/2] ^= 0x01

	cases := map[string]func(t *testing.T, dir string){
		"unit missing":        func(*testing.T, string) {},
		"one byte differs":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, flipped, 0o644) },
		"extra trailing byte": func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, append(slices.Clone(want), '\n'), 0o644) },
		"extra trailing text": func(t *testing.T, dir string) {
			hostActApplyWriteRaw(t, dir, append(slices.Clone(want), "ExecStartPre=/bin/evil\n"...), 0o644)
		},
		"truncated by a byte": func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want[:len(want)-1], 0o644) },
		"empty":               func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, nil, 0o644) },
		"wrong content": func(t *testing.T, dir string) {
			hostActApplyWriteRaw(t, dir, []byte("[Service]\nExecStart=/bin/sh\n"), 0o644)
		},
		"larger than the bound": func(t *testing.T, dir string) {
			hostActApplyWriteRaw(t, dir, bytes.Repeat([]byte("x"), hostActionUnitMaxRead+10), 0o644)
		},
		"mode 0600":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o600) },
		"mode 0664":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o664) },
		"mode 0666":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o666) },
		"mode 0755":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o755) },
		"mode 0444":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o444) },
		"mode 0000":    func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, 0o000) },
		"setuid 04644": func(t *testing.T, dir string) { hostActApplyWriteRaw(t, dir, want, os.ModeSetuid|0o644) },
		"hard-linked": func(t *testing.T, dir string) {
			p := hostActApplyWriteRaw(t, dir, want, 0o644)
			if err := os.Link(p, filepath.Join(dir, "second-name")); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to an exact unit": func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "exact.service")
			if err := os.WriteFile(target, want, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, hostActApplyUnitName)); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, hostActApplyUnitName)); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, hostActApplyUnitName), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, dir string) {
			if err := unix.Mkfifo(filepath.Join(dir, hostActApplyUnitName), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"unit of another operation only": func(t *testing.T, dir string) {
			other := hostActFrameIntent(hostActFrameKindService)
			other.Identity.OperationID = hostActFrameOtherOperation
			p := filepath.Join(dir, "uptime-phoenix-probe-"+hostActFrameOtherOperation+".service")
			if err := os.WriteFile(p, hostActApplyUnit(other), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"unit derived for another release": func(t *testing.T, dir string) {
			other := hostActFrameIntent(hostActFrameKindService)
			other.Identity.Binary.Version = "9.9.9"
			hostActApplyWriteRaw(t, dir, hostActApplyUnit(other), 0o644)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			hostActApplyUmask(t, 0o022)
			dir, parent := hostActApplyParent(t)
			setup(t, dir)
			ctl := &hostActApplySystemctl{}
			err := hostActApplyWithin(t, "service apply", func() error { return ApplyProbeService(t.Context(), parent, intent, ctl) })
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnavailable)
			hostActApplyRequireCalls(t, ctl, nil)
		})
	}
}

// hostActApplyWriteRaw writes data at the unit name with an exact mode (the
// setuid case included) and returns its path.
func hostActApplyWriteRaw(t *testing.T, dir string, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, hostActApplyUnitName)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Skipf("cannot set mode %v on this platform: %v", mode, err)
	}
	return p
}

func TestApplyProbeServiceAlreadyEnabledIsConflictAfterOneCall(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	// is-enabled exits 0 for every enabled-like state; none may be adopted.
	for _, state := range []string{"enabled\n", "enabled-runtime\n", "static\n", "alias\n", "linked\n", "indirect\n", "generated\n", ""} {
		t.Run(fmt.Sprintf("%q", state), func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: func(int, []string) (string, int, error) { return state, 0, nil }}
			hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteConflict)
			hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 1))
		})
	}
}

func TestApplyProbeServiceSystemctlRunErrorBeforeMutationIsUnavailable(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	dir, parent := hostActApplyParent(t)
	unit := hostActApplyWriteUnit(t, dir, intent)
	for name, ctl := range map[string]*hostActApplySystemctl{
		"error with output":    {script: func(int, []string) (string, int, error) { return "enabled\n", 0, errors.New("systemctl not found") }},
		"error with exit code": {script: func(int, []string) (string, int, error) { return "", 1, errors.New("exec failed") }},
		"error only":           {script: func(int, []string) (string, int, error) { return "", 0, errors.New("bounded output exceeded") }},
	} {
		hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
		if got := ctl.recorded(); len(got) != 1 {
			t.Fatalf("%s: %d systemctl calls, want only the is-enabled probe: %v", name, len(got), got)
		}
	}
	hostActApplyRequireUnitUntouched(t, unit, hostActApplyUnit(intent), 0o644)
}

func TestApplyProbeServiceFirstProbeMustBeExactlyDisabled(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	// Only an exact "disabled" answer (systemctl reads unit files directly)
	// lets the action mutate; every other nonzero answer refuses first.
	for _, code := range []int{1, 2, 4, 127, 255} {
		for _, stdout := range []string{"", "not-found\n", "Failed to get unit file state\n", "disabled", "DISABLED\n", "masked\n"} {
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: hostActApplyScript(map[int]func([]string) (string, int, error){
				0: hostActApplyResult(stdout, code, nil),
			})}
			hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
			hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 1))
		}
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{script: hostActApplyScript(map[int]func([]string) (string, int, error){
			0: hostActApplyResult("disabled\n", code, nil),
		})}
		hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), nil)
		hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 4))
	}
}

func TestApplyProbeServiceFailureAfterMutationIsUnknown(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	type step = func([]string) (string, int, error)
	cases := []struct {
		name      string
		override  map[int]step
		wantCalls int
	}{
		{"daemon-reload nonzero", map[int]step{1: hostActApplyResult("", 1, nil)}, 2},
		{"daemon-reload error", map[int]step{1: hostActApplyResult("", 0, errors.New("boom"))}, 2},
		{"daemon-reload exit 255", map[int]step{1: hostActApplyResult("", 255, nil)}, 2},
		{"enable nonzero", map[int]step{2: hostActApplyResult("", 1, nil)}, 3},
		{"enable error", map[int]step{2: hostActApplyResult("", 0, errors.New("boom"))}, 3},
		{"final is-enabled error", map[int]step{3: hostActApplyResult("enabled\n", 0, errors.New("boom"))}, 4},
		{"final is-enabled nonzero", map[int]step{3: hostActApplyResult("enabled\n", 1, nil)}, 4},
		{"final is-enabled disabled with exit 0", map[int]step{3: hostActApplyResult("disabled\n", 0, nil)}, 4},
		{"final is-enabled enabled-runtime", map[int]step{3: hostActApplyResult("enabled-runtime\n", 0, nil)}, 4},
		{"final is-enabled static", map[int]step{3: hostActApplyResult("static\n", 0, nil)}, 4},
		{"final is-enabled alias", map[int]step{3: hostActApplyResult("alias\n", 0, nil)}, 4},
		{"final is-enabled without LF", map[int]step{3: hostActApplyResult("enabled", 0, nil)}, 4},
		{"final is-enabled with two LFs", map[int]step{3: hostActApplyResult("enabled\n\n", 0, nil)}, 4},
		{"final is-enabled with CRLF", map[int]step{3: hostActApplyResult("enabled\r\n", 0, nil)}, 4},
		{"final is-enabled with a prefix line", map[int]step{3: hostActApplyResult("warning\nenabled\n", 0, nil)}, 4},
		{"final is-enabled upper case", map[int]step{3: hostActApplyResult("ENABLED\n", 0, nil)}, 4},
		{"final is-enabled empty", map[int]step{3: hostActApplyResult("", 0, nil)}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			unit := hostActApplyWriteUnit(t, dir, intent)
			ctl := &hostActApplySystemctl{script: hostActApplyScript(tc.override)}
			hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
			hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, tc.wantCalls))
			hostActApplyRequireUnitUntouched(t, unit, hostActApplyUnit(intent), 0o644)
		})
	}
}

func TestApplyProbeServiceContextCancellation(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)

	t.Run("canceled before the call makes no systemctl call", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctl := &hostActApplySystemctl{}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		hostActApplyRequireErr(t, ApplyProbeService(canceled, parent, intent, ctl), context.Canceled)
		hostActApplyRequireCalls(t, ctl, nil)
	})

	t.Run("canceled during the mutation is never applied", func(t *testing.T) {
		dir, parent := hostActApplyParent(t)
		hostActApplyWriteUnit(t, dir, intent)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctl := &hostActApplySystemctl{script: hostActApplyScript(map[int]func([]string) (string, int, error){
			// The final probe reports a clean enabled unit, but the caller's
			// context ended first: success must not be reported.
			3: hostActApplyHook(cancel, "enabled\n"),
		})}
		hostActApplyRequireErr(t, ApplyProbeService(ctx, parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnknown)
		hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 4))
	})
}

func TestApplyProbeServiceRefusesInvalidCallsWithoutAnySystemctlCall(t *testing.T) {
	var nilCtx *sendTNilContext
	var typedNil *hostActApplySystemctl
	valid := hostActFrameIntent(hostActFrameKindService)

	for name, intent := range hostActApplyWrongKindIntents() {
		if name == "service" {
			continue
		}
		t.Run("intent "+name, func(t *testing.T) {
			dir, parent := hostActApplyParent(t)
			hostActApplyWriteUnit(t, dir, valid)
			ctl := &hostActApplySystemctl{}
			hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteInput)
			hostActApplyRequireCalls(t, ctl, nil)
		})
	}

	dir, parent := hostActApplyParent(t)
	hostActApplyWriteUnit(t, dir, valid)
	ctl := &hostActApplySystemctl{}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), nil, valid, ctl), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireErr(t, ApplyProbeService(nilCtx, parent, valid, ctl), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireCalls(t, ctl, nil)

	// A nil systemctl is refused even though the unit is exactly right and
	// even when the unit is missing; it is never dereferenced.
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, valid, nil), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, valid, typedNil), domain.ErrProbeBinaryStageWriteInput)
	emptyDir, emptyParent := hostActApplyParent(t)
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), emptyParent, valid, nil), domain.ErrProbeBinaryStageWriteInput)
	hostActApplyRequireEntries(t, emptyDir)
}

func TestApplyProbeServiceFailureBeforeProbeWithBadParentIsUnavailable(t *testing.T) {
	intent := hostActFrameIntent(hostActFrameKindService)
	dir, parent := hostActApplyParent(t)
	hostActApplyWriteUnit(t, dir, intent)
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	ctl := &hostActApplySystemctl{}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), parent, intent, ctl), domain.ErrProbeBinaryStageWriteUnavailable)
	hostActApplyRequireCalls(t, ctl, nil)
}

// The three actions compose: the unit file the unit_file action writes is
// exactly what the service action accepts, each action is fresh-only on replay,
// and a unit derived for another release is refused.
func TestHostActionsLifecycleDataUnitService(t *testing.T) {
	hostActApplyUmask(t, 0o077) // a restrictive umask must not break the unit/service handshake
	data := hostActFrameIntent(hostActFrameKindData)
	unit := hostActFrameIntent(hostActFrameKindUnit)
	service := hostActFrameIntent(hostActFrameKindService)
	varLib, varLibFD := hostActApplyParent(t)
	systemd, systemdFD := hostActApplyParent(t)

	// The service cannot be enabled before its unit file exists.
	early := &hostActApplySystemctl{}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), systemdFD, service, early), domain.ErrProbeBinaryStageWriteUnavailable)
	hostActApplyRequireCalls(t, early, nil)

	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), varLibFD, data), nil)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), systemdFD, unit), nil)
	ctl := &hostActApplySystemctl{}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), systemdFD, service, ctl), nil)
	hostActApplyRequireCalls(t, ctl, hostActApplyWantCalls(hostActApplyUnitName, 4))

	// Replays are definite conflicts and change nothing.
	hostActApplyRequireErr(t, ApplyProbeDataDirectory(t.Context(), varLibFD, data), domain.ErrProbeBinaryStageWriteConflict)
	hostActApplyRequireErr(t, ApplyProbeUnitFile(t.Context(), systemdFD, unit), domain.ErrProbeBinaryStageWriteConflict)
	enabled := &hostActApplySystemctl{script: func(int, []string) (string, int, error) { return "enabled\n", 0, nil }}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), systemdFD, service, enabled), domain.ErrProbeBinaryStageWriteConflict)
	hostActApplyRequireCalls(t, enabled, hostActApplyWantCalls(hostActApplyUnitName, 1))

	hostActApplyRequireEntries(t, varLib, hostActApplyDataName)
	hostActApplyRequireEntries(t, systemd, hostActApplyUnitName)
	hostActApplyRequireUnitUntouched(t, filepath.Join(systemd, hostActApplyUnitName), hostActApplyUnit(unit), 0o644)

	// An intent for another release reads the same unit name and is refused,
	// because the file on disk is not the unit it derives.
	other := service
	other.Identity.Binary.Version = "9.9.9"
	refused := &hostActApplySystemctl{}
	hostActApplyRequireErr(t, ApplyProbeService(t.Context(), systemdFD, other, refused), domain.ErrProbeBinaryStageWriteUnavailable)
	hostActApplyRequireCalls(t, refused, nil)
}
