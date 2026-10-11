package indexer

import (
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// The dataflow pass's callee parameter index, kept per stack.
//
// Rewriting a changed file's arg_of edges onto parameter nodes reads, for
// every callee the file passes arguments to, the param_of edges into it and
// the parameter nodes — composed through every layer of the stack, 2.5-9 s
// per delta on a large store for a few hundred callees. A callee's parameters
// are declared in the callee's own file, so for a callee outside the delta's
// change set they are the stack's, the same on every delta over it: they are
// kept per stack, and only the change set's callees are read per delta.

const editDeltaParamIndexEntries = 8

var editDeltaParamIndexes struct {
	sync.Mutex
	keys  []string // most recent last
	byKey map[string]map[string]map[int]string
}

// editDeltaParamIndex is one delta's reader of the stack's parameter index.
type editDeltaParamIndex struct {
	key     string
	changed map[string]struct{} // graph paths of the change set
	hits    int
	misses  int
}

func newEditDeltaParamIndex(key string, changedGraphPaths []string) *editDeltaParamIndex {
	changed := make(map[string]struct{}, len(changedGraphPaths))
	for _, p := range changedGraphPaths {
		changed[p] = struct{}{}
	}
	return &editDeltaParamIndex{key: key, changed: changed}
}

// newEditDeltaParamIndexOver is newEditDeltaParamIndex for a delta over dw:
// a path the dirty chain below the delta speaks for is read through the delta
// like the change set's, since the kept tables are the stack's below the
// chain.
func newEditDeltaParamIndexOver(dw *graph.DeltaWriter, key string, changedGraphPaths []string) *editDeltaParamIndex {
	changed := append([]string(nil), changedGraphPaths...)
	for p := range dw.ChainTouchedPaths() {
		changed = append(changed, p)
	}
	return newEditDeltaParamIndex(key, changed)
}

// resetEditDeltaParamIndexes empties the cache (tests).
func resetEditDeltaParamIndexes() {
	editDeltaParamIndexes.Lock()
	editDeltaParamIndexes.keys, editDeltaParamIndexes.byKey = nil, nil
	editDeltaParamIndexes.Unlock()
}

func stackParamIndex(key string) map[string]map[int]string {
	editDeltaParamIndexes.Lock()
	defer editDeltaParamIndexes.Unlock()
	if editDeltaParamIndexes.byKey == nil {
		editDeltaParamIndexes.byKey = make(map[string]map[string]map[int]string)
	}
	idx, ok := editDeltaParamIndexes.byKey[key]
	if !ok {
		idx = make(map[string]map[int]string)
		editDeltaParamIndexes.byKey[key] = idx
		editDeltaParamIndexes.keys = append(editDeltaParamIndexes.keys, key)
		for len(editDeltaParamIndexes.keys) > editDeltaParamIndexEntries {
			delete(editDeltaParamIndexes.byKey, editDeltaParamIndexes.keys[0])
			editDeltaParamIndexes.keys = editDeltaParamIndexes.keys[1:]
		}
	}
	return idx
}

// ownerPath is the graph path a node identity is declared at (its prefix
// before "::"), empty for an identity that names none.
func ownerPath(id string) string {
	if i := strings.Index(id, "::"); i > 0 {
		return id[:i]
	}
	return ""
}

// index answers buildParamPositionIndex for callees: the stack's tables for
// callees declared outside the change set (read once per stack), and a read
// through g for the rest.
func (p *editDeltaParamIndex) index(g graph.Store, callees map[string]struct{}) map[string]map[int]string {
	stack := stackParamIndex(p.key)
	out := make(map[string]map[int]string, len(callees))
	own := make(map[string]struct{})
	missing := make(map[string]struct{})
	editDeltaParamIndexes.Lock()
	for id := range callees {
		if _, changed := p.changed[ownerPath(id)]; changed || ownerPath(id) == "" {
			own[id] = struct{}{}
			continue
		}
		if table, ok := stack[id]; ok {
			p.hits++
			if len(table) > 0 {
				out[id] = table
			}
			continue
		}
		missing[id] = struct{}{}
	}
	editDeltaParamIndexes.Unlock()
	p.misses += len(missing)
	if len(missing) > 0 {
		loaded := paramPositionIndexByFile(g, missing)
		editDeltaParamIndexes.Lock()
		for id := range missing {
			stack[id] = loaded[id]
			if len(loaded[id]) > 0 {
				out[id] = loaded[id]
			}
		}
		editDeltaParamIndexes.Unlock()
	}
	for id, table := range paramPositionIndexByFile(g, own) {
		out[id] = table
	}
	return out
}

// paramPositionIndexByFile answers buildParamPositionIndex for Go callees
// from their own files' parameter nodes: a Go parameter node is declared in
// its owner's file, identified <owner>#param:<name> with its position in
// Meta, and its param_of edge points at the owner. Reading the callees'
// files' parameter nodes (kept per stack by a delta's projection cache) costs
// O(callee files), where the param_of in-edge read of a popular callee reads
// its every incoming edge of every kind. Other callees keep the edge read.
func paramPositionIndexByFile(g graph.Store, callees map[string]struct{}) map[string]map[int]string {
	finder, ok := g.(graph.NodesInFilesByKindFinder)
	byFile := make(map[string][]string)
	rest := make(map[string]struct{})
	for id := range callees {
		file := ownerPath(id)
		if !ok || file == "" || !strings.HasSuffix(file, ".go") {
			rest[id] = struct{}{}
			continue
		}
		byFile[file] = append(byFile[file], id)
	}
	out := make(map[string]map[int]string, len(callees))
	if len(byFile) > 0 {
		files := make([]string, 0, len(byFile))
		for file := range byFile {
			files = append(files, file)
		}
		sort.Strings(files)
		wanted := make(map[string]struct{}, len(callees))
		for _, ids := range byFile {
			for _, id := range ids {
				wanted[id] = struct{}{}
			}
		}
		params := finder.NodesInFilesByKind(files, []graph.NodeKind{graph.KindParam})
		sort.Slice(params, func(i, j int) bool { return params[i].ID < params[j].ID })
		for _, n := range params {
			if n == nil || n.Kind != graph.KindParam || n.Language != "go" {
				continue
			}
			i := strings.Index(n.ID, "#param:")
			if i <= 0 {
				continue
			}
			owner := n.ID[:i]
			if _, want := wanted[owner]; !want {
				continue
			}
			pos, ok := intFromMeta(n.Meta, "position")
			if !ok {
				continue
			}
			table := out[owner]
			if table == nil {
				table = make(map[int]string)
				out[owner] = table
			}
			if _, exists := table[pos]; !exists {
				table[pos] = n.ID
			}
		}
	}
	for id, table := range buildParamPositionIndex(g, rest) {
		out[id] = table
	}
	return out
}
