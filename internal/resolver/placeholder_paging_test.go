package resolver

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// placeholderPagingFixture writes callers whose call resolves in the same
// package, and at each call site a dataflow edge sourced from the call's
// placeholder (`repo/unresolved::Cmd<i>`) whose callee also resolves. Filler
// pending references spread the two edges of a site across chunks.
func placeholderPagingFixture(g graph.Store, sites, fillerCount int) {
	nodes := []*graph.Node{
		{ID: "repo/pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo/pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repo/pkg/b.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo/pkg/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repo/pkg/a.go", Language: "go", RepoPrefix: "repo"},
	}
	var calls, fillers, flows []*graph.Edge
	for i := 0; i < sites; i++ {
		cmd := fmt.Sprintf("Cmd%d", i)
		nodes = append(nodes,
			&graph.Node{ID: "repo/pkg/b.go::" + cmd, Kind: graph.KindFunction, Name: cmd, FilePath: "repo/pkg/b.go", Language: "go", RepoPrefix: "repo"})
		line := 10 + i
		calls = append(calls, &graph.Edge{From: "repo/pkg/a.go::Run", To: graph.UnresolvedMarker + cmd, Kind: graph.EdgeCalls,
			FilePath: "repo/pkg/a.go", Line: line})
		// The call's result flows into a package-qualified standard-library
		// callee at the same site.
		flows = append(flows, &graph.Edge{From: "repo/" + graph.UnresolvedMarker + cmd,
			To: graph.UnresolvedMarker + "extern::strings::TrimSpace", Kind: graph.EdgeArgOf,
			FilePath: "repo/pkg/a.go", Line: line, Meta: map[string]any{"arg_position": 0}})
	}
	// Fillers that never resolve push the dataflow edges past the first page.
	for i := 0; i < fillerCount; i++ {
		fillers = append(fillers, &graph.Edge{From: "repo/pkg/a.go::Run", To: graph.UnresolvedMarker + fmt.Sprintf("Nowhere%d", i),
			Kind: graph.EdgeCalls, FilePath: "repo/pkg/a.go", Line: 100000 + i})
	}
	g.AddBatch(nodes, append(append(calls, fillers...), flows...))
}

func placeholderPagingDigest(g graph.Store) []string {
	var rows []string
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeArgOf} {
		for e := range g.EdgesByKind(kind) {
			if e.Line >= 100000 {
				continue
			}
			rows = append(rows, fmt.Sprintf("%s -> %s %s %s:%d", e.From, e.To, e.Kind, e.FilePath, e.Line))
		}
	}
	sort.Strings(rows)
	return rows
}

// A dataflow edge sourced from a call's placeholder gets one resolution of its
// callee (against the source it had when the pass began) and one move of its
// source, however the pass is paged and chunked: one page or several, chunk
// sizes 1, 64 and the default all write identical rows, on both backends.
func TestResolveAllPlaceholderSourcedDataflowIsIndependentOfPaging(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var reference []string
			var referenceName string
			for _, fillers := range []int{0, resolvePendingPageRows + 64} {
				for _, chunk := range []string{"", "1", "64"} {
					name := fmt.Sprintf("fillers=%d chunk=%q", fillers, chunk)
					t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", chunk)
					var g graph.Store
					if backend == "memory" {
						g = graph.New()
					} else {
						store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "paging.sqlite"))
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = store.Close() })
						g = store
					}
					placeholderPagingFixture(g, 8, fillers)
					New(g).ResolveAll()
					rows := placeholderPagingDigest(g)
					if reference == nil {
						reference, referenceName = rows, name
						want := "repo/pkg/b.go::Cmd0 -> stdlib::strings::TrimSpace arg_of repo/pkg/a.go:10"
						found := false
						for _, row := range rows {
							found = found || row == want
						}
						if !found {
							t.Fatalf("%s: the dataflow edge was not moved and bound (%q):\n%v", name, want, rows)
						}
						continue
					}
					if fmt.Sprint(rows) != fmt.Sprint(reference) {
						t.Fatalf("%s wrote different rows than %s:\n%v\nvs\n%v", name, referenceName, rows, reference)
					}
				}
			}
		})
	}
}
