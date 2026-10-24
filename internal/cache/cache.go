// Package cache stores completed responses keyed by a hash of the request so
// an identical follow-up request never spends free-tier quota again.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Entry is one cached response.
type Entry struct {
	ContentType string
	Body        []byte
	Stream      bool
	Status      int
	Created     time.Time
	expires     time.Time
}

// Cache is a bounded, TTL-scoped response cache with LRU eviction.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	max     int
	ttl     time.Duration
	hits    int64
	misses  int64
	evicted int64
}

type item struct {
	key   string
	entry *Entry
}

// New creates a cache. A non-positive max disables the cache.
func New(max int, ttl time.Duration) *Cache {
	return &Cache{
		entries: map[string]*list.Element{},
		order:   list.New(),
		max:     max,
		ttl:     ttl,
	}
}

// Key hashes a canonical representation of the request.
func Key(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte("unhashable")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Get returns a live entry and marks it as recently used.
func (c *Cache) Get(key string) (*Entry, bool) {
	if c == nil || c.max <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	entry := element.Value.(*item).entry
	if time.Now().After(entry.expires) {
		c.order.Remove(element)
		delete(c.entries, key)
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(element)
	c.hits++
	return entry, true
}

// Set stores an entry, evicting the least recently used one when full.
func (c *Cache) Set(key string, entry *Entry) {
	if c == nil || c.max <= 0 || entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.Created = time.Now()
	entry.expires = entry.Created.Add(c.ttl)
	if element, ok := c.entries[key]; ok {
		element.Value.(*item).entry = entry
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(&item{key: key, entry: entry})
	for c.order.Len() > c.max {
		last := c.order.Back()
		if last == nil {
			break
		}
		c.order.Remove(last)
		delete(c.entries, last.Value.(*item).key)
		c.evicted++
	}
}

// Stats reports counters for the health endpoint.
func (c *Cache) Stats() map[string]any {
	if c == nil {
		return map[string]any{"enabled": false}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"enabled": c.max > 0,
		"entries": len(c.entries),
		"max":     c.max,
		"ttl":     c.ttl.String(),
		"hits":    c.hits,
		"misses":  c.misses,
		"evicted": c.evicted,
	}
}
