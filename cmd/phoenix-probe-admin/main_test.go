package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Use the real command entry point in separate processes. Capturing only the
// injected result writer would miss migrations accidentally writing to stdout.
func TestProbeAdminHelperProcess(t *testing.T) {
	if os.Getenv("PHOENIX_PROBE_ADMIN_TEST_PROCESS") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing command arguments")
	}
	os.Args = append([]string{"phoenix-probe-admin"}, os.Args[separator+1:]...)
	main()
}

func TestProbeAdminWatchdogSettingsAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyFile, policyFile := filepath.Join(dir, "hub.key"), filepath.Join(dir, "endpoints.json")
	if err := os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyFile, []byte(`{"allowed_cidrs":["127.0.0.1/32"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PHOENIX_PROBE_ADMIN_TEST_PROCESS=1", "DB_ENGINE=sqlite",
		"DB_DSN=file:"+filepath.Join(dir, "hub.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)",
		"PROBE_SECRET_KEY_FILE="+keyFile, "PROBE_ENDPOINT_POLICY_FILE="+policyFile,
		"PROBES_ENABLED=false", "PROBE_HUB_ID=", "JWT_SECRET=private-cli-test-only-secret", "PRODUCTION=false")
	type result struct {
		State           string `json:"state"`
		AppliedRevision string `json:"applied_revision"`
		Watchdog        struct {
			Revision         string  `json:"revision"`
			Enabled          bool    `json:"enabled"`
			LostAfterSeconds int     `json:"lost_after_seconds"`
			NotificationIDs  []int64 `json:"notification_ids"`
		} `json:"watchdog"`
	}
	run := func(success bool, args ...string) result {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestProbeAdminHelperProcess$", "--"}, args...)...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if (err == nil) != success {
			t.Fatalf("%s exit: %v; %s", args[0], err, stderr.String())
		}
		var out result
		if success {
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("%s did not return one JSON result: %v; stdout=%s", args[0], err, stdout.String())
			}
		} else if stdout.Len() != 0 {
			t.Fatal("failed command emitted a success result")
		}
		return out
	}
	const probeID = "26dbf694-6b24-4e35-b710-cf8734c6015d"
	run(true, "register", "--probe-id", probeID, "--stream-id", "443c8055-a01f-4361-b01c-9425b0c920be", "--key", "bkk-edge", "--name", "Bangkok", "--location", "TH", "--endpoint", "wss://127.0.0.1:49999/ws/probe/v1", "--fingerprint", "abababababababababababababababababababababababababababababababab")
	s := run(true, "status", "--probe-id", probeID)
	if s.Watchdog.Revision != "0" || s.Watchdog.Enabled {
		t.Fatal("new registration not disabled by default")
	}
	s = run(true, "watchdog", "--probe-id", probeID, "--expected-revision", "0", "--enabled=false")
	if s.Watchdog.Revision != "0" {
		t.Fatal("unchanged default created an edit")
	}
	s = run(true, "watchdog", "--probe-id", probeID, "--expected-revision", "0", "--enabled=true", "--lost-after-seconds", "120", "--resend-interval", "5")
	if s.State != "watchdog_saved" || s.Watchdog.Revision != "1" {
		t.Fatal("settings did not commit")
	}
	s = run(true, "status", "--probe-id", probeID)
	if !s.Watchdog.Enabled || s.Watchdog.LostAfterSeconds != 120 || s.AppliedRevision != "0" || len(s.Watchdog.NotificationIDs) != 0 {
		t.Fatal("restart lost intent or invented an applied receipt")
	}
	run(false, "watchdog", "--probe-id", probeID, "--expected-revision", "0", "--enabled=false")
	run(false, "watchdog", "--probe-id", probeID, "--expected-revision", "1")
	run(false, "watchdog", "--probe-id", probeID, "--expected-revision", "1", "--enabled=false", "--lost-after-seconds", "4294967386")
	s = run(true, "watchdog", "--probe-id", probeID, "--expected-revision", "1", "--enabled=false")
	if s.Watchdog.Revision != "2" || s.Watchdog.Enabled {
		t.Fatal("complete disable did not persist")
	}
}
