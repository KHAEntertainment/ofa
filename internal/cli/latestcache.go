package cli

import (
	"sync"
	"time"

	"ofa/internal/resolve"
	"ofa/internal/state"
)

// latestCache adapts the state file's `latest` map to resolve.Cache
// (issue #8). Reads and writes arrive from Resolve's workers, so it
// carries its own mutex; writes mark the cache dirty, and runPipeline
// persists a dirty cache back into the existing state file.
type latestCache struct {
	mu      sync.Mutex
	entries map[string]resolve.CacheEntry
	dirty   bool
}

// newLatestCache builds a cache from the loaded state file. A nil file
// (no scan yet) yields a read-through-only cache: entries are used for
// this run but never persisted — creating a state file with only a cache
// would corrupt drift detection.
func newLatestCache(f *stateFile) *latestCache {
	c := &latestCache{entries: map[string]resolve.CacheEntry{}}
	if f == nil {
		return c
	}
	for k, e := range f.Latest {
		t, err := time.Parse(time.RFC3339, e.ResolvedAt)
		if err != nil {
			// Corrupt timestamp: treat the entry as absent so it is
			// refetched rather than trusted forever.
			continue
		}
		c.entries[k] = resolve.CacheEntry{Version: e.Version, ResolvedAt: t}
	}
	return c
}

// Get implements resolve.Cache.
func (c *latestCache) Get(key string) (resolve.CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

// Put implements resolve.Cache.
func (c *latestCache) Put(key string, e resolve.CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = e
	c.dirty = true
}

// isDirty reports whether any entry changed since load.
func (c *latestCache) isDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// snapshot renders the cache back into state-file form (RFC3339
// timestamps, matching state.LatestEntry).
func (c *latestCache) snapshot() map[string]state.LatestEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]state.LatestEntry, len(c.entries))
	for k, e := range c.entries {
		out[k] = state.LatestEntry{
			Version:    e.Version,
			ResolvedAt: e.ResolvedAt.UTC().Format(time.RFC3339),
		}
	}
	return out
}
