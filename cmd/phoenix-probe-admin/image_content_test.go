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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

// TestProbeAdminImageContentAcrossProcesses proves verify-image-content dispatches
// before hub configuration/DB loading and returns only fixed, safe non-claiming DTOs.
func TestProbeAdminImageContentAcrossProcesses(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local image sources are supported on Linux/macOS only")
	}
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
	dir := t.TempDir()
	names := []string{"index", "manifest", "config"}
	blobs := [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes}
	paths := make([]string, 0, 3+len(f.Layers))
	for i, b := range blobs {
		p := filepath.Join(dir, names[i])
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for i, b := range f.Layers {
		p := filepath.Join(dir, "layer-"+strconv.Itoa(i))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
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
		argv := append([]string{"-test.run=^TestProbeAdminHelperProcess$", "--", "verify-image-content"}, args...)
		cmd := exec.CommandContext(ctx, executable, argv...)
		cmd.Env = []string{"PHOENIX_PROBE_ADMIN_TEST_PROCESS=1", "DB_ENGINE=invalid", "PRODUCTION=not-a-boolean", "PATH=" + dir, "TMPDIR=" + dir}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		return out.Bytes(), stderr.Bytes(), err
	}
	args := make([]string, 0, 16+2*(len(paths)-3))
	args = append(args, "--repository", f.Pin.Repository, "--version", f.Pin.Version, "--architecture", "amd64", "--index", paths[0], "--index-sha256", f.Pin.IndexSHA256, "--index-size", strconv.FormatInt(f.Pin.IndexSize, 10), "--manifest", paths[1], "--config", paths[2])
	for _, p := range paths[3:] {
		args = append(args, "--layer", p)
	}
	out, stderr, err := run(args...)
	if err != nil || len(stderr) != 0 {
		t.Fatalf("DB-independent image content dispatch failed: %v %s", err, stderr)
	}
	var view map[string]json.RawMessage
	if json.Unmarshal(out, &view) != nil || len(view) != 19 ||
		string(view["state"]) != `"image_content_digests_verified"` ||
		string(view["version_source"]) != `"digest_bound_config_label"` ||
		string(view["layer_digests_verified"]) != "true" ||
		string(view["diff_ids_verified"]) != "true" ||
		string(view["rootfs_verified"]) != "false" ||
		string(view["runtime_version_verified"]) != "false" ||
		string(view["content_retained"]) != "false" ||
		string(view["layer_count"]) != "2" {
		t.Fatal("unsafe or invented image content receipt", string(out))
	}
	if bytes.Contains(out, []byte(dir)) || bytes.Contains(out, []byte("sensitive-")) {
		t.Fatal("private content escaped")
	}
	out, stderr, err = run("--help")
	if err != nil || len(stderr) != 0 || !bytes.Contains(out, []byte("No registry, daemon")) {
		t.Fatal("image content help loaded hub config")
	}
	// Flag refusal stays local and fixed: no layer arguments at all.
	out, stderr, err = run(args[:len(args)-4]...)
	if err == nil || len(out) != 0 || !bytes.Contains(stderr, []byte("Usage:")) || bytes.Contains(stderr, []byte(dir)) {
		t.Fatal("layer-less invocation accepted or leaked", string(stderr))
	}
	// Ordered layer bytes with one flipped byte refuse with the fixed digest error.
	layer := append([]byte(nil), f.Layers[0]...)
	layer[len(layer)-1] ^= 0xff
	if err := os.WriteFile(paths[3], layer, 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = run(args...)
	if err == nil || len(out) != 0 || !bytes.Contains(stderr, []byte("digest mismatch")) || bytes.Contains(stderr, []byte(dir)) {
		t.Fatal("untrusted layer bytes accepted or leaked", string(stderr))
	}
	if err := os.WriteFile(paths[3], f.Layers[0], 0o600); err != nil {
		t.Fatal(err)
	}
	// Nonregular sources must be refused without blocking: device and, where
	// supported, a named pipe that would hang a blocking open forever.
	for _, kind := range []string{"device", "fifo", "directory"} {
		target := "/dev/null"
		switch kind {
		case "fifo":
			target = filepath.Join(dir, "layer.fifo")
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
		case "directory":
			target = filepath.Join(dir, "layer-dir")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		argv := append([]string(nil), args[:len(args)-2]...)
		argv = append(argv, "--layer", target)
		out, stderr, err = run(argv...)
		if err == nil || len(out) != 0 || len(stderr) == 0 || bytes.Contains(stderr, []byte(target)) || strings.Contains(string(stderr), "sensitive-") {
			t.Fatal(kind, "source accepted or leaked", string(stderr))
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("content verification executed Docker")
	}
}
