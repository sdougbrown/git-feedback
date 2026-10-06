package github

import "sync"

// CacheEntry is one conditional-GET cache entry: the ETag to revalidate
// against, the complete body, and the pagination metadata (the raw Link
// header) that must travel with the body for a 304 to be usable.
type CacheEntry struct {
	ETag string
	Body []byte
	Link string
}

// HTTPCache stores conditional-GET representations keyed by full URL.
type HTTPCache interface {
	Get(key string) (CacheEntry, bool)
	Put(key string, entry CacheEntry)
}

// MemoryCache is the in-memory HTTPCache used before Stage 4 supplies a
// persistent store.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]CacheEntry
}

// NewMemoryCache returns an empty MemoryCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: map[string]CacheEntry{}}
}

// Get implements HTTPCache.
func (c *MemoryCache) Get(key string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

// Put implements HTTPCache.
func (c *MemoryCache) Put(key string, entry CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry
}
