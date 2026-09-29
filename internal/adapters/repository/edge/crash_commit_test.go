package edge

import (
	"bufio"
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestEdgeCheckCrashAroundCommit kills a separate Go process while a real
// SQLite transaction is in progress, and again after a commit has returned.
// Each child uses a fresh real edge database in the parent's temporary dir.
func TestEdgeCheckCrashAroundCommit(t *testing.T) {
	for _, phase := range []string{"inside-transaction", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestEdgeCheckCrashChild$")
			cmd.Env = append(os.Environ(), "PHOENIX_EDGE_CRASH_CHILD="+phase, "PHOENIX_EDGE_CRASH_DIR="+dir)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			signals := make(chan string, 1)
			go func() {
				line, readErr := bufio.NewReader(stdout).ReadString('\n')
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					signals <- "read error: " + readErr.Error()
					return
				}
				signals <- strings.TrimSpace(line)
			}()
			want := "IN_TX"
			if phase == "after-commit" {
				want = "COMMITTED"
			}
			select {
			case got := <-signals:
				if got != want {
					t.Fatalf("child failed to reach %s: %q (%s)", phase, got, stderr.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("child did not reach %s; %s", phase, stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("child exited normally instead of being killed")
			}

			s, err := Open(t.Context(), dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
			if err != nil {
				t.Fatalf("restart after abrupt kill: %v", err)
			}
			defer func() { _ = s.Close() }()
			identity, err := s.ReadIdentity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			wantSeq := int64(0)
			wantCount := 0
			if phase == "after-commit" {
				wantSeq = 2 // Observation and incident transition.
				wantCount = 1
			}
			if identity.LastCreatedSeq != wantSeq {
				t.Fatalf("torn stream counter after %s: got %d want %d", phase, identity.LastCreatedSeq, wantSeq)
			}
			for table, expected := range map[string]int{
				"edge_regional_state":   wantCount,
				"edge_alerts":           wantCount,
				"edge_delivery_outbox":  wantCount,
				"edge_telemetry_outbox": 2 * wantCount,
			} {
				var count int
				if err := s.db.NewRaw("SELECT COUNT(*) FROM "+table).Scan(t.Context(), &count); err != nil || count != expected {
					t.Fatalf("%s after %s: got %d want %d err=%v", table, phase, count, expected, err)
				}
			}
			if phase == "inside-transaction" {
				// The test trigger only pauses the first attempt. Remove it before retry.
				if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER stop_on_seq"); err != nil {
					t.Fatal(err)
				}
				got, err := s.CommitEdgeCheck(t.Context(), checkRecord())
				if err != nil || got.Seq != 1 {
					t.Fatalf("retry after crash: seq=%d err=%v", got.Seq, err)
				}
			} else {
				if _, err := s.CommitEdgeCheck(t.Context(), checkRecord()); !errors.Is(err, ports.ErrStaleLocalState) {
					t.Fatalf("duplicate after completed commit was accepted: %v", err)
				}
			}
			identity, err = s.ReadIdentity(t.Context())
			if err != nil || identity.LastCreatedSeq != 2 {
				t.Fatalf("single durable retry/commit after %s: %+v %v", phase, identity, err)
			}
		})
	}
}

// TestEdgeCheckCrashChild is only invoked as the isolated child of the parent
// test; normal package runs skip it. A test-only SQLite trigger stops the
// production transaction AFTER its state/intent writes but BEFORE its sequence
// update/commit, so the parent can SIGKILL it at a deterministic boundary.
func TestEdgeCheckCrashChild(t *testing.T) {
	phase := os.Getenv("PHOENIX_EDGE_CRASH_CHILD")
	if phase == "" {
		t.Skip("child process of TestEdgeCheckCrashAroundCommit only")
	}
	if phase != "inside-transaction" && phase != "after-commit" {
		t.Fatal("invalid crash phase")
	}
	if phase == "inside-transaction" {
		if err := sqlite.RegisterScalarFunction("edge_crash_stop", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			_, _ = fmt.Fprintln(os.Stdout, "IN_TX")
			for {
				time.Sleep(time.Hour)
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(t.Context(), os.Getenv("PHOENIX_EDGE_CRASH_DIR"), testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	if phase == "inside-transaction" {
		_, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER stop_on_seq BEFORE UPDATE OF last_created_seq ON edge_identity WHEN NEW.last_created_seq > OLD.last_created_seq BEGIN SELECT edge_crash_stop(); END")
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CommitEdgeCheck(t.Context(), checkRecord())
	if err != nil || got.Seq != 1 {
		t.Fatalf("child commit: seq=%d err=%v", got.Seq, err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "COMMITTED")
	for {
		time.Sleep(time.Hour)
	}
}
