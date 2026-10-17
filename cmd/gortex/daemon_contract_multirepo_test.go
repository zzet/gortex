package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// One provider edit changes a real bridge's provider cardinality. The consumer
// and the filler have no local save: their new attachments must follow the
// admitted dependency vector, in the same production cohort publication.
func TestDaemonContractMultiRepoDeltaPublishesThreeCurrentPayloads(t *testing.T) {
	provider := "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /v1/items\", handle) }\nfunc handle(w http.ResponseWriter, r *http.Request) {}\n"
	filler := map[string]string{"main.go": "package filler\n"}
	for i := 0; i < 200; i++ {
		filler[fmt.Sprintf("filler%03d.go", i)] = fmt.Sprintf("package filler\nfunc Filler%d() int { return %d }\n", i, i)
	}
	f := newDaemonContractFixture(t, map[string]map[string]string{
		"provider": {"main.go": provider},
		"consumer": {"main.go": "package fixture\nimport \"net/http\"\nfunc call() { http.Get(\"http://example.test/v1/items\") }\n"},
		"filler":   filler,
	})
	require.NotEmpty(t, f.publicContracts())
	f.idle()
	actor := indexer.NewContractActor("", f.repos)
	beforeProof, releaseBefore, err := f.runtime.coordinator.AcquireLastPublishedCohort(f.ctx, actor)
	require.NoError(t, err)
	require.Len(t, beforeProof.Targets, 3)
	releaseBefore()

	bridge := func(repo string, providers int) *graph.Node {
		t.Helper()
		analysis := f.currentAnalysis(repo)
		defer analysis.Close()
		nodes, err := analysis.NodesByKindsContext(f.ctx, []graph.NodeKind{graph.KindContractBridge})
		require.NoError(t, err)
		require.Len(t, nodes, 1, "actual HTTP provider and consumer must create a bridge")
		node := nodes[0]
		require.Equal(t, "http::GET::/v1/items", node.Meta["contract_id"])
		require.Equal(t, true, node.Meta["cross_repo"])
		require.EqualValues(t, providers, node.Meta["provider_count"])
		require.EqualValues(t, 1, node.Meta["consumer_count"])
		return node
	}
	oldBridge := bridge("consumer", 1)
	beforeLogCount := f.logs.FilterMessage("contract analysis job").Len()
	before := f.counters()
	started := time.Now()
	provider += "\nfunc registerSecond(mux *http.ServeMux) { mux.HandleFunc(\"GET /v1/items\", second) }\nfunc second(w http.ResponseWriter, r *http.Request) {}\n"
	require.True(t, f.save("provider", "main.go", provider))
	require.NotEmpty(t, f.publicContracts())
	f.idle()
	f.assertEditBudget(before, 1, true)
	afterProof, releaseAfter, err := f.runtime.coordinator.AcquireLastPublishedCohort(f.ctx, actor)
	require.NoError(t, err)
	defer releaseAfter()
	require.Greater(t, afterProof.Seq, beforeProof.Seq)
	require.Len(t, afterProof.Targets, 3)
	require.Len(t, afterProof.ExpectedTargets, 3)
	jobs := f.logs.FilterMessage("contract analysis job").All()[beforeLogCount:]
	require.Len(t, jobs, 1, "the entire edit window must contain one cohort job; startup work is accounted separately")
	require.EqualValues(t, 3, jobs[0].ContextMap()["targets"])
	require.EqualValues(t, 3, jobs[0].ContextMap()["published"])
	require.Equal(t, viewmetrics.ContractModeDelta, jobs[0].ContextMap()["mode"])

	beforeTargets := map[string]indexer.ContractPublishedTarget{}
	for _, target := range beforeProof.Targets {
		beforeTargets[target.Key.RepoPrefix] = target
	}
	newPayloads := map[int64]bool{}
	for _, target := range afterProof.Targets {
		previous, found := beforeTargets[target.Key.RepoPrefix]
		require.True(t, found)
		require.NotEqual(t, previous.Key.InputFingerprint, target.Key.InputFingerprint,
			"unchanged local files still require the new admitted cohort key")
		require.NotEqual(t, previous.PayloadGeneration, target.PayloadGeneration)
		require.False(t, newPayloads[target.PayloadGeneration])
		newPayloads[target.PayloadGeneration] = true
		header, err := f.store.GetContractAttachmentContext(f.ctx, target.Key)
		require.NoError(t, err)
		require.NotNil(t, header)
		require.Equal(t, target.PayloadGeneration, header.PayloadGeneration)
		generation, found, err := f.store.Catalog().GetViewGeneration(f.ctx, target.PayloadGeneration)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, store_sqlite.ViewGenerationReady, generation.State)
		if target.Key.RepoPrefix == "filler" {
			analysis := f.currentAnalysis("filler")
			nodes, err := analysis.NodesByKindsContext(f.ctx, []graph.NodeKind{graph.KindContractBridge})
			require.NoError(t, err)
			require.Empty(t, nodes, "the filler payload must not inherit another repository's bridge scope")
			analysis.Close()
		} else {
			currentBridge := bridge(target.Key.RepoPrefix, 2)
			require.Equal(t, oldBridge.ID, currentBridge.ID)
			require.False(t, reflect.DeepEqual(oldBridge.Meta, currentBridge.Meta), "the bridge must actually change")
		}
	}
	require.Len(t, newPayloads, 3)
	// AcquireLastPublishedCohort returns only the plural transaction's complete
	// target vector. Atomic rollback/one-write-gate internals have permanent
	// store tests; this daemon row verifies its actual three-target outcome.
	t.Logf("three current attachments and updated consumer bridge in %s", time.Since(started))
}
