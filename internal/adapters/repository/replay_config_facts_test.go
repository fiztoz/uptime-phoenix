package repository

import (
	"crypto/sha256"
	"strconv"
	"testing"
)

func replayCacheTestKey(revision int64) replayConfigKey {
	payload := []byte("ciphertext-" + strconv.FormatInt(revision, 10))
	return replayConfigKey{hubID: "hub", probeID: "probe", revision: revision, sha256: "digest", payload: sha256.Sum256(payload)}
}

// The cross-batch cache holds at most replayConfigCacheLimit snapshots and
// evicts the oldest insert first, so a long-running hub that keeps seeing new
// revisions cannot grow it without bound.
func TestReplayConfigCache_EvictsOldestBeyondLimit(t *testing.T) {
	var c replayConfigCache
	facts := make(map[int64]*replayConfigFacts)
	for revision := int64(1); revision <= replayConfigCacheLimit; revision++ {
		facts[revision] = &replayConfigFacts{}
		c.put(replayCacheTestKey(revision), facts[revision])
	}
	// Re-putting a cached key neither replaces it nor moves it to the back.
	c.put(replayCacheTestKey(1), &replayConfigFacts{})
	if got, ok := c.get(replayCacheTestKey(1)); !ok || got != facts[1] {
		t.Fatal("re-put replaced the cached facts for revision 1")
	}
	if len(c.entries) != replayConfigCacheLimit || len(c.order) != replayConfigCacheLimit {
		t.Fatalf("cache at limit holds %d entries / %d order, want %d", len(c.entries), len(c.order), replayConfigCacheLimit)
	}

	overflow := int64(replayConfigCacheLimit + 1)
	facts[overflow] = &replayConfigFacts{}
	c.put(replayCacheTestKey(overflow), facts[overflow])

	if len(c.entries) != replayConfigCacheLimit || len(c.order) != replayConfigCacheLimit {
		t.Fatalf("cache past limit holds %d entries / %d order, want %d", len(c.entries), len(c.order), replayConfigCacheLimit)
	}
	if _, ok := c.get(replayCacheTestKey(1)); ok {
		t.Fatal("oldest snapshot (revision 1) was not evicted when the cache passed its limit")
	}
	for _, revision := range []int64{2, replayConfigCacheLimit, overflow} {
		if got, ok := c.get(replayCacheTestKey(revision)); !ok || got != facts[revision] {
			t.Fatalf("revision %d missing or replaced after evicting the oldest entry", revision)
		}
	}

	// Further inserts keep evicting in insertion order.
	c.put(replayCacheTestKey(overflow+1), &replayConfigFacts{})
	if _, ok := c.get(replayCacheTestKey(2)); ok {
		t.Fatal("revision 2 survived the next eviction")
	}
	if _, ok := c.get(replayCacheTestKey(3)); !ok || len(c.entries) != replayConfigCacheLimit {
		t.Fatalf("eviction removed more than the oldest entry (%d entries)", len(c.entries))
	}
}
