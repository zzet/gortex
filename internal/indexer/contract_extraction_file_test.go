package indexer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func extractionFileEvidence(t *testing.T, edges []*graph.Edge, meta map[string]any) *contractFollowupEvidence {
	t.Helper()
	scratch := newFTSStore(t)
	guard := &contractReadGuard{open: true}
	e := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: t.Context(), ledger: newContractUnitLedger(), guard: guard,
		files: map[string]ContractFollowupFile{"repo/dashboard.go": {Path: "repo/dashboard.go", RepoPrefix: "repo"}}, allowedRepos: map[string]bool{"repo": true}}
	e.readCore = func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error) {
		require.True(t, guard.recorded(), "selected file projection stays inside its recorded-read bracket")
		return ContractFollowupCoreFile{Nodes: []*graph.Node{
			{ID: "repo/dashboard.go", Kind: graph.KindFile, FilePath: "repo/dashboard.go", RepoPrefix: "repo"},
			{ID: "repo/dashboard.go::render", Kind: graph.KindFunction, Name: "render", FilePath: "repo/dashboard.go", RepoPrefix: "repo", Meta: meta},
		}, Edges: edges}, nil
	}
	return e
}

func TestContractExtractionFileRetainsOnlyConsumedEdgeInputs(t *testing.T) {
	ordinary := &graph.Edge{From: "repo/dashboard.go::render", To: "repo/handler.go::handler", Kind: graph.EdgeCalls, FilePath: "repo/handler.go"}
	provides := &graph.Edge{From: "repo/dashboard.go::render", To: "repo/provider.go::Service", Kind: graph.EdgeProvides, FilePath: "repo/dashboard.go",
		Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "TOKEN"}}
	consumes := &graph.Edge{From: "repo/dashboard.go::render", To: "repo/provider.go::Service", Kind: graph.EdgeConsumes, FilePath: "repo/dashboard.go",
		Meta: map[string]any{"via": "@Inject", graph.MetaDIToken: "TOKEN"}}
	spring := &graph.Edge{From: "repo/dashboard.go::render", To: "repo/provider.go::Bean", Kind: graph.EdgeCalls, FilePath: "repo/dashboard.go", Meta: map[string]any{"via": "spring.Bean"}}
	baseline := []*graph.Edge{ordinary, provides, consumes, spring}
	e := extractionFileEvidence(t, baseline, map[string]any{"signature": "func render()"})
	_, edges, err := e.loadExtractionFile("repo/dashboard.go")
	require.NoError(t, err)
	require.Equal(t, []*graph.Edge{provides, consumes, spring}, edges)
	read := e.ledger.reads[0]
	require.Equal(t, contractReadExtractionFile, read.kind)
	require.NotContains(t, read.owners, "repo/handler.go")
	check := func(t *testing.T, edges []*graph.Edge, meta map[string]any, equal bool) {
		t.Helper()
		next := extractionFileEvidence(t, edges, meta)
		got, err := (&contractConvergence{}).replay(next, []contractRecordedRead{read}, nil)
		require.NoError(t, err)
		require.Equal(t, equal, got)
	}
	t.Run("ordinary_callee_and_foreign_owner", func(t *testing.T) {
		changed := *ordinary
		changed.Kind, changed.To = graph.EdgeArgOf, "repo/other.go::Other"
		check(t, []*graph.Edge{&changed, provides, consumes, spring}, map[string]any{"signature": "func render()"}, true)
	})
	t.Run("di_token", func(t *testing.T) {
		changed := *provides
		changed.Meta = map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "CHANGED"}
		check(t, []*graph.Edge{ordinary, &changed, consumes, spring}, map[string]any{"signature": "func render()"}, false)
	})
	t.Run("di_binding", func(t *testing.T) {
		changed := *provides
		changed.Meta = map[string]any{graph.MetaDIBinding: graph.DIBindingUseFactory, graph.MetaDIToken: "TOKEN"}
		check(t, []*graph.Edge{ordinary, &changed, consumes, spring}, map[string]any{"signature": "func render()"}, false)
	})
	t.Run("spring_via", func(t *testing.T) {
		changed := *spring
		changed.Meta = map[string]any{"via": "other"}
		check(t, []*graph.Edge{ordinary, provides, consumes, &changed}, map[string]any{"signature": "func render()"}, false)
	})
	t.Run("removed_seed", func(t *testing.T) {
		check(t, []*graph.Edge{ordinary, provides, spring}, map[string]any{"signature": "func render()"}, false)
	})
	t.Run("full_node_metadata", func(t *testing.T) {
		check(t, baseline, map[string]any{"signature": "func render(value int)"}, false)
	})
	full := extractionFileEvidence(t, baseline, nil)
	_, fullEdges, err := full.loadFile("repo/dashboard.go")
	require.NoError(t, err)
	require.Equal(t, baseline, fullEdges, "common file reads retain every edge for enrichment and generated-core proofs")
	require.Contains(t, full.ledger.reads[0].owners, "repo/handler.go")
}

func TestContractDeltaExtractionEdgeLocalityAndSpringSeedChange(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary_call", true: "spring_seed"}[seed], func(t *testing.T) {
			files := map[string]string{
				"dashboard.go": "package fixture\nfunc render() { http.Get(\"/stable\") }\n",
				"handler.go":   "package fixture\nfunc handler(r Router) { r.GET(\"/before\", other) }\nfunc other() {}\n",
			}
			f := newContractJobFixture(t, files)
			changed := false
			adjustments, injectedRows := 0, 0
			f.adjust = func(snapshot *ContractFollowupSnapshot) {
				adjustments++
				readCore := snapshot.ReadCoreFile
				snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
					rows, err := readCore(ctx, file)
					if err == nil && file.Path == "fixture/dashboard.go" {
						edge := &graph.Edge{From: "fixture/dashboard.go::render", To: "fixture/handler.go::handler", Kind: graph.EdgeCalls, FilePath: "fixture/handler.go"}
						if seed {
							edge.Meta = map[string]any{"via": "spring.Bean"}
						}
						if changed {
							edge.To = "fixture/handler.go::other"
						}
						rows.Edges = append(rows.Edges, edge)
						injectedRows++
					}
					return rows, err
				}
			}
			var cache *contractDeltaCache
			runCachedContractFixture(t, f, &cache)
			require.Equal(t, 1, adjustments)
			require.Positive(t, injectedRows, "the actual selected dashboard read must contain the injected edge")
			reads := cache.units["fixture/dashboard.go"].ledger.reads
			require.True(t, slices.ContainsFunc(reads, func(read contractRecordedRead) bool { return read.kind == contractReadExtractionFile }), "the checkpoint retains the actual projected extraction query")
			require.Equal(t, seed, slices.ContainsFunc(reads, func(read contractRecordedRead) bool {
				return read.kind == contractReadExtractionFile && slices.Contains(read.owners, "fixture/handler.go")
			}), "only the consumed Spring input retains the foreign handler dependency")
			previousReads := injectedRows
			changed = true
			files["handler.go"] = strings.ReplaceAll(files["handler.go"], "/before", "/after")
			f.save("handler.go", files["handler.go"])
			report := runCachedContractFixture(t, f, &cache)
			require.Equal(t, 2, adjustments)
			if seed {
				require.Greater(t, injectedRows, previousReads, "changed consumed seed must be read again")
			}
			want := 1
			if seed {
				want = 2
			}
			require.Equal(t, want, report.Files, "only consumed edge changes may extract the unchanged dashboard")
			require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
		})
	}
}
