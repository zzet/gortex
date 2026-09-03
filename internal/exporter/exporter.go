// Package exporter walks a graph store and writes it to portable formats so
// users can load it into external visualization and query tools (Neo4j,
// Memgraph via Cypher; yEd, Gephi, Cytoscape via GraphML).
//
// The exporter is read-only — it never mutates the graph. Filters (repo,
// kinds) are applied during emission.
package exporter

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Options narrows what gets exported. Empty filters mean "include everything".
type Options struct {
	// Repo restricts emission to nodes whose RepoPrefix matches. Empty means all.
	Repo string

	// Kinds restricts emission to nodes whose Kind is in this set. Empty means all.
	// Edges are always emitted iff both endpoints are emitted.
	Kinds []graph.NodeKind

	// Languages restricts emission to nodes whose Language is in this set.
	Languages []string

	// DropSynthetic suppresses synthetic stub nodes generated for edges that
	// point to unresolved imports, external symbols (`external::error`), or
	// annotation sentinels (`annotation::go::Deprecated`). Default false:
	// stubs are emitted with kind="external" so the call topology stays
	// intact for visualization. Set true for a strict view of the graph.
	DropSynthetic bool

	// Pretty toggles human-readable formatting (line breaks, indentation).
	Pretty bool
}

// Stats reports what was emitted. Returned by every exporter Write call.
type Stats struct {
	NodesWritten int
	EdgesWritten int
	NodesSkipped int
	EdgesSkipped int
	BytesWritten int64
}

// nodeFilter returns true for nodes that pass the option filters.
func (o *Options) nodeFilter(n *graph.Node) bool {
	if o.Repo != "" && n.RepoPrefix != o.Repo {
		return false
	}
	if len(o.Kinds) > 0 && !slices.Contains(o.Kinds, n.Kind) {
		return false
	}
	if len(o.Languages) > 0 && !slices.Contains(o.Languages, n.Language) {
		return false
	}
	return true
}

// snapshot collects nodes/edges that pass the filter into stable-sorted slices.
// Sorting makes exporter output deterministic — important for tests and diffs.
//
// When opts.DropSynthetic is false (default), edges pointing at IDs that are
// not real graph nodes (`unresolved::*`, `external::*`, `annotation::*`) get
// synthesized stub nodes added to the result so the call topology is preserved.
func snapshot(g graph.Reader, opts Options) ([]*graph.Node, []*graph.Edge, map[string]bool) {
	allNodes := g.AllNodes()
	allEdges := g.AllEdges()

	keptNodeIDs := make(map[string]bool, len(allNodes))
	nodes := make([]*graph.Node, 0, len(allNodes))
	for _, n := range allNodes {
		if opts.nodeFilter(n) {
			nodes = append(nodes, n)
			keptNodeIDs[n.ID] = true
		}
	}

	syntheticIDs := make(map[string]bool)
	edges := make([]*graph.Edge, 0, len(allEdges))
	for _, e := range allEdges {
		fromKept := keptNodeIDs[e.From]
		toKept := keptNodeIDs[e.To]
		if fromKept && toKept {
			edges = append(edges, e)
			continue
		}
		if opts.DropSynthetic {
			continue
		}
		// One endpoint is a synthetic placeholder (unresolved::*, external::*,
		// annotation::*). Only synthesize when the *other* endpoint is real
		// — otherwise we'd add an edge between two stubs no caller cares
		// about.
		if !fromKept && !toKept {
			continue
		}
		if !fromKept && classifySynthetic(e.From) != "" {
			syntheticIDs[e.From] = true
			edges = append(edges, e)
		}
		if !toKept && classifySynthetic(e.To) != "" {
			syntheticIDs[e.To] = true
			edges = append(edges, e)
		}
	}

	for id := range syntheticIDs {
		stub := &graph.Node{
			ID:   id,
			Name: synthName(id),
			Kind: graph.NodeKind(classifySynthetic(id)),
			Meta: map[string]any{"synthetic": true},
		}
		nodes = append(nodes, stub)
		keptNodeIDs[id] = true
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		return edges[i].Line < edges[j].Line
	})

	// De-dup edges: when both From and To are synthetic-but-tied to a real
	// other endpoint, the loop above could enqueue the same edge twice.
	dedup := edges[:0]
	seen := make(map[edgeKey]bool, len(edges))
	for _, e := range edges {
		k := edgeKey{from: e.From, to: e.To, kind: e.Kind, line: e.Line}
		if seen[k] {
			continue
		}
		seen[k] = true
		dedup = append(dedup, e)
	}

	return nodes, dedup, keptNodeIDs
}

type edgeKey struct {
	from string
	to   string
	kind graph.EdgeKind
	line int
}

// classifySynthetic returns a label kind ("external", "unresolved",
// "annotation") for the recognized synthetic ID prefixes, or "" when the ID
// doesn't look like a placeholder.
func classifySynthetic(id string) string {
	switch {
	case strings.HasPrefix(id, "unresolved::"):
		return "unresolved"
	case strings.HasPrefix(id, "external::"):
		return "external"
	case strings.HasPrefix(id, "annotation::"):
		return "annotation"
	}
	return ""
}

// synthName extracts a human-readable name from a synthetic ID. The IDs use
// "::" as separator; the last segment is the most informative bit.
func synthName(id string) string {
	idx := strings.LastIndex(id, "::")
	if idx < 0 {
		return id
	}
	return id[idx+2:]
}

// flattenMeta walks Node.Meta or Edge.Meta and returns it as a sorted slice of
// key/value pairs with values coerced to (string|int64|float64|bool). Nested
// maps and slices are JSON-encoded into a string. Keys with characters that
// can't legally be Cypher / GraphML property names are sanitized.
type metaEntry struct {
	Key   string
	Value any
}

func flattenMeta(meta map[string]any) []metaEntry {
	if len(meta) == 0 {
		return nil
	}
	out := make([]metaEntry, 0, len(meta))
	for k, v := range meta {
		safeKey := sanitizePropertyName(k)
		if safeKey == "" {
			continue
		}
		out = append(out, metaEntry{Key: safeKey, Value: coerceValue(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// coerceValue maps a graph Meta value to a primitive the export formats can
// represent. Nested types fall back to JSON-encoded strings so no information
// is lost — receivers just have to parse the string if they care.
func coerceValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, int, int32, int64, uint, uint32, uint64, float32, float64:
		return x
	default:
		// Maps, slices, structs — encode as JSON.
		data, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(data)
	}
}

// sanitizePropertyName returns a Cypher / GraphML-safe identifier. Replaces
// any character not in [a-zA-Z0-9_] with underscore. Empty / fully-illegal
// names are dropped (return "").
func sanitizePropertyName(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for i, r := range name {
		ok := r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "_" || strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// nodeLabel converts a NodeKind into a Cypher / GraphML label. Capitalizes
// the first letter of each underscore-separated segment so "function" →
// "Function" and "import_path" → "ImportPath".
func nodeLabel(kind graph.NodeKind) string {
	if kind == "" {
		return "Unknown"
	}
	parts := strings.Split(string(kind), "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// edgeRelType converts an EdgeKind to UPPER_SNAKE for Cypher relationship
// type / GraphML edge label.
func edgeRelType(kind graph.EdgeKind) string {
	if kind == "" {
		return "RELATED"
	}
	return strings.ToUpper(string(kind))
}

// countingWriter wraps an io.Writer to track bytes written.
type countingWriter struct {
	w io.Writer
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}
