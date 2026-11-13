package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// T20: the health frame a probe sends while its disk is full must say the
// store is not writable and must not advertise ready. Runs only on an
// isolated tmpfs mounted at /data (docs/TESTING.md, disk-full fixture).
func TestProbeHealthFrameReportsDiskFull(t *testing.T) {
	if os.Getenv("PHOENIX_EDGE_DISK_FULL_TEST") != "1" {
		t.Skip("requires isolated /data tmpfs and PHOENIX_EDGE_DISK_FULL_TEST=1")
	}
	requireIsolatedDataTmpfs(t)
	ctx := t.Context()
	dir := t.TempDir() // TMPDIR=/data puts the edge database on the tmpfs.
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	i := domain.EdgeIdentity{ProbeID: uuid.NewString(), StreamID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}
	store, err := edge.Open(ctx, dir, i)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edge.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "UPDATE edge_identity SET hub_id=?, connection_generation=1, last_created_seq=7, committed_seq=7", uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	healthy := frameHealth(t, store)
	if !healthy.Ready || !healthy.DBWritable || slices.Contains(healthy.Errors, "storage_unavailable") || healthy.CommittedSeq != 7 {
		t.Fatalf("baseline health frame not ready and writable: %+v", healthy)
	}

	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	filler, err := os.CreateTemp("/data", "probe-health-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = filler.Close()
		_ = os.Remove(filler.Name())
	}()
	block := make([]byte, 1<<20)
	full := false
	for n := 0; n < 64 && !full; n++ { // The fixture mounts 32 MiB.
		_, err := filler.Write(block)
		if errors.Is(err, syscall.ENOSPC) {
			full = true
		} else if err != nil {
			t.Fatalf("unexpected tmpfs write error: %v", err)
		}
	}
	if !full {
		t.Fatal("test did not exhaust the isolated tmpfs")
	}

	frame := frameHealth(t, store)
	if frame.Ready || frame.DBWritable || !slices.Contains(frame.Errors, "storage_unavailable") {
		t.Fatalf("full disk health frame claims writable or ready: %+v", frame)
	}
	if frame.CommittedSeq != 7 {
		t.Fatalf("full disk changed committed seq in health frame: %d", frame.CommittedSeq)
	}
	// The hub-side decoder refuses a frame that claims ready without writable
	// persistence, so a probe cannot paper over the condition.
	forged := frame
	forged.Ready = true
	if _, _, err := probe.DecodeHealth(encodeHealthFrame(t, forged)); err == nil {
		t.Fatal("decoder accepted ready without writable persistence")
	}

	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filler.Name()); err != nil {
		t.Fatal(err)
	}
	recovered := frameHealth(t, store)
	if !recovered.Ready || !recovered.DBWritable || slices.Contains(recovered.Errors, "storage_unavailable") {
		t.Fatalf("health frame did not recover after freeing space: %+v", recovered)
	}
}

// frameHealth runs the production health read, encodes it as the session
// would, and returns what the hub decoder accepts.
func frameHealth(t *testing.T, store *edge.Store) probe.Health {
	t.Helper()
	h, err := readEdgeHealth(t.Context(), store, true, 1, false)
	if err != nil {
		t.Fatalf("health unreadable: %v", err)
	}
	frame := encodeHealthFrame(t, h)
	_, decoded, err := probe.DecodeHealth(frame)
	if err != nil {
		t.Fatalf("hub refused health frame: %v", err)
	}
	if !bytes.Contains(frame, []byte(`"db_writable":`+map[bool]string{true: "true", false: "false"}[h.DBWritable])) {
		t.Fatalf("wire frame does not carry db_writable: %s", frame)
	}
	return decoded
}

func encodeHealthFrame(t *testing.T, h probe.Health) []byte {
	t.Helper()
	payload, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(probe.Envelope{ProtocolVersion: 1, Type: "health", MessageID: uuid.NewString(), SentAt: probe.Timestamp(time.Now().UTC()), ConnectionGeneration: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func requireIsolatedDataTmpfs(t *testing.T) {
	t.Helper()
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("cannot verify isolated tmpfs: %v", err)
	}
	for _, line := range strings.Split(string(mounts), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) == 2 && strings.Contains(" "+parts[0]+" ", " /data ") && strings.HasPrefix(parts[1], "tmpfs ") && os.TempDir() == "/data" {
			return
		}
	}
	t.Fatal("refusing to fill a non-isolated filesystem")
}
