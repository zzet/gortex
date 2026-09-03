package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval/quality"
	"github.com/zzet/gortex/internal/graph"
)

func TestSmartContextDefaultOff(t *testing.T) {
	s := &Server{} // no configManager → empty config
	// No include_* args → every in-pack section off.
	got := s.smartContextSections(map[string]any{}, "")
	if got.Any() {
		t.Errorf("smart_context in-pack sections should default off, got %+v", got)
	}

	// An explicit per-call opt-in turns the section on.
	on := s.smartContextSections(map[string]any{"include_call_paths": true}, "")
	if !on.CallPaths {
		t.Errorf("include_call_paths=true should enable CallPaths, got %+v", on)
	}
	if on.Flows || on.Confidence {
		t.Errorf("only CallPaths should be on, got %+v", on)
	}
}

func TestSmartContextDefaultOff_AttachNoop(t *testing.T) {
	s := &Server{}
	result := map[string]any{"relevant_symbols": []string{}}
	// Default-off sections leave the pack untouched.
	s.attachInPackSections(context.Background(), result, s.smartContextSections(map[string]any{}, ""), nil)
	if _, ok := result["in_pack"]; ok {
		t.Errorf("default-off should not add an in_pack block, got %+v", result["in_pack"])
	}
	// Opting call-paths in with no graph / no reachable paths adds no block.
	s.attachInPackSections(context.Background(), result, config.SmartContextSections{CallPaths: true}, nil)
	if _, ok := result["in_pack"]; ok {
		t.Errorf("opt-in with no reachable paths should add no block, got %+v", result["in_pack"])
	}
}

// smartCtxGraph builds a→b→focus, c→focus.
func smartCtxGraph() *graph.Graph {
	g := graph.New()
	for _, id := range []string{"focus", "a", "b", "c"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "b", To: "focus", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "c", To: "focus", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	return g
}

func TestSmartContextCallPaths(t *testing.T) {
	s := &Server{graph: smartCtxGraph()}
	// focus is the anchor (symbols[0]); a and c are roots.
	symbols := []*graph.Node{{ID: "focus"}, {ID: "a"}, {ID: "c"}}

	result := map[string]any{}
	s.attachInPackSections(context.Background(), result, config.SmartContextSections{CallPaths: true}, symbols)

	blk, ok := result["in_pack"].(map[string]any)
	if !ok {
		t.Fatalf("expected in_pack block, got %T", result["in_pack"])
	}
	cps, ok := blk["call_paths"].([]map[string]any)
	if !ok || len(cps) != 2 {
		t.Fatalf("expected 2 call_paths, got %T len=%d", blk["call_paths"], len(cps))
	}
	// Shortest first: c→focus (len 1) before a→b→focus (len 2).
	if cps[0]["root"] != "c" || cps[0]["length"] != 1 {
		t.Errorf("first path = %+v, want c len 1", cps[0])
	}
	if cps[1]["root"] != "a" || cps[1]["length"] != 2 {
		t.Errorf("second path = %+v, want a len 2", cps[1])
	}
	if cps[0]["anchor"] != "focus" {
		t.Errorf("anchor = %v, want focus", cps[0]["anchor"])
	}

	// Default-off leaves the pack untouched even with reachable symbols.
	off := map[string]any{}
	s.attachInPackSections(context.Background(), off, config.SmartContextSections{}, symbols)
	if _, ok := off["in_pack"]; ok {
		t.Errorf("call-paths off should add no block, got %+v", off["in_pack"])
	}
}

func TestGCXSmartContext_CallPaths(t *testing.T) {
	result := map[string]any{
		"relevant_symbols": []map[string]any{},
		"in_pack": map[string]any{
			"call_paths": []map[string]any{
				{"root": "c", "anchor": "focus", "length": 1, "confidence": 0.9, "nodes": []string{"c", "focus"}},
			},
		},
	}
	out, err := encodeSmartContext(result)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "smart_context.call_paths") {
		t.Errorf("GCX output missing call_paths section:\n%s", s)
	}
	if !strings.Contains(s, "c>focus") {
		t.Errorf("GCX output missing joined nodes 'c>focus':\n%s", s)
	}
}

// flowGraph builds a forward chain focus→a→b.
func flowGraph() *graph.Graph {
	g := graph.New()
	for _, id := range []string{"focus", "a", "b"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	g.AddEdge(&graph.Edge{From: "focus", To: "a", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	return g
}

func TestSmartContextFlows(t *testing.T) {
	s := &Server{graph: flowGraph()}
	result := map[string]any{}
	s.attachInPackSections(context.Background(), result, config.SmartContextSections{Flows: true}, []*graph.Node{{ID: "focus"}})

	blk, ok := result["in_pack"].(map[string]any)
	if !ok {
		t.Fatalf("expected in_pack block, got %T", result["in_pack"])
	}
	flows, ok := blk["flows"].(map[string]any)
	if !ok {
		t.Fatalf("expected flows section, got %T", blk["flows"])
	}
	spine, ok := flows["spine"].([]string)
	if !ok || len(spine) != 3 || spine[0] != "focus" || spine[1] != "a" || spine[2] != "b" {
		t.Fatalf("spine = %v, want [focus a b]", flows["spine"])
	}

	// GCX encoding emits a flow_spine section.
	out, err := encodeSmartContext(map[string]any{"relevant_symbols": []map[string]any{}, "in_pack": blk})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "smart_context.flow_spine") || !strings.Contains(string(out), "focus>a>b") {
		t.Errorf("GCX missing flow_spine:\n%s", out)
	}

	// Flows off → no block.
	off := map[string]any{}
	s.attachInPackSections(context.Background(), off, config.SmartContextSections{}, []*graph.Node{{ID: "focus"}})
	if _, ok := off["in_pack"]; ok {
		t.Errorf("flows off should add no block, got %+v", off["in_pack"])
	}
}

func TestSmartContextBoundary(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "focus", Kind: graph.KindFunction, Name: "focus", FilePath: "x.go"})
	g.AddEdge(&graph.Edge{From: "focus", To: "unresolved::dynamicCall", Kind: graph.EdgeCalls})
	s := &Server{graph: g}

	result := map[string]any{}
	s.attachInPackSections(context.Background(), result, config.SmartContextSections{Flows: true}, []*graph.Node{{ID: "focus"}})

	blk := result["in_pack"].(map[string]any)
	flows := blk["flows"].(map[string]any)
	bs, ok := flows["boundaries"].([]map[string]any)
	if !ok || len(bs) != 1 {
		t.Fatalf("boundaries = %v, want one", flows["boundaries"])
	}
	if bs[0]["from"] != "focus" || bs[0]["target"] != "dynamicCall" || bs[0]["reason"] != "dynamic_dispatch" {
		t.Errorf("boundary = %+v", bs[0])
	}

	out, err := encodeSmartContext(map[string]any{"relevant_symbols": []map[string]any{}, "in_pack": blk})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "smart_context.flow_boundaries") || !strings.Contains(string(out), "dynamicCall") {
		t.Errorf("GCX missing flow_boundaries:\n%s", out)
	}
}

func TestSmartContextConfidence(t *testing.T) {
	// Sharp top-1 → high (ratio 5.0).
	high := buildConfidenceVerdict(quality.ConfidenceFromScores("q", []float64{10, 2, 1}))
	if high == nil || high["verdict"] != "high" {
		t.Errorf("expected high verdict, got %+v", high)
	}
	// Modest lead → medium (ratio 1.3).
	med := buildConfidenceVerdict(quality.ConfidenceFromScores("q", []float64{1.3, 1.0}))
	if med == nil || med["verdict"] != "medium" {
		t.Errorf("expected medium verdict, got %+v", med)
	}
	// Flat → low (ratio 1.0).
	low := buildConfidenceVerdict(quality.ConfidenceFromScores("q", []float64{1.0, 1.0, 1.0}))
	if low == nil || low["verdict"] != "low" {
		t.Errorf("expected low verdict, got %+v", low)
	}
	// Single candidate → single.
	one := buildConfidenceVerdict(quality.ConfidenceFromScores("q", []float64{4.2}))
	if one == nil || one["verdict"] != "single" {
		t.Errorf("expected single verdict, got %+v", one)
	}
	// Empty → nil.
	if buildConfidenceVerdict(quality.ConfidenceFromScores("q", nil)) != nil {
		t.Errorf("empty scores should yield nil verdict")
	}

	// GCX encodes a confidence section.
	result := map[string]any{"relevant_symbols": []map[string]any{}}
	addInPackSection(result, "confidence", high)
	out, err := encodeSmartContext(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "smart_context.confidence") || !strings.Contains(string(out), "high") {
		t.Errorf("GCX missing confidence section:\n%s", out)
	}
}

func TestSmartCtxInPackBudget(t *testing.T) {
	// Five buckets, all caps non-decreasing with repo size.
	sizes := []int{1_000, 5_000, 20_000, 80_000, 500_000}
	var prev InPackBudget
	for i, n := range sizes {
		b := inPackBudgetForNodeCount(n)
		if b.MaxCallPaths < 1 || b.FlowDepth < 1 || b.MaxBoundaries < 1 {
			t.Errorf("nodes=%d: degenerate budget %+v", n, b)
		}
		if i > 0 {
			if b.MaxCallPaths < prev.MaxCallPaths || b.FlowDepth < prev.FlowDepth || b.MaxBoundaries < prev.MaxBoundaries {
				t.Errorf("nodes=%d: budget %+v should not shrink vs %+v", n, b, prev)
			}
		}
		prev = b
	}
	// Smallest bucket is tighter than the largest.
	small, large := inPackBudgetForNodeCount(100), inPackBudgetForNodeCount(1_000_000)
	if small.MaxCallPaths >= large.MaxCallPaths || small.FlowDepth >= large.FlowDepth {
		t.Errorf("small budget %+v should be tighter than large %+v", small, large)
	}
}

func TestSmartContextAdaptive(t *testing.T) {
	// A small graph (NodeCount < 2000) → MaxCallPaths 3, so six reachable roots
	// truncate to three.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "focus", Kind: graph.KindFunction, Name: "focus", FilePath: "x.go"})
	roots := []*graph.Node{{ID: "focus"}}
	for _, r := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		g.AddNode(&graph.Node{ID: r, Kind: graph.KindFunction, Name: r, FilePath: "x.go"})
		g.AddEdge(&graph.Edge{From: r, To: "focus", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
		roots = append(roots, &graph.Node{ID: r})
	}
	s := &Server{graph: g}
	if got := s.inPackBudget().MaxCallPaths; got != 3 {
		t.Fatalf("small-repo MaxCallPaths = %d, want 3", got)
	}

	cps := s.inPackCallPaths(roots)
	if len(cps) != 3 {
		t.Errorf("call_paths should be capped to 3 by the small-repo budget, got %d", len(cps))
	}
}

func TestSmartContextAssembly(t *testing.T) {
	// Pack symbols a, b, c with a→b (calls) and b→c (references) between them.
	g := graph.New()
	for _, id := range []string{"a", "b", "c", "outside"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "x.go"})
	}
	g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "b", To: "c", Kind: graph.EdgeReferences})
	g.AddEdge(&graph.Edge{From: "a", To: "outside", Kind: graph.EdgeCalls}) // outside the pack — excluded
	s := &Server{graph: g}
	pack := []*graph.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	rec := s.recoverPackEdges(context.Background(), pack)
	if len(rec) != 2 {
		t.Fatalf("expected 2 recovered edges (a→b, b→c), got %d: %+v", len(rec), rec)
	}
	// a→b first (sorted by from id).
	if rec[0]["from"] != "a" || rec[0]["to"] != "b" || rec[0]["kind"] != "calls" {
		t.Errorf("first recovered edge = %+v", rec[0])
	}
	if rec[1]["from"] != "b" || rec[1]["to"] != "c" || rec[1]["kind"] != "references" {
		t.Errorf("second recovered edge = %+v", rec[1])
	}

	// GCX encodes the section.
	out, err := encodeSmartContext(map[string]any{"relevant_symbols": []map[string]any{}, "recovered_edges": rec})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "smart_context.recovered_edges") {
		t.Errorf("GCX missing recovered_edges:\n%s", out)
	}
}

func TestSmartContextSiblings(t *testing.T) {
	// InternalEngine and ReadOnlyEngine both extend Engine; pack has InternalEngine.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Engine", Kind: graph.KindInterface, Name: "Engine", FilePath: "e.go"})
	g.AddNode(&graph.Node{ID: "InternalEngine", Kind: graph.KindType, Name: "InternalEngine", FilePath: "i.go"})
	g.AddNode(&graph.Node{ID: "ReadOnlyEngine", Kind: graph.KindType, Name: "ReadOnlyEngine", FilePath: "r.go"})
	g.AddEdge(&graph.Edge{From: "InternalEngine", To: "Engine", Kind: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: "ReadOnlyEngine", To: "Engine", Kind: graph.EdgeImplements})
	s := &Server{graph: g}
	pack := []*graph.Node{{ID: "InternalEngine", Kind: graph.KindType}}

	sibs := s.packHierarchySiblings(context.Background(), pack)
	if len(sibs) != 1 || sibs[0]["id"] != "ReadOnlyEngine" || sibs[0]["parent"] != "Engine" {
		t.Fatalf("expected ReadOnlyEngine sibling via Engine, got %+v", sibs)
	}

	out, err := encodeSmartContext(map[string]any{"relevant_symbols": []map[string]any{}, "hierarchy_siblings": sibs})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "smart_context.hierarchy_siblings") || !strings.Contains(string(out), "ReadOnlyEngine") {
		t.Errorf("GCX missing hierarchy_siblings:\n%s", out)
	}
}

// TestRetrievalNoteFiresOnFlatDistribution exercises the pure trigger behind
// the always-on low-confidence retrieval note: it fires on a flat ranked
// distribution, fires on a speculatively-anchored head even when the
// distribution is sharp (the provenance-fusion BEAT axis), and is suppressed
// for sharp distributions, distinctive-identifier lookups, and single hits.
func TestRetrievalNoteFiresOnFlatDistribution(t *testing.T) {
	task := "how does the parser handle errors"
	flat := quality.ConfidenceFromScores(task, []float64{10, 9.8, 9.7, 9.6, 9.5})
	sharp := quality.ConfidenceFromScores(task, []float64{10, 2, 1.5, 1, 0.5})

	// 1. Flat distribution + natural-language task → note fires, routes to the
	// richer escape hatches, carries the pack dirs, cites the distribution.
	note := retrievalNoteFor(task, flat, false, []string{"internal/parser"})
	if note == nil {
		t.Fatal("flat distribution must produce a low-confidence note")
	}
	if note["verdict"] != "low" {
		t.Errorf("verdict = %v, want low", note["verdict"])
	}
	tools, _ := note["suggested_tools"].([]string)
	if !containsString(tools, "find_usages") || !containsString(tools, "search_text") || !containsString(tools, "find_files") {
		t.Errorf("suggested_tools = %v, want find_usages + search_text + find_files", tools)
	}
	dirs, _ := note["likely_dirs"].([]string)
	if len(dirs) == 0 || dirs[0] != "internal/parser" {
		t.Errorf("likely_dirs = %v, want the pack dirs", dirs)
	}
	if reason, _ := note["reason"].(string); !strings.Contains(reason, "clustered") {
		t.Errorf("flat reason should cite the distribution; got %q", reason)
	}

	// 2. Sharp distribution + non-speculative head → no note.
	if n := retrievalNoteFor(task, sharp, false, nil); n != nil {
		t.Errorf("a sharp distribution must not draw a note; got %v", n)
	}

	// 3. Provenance fusion: sharp distribution but the head symbol is anchored
	// only by speculative edges → the note still fires.
	n := retrievalNoteFor(task, sharp, true, nil)
	if n == nil {
		t.Fatal("a speculatively-anchored head must draw a note even on a sharp distribution")
	}
	if reason, _ := n["reason"].(string); !strings.Contains(reason, "speculative") {
		t.Errorf("provenance reason should cite speculative anchoring; got %q", reason)
	}

	// 4. A distinctive identifier lookup is an exact query — never hedged.
	if n := retrievalNoteFor("ParseFile", flat, false, nil); n != nil {
		t.Errorf("a distinctive identifier lookup must not draw a note; got %v", n)
	}

	// 5. A single ranked candidate is confident — no note.
	single := quality.ConfidenceFromScores(task, []float64{10})
	if n := retrievalNoteFor(task, single, true, nil); n != nil {
		t.Errorf("a single-candidate retrieval must not draw a note; got %v", n)
	}
}
