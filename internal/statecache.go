package internal

import (
	"sync"
	"time"
)

// StateCache is a small in-memory TTL cache used for "currently
// installed" state that's expensive to refetch on every page load —
// running container lists, orphan container lists, etc.
//
// It's deliberately scoped to the UI dispatcher: external API callers
// (the bearer-token /api/* paths) bypass this and hit the SSH path
// directly, so automation that depends on freshness keeps working.
//
// Cache keys are opaque strings the caller picks (e.g. "containers:demo");
// values are any (caller is responsible for type assertions).
type StateCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]stateEntry
}

type stateEntry struct {
	data any
	at   time.Time
}

func NewStateCache(ttl time.Duration) *StateCache {
	return &StateCache{ttl: ttl, m: map[string]stateEntry{}}
}

// Get returns the cached value, the time it was fetched, and whether
// the entry is still fresh. Stale entries are reported as missing.
func (c *StateCache) Get(key string) (any, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, time.Time{}, false
	}
	if time.Since(e.at) > c.ttl {
		delete(c.m, key)
		return nil, time.Time{}, false
	}
	return e.data, e.at, true
}

func (c *StateCache) Set(key string, v any) time.Time {
	now := time.Now()
	c.mu.Lock()
	c.m[key] = stateEntry{data: v, at: now}
	c.mu.Unlock()
	return now
}

// Invalidate drops a key — typically called after a task that mutates
// the state (e.g. container toggle) so the next read refetches.
func (c *StateCache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
