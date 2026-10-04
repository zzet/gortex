package graph

import (
	"sort"
	"strings"
)

// The bottom store's scoped edge rows, kept per stack.
//
// A scoped framework run asks for the changed files' own rows of a kind
// (EdgesInScopeSeq) once per candidate check and once per pass that reads
// them. Under a per-stack projection cache the bottom store is the stack's
// immutable base generation, so its rows for a scope are the same for every
// delta over the stack: they are read once and kept unfiltered, filtered
// against the full stack (the delta's own layer included) at every read; the
// read hands out copies (EdgesInScopeSeq clones every row it yields), so a
// pass that rebinds a row it read never reaches the kept one.

// maxStackBaseScopedRows bounds the kept rows; past it, a scope is read and
// served without being kept.
const maxStackBaseScopedRows = 200_000

// stackBaseScopedKey names one scope: repositories, files and kinds, each
// sorted.
func stackBaseScopedKey(repoPrefixes, filePaths []string, kinds []EdgeKind) string {
	repos := append([]string(nil), repoPrefixes...)
	files := append([]string(nil), filePaths...)
	sort.Strings(repos)
	sort.Strings(files)
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	sort.Strings(names)
	return strings.Join(repos, "\x00") + "\x01" + strings.Join(files, "\x00") + "\x01" + strings.Join(names, "\x00")
}

// stackBaseScopedEdges answers the bottom store's scoped rows from the
// per-stack cache, reading and keeping them through load on a miss. ok is
// false when no per-stack cache is installed (the caller reads directly).
func (dw *DeltaWriter) stackBaseScopedEdges(repoPrefixes, filePaths []string, kinds []EdgeKind, load func() []ScopedEdgeRow) ([]ScopedEdgeRow, bool) {
	c := dw.baseCache
	if c == nil {
		return nil, false
	}
	key := stackBaseScopedKey(repoPrefixes, filePaths, kinds)
	c.mu.Lock()
	if rows, ok := c.stackBaseScoped[key]; ok {
		c.stackBaseScopedHit++
		c.mu.Unlock()
		return rows, true
	}
	c.stackBaseScopedMis++
	c.mu.Unlock()
	rows := load()
	c.mu.Lock()
	if c.stackBaseScoped == nil {
		c.stackBaseScoped = make(map[string][]ScopedEdgeRow)
	}
	if c.stackBaseScopedLen+len(rows) <= maxStackBaseScopedRows {
		if rows == nil {
			rows = []ScopedEdgeRow{}
		}
		c.stackBaseScoped[key] = rows
		c.stackBaseScopedLen += len(rows)
	}
	c.mu.Unlock()
	return rows, true
}

// StackBaseScopedStats reports the per-stack scoped-row hits and misses
// (scopes).
func (c *BaseProjectionCache) StackBaseScopedStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackBaseScopedHit, c.stackBaseScopedMis
}
