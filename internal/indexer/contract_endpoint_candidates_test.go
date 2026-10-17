package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

type endpointCandidateRows struct {
	graph.Reader
	rows  []*graph.Node
	guard *contractReadGuard
	err   error
}

func (r *endpointCandidateRows) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	if r.guard != nil && !r.guard.recorded() {
		return graph.ErrContractProjectionIncomplete
	}
	for _, node := range r.rows {
		if node.Name == name && !yield(node) {
			break
		}
	}
	if r.err != nil {
		return r.err
	}
	return ctx.Err()
}

func TestContractEndpointCandidatesReplayOnlyConsumedDeclarationFields(t *testing.T) {
	guard := &contractReadGuard{open: true}
	constant := &graph.Node{ID: "repo/constants.go::id", Name: "id", Kind: graph.KindConstant, FilePath: "repo/constants.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "string"}}
	local := &graph.Node{ID: "repo/handler.go::handler#param:id", Name: "id", Kind: graph.KindParam, FilePath: "repo/handler.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "string"}}
	outside := &graph.Node{ID: "other/constants.go::id", Name: "id", Kind: graph.KindConstant, FilePath: "other/constants.go", RepoPrefix: "repo"}
	r := &endpointCandidateRows{rows: []*graph.Node{local, outside, constant}, guard: guard}
	e := &contractFollowupEvidence{ctx: t.Context(), core: r, ledger: newContractUnitLedger(), guard: guard, allowedRepos: map[string]bool{"repo": true}}
	require.Equal(t, []contracts.EndpointConstantCandidate{{ID: constant.ID, FilePath: constant.FilePath}}, e.FindEndpointConstantCandidates("id", "repo"))
	require.NoError(t, e.err)
	require.Equal(t, map[string]bool{"repo/constants.go": true}, e.ledger.owners)
	require.True(t, e.ledger.keys["name:id"])
	require.Len(t, e.ledger.reads, 1)
	reads := e.ledger.reads
	constant.Meta = nil
	constant.StartLine = 99
	local.Meta = nil
	local.StartLine = 100
	c := &contractConvergence{}
	equal, err := c.replay(e, reads, nil)
	require.NoError(t, err)
	require.True(t, equal, "locations and enrichment metadata are not consumed by endpoint constant resolution")
	constant.FilePath = "repo/new-owner.go"
	equal, err = c.replay(e, reads, nil)
	require.NoError(t, err)
	require.False(t, equal, "owner moves can change same-file preference")
	constant.FilePath = "repo/constants.go"
	r.rows = append(r.rows, &graph.Node{ID: "repo/added.go::id", Name: "id", Kind: graph.KindConstant, FilePath: "repo/added.go", RepoPrefix: "repo"})
	equal, err = c.replay(e, reads, nil)
	require.NoError(t, err)
	require.False(t, equal, "a new constant candidate may introduce an ambiguous value")
}

func TestContractEndpointCandidatesNegativeReadTracksNewDeclarations(t *testing.T) {
	r := &endpointCandidateRows{}
	e := &contractFollowupEvidence{ctx: t.Context(), core: r, ledger: newContractUnitLedger(), allowedRepos: map[string]bool{"repo": true}}
	require.Empty(t, e.FindEndpointConstantCandidates("Missing", "repo"))
	read := e.ledger.reads[0]
	moves := contractMoves{moved: map[string]bool{"repo/new.go": true}, declaredKnown: true, declared: map[string]bool{"name:Missing": true}, byPath: map[string][]string{"repo/new.go": {"name:Missing"}}}
	require.True(t, moves.affected(read))
	require.True(t, moves.depends("repo/new.go", read))
	require.Equal(t, []string{"repo/new.go"}, moves.inputsOf([]contractRecordedRead{read}))
	r.rows = []*graph.Node{{ID: "repo/new.go::Missing", Name: "Missing", Kind: graph.KindConstant, FilePath: "repo/new.go", RepoPrefix: "repo"}}
	equal, err := (&contractConvergence{}).replay(e, []contractRecordedRead{read}, nil)
	require.NoError(t, err)
	require.False(t, equal)
}

// This reader models cold semantic enrichment that an ordinary handler parse
// replaces. It affects only broad name candidates; accepted source and receipt
// movement still go through the real production fixture and save door.
type endpointSemanticCandidates struct {
	graph.Reader
	enriched bool
	observed func(*graph.Node)
}

func (r *endpointSemanticCandidates) Unwrap() graph.Reader { return r.Reader }

func (r *endpointSemanticCandidates) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return graph.VisitNodesByNamesContext(ctx, r.Reader, []string{name}, func(node *graph.Node) bool {
		if r.enriched && node.FilePath == "fixture/handler.go" && node.Kind != graph.KindConstant {
			copyNode := *node
			copyNode.Meta = map[string]any{"semantic_type": "string", "semantic_source": "go-types"}
			if r.observed != nil {
				r.observed(&copyNode)
			}
			return yield(&copyNode)
		}
		if r.observed != nil {
			r.observed(node)
		}
		return yield(node)
	})
}

func TestContractDeltaEndpointCandidateLocalityAndProducerLifecycle(t *testing.T) {
	files := map[string]string{
		"routes.go":    "package fixture\nfunc register(r Router) { r.GET(id, users); r.GET(name, users) }\nfunc users() {}\n",
		"constants.go": "package fixture\nconst id = \"/stable\"\n",
		"handler.go":   "package fixture\nfunc handler(r Router, id string, name string) { r.GET(\"/before\", users) }\n",
	}
	f := newContractJobFixture(t, files)
	enriched := true
	adjustments, enrichedRows, plainRows := 0, 0, 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		adjustments++
		snapshot.Core = &endpointSemanticCandidates{Reader: snapshot.Core, enriched: enriched, observed: func(node *graph.Node) {
			if node.FilePath == "fixture/handler.go" && node.Kind != graph.KindConstant {
				if enriched {
					require.Equal(t, "go-types", node.Meta["semantic_source"])
					enrichedRows++
				} else {
					require.NotEqual(t, "go-types", node.Meta["semantic_source"])
					plainRows++
				}
			}
		}}
		if !enriched {
			// The warm reader may correctly avoid this unrelated query. Check
			// the selected fixture's changed answer directly before the job.
			require.NoError(t, snapshot.Core.(*endpointSemanticCandidates).VisitNodesByNameContext(t.Context(), "id", func(*graph.Node) bool { return true }))
		}
	}
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Equal(t, 1, adjustments)
	require.Positive(t, enrichedRows, "the selected reader must actually return injected semantic parameter rows")
	reader := cache.units["fixture/routes.go"]
	require.True(t, reader.keys["name:id"])
	require.True(t, reader.keys["name:name"], "the unresolved route retains its negative lookup")
	require.NotContains(t, reader.owners, "fixture/handler.go", "same-named parameters are not constant dependencies")
	files["handler.go"] = strings.ReplaceAll(files["handler.go"], "/before", "/after")
	enriched = false
	f.save("handler.go", files["handler.go"])
	report := runCachedContractFixture(t, f, &cache)
	require.Equal(t, 2, adjustments)
	require.Positive(t, plainRows, "the selected reader must observe the same parameter declarations without semantic enrichment")
	require.Equal(t, 1, report.Files, "an unrelated parameter's enrichment loss must not extract the route reader")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	for _, change := range []struct{ path, source string }{
		{"new-owner.go", "package fixture\nconst name = \"/new-route\"\n"},
		{"constants.go", "package fixture\nconst id = \"/changed\"\n"},
		{"new-owner.go", "package fixture\nconst Renamed = \"/new-route\"\n"},
	} {
		files[change.path] = change.source
		f.save(change.path, change.source)
		report := runCachedContractFixture(t, f, &cache)
		require.GreaterOrEqual(t, report.Files, 2, "constant addition, value change, and removal must extract their reader")
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	}
}
