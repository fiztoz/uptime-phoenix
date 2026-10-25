//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func stageBinaryCLIFixture(t *testing.T, arch string) []byte {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                      "module github.com/fiztoz/uptime-phoenix\n\ngo 1.23\n",
		"internal/version/version.go": "package version\nvar Version=\"dev\"\n",
		"cmd/probe/main.go": `package main
import("os";"github.com/fiztoz/uptime-phoenix/internal/version")
func main(){if p:=os.Getenv("PHOENIX_ARTIFACT_EXECUTION_CANARY");p!=""{_=os.WriteFile(p,[]byte("executed"),0600)};println(version.Version)}
`,
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goToolPath(t), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=0.4.5", "-o", "fixture", "./cmd/probe")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static canary fixture: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func stageBinaryCLIIntent(t *testing.T, data []byte, arch string) domain.ProbeBinaryStageIntent {
	t.Helper()
	sum := sha256.Sum256(data)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	intent := domain.ProbeBinaryStageIntent{Identity: domain.ProbeInstallIdentity{OperationID: "11111111-1111-4111-8111-111111111111", Host: domain.ProbeSSHHost{Address: "example.test:22", User: "deploy"}, HostKeyFingerprint: fp, ClientKeyFingerprint: fp, RequestedBy: 1, LayoutVersion: 1, Mode: domain.ProbeInstallSystemd, Binary: domain.ProbeBinaryPin{Version: "0.4.5", Architecture: arch, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}}, OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333"}
	if !intent.Valid() {
		t.Fatal("CLI fixture intent")
	}
	return intent
}

func stageBinaryCLIFrame(t *testing.T, intent domain.ProbeBinaryStageIntent, data []byte) []byte {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal(err)
	}
	return append(header, data...)
}

func assertStageBinaryCLIGraph(t *testing.T, parent string, intent domain.ProbeBinaryStageIntent, data []byte) {
	t.Helper()
	name := (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatal("CLI relative stage directory", err)
	}
	dir := filepath.Join(parent, name)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("CLI directory mode", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("CLI closed stage graph", err)
	}
	for filename, want := range map[string][]byte{intent.Identity.Binary.ReleaseName(): data, domain.ProbeBinaryStageMarkerName: []byte(intent.Marker())} {
		p := filepath.Join(dir, filename)
		info, err := os.Lstat(p)
		perm := os.FileMode(0555)
		if filename == domain.ProbeBinaryStageMarkerName {
			perm = 0444
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != perm {
			t.Fatal("CLI stage mode/type", err)
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("CLI did not persist exact bytes/marker", err)
		}
	}
}

func TestProbeAdminStageBinaryAcrossProcesses(t *testing.T) {
	// Run the actual admin executable, not a test-only dispatch stub.
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	executable := filepath.Join(t.TempDir(), "phoenix-probe-admin")
	build := exec.CommandContext(ctx, goToolPath(t), "build", "-o", executable, "./cmd/phoenix-probe-admin")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("admin executable build: %v %s", err, out)
	}
	newCommand := func(t *testing.T, parent *os.File, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, string, string) {
		t.Helper()
		scratch := t.TempDir()
		work := t.TempDir()
		canary := filepath.Join(work, "must-not-execute")
		childCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(stop)
		cmd := exec.CommandContext(childCtx, executable, append([]string{"stage-binary"}, args...)...)
		cmd.Dir = work
		cmd.Env = []string{"DB_ENGINE=invalid", "PRODUCTION=not-a-boolean", "TMPDIR=" + scratch, "PHOENIX_ARTIFACT_EXECUTION_CANARY=" + canary}
		if parent != nil {
			cmd.ExtraFiles = []*os.File{parent}
		}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		return cmd, &out, &stderr, scratch, work
	}
	// Empty work directories detect unexpected side effects. A Linux fixture's
	// canary detects execution only on matching Linux hosts, not on Darwin.
	assertClean := func(t *testing.T, scratch, work string) {
		t.Helper()
		for _, p := range []string{scratch, work} {
			entries, err := os.ReadDir(p)
			if err != nil || len(entries) != 0 {
				t.Fatal("CLI retained snapshot or unexpected working-directory effects", err)
			}
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			data := stageBinaryCLIFixture(t, arch)
			intent := stageBinaryCLIIntent(t, data, arch)
			parent := t.TempDir()
			fd, err := os.Open(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fd.Close() }()
			cmd, out, stderr, scratch, work := newCommand(t, fd)
			cmd.Stdin = bytes.NewReader(stageBinaryCLIFrame(t, intent, data))
			if err := cmd.Run(); err != nil || stderr.Len() != 0 {
				t.Fatal("real inherited-fd helper failed", err, stderr.String())
			}
			var view map[string]json.RawMessage
			if json.Unmarshal(out.Bytes(), &view) != nil || len(view) != 9 || out.Len() > 2048 || bytes.Count(out.Bytes(), []byte{'\n'}) != 1 || out.Bytes()[out.Len()-1] != '\n' {
				t.Fatal("CLI receipt wire shape", out.String())
			}
			for key, want := range map[string]string{"state": "local_binary_staged", "directory_name": (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName(), "marker_sha256": intent.MarkerSHA256(), "pinned_version": "0.4.5", "version_source": "operator_pin", "os": "linux", "architecture": arch, "sha256": intent.Identity.Binary.SHA256} {
				var got string
				if json.Unmarshal(view[key], &got) != nil || got != want {
					t.Fatal("CLI response identity", key)
				}
			}
			var size int64
			if json.Unmarshal(view["size"], &size) != nil || size != int64(len(data)) || bytes.Contains(out.Bytes(), []byte(parent)) || bytes.Contains(out.Bytes(), []byte("example.test")) {
				t.Fatal("CLI unsafe response")
			}
			assertStageBinaryCLIGraph(t, parent, intent, data)
			assertClean(t, scratch, work)
			if _, err := fd.Stat(); err != nil {
				t.Fatal("CLI closed caller fd3", err)
			}
			cmd, out, stderr, scratch, work = newCommand(t, fd)
			cmd.Stdin = bytes.NewReader(stageBinaryCLIFrame(t, intent, data))
			if err := cmd.Run(); err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteConflict.Error()+"\n" {
				t.Fatal("CLI replay adopted/overwrote stage", err, stderr.String())
			}
			assertStageBinaryCLIGraph(t, parent, intent, data)
			assertClean(t, scratch, work)
			// Existing content is never repaired or adopted, even when it no
			// longer matches the request. Preserve foreign bytes and file identity.
			markerPath := filepath.Join(parent, (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName(), domain.ProbeBinaryStageMarkerName)
			if err := os.Chmod(markerPath, 0644); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("externally-modified-stage-marker\n")
			if err := os.WriteFile(markerPath, foreign, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(markerPath, 0444); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			cmd, out, stderr, scratch, work = newCommand(t, fd)
			cmd.Stdin = bytes.NewReader(stageBinaryCLIFrame(t, intent, data))
			if err := cmd.Run(); err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 || out.Len() != 0 || stderr.String() != domain.ErrProbeBinaryStageWriteConflict.Error()+"\n" {
				t.Fatal("CLI adopted foreign existing marker", err, stderr.String())
			}
			after, err := os.Stat(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(markerPath)
			if err != nil || !bytes.Equal(got, foreign) || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("CLI retry rewrote foreign marker", err)
			}
			assertClean(t, scratch, work)
		})
	}
	data := stageBinaryCLIFixture(t, "amd64")
	intent := stageBinaryCLIIntent(t, data, "amd64")
	for _, mode := range []string{"missing_fd", "regular_fd", "args", "bad_magic", "bad_size", "duplicate", "case_alias", "null", "unknown", "short_header", "short_body", "extra_body", "bad_digest", "bad_elf"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			fd, err := os.Open(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fd.Close() }()
			passed := fd
			var args []string
			item := intent
			body := data
			if mode == "bad_digest" {
				item.Identity.Binary.SHA256 = strings.Repeat("a", 64)
			}
			if mode == "bad_elf" {
				body = append([]byte(nil), data...)
				body[0] = 0
				item = stageBinaryCLIIntent(t, body, "amd64")
			}
			frame := stageBinaryCLIFrame(t, item, body)
			switch mode {
			case "missing_fd":
				passed = nil
			case "regular_fd":
				passed, err = os.CreateTemp(t.TempDir(), "not-directory")
				if err != nil {
					t.Fatal(err)
				}
				defer func(f *os.File) { _ = f.Close() }(passed)
			case "args":
				passed = nil
				args = []string{"--path=/private-canary"}
			case "bad_magic":
				frame[0] = 'x'
			case "bad_size":
				binary.BigEndian.PutUint32(frame[8:12], 4097)
			case "short_header":
				frame = frame[:11]
			case "short_body":
				frame = frame[:len(frame)-1]
			case "extra_body":
				frame = append(frame, 1)
			case "duplicate", "case_alias", "null", "unknown":
				n := int(binary.BigEndian.Uint32(frame[8:12]))
				h := string(frame[12 : 12+n])
				switch mode {
				case "duplicate":
					h = strings.Replace(h, `"requested_by":1,`, `"requested_by":1,"requested_by":1,`, 1)
				case "case_alias":
					h = strings.Replace(h, `"requested_by"`, `"Requested_By"`, 1)
				case "null":
					h = strings.Replace(h, `"requested_by":1`, `"requested_by":null`, 1)
				case "unknown":
					h = strings.Replace(h, `"requested_by":1,`, `"requested_by":1,"extra_field":"fixture",`, 1)
				}
				prefix := append([]byte(nil), frame[:12]...)
				binary.BigEndian.PutUint32(prefix[8:12], uint32(len(h)))
				frame = append(append(prefix, []byte(h)...), body...)
			}
			cmd, out, stderr, scratch, work := newCommand(t, passed, args...)
			cmd.Stdin = bytes.NewReader(frame)
			wantCode := 1
			wantDiagnostic := domain.ErrProbeBinaryStageWriteInput.Error() + "\n"
			switch mode {
			case "missing_fd", "regular_fd":
				wantDiagnostic = domain.ErrProbeBinaryStageWriteUnavailable.Error() + "\n"
			case "short_body", "extra_body":
				wantDiagnostic = domain.ErrProbeArtifactSize.Error() + "\n"
			case "bad_digest":
				wantDiagnostic = domain.ErrProbeArtifactDigest.Error() + "\n"
			case "bad_elf":
				wantDiagnostic = domain.ErrProbeArtifactMetadata.Error() + "\n"
			case "args":
				wantCode = 2
				wantDiagnostic = "Usage: phoenix-probe-admin stage-binary\nNo flags, paths, or arguments. Stdin is one frame under inherited directory descriptor 3.\nThis is NOT installation, enrollment, cleanup, or host authority.\n"
			}
			if err := cmd.Run(); err == nil || cmd.ProcessState == nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != wantCode || out.Len() != 0 || stderr.String() != wantDiagnostic {
				t.Fatal("CLI unsafe refusal/config startup", err, stderr.String())
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("CLI refusal changed parent", err)
			}
			assertClean(t, scratch, work)
		})
	}
	t.Run("withheld_eof_signal", func(t *testing.T) {
		parent := t.TempDir()
		fd, err := os.Open(parent)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fd.Close() }()
		cmd, out, stderr, scratch, work := newCommand(t, fd)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stdin.Close() }()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		joined := false
		defer func() {
			if !joined {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		if _, err := stdin.Write(stageBinaryCLIFrame(t, intent, data)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 0 {
			t.Fatal("CLI staged before actual pipe EOF", err)
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		err = cmd.Wait()
		joined = true
		if err == nil || out.Len() != 0 || cmd.ProcessState.ExitCode() != 1 || stderr.String() != domain.ErrProbeBinaryStageWriteUnknown.Error()+"\n" {
			t.Fatal("CLI signal did not join/refuse", err, stderr.String())
		}
		entries, err = os.ReadDir(parent)
		if err != nil || len(entries) != 0 {
			t.Fatal("signal created a stage", err)
		}
		assertClean(t, scratch, work)
	})
}
