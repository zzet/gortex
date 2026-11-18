package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type endpointReplayCountingReader struct {
	graph.Reader
	observe func([]string)
}

func (r *endpointReplayCountingReader) Unwrap() graph.Reader { return r.Reader }

func (r *endpointReplayCountingReader) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	r.observe([]string{name})
	return graph.VisitNodesByNamesContext(ctx, r.Reader, []string{name}, yield)
}
func (r *endpointReplayCountingReader) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.observe(names)
	return graph.VisitNodesByNamesContext(ctx, r.Reader, names, yield)
}

func TestContractEndpointCandidatesBracketGroupsDistinctReads(t *testing.T) {
	files := map[string]string{
		"routes.go":    "package fixture\nfunc register(r Router) { r.GET(firstPath, handler); r.GET(secondPath, handler); r.GET(thirdPath, handler) }\nfunc handler() {}\n",
		"constants.go": "package fixture\nconst firstPath = \"/first\"\nconst secondPath = \"/second\"\nconst thirdPath = \"/third\"\n",
	}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	for _, name := range []string{"firstPath", "secondPath", "thirdPath"} {
		require.True(t, cache.units["fixture/routes.go"].keys["name:"+name])
	}
	// Same-fingerprint work must force re-asking the cached dependents even
	// though their declaration answers are unchanged; it cannot skip to parse.
	debt, _ := advanceContractCoverageInputs(t, f.store, "fixture", "fixture/constants.go", graph.ContractWorkScope{})
	asking, calls, groups := false, 0, 0
	names := map[string]bool{}
	hooks := &contractConvergenceHooks{
		beforeAsk: func() { asking = true; calls = 0; names = map[string]bool{} },
		beforeRevalidateR1: func() {
			if asking && len(names) == 3 {
				groups++
				require.Equal(t, 1, calls, "distinct endpoint questions in one bracket must share one checked selected-name visit")
			}
			asking = false
		},
	}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: &cache, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(s *ContractFollowupSnapshot) {
		require.Contains(t, s.Work, debt)
		s.Core = &endpointReplayCountingReader{Reader: s.Core, observe: func(got []string) {
			if !asking {
				return
			}
			hit := false
			for _, name := range got {
				if name == "firstPath" || name == "secondPath" || name == "thirdPath" {
					names[name] = true
					hit = true
				}
			}
			if hit {
				calls++
			}
		}}
	}})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Positive(t, groups, "the real warm job must re-ask all three distinct constant declarations in one bracket")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	files["constants.go"] = strings.ReplaceAll(files["constants.go"], "/first", "/changed")
	f.save("constants.go", files["constants.go"])
	runCachedContractFixture(t, f, &cache)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	require.NotContains(t, contractMeta(f.contracts()), "/first")
}
