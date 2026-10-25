package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// goToolPath resolves the go command from PATH. runtime.GOROOT is deprecated
// because the build-time root is not meaningful once a binary moves hosts.
func goToolPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locating the go command: %v", err)
	}
	return path
}

func TestProbeAdminBinaryArtifactAcrossProcesses(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("offline artifact source is supported on Linux/macOS only")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "probe")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	// Exercise the actual release probe, not only the minimal adapter fixtures.
	build := exec.CommandContext(ctx, goToolPath(t), "build", "-trimpath", "-ldflags=-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=0.4.5", "-o", binary, "./cmd/probe")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("release probe build: %v: %s", err, data)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	expected := hex.EncodeToString(digest[:])
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	run := func(args ...string) ([]byte, []byte, error) {
		t.Helper()
		argv := append([]string{"-test.run=^TestProbeAdminHelperProcess$", "--", "verify-artifact"}, args...)
		cmd := exec.CommandContext(ctx, executable, argv...)
		cmd.Env = []string{"PHOENIX_PROBE_ADMIN_TEST_PROCESS=1", "DB_ENGINE=invalid", "PRODUCTION=not-a-boolean", "TMPDIR=" + staging}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		return out.Bytes(), stderr.Bytes(), err
	}
	args := []string{"--file", binary, "--version", "0.4.5", "--architecture", "amd64", "--sha256", expected, "--size", strconv.Itoa(len(data))}
	out, stderr, err := run(args...)
	if err != nil || len(stderr) != 0 {
		t.Fatalf("real binary preparation failed: %v %s", err, stderr)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 8 || string(result["state"]) != "\"artifact_verified\"" || string(result["version_source"]) != "\"operator_pin\"" || string(result["pinned_version"]) != "\"0.4.5\"" || bytes.Contains(out, []byte(binary)) {
		t.Fatal("unsafe or invented view", string(out))
	}
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatal("CLI snapshot retained after success")
	}
	// Version is a trusted label, not a fabricated extraction from -trimpath.
	args[3] = "0.4.6"
	out, stderr, err = run(args...)
	if err != nil || len(stderr) != 0 || !bytes.Contains(out, []byte("\"pinned_version\":\"0.4.6\"")) {
		t.Fatal("version provenance contract changed")
	}
	args[7] = strings.Repeat("0", 64)
	out, stderr, err = run(args...)
	if err == nil || len(out) != 0 || !bytes.Contains(stderr, []byte("digest mismatch")) {
		t.Fatal("wrong digest succeeded")
	}
	entries, err = os.ReadDir(staging)
	if err != nil || len(entries) != 0 {
		t.Fatal("CLI partial snapshot retained")
	}
	out, stderr, err = run("--help")
	if err != nil || len(stderr) != 0 || !bytes.Contains(out, []byte("No automatic checksum trust")) {
		t.Fatal("local dispatch loaded hub config")
	}
}
