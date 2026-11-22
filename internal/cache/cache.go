// Package cache provides a small TTL cache with single-flight loading.
package cache

import (
	"sync"
	"time"
)

type entry struct {
	value   any
	expires time.Time
}

type call struct {
	done  chan struct{}
	value any
	err   error
}

// Cache is a concurrency-safe in-memory TTL cache. Concurrent GetOrLoad calls
// for the same key share a single load.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	calls   map[string]*call
	now     func() time.Time
}

func New() *Cache {
	return &Cache{
		entries: make(map[string]entry),
		calls:   make(map[string]*call),
		now:     time.Now,
	}
}

// Get returns a value if it is present and not expired.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !item.expires.After(c.now()) {
		delete(c.entries, key)
		return nil, false
	}
	return item.value, true
}

// Put stores a value with a TTL. A non-positive TTL stores nothing.
func (c *Cache) Put(key string, value any, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry{value: value, expires: c.now().Add(ttl)}
}

// GetOrLoad returns a cached value or calls load once for concurrent callers.
func (c *Cache) GetOrLoad(key string, ttl time.Duration, load func() (any, error)) (any, error) {
	c.mu.Lock()
	if item, ok := c.entries[key]; ok && item.expires.After(c.now()) {
		value := item.value
		c.mu.Unlock()
		return value, nil
	}
	if pending, ok := c.calls[key]; ok {
		c.mu.Unlock()
		<-pending.done
		return pending.value, pending.err
	}
	pending := &call{done: make(chan struct{})}
	c.calls[key] = pending
	c.mu.Unlock()

	value, err := load()

	c.mu.Lock()
	if err == nil {
		c.entries[key] = entry{value: value, expires: c.now().Add(ttl)}
	}
	pending.value = value
	pending.err = err
	delete(c.calls, key)
	close(pending.done)
	c.mu.Unlock()
	return value, err
}

// Len reports the number of unexpired entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	count := 0
	for key, item := range c.entries {
		if item.expires.After(now) {
			count++
		} else {
			delete(c.entries, key)
		}
	}
	return count
}
