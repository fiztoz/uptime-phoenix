package services

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	insightsCacheTTL        = 20 * time.Second
	insightsCacheMaxEntries = 64
	insightsCacheMaxRows    = 50000
)

type insightsCacheKey struct {
	userID   int64
	period   InsightsPeriod
	typ      string
	groupID  int64
	hasGroup bool
	monitors [sha256.Size]byte
}

func newInsightsCacheKey(userID int64, period InsightsPeriod, typ string, groupID *int64, monitors []*domain.Monitor) insightsCacheKey {
	key := insightsCacheKey{userID: userID, period: period, typ: typ, hasGroup: groupID != nil}
	if groupID != nil {
		key.groupID = *groupID
	}
	// Canonicalize independently of repository display order. Hash only the
	// fields computeRow consumes; configuration and credentials never enter
	// the cache, and a monitor edit need not wait for the TTL to become visible.
	ordered := append([]*domain.Monitor(nil), monitors...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	hash := sha256.New()
	for _, m := range ordered {
		var group int64
		if m.GroupID != nil {
			group = *m.GroupID
		}
		_, _ = fmt.Fprintf(hash, "%d:%q:%q:%d:%t:%d;", m.ID, m.Name, m.Type, m.Interval, m.GroupID != nil, group)
	}
	copy(key.monitors[:], hash.Sum(nil))
	return key
}

type insightsCacheEntry struct {
	result  *InsightsResult
	expires time.Time
}

type insightsFlight struct {
	done   chan struct{}
	result *InsightsResult
	err    error
}

// Both maps are local to one service/API process. Entries contain immutable
// computed rows, never domain monitors, heartbeat history or authentication.
type insightsCache struct {
	mu      sync.Mutex
	entries map[insightsCacheKey]insightsCacheEntry
	flights map[insightsCacheKey]*insightsFlight
	rows    int
}

func (s *InsightsService) cachedInsights(ctx context.Context, key insightsCacheKey, calculate func(context.Context) (*InsightsResult, error)) (*InsightsResult, error) {
	c := &s.cache
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && s.now().Before(entry.expires) {
			c.mu.Unlock()
			s.recordCache("hit")
			return entry.result, nil
		}
		if flight, ok := c.flights[key]; ok {
			c.mu.Unlock()
			s.recordCache("shared")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
				// A departing leader must not fail an unrelated live request.
				// Retry through the map so surviving waiters elect only one new
				// leader. No detached work or background goroutines survive it.
				if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
					continue
				}
				return flight.result, flight.err
			}
		}
		flight := &insightsFlight{done: make(chan struct{})}
		if c.flights == nil {
			c.flights = make(map[insightsCacheKey]*insightsFlight)
		}
		c.flights[key] = flight
		c.mu.Unlock()
		s.recordCache("miss")

		flight.result, flight.err = calculate(ctx)
		if flight.err == nil {
			flight.err = ctx.Err()
		}
		c.mu.Lock()
		if flight.err == nil {
			c.put(key, flight.result, s.now())
		}
		delete(c.flights, key)
		close(flight.done)
		c.mu.Unlock()
		return flight.result, flight.err
	}
}

func (s *InsightsService) recordCache(outcome string) {
	if s.observer != nil {
		s.observer.IncInsightsCache(outcome)
	}
}

// put runs with mu held. Limit retained rows as well as keys so large installs
// do not retain 64 copies of an arbitrarily large result. TTL starts at the
// result's window end, rather than extending freshness by calculation latency.
func (c *insightsCache) put(key insightsCacheKey, result *InsightsResult, now time.Time) {
	for k, entry := range c.entries {
		if !now.Before(entry.expires) || k == key {
			c.rows -= len(entry.result.Rows)
			delete(c.entries, k)
		}
	}
	expires := result.To.Add(insightsCacheTTL)
	if len(result.Rows) > insightsCacheMaxRows || !now.Before(expires) {
		return
	}
	for len(c.entries) >= insightsCacheMaxEntries || c.rows+len(result.Rows) > insightsCacheMaxRows {
		var oldest insightsCacheKey
		var earliest time.Time
		for k, entry := range c.entries {
			if earliest.IsZero() || entry.expires.Before(earliest) {
				oldest, earliest = k, entry.expires
			}
		}
		c.rows -= len(c.entries[oldest].result.Rows)
		delete(c.entries, oldest)
	}
	if c.entries == nil {
		c.entries = make(map[insightsCacheKey]insightsCacheEntry)
	}
	c.entries[key] = insightsCacheEntry{result: result, expires: expires}
	c.rows += len(result.Rows)
}

func cloneInsightsResult(result *InsightsResult) *InsightsResult {
	out := *result
	out.Rows = make([]InsightsRow, len(result.Rows))
	for i, row := range result.Rows {
		if row.GroupID != nil {
			group := *row.GroupID
			row.GroupID = &group
		}
		if row.AvailabilityPercent != nil {
			availability := *row.AvailabilityPercent
			row.AvailabilityPercent = &availability
		}
		if row.LatencyAvgMs != nil {
			latency := *row.LatencyAvgMs
			row.LatencyAvgMs = &latency
		}
		out.Rows[i] = row
	}
	return &out
}
