package tstypes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildFixture writes the fixture files under a temp dir, indexes them
// with the real per-language extractors (so the graph carries the
// exact node-ID and unresolved-edge conventions the daemon's index
// produces), and returns the graph plus the repo root. testing.TB so
// benchmarks can build the same fixtures.
func buildFixture(t testing.TB, files map[string]string) (*graph.Graph, string) {
	t.Helper()
	dir := t.TempDir()
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		lang, ok := reg.DetectLanguage(rel)
		if !ok {
			t.Fatalf("no language for %s", rel)
		}
		ext, ok := reg.GetByLanguage(lang)
		if !ok {
			t.Fatalf("no extractor for %s", lang)
		}
		res, err := ext.Extract(rel, []byte(content))
		if err != nil {
			t.Fatalf("extract %s: %v", rel, err)
		}
		if res.Tree != nil {
			res.Tree.Close()
		}
		g.AddBatch(res.Nodes, res.Edges)
	}
	return g, dir
}

// nodeByNameKind returns the unique node with the given name and kind.
func nodeByNameKind(t *testing.T, g *graph.Graph, name string, kind graph.NodeKind) *graph.Node {
	t.Helper()
	var found *graph.Node
	for _, n := range g.FindNodesByName(name) {
		if n.Kind != kind {
			continue
		}
		if found != nil {
			t.Fatalf("multiple %s nodes named %q", kind, name)
		}
		found = n
	}
	if found == nil {
		t.Fatalf("no %s node named %q", kind, name)
	}
	return found
}

// callEdgeTo returns the first calls-edge from the caller whose target
// id is exactly to.
func callEdgeTo(g *graph.Graph, fromID, to string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls && e.To == to {
			return e
		}
	}
	return nil
}

// callEdgesNamed returns every calls-edge from the caller whose target
// trailing name matches.
func callEdgesNamed(g *graph.Graph, fromID, name string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls && trailingNameMatches(e.To, name) {
			out = append(out, e)
		}
	}
	return out
}

// edgeBetween returns the edge of the given kind between two node ids.
func edgeBetween(g *graph.Graph, fromID string, kind graph.EdgeKind, toID string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind && e.To == toID {
			return e
		}
	}
	return nil
}

// assertASTProvenance checks the edge carries this engine's stamp.
func assertASTProvenance(t *testing.T, e *graph.Edge, provider string) {
	t.Helper()
	if e.Origin != graph.OriginASTResolved {
		t.Errorf("origin = %q, want %q", e.Origin, graph.OriginASTResolved)
	}
	if e.Meta == nil || e.Meta["semantic_source"] != provider {
		t.Errorf("semantic_source = %v, want %q", e.Meta["semantic_source"], provider)
	}
	if e.Confidence < astConfidence {
		t.Errorf("confidence = %v, want >= %v", e.Confidence, astConfidence)
	}
}

// assertUntouched checks no engine stamp landed on any calls-edge of
// the caller matching the method name — the negative-case contract.
func assertUntouched(t *testing.T, g *graph.Graph, fromID, method, provider string) {
	t.Helper()
	for _, e := range callEdgesNamed(g, fromID, method) {
		if e.Meta != nil && e.Meta["semantic_source"] == provider {
			t.Errorf("edge %s -> %s was touched by %s; want untouched", e.From, e.To, provider)
		}
		if !graph.IsUnresolvedTarget(e.To) && !strings.Contains(e.To, "::") {
			t.Errorf("edge target %q unexpectedly resolved", e.To)
		}
	}
}
