package indexer

import (
	"os"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// bodyFactsCache holds parsed body facts for the duration of one
// post-pass invocation. Keyed by the handler's FilePath; trees are
// parsed lazily on first lookup and released by Close. Per-handler
// BodyFacts are memoised by SymbolID inside cacheEntry so a router
// referencing many handlers in the same file only parses that file
// once.
type bodyFactsCache struct {
	idx     *Indexer
	entries map[string]*bfCacheEntry // FilePath → entry
}

type bfCacheEntry struct {
	tree  *parser.ParseTree
	src   []byte
	lang  string
	facts map[string]contracts.BodyFacts // SymbolID → facts
}

// newBodyFactsCache creates a new cache scoped to one resolver pass.
func newBodyFactsCache(idx *Indexer) *bodyFactsCache {
	return &bodyFactsCache{idx: idx, entries: map[string]*bfCacheEntry{}}
}

// For returns BodyFacts for the handler graph node. Returns nopBodyFacts
// when the handler is unknown, the file can't be read, or the language
// has no registered factory.
func (c *bodyFactsCache) For(handler *graph.Node) contracts.BodyFacts {
	if handler == nil {
		return nopFacts
	}
	entry := c.entryFor(handler)
	if entry == nil {
		return nopFacts
	}
	if bf, ok := entry.facts[handler.ID]; ok {
		return bf
	}
	bf := contracts.MakeBodyFacts(entry.tree, handler)
	entry.facts[handler.ID] = bf
	return bf
}

func (c *bodyFactsCache) entryFor(handler *graph.Node) *bfCacheEntry {
	if entry, ok := c.entries[handler.FilePath]; ok {
		return entry
	}
	src := c.readFile(handler)
	if len(src) == 0 {
		c.entries[handler.FilePath] = nil
		return nil
	}
	lang := handler.Language
	if lang == "" {
		lang = detectLangFromPath(handler.FilePath)
	}
	tree := contracts.ParseTreeForLang(lang, src)
	entry := &bfCacheEntry{
		tree:  tree,
		src:   src,
		lang:  lang,
		facts: map[string]contracts.BodyFacts{},
	}
	c.entries[handler.FilePath] = entry
	return entry
}

// readFile resolves the handler's graph path to disk and reads its
// bytes. Mirrors the path-resolution dance in resolveProviderHandlers
// (strip repo prefix, join with rootPath).
func (c *bodyFactsCache) readFile(handler *graph.Node) []byte {
	if c.idx.contractAnalysisOnly {
		return c.idx.contractFileSrc(handler.FilePath)
	}
	disk := c.idx.ResolveFilePath(handler.FilePath)
	if disk == "" {
		return nil
	}
	data, err := os.ReadFile(disk)
	if err != nil {
		return nil
	}
	return data
}

// Close releases every cached parse tree. Safe to call once.
func (c *bodyFactsCache) Close() {
	for _, e := range c.entries {
		if e == nil {
			continue
		}
		e.tree.Release()
	}
	c.entries = nil
}

// nopFacts is the singleton no-op returned when no facts are available.
// Cheaper than allocating a new contracts.nopBodyFacts each call.
var nopFacts = contracts.MakeBodyFacts(nil, nil)
