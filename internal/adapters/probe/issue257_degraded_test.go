package probe

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// A validated degraded health frame is contact, not a readiness assertion.
// The connector's independent lifecycle test covers the elapsed-time reset.
func TestIssue257DegradedHealthEstablishesTransport(t *testing.T) {
	s := setupControlledHubPeer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	established := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.transport.Run(ctx, s.input, func(context.Context, domain.ProbeQueueReport) error {
			established <- time.Now()
			return nil
		}, func(context.Context, domain.ProbeActiveConfig) error { return nil }, nil)
	}()
	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case <-ctx.Done():
		t.Fatal("peer did not connect")
	}
	defer conn.CloseNow()
	hello := Hello{SessionIdentity: SessionIdentity{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID, StreamID: s.input.Connection.StreamID}, AgentVersion: "test", ProtocolMin: 1, ProtocolMax: 1, ConfigRevision: s.snapshot.Revision, FirstRetainedSeq: 1, LastCreatedSeq: 100, Capabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"}, ResourceBindings: []ResourceBinding{}}
	frame, err := encodeFrame("hello", 0, hello)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, welcome, err := DecodeWelcome(data)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-established:
		t.Fatal("handshake alone established contact")
	case err := <-done:
		t.Fatalf("session ended after handshake: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unhealthy, zero := false, int64(0)
	health := Health{Role: "probe", Ready: false, DBWritable: false, SchedulerHealthy: &unhealthy, ConfigRevision: welcome.DesiredConfigRevision, QueueBytes: &zero, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{"degraded"}}
	frame, err = encodeFrame("health", Decimal(s.input.Generation), health)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	select {
	case at = <-established:
	case err := <-done:
		t.Fatalf("degraded health ended session: %v", err)
	case <-ctx.Done():
		t.Fatal("degraded health did not establish contact")
	}
	select {
	case err := <-done:
		t.Fatalf("session ended before stability interval: %v", err)
	case <-time.After(1100 * time.Millisecond):
	}
	if time.Since(at) < time.Second {
		t.Fatal("session did not remain stable for one second")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("transport did not stop")
	}
}
