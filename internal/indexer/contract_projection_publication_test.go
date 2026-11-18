package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractFollowupLateSemanticMutationRefusesPublication(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": `package fixture
import "net/http"
func routes(mux *http.ServeMux) {mux.HandleFunc("GET /route",handler)}
func handler(http.ResponseWriter,*http.Request) {}
`})
	site := graph.SemanticBindingSite{RepoPrefix: "fixture", FilePath: "fixture/routes.go", Line: 4, Name: "value"}
	changed := false
	hooks := &contractConvergenceHooks{afterPP: func() {
		if !changed {
			changed = true
			require.NoError(t, f.store.ReplaceSemanticBindingTypes("fixture", []graph.SemanticBindingType{{Site: site, TypeName: "LateResponse"}}))
		}
	}}
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, hooks: hooks, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(s *ContractFollowupSnapshot) { s.projectionProofRequired = true }})
	require.True(t, changed)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.True(t, errors.Is(err, store_sqlite.ErrPayloadInputChanged))
	require.False(t, report.Published, "no output or covered work may publish with pre-mutation proof")
	actual, readErr := f.store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, readErr)
	require.Equal(t, "LateResponse", actual[site])
	// The next stable job captures new current proof and uses the same unchanged
	// receipt/input/work guards. No retry relaxation or scheduler policy needed.
	f.layers++
	report, err = runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, epoch: f.epoch.Load, wait: f.waitPublished, adjust: func(s *ContractFollowupSnapshot) { s.projectionProofRequired = true }})
	require.NoError(t, err)
	require.True(t, report.Published)
}
