package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// #192: a probe restored from an older backup has created fewer sequences than
// the hub already committed. The handshake still refuses it (the hub cursor
// cannot be ahead of the source), but the hub now says why, loudly, instead of
// looping through silent reconnects that show the probe as Disconnected.
func TestHubTransport_RestoredProbeBehindCursorLogsLoudly(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := setupControlledHubPeer(t)
	s.input.CommittedSeq = 2288
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- s.transport.Run(ctx, s.input, func(context.Context, domain.ProbeQueueReport) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, nil)
	}()
	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case err := <-runErrCh:
		t.Fatalf("Run exited before connection: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for peer connection")
	}
	defer func() { _ = conn.CloseNow() }()

	hello := Hello{
		SessionIdentity:  SessionIdentity{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID, StreamID: s.input.Connection.StreamID},
		AgentVersion:     "test",
		ProtocolMin:      1,
		ProtocolMax:      1,
		ConfigRevision:   s.snapshot.Revision,
		FirstRetainedSeq: 1959,
		LastCreatedSeq:   2000,
		Capabilities:     []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"},
		ResourceBindings: []ResourceBinding{},
	}
	frame, err := encodeFrame("hello", 0, hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("a probe behind the hub cursor must not be admitted")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the handshake refusal")
	}

	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["probe_id"] == s.input.Connection.ProbeID && record["level"] == "ERROR" {
			found = record
		}
	}
	if found == nil {
		t.Fatalf("no ERROR record for the restored probe in %q", buf.String())
	}
	if found["stream_id"] != s.input.Connection.StreamID || found["hub_committed_seq"] != float64(2288) || found["probe_last_created_seq"] != float64(2000) {
		t.Fatalf("restored-probe record: %v", found)
	}
}
