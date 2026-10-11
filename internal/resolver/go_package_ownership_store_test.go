package resolver

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGoPackageOwnershipActualStoreMultiplePagesPrepareOnce(t *testing.T) {
	if resolvePendingPageRows < 1 || resolvePendingPageRows > 32768 {
		t.Fatalf("fixture page cap needs review: %d", resolvePendingPageRows)
	}
	f := newOwnershipResolverFixture(t, "go")
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "pages.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch(f.r.graph.AllNodes(), nil)
	f.r = New(store)
	count := resolvePendingPageRows + 1
	nodes := make([]*graph.Node, 0, count)
	edges := make([]*graph.Edge, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("repo/main.go::Call%d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("Call%d", i), Kind: graph.KindFunction, FilePath: f.mainFile.FilePath, RepoPrefix: "repo", Language: "go"})
		edges = append(edges, &graph.Edge{From: id, To: "unresolved::extern::example.test/fixture/misc/graph::Use", Kind: graph.EdgeCalls, FilePath: f.mainFile.FilePath})
	}
	store.AddBatch(nodes, edges)
	calls := 0
	f.r.SetGoPackageOwnershipFactory(ownershipFactoryForFixture(t, f, &calls))
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	stats, err := f.r.ResolveAllContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unchanged multi-page pass prepared factory %d times; rows=%d page_cap=%d", calls, count, resolvePendingPageRows)
	}
	resolved := 0
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	bySource := store.GetOutEdgesByNodeIDs(ids)
	for _, node := range nodes {
		for _, edge := range bySource[node.ID] {
			if edge.Kind == graph.EdgeCalls {
				resolved++
				if edge.To != f.definitions[1].ID {
					t.Fatalf("paged target=%s want=%s", edge.To, f.definitions[1].ID)
				}
			}
		}
	}
	if resolved != count {
		t.Fatalf("paged fixture processed %d/%d calls", resolved, count)
	}
	if len(f.r.goPackageOwnershipPrepared) != 0 {
		t.Fatal("pass retained staged authority")
	}
	t.Logf("actual Store rows=%d page_cap=%d factory_calls=%d resolved=%d stats=%+v", count, resolvePendingPageRows, calls, resolved, stats)
}

// Actual Store + public resolver control. The foreign target is deliberately
// outside provider coverage; this does not claim cross-repo module certification
// or exercise the parser/registry lifecycle.
func TestGoPackageOwnershipForeignConsumerPreservesActualStoreBinding(t *testing.T) {
	if !dirMatchesImport("dependency/pkg", "example.test/dependency/pkg") {
		t.Fatal("foreign fixture is not eligible under the actual legacy import-path rule")
	}
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled_%v", enabled), func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "foreign.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			mainFile := &graph.Node{ID: "repo/main.go", Kind: graph.KindFile, FilePath: "repo/main.go", RepoPrefix: "repo", Language: "go"}
			caller := &graph.Node{ID: mainFile.ID + "::Call", Name: "Call", Kind: graph.KindFunction, FilePath: mainFile.FilePath, RepoPrefix: "repo", Language: "go"}
			foreignFile := &graph.Node{ID: "dependency/pkg/a.go", Kind: graph.KindFile, FilePath: "dependency/pkg/a.go", RepoPrefix: "dependency", Language: "go"}
			foreign := &graph.Node{ID: foreignFile.ID + "::Use", Name: "Use", Kind: graph.KindFunction, FilePath: foreignFile.FilePath, RepoPrefix: "dependency", Language: "go"}
			store.AddBatch([]*graph.Node{mainFile, caller, foreignFile, foreign}, []*graph.Edge{
				{From: caller.ID, To: "unresolved::extern::example.test/dependency/pkg::Use", Kind: graph.EdgeCalls, FilePath: mainFile.FilePath},
				{From: mainFile.ID, To: "unresolved::import::example.test/dependency/pkg", Kind: graph.EdgeImports, FilePath: mainFile.FilePath},
			})
			r := New(store)
			prepared := 0
			if enabled {
				r.SetGoPackageOwnershipFactory(func(ctx context.Context, prefixes []string, files iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error) {
					prepared++
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					if len(prefixes) != 1 || prefixes[0] != "repo" {
						t.Fatalf("scope=%v", prefixes)
					}
					for file := range files {
						if file.RepoPrefix != "repo" {
							t.Fatalf("foreign source enumerated: %+v", file)
						}
					}
					return map[string]GoPackageOwnershipLookup{"repo": func(q GoImportCandidate) GoPackageOwnershipResult {
						if q.CandidateRepoPrefix != "repo" {
							return GoPackageOwnershipUnknown
						}
						return GoPackageOwnershipDifferent
					}}, nil
				})
			}
			if _, err := r.ResolveAllContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if enabled && prepared != 1 {
				t.Fatalf("enabled control did not prepare: %d", prepared)
			}
			for _, item := range []struct {
				from, to string
				kind     graph.EdgeKind
			}{
				{caller.ID, foreign.ID, graph.EdgeCalls}, {mainFile.ID, foreignFile.ID, graph.EdgeImports},
			} {
				count := 0
				for _, edge := range store.GetOutEdges(item.from) {
					if edge.Kind == item.kind {
						count++
						if edge.To != item.to {
							t.Errorf("foreign target=%s want=%s", edge.To, item.to)
						}
					}
				}
				if count != 1 {
					t.Errorf("foreign matching edge count=%d", count)
				}
			}
		})
	}
}
