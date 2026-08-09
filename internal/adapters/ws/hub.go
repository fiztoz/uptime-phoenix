// Package ws provides the WebSocket hub adapter.
package ws

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// jwtCtxKey is the typed context key carrying the JWT passed via the
// /ws?token=... query parameter. A typed key avoids collisions with other
// packages that might use a plain string key for the same value.
type jwtCtxKey struct{}

// WithJWT returns a context carrying the JWT extracted from the WS upgrade
// query string, so HandleWebSocket can read it without a shared global.
func WithJWT(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, jwtCtxKey{}, token)
}

// JWTFromContext returns the JWT placed on the context by WithJWT, if any.
func JWTFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(jwtCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// visibilityTTL is how long a client's resolved monitor set is cached before it
// is recomputed from the AccessService.
//
// The set is ALSO invalidated eagerly on every monitor.update / monitor.delete
// (see listen), because those are the events that can change which monitors a
// user can see — a monitor moved into or out of a granted group, or deleted
// outright. The TTL is the backstop for the one thing the hub cannot observe: an
// admin editing a user's grants over the REST API, which publishes nothing on the
// bus. Worst case, a revoked user keeps receiving events for at most this long.
// Shorten it if that window ever matters more than the extra queries.
const visibilityTTL = 30 * time.Second

// statsDebounce is the trailing-edge window over which stats.update recomputes
// are coalesced.
//
// stats.update drives a badge counter. It does not need to fire once per event,
// and firing once per event is what made the hub quadratic: N monitors coming up
// for the first time publish N status.change events (heartbeat_service publishes
// on the FIRST check of every monitor, not only on later transitions), and each
// one used to trigger a full recompute over every active monitor. Collapsing a
// storm into one trailing recompute turns O(N) recomputes into O(1) per burst.
//
// The cost is that the badge can lag a change by up to this long. 250 ms is
// below the threshold at which a counter feels stale, and far below the seconds
// of delay the un-debounced version actually produced under load.
const statsDebounce = 250 * time.Millisecond

// dropMetrics counts frames the hub could not deliver.
//
// It is a narrow local interface rather than ports.MetricsExporter so tests and
// alternate wirings are not forced to implement the whole exporter surface; the
// Prometheus adapter satisfies it structurally. A nil metrics field is valid and
// simply records nothing.
type dropMetrics interface {
	IncWSFrameDropped()
}

// Hub manages WebSocket client connections and fans out events from the EventBus.
//
// RBAC: the hub does NOT broadcast indiscriminately. Every outbound frame is
// filtered against the receiving client's visible-monitor set (resolved from
// services.AccessService), and stats.update is computed PER CLIENT from that set
// rather than from a global ListActive. Before this, every connected client
// received every heartbeat, every status change and an install-wide monitor count
// — a cross-tenant leak that no amount of REST-layer scoping could contain.
type Hub struct {
	mu            sync.RWMutex
	clients       map[*Client]bool
	bus           ports.EventBus
	monitorRepo   ports.MonitorRepository
	heartbeatRepo ports.HeartbeatRepository
	// access resolves each client's visible monitors. When nil the hub fails
	// CLOSED: no client sees anything. That is deliberate — a hub wired without
	// access control must go quiet, not go public.
	access *services.AccessService
	// batch, when non-nil, resolves many monitors' latest heartbeats in one round
	// trip. Set from heartbeatRepo when it implements ports.HeartbeatBatchReader
	// (both real adapters do). When nil the hub falls back to per-monitor
	// GetLatest — correct, but the N+1 that R3.6 was about.
	batch ports.HeartbeatBatchReader
	// tags enriches the monitor payloads with their tags. Optional: when nil,
	// monitors go out with an empty tags array.
	tags    *services.TagService
	log     *slog.Logger
	metrics dropMetrics
	// ready is closed once listen() has finished subscribing to the bus. NewHub
	// starts listen() in a goroutine and returns immediately, so a caller that
	// publishes straight away can otherwise race the subscription and have its
	// event silently dropped. Tests wait on it; production code never needs to.
	ready chan struct{}
	// statsTrigger is the coalescing channel between the fan-out goroutine and the
	// stats worker. Capacity 1: a pending request already covers any number of
	// further ones, so a non-blocking send is enough and the fan-out path never
	// waits on the recompute. This is the decoupling that keeps heartbeat delivery
	// off the database's critical path.
	statsTrigger chan struct{}
	// statsDebounce is settable so tests do not have to sleep out the production
	// window. Zero means "use statsDebounce".
	statsWindow time.Duration
}

// Client represents a connected WebSocket client.
//
// The visible-monitor set is cached on the client rather than resolved per event:
// a busy install emits many heartbeats a second, and re-deriving the grant
// expansion for each one, for each client, would hammer the DB. See visibilityTTL
// for how staleness is bounded.
type Client struct {
	ID     string
	UserID int64
	send   chan []byte
	// closed is set before send is closed so fan-out can skip without panicking
	// on "send on closed channel" when snapshotClients races with disconnect.
	closed atomic.Bool

	visMu      sync.Mutex
	visAll     bool           // true for admins: every monitor, no id set needed
	visIDs     map[int64]bool // the allowlist when visAll is false
	visResolve time.Time      // when the set was last resolved; zero = never/invalidated
}

// NewHub creates a new WebSocket hub that listens on the EventBus.
//
// access is required for the hub to emit anything at all (see Hub.access); tags
// may be nil.
func NewHub(
	bus ports.EventBus,
	monitorRepo ports.MonitorRepository,
	heartbeatRepo ports.HeartbeatRepository,
	access *services.AccessService,
	tags *services.TagService,
	log *slog.Logger,
) *Hub {
	h := &Hub{
		clients:       make(map[*Client]bool),
		bus:           bus,
		monitorRepo:   monitorRepo,
		heartbeatRepo: heartbeatRepo,
		access:        access,
		tags:          tags,
		log:           log,
		ready:         make(chan struct{}),
		statsTrigger:  make(chan struct{}, 1),
	}
	// Prefer the batched lookup when the repository offers it. Both production
	// adapters do (they carry compile-time assertions); the fallback exists for
	// hand-written test fakes that implement only ports.HeartbeatRepository.
	if b, ok := heartbeatRepo.(ports.HeartbeatBatchReader); ok {
		h.batch = b
	}
	go h.listen()
	go h.statsWorker()
	return h
}

// SetDropMetrics attaches a counter for frames dropped because a client's send
// buffer was full. Optional; nil-safe. Without it the drop is invisible, which
// is precisely the R3.6 complaint: a stalled hub lost UI events with no log line
// and no metric.
func (h *Hub) SetDropMetrics(m dropMetrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metrics = m
}

// waitReady blocks until the hub has subscribed to the bus, or the timeout
// elapses (reporting false). Used by tests that publish immediately after NewHub.
func (h *Hub) waitReady(timeout time.Duration) bool {
	select {
	case <-h.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

// listen subscribes to all relevant event types and fans out to connected clients.
//
// This loop is the LATENCY-CRITICAL path: every branch must stay O(clients) and
// must not touch the database more than a constant number of times. It used to
// call emitStatsUpdate inline, which was O(active monitors) DATABASE ROUND TRIPS
// per status.change — and heartbeat_service publishes status.change on the first
// check of every monitor, so a cold start of N monitors performed ~N² serialized
// queries here while heartbeats queued behind them and were dropped at the bus
// buffer. That is R3.6. Stats recomputes now go to statsWorker instead.
//
// The four event types stay in ONE goroutine on purpose: heartbeat and
// status.change for the same monitor must not be reordered relative to each
// other, or the dashboard can render a stale status after a fresh heartbeat.
// Serializing fan-out is cheap; it was never the bottleneck.
func (h *Hub) listen() {
	heartbeatCh := h.bus.Subscribe(EventHeartbeat)
	statusCh := h.bus.Subscribe(EventStatusChange)
	monitorCh := h.bus.Subscribe(EventMonitorUpdate)
	monitorDelCh := h.bus.Subscribe(EventMonitorDelete)
	close(h.ready)

	for {
		select {
		case ev := <-heartbeatCh:
			h.broadcast(ev)
		case ev := <-statusCh:
			h.broadcast(ev)
			// Badge counters are eventually consistent by design — see statsDebounce.
			h.requestStatsUpdate()
		case ev := <-monitorCh:
			// A monitor's group can change here, which changes who can see it, so
			// every client's cached visibility is stale as of now.
			h.invalidateVisibility()
			// Resolve the actual status from the latest heartbeat so the
			// frontend doesn't get stuck on "pending" after creation.
			h.broadcastMonitorUpdate(ev)
			h.requestStatsUpdate()
		case ev := <-monitorDelCh:
			// The deleted monitor must drop out of every cached set, or a client
			// keeps counting a monitor that no longer exists.
			h.invalidateVisibility()
			h.broadcast(ev)
			h.requestStatsUpdate()
		}
	}
}

// requestStatsUpdate asks for a stats recompute without waiting for one.
//
// The trigger channel has capacity 1 and the send is non-blocking: if a request
// is already pending it subsumes this one, so a burst of a thousand status
// changes costs a thousand cheap channel probes rather than a thousand full
// recomputes. Critically, this NEVER blocks the fan-out loop.
func (h *Hub) requestStatsUpdate() {
	select {
	case h.statsTrigger <- struct{}{}:
	default:
	}
}

// statsWorker performs the expensive recompute off the fan-out path, collapsing
// bursts with a trailing-edge debounce.
//
// After a trigger it waits one debounce window and drains anything that arrived
// meanwhile, so a storm produces one recompute at the END of the storm — which
// is also the only recompute whose numbers are still correct by the time they
// reach the client.
func (h *Hub) statsWorker() {
	for range h.statsTrigger {
		window := h.statsWindow
		if window <= 0 {
			window = statsDebounce
		}
		timer := time.NewTimer(window)
		<-timer.C
		timer.Stop()

		// Collapse everything that piled up during the window.
		select {
		case <-h.statsTrigger:
		default:
		}

		h.emitStatsUpdate()
	}
}

// broadcastMonitorUpdate resolves the real status for a monitor.update event
// before broadcasting, so the frontend always shows the correct status.
func (h *Hub) broadcastMonitorUpdate(ev ports.Event) {
	var monitorID int64
	var view MonitorView

	switch m := ev.Payload.(type) {
	case *domain.Monitor:
		monitorID = m.ID
		view = toMonitorView(m, "pending")
	case MonitorView:
		monitorID = m.ID
		view = m
	case map[string]any:
		// Redis deserialization produces a generic map.
		monitorID = extractInt64(m, "id")
		view = monitorMapToView(m, "pending")
	default:
		h.broadcast(ev)
		return
	}

	// Resolve the actual status from the latest heartbeat (unless paused).
	if !view.Active {
		view.Status = "paused"
	} else if h.heartbeatRepo != nil && monitorID > 0 {
		if hb, err := h.heartbeatRepo.GetLatest(context.Background(), monitorID); err == nil && hb != nil {
			view.Status = statusToWire(hb.Status)
		}
	}
	view.Tags = h.tagsFor(context.Background(), monitorID)

	h.broadcast(ports.Event{Type: ev.Type, Payload: view})
}

// emitStatsUpdate computes the monitor counts PER CLIENT, from that client's own
// visible set, and sends each client its own stats.update.
//
// It used to count ListActive() across the entire install and broadcast one
// number to everyone, which told every user exactly how many monitors existed and
// how many were down — including the ones they had no access to.
func (h *Hub) emitStatsUpdate() {
	if h.monitorRepo == nil {
		return
	}
	ctx := context.Background()
	monitors, err := h.monitorRepo.ListActive(ctx)
	if err != nil {
		return
	}

	// Resolve each active monitor's status once, not once per client.
	statuses := h.latestStatuses(ctx, monitors)

	for _, client := range h.snapshotClients() {
		all, visible := h.visibilityFor(ctx, client)
		total, up, down, pending := 0, 0, 0, 0
		for _, m := range monitors {
			if !all && !visible[m.ID] {
				continue
			}
			total++
			switch statuses[m.ID] {
			case domain.StatusUp:
				up++
			case domain.StatusDown:
				down++
			default:
				pending++
			}
		}
		data, mErr := marshalWireEvent(ports.Event{
			Type: EventStatsUpdate,
			Payload: map[string]any{
				"total":   total,
				"up":      up,
				"down":    down,
				"pending": pending,
			},
		})
		if mErr != nil {
			h.log.Error("ws hub: failed to marshal stats.update", "error", mErr)
			return
		}
		h.send(client, data)
	}
}

// latestStatuses resolves the current status of every given monitor.
//
// It takes the batched path when the repository supports it: ONE query (per 500
// monitors) instead of one per monitor. The previous per-monitor loop was the
// N+1 half of R3.6 — at 1,000 monitors it issued 1,000 serialized round trips
// every time a monitor changed state, and status.change fires on every monitor's
// first check.
//
// Monitors with no heartbeat yet, and every monitor at all if the lookup fails,
// report PENDING — the same answer the per-monitor version gave when GetLatest
// returned an error or no row. Stats are a badge counter: degrading to PENDING
// is correct-ish and silent, whereas failing the whole fan-out would not be.
func (h *Hub) latestStatuses(ctx context.Context, monitors []*domain.Monitor) map[int64]domain.Status {
	statuses := make(map[int64]domain.Status, len(monitors))
	for _, m := range monitors {
		statuses[m.ID] = domain.StatusPending
	}
	if h.heartbeatRepo == nil || len(monitors) == 0 {
		return statuses
	}

	if h.batch != nil {
		ids := make([]int64, 0, len(monitors))
		for _, m := range monitors {
			ids = append(ids, m.ID)
		}
		latest, err := h.batch.GetLatestForMonitors(ctx, ids)
		if err != nil {
			h.log.Warn("ws hub: batched heartbeat lookup failed, reporting pending", "error", err)
			return statuses
		}
		for id, hb := range latest {
			if hb != nil {
				statuses[id] = hb.Status
			}
		}
		return statuses
	}

	// Fallback for repositories that implement only ports.HeartbeatRepository.
	for _, m := range monitors {
		if hb, hbErr := h.heartbeatRepo.GetLatest(ctx, m.ID); hbErr == nil && hb != nil {
			statuses[m.ID] = hb.Status
		}
	}
	return statuses
}

// broadcast serializes an event and sends it ONLY to the clients allowed to see
// the monitor it concerns.
//
// An event whose monitor cannot be determined is DROPPED, not fanned out: the
// four event types the hub subscribes to all carry a monitor id, so an
// unresolvable one means the payload shape changed, and the safe reading of "I
// don't know who this belongs to" is "nobody".
func (h *Hub) broadcast(event ports.Event) {
	monitorID, ok := monitorIDForEvent(event)
	if !ok {
		h.log.Warn("ws hub: dropping event with no resolvable monitor id", "type", event.Type)
		return
	}

	data, err := marshalWireEvent(event)
	if err != nil {
		h.log.Error("ws hub: failed to marshal event", "error", err)
		return
	}

	ctx := context.Background()
	for _, client := range h.snapshotClients() {
		all, visible := h.visibilityFor(ctx, client)
		if !all && !visible[monitorID] {
			continue
		}
		h.send(client, data)
	}
}

// send delivers one frame to one client, dropping it if the client's buffer is
// full (a slow consumer must not stall the hub).
//
// The drop itself is unchanged and still correct — one slow browser tab must not
// stall fan-out for everyone. What changed is that it is now COUNTED. Before,
// a backlogged hub silently discarded UI events with no log line and no metric,
// so "delivered events/sec" in the load harness overstated what clients actually
// received and there was no way to tell a quiet system from a lossy one.
//
// Disconnect race: readPump closes client.send when the socket ends, but
// broadcast may still hold a snapshot that includes that client until
// RemoveClient runs. closed + recover make that race a silent drop instead of
// panic: send on closed channel (which used to kill the whole process in e2e).
func (h *Hub) send(client *Client, data []byte) {
	if client == nil || client.closed.Load() {
		return
	}
	defer func() {
		// Last-line defense if closed flipped between the check and the send.
		_ = recover()
	}()
	select {
	case client.send <- data:
	default:
		h.mu.RLock()
		m := h.metrics
		h.mu.RUnlock()
		if m != nil {
			m.IncWSFrameDropped()
		}
	}
}

// snapshotClients copies the client set so fan-out can run without holding the
// hub lock (visibility resolution can hit the database).
func (h *Hub) snapshotClients() []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Client, 0, len(h.clients))
	for client := range h.clients {
		out = append(out, client)
	}
	return out
}

// monitorIDForEvent extracts the monitor an event concerns. The second return is
// false when the payload carries no usable id — see broadcast for why that is a
// drop rather than a broadcast.
func monitorIDForEvent(event ports.Event) (int64, bool) {
	switch p := event.Payload.(type) {
	case *domain.Heartbeat:
		return p.MonitorID, p.MonitorID > 0
	case *domain.Monitor:
		return p.ID, p.ID > 0
	case MonitorView:
		return p.ID, p.ID > 0
	case int64: // monitor.delete publishes the bare id
		return p, p > 0
	case int:
		return int64(p), p > 0
	case float64: // …and Redis JSON turns that id into a float
		return int64(p), p > 0
	case map[string]any:
		// heartbeat / status.change maps, and Redis-decoded monitors.
		for _, key := range []string{"monitor_id", "MonitorID", "id", "ID"} {
			if id := extractInt64(p, key); id > 0 {
				return id, true
			}
		}
	}
	return 0, false
}

// visibilityFor returns the client's cached visible-monitor set, re-resolving it
// when it has expired or been invalidated.
//
// Fails CLOSED on every error path: no access service, an unauthenticated client
// (UserID 0), or a lookup failure all yield "sees nothing". An anonymous WS
// connection used to receive the entire install's heartbeat stream.
func (h *Hub) visibilityFor(ctx context.Context, client *Client) (bool, map[int64]bool) {
	client.visMu.Lock()
	defer client.visMu.Unlock()

	if !client.visResolve.IsZero() && time.Since(client.visResolve) < visibilityTTL {
		return client.visAll, client.visIDs
	}

	client.visAll = false
	client.visIDs = map[int64]bool{}
	client.visResolve = time.Now()

	if h.access == nil || client.UserID == 0 {
		return false, client.visIDs
	}

	all, ids, err := h.access.VisibleMonitorIDs(ctx, client.UserID)
	if err != nil {
		h.log.Warn("ws hub: failed to resolve visible monitors, denying all",
			"client_id", client.ID, "user_id", client.UserID, "error", err)
		return false, client.visIDs
	}
	client.visAll = all
	for _, id := range ids {
		client.visIDs[id] = true
	}
	return client.visAll, client.visIDs
}

// invalidateVisibility marks every client's cached set stale, so the next event
// re-resolves it. Called when a monitor is created, updated (its group may have
// changed) or deleted.
func (h *Hub) invalidateVisibility() {
	for _, client := range h.snapshotClients() {
		client.visMu.Lock()
		client.visResolve = time.Time{}
		client.visMu.Unlock()
	}
}

// tagsFor loads a monitor's tags for the wire payload. Returns an empty slice on
// any failure — tags are display metadata and must never fail an event.
func (h *Hub) tagsFor(ctx context.Context, monitorID int64) []MonitorTagView {
	if h.tags == nil || monitorID <= 0 {
		return []MonitorTagView{}
	}
	details, err := h.tags.TagsForMonitor(ctx, monitorID)
	if err != nil {
		return []MonitorTagView{}
	}
	return toWireTagViews(details)
}

// AddClient registers a new WebSocket client.
func (h *Hub) AddClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[client] = true
}

// RemoveClient unregisters a WebSocket client.
func (h *Hub) RemoveClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, client)
}

// ActiveConnections returns the current number of connected clients.
func (h *Hub) ActiveConnections() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// NewClient builds a client with an empty send buffer of the standard size. It
// exists so tests can construct a client the same way HandleWebSocket does.
func NewClient(id string, userID int64) *Client {
	return &Client{
		ID:     id,
		UserID: userID,
		send:   make(chan []byte, 256),
	}
}

// Outbound exposes the client's send channel for tests to drain.
func (c *Client) Outbound() <-chan []byte { return c.send }

// HandleWebSocket accepts a WebSocket connection, authenticates it via query param
// or first message JWT, creates a Client, and runs read/write pumps until
// the connection closes.
func (h *Hub) HandleWebSocket(ctx context.Context, conn *websocket.Conn, authSvc *services.AuthService) {
	// Extract JWT from query parameter (browsers can't set WS headers).
	// Fall back to reading first message if no query token.
	jwt := JWTFromContext(ctx)

	var userID int64
	if jwt != "" {
		var err error
		userID, err = authSvc.VerifyToken(ctx, jwt)
		if err != nil {
			h.log.Warn("ws: invalid JWT in connection", "error", err)
			_ = conn.Close(websocket.StatusPolicyViolation, "unauthorized")
			return
		}
	}

	client := NewClient(fmt.Sprintf("ws-%d-%d", userID, time.Now().UnixNano()), userID)

	h.AddClient(client)
	h.log.Info("ws client connected", "client_id", client.ID, "user_id", userID)

	// Push the monitor list this client is allowed to see, so the dashboard
	// rehydrates on connect.
	h.sendMonitorList(client)

	// Run read and write pumps concurrently. When either exits, close the
	// connection and clean up.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		h.writePump(ctx, conn, client)
	}()

	go func() {
		defer wg.Done()
		h.readPump(ctx, conn, client)
	}()

	wg.Wait()

	h.RemoveClient(client)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	h.log.Info("ws client disconnected", "client_id", client.ID, "user_id", userID)
}

// writePump writes messages from the client's send channel to the WebSocket connection.
func (h *Hub) writePump(ctx context.Context, conn *websocket.Conn, client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-client.send:
			if !ok {
				// Channel closed.
				return
			}
			err := conn.Write(ctx, websocket.MessageText, msg)
			if err != nil {
				h.log.Error("ws: write error", "client_id", client.ID, "error", err)
				return
			}
		case <-ticker.C:
			// Send a ping to keep the connection alive.
			err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`))
			if err != nil {
				return
			}
		}
	}
}

// readPump reads messages from the WebSocket connection (e.g., client pong responses).
func (h *Hub) readPump(ctx context.Context, conn *websocket.Conn, client *Client) {
	defer func() {
		// Mark closed before close(send) so concurrent send() skips cleanly.
		client.closed.Store(true)
		close(client.send)
	}()

	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			// Connection closed or read error — exit.
			return
		}
		// We don't process inbound messages yet, but we consume them
		// to keep the connection alive.
	}
}
