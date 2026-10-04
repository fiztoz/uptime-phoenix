//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyCommandLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "confidential-path.key")
	t.Setenv("PROBE_SECRET_KEY_FILE", path)
	var out, diagnostic bytes.Buffer
	call := func(want int, args ...string) {
		t.Helper()
		out.Reset()
		diagnostic.Reset()
		if code := run(context.Background(), args, &out, &diagnostic); code != want {
			t.Fatalf("%v: exit %d, expected %d; diagnostic %s", args, code, want, diagnostic.String())
		}
		if strings.Contains(out.String()+diagnostic.String(), path) {
			t.Fatal("command exposed configured path")
		}
	}
	call(1, "check")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("check created a key")
	}
	call(0, "init")
	key, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), key) || bytes.Contains(diagnostic.Bytes(), key) {
		t.Fatal("command printed key material")
	}
	call(0, "check")
	call(1, "init")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, key) {
		t.Fatal("second init changed the key")
	}
	// --file overrides the environment, including when that env path is absent.
	t.Setenv("PROBE_SECRET_KEY_FILE", filepath.Join(dir, "unused"))
	call(0, "check", "--file", path)
	for _, args := range [][]string{nil, {"rotate"}, {"init", "--force"}, {"check", "extra"}, {"check", "--file", ""}} {
		call(2, args...)
	}
	for _, args := range [][]string{{"--help"}, {"init", "--help"}, {"check", "-h"}} {
		call(0, args...)
	}
	t.Setenv("PROBE_SECRET_KEY_FILE", "")
	call(2, "init")
	call(2, "check")
}
