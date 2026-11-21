package repository

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// replayConfigFacts is the part of a decoded probe configuration snapshot that
// replay and current-state authorization read: identities, generations and
// channel versions. It holds no secrets (no channel or monitor settings), so it
// may outlive the transaction that decoded it.
type replayConfigFacts struct {
	effectiveAt      time.Time
	watchdogEnabled  bool
	watchdogChannels map[int64]int64
	assignments      []replayAssignmentFact
	byMonitor        map[int64]int
	// missingMonitor records an assignment without a monitor. Replay skips
	// such entries; current-state authorization rejects the snapshot.
	missingMonitor bool
}

type replayAssignmentFact struct {
	monitorID  int64
	generation int64
	active     bool
	channels   map[int64]int64
}

// deriveReplayConfigFacts keeps exactly what replayFacts and
// currentSnapshotAssignments used to read from the decoded config.
func deriveReplayConfigFacts(resolved *domain.EdgeResolvedConfig) *replayConfigFacts {
	activeChannel := func(id int64) (int64, bool) {
		channel, ok := resolved.Channels[id]
		if !ok || channel.Notification == nil || !channel.Notification.Active {
			return 0, false
		}
		return channel.Version, true
	}
	f := &replayConfigFacts{
		effectiveAt:      resolved.Metadata.EffectiveAt.UTC(),
		watchdogEnabled:  resolved.Watchdog.Enabled,
		watchdogChannels: map[int64]int64{},
		byMonitor:        make(map[int64]int, len(resolved.Assignments)),
	}
	for _, id := range resolved.Watchdog.NotificationIDs {
		if version, ok := activeChannel(id); ok {
			f.watchdogChannels[id] = version
		}
	}
	for _, a := range resolved.Assignments {
		if a.Monitor == nil {
			f.missingMonitor = true
			continue
		}
		fact := replayAssignmentFact{monitorID: a.Monitor.ID, generation: a.Generation, active: a.Monitor.Active, channels: map[int64]int64{}}
		for _, link := range a.NotificationLinks {
			if version, ok := activeChannel(link.NotificationID); ok {
				fact.channels[link.NotificationID] = version
			}
		}
		if _, seen := f.byMonitor[fact.monitorID]; !seen {
			f.byMonitor[fact.monitorID] = len(f.assignments)
		}
		f.assignments = append(f.assignments, fact)
	}
	return f
}

// replayConfigKey identifies the exact stored ciphertext. Decryption with the
// hub's fixed key and decoding are deterministic, so equal keys give equal
// facts; a rewritten row has a different payload hash and misses.
type replayConfigKey struct {
	hubID    string
	probeID  string
	revision int64
	sha256   string
	payload  [sha256.Size]byte
}

func newReplayConfigKey(m *probeConfigModel) replayConfigKey {
	return replayConfigKey{hubID: m.HubID, probeID: m.ProbeID, revision: m.Revision, sha256: m.SHA256, payload: sha256.Sum256(m.ProtectedPayload)}
}

// replayConfigCacheLimit bounds the cache: one live revision per probe plus a
// few retained ones covers a fleet of dozens of probes.
const replayConfigCacheLimit = 256

// replayConfigCache memoizes facts across replay batches. Every batch used to
// decrypt and decode the probe's whole snapshot to read one assignment, so
// ingest cost grew with the number of monitors on the probe.
type replayConfigCache struct {
	mu      sync.Mutex
	entries map[replayConfigKey]*replayConfigFacts
	order   []replayConfigKey
}

func (c *replayConfigCache) get(key replayConfigKey) (*replayConfigFacts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.entries[key]
	return f, ok
}

func (c *replayConfigCache) put(key replayConfigKey, f *replayConfigFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[replayConfigKey]*replayConfigFacts)
	}
	if _, ok := c.entries[key]; ok {
		return
	}
	if len(c.order) >= replayConfigCacheLimit {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = f
	c.order = append(c.order, key)
}
