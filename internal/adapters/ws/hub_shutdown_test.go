package ws

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
)

// syncBuffer is a goroutine-safe log sink: the hub logs from its own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// #165: closing the bus on shutdown closes every subscriber channel. The hub's
// fan-out loop must treat that as "stop", not read zero-value events from the
// closed channels in a hot loop and warn about each one (156 WARN lines in one
// second in the 0.6.2 soak).
func TestHubStopsQuietlyWhenBusCloses(t *testing.T) {
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bus := eventbus.NewMemoryBus()
	h := NewHub(bus, nil, nil, nil, nil, log)
	if !h.waitReady(5 * time.Second) {
		t.Fatal("hub never subscribed to the bus")
	}

	bus.Close()
	time.Sleep(200 * time.Millisecond)

	if n := strings.Count(logs.String(), "no resolvable monitor id"); n != 0 {
		t.Fatalf("hub logged %d 'no resolvable monitor id' warnings after the bus closed; want 0\nfirst lines:\n%s",
			n, firstLines(logs.String(), 3))
	}
	if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("hub logged warnings/errors on shutdown:\n%s", firstLines(logs.String(), 5))
	}
}

// The fan-out goroutine and the stats worker must both exit once the bus is
// closed, instead of spinning (listen) or leaking (statsWorker).
func TestHubGoroutinesExitWhenBusCloses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
	bus := eventbus.NewMemoryBus()
	h := NewHub(bus, nil, nil, nil, nil, log)
	if !h.waitReady(5 * time.Second) {
		t.Fatal("hub never subscribed to the bus")
	}

	bus.Close()

	for name, done := range map[string]<-chan struct{}{"listen": h.listenDone, "statsWorker": h.statsDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not exit after the bus closed", name)
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
