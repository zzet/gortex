package contracts_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Frozen pre-owner-loader baseline: full exact-repository node hydration and
// the original scalar decoder. Do not route through the new loader: its owner
// projection, fallback policy and exact-prefix checks are the behavior under
// measurement, so reusing it would invalidate the baseline comparison.
func legacyLoadRegistryFromGraph(g graph.Store, repoPrefix string) *contracts.Registry {
	if g == nil {
		return nil
	}
	all := g.GetRepoNodes(repoPrefix)
	if len(all) == 0 {
		return nil
	}
	reg := contracts.NewRegistry()
	for _, n := range all {
		if n == nil || n.Kind != graph.KindContract {
			continue
		}
		c := legacyContractFromNode(n)
		if c.ID == "" {
			continue
		}
		reg.Add(c)
	}
	if len(reg.All()) == 0 {
		return nil
	}
	return reg
}

func legacyContractFromNode(n *graph.Node) contracts.Contract {
	c := contracts.Contract{
		ID:         n.ID,
		FilePath:   n.FilePath,
		RepoPrefix: n.RepoPrefix,
	}
	if n.Meta == nil {
		return c
	}
	if v, ok := n.Meta["type"].(string); ok {
		c.Type = contracts.ContractType(v)
	}
	if v, ok := n.Meta["role"].(string); ok {
		c.Role = contracts.Role(v)
	}
	if v, ok := n.Meta["symbol_id"].(string); ok {
		c.SymbolID = v
	}
	if v, ok := n.Meta["line"].(int); ok {
		c.Line = v
	} else if v, ok := n.Meta["line"].(int64); ok {
		c.Line = int(v)
	}
	if v, ok := n.Meta["confidence"].(float64); ok {
		c.Confidence = v
	}
	c.WorkspaceID = n.WorkspaceID
	c.ProjectID = n.ProjectID
	if v, ok := n.Meta["contract_meta"].(map[string]any); ok && len(v) > 0 {
		c.Meta = v
	}
	return c
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

func TestLoadRegistrySparseScalarSymbolPresence(t *testing.T) {
	factories := map[string]func(*testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store { return openContractLoaderSQLite(t) },
	}
	for backend, factory := range factories {
		for _, explicitEmpty := range []bool{false, true} {
			name := "absent"
			if explicitEmpty {
				name = "explicit_empty"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				g := factory(t)
				owner := contracts.Contract{
					ID: "sparse-contract", Type: contracts.ContractGRPC, Role: contracts.RoleConsumer,
					SymbolID: "repo::client", FilePath: "repo/client.go", RepoPrefix: "repo",
					WorkspaceID: "workspace", ProjectID: "project", Confidence: 0.9,
					Meta: map[string]any{"service": "Users", "method": "Get"},
				}
				scalarMeta := map[string]any{
					"type": string(owner.Type), "role": string(owner.Role),
					"contract_meta": owner.Meta, "confidence": owner.Confidence,
				}
				if explicitEmpty {
					scalarMeta["symbol_id"] = ""
				}
				g.AddBatch([]*graph.Node{
					{ID: owner.SymbolID, Kind: graph.KindFunction, Name: "client", FilePath: owner.FilePath, RepoPrefix: owner.RepoPrefix},
					{ID: owner.ID, Kind: graph.KindContract, Name: owner.ID, FilePath: owner.FilePath,
						RepoPrefix: owner.RepoPrefix, WorkspaceID: owner.WorkspaceID, ProjectID: owner.ProjectID, Meta: scalarMeta},
				}, []*graph.Edge{{
					From: owner.SymbolID, To: owner.ID, Kind: graph.EdgeConsumes, FilePath: owner.FilePath,
					Meta: map[string]any{
						"contract_owner_repo_prefix": owner.RepoPrefix,
						"contract_owner_workspace":   owner.WorkspaceID,
						"contract_owner_project":     owner.ProjectID,
						"contract_owner_type":        string(owner.Type),
						"contract_owner_confidence":  owner.Confidence,
						"contract_owner_meta":        owner.Meta,
						"contract_owner_symbol_id":   owner.SymbolID,
					},
				}})
				require.Len(t, g.GetInEdges(owner.ID), 1, "a genuine source owner must be persisted")
				canonical := g.GetNodesByIDs([]string{owner.ID})[owner.ID]
				require.NotNil(t, canonical)
				_, symbolPresent := canonical.Meta["symbol_id"]
				require.Equal(t, explicitEmpty, symbolPresent, "fixture must preserve absence versus explicit empty")
				expected := []contracts.Contract{owner}
				if explicitEmpty {
					symbolLess := owner
					symbolLess.SymbolID = ""
					expected = append(expected, symbolLess)
				}
				for _, scoped := range []bool{false, true} {
					var loaded *contracts.Registry
					if scoped {
						loaded = contracts.LoadRegistryFromGraphWithScope(g, "repo", "workspace", "project")
					} else {
						loaded = contracts.LoadRegistryFromGraph(g, "repo")
					}
					require.NotNil(t, loaded)
					require.ElementsMatch(t, expected, loaded.ByID(owner.ID), "full records, scoped=%v", scoped)
				}
			})
		}
	}
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
					// The owner-loader also fixes the old nil-Meta early return
					// dropping sparse contract scope. Keep the benchmark decoder
					// frozen and express this intended correction only here.
					if sparse, ok := want["alpha::sparse"]; ok {
						sparse.WorkspaceID, sparse.ProjectID = "workspace", "project"
						want["alpha::sparse"] = sparse
					}
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
	spy := &contractLoaderProjectionSpy{Store: graph.New(), t: t, nodes: []*graph.Node{
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

// Deliberately erase native optional capabilities to exercise the documented
// conservative legacy policy without adding new errors to existing writers.
type opaqueOwnerLoaderStore struct{ graph.Store }

type boundedOwnerLoaderStore struct {
	graph.Store
	incomingCalls, largestIncoming int
}

func (store *boundedOwnerLoaderStore) GetRepoNodes(repo string) []*graph.Node {
	if repo != "" {
		panic("scoped owner loader must not hydrate all repository nodes")
	}
	return store.Store.GetRepoNodes(repo)
}

func (store *boundedOwnerLoaderStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	store.incomingCalls++
	store.largestIncoming = max(store.largestIncoming, len(ids))
	return store.Store.GetInEdgesByNodeIDs(ids)
}

func ownerLoaderRecords(t *testing.T, records []contracts.Contract) map[string]int {
	t.Helper()
	set := make(map[string]int, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		set[string(encoded)]++
	}
	return set
}

func ownerLoaderAll(store graph.Store, repo string) []contracts.Contract {
	if registry := contracts.LoadRegistryFromGraph(store, repo); registry != nil {
		return registry.All()
	}
	return nil
}

func ownerLoaderRecord(repo, workspace, role, file, symbol string) contracts.Contract {
	return contracts.Contract{
		ID: "env::OWNER_ROUNDTRIP", Type: contracts.ContractType("env"), Role: contracts.Role(role),
		RepoPrefix: repo, WorkspaceID: workspace, ProjectID: workspace + "-project",
		FilePath: file, SymbolID: symbol, Line: 7, Confidence: 0.75,
		Meta: map[string]any{"var": "OWNER_ROUNDTRIP", "nested": map[string]any{"values": []any{"one", "two"}}},
	}
}

func ownerLoaderNode(c contracts.Contract, marker string) *graph.Node {
	meta := map[string]any{
		"type": string(c.Type), "role": string(c.Role), "symbol_id": c.SymbolID,
		"line": c.Line, "confidence": c.Confidence, "contract_meta": c.Meta,
	}
	if marker != "" {
		meta[marker] = true
	}
	return &graph.Node{
		ID: c.ID, Kind: graph.KindContract, FilePath: c.FilePath, RepoPrefix: c.RepoPrefix,
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, Meta: meta,
	}
}

func ownerLoaderSourceAndEdge(c contracts.Contract, explicitSymbol bool) (*graph.Node, *graph.Edge) {
	from, kind := c.SymbolID, graph.KindFunction
	if from == "" {
		from, kind = c.FilePath, graph.KindFile
	}
	edgeKind := graph.EdgeProvides
	if string(c.Role) == "consumer" {
		edgeKind = graph.EdgeConsumes
	}
	meta := map[string]any{
		"contract_owner_repo_prefix": c.RepoPrefix,
		"contract_owner_workspace":   c.WorkspaceID, "contract_owner_project": c.ProjectID,
		"contract_owner_type": string(c.Type), "contract_owner_confidence": c.Confidence,
		"contract_owner_meta": c.Meta,
	}
	if explicitSymbol {
		meta["contract_owner_symbol_id"] = c.SymbolID
	}
	return &graph.Node{ID: from, Kind: kind, FilePath: c.FilePath, RepoPrefix: c.RepoPrefix, WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID},
		&graph.Edge{From: from, To: c.ID, Kind: edgeKind, FilePath: c.FilePath, Line: c.Line, Meta: meta}
}

func ownerLoaderFactories() map[string]func(*testing.T) graph.Store {
	return map[string]func(*testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		},
	}
}

func TestContractOwnerLoaderPreservesCrossRepoScope(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			provider := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/.env", "")
			consumer := ownerLoaderRecord("repo-b", "workspace-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
			aNode, aEdge := ownerLoaderSourceAndEdge(provider, true)
			bNode, bEdge := ownerLoaderSourceAndEdge(consumer, true)
			// B is the last scalar owner. A must still be discovered from its
			// repo-owned edge rather than from the canonical node's repo scope.
			store.AddBatch([]*graph.Node{aNode, bNode, ownerLoaderNode(consumer, "contract_owner_record")}, []*graph.Edge{aEdge, bEdge})
			require.Len(t, store.GetOutEdges(aNode.ID), 1, "file ownership fixture must pass real store admission")
			require.Len(t, store.GetOutEdges(bNode.ID), 1)
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{provider}), ownerLoaderRecords(t, ownerLoaderAll(store, "repo-a")))
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{consumer}), ownerLoaderRecords(t, ownerLoaderAll(store, "repo-b")))
			assert.Nil(t, contracts.LoadRegistryFromGraph(store, "unrelated"))
		})
	}
}

func TestContractOwnerFileRowsAreAcceptedByStores(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			provider := ownerLoaderRecord("repo", "workspace", "provider", "repo/provider.env", "")
			consumer := ownerLoaderRecord("repo", "workspace", "consumer", "repo/consumer.env", "")
			aNode, aEdge := ownerLoaderSourceAndEdge(provider, true)
			bNode, bEdge := ownerLoaderSourceAndEdge(consumer, true)
			store.AddBatch([]*graph.Node{aNode, bNode, ownerLoaderNode(consumer, "contract_owner_record")}, []*graph.Edge{aEdge, bEdge})
			require.Len(t, store.GetOutEdges(aNode.ID), 1)
			require.Len(t, store.GetOutEdges(bNode.ID), 1)
			rows := graph.ReadRepoEdgesByKinds(store, []string{"repo"}, []graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes})
			require.Len(t, rows, 2, "existing projections accept genuine file-owned records")
		})
	}
}

func TestContractOwnerLoaderDoesNotInheritSiblingMetadataForNilOwnerPayload(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			provider := ownerLoaderRecord("repo", "workspace", "provider", "repo/provider.go", "repo/provider.go::Provide")
			provider.Meta = nil
			provider.Confidence = 0
			consumer := ownerLoaderRecord("repo", "workspace", "consumer", "repo/consumer.go", "repo/consumer.go::Consume")
			aNode, aEdge := ownerLoaderSourceAndEdge(provider, true)
			bNode, bEdge := ownerLoaderSourceAndEdge(consumer, true)
			store.AddBatch([]*graph.Node{aNode, bNode, ownerLoaderNode(consumer, "contract_owner_record")}, []*graph.Edge{aEdge, bEdge})
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{provider, consumer}), ownerLoaderRecords(t, ownerLoaderAll(store, "repo")), "nil/zero owner values must not inherit last scalar sibling payload")
		})
	}
}

func TestContractOwnerLoaderLegacyUnionAndRemovedFallback(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		for _, marker := range []string{"", "contract_owner_record", "contract_owner_removed"} {
			name := marker
			if name == "" {
				name = "legacy_unmarked"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				store := factory(t)
				provider := ownerLoaderRecord("repo", "workspace", "provider", "repo/.env", "")
				consumer := ownerLoaderRecord("repo", "workspace", "consumer", "repo/use.go", "repo/use.go::Use")
				source, edge := ownerLoaderSourceAndEdge(consumer, false) // genuine legacy edge
				store.AddBatch([]*graph.Node{source, ownerLoaderNode(provider, marker)}, []*graph.Edge{edge})
				want := []contracts.Contract{consumer}
				if marker == "" {
					want = append(want, provider)
				}
				assert.Equal(t, ownerLoaderRecords(t, want), ownerLoaderRecords(t, ownerLoaderAll(store, "repo")))
			})
		}
	}
}

func TestContractOwnerLoaderEmptyPrefixRemainsExact(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			empty := ownerLoaderRecord("", "workspace", "provider", "plain.env", "")
			other := ownerLoaderRecord("other", "workspace", "consumer", "other/use.go", "other/use.go::Use")
			other.ID = "env::OTHER_NAMESPACE"
			emptySource, emptyEdge := ownerLoaderSourceAndEdge(empty, true)
			otherSource, otherEdge := ownerLoaderSourceAndEdge(other, true)
			store.AddBatch([]*graph.Node{emptySource, otherSource, ownerLoaderNode(empty, "contract_owner_record"), ownerLoaderNode(other, "contract_owner_record")}, []*graph.Edge{emptyEdge, otherEdge})
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{empty}), ownerLoaderRecords(t, ownerLoaderAll(store, "")))
		})
	}
}

func TestContractOwnerLoaderRepeatedRowsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	provider := ownerLoaderRecord("repo", "workspace", "provider", "repo/.env", "")
	consumer := ownerLoaderRecord("repo", "workspace", "consumer", "repo/consumer.env", "")
	aNode, aEdge := ownerLoaderSourceAndEdge(provider, true)
	bNode, bEdge := ownerLoaderSourceAndEdge(consumer, true)
	nodes := []*graph.Node{aNode, bNode, ownerLoaderNode(consumer, "contract_owner_record")}
	for range 3 {
		store.AddBatch(nodes, []*graph.Edge{aEdge, bEdge})
	}
	want := ownerLoaderRecords(t, []contracts.Contract{provider, consumer})
	assert.Equal(t, want, ownerLoaderRecords(t, ownerLoaderAll(store, "repo")), "same rows do not duplicate full records")
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	assert.Equal(t, want, ownerLoaderRecords(t, ownerLoaderAll(store, "repo")), "empty symbols and nested metadata survive reopen")
}

func TestContractOwnerLoaderNilAndSparseCompatibility(t *testing.T) {
	assert.Nil(t, contracts.LoadRegistryFromGraph(nil, "repo"))
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			assert.Nil(t, contracts.LoadRegistryFromGraph(store, "repo"))
			sparse := contracts.Contract{ID: "legacy::sparse", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", FilePath: "repo/source"}
			store.AddNode(&graph.Node{ID: sparse.ID, Kind: graph.KindContract, RepoPrefix: sparse.RepoPrefix, WorkspaceID: sparse.WorkspaceID, ProjectID: sparse.ProjectID, FilePath: sparse.FilePath})
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{sparse}), ownerLoaderRecords(t, ownerLoaderAll(store, "repo")))
		})
	}
}

func TestContractOwnerLoaderOpaqueAdapterConservativelyOmitsAmbiguousScalar(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			provider := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/.env", "")
			consumer := ownerLoaderRecord("repo-b", "workspace-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
			source, edge := ownerLoaderSourceAndEdge(consumer, false)
			store.AddBatch([]*graph.Node{source, ownerLoaderNode(provider, "")}, []*graph.Edge{edge})
			opaque := opaqueOwnerLoaderStore{store}
			// This is an intentional conservative limitation, not full parity:
			// legitimate scalar A cannot be distinguished from removed A in a
			// capability-erasing adapter, so a surviving B suppresses A already
			// before deletion. Native stores preserve A via exact invalidation.
			assert.Empty(t, ownerLoaderAll(opaque, "repo-a"))
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{consumer}), ownerLoaderRecords(t, ownerLoaderAll(opaque, "repo-b")))
			scalarOnly := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/unique.env", "")
			scalarOnly.ID = "env::SCALAR_ONLY"
			store.AddNode(ownerLoaderNode(scalarOnly, ""))
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{scalarOnly}), ownerLoaderRecords(t, ownerLoaderAll(opaque, "repo-a")), "unambiguous scalar-only compatibility remains")
		})
	}
}

func TestContractOwnerLoaderOpaqueIncomingProbeIsBounded(t *testing.T) {
	base := graph.New()
	for i := range 257 {
		c := ownerLoaderRecord("repo", "workspace", "provider", fmt.Sprintf("repo/%d.env", i), "")
		c.ID = fmt.Sprintf("env::SCALAR_%d", i)
		base.AddNode(ownerLoaderNode(c, ""))
	}
	store := &boundedOwnerLoaderStore{Store: base}
	require.Len(t, ownerLoaderAll(store, "repo"), 257)
	assert.Equal(t, 3, store.incomingCalls, "one bounded incoming query per 128 legacy candidate IDs")
	assert.LessOrEqual(t, store.largestIncoming, 128)
}

func TestContractOwnerLoaderPreservesConcreteNumericMetadata(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			var durable *store_sqlite.Store
			var databasePath string
			if backend == "sqlite" {
				databasePath = filepath.Join(t.TempDir(), "graph.sqlite")
				var err error
				durable, err = store_sqlite.Open(databasePath)
				require.NoError(t, err)
				store = durable
				t.Cleanup(func() {
					if durable != nil {
						require.NoError(t, durable.Close())
					}
				})
			}
			left := ownerLoaderRecord("repo", "workspace", "provider", "repo/shared.env", "")
			left.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "numeric": int(7)}
			right := left
			right.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "numeric": float64(7)}
			want := []contracts.Contract{left, right}
			leftJSON, err := json.Marshal(left)
			require.NoError(t, err)
			rightJSON, err := json.Marshal(right)
			require.NoError(t, err)
			require.Equal(t, string(leftJSON), string(rightJSON), "the proposed JSON-only record key must actually collide")
			require.False(t, reflect.DeepEqual(left, right), "the complete contracts retain a real concrete-type distinction")
			leftSource, leftEdge := ownerLoaderSourceAndEdge(left, true)
			rightSource, rightEdge := ownerLoaderSourceAndEdge(right, true)
			// Two genuine same-repo KindFile sources make the ownership edges
			// structurally distinct. Their record path/line and explicit empty
			// symbol ID remain identical; the scoped reader admits both.
			leftSource.ID, leftSource.FilePath = "repo/left.env", "repo/left.env"
			rightSource.ID, rightSource.FilePath = "repo/right.env", "repo/right.env"
			leftEdge.From, rightEdge.From = leftSource.ID, rightSource.ID
			store.AddBatch([]*graph.Node{leftSource, rightSource, ownerLoaderNode(left, "contract_owner_record")}, []*graph.Edge{leftEdge, rightEdge})
			verifyStored := func(t *testing.T) {
				t.Helper()
				for _, source := range []*graph.Node{leftSource, rightSource} {
					actual := store.GetNode(source.ID)
					require.NotNil(t, actual)
					require.Equal(t, graph.KindFile, actual.Kind)
					require.Equal(t, "repo", actual.RepoPrefix)
				}
				rows := graph.ReadRepoEdgesByKinds(store, []string{"repo"}, []graph.EdgeKind{graph.EdgeProvides})
				require.Len(t, rows, 2, "both persisted rows must be admitted by the actual repository projection")
				edges := store.GetInEdges(left.ID)
				require.Len(t, edges, 2)
				seen := make(map[string]bool)
				for _, edge := range edges {
					require.Equal(t, left.FilePath, edge.FilePath)
					require.Equal(t, left.Line, edge.Line)
					symbol, present := edge.Meta["contract_owner_symbol_id"].(string)
					require.True(t, present)
					require.Empty(t, symbol)
					metadata, ok := edge.Meta["contract_owner_meta"].(map[string]any)
					require.True(t, ok)
					switch edge.From {
					case leftSource.ID:
						require.IsType(t, int(0), metadata["numeric"], "native store codec must preserve int, not normalize it to float64")
						require.True(t, reflect.DeepEqual(left.Meta, metadata))
					case rightSource.ID:
						require.IsType(t, float64(0), metadata["numeric"])
						require.True(t, reflect.DeepEqual(right.Meta, metadata))
					default:
						t.Fatalf("unexpected owner source %q", edge.From)
					}
					seen[edge.From] = true
				}
				require.Len(t, seen, 2)
			}
			if !t.Run("actual_storage_codec_prerequisites", func(t *testing.T) {
				verifyStored(t)
				if durable != nil {
					require.NoError(t, durable.Close())
					durable = nil
					durable, err = store_sqlite.Open(databasePath)
					require.NoError(t, err)
					store = durable
					verifyStored(t)
				}
			}) {
				return // do not claim a loader regression from an invalid fixture
			}
			t.Run("public_loader_full_record_multiset", func(t *testing.T) {
				registry := contracts.LoadRegistryFromGraph(store, "repo")
				require.NotNil(t, registry)
				actual := registry.ByID(left.ID)
				require.Len(t, actual, 2, "exact-record dedup must not collapse JSON-equal concrete metadata types")
				seen := make([]bool, len(want))
				for _, record := range actual {
					found := -1
					for i, expected := range want {
						if reflect.DeepEqual(record, expected) {
							found = i
							break
						}
					}
					require.NotEqual(t, -1, found, "compare complete records with concrete Go types, never JSON-only multisets")
					require.False(t, seen[found], "do not substitute a duplicate for the distinct numeric record")
					seen[found] = true
				}
				// All deliberately coalesces ID/file/symbol/role; this test does
				// not change Registry.All's separate established contract.
				require.Len(t, registry.All(), 1)
			})
		})
	}
}

// Preserve only the mandatory bounded deletion capability needed by the
// compatibility writer. Native owner replacement and scalar invalidation remain
// hidden, as do native read projections. A bare Store wrapper cannot delete.
type opaqueOwnerLifecycleStore struct{ graph.Store }

func (store opaqueOwnerLifecycleStore) RemoveEdgesExact(edges []*graph.Edge) int {
	remover, ok := store.Store.(graph.ExactEdgeBatchRemover)
	if !ok {
		panic("lifecycle fixture requires exact edge batch removal")
	}
	return remover.RemoveEdgesExact(edges)
}

func (store opaqueOwnerLifecycleStore) EvictContractNodesByIDs(ids []string) (int, int) {
	nodes, edges, supported := graph.EvictContractNodesByIDs(store.Store, ids)
	if !supported {
		panic("lifecycle fixture requires bounded contract-node eviction")
	}
	return nodes, edges
}

func TestContractOwnerOpaqueReplacementLifecycleDoesNotResurrect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	provider := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/.env", "")
	consumer := ownerLoaderRecord("repo-b", "workspace-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
	consumer.Line, consumer.Confidence = 29, 0.5
	consumer.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "nested": map[string]any{"values": []any{"only-b", "preserve"}}}
	unrelated := ownerLoaderRecord("repo-c", "workspace-c", "provider", "repo-c/.env", "")
	unrelated.ID = "env::OPAQUE_UNRELATED"
	sourceB, edgeB := ownerLoaderSourceAndEdge(consumer, false)
	store.AddBatch([]*graph.Node{
		{ID: provider.FilePath, Kind: graph.KindFile, RepoPrefix: provider.RepoPrefix, FilePath: provider.FilePath},
		sourceB, ownerLoaderNode(provider, ""), ownerLoaderNode(unrelated, ""),
	}, []*graph.Edge{edgeB})
	require.Len(t, store.GetOutEdges(sourceB.ID), 1, "fixture must persist the surviving legacy B owner")
	opaque := opaqueOwnerLifecycleStore{store}
	// Keep the same capability-erasing adapter after reopen. Switching to a
	// native handle would change the deliberately conservative legacy policy.
	reopen := func() {
		t.Helper()
		require.NoError(t, store.Close())
		store = nil
		store, err = store_sqlite.Open(path)
		require.NoError(t, err)
		opaque = opaqueOwnerLifecycleStore{store}
	}
	check := func(stage string, wantB bool) {
		t.Helper()
		assert.Empty(t, ownerLoaderAll(opaque, provider.RepoPrefix), "%s: ambiguous/removed A must not hydrate", stage)
		var expectedB []contracts.Contract
		if wantB {
			expectedB = []contracts.Contract{consumer}
		}
		assert.Equal(t, ownerLoaderRecords(t, expectedB), ownerLoaderRecords(t, ownerLoaderAll(opaque, consumer.RepoPrefix)), "%s: retain B's entire record, not merely its ID", stage)
		assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{unrelated}), ownerLoaderRecords(t, ownerLoaderAll(opaque, unrelated.RepoPrefix)), "%s: unrelated scalar-only compatibility survives", stage)
	}
	check("before removal", true)
	_, err = graph.ReplaceContractOwners(opaque, graph.ContractOwnerReplacement{
		RepoPrefix: provider.RepoPrefix, FilePaths: []string{provider.FilePath}, TouchedNodeIDs: []string{provider.ID},
	})
	require.NoError(t, err)
	require.NotNil(t, store.GetNode(provider.ID), "surviving B must retain the shared canonical node")
	check("after A removal", true)
	reopen()
	check("after A removal and reopen", true)
	// Refresh B through the same compatibility path without replacing the
	// canonical scalar. This must neither resurrect A nor duplicate B's record.
	_, err = graph.ReplaceContractOwners(opaque, graph.ContractOwnerReplacement{
		RepoPrefix: consumer.RepoPrefix, FilePaths: []string{consumer.FilePath},
		TouchedNodeIDs: []string{consumer.ID}, Edges: []*graph.Edge{edgeB},
	})
	require.NoError(t, err)
	require.Len(t, store.GetOutEdges(sourceB.ID), 1)
	check("after B refresh", true)
	_, err = graph.ReplaceContractOwners(opaque, graph.ContractOwnerReplacement{
		RepoPrefix: consumer.RepoPrefix, FilePaths: []string{consumer.FilePath}, TouchedNodeIDs: []string{consumer.ID},
	})
	require.NoError(t, err)
	assert.Nil(t, store.GetNode(provider.ID), "removing the last real owner prunes the shared canonical node")
	assert.Empty(t, store.GetOutEdges(sourceB.ID))
	check("after final B removal", false)
	reopen()
	assert.Nil(t, store.GetNode(provider.ID), "pruning survives reopen")
	check("after final removal and reopen", false)
}

// Capture complete stored rows, including encoded metadata, as a multiset.
// No schema-specific omitted columns or count-only comparison can hide damage.
func ownerGenerationRowMultiset(t *testing.T, db *sql.DB, query string, args ...any) map[string]int {
	t.Helper()
	rows, err := db.Query(query, args...)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	set := make(map[string]int)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		for i, value := range values {
			if blob, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), blob...)
			}
		}
		encoded, err := json.Marshal(values)
		require.NoError(t, err)
		set[string(encoded)]++
	}
	require.NoError(t, rows.Err())
	return set
}

func TestContractOwnerLoaderAndRemovalIsolateDistinctGenerationPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	const canonicalID = "env::DISTINCT_GENERATION_OWNER"
	records := func(workspace, tag string, line int, confidence float64) (contracts.Contract, contracts.Contract) {
		provider := ownerLoaderRecord("repo-a", workspace, "provider", "repo-a/.env", "")
		provider.ID, provider.Line, provider.Confidence = canonicalID, line, confidence
		provider.Meta = map[string]any{"var": "DISTINCT_GENERATION_OWNER", "generation": tag, "nested": map[string]any{"values": []any{tag, "provider"}}}
		consumer := ownerLoaderRecord("repo-b", workspace+"-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
		consumer.ID, consumer.Line, consumer.Confidence = canonicalID, line+1, confidence/2
		consumer.Meta = map[string]any{"var": "DISTINCT_GENERATION_OWNER", "generation": tag, "nested": map[string]any{"values": []any{tag, "consumer"}}}
		return provider, consumer
	}
	seed := func(provider, consumer contracts.Contract) {
		t.Helper()
		aNode, aEdge := ownerLoaderSourceAndEdge(provider, true)
		bNode, bEdge := ownerLoaderSourceAndEdge(consumer, true)
		// Legacy scalar A exercises exact invalidation when A is removed while
		// B retains the shared ID. Both generations use identical row identities.
		store.AddBatch([]*graph.Node{aNode, bNode, ownerLoaderNode(provider, "")}, []*graph.Edge{aEdge, bEdge})
		require.Len(t, store.GetOutEdges(aNode.ID), 1)
		require.Len(t, store.GetOutEdges(bNode.ID), 1)
	}
	historicalA, historicalB := records("historical-workspace", "generation-7", 71, 0.875)
	selectedA, selectedB := records("selected-workspace", "generation-0", 3, 0.25)
	require.NotEqual(t, ownerLoaderRecords(t, []contracts.Contract{historicalA, historicalB}), ownerLoaderRecords(t, []contracts.Contract{selectedA, selectedB}), "fixture must distinguish metadata, line, confidence and ownership across generations")
	seed(historicalA, historicalB)
	require.NoError(t, store.Close())
	store = nil
	// Move only this disposable, stopped fixture to a sibling generation,
	// following the existing generation regression pattern. Never a daemon store.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE nodes SET view_gen = 7")
	require.NoError(t, err)
	_, err = db.Exec("UPDATE edges SET view_gen = 7")
	require.NoError(t, err)
	beforeNodes := ownerGenerationRowMultiset(t, db, "SELECT * FROM nodes WHERE view_gen = ?", 7)
	beforeEdges := ownerGenerationRowMultiset(t, db, "SELECT * FROM edges WHERE view_gen = ?", 7)
	require.Len(t, beforeNodes, 3)
	require.Len(t, beforeEdges, 2, "both historical ownership payloads must be present")
	require.NoError(t, db.Close())
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	seed(selectedA, selectedB)
	checkSelected := func(stage string, wantA bool) {
		t.Helper()
		var expectedA []contracts.Contract
		if wantA {
			expectedA = []contracts.Contract{selectedA}
		}
		assert.Equal(t, ownerLoaderRecords(t, expectedA), ownerLoaderRecords(t, ownerLoaderAll(store, "repo-a")), "%s: only selected-generation A records may hydrate", stage)
		assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{selectedB}), ownerLoaderRecords(t, ownerLoaderAll(store, "repo-b")), "%s: B must retain its selected-generation full record", stage)
	}
	checkSelected("before removal", true)
	_, err = graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{
		RepoPrefix: selectedA.RepoPrefix, FilePaths: []string{selectedA.FilePath}, TouchedNodeIDs: []string{canonicalID},
	})
	require.NoError(t, err)
	require.NotNil(t, store.GetNode(canonicalID), "selected B retains the shared canonical ID")
	checkSelected("after selected A removal", false)
	require.NoError(t, store.Close())
	store = nil
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	checkSelected("after selected A removal and reopen", false)
	require.NoError(t, store.Close())
	store = nil
	db, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, beforeNodes, ownerGenerationRowMultiset(t, db, "SELECT * FROM nodes WHERE view_gen = ?", 7), "every sibling node column remains unchanged")
	assert.Equal(t, beforeEdges, ownerGenerationRowMultiset(t, db, "SELECT * FROM edges WHERE view_gen = ?", 7), "every sibling edge column, including exact encoded ownership metadata, remains unchanged")
}

// oldOwnerReplacerStore models an implementation exposing the old optional
// replacement interface without the new conditional scalar invalidator.
// Its exact replacement delegates to the supported compatibility writer;
// this sequential test makes no stronger concurrent-atomicity claim.
type oldOwnerReplacerStore struct{ opaqueOwnerLifecycleStore }

func (store oldOwnerReplacerStore) ReplaceContractOwners(replacement graph.ContractOwnerReplacement) (graph.ContractOwnerReplaceResult, error) {
	return graph.ReplaceContractOwners(store.opaqueOwnerLifecycleStore, replacement)
}

// A callable invalidator does not prove that a custom replacement invokes it.
// This adapter exposes the correct native Graph operation but retains the old
// replacement implementation above, which bypasses that operation entirely.
type oldReplacerWithUnusedInvalidator struct {
	oldOwnerReplacerStore
	invalidator *graph.Graph
}

func (store oldReplacerWithUnusedInvalidator) InvalidateContractOwnerScalars(replacement graph.ContractOwnerReplacement) int {
	return store.invalidator.InvalidateContractOwnerScalars(replacement)
}

type oldReplacerWithFalseGuarantee struct{ oldOwnerReplacerStore }

func (store oldReplacerWithFalseGuarantee) ContractOwnerScalarLivenessGuaranteed() bool { return false }

func TestContractOwnerLegacyReplacerDoesNotImplyScalarInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		factory           func(*testing.T) graph.Store
		exposeInvalidator bool
		falseGuarantee    bool
	}{
		{name: "memory", factory: func(t *testing.T) graph.Store { return graph.New() }},
		{name: "sqlite", factory: func(t *testing.T) graph.Store { return openContractLoaderSQLite(t) }},
		{name: "memory_with_unused_invalidator", factory: func(t *testing.T) graph.Store { return graph.New() }, exposeInvalidator: true},
		{name: "memory_with_false_guarantee", factory: func(t *testing.T) graph.Store { return graph.New() }, falseGuarantee: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.factory(t)
			oldReplacer := oldOwnerReplacerStore{opaqueOwnerLifecycleStore{raw}}
			var store graph.Store = oldReplacer
			if tc.exposeInvalidator {
				memory, ok := raw.(*graph.Graph)
				require.True(t, ok)
				store = oldReplacerWithUnusedInvalidator{oldOwnerReplacerStore: oldReplacer, invalidator: memory}
			}
			if tc.falseGuarantee {
				store = oldReplacerWithFalseGuarantee{oldReplacer}
			}
			if guarantee, ok := store.(interface{ ContractOwnerScalarLivenessGuaranteed() bool }); ok {
				require.True(t, tc.falseGuarantee)
				require.False(t, guarantee.ContractOwnerScalarLivenessGuaranteed())
			} else {
				require.False(t, tc.falseGuarantee)
			}
			_, replacementSupported := any(store).(graph.ContractOwnerReplacer)
			require.True(t, replacementSupported, "fixture must expose the existing optional replacement interface")
			_, scalarInvalidationSupported := any(store).(graph.ContractOwnerScalarInvalidator)
			require.Equal(t, tc.exposeInvalidator, scalarInvalidationSupported, "fixture must expose only the requested callable capability")

			provider := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/.env", "")
			consumer := ownerLoaderRecord("repo-b", "workspace-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
			consumer.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "nested": map[string]any{"survivor": "B"}}
			sourceB, edgeB := ownerLoaderSourceAndEdge(consumer, false)
			raw.AddBatch([]*graph.Node{sourceB, ownerLoaderNode(provider, "")}, []*graph.Edge{edgeB})
			_, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{
				RepoPrefix: provider.RepoPrefix, FilePaths: []string{provider.FilePath}, TouchedNodeIDs: []string{provider.ID},
			})
			require.NoError(t, err)
			canonical := raw.GetNode(provider.ID)
			require.NotNil(t, canonical, "B still owns the shared canonical")
			require.NotEqual(t, true, canonical.Meta["contract_owner_removed"], "old replacement did not add the new marker")
			require.Len(t, raw.GetInEdges(provider.ID), 1, "B ownership survives")

			// Optional atomic replacement alone says nothing about this new
			// legacy marker. The loader must apply its conservative fallback
			// policy instead of reviving A from the now-ambiguous scalar.
			assert.Empty(t, ownerLoaderAll(store, provider.RepoPrefix), "old Replacer capability must not certify removed A")
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{consumer}),
				ownerLoaderRecords(t, ownerLoaderAll(store, consumer.RepoPrefix)), "full B owner payload remains recoverable")
		})
	}
}

func TestContractOwnerLoaderPreservesDistinctLinesAndOwnerPayload(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			store := factory(t)
			first := ownerLoaderRecord("repo", "workspace", "provider", "repo/use.go", "repo/use.go::Use")
			second := first
			second.Line = first.Line + 10
			second.Confidence = 0.25
			second.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "payload": "second occurrence"}
			source, edgeA := ownerLoaderSourceAndEdge(first, true)
			_, edgeB := ownerLoaderSourceAndEdge(second, true)
			stale := first
			stale.Confidence = 0.5
			stale.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "payload": "obsolete scalar"}
			store.AddBatch([]*graph.Node{source, ownerLoaderNode(stale, "")}, []*graph.Edge{edgeA, edgeB})
			require.Len(t, store.GetOutEdges(source.ID), 2, "the existing store admits both line-distinct owners")
			loaded := contracts.LoadRegistryFromGraph(store, "repo")
			require.NotNil(t, loaded)
			got := loaded.ByID(first.ID)
			assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{first, second}), ownerLoaderRecords(t, got), "owner rows are complete records; an obsolete scalar for the first identity is not a third record")
		})
	}
}

func TestContractRegistryRetainsDistinctRecordPayloads(t *testing.T) {
	first := ownerLoaderRecord("repo", "workspace", "provider", "repo/use.go", "repo/use.go::Use")
	second := first
	second.Line++
	third := first
	third.Confidence = 0.25
	third.Meta = map[string]any{"different": "same-line payload"}
	registry := contracts.NewRegistry()
	registry.Add(first)
	registry.Add(second)
	registry.Add(third)
	require.Equal(t, ownerLoaderRecords(t, []contracts.Contract{first, second, third}), ownerLoaderRecords(t, registry.ByID(first.ID)))
	require.Equal(t, ownerLoaderRecords(t, []contracts.Contract{first, second, third}), ownerLoaderRecords(t, registry.ByWorkspace(first.WorkspaceID)))
	require.Equal(t, ownerLoaderRecords(t, []contracts.Contract{first}), ownerLoaderRecords(t, registry.All()), "All intentionally keeps the first logical ID/file/symbol/role record")
}

func TestContractOwnerRemovalPreservesOtherFileLegacyScalar(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				durable, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, durable.Close()) })
				store = durable
			}
			const id = "env::REVERSE_LEGACY"
			const scalarFile = "repo-b/provider.env"
			const edgeFile = "repo-a/consumer.go"
			scalar := &graph.Node{ID: id, Name: id, Kind: graph.KindContract, FilePath: scalarFile, RepoPrefix: "repo-b", WorkspaceID: "workspace-b", ProjectID: "project-b", Meta: map[string]any{"type": "env", "role": "provider", "symbol_id": "", "line": 2, "confidence": 1.0, "contract_meta": map[string]any{"var": "REVERSE_LEGACY", "scalar_only": true}}}
			source := &graph.Node{ID: edgeFile + "::Consume", Name: "Consume", Kind: graph.KindFunction, FilePath: edgeFile, RepoPrefix: "repo-a"}
			store.AddBatch([]*graph.Node{scalar, source}, []*graph.Edge{{From: source.ID, To: id, Kind: graph.EdgeConsumes, FilePath: edgeFile, Line: 3, Meta: map[string]any{"contract_owner_repo_prefix": "repo-a", "contract_owner_type": "env", "contract_owner_symbol_id": source.ID, "contract_owner_meta": map[string]any{"var": "REVERSE_LEGACY"}}}})
			before := contracts.LoadRegistryFromGraph(store, "repo-b")
			require.NotNil(t, before, "the unmarked scalar-only sibling is a real recoverable legacy record")
			require.Len(t, before.All(), 1)
			result, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{RepoPrefix: "repo-a", FilePaths: []string{edgeFile}, TouchedNodeIDs: []string{id}})
			require.NoError(t, err)
			assert.Equal(t, 1, result.EdgesRemoved)
			assert.Zero(t, result.NodesRemoved, "removing A's record must not prune the intact legacy scalar B")
			assert.NotNil(t, store.GetNode(id), "B is still a known record even though it has no owner edge")
			if after := contracts.LoadRegistryFromGraph(store, "repo-b"); assert.NotNil(t, after) {
				assert.Equal(t, before.All(), after.All(), "preserve the exact legacy scalar payload/scope")
			}
			_, err = graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{RepoPrefix: "repo-b", FilePaths: []string{scalarFile}, TouchedNodeIDs: []string{id}})
			require.NoError(t, err)
			assert.Nil(t, store.GetNode(id), "deleting B's own scalar file must still remove the last record")
		})
	}
}

func TestContractOwnerLegacyAdaptersReplacementLifecycleDoesNotResurrect(t *testing.T) {
	for _, tc := range []struct {
		name              string
		durable           bool
		exposeInvalidator bool
		falseGuarantee    bool
	}{
		{name: "memory"},
		{name: "memory_with_unused_invalidator", exposeInvalidator: true},
		{name: "memory_with_false_guarantee", falseGuarantee: true},
		{name: "sqlite", durable: true},
		{name: "sqlite_with_false_guarantee", durable: true, falseGuarantee: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw graph.Store = graph.New()
			var durable *store_sqlite.Store
			databasePath := filepath.Join(t.TempDir(), "graph.sqlite")
			if tc.durable {
				var err error
				durable, err = store_sqlite.Open(databasePath)
				require.NoError(t, err)
				raw = durable
				t.Cleanup(func() {
					if durable != nil {
						require.NoError(t, durable.Close())
					}
				})
			}
			makeAdapter := func() graph.Store {
				t.Helper()
				old := oldOwnerReplacerStore{opaqueOwnerLifecycleStore{raw}}
				var adapter graph.Store = old
				if tc.exposeInvalidator {
					memory, ok := raw.(*graph.Graph)
					require.True(t, ok)
					adapter = oldReplacerWithUnusedInvalidator{oldOwnerReplacerStore: old, invalidator: memory}
				}
				if tc.falseGuarantee {
					adapter = oldReplacerWithFalseGuarantee{old}
				}
				_, replacementSupported := adapter.(graph.ContractOwnerReplacer)
				require.True(t, replacementSupported, "retain the old optional dispatch capability")
				_, invalidationSupported := adapter.(graph.ContractOwnerScalarInvalidator)
				require.Equal(t, tc.exposeInvalidator, invalidationSupported)
				if guarantee, ok := adapter.(interface{ ContractOwnerScalarLivenessGuaranteed() bool }); ok {
					require.True(t, tc.falseGuarantee)
					require.False(t, guarantee.ContractOwnerScalarLivenessGuaranteed())
				} else {
					require.False(t, tc.falseGuarantee)
				}
				return adapter
			}
			adapter := makeAdapter()
			provider := ownerLoaderRecord("repo-a", "workspace-a", "provider", "repo-a/.env", "")
			consumer := ownerLoaderRecord("repo-b", "workspace-b", "consumer", "repo-b/use.go", "repo-b/use.go::Use")
			consumer.Line, consumer.Confidence = 29, 0.5
			consumer.Meta = map[string]any{"var": "OWNER_ROUNDTRIP", "nested": map[string]any{"values": []any{"only-b", "preserve"}}}
			unrelated := ownerLoaderRecord("repo-c", "workspace-c", "provider", "repo-c/.env", "")
			unrelated.ID = "env::OLD_REPLACER_UNRELATED"
			sourceB, edgeB := ownerLoaderSourceAndEdge(consumer, false)
			raw.AddBatch([]*graph.Node{
				{ID: provider.FilePath, Kind: graph.KindFile, RepoPrefix: provider.RepoPrefix, FilePath: provider.FilePath},
				sourceB, ownerLoaderNode(provider, ""), ownerLoaderNode(unrelated, ""),
			}, []*graph.Edge{edgeB})
			require.Len(t, raw.GetOutEdges(sourceB.ID), 1, "the real store must admit surviving legacy B")
			check := func(stage string, wantB bool) {
				t.Helper()
				assert.Empty(t, ownerLoaderAll(adapter, provider.RepoPrefix), "%s: ambiguous/removed A must not hydrate", stage)
				var expectedB []contracts.Contract
				if wantB {
					expectedB = []contracts.Contract{consumer}
				}
				assert.Equal(t, ownerLoaderRecords(t, expectedB), ownerLoaderRecords(t, ownerLoaderAll(adapter, consumer.RepoPrefix)), "%s: preserve B's complete record", stage)
				assert.Equal(t, ownerLoaderRecords(t, []contracts.Contract{unrelated}), ownerLoaderRecords(t, ownerLoaderAll(adapter, unrelated.RepoPrefix)), "%s: unrelated unambiguous scalar survives", stage)
			}
			reopen := func() {
				t.Helper()
				require.True(t, tc.durable)
				require.NoError(t, durable.Close())
				durable = nil
				var err error
				durable, err = store_sqlite.Open(databasePath)
				require.NoError(t, err)
				raw = durable
				adapter = makeAdapter() // preserve exactly the same erased capabilities
			}
			check("before removal", true)
			_, err := graph.ReplaceContractOwners(adapter, graph.ContractOwnerReplacement{
				RepoPrefix: provider.RepoPrefix, FilePaths: []string{provider.FilePath}, TouchedNodeIDs: []string{provider.ID},
			})
			require.NoError(t, err)
			require.NotNil(t, raw.GetNode(provider.ID), "B must retain the shared canonical after A removal")
			check("after A removal", true)
			if tc.durable {
				reopen()
				check("after A removal and reopen", true)
			}
			_, err = graph.ReplaceContractOwners(adapter, graph.ContractOwnerReplacement{
				RepoPrefix: consumer.RepoPrefix, FilePaths: []string{consumer.FilePath},
				TouchedNodeIDs: []string{consumer.ID}, Edges: []*graph.Edge{edgeB},
			})
			require.NoError(t, err)
			require.Len(t, raw.GetOutEdges(sourceB.ID), 1, "B refresh must preserve one full owner record")
			check("after B refresh", true)
			_, err = graph.ReplaceContractOwners(adapter, graph.ContractOwnerReplacement{
				RepoPrefix: consumer.RepoPrefix, FilePaths: []string{consumer.FilePath}, TouchedNodeIDs: []string{consumer.ID},
			})
			require.NoError(t, err)
			assert.Nil(t, raw.GetNode(provider.ID), "removing final B must prune the shared canonical")
			assert.Empty(t, raw.GetOutEdges(sourceB.ID))
			check("after final B removal", false)
			if tc.durable {
				reopen()
				assert.Nil(t, raw.GetNode(provider.ID), "final canonical pruning must survive reopen")
				check("after final removal and reopen", false)
			}
		})
	}
}

func TestContractOwnerLegacyAdaptersPreserveUntouchedScalarFrontier(t *testing.T) {
	for _, tc := range []struct {
		name              string
		factory           func(*testing.T) graph.Store
		exposeInvalidator bool
		falseGuarantee    bool
	}{
		{name: "memory", factory: func(t *testing.T) graph.Store { return graph.New() }},
		{name: "memory_with_unused_invalidator", factory: func(t *testing.T) graph.Store { return graph.New() }, exposeInvalidator: true},
		{name: "memory_with_false_guarantee", factory: func(t *testing.T) graph.Store { return graph.New() }, falseGuarantee: true},
		{name: "sqlite", factory: func(t *testing.T) graph.Store { return openContractLoaderSQLite(t) }},
		{name: "sqlite_with_false_guarantee", factory: func(t *testing.T) graph.Store { return openContractLoaderSQLite(t) }, falseGuarantee: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.factory(t)
			old := oldOwnerReplacerStore{opaqueOwnerLifecycleStore{raw}}
			var adapter graph.Store = old
			if tc.exposeInvalidator {
				memory, ok := raw.(*graph.Graph)
				require.True(t, ok)
				adapter = oldReplacerWithUnusedInvalidator{oldOwnerReplacerStore: old, invalidator: memory}
			}
			if tc.falseGuarantee {
				adapter = oldReplacerWithFalseGuarantee{old}
			}
			scalar := ownerLoaderRecord("repo-b", "workspace-b", "provider", "repo-b/untouched.env", "")
			scalar.ID = "env::UNTOUCHED_SCALAR_FRONTIER"
			expectedNode := ownerLoaderNode(ownerLoaderRecord("repo-b", "workspace-b", "provider", "repo-b/untouched.env", ""), "")
			expectedNode.ID = scalar.ID // independently allocated expected metadata
			const unrelatedFile = "repo-a/refresh.go"
			raw.AddBatch([]*graph.Node{
				{ID: unrelatedFile, Kind: graph.KindFile, RepoPrefix: "repo-a", FilePath: unrelatedFile},
				ownerLoaderNode(scalar, ""),
			}, nil)
			require.Empty(t, raw.GetInEdges(scalar.ID), "this is genuinely scalar-only, not a hidden surviving ownership edge")
			before := contracts.LoadRegistryFromGraph(adapter, scalar.RepoPrefix)
			require.NotNil(t, before)
			require.Equal(t, ownerLoaderRecords(t, []contracts.Contract{scalar}), ownerLoaderRecords(t, before.ByID(scalar.ID)))
			result, err := graph.ReplaceContractOwners(adapter, graph.ContractOwnerReplacement{
				RepoPrefix: "repo-a", FilePaths: []string{unrelatedFile}, TouchedNodeIDs: []string{scalar.ID},
			})
			require.NoError(t, err)
			assert.Zero(t, result.EdgesRemoved, "no owner was removed in this operation")
			assert.Zero(t, result.NodesRemoved, "a touched ID alone cannot invalidate an outside-frontier legacy scalar")
			assert.Equal(t, expectedNode, raw.GetNode(scalar.ID), "preserve every node field and nested metadata value")
			after := contracts.LoadRegistryFromGraph(adapter, scalar.RepoPrefix)
			if assert.NotNil(t, after) {
				assert.Equal(t, ownerLoaderRecords(t, before.ByID(scalar.ID)), ownerLoaderRecords(t, after.ByID(scalar.ID)), "preserve the complete untouched scalar record")
			}
		})
	}
}

// The observed entrypoint must retain complete owner/scalar content across
// bounded refetch/liveness batches, and each call must report its own counts.
func TestLoadRegistryWithStatsPreservesRecordsAndBounds(t *testing.T) {
	for backend, factory := range ownerLoaderFactories() {
		t.Run(backend, func(t *testing.T) {
			base := factory(t)
			const records = 129
			for i := range records {
				owner := ownerLoaderRecord("repo", "workspace", "provider", fmt.Sprintf("repo/%d.go", i), fmt.Sprintf("repo::symbol-%d", i))
				owner.ID = fmt.Sprintf("env::OWNER_%d", i)
				source, edge := ownerLoaderSourceAndEdge(owner, true)
				canonical := ownerLoaderNode(owner, "contract_owner_record")
				canonical.RepoPrefix = "other" // Owner targets require exact refetch.
				scalar := ownerLoaderRecord("repo", "workspace", "consumer", fmt.Sprintf("repo/%d.env", i), "")
				scalar.ID = fmt.Sprintf("env::SCALAR_%d", i)
				base.AddBatch([]*graph.Node{source, canonical, ownerLoaderNode(scalar, "")}, []*graph.Edge{edge})
			}
			for _, opaque := range []bool{false, true} {
				store := base
				if opaque {
					store = opaqueOwnerLoaderStore{base}
				}
				want := contracts.LoadRegistryFromGraphWithScope(store, "repo", "workspace", "project")
				got, stats := contracts.LoadRegistryFromGraphWithScopeAndStats(store, "repo", "workspace", "project")
				require.NotNil(t, want)
				require.NotNil(t, got)
				assert.Equal(t, ownerLoaderRecords(t, want.ByRepo("repo")), ownerLoaderRecords(t, got.ByRepo("repo")))
				assert.Equal(t, records, stats.OwnerEdgeRows)
				assert.Equal(t, records, stats.ScopedNodeRows)
				assert.Equal(t, records, stats.MissingTargetIDs)
				assert.Equal(t, 2, stats.MissingTargetBatches)
				assert.Equal(t, records, stats.MissingTargetRows)
				assert.Equal(t, 2*records, stats.RecoveredRecords)
				if opaque {
					assert.Equal(t, records, stats.LegacyCandidateIDs)
					assert.Equal(t, 2, stats.LegacyLivenessBatches)
				} else {
					assert.Zero(t, stats.LegacyCandidateIDs)
					assert.Zero(t, stats.LegacyLivenessBatches)
				}
				for _, ms := range []float64{stats.OwnerEdgesMS, stats.ScopedNodesMS, stats.MissingTargetsMS, stats.LegacyLivenessMS, stats.ConstructionMS} {
					assert.GreaterOrEqual(t, ms, float64(0))
				}
				_, empty := contracts.LoadRegistryFromGraphWithScopeAndStats(store, "absent", "workspace", "project")
				assert.Zero(t, empty.OwnerEdgeRows)
				assert.Zero(t, empty.ScopedNodeRows)
				assert.Zero(t, empty.MissingTargetBatches)
				assert.Zero(t, empty.LegacyLivenessBatches)
				assert.Zero(t, empty.RecoveredRecords)
			}
		})
	}
	registry, stats := contracts.LoadRegistryFromGraphWithScopeAndStats(nil, "repo", "workspace", "project")
	assert.Nil(t, registry)
	assert.Equal(t, contracts.RegistryLoadStats{}, stats)
}
