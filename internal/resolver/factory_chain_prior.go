package resolver

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// A per-save factory-chain pass walked every unresolved call or reference with
// a receiver expression in the changed file — hundreds in a Go file, where
// every `c.store.X()` qualifies — and typed each chain with name lookups
// across the store, only for nearly all of them to fail exactly as they did
// before the save.
//
// A chain's outcome depends on the declarations it names (the base function
// or type, each segment's method and its return type), the methods of the
// types it walks, and the implements/extends rows of those types. A save
// changes only the changed files, so when the files declare exactly what they
// declared before — same kinds, names, receivers and return types, and the
// same hierarchy rows out of them — a chain that stayed unresolved last time
// stays unresolved now. FactoryChainPrior carries what that needs, read from
// the changed files' prior rows before they were evicted.

// FactoryChainPrior is the factory-chain state of the files one incremental
// mutation re-parses.
type FactoryChainPrior struct {
	// Files are the graph paths it was captured for.
	Files map[string]struct{}
	// unresolved holds the chains (factoryChainPriorKey) that were still
	// unresolved in the prior rows.
	unresolved map[string]struct{}
	// declarations is, per file, the declaration signature
	// (factoryChainDeclarationSignature) of the prior nodes.
	declarations map[string]string
}

// NewFactoryChainPrior returns an empty prior.
func NewFactoryChainPrior() *FactoryChainPrior {
	return &FactoryChainPrior{
		Files:        make(map[string]struct{}),
		unresolved:   make(map[string]struct{}),
		declarations: make(map[string]string),
	}
}

// factoryChainPriorKey identifies a chain independently of its line: the
// file, the receiver expression and the unresolved target (which names the
// method).
func factoryChainPriorKey(edge *graph.Edge) string {
	expr, _ := edge.Meta["receiver_expr"].(string)
	return edge.FilePath + "\x00" + string(edge.Kind) + "\x00" + expr + "\x00" + edge.To
}

// factoryChainCandidate reports an edge the factory-chain pass would try.
func factoryChainCandidate(edge *graph.Edge) bool {
	if edge == nil || edge.Meta == nil || (edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences) {
		return false
	}
	if !graph.IsUnresolvedTarget(edge.To) {
		return false
	}
	expr, _ := edge.Meta["receiver_expr"].(string)
	return expr != ""
}

// factoryChainDeclarationSignature is what the file declares for the chain
// walk: every node's kind, name, receiver and return type, and every
// implements/extends row out of its types (the conformance walk's rows).
func factoryChainDeclarationSignature(nodes []*graph.Node, out func(id string) []*graph.Edge) string {
	rows := make([]string, 0, len(nodes))
	for _, node := range nodes {
		// A closure is named by its line (closure@N) and no chain can name
		// it, so a line typed above one does not change what the file
		// declares for a chain.
		if node == nil || node.Kind == graph.KindClosure {
			continue
		}
		receiver, returnType := "", ""
		if node.Meta != nil {
			receiver, _ = node.Meta["receiver"].(string)
			returnType, _ = node.Meta["return_type"].(string)
		}
		rows = append(rows, "n\x00"+string(node.Kind)+"\x00"+node.Name+"\x00"+receiver+"\x00"+returnType)
		if !isTypeNodeKind(node.Kind) {
			continue
		}
		for _, edge := range out(node.ID) {
			if edge != nil && (edge.Kind == graph.EdgeImplements || edge.Kind == graph.EdgeExtends) {
				rows = append(rows, "h\x00"+node.Name+"\x00"+string(edge.Kind)+"\x00"+edge.To)
			}
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\x01")
}

// AddFile records one file's prior nodes and their outgoing rows.
func (p *FactoryChainPrior) AddFile(path string, nodes []*graph.Node, outByNode map[string][]*graph.Edge) {
	if p == nil || path == "" {
		return
	}
	p.Files[path] = struct{}{}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, edge := range outByNode[node.ID] {
			if factoryChainCandidate(edge) && edge.FilePath == path {
				p.unresolved[factoryChainPriorKey(edge)] = struct{}{}
			}
		}
	}
	p.declarations[path] = factoryChainDeclarationSignature(nodes, func(id string) []*graph.Edge { return outByNode[id] })
}

// Merge folds other into p and returns the result.
func (p *FactoryChainPrior) Merge(other *FactoryChainPrior) *FactoryChainPrior {
	if other == nil {
		return p
	}
	if p == nil {
		return other
	}
	for file := range other.Files {
		p.Files[file] = struct{}{}
	}
	for key := range other.unresolved {
		p.unresolved[key] = struct{}{}
	}
	for file, signature := range other.declarations {
		p.declarations[file] = signature
	}
	return p
}

// declarationsUnchanged reports whether every captured file declares what it
// declared before the save.
func (p *FactoryChainPrior) declarationsUnchanged(g graph.Store) bool {
	if p == nil || len(p.Files) == 0 {
		return false
	}
	files := make([]string, 0, len(p.Files))
	for file := range p.Files {
		files = append(files, file)
	}
	sort.Strings(files)
	byFile := g.GetFileNodesByPaths(files)
	ids := make([]string, 0)
	for _, file := range files {
		for _, node := range byFile[file] {
			if node != nil && isTypeNodeKind(node.Kind) {
				ids = append(ids, node.ID)
			}
		}
	}
	out := g.GetOutEdgesByNodeIDs(ids)
	for _, file := range files {
		prior, ok := p.declarations[file]
		if !ok || prior != factoryChainDeclarationSignature(byFile[file], func(id string) []*graph.Edge { return out[id] }) {
			return false
		}
	}
	return true
}

// factoryChainPriorSkipped counts the chains a per-save pass did not walk
// because they failed before and nothing they depend on changed.
var factoryChainPriorSkipped atomic.Int64

// FactoryChainPriorSkipped reports factoryChainPriorSkipped (tests and
// measurement).
func FactoryChainPriorSkipped() int64 { return factoryChainPriorSkipped.Load() }

// resolveFactoryChainsWithPrior is resolveFactoryChains for a scoped run with
// the changed files' prior: when the files declare what they did, a chain
// that stayed unresolved before is not walked again.
func resolveFactoryChainsWithPrior(g graph.Store, prior *FactoryChainPrior) int {
	if g == nil {
		return 0
	}
	if prior == nil || !prior.declarationsUnchanged(g) {
		return resolveFactoryChains(g, nil)
	}
	var walk []*graph.Edge
	skipped := 0
	for e := range edgesByKinds(g, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}) {
		if !factoryChainCandidate(e) {
			continue
		}
		if _, captured := prior.Files[e.FilePath]; captured {
			if _, failed := prior.unresolved[factoryChainPriorKey(e)]; failed {
				skipped++
				continue
			}
		}
		walk = append(walk, e)
	}
	factoryChainPriorSkipped.Add(int64(skipped))
	return resolveFactoryChains(g, &frameworkPassCandidates{calls: walk})
}
