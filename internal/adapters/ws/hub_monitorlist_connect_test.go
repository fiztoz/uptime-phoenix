package ws

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// delayingListRepo blocks List until release is closed. Used to reproduce the
// production race: heartbeats fill the send buffer while the snapshot is still
// being built, and drop-on-full then discards monitor.list.
type delayingListRepo struct {
	*hubFakeMonitorRepo
	started chan struct{}
	release chan struct{}
	enter   sync.Once
}

func (r *delayingListRepo) List(ctx context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.enter.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.hubFakeMonitorRepo.List(ctx, filter)
}

// The dashboard's first paint is monitor.list. If that frame is drop-on-full'd
// while List is slow, the UI stays on skeleton cards ("No monitors") even though
// the socket later writes heartbeats into a broken pipe.
//
// Production sequence this guards (2026-08-20 v2.phoenix.pea.co.th):
//
//	AddClient → sendMonitorList (slow) → send buffer fills with heartbeats →
//	"dropped monitor.list" → writePump starts → broken pipe → disconnect.
func TestHandleWebSocket_MonitorListSurvivesHeartbeatFloodDuringSlowList(t *testing.T) {
	ctx := context.Background()

	inner := newHubFakeMonitorRepo()
	delayed := &delayingListRepo{
		hubFakeMonitorRepo: inner,
		started:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	heartbeats := newHubFakeHeartbeatRepo()

	mon := &domain.Monitor{UserID: 1, Name: "web", Type: "http", Active: true, Interval: 60}
	if err := inner.Create(ctx, mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if err := heartbeats.Save(ctx, &domain.Heartbeat{
		MonitorID: mon.ID, Status: domain.StatusUp, Time: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	users := memory.NewUserRepo()
	admin := &domain.User{Username: "admin", Active: true, IsAdmin: true}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	access := services.NewAccessService(users, memory.NewUserPermissionRepo(), nil, inner)

	bus := eventbus.NewMemoryBus()
	t.Cleanup(bus.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHub(bus, delayed, heartbeats, access, nil, log)
	if !h.waitReady(2 * time.Second) {
		t.Fatal("hub never subscribed to the event bus")
	}

	authSvc := services.NewAuthService(nil, nil, closeAuthn{userID: admin.ID}, nil)
	srv := serveHandleWebSocket(t, h, authSvc, "good-token")
	t.Cleanup(srv.Close)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dialCancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	gotList := make(chan struct{})
	var sawOther atomic.Bool
	go func() {
		for {
			_, data, readErr := conn.Read(dialCtx)
			if readErr != nil {
				return
			}
			if containsType(data, EventMonitorList) {
				select {
				case <-gotList:
				default:
					close(gotList)
				}
				return
			}
			sawOther.Store(true)
		}
	}()

	select {
	case <-delayed.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sendMonitorList never reached List")
	}

	for i := 0; i < 2000; i++ {
		if err := bus.Publish(ctx, ports.Event{
			Type: EventHeartbeat,
			Payload: &domain.Heartbeat{
				MonitorID: mon.ID, Status: domain.StatusUp, Time: time.Now().UTC(),
			},
		}); err != nil {
			t.Fatalf("publish heartbeat: %v", err)
		}
	}

	// Pumps must already be running *during* the slow List — that is what keeps
	// the proxy from idle-timing out and what drains heartbeats so monitor.list
	// has a buffer slot. Assert it before releasing List.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sawOther.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if !sawOther.Load() {
		t.Fatal("write pump sent nothing while List was blocked; pumps are still starting after sendMonitorList")
	}

	close(delayed.release)

	select {
	case <-gotList:
	case <-time.After(5 * time.Second):
		t.Fatal("client never received monitor.list after a heartbeat flood during a slow List; " +
			"the dashboard would stay on skeleton cards")
	}
}

// monitor.list must wait for a send-buffer slot rather than drop. Drop-on-full
// is correct for heartbeats; it is not correct for the snapshot the dashboard
// is blocked on.
func TestSendMonitorViews_BlocksUntilBufferHasRoom(t *testing.T) {
	h := newAuthCloseHub(t)
	client := NewClient("full-buffer", 1)
	for i := 0; i < clientSendBufSize; i++ {
		client.send <- []byte("x")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		h.sendMonitorViews(ctx, client, []MonitorView{{
			ID: 1, Name: "web", Type: "http", Status: "up",
			Config: map[string]any{}, Tags: []MonitorTagView{},
		}})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("sendMonitorViews returned while the buffer was full; it dropped monitor.list")
	case <-time.After(50 * time.Millisecond):
	}

	<-client.send // free one slot
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("sendMonitorViews did not complete after a buffer slot opened")
	}
}
