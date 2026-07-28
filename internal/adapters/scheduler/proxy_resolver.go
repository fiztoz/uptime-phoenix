// Package scheduler provides adapters for the ports.Scheduler interface.
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// proxyCacheTTL bounds how long a resolved proxy is cached before the next
// lookup re-fetches it from the repository. This keeps proxy edits (e.g.
// rotating credentials, flipping active) visible within a bounded window
// without issuing a DB query on every single check tick for every monitor
// that has a proxy configured.
const proxyCacheTTL = 30 * time.Second

// proxyCacheEntry is a cached proxy lookup result with an expiry.
type proxyCacheEntry struct {
	proxy     *domain.Proxy
	expiresAt time.Time
}

// proxyResolver resolves a monitor's configured proxy into the "_proxy"
// config fragment injected into checker configs.
//
// ports.Checker.Check only receives a config map — never the monitor — so a
// checker cannot look up a proxy itself (see internal/adapters/checker/http.go).
// The scheduler is the only place that has both the monitor (with ProxyID)
// and a repository, so proxy resolution has to happen here, in
// checkConfigForMonitor's caller, not in the checker.
//
// Shared by LocalScheduler and ShardedScheduler. Safe for concurrent use
// since both schedulers run checks in per-monitor goroutines.
type proxyResolver struct {
	repo ports.ProxyRepository // nil-safe: proxy support is optional

	mu    sync.Mutex
	cache map[int64]proxyCacheEntry
}

// newProxyResolver creates a resolver. repo may be nil, in which case
// configFor always returns nil (proxy support disabled) until setRepo is
// called.
func newProxyResolver(repo ports.ProxyRepository) *proxyResolver {
	return &proxyResolver{repo: repo, cache: make(map[int64]proxyCacheEntry)}
}

// setRepo wires (or rewires) the proxy repository after construction —
// used by LocalScheduler.SetProxyRepo / ShardedScheduler.SetProxyRepo, which
// mirror the codebase's established optional-dependency pattern (e.g.
// HeartbeatService.SetTLSInfoRepo).
func (r *proxyResolver) setRepo(repo ports.ProxyRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repo = repo
}

// configFor returns the "_proxy" config fragment for m, or nil if m has no
// proxy configured, the resolved proxy is inactive, the lookup fails, or
// proxy support isn't wired up.
func (r *proxyResolver) configFor(ctx context.Context, m *domain.Monitor) map[string]any {
	if r == nil || m.ProxyID == nil {
		return nil
	}
	p := r.get(ctx, *m.ProxyID)
	if p == nil || !p.Active {
		return nil
	}
	return map[string]any{
		"protocol": p.Protocol,
		"host":     p.Host,
		"port":     p.Port,
		"auth":     p.Auth,
		"username": p.Username,
		"password": p.Password,
	}
}

// get returns the proxy for id, using the TTL cache when fresh and falling
// back to the repository (and refreshing the cache) otherwise. Returns nil
// if no repository is wired or the lookup fails.
func (r *proxyResolver) get(ctx context.Context, id int64) *domain.Proxy {
	r.mu.Lock()
	repo := r.repo
	if entry, ok := r.cache[id]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.proxy
	}
	r.mu.Unlock()

	if repo == nil {
		return nil
	}
	p, err := repo.GetByID(ctx, id)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	r.cache[id] = proxyCacheEntry{proxy: p, expiresAt: time.Now().Add(proxyCacheTTL)}
	r.mu.Unlock()
	return p
}
