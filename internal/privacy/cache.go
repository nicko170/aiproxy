package privacy

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// defaultCacheEntries is the fallback bound. Findings are almost always empty
// and a key is 64 hex characters, so tens of thousands of entries is a few
// megabytes — cheap next to what it saves.
const defaultCacheEntries = 50_000

// Salt combines the inputs that change what a scan MEANS into one string, mixed
// into every cache key.
//
// It stands in for expiry. Content -> findings is a pure function, so a cached
// entry never goes stale on its own; what makes it wrong is a change to the
// rules, the model, or the enabled label set. Putting those in the key means such
// a change invalidates everything automatically, and nothing has to be timed out
// "just in case".
func Salt(parts ...string) string { return strings.Join(parts, "\x00") }

// Cache remembers findings per scanned string, bounded by an LRU.
//
// It stores SHA256(salt || text) -> []Finding. Hashing the key is not an
// optimisation: using the text itself would hold every scanned string —
// credentials included — in a structure outliving the request. Findings are byte
// offsets and labels, from which no value can be reconstructed.
//
// Safe for concurrent use: one cache is shared by every in-flight request.
type Cache struct {
	mu      sync.Mutex
	salt    string
	max     int
	lru     *list.List               // front = most recently used
	entries map[string]*list.Element // key -> element holding *cacheEntry
}

type cacheEntry struct {
	key      string
	findings []Finding
}

func NewCache(maxEntries int, salt string) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultCacheEntries
	}
	return &Cache{
		salt:    salt,
		max:     maxEntries,
		lru:     list.New(),
		entries: make(map[string]*list.Element, maxEntries/8+1),
	}
}

func (c *Cache) key(text string) string {
	h := sha256.New()
	h.Write([]byte(c.salt))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the cached findings for text. A cached EMPTY result is a hit, not a
// miss — most strings contain nothing sensitive, so caching "clean" is where most
// of the saving comes from.
func (c *Cache) Get(text string) ([]Finding, bool) {
	k := c.key(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*cacheEntry).findings, true
}

// Put records findings for text, evicting the least recently used entry if the
// cache is full.
func (c *Cache) Put(text string, findings []Finding) {
	k := c.key(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value.(*cacheEntry).findings = findings
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&cacheEntry{key: k, findings: findings})
	c.entries[k] = el
	for c.lru.Len() > c.max {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// keysForTest exposes the stored keys so a test can assert no plaintext is
// retained.
func (c *Cache) keysForTest() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for k := range c.entries {
		out = append(out, k)
	}
	return out
}
