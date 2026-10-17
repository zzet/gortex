package indexer

import (
	"bytes"
	"container/list"
	"sync"
)

// contractAcceptedSourceCacheBytes bounds the accepted-bytes cache.
const contractAcceptedSourceCacheBytes = 32 << 20

// contractAcceptedSourceCache maps a receipt's source fingerprint to the exact
// transformed bytes the journal fingerprinted. Entries are immutable by key.
// A contract job reads a file through it, so a file a writer saves again is
// still readable from its accept until its next begin, not only until its
// next disk write.
type contractAcceptedSourceCache struct {
	mu      sync.Mutex
	limit   int
	size    int
	order   *list.List
	entries map[string]*list.Element
}

type contractAcceptedSource struct {
	fingerprint string
	bytes       []byte
}

func newContractAcceptedSourceCache(limit int) *contractAcceptedSourceCache {
	return &contractAcceptedSourceCache{limit: limit, order: list.New(), entries: make(map[string]*list.Element)}
}

// put records bytes under their fingerprint. Bytes larger than the whole
// budget are not cached.
func (c *contractAcceptedSourceCache) put(fingerprint string, source []byte) {
	if c == nil || fingerprint == "" || len(source) > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[fingerprint]; ok {
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(contractAcceptedSource{fingerprint: fingerprint, bytes: bytes.Clone(source)})
	c.entries[fingerprint] = element
	c.size += len(source)
	for c.size > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(contractAcceptedSource)
		c.order.Remove(oldest)
		delete(c.entries, entry.fingerprint)
		c.size -= len(entry.bytes)
	}
}

// contractAcceptedSources returns the process's accepted-bytes cache.
func (mi *MultiIndexer) contractAcceptedSources() *contractAcceptedSourceCache {
	if mi == nil {
		return nil
	}
	mi.contractSourcesOnce.Do(func() {
		mi.contractSources = newContractAcceptedSourceCache(contractAcceptedSourceCacheBytes)
	})
	return mi.contractSources
}

// get returns a copy of the cached bytes for fingerprint.
func (c *contractAcceptedSourceCache) get(fingerprint string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[fingerprint]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return bytes.Clone(element.Value.(contractAcceptedSource).bytes), true
}
