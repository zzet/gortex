package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const foldQuantumFile = "repo/pending.go"

func foldQuantumStore(t *testing.T, count int) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s::N%05d", foldQuantumFile, i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("N%05d", i), FilePath: foldQuantumFile, RepoPrefix: "repo", StartLine: i + 1, Meta: map[string]any{"receipt": "selected"}})
		edges = append(edges, &graph.Edge{From: id, To: "repo/outside.go::Target", Kind: graph.EdgeCalls, FilePath: foldQuantumFile, Line: i + 1, Confidence: 0.75, Origin: "derived", Meta: map[string]any{"via": "test"}})
	}
	nodes = append(nodes, &graph.Node{ID: "repo/outside.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/outside.go", RepoPrefix: "repo"})
	edges = append(edges, &graph.Edge{From: "repo/outside.go::Target", To: nodes[0].ID, Kind: graph.EdgeReferences, FilePath: foldQuantumFile, Line: 9000, Confidence: 0.5, Origin: "import"})
	if err := store.AddBatchChecked(nodes, edges); err != nil {
		t.Fatal(err)
	}
	return store
}

type foldQuantumProbe struct {
	graph.Reader
	cancelAt string
	cancel   context.CancelFunc
	files    int
	out      [][]string
	in       [][]string
	recorded int
}

func (p *foldQuantumProbe) GetFileNodes(path string) []*graph.Node {
	p.files++
	nodes := p.Reader.GetFileNodes(path)
	if p.cancelAt == "file" {
		p.cancel()
	}
	if p.cancelAt == "node" && len(nodes) > 0 {
		first := *nodes[0]
		first.Meta = map[string]any{"cancel": foldQuantumCancelStringer{p.cancel}}
		nodes = slices.Clone(nodes)
		nodes[0] = &first
	}
	return nodes
}

func (p *foldQuantumProbe) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	p.out = append(p.out, slices.Clone(ids))
	rows := p.Reader.GetOutEdgesByNodeIDs(ids)
	if p.cancelAt == "out" {
		p.cancel()
	}
	return rows
}

func (p *foldQuantumProbe) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	p.in = append(p.in, slices.Clone(ids))
	rows := p.Reader.GetInEdgesByNodeIDs(ids)
	if p.cancelAt == "in" {
		p.cancel()
	}
	return rows
}

func (p *foldQuantumProbe) RecordedEdgesAt(paths []string) []*graph.Edge {
	p.recorded++
	r, ok := graph.RecordedEdgesOf(p.Reader)
	if !ok {
		panic("fixture reader has no recorded edges")
	}
	rows := r.RecordedEdgesAt(paths)
	if p.cancelAt == "recorded" {
		p.cancel()
	}
	return rows
}

type foldQuantumCancelStringer struct{ cancel context.CancelFunc }

func (s foldQuantumCancelStringer) String() string {
	s.cancel()
	return "cancelled during node rendering"
}

// A single real stored file has more identities than a path quantum. Cancel
// at deterministic read/render boundaries, without timers or reader goroutines.
func TestRenderFoldChunkSinglePathCancellationStopsAdjacency(t *testing.T) {
	store := foldQuantumStore(t, 8193)
	for _, at := range []string{"file", "node", "out", "in", "recorded"} {
		t.Run(at, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probe := &foldQuantumProbe{Reader: store, cancelAt: at, cancel: cancel}
			got, err := renderFoldChunk(ctx, probe, []string{foldQuantumFile}, nil, 0, 1, map[string]struct{}{})
			if !errors.Is(err, context.Canceled) || got != nil {
				t.Fatalf("cancel at %s returned %d usable rows, error %v", at, len(got), err)
			}
			wantOut, wantIn, wantRecorded := 0, 0, 0
			switch at {
			case "out":
				wantOut = 1
			case "in":
				wantOut, wantIn = 33, 1
			case "recorded":
				wantOut, wantIn, wantRecorded = 33, 33, 1
			}
			if probe.files != 1 || len(probe.out) != wantOut || len(probe.in) != wantIn || probe.recorded != wantRecorded {
				t.Fatalf("continued reads after cancellation: files=%d out=%d in=%d recorded=%d; want 1/%d/%d/%d", probe.files, len(probe.out), len(probe.in), probe.recorded, wantOut, wantIn, wantRecorded)
			}
			for _, groups := range [][][]string{probe.out, probe.in} {
				for _, ids := range groups {
					if len(ids) > verifyFoldChunk {
						t.Fatalf("single-path adjacency contained %d IDs, want <=%d", len(ids), verifyFoldChunk)
					}
				}
			}
		})
	}
}

// This independent unbatched point oracle retains every node, absent identity,
// outgoing/incoming edge and recording-file row of the selected reader.
func foldQuantumPointOracle(r graph.Reader, paths, ids []string) []string {
	var out, visited, absent []string
	seen := map[string]struct{}{}
	visit := func(n *graph.Node) {
		if n == nil {
			return
		}
		if _, dup := seen[n.ID]; dup {
			return
		}
		seen[n.ID] = struct{}{}
		out = append(out, renderFoldNode(n))
		visited = append(visited, n.ID)
	}
	for _, p := range paths {
		for _, n := range r.GetFileNodes(p) {
			visit(n)
		}
	}
	for _, id := range ids {
		if n := r.GetNode(id); n != nil {
			visit(n)
		} else {
			absent = append(absent, id)
			out = append(out, "absent "+id)
		}
	}
	for _, id := range append(slices.Clone(visited), absent...) {
		for _, e := range r.GetOutEdges(id) {
			out = append(out, renderFoldEdge(e))
		}
	}
	for _, id := range visited {
		for _, e := range r.GetInEdges(id) {
			out = append(out, "in "+renderFoldEdge(e))
		}
	}
	if recorded, ok := graph.RecordedEdgesOf(r); ok {
		for _, e := range recorded.RecordedEdgesAt(paths) {
			out = append(out, "at "+renderFoldEdge(e))
		}
	}
	sort.Strings(out)
	return out
}

func TestRenderFoldChunkPreservesSelectedRowsAndSeen(t *testing.T) {
	store := foldQuantumStore(t, 513)
	paths := []string{foldQuantumFile}
	ids := []string{foldQuantumFile + "::N00000", "repo/missing.go::Gone"}
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprintf("overlay=%t", selected), func(t *testing.T) {
			var reader graph.Reader = store
			if selected {
				layer := graph.NewOverlayLayer()
				layer.MarkFile(foldQuantumFile, false)
				layer.AddNode(foldQuantumFile, &graph.Node{ID: ids[0], Kind: graph.KindFunction, Name: "Replacement", FilePath: foldQuantumFile, StartLine: 7, Meta: map[string]any{"receipt": "replacement"}})
				layer.AddEdge(&graph.Edge{From: ids[0], To: "repo/outside.go::Target", Kind: graph.EdgeCalls, FilePath: foldQuantumFile, Line: 7, Confidence: 0.25, Origin: "inferred", Meta: map[string]any{"via": "replacement"}})
				reader = graph.NewOverlaidViewWithLayer(store, layer)
			}
			probe := &foldQuantumProbe{Reader: reader}
			seen := map[string]struct{}{}
			got, err := renderFoldChunk(t.Context(), probe, paths, ids, 0, len(paths)+len(ids), seen)
			if err != nil {
				t.Fatal(err)
			}
			want := foldQuantumPointOracle(reader, paths, ids)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("selected rows changed: got %d rows, want %d", len(got), len(want))
			}
			if !slices.Contains(got, "absent "+ids[1]) {
				t.Fatal("missing identity was not checked")
			}
			if selected {
				for _, row := range got {
					if strings.Contains(row, "receipt:selected") || strings.Contains(row, "N00001|") {
						t.Fatalf("legacy lower row survived selected replacement: %s", row)
					}
				}
			}
			again, err := renderFoldChunk(t.Context(), probe, nil, ids[:1], 0, 1, seen)
			if err != nil || len(again) != 0 {
				t.Fatalf("already-rendered identity changed seen semantics: rows=%v err=%v", again, err)
			}
		})
	}
}

func TestRenderFoldChunkCancelledEmptyInputDoesNotCertify(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err := renderFoldChunk(ctx, nil, nil, nil, 0, 0, map[string]struct{}{})
	if rows != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("empty cancelled verification returned rows=%v err=%v", rows, err)
	}
}
