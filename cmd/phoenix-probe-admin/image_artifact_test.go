package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func TestProbeAdminImageMetadataAcrossProcesses(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local metadata source is supported on Linux/macOS only")
	}
	f := testutil.NewProbeImageFixture(t, "arm64", true)
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "index"), filepath.Join(dir, "manifest"), filepath.Join(dir, "config")}
	for i, b := range [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes} {
		if err := os.WriteFile(paths[i], b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A Docker invocation would leave a marker. It must never occur.
	marker := filepath.Join(dir, "docker-was-executed")
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	run := func(args ...string) ([]byte, []byte, error) {
		argv := append([]string{"-test.run=^TestProbeAdminHelperProcess$", "--", "verify-image"}, args...)
		cmd := exec.CommandContext(ctx, executable, argv...)
		cmd.Env = []string{"PHOENIX_PROBE_ADMIN_TEST_PROCESS=1", "DB_ENGINE=invalid", "PRODUCTION=not-a-boolean", "PATH=" + dir, "TMPDIR=" + dir}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		return out.Bytes(), stderr.Bytes(), err
	}
	args := []string{"--repository", f.Pin.Repository, "--version", f.Pin.Version, "--architecture", "arm64", "--index", paths[0], "--index-sha256", f.Pin.IndexSHA256, "--index-size", strconv.FormatInt(f.Pin.IndexSize, 10), "--manifest", paths[1], "--config", paths[2]}
	out, stderr, err := run(args...)
	if err != nil || len(stderr) != 0 {
		t.Fatalf("DB-independent image dispatch failed: %v %s", err, stderr)
	}
	var view map[string]json.RawMessage
	if json.Unmarshal(out, &view) != nil || len(view) != 14 || string(view["state"]) != `"image_metadata_verified"` || string(view["layers_verified"]) != "false" || string(view["runtime_version_verified"]) != "false" {
		t.Fatal("unsafe or invented image receipt", string(out))
	}
	if bytes.Contains(out, []byte(dir)) || bytes.Contains(out, []byte("sensitive-")) {
		t.Fatal("private metadata escaped")
	}
	out, stderr, err = run("--help")
	if err != nil || len(stderr) != 0 || !bytes.Contains(out, []byte("No registry, daemon")) {
		t.Fatal("image help loaded hub config")
	}
	args[9] = "0000000000000000000000000000000000000000000000000000000000000000"
	out, stderr, err = run(args...)
	if err == nil || len(out) != 0 || !bytes.Contains(stderr, []byte("digest mismatch")) {
		t.Fatal("untrusted image index accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("metadata verification executed Docker")
	}
}
