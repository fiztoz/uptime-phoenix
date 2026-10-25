//go:build linux || darwin

package provisioning

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests exercise ApplyProbeCleanup against t.TempDir() parents standing
// in for the borrowed /var/lib, /opt and /etc/systemd/system descriptors and a
// recording fake in place of systemctl. No real systemctl, privileged path or
// network is involved, and the owner checks are against the test process's own
// euid, so root ownership on a real host is not established here.

const cleanupTContent = "phoenix pinned probe binary bytes, cleanup fixture"

// cleanupTEnv is a fake host: three parents, an unrelated "outside" tree that
// links point at, and the install's intent.
type cleanupTEnv struct {
	intent     domain.ProbeHostActionIntent
	release    string
	dataRoot   string
	optRoot    string
	unitRoot   string
	outside    string
	dataParent *os.File
	optParent  *os.File
	unitParent *os.File
}

func newCleanupTEnv(t *testing.T) *cleanupTEnv {
	t.Helper()
	e := &cleanupTEnv{}
	e.intent = hostActFrameIntent(domain.ProbeHostActionCleanup)
	e.intent.Identity.Binary = sendTPin([]byte(cleanupTContent))
	e.release = e.intent.Identity.Binary.ReleaseName()
	e.dataRoot, e.dataParent = hostActApplyParent(t)
	e.optRoot, e.optParent = hostActApplyParent(t)
	e.unitRoot, e.unitParent = hostActApplyParent(t)
	e.outside = t.TempDir()
	cleanupTWrite(t, filepath.Join(e.outside, "secret.txt"), "outside secret", 0o600)
	if err := os.Mkdir(filepath.Join(e.outside, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	cleanupTWrite(t, filepath.Join(e.outside, "keep", "inner.txt"), "outside inner", 0o600)
	return e
}

func (e *cleanupTEnv) dataDir() string     { return filepath.Join(e.dataRoot, hostActApplyDataName) }
func (e *cleanupTEnv) artifactDir() string { return filepath.Join(e.optRoot, hostActApplyDataName) }
func (e *cleanupTEnv) unitFile() string    { return filepath.Join(e.unitRoot, hostActApplyUnitName) }
func (e *cleanupTEnv) binary() string      { return filepath.Join(e.artifactDir(), e.release) }
func (e *cleanupTEnv) marker() string {
	return filepath.Join(e.artifactDir(), domain.ProbeBinaryStageMarkerName)
}

func (e *cleanupTEnv) apply(ctl ProbeSystemctl) error {
	return ApplyProbeCleanup(context.Background(), e.dataParent, e.optParent, e.unitParent, e.intent, ctl)
}

func cleanupTWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func cleanupTMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// writeUnit writes the exact derived unit file.
func (e *cleanupTEnv) writeUnit(t *testing.T) {
	t.Helper()
	hostActApplyWriteUnit(t, e.unitRoot, e.intent)
}

// writeArtifact stages the artifact directory the way the stager leaves it.
func (e *cleanupTEnv) writeArtifact(t *testing.T, binary, marker bool) {
	t.Helper()
	cleanupTMkdir(t, e.artifactDir(), 0o700)
	if binary {
		cleanupTWrite(t, e.binary(), cleanupTContent, 0o555)
	}
	if marker {
		cleanupTWrite(t, e.marker(), "marker bytes", 0o444)
	}
}

// writeData builds a data tree like a started probe leaves: files, nested
// directories, and every kind of link that could lead outside it.
func (e *cleanupTEnv) writeData(t *testing.T) {
	t.Helper()
	dir := e.dataDir()
	cleanupTMkdir(t, dir, 0o700)
	cleanupTWrite(t, filepath.Join(dir, "identity.json"), "{}", 0o600)
	cleanupTWrite(t, filepath.Join(dir, "state.db"), "state", 0o600)
	cleanupTMkdir(t, filepath.Join(dir, "sub"), 0o700)
	cleanupTMkdir(t, filepath.Join(dir, "sub", "deeper"), 0o700)
	cleanupTWrite(t, filepath.Join(dir, "sub", "deeper", "probe.key"), "key", 0o600)
	// Links that must be unlinked, never followed.
	if err := os.Symlink(filepath.Join(e.outside, "secret.txt"), filepath.Join(dir, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.outside, "keep"), filepath.Join(dir, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.outside, "keep"), filepath.Join(dir, "sub", "deeper", "nested-dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../does-not-exist", filepath.Join(dir, "sub", "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(e.outside, "secret.txt"), filepath.Join(dir, "hard-link")); err != nil {
		t.Fatal(err)
	}
	// A FIFO must be unlinked without being opened.
	if err := unix.Mkfifo(filepath.Join(dir, "sub", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cleanupTSnapshot lists every entry beneath root without following links.
// Link counts are omitted on purpose: removing a hard link legitimately lowers
// the count on the object it shared with an outside file.
func cleanupTSnapshot(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			line := fmt.Sprintf("%s|%v", path, info.Mode())
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				line += "|" + target
			case info.Mode().IsRegular():
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				line += "|" + string(data)
			}
			out = append(out, line)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func cleanupTRequireSnapshot(t *testing.T, want []string, roots ...string) {
	t.Helper()
	if got := cleanupTSnapshot(t, roots...); !slices.Equal(got, want) {
		t.Fatalf("host state changed:\n got %v\nwant %v", got, want)
	}
}

func cleanupTRequireAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still present: %v", p, err)
		}
	}
}

func cleanupTRequirePresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s missing: %v", p, err)
		}
	}
}

// requireOutsideIntact proves nothing a link pointed at was touched.
func (e *cleanupTEnv) requireOutsideIntact(t *testing.T) {
	t.Helper()
	for path, want := range map[string]string{
		filepath.Join(e.outside, "secret.txt"):        "outside secret",
		filepath.Join(e.outside, "keep", "inner.txt"): "outside inner",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s was touched: %q %v", path, got, err)
		}
	}
	hostActApplyRequireEntries(t, e.outside, "keep", "secret.txt")
	hostActApplyRequireEntries(t, filepath.Join(e.outside, "keep"), "inner.txt")
}

func (e *cleanupTEnv) requireBorrowedParentsOpen(t *testing.T) {
	t.Helper()
	for _, p := range []*os.File{e.dataParent, e.optParent, e.unitParent} {
		hostActApplyRequireBorrowedParentOpen(t, p)
	}
}

// cleanupTSystemctl is a recording fake that answers the three cleanup verbs
// with success unless a per-verb override says otherwise. Any other verb is an
// error: cleanup must never run it.
func cleanupTSystemctl(onVerb map[string]func(args []string) (string, int, error)) *hostActApplySystemctl {
	return &hostActApplySystemctl{script: func(_ int, args []string) (string, int, error) {
		if len(args) > 0 {
			if fn, ok := onVerb[args[0]]; ok {
				return fn(args)
			}
			switch args[0] {
			case "disable", "daemon-reload", "reset-failed":
				return "", 0, nil
			}
		}
		return "", 127, errors.New("unexpected systemctl invocation")
	}}
}

func cleanupTWantCalls() [][]string {
	return [][]string{
		{"disable", "--now", hostActApplyUnitName},
		{"daemon-reload"},
		{"reset-failed", hostActApplyUnitName},
	}
}

// ---- nothing to do ----------------------------------------------------------

func TestApplyProbeCleanupNothingPresentIsNoOpWithoutSystemctl(t *testing.T) {
	e := newCleanupTEnv(t)
	bystander := filepath.Join(e.dataRoot, "uptime-phoenix-probe-"+hostActFrameOtherOperation)
	cleanupTMkdir(t, bystander, 0o700)
	before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot)

	ctl := cleanupTSystemctl(nil)
	hostActApplyRequireErr(t, e.apply(ctl), nil)
	hostActApplyRequireCalls(t, ctl, nil)
	// Nothing present, so no systemctl is needed at all.
	hostActApplyRequireErr(t, e.apply(nil), nil)

	cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot)
	e.requireBorrowedParentsOpen(t)
}

// ---- the full install ---------------------------------------------------------

func TestApplyProbeCleanupRemovesFullInstallExactly(t *testing.T) {
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)

	// Bystanders: another operation's resources and unrelated host entries.
	other := "uptime-phoenix-probe-" + hostActFrameOtherOperation
	cleanupTMkdir(t, filepath.Join(e.dataRoot, other), 0o700)
	cleanupTWrite(t, filepath.Join(e.dataRoot, other, "identity.json"), "{}", 0o600)
	cleanupTMkdir(t, filepath.Join(e.optRoot, other), 0o700)
	cleanupTWrite(t, filepath.Join(e.optRoot, "unrelated"), "x", 0o644)
	cleanupTWrite(t, filepath.Join(e.unitRoot, other+".service"), "[Unit]\n", 0o644)
	cleanupTWrite(t, filepath.Join(e.unitRoot, "sshd.service"), "[Unit]\n", 0o644)
	outsideBefore := cleanupTSnapshot(t, e.outside)
	otherState := []string{
		filepath.Join(e.dataRoot, other), filepath.Join(e.optRoot, other), filepath.Join(e.optRoot, "unrelated"),
		filepath.Join(e.unitRoot, other+".service"), filepath.Join(e.unitRoot, "sshd.service"),
	}

	// The unit file must still exist while the service is stopped, and must be
	// gone before systemd is told to reload.
	ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){
		"disable": hostActApplyHook(func() {
			cleanupTRequirePresent(t, e.unitFile(), e.binary(), e.dataDir())
		}, ""),
		"daemon-reload": hostActApplyHook(func() {
			cleanupTRequireAbsent(t, e.unitFile())
		}, ""),
	})
	hostActApplyRequireErr(t, e.apply(ctl), nil)
	hostActApplyRequireCalls(t, ctl, cleanupTWantCalls())

	cleanupTRequireAbsent(t, e.unitFile(), e.artifactDir(), e.dataDir())
	// Only this operation's three names went away.
	cleanupTRequirePresent(t, otherState...)
	hostActApplyRequireEntries(t, e.dataRoot, other)
	hostActApplyRequireEntries(t, e.optRoot, other, "unrelated")
	hostActApplyRequireEntries(t, e.unitRoot, other+".service", "sshd.service")
	// Links were removed, not followed.
	e.requireOutsideIntact(t)
	cleanupTRequireSnapshot(t, outsideBefore, e.outside)
	e.requireBorrowedParentsOpen(t)

	// Fixed arguments only: the bare unit name, never a path.
	for _, call := range ctl.recorded() {
		for _, arg := range call {
			if strings.Contains(arg, "/") {
				t.Fatalf("systemctl argument %q is a path", arg)
			}
		}
	}
}

func TestApplyProbeCleanupIsIdempotent(t *testing.T) {
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)

	ctl := cleanupTSystemctl(nil)
	hostActApplyRequireErr(t, e.apply(ctl), nil)
	hostActApplyRequireCalls(t, ctl, cleanupTWantCalls())

	again := cleanupTSystemctl(nil)
	hostActApplyRequireErr(t, e.apply(again), nil)
	hostActApplyRequireCalls(t, again, nil)
	hostActApplyRequireEntries(t, e.dataRoot)
	hostActApplyRequireEntries(t, e.optRoot)
	hostActApplyRequireEntries(t, e.unitRoot)
}

func TestApplyProbeCleanupRemovesEveryPartialInstall(t *testing.T) {
	// artifact: "" absent, "empty", "binary", "marker" or "both".
	cases := []struct {
		name     string
		unit     bool
		artifact string
		data     bool
	}{
		{"unit only", true, "", false},
		{"artifact both only", false, "both", false},
		{"artifact empty only", false, "empty", false},
		{"artifact binary only", false, "binary", false},
		{"artifact marker only", false, "marker", false},
		{"data tree only", false, "", true},
		{"unit and artifact", true, "both", false},
		{"unit and data", true, "", true},
		{"artifact and data", false, "both", true},
		{"half-staged artifact and data", false, "binary", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			if tc.unit {
				e.writeUnit(t)
			}
			if tc.artifact != "" {
				e.writeArtifact(t, tc.artifact == "both" || tc.artifact == "binary", tc.artifact == "both" || tc.artifact == "marker")
			}
			if tc.data {
				e.writeData(t)
			}
			ctl := cleanupTSystemctl(nil)
			hostActApplyRequireErr(t, e.apply(ctl), nil)
			if tc.unit {
				hostActApplyRequireCalls(t, ctl, cleanupTWantCalls())
			} else {
				// No unit file, so systemd is never consulted.
				hostActApplyRequireCalls(t, ctl, nil)
			}
			cleanupTRequireAbsent(t, e.unitFile(), e.artifactDir(), e.dataDir())
			hostActApplyRequireEntries(t, e.dataRoot)
			hostActApplyRequireEntries(t, e.optRoot)
			hostActApplyRequireEntries(t, e.unitRoot)
			e.requireOutsideIntact(t)
		})
	}
}

func TestApplyProbeCleanupPartialInstallWithoutSystemctlStillCleans(t *testing.T) {
	// A host whose systemctl cannot be located can still clean an install that
	// never wrote its unit file.
	e := newCleanupTEnv(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)
	hostActApplyRequireErr(t, e.apply(nil), nil)
	cleanupTRequireAbsent(t, e.artifactDir(), e.dataDir())
	e.requireOutsideIntact(t)
}

// ---- refusals before any effect -----------------------------------------------

func TestApplyProbeCleanupRefusesModifiedUnitAndRemovesNothing(t *testing.T) {
	unitBytes := func(e *cleanupTEnv) string { return domain.ProbeSystemdUnit(e.intent.Identity) }
	mutations := map[string]func(t *testing.T, e *cleanupTEnv){
		"extra directive": func(t *testing.T, e *cleanupTEnv) {
			cleanupTWrite(t, e.unitFile(), unitBytes(e)+"ExecStartPost=/bin/true\n", 0o644)
		},
		"truncated": func(t *testing.T, e *cleanupTEnv) {
			cleanupTWrite(t, e.unitFile(), unitBytes(e)[:len(unitBytes(e))-1], 0o644)
		},
		"empty": func(t *testing.T, e *cleanupTEnv) {
			cleanupTWrite(t, e.unitFile(), "", 0o644)
		},
		"wrong mode": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Chmod(e.unitFile(), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to exact bytes": func(t *testing.T, e *cleanupTEnv) {
			real := filepath.Join(e.outside, "real.service")
			cleanupTWrite(t, real, unitBytes(e), 0o644)
			if err := os.Remove(e.unitFile()); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, e.unitFile()); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Link(e.unitFile(), filepath.Join(e.outside, "linked.service")); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.unitFile()); err != nil {
				t.Fatal(err)
			}
			cleanupTMkdir(t, e.unitFile(), 0o755)
		},
		"fifo": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.unitFile()); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(e.unitFile(), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)
			mutate(t, e)
			before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot, e.outside)

			ctl := cleanupTSystemctl(nil)
			err := hostActApplyWithin(t, "cleanup", func() error { return e.apply(ctl) })
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
			hostActApplyRequireCalls(t, ctl, nil)
			cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot, e.outside)
			e.requireBorrowedParentsOpen(t)
		})
	}
}

func TestApplyProbeCleanupRefusesForeignArtifactContent(t *testing.T) {
	mutations := map[string]func(t *testing.T, e *cleanupTEnv){
		"extra file": func(t *testing.T, e *cleanupTEnv) {
			cleanupTWrite(t, filepath.Join(e.artifactDir(), "extra"), "x", 0o644)
		},
		"extra directory": func(t *testing.T, e *cleanupTEnv) {
			cleanupTMkdir(t, filepath.Join(e.artifactDir(), "sub"), 0o700)
		},
		"different release": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Rename(e.binary(), filepath.Join(e.artifactDir(), "uptime-phoenix-probe_9.9.9_linux_amd64")); err != nil {
				t.Fatal(err)
			}
		},
		"release is a symlink": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.binary()); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(e.outside, "secret.txt"), e.binary()); err != nil {
				t.Fatal(err)
			}
		},
		"release is a directory": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.binary()); err != nil {
				t.Fatal(err)
			}
			cleanupTMkdir(t, e.binary(), 0o700)
		},
		"release is hard linked": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Link(e.binary(), filepath.Join(e.outside, "linked-binary")); err != nil {
				t.Fatal(err)
			}
		},
		"marker is a fifo": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.marker()); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(e.marker(), 0o444); err != nil {
				t.Fatal(err)
			}
		},
		"marker is a symlink": func(t *testing.T, e *cleanupTEnv) {
			if err := os.Remove(e.marker()); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(e.outside, "secret.txt"), e.marker()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)
			mutate(t, e)
			before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot, e.outside)

			ctl := cleanupTSystemctl(nil)
			err := hostActApplyWithin(t, "cleanup", func() error { return e.apply(ctl) })
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
			// Refused before any effect: the service was not stopped either.
			hostActApplyRequireCalls(t, ctl, nil)
			cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot, e.outside)
		})
	}
}

func TestApplyProbeCleanupRefusesSymlinkedResourceRoots(t *testing.T) {
	for _, which := range []string{"artifact", "data"} {
		for _, dangling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dangling=%v", which, dangling), func(t *testing.T) {
				e := newCleanupTEnv(t)
				e.writeUnit(t)
				target := filepath.Join(e.outside, "keep")
				if dangling {
					target = filepath.Join(e.outside, "nowhere")
				}
				link := e.artifactDir()
				if which == "data" {
					link = e.dataDir()
					e.writeArtifact(t, true, true)
				} else {
					e.writeData(t)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot, e.outside)

				ctl := cleanupTSystemctl(nil)
				err := hostActApplyWithin(t, "cleanup", func() error { return e.apply(ctl) })
				hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteConflict)
				hostActApplyRequireCalls(t, ctl, nil)
				cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot, e.outside)
				e.requireOutsideIntact(t)
			})
		}
	}
}

func TestApplyProbeCleanupRefusesNonDirectoryResourceRoots(t *testing.T) {
	for _, which := range []string{"artifact", "data"} {
		t.Run(which, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			if which == "artifact" {
				cleanupTWrite(t, e.artifactDir(), "not a directory", 0o600)
			} else {
				cleanupTWrite(t, e.dataDir(), "not a directory", 0o600)
			}
			before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot)

			ctl := cleanupTSystemctl(nil)
			hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteConflict)
			hostActApplyRequireCalls(t, ctl, nil)
			cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot)
		})
	}
}

func TestApplyProbeCleanupRefusesPresentUnitWithoutSystemctl(t *testing.T) {
	var typedNil *hostActApplySystemctl
	for name, ctl := range map[string]ProbeSystemctl{"nil": nil, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)
			before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot)
			hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteInput)
			cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot)
		})
	}
}

func TestApplyProbeCleanupRefusesWrongIntentAndMissingParents(t *testing.T) {
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)
	before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot)
	ctl := cleanupTSystemctl(nil)

	intents := hostActApplyWrongKindIntents()
	for _, kind := range []string{domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart} {
		intents[kind] = hostActFrameIntent(kind)
	}
	for name, intent := range intents {
		t.Run(name, func(t *testing.T) {
			err := ApplyProbeCleanup(t.Context(), e.dataParent, e.optParent, e.unitParent, intent, ctl)
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteInput)
		})
	}
	t.Run("invalid owner", func(t *testing.T) {
		intent := e.intent
		intent.OwnerID = "not-a-uuid"
		hostActApplyRequireErr(t, ApplyProbeCleanup(t.Context(), e.dataParent, e.optParent, e.unitParent, intent, ctl), domain.ErrProbeBinaryStageWriteInput)
	})
	t.Run("nil context", func(t *testing.T) {
		hostActApplyRequireErr(t, ApplyProbeCleanup(nil, e.dataParent, e.optParent, e.unitParent, e.intent, ctl), domain.ErrProbeBinaryStageWriteInput) //nolint:staticcheck // a nil context is the refusal under test
	})
	for name, parents := range map[string][3]*os.File{
		"nil data parent": {nil, e.optParent, e.unitParent},
		"nil opt parent":  {e.dataParent, nil, e.unitParent},
		"nil unit parent": {e.dataParent, e.optParent, nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := ApplyProbeCleanup(t.Context(), parents[0], parents[1], parents[2], e.intent, ctl)
			hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteInput)
		})
	}

	hostActApplyRequireCalls(t, ctl, nil)
	cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot)
	e.requireBorrowedParentsOpen(t)
}

func TestApplyProbeCleanupCanceledContextBeforeEffects(t *testing.T) {
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)
	before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ctl := cleanupTSystemctl(nil)
	err := ApplyProbeCleanup(ctx, e.dataParent, e.optParent, e.unitParent, e.intent, ctl)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	hostActApplyRequireCalls(t, ctl, nil)
	cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot)
}

// ---- failures after an effect ---------------------------------------------------

func TestApplyProbeCleanupDisableFailureIsUnknownAndStopsThere(t *testing.T) {
	failures := map[string]func([]string) (string, int, error){
		"nonzero exit": func([]string) (string, int, error) { return "", 1, nil },
		"error":        func([]string) (string, int, error) { return "", -1, errors.New("boom") },
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)
			before := cleanupTSnapshot(t, e.dataRoot, e.optRoot, e.unitRoot, e.outside)

			ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){"disable": fail})
			hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteUnknown)
			hostActApplyRequireCalls(t, ctl, cleanupTWantCalls()[:1])
			// The unit file and both directories are left for a retry.
			cleanupTRequireSnapshot(t, before, e.dataRoot, e.optRoot, e.unitRoot, e.outside)
		})
	}
}

func TestApplyProbeCleanupDaemonReloadFailureIsUnknownAndRetryCompletes(t *testing.T) {
	failures := map[string]func([]string) (string, int, error){
		"nonzero exit": func([]string) (string, int, error) { return "", 1, nil },
		"error":        func([]string) (string, int, error) { return "", -1, errors.New("boom") },
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)

			ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){"daemon-reload": fail})
			hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteUnknown)
			hostActApplyRequireCalls(t, ctl, cleanupTWantCalls()[:2])
			// The unit file is already gone; the directories were not touched.
			cleanupTRequireAbsent(t, e.unitFile())
			cleanupTRequirePresent(t, e.binary(), e.marker(), e.dataDir())

			// A new cleanup finishes the job and needs no systemctl: the unit
			// file that would require it is gone.
			retry := cleanupTSystemctl(nil)
			hostActApplyRequireErr(t, e.apply(retry), nil)
			hostActApplyRequireCalls(t, retry, nil)
			cleanupTRequireAbsent(t, e.unitFile(), e.artifactDir(), e.dataDir())
			e.requireOutsideIntact(t)
		})
	}
}

func TestApplyProbeCleanupIgnoresResetFailedResult(t *testing.T) {
	results := map[string]func([]string) (string, int, error){
		"nonzero exit": func([]string) (string, int, error) { return "", 5, nil },
		"error":        func([]string) (string, int, error) { return "", -1, errors.New("no failed unit") },
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			e := newCleanupTEnv(t)
			e.writeUnit(t)
			e.writeArtifact(t, true, true)
			e.writeData(t)
			ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){"reset-failed": result})
			hostActApplyRequireErr(t, e.apply(ctl), nil)
			hostActApplyRequireCalls(t, ctl, cleanupTWantCalls())
			cleanupTRequireAbsent(t, e.unitFile(), e.artifactDir(), e.dataDir())
		})
	}
}

func TestApplyProbeCleanupContextCanceledAfterUnitRemovalIsUnknown(t *testing.T) {
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)

	ctx, cancel := context.WithCancel(t.Context())
	ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){
		"daemon-reload": hostActApplyHook(cancel, ""),
	})
	err := ApplyProbeCleanup(ctx, e.dataParent, e.optParent, e.unitParent, e.intent, ctl)
	hostActApplyRequireErr(t, err, domain.ErrProbeBinaryStageWriteUnknown)
	cleanupTRequireAbsent(t, e.unitFile())
	cleanupTRequirePresent(t, e.artifactDir(), e.dataDir())
}

func TestApplyProbeCleanupArtifactDirectoryGainingAFileBeforeRemovalIsUnknown(t *testing.T) {
	// A file that appears after inspection (the unit's service writing, say)
	// keeps the directory from being removed: rmdir fails closed as Unknown.
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){
		"disable": hostActApplyHook(func() {
			cleanupTWrite(t, filepath.Join(e.artifactDir(), "late"), "x", 0o600)
		}, ""),
	})
	hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteUnknown)
	cleanupTRequirePresent(t, filepath.Join(e.artifactDir(), "late"))
	cleanupTRequireAbsent(t, e.unitFile(), e.binary(), e.marker())
}

// ---- the data tree walk -------------------------------------------------------

func TestApplyProbeCleanupDataTreeDepthBoundIsUnknown(t *testing.T) {
	old := cleanupTreeMaxDepth
	cleanupTreeMaxDepth = 4
	t.Cleanup(func() { cleanupTreeMaxDepth = old })

	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	cleanupTMkdir(t, e.dataDir(), 0o700)
	deep := e.dataDir()
	for range 8 {
		deep = filepath.Join(deep, "d")
		cleanupTMkdir(t, deep, 0o700)
	}
	if err := os.Symlink(filepath.Join(e.outside, "keep"), filepath.Join(deep, "link")); err != nil {
		t.Fatal(err)
	}

	hostActApplyRequireErr(t, e.apply(cleanupTSystemctl(nil)), domain.ErrProbeBinaryStageWriteUnknown)
	// The unit and the artifact directory went first; the tree is left for a
	// retry, and nothing outside it was touched.
	cleanupTRequireAbsent(t, e.unitFile(), e.artifactDir())
	cleanupTRequirePresent(t, e.dataDir(), filepath.Join(deep, "link"))
	e.requireOutsideIntact(t)
}

func TestApplyProbeCleanupDataTreeEntryBound(t *testing.T) {
	old := cleanupTreeMaxEntries
	cleanupTreeMaxEntries = 10
	t.Cleanup(func() { cleanupTreeMaxEntries = old })

	build := func(t *testing.T, files int) *cleanupTEnv {
		t.Helper()
		e := newCleanupTEnv(t)
		cleanupTMkdir(t, e.dataDir(), 0o700)
		for i := range files {
			cleanupTWrite(t, filepath.Join(e.dataDir(), fmt.Sprintf("f%02d", i)), "x", 0o600)
		}
		return e
	}
	t.Run("over the bound", func(t *testing.T) {
		e := build(t, 11)
		hostActApplyRequireErr(t, e.apply(nil), domain.ErrProbeBinaryStageWriteUnknown)
		// The directory itself is never removed while the walk failed.
		cleanupTRequirePresent(t, e.dataDir())
	})
	t.Run("exactly at the bound", func(t *testing.T) {
		e := build(t, 10)
		hostActApplyRequireErr(t, e.apply(nil), nil)
		cleanupTRequireAbsent(t, e.dataDir())
	})
}

func TestApplyProbeCleanupDataTreeLargerThanOneReadBatch(t *testing.T) {
	// More entries than one directory read returns: removal rewinds between
	// batches, so none are skipped.
	e := newCleanupTEnv(t)
	cleanupTMkdir(t, e.dataDir(), 0o700)
	for i := range cleanupTreeBatch*2 + 17 {
		cleanupTWrite(t, filepath.Join(e.dataDir(), fmt.Sprintf("file-%04d", i)), "x", 0o600)
	}
	hostActApplyRequireErr(t, e.apply(nil), nil)
	cleanupTRequireAbsent(t, e.dataDir())
	hostActApplyRequireEntries(t, e.dataRoot)
}

// ---- the final absence check --------------------------------------------------

func TestApplyProbeCleanupNameReappearingBeforeVerificationIsUnknown(t *testing.T) {
	// reset-failed is the last systemctl call, and the directories are removed
	// after it, so recreating the unit file there models something putting a
	// name back behind the cleanup. The final check reads every name back and
	// fails closed instead of reporting a clean host.
	e := newCleanupTEnv(t)
	e.writeUnit(t)
	e.writeArtifact(t, true, true)
	e.writeData(t)
	ctl := cleanupTSystemctl(map[string]func([]string) (string, int, error){
		"reset-failed": hostActApplyHook(func() { e.writeUnit(t) }, ""),
	})
	hostActApplyRequireErr(t, e.apply(ctl), domain.ErrProbeBinaryStageWriteUnknown)
	hostActApplyRequireCalls(t, ctl, cleanupTWantCalls())
	cleanupTRequirePresent(t, e.unitFile())
	cleanupTRequireAbsent(t, e.artifactDir(), e.dataDir())
}
