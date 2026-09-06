package contracts_test

import (
	"fmt"
	"iter"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Route the retained global loader path to one repository, exercising the old
// full-repository hydration without copying or changing the contract decoder.
type legacyRepoStore struct {
	graph.Store
	repoPrefix string
}

func (s legacyRepoStore) GetRepoNodes(string) []*graph.Node {
	return s.Store.GetRepoNodes(s.repoPrefix)
}

func legacyLoadRegistryFromGraph(g graph.Store, repoPrefix string) *contracts.Registry {
	if g == nil {
		return nil
	}
	return contracts.LoadRegistryFromGraph(legacyRepoStore{Store: g, repoPrefix: repoPrefix}, "")
}

func registryContractsByID(reg *contracts.Registry) map[string]contracts.Contract {
	if reg == nil {
		return nil
	}
	out := make(map[string]contracts.Contract)
	for _, c := range reg.All() {
		out[c.ID] = c
	}
	return out
}

func openContractLoaderSQLite(tb testing.TB) *store_sqlite.Store {
	tb.Helper()
	g, err := store_sqlite.Open(filepath.Join(tb.TempDir(), "contracts.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := g.Close(); err != nil {
			tb.Error(err)
		}
	})
	return g
}

// Erase optional projection capabilities to exercise the generic adapter path.
type contractLoaderAdapter struct {
	graph.Store
}

func TestLoadRegistryFromGraphProjectionEquivalent(t *testing.T) {
	factories := map[string]func(*testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store { return openContractLoaderSQLite(t) },
		"adapter": func(t *testing.T) graph.Store {
			return contractLoaderAdapter{Store: graph.New()}
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			g := factory(t)
			g.AddBatch([]*graph.Node{
				{
					ID: "alpha::ordinary", Kind: graph.KindFunction, RepoPrefix: "alpha",
					FilePath: "api.go", Meta: map[string]any{"type": "not-a-contract"},
				},
				{
					ID: "alpha::full", Kind: graph.KindContract, RepoPrefix: "alpha",
					FilePath: "api.go", WorkspaceID: "workspace", ProjectID: "project",
					Meta: map[string]any{
						"type": "http", "role": "provider", "symbol_id": "alpha::ordinary",
						"line": 31, "confidence": 0.75,
						"contract_meta": map[string]any{
							"route": "/api", "nested": map[string]any{"method": "GET"},
						},
					},
				},
				{
					ID: "alpha::wrapper", Kind: graph.KindContract, RepoPrefix: "alpha",
					FilePath: "wrapper.go", WorkspaceID: "workspace", ProjectID: "project",
					Meta: map[string]any{
						"type": "http", "role": "consumer", "line": int64(47),
						"contract_meta": map[string]any{"wrapped_symbol": "alpha::ordinary"},
					},
				},
				{
					ID: "alpha::sparse", Kind: graph.KindContract, RepoPrefix: "alpha",
					FilePath: "sparse.go", WorkspaceID: "workspace", ProjectID: "project",
				},
				{
					ID: "alpha::bridge", Kind: graph.KindContractBridge, RepoPrefix: "alpha",
					FilePath: "api.go",
				},
				{
					ID: "beta::contract", Kind: graph.KindContract, RepoPrefix: "beta",
					FilePath: "api.go", Meta: map[string]any{"type": "http"},
				},
			}, nil)
			for _, prefix := range []string{"alpha", "beta", "missing", ""} {
				t.Run(prefix, func(t *testing.T) {
					want := registryContractsByID(legacyLoadRegistryFromGraph(g, prefix))
					got := registryContractsByID(contracts.LoadRegistryFromGraph(g, prefix))
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("loaded contracts differ: got %#v; want %#v", got, want)
					}
					if prefix == "alpha" {
						if len(got) != 3 {
							t.Fatalf("alpha contracts = %d, want 3", len(got))
						}
						wantFull := contracts.Contract{
							ID: "alpha::full", FilePath: "api.go", RepoPrefix: "alpha",
							WorkspaceID: "workspace", ProjectID: "project",
							Type: contracts.ContractType("http"), Role: contracts.Role("provider"),
							SymbolID: "alpha::ordinary", Line: 31, Confidence: 0.75,
							Meta: map[string]any{
								"route": "/api", "nested": map[string]any{"method": "GET"},
							},
						}
						if !reflect.DeepEqual(got["alpha::full"], wantFull) {
							t.Fatalf("full contract = %#v; want %#v", got["alpha::full"], wantFull)
						}
						if got["alpha::full"].Line != 31 || got["alpha::wrapper"].Line != 47 {
							t.Fatal("metadata line numbers were not preserved")
						}
						if got["alpha::full"].WorkspaceID != "workspace" ||
							got["alpha::full"].ProjectID != "project" {
							t.Fatal("contract workspace/project identity was not preserved")
						}
					}
				})
			}
		})
	}
	if got := contracts.LoadRegistryFromGraph(nil, "alpha"); got != nil {
		t.Fatalf("nil store returned %#v", got)
	}
}

type contractLoaderProjectionSpy struct {
	graph.Store
	graph.ScopedProjectionSequencer
	t     *testing.T
	nodes []*graph.Node
	calls int
}

func (s *contractLoaderProjectionSpy) GetRepoNodes(string) []*graph.Node {
	s.t.Fatal("scoped contract loading must not hydrate the whole repository")
	return nil
}

func (s *contractLoaderProjectionSpy) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.calls++
	if !reflect.DeepEqual(repos, []string{"alpha"}) || len(files) != 0 ||
		!reflect.DeepEqual(kinds, []graph.NodeKind{graph.KindContract}) {
		s.t.Fatalf("unexpected contract projection: repos=%v files=%v kinds=%v", repos, files, kinds)
	}
	return func(yield func(*graph.Node) bool) {
		for _, n := range s.nodes {
			if !yield(n) {
				return
			}
		}
	}
}

func TestLoadRegistryFromGraphUsesScopedProjection(t *testing.T) {
	spy := &contractLoaderProjectionSpy{t: t, nodes: []*graph.Node{
		nil,
		{ID: "", Kind: graph.KindContract, RepoPrefix: "alpha"},
		{ID: "alpha::ordinary", Kind: graph.KindFunction, RepoPrefix: "alpha"},
		{ID: "alpha::sparse", Kind: graph.KindContract, RepoPrefix: "alpha"},
	}}
	got := registryContractsByID(contracts.LoadRegistryFromGraph(spy, "alpha"))
	if spy.calls != 1 || len(got) != 1 || got["alpha::sparse"].ID != "alpha::sparse" {
		t.Fatalf("projection calls=%d, contracts=%#v", spy.calls, got)
	}
}

func BenchmarkLoadRegistryFromGraph(b *testing.B) {
	g := openContractLoaderSQLite(b)
	const ordinaryCount = 10000
	const contractCount = 100
	nodes := make([]*graph.Node, 0, ordinaryCount+contractCount)
	payload := strings.Repeat("source and documentation ", 64)
	for i := 0; i < ordinaryCount; i++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("bench::function-%d", i), Kind: graph.KindFunction,
			RepoPrefix: "bench", FilePath: "api.go",
			Meta: map[string]any{"documentation": payload, "signature": "func Example()"},
		})
	}
	for i := 0; i < contractCount; i++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("bench::contract-%d", i), Kind: graph.KindContract,
			RepoPrefix: "bench", FilePath: "api.go",
			Meta: map[string]any{
				"type": "http", "role": "provider", "line": i + 1,
				"contract_meta": map[string]any{"path": fmt.Sprintf("/api/%d", i)},
			},
		})
	}
	g.AddBatch(nodes, nil)
	for _, tc := range []struct {
		name string
		load func(graph.Store, string) *contracts.Registry
	}{
		{"legacy_full_repo", legacyLoadRegistryFromGraph},
		{"scoped_contracts", contracts.LoadRegistryFromGraph},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				reg := tc.load(g, "bench")
				if reg == nil || len(reg.All()) != contractCount {
					b.Fatal("contract fixture did not load completely")
				}
			}
		})
	}
}
