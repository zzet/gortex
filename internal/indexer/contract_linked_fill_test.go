package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestContractLinkedFillReacquiresMovedAncestryAndResumesPartialCache(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 33; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
	linked := make(map[string]string, len(files))
	for name, text := range files {
		linked[name] = text
	}
	linked["consts.go"] = "package fixture\nconst RoutePath = \"/linked\"\n"
	w := newContractLinkedWorktree(t, f, linked, "consts.go")
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture"})
	var cache *contractDeltaCache
	var firstKey, lastKey string
	extracted := map[string]int{}
	before := f.options.Materializer.Leases.Held()
	scratch := t.TempDir()
	run := func() (ContractFollowupReport, error) {
		f.layers++
		inputs, source, err := factory(t.Context(), actor, "fixture")
		require.NoError(t, err)
		defer source.Release()
		require.Equal(t, contractLinkedCheckout, inputs.State.CheckoutID)
		lastKey = contractInputsKey(inputs)
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(t.Context(), source, inputs, "fixture", contractLinkedCheckout)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
		_, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "linked-fill", LayerID: fmt.Sprintf("segment-%d", f.layers), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
		require.NoError(t, err)
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		report, runErr := RunContractFollowup(t.Context(), ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, Logger: zap.NewNop(), ScratchParent: scratch, OtherActorWaiting: func() bool { return true }, delta: session, convergenceHooks: &contractConvergenceHooks{beforeExtract: func(path string) { extracted[path]++ }}})
		cache = session.cache
		return report, runErr
	}
	report, err := run()
	require.ErrorAs(t, err, new(*ContractFillContinued))
	require.False(t, report.Published)
	require.NotNil(t, cache)
	require.False(t, cache.complete)
	firstKey = lastKey
	require.Equal(t, before, f.options.Materializer.Leases.Held(), "no consumed linked lease survives a segment")
	view := w.view(t)
	linked["consts.go"] = "package fixture\nconst RoutePath = \"/linked-moved\"\n"
	writeFile(t, filepath.Join(w.root, "consts.go"), linked["consts.go"])
	dirty := w.build(t, DirtyLayerGenerationKind, w.commit, view, "consts.go")
	view.Close()
	w.route(t, w.commit, dirty)
	published := false
	for segment := 0; segment < 5; segment++ {
		report, err = run()
		require.Equal(t, before, f.options.Materializer.Leases.Held())
		entries, readErr := os.ReadDir(scratch)
		require.NoError(t, readErr)
		require.Empty(t, entries, "segment scratch is closed before reacquiring another ancestry")
		if err == nil {
			require.True(t, report.Published)
			published = true
			break
		}
		require.ErrorAs(t, err, new(*ContractFillContinued))
	}
	require.True(t, published)
	require.NotEqual(t, firstKey, lastKey)
	require.True(t, cache.complete)
	require.Equal(t, 1, extracted["fixture/helper00.go"], "a valid partial raw unit is not re-extracted on the new linked ancestry")
	finalView := w.view(t)
	require.Equal(t, contractMeta(coldContracts(t, linked)), contractMeta(w.contracts(t, finalView)))
	finalView.Close()
}
