package resolve

import (
	"time"
)

// Cache stores resolved latest versions so repeated runs don't re-query
// package registries. Keys come from CacheKey. Implementations must be
// safe for concurrent use: Resolve reads and writes from its workers.
type Cache interface {
	Get(key string) (CacheEntry, bool)
	Put(key string, e CacheEntry)
}

// CacheEntry is one cached latest-version answer.
type CacheEntry struct {
	Version    string
	ResolvedAt time.Time
}

// CacheKey builds the cache key for a (tier, package) pair. The tier
// prefix keeps identically-named packages in different ecosystems apart
// (npm and PyPI names can collide).
func CacheKey(tier, pkg string) string { return tier + ":" + pkg }

// DefaultTTL is how long a cached latest version is served without a
// refresh (issue #8).
const DefaultTTL = 12 * time.Hour

// defaultConcurrency caps parallel registry lookups. Eight keeps every
// npm/PyPI request in flight on a typical machine without turning a
// flaky network into a burst.
const defaultConcurrency = 8

// Config tunes a Resolve run. The zero value is valid: uncached,
// clock = time.Now, fresh lookups allowed, default concurrency, and brew
// queried inline when brew-tier installs exist.
type Config struct {
	// Cache reads and records latest versions. nil disables caching.
	Cache Cache
	// Now supplies the clock (tests pin it). nil means time.Now.
	Now func() time.Time
	// TTL is how long a cache entry stays fresh. 0 means DefaultTTL.
	TTL time.Duration
	// Refresh skips fresh cache entries (fresh answers are still written
	// back, and a failed refresh still falls back to a stale entry).
	Refresh bool
	// Concurrency caps parallel npm/PyPI lookups. <=0 means
	// defaultConcurrency.
	Concurrency int
	// Prefetch, when set, is a `brew outdated` already in flight —
	// started before detection so the slowest single query in the
	// pipeline overlaps the scan. Resolve consumes it only when
	// brew-tier installs exist; a future nobody consumes completes into
	// its own buffer and is garbage-collected.
	Prefetch *BrewFuture
}

// withDefaults fills zero fields. Callers must not use Config before it.
func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	return c
}

// brewOutcome is a completed `brew outdated` query.
type brewOutcome struct {
	res *brewResolver
	err error
}

// BrewFuture is an in-flight `brew outdated` query. Create one with
// PrefetchBrew before detection runs so the query overlaps the scan, and
// pass it via Config.Prefetch; Resolve consumes it when brew-tier
// installs exist. The channel is buffered, so an unconsumed future
// finishes without blocking.
type BrewFuture struct {
	ch chan brewOutcome
}

// PrefetchBrew starts `brew outdated` in the background. The runner is
// the same one Resolve would use inline.
func PrefetchBrew(run Runner) *BrewFuture {
	f := &BrewFuture{ch: make(chan brewOutcome, 1)}
	go func() {
		r, err := newBrewResolver(run)
		f.ch <- brewOutcome{res: r, err: err}
	}()
	return f
}

// wait blocks for the query. Resolve calls it at most once, and only
// when brew-tier installs exist — at most one consumer per future. The
// channel is drained by that read: sharing one Config.Prefetch across
// two Resolve calls would deadlock the second waiter, which is why the
// CLI builds a fresh future per invocation and never reuses it.
func (f *BrewFuture) wait() (*brewResolver, error) {
	o := <-f.ch
	return o.res, o.err
}

// fetchLatest resolves one package's latest version through cfg's cache:
//
//   - a fresh entry short-circuits the network (unless Refresh),
//   - a successful fetch is written back for the next run,
//   - a failed fetch falls back to a stale entry (stale=true, carrying
//     the entry's timestamp and the refresh error) instead of failing
//     the resolution outright,
//   - a failed fetch with no cache returns the error, exactly as an
//     uncached run would.
//
// A resolved_at in the future (wall clock stepped back between runs)
// counts as fresh until the clock passes it plus the TTL; --refresh is
// the deliberate escape hatch rather than negative-age clamping.
func fetchLatest(cfg Config, tier, pkg string, fetch func() (string, error)) (latest string, stale bool, cachedAt time.Time, err error) {
	key := CacheKey(tier, pkg)
	now := cfg.Now()
	if cfg.Cache != nil && !cfg.Refresh {
		if e, ok := cfg.Cache.Get(key); ok && now.Sub(e.ResolvedAt) <= cfg.TTL {
			return e.Version, false, time.Time{}, nil
		}
	}
	v, ferr := fetch()
	if ferr == nil {
		if cfg.Cache != nil {
			cfg.Cache.Put(key, CacheEntry{Version: v, ResolvedAt: now})
		}
		return v, false, time.Time{}, nil
	}
	if cfg.Cache != nil {
		if e, ok := cfg.Cache.Get(key); ok {
			return e.Version, true, e.ResolvedAt, ferr
		}
	}
	return "", false, time.Time{}, ferr
}
