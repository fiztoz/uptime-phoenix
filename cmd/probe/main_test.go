package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

func TestProbeCommandExplicitInitializationAndSecretRetention(t *testing.T) {
	t.Setenv("PROBE_SECRET_KEY_FILE", "")
	dir := filepath.Join(t.TempDir(), "edge")
	invoke := func(action string) (int, map[string]any) {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := run(t.Context(), []string{action, "--data-dir", dir}, &out, &diagnostic)
		var value map[string]any
		if code == 0 {
			if err := json.Unmarshal(out.Bytes(), &value); err != nil {
				t.Fatal("invalid CLI JSON")
			}
		}
		return code, value
	}
	if code, _ := invoke("inspect"); code == 0 {
		t.Fatal("inspect silently initialized missing identity")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("read-only command created data directory")
	}
	code, first := invoke("init")
	if code != 0 || first["probe_id"] == "" || first["stream_id"] == "" || first["enrollment_token"] == nil {
		t.Fatal("initialization did not return operator identity and token")
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, "config.key"))
	if err != nil || len(keyBefore) != 32 {
		t.Fatal("missing protected key")
	}
	code, second := invoke("init")
	if code != 0 || first["probe_id"] != second["probe_id"] || first["stream_id"] != second["stream_id"] || first["certificate_fingerprint"] != second["certificate_fingerprint"] || first["enrollment_token"] == second["enrollment_token"] {
		t.Fatal("repeat init replaced identity or reused authorization")
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, "config.key"))
	if err != nil || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("repeat init rotated protection key")
	}
	code, inspection := invoke("inspect")
	if code != 0 || inspection["enrollment_token"] != nil || inspection["token_hash"] != nil || inspection["private_key"] != nil {
		t.Fatal("inspection exposed secret material")
	}
	locked, err := probe.OpenRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := invoke("token"); code == 0 {
		t.Fatal("second local owner acquired identity")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "config.key")); err != nil {
		t.Fatal(err)
	}
	if code, _ := invoke("init"); code == 0 {
		t.Fatal("missing retained key was regenerated")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.key")); !os.IsNotExist(err) {
		t.Fatal("recovery path wrote replacement key")
	}
}
