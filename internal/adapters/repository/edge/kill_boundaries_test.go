package edge

import (
	"bufio"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// M6-F: restart/kill at the probe-side durable boundaries: the replay ACK,
// config activation and credential activation. A child copy of this test
// binary opens the parent's edge store and is SIGKILLed either inside the
// production transaction (paused by a test-only SQLite trigger, like T06) or at
// a random moment of a loop. The parent reopens the store and checks the
// boundary is all-or-nothing and that the next step after restart works.

const edgeKillEnv = "PHOENIX_EDGE_KILL_CHILD"

func edgeKillIterations() int {
	if n, err := strconv.Atoi(os.Getenv("PHOENIX_KILL_ITERATIONS")); err == nil && n > 20 {
		return n
	}
	return 20
}

// edgeKillRegisterStop registers the SQLite function a pause trigger calls.
// It must run before the store opens its connection.
func edgeKillRegisterStop(t *testing.T) {
	t.Helper()
	if err := sqlite.RegisterScalarFunction("edge_kill_stop", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		_, _ = fmt.Fprintln(os.Stdout, "IN_TX")
		select {}
	}); err != nil {
		t.Fatal(err)
	}
}

// edgeKillPause installs a test-only trigger (as T06 does) that blocks the
// child's write transaction at the given UPDATE, after the transaction's
// earlier writes. The parent drops it after the kill with edgeKillUnpause.
func edgeKillPause(t *testing.T, s *Store, trigger string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
}

func edgeKillUnpause(t *testing.T, s *Store, name string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), "DROP TRIGGER "+name); err != nil {
		t.Fatal(err)
	}
}

func edgeKillChildStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := os.Getenv("PHOENIX_EDGE_KILL_DIR")
	if os.Getenv(edgeKillEnv) == "inside-transaction" {
		edgeKillRegisterStop(t)
	}
	s, err := Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func edgeKillFail(err error) {
	_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
	select {}
}

// spawnEdgeKill runs child test name in dir and SIGKILLs it after marker, or
// after delay from READY when marker is empty. It returns the child's lines.
func spawnEdgeKill(t *testing.T, name, mode, dir, marker string, delay time.Duration) []string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), edgeKillEnv+"="+mode, "PHOENIX_EDGE_KILL_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ch := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			ch <- sc.Text()
		}
		close(ch)
	}()
	var lines []string
	want := marker
	if want == "" {
		want = "READY"
	}
	deadline := time.After(60 * time.Second)
wait:
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				t.Fatalf("child exited before %s: %v / %s", want, lines, stderr.String())
			}
			lines = append(lines, l)
			if strings.HasPrefix(l, "ERR ") {
				_ = cmd.Process.Kill()
				t.Fatalf("child failed: %v / %s", lines, stderr.String())
			}
			if l == want {
				break wait
			}
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatalf("child never printed %s: %v / %s", want, lines, stderr.String())
		}
	}
	if marker == "" {
		time.Sleep(delay)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for l := range ch { // drain before Wait closes the pipe
		lines = append(lines, l)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited normally instead of being killed")
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "ERR ") {
			t.Fatalf("child failed: %v", lines)
		}
	}
	return lines
}

func lastNumbered(lines []string, prefix string) int64 {
	var n int64
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			n, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return n
}

func reopenEdge(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatalf("restart after SIGKILL: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---- replay ACK ----

const edgeKillOutbox = 400

func edgeKillACKStore(t *testing.T) string {
	t.Helper()
	s, dir := testStore(t)
	enroll(t, s)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(1); i <= edgeKillOutbox; i++ {
		insertOutboxRow(t, s, i, domain.ReplayKindObservation, now, []byte("val"))
	}
	setIdentitySeqs(t, s, edgeKillOutbox, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEdgeKillReplayACKChild(t *testing.T) {
	mode := os.Getenv(edgeKillEnv)
	if mode == "" {
		t.Skip("child process of TestEdgeKillReplayACK")
	}
	s, _ := edgeKillChildStore(t)
	if mode == "inside-transaction" {
		// Fires after committed_seq moved and the first acknowledged row is gone.
		edgeKillPause(t, s, "CREATE TRIGGER kill_ack AFTER DELETE ON edge_telemetry_outbox BEGIN SELECT edge_kill_stop(); END")
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for seq := int64(7); ; seq += 7 {
		if seq > edgeKillOutbox {
			seq = edgeKillOutbox
		}
		if err := s.CommitReplayACK(t.Context(), validFence(), domain.ProbeReplayResult{StreamID: testIdentity().StreamID, CommittedSeq: seq}); err != nil {
			edgeKillFail(err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "A %d\n", seq)
		if seq == edgeKillOutbox {
			select {}
		}
	}
}

func checkEdgeACK(t *testing.T, s *Store, label string, acked int64) int64 {
	t.Helper()
	ctx := t.Context()
	i, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := i.CommittedSeq
	if c < acked || c > acked+7 || c%7 != 0 && c != edgeKillOutbox {
		t.Fatalf("%s: committed %d after last ACK %d", label, c, acked)
	}
	var n, lo, hi int64
	if err := s.db.NewRaw("SELECT COUNT(*), COALESCE(MIN(seq),0), COALESCE(MAX(seq),0) FROM edge_telemetry_outbox").Scan(ctx, &n, &lo, &hi); err != nil {
		t.Fatal(err)
	}
	if n != edgeKillOutbox-c || n > 0 && (lo != c+1 || hi != edgeKillOutbox) {
		t.Fatalf("%s: committed %d but outbox count=%d range %d..%d", label, c, n, lo, hi)
	}
	if c < edgeKillOutbox {
		b, err := s.ReadReplayBatch(ctx, c+1, 10, 512<<10)
		if err != nil || b == nil || b.FirstSeq != c+1 {
			t.Fatalf("%s: replay after restart does not resume at %d: %+v %v", label, c+1, b, err)
		}
	}
	return c
}

func TestEdgeKillReplayACK(t *testing.T) {
	if os.Getenv(edgeKillEnv) != "" || testing.Short() {
		t.Skip("parent only; spawns child processes")
	}
	t.Run("inside-transaction", func(t *testing.T) {
		dir := edgeKillACKStore(t)
		spawnEdgeKill(t, "TestEdgeKillReplayACKChild", "inside-transaction", dir, "IN_TX", 0)
		s := reopenEdge(t, dir)
		edgeKillUnpause(t, s, "kill_ack")
		if c := checkEdgeACK(t, s, "killed inside ACK", 0); c != 0 {
			t.Fatalf("ACK killed mid-transaction advanced to %d", c)
		}
		if err := s.CommitReplayACK(t.Context(), validFence(), domain.ProbeReplayResult{StreamID: testIdentity().StreamID, CommittedSeq: 7}); err != nil {
			t.Fatalf("retry ACK after restart: %v", err)
		}
		checkEdgeACK(t, s, "retried ACK", 7)
	})
	t.Run("random", func(t *testing.T) {
		seed := time.Now().UnixNano()
		rng := rand.New(rand.NewSource(seed))
		t.Logf("random kill seed %d", seed)
		var lost, partial int
		for i := 0; i < edgeKillIterations(); i++ {
			dir := edgeKillACKStore(t)
			lines := spawnEdgeKill(t, "TestEdgeKillReplayACKChild", "random", dir, "", time.Duration(rng.Int63n(int64(20*time.Millisecond))))
			acked := lastNumbered(lines, "A ")
			s := reopenEdge(t, dir)
			c := checkEdgeACK(t, s, fmt.Sprintf("iter %d", i), acked)
			if c > acked {
				lost++ // committed, but the caller never learned it
			}
			if c > 0 && c < edgeKillOutbox {
				partial++
			}
			// The same ACK again (the hub resends its result) is idempotent.
			if err := s.CommitReplayACK(t.Context(), validFence(), domain.ProbeReplayResult{StreamID: testIdentity().StreamID, CommittedSeq: c}); err != nil {
				t.Fatalf("iter %d: repeated ACK at %d: %v", i, c, err)
			}
			_ = s.Close()
		}
		t.Logf("%d kills: %d mid-stream, %d committed without the caller seeing it", edgeKillIterations(), partial, lost)
	})
}

// ---- config activation ----

func edgeKillConfig(t *testing.T, revision int64) domain.EdgeActiveConfig {
	c := protectedConfig(t, revision)
	c.Assignments = []domain.EdgeAssignmentIdentity{{MonitorID: 17, Generation: revision, Active: true}, {MonitorID: 18, Generation: revision, Active: revision%2 == 0}, {MonitorID: 19, Generation: revision, Active: true}}
	return c
}

func TestEdgeKillConfigActivationChild(t *testing.T) {
	mode := os.Getenv(edgeKillEnv)
	if mode == "" {
		t.Skip("child process of TestEdgeKillConfigActivation")
	}
	s, _ := edgeKillChildStore(t)
	if mode == "inside-transaction" {
		edgeKillPause(t, s, "CREATE TRIGGER kill_cfg BEFORE UPDATE OF config_revision ON edge_identity BEGIN SELECT edge_kill_stop(); END")
	}
	i, err := s.ReadIdentity(t.Context())
	if err != nil {
		edgeKillFail(err)
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for rev := i.ConfigRevision + 1; ; rev++ {
		if err := s.ActivateConfig(t.Context(), edgeKillConfig(t, rev)); err != nil {
			edgeKillFail(err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "V %d\n", rev)
	}
}

func checkEdgeConfig(t *testing.T, s *Store, label string, printed int64) int64 {
	t.Helper()
	ctx := t.Context()
	active, err := s.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatalf("%s: read active config after restart: %v", label, err)
	}
	r := active.Snapshot.Revision
	if r < printed || r > printed+1 {
		t.Fatalf("%s: active revision %d after last acknowledged %d", label, r, printed)
	}
	want := edgeKillConfig(t, r).Assignments
	if len(active.Assignments) != len(want) {
		t.Fatalf("%s: revision %d has %d assignments", label, r, len(active.Assignments))
	}
	for n, a := range active.Assignments {
		if a != want[n] {
			t.Fatalf("%s: revision %d assignment %+v, want %+v", label, r, a, want[n])
		}
	}
	var maxRev, stray int64
	if err := s.db.NewRaw("SELECT COALESCE(MAX(revision),0) FROM edge_config").Scan(ctx, &maxRev); err != nil || maxRev != r {
		t.Fatalf("%s: stored config revisions reach %d, active %d (%v)", label, maxRev, r, err)
	}
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_assignments WHERE revision <> ?", r).Scan(ctx, &stray); err != nil || stray != 0 {
		t.Fatalf("%s: %d assignments from another revision (%v)", label, stray, err)
	}
	return r
}

func TestEdgeKillConfigActivation(t *testing.T) {
	if os.Getenv(edgeKillEnv) != "" || testing.Short() {
		t.Skip("parent only; spawns child processes")
	}
	newStore := func(t *testing.T) string {
		s, dir := testStore(t)
		enroll(t, s)
		if err := s.ActivateConfig(t.Context(), edgeKillConfig(t, 1)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("inside-transaction", func(t *testing.T) {
		dir := newStore(t)
		spawnEdgeKill(t, "TestEdgeKillConfigActivationChild", "inside-transaction", dir, "IN_TX", 0)
		s := reopenEdge(t, dir)
		edgeKillUnpause(t, s, "kill_cfg")
		if r := checkEdgeConfig(t, s, "killed inside activation", 1); r != 1 {
			t.Fatalf("activation killed mid-transaction left revision %d", r)
		}
		if err := s.ActivateConfig(t.Context(), edgeKillConfig(t, 2)); err != nil {
			t.Fatalf("retry activation after restart: %v", err)
		}
		checkEdgeConfig(t, s, "retried activation", 2)
	})
	t.Run("random", func(t *testing.T) {
		dir := newStore(t)
		seed := time.Now().UnixNano()
		rng := rand.New(rand.NewSource(seed))
		t.Logf("random kill seed %d", seed)
		var unseen int
		var r int64 = 1
		for i := 0; i < edgeKillIterations(); i++ {
			lines := spawnEdgeKill(t, "TestEdgeKillConfigActivationChild", "random", dir, "", time.Duration(rng.Int63n(int64(20*time.Millisecond))))
			printed := lastNumbered(lines, "V ")
			if printed == 0 {
				printed = r
			}
			s := reopenEdge(t, dir)
			r = checkEdgeConfig(t, s, fmt.Sprintf("iter %d", i), printed)
			if r > printed {
				unseen++
			}
			// The same revision again (lost result) is accepted as a retry.
			if err := s.ActivateConfig(t.Context(), edgeKillConfig(t, r)); err != nil && !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("iter %d: retry of revision %d: %v", i, r, err)
			}
			_ = s.Close()
		}
		t.Logf("%d kills: final revision %d, %d activations committed without the caller seeing them", edgeKillIterations(), r, unseen)
	})
}

// ---- credential activation ----

type edgeKillCredential struct {
	Authority domain.EdgeCommandAuthority
	Activate  domain.ProbeCredentialCommand
	Prepare   domain.ProbeCredentialCommand
	OldToken  string
	NewToken  string
}

func TestEdgeKillCredentialActivationChild(t *testing.T) {
	mode := os.Getenv(edgeKillEnv)
	if mode == "" {
		t.Skip("child process of TestEdgeKillCredentialActivation")
	}
	s, dir := edgeKillChildStore(t)
	raw, err := os.ReadFile(filepath.Join(dir, "kill-credential.json"))
	if err != nil {
		edgeKillFail(err)
	}
	var c edgeKillCredential
	if err := json.Unmarshal(raw, &c); err != nil {
		edgeKillFail(err)
	}
	if mode == "inside-transaction" {
		edgeKillPause(t, s, "CREATE TRIGGER kill_cred BEFORE UPDATE OF activated_at ON edge_credential_rotations BEGIN SELECT edge_kill_stop(); END")
	}
	_, _ = os.Stdout.WriteString("READY\n")
	out, err := s.ApplyCredentialCommand(t.Context(), c.Authority, c.Activate)
	if err != nil || out.Status != "applied" {
		edgeKillFail(fmt.Errorf("activate: %+v %v", out, err))
	}
	_, _ = os.Stdout.WriteString("COMMITTED\n")
	select {}
}

func TestEdgeKillCredentialActivation(t *testing.T) {
	if os.Getenv(edgeKillEnv) != "" || testing.Short() {
		t.Skip("parent only; spawns child processes")
	}
	for _, phase := range []string{"inside-transaction", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			for i := 0; i < edgeKillIterations()/2; i++ {
				s, dir, a, prepare, oldToken, newToken := credentialFixture(t)
				s.commandNow = time.Now
				requireCredentialOutcome(t, s, a, prepare, "applied")
				old := credentialAuth(t, s, oldToken, 1, &prepare.OverlapExpiresAt)
				if _, err := s.AcceptCredentialConnection(t.Context(), old, 2); err != nil {
					t.Fatal(err)
				}
				a.ConnectionGeneration = 2
				activate := activation(prepare)
				raw, err := json.Marshal(edgeKillCredential{Authority: a, Activate: activate, Prepare: prepare, OldToken: oldToken, NewToken: newToken})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "kill-credential.json"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				marker := "IN_TX"
				if phase == "after-commit" {
					marker = "COMMITTED"
				}
				spawnEdgeKill(t, "TestEdgeKillCredentialActivationChild", phase, dir, marker, 0)
				s = reopenEdge(t, dir)
				if phase == "inside-transaction" {
					edgeKillUnpause(t, s, "kill_cred")
				}
				var activated int
				var receipts int
				if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_credential_rotations WHERE activated_at IS NOT NULL").Scan(t.Context(), &activated); err != nil {
					t.Fatal(err)
				}
				if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil {
					t.Fatal(err)
				}
				if phase == "inside-transaction" {
					if activated != 0 || receipts != 1 {
						t.Fatalf("iter %d: killed activation left activated=%d receipts=%d", i, activated, receipts)
					}
					// Still in overlap: both tokens authenticate, old one with its deadline.
					credentialAuth(t, s, oldToken, 1, &prepare.OverlapExpiresAt)
					credentialAuth(t, s, newToken, 2, &prepare.OverlapExpiresAt)
				} else {
					if activated != 1 || receipts != 2 {
						t.Fatalf("iter %d: committed activation left activated=%d receipts=%d", i, activated, receipts)
					}
					credentialAuth(t, s, newToken, 2, nil)
					credentialAuth(t, s, oldToken, 1, &prepare.OverlapExpiresAt)
				}
				// The probe reconnects with the new credential, and the hub resends
				// the activation it never saw answered.
				var deadline *time.Time
				if phase == "inside-transaction" {
					deadline = &prepare.OverlapExpiresAt // not yet activated: still the overlap
				}
				newBinding := credentialAuth(t, s, newToken, 2, deadline)
				if _, err := s.AcceptCredentialConnection(t.Context(), newBinding, 3); err != nil {
					t.Fatalf("iter %d: reconnect after restart: %v", i, err)
				}
				a.ConnectionGeneration = 3
				requireCredentialOutcome(t, s, a, activate, "applied")
				credentialAuth(t, s, newToken, 2, nil)
				if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_applied_commands").Scan(t.Context(), &receipts); err != nil || receipts != 2 {
					t.Fatalf("iter %d: receipts after retry %d (%v)", i, receipts, err)
				}
				_ = s.Close()
			}
		})
	}
}
