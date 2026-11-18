package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractLinkedInputCaptureReopensCurrentCheckoutAncestry(t *testing.T) {
	f := newRouteConstantFixture(t)
	files := map[string]string{"routes.go": jobRoutes, "consts.go": "package fixture\nconst RoutePath = \"/linked\"\n", "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, files, "consts.go")
	capture := NewContractLinkedInputCapture(f.options.Materializer)
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture"})
	before := f.options.Materializer.Leases.Held()
	first, source, err := capture(t.Context(), actor, "fixture")
	require.NoError(t, err)
	require.True(t, contractLinkedInputsMatch(actor, "fixture", first))
	require.Equal(t, contractLinkedCheckout, first.State.CheckoutID)
	require.True(t, f.options.Materializer.Leases.InUse(w.commit))
	source.Release()
	require.Equal(t, before, f.options.Materializer.Leases.Held(), "the original materialized view closed before returning its independent source")
	view := w.view(t)
	writeFile(t, filepath.Join(w.root, "consts.go"), "package fixture\nconst RoutePath = \"/moved\"\n")
	dirty := w.build(t, DirtyLayerGenerationKind, w.commit, view, "consts.go")
	view.Close()
	w.route(t, w.commit, dirty)
	second, source, err := capture(t.Context(), actor, "fixture")
	require.NoError(t, err)
	defer source.Release()
	require.NotEqual(t, contractInputsKey(first), contractInputsKey(second))
	require.Equal(t, contractLinkedCheckout, second.State.CheckoutID)
	found := false
	for _, witness := range second.Witnesses {
		if witness.GenerationID == dirty {
			found = true
			require.Equal(t, contractLinkedCheckout, witness.State.CheckoutID)
		}
	}
	require.True(t, found, "the fresh source uses the newly routed dirty generation")
	source.Release()
	require.Equal(t, before, f.options.Materializer.Leases.Held())
}

func TestContractLinkedInputCaptureKeepsCompanionPositiveAbsence(t *testing.T) {
	f := newRouteConstantFixture(t)
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, files, "helper.go")
	state := graph.ContractInputState{RepoPrefix: "companion", InputVersion: "v", InputFingerprint: "f", Accepted: true}
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), nil, state, nil))
	method := &graph.Node{ID: "companion/Service.java::bean", Name: "bean", Kind: graph.KindMethod, Language: "java", RepoPrefix: "companion", FilePath: "companion/Service.java", StartLine: 7, Meta: map[string]any{"signature": "Bean bean()", "spring_config_keys": []string{"service.url"}}}
	foreign := &graph.Node{ID: "other/Foreign::target", Kind: graph.KindFunction, RepoPrefix: "other", FilePath: "other/foreign.go"}
	require.NoError(t, f.store.AddBatchChecked([]*graph.Node{method, foreign}, []*graph.Edge{
		{From: method.ID, To: foreign.ID, Kind: graph.EdgeProvides, FilePath: method.FilePath, Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseExisting, graph.MetaDIToken: "ALIAS"}},
		{From: foreign.ID, To: method.ID, Kind: graph.EdgeConsumes, FilePath: foreign.FilePath},
	}))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), state))
	capture := NewContractLinkedInputCapture(f.options.Materializer)
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture", "companion"})
	inputs, source, err := capture(t.Context(), actor, "fixture")
	require.NoError(t, err)
	defer source.Release()
	require.True(t, contractLinkedInputsMatch(actor, "fixture", inputs))
	absent := false
	for _, witness := range inputs.Witnesses {
		if witness.State.RepoPrefix == "companion" && witness.GenerationID == w.commit {
			require.False(t, witness.Found)
			require.Equal(t, contractLinkedCheckout, witness.State.CheckoutID)
			absent = true
		}
	}
	require.True(t, absent, "selected positive namespace absence participates in the exact vector")
	reader, ok := inputs.SourceReaders()["companion"].(*store_sqlite.Store)
	require.True(t, ok, "the actual companion source is native despite its positive absence witness")
	require.Zero(t, reader.ViewGeneration())
	var handles []*store_sqlite.Store
	for _, witness := range inputs.Witnesses {
		if witness.State.RepoPrefix == "companion" {
			handles = append(handles, f.store.AtGeneration(witness.GenerationID))
		}
	}
	require.GreaterOrEqual(t, len(handles), 2, "the handle-count proxy would choose composed seed preparation")
	before := append([]graph.ContractInputWitness(nil), inputs.Witnesses...)
	wantNodes, err := reader.ContractSeedNodesContext(t.Context(), "companion")
	require.NoError(t, err)
	wantEdges, err := reader.ContractSeedEdgesContext(t.Context(), "companion")
	require.NoError(t, err)
	require.Len(t, wantNodes, 1)
	require.Len(t, wantEdges, 1)
	nodes, edges, supported, err := readContractFollowupSeeds(t.Context(), reader, handles, "companion")
	require.NoError(t, err)
	require.True(t, supported)
	require.Equal(t, wantNodes, nodes)
	require.Equal(t, wantEdges, edges)
	require.Equal(t, before, inputs.Witnesses, "seed dispatch must not remove the physical absence publication guard")
	require.NoError(t, inputs.Validate(t.Context()))
	companionOnly, selectedSource, err := capture(t.Context(), NewContractActor(contractLinkedCheckout, []string{"companion"}), "companion")
	require.NoError(t, err)
	defer selectedSource.Release()
	require.Equal(t, "fixture", selectedSource.repo, "the real selected checkout remains the source authority")
	require.True(t, selectedSource.composesBase, "the sparse fixture retains primary authority in its selected input vector")
	for _, witness := range companionOnly.Witnesses {
		require.Equal(t, "companion", witness.State.RepoPrefix, "selected authority is private, never an extra payload target")
	}
	require.NoError(t, companionOnly.Validate(t.Context()))
	selected, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	moved := selected
	moved.InputFingerprint += "-changed"
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &selected, moved, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), moved))
	require.ErrorIs(t, inputs.Validate(t.Context()), graph.ErrContractProjectionStale, "native companion seeds do not weaken the selected cohort fence")
	require.ErrorIs(t, companionOnly.Validate(t.Context()), graph.ErrContractProjectionStale, "the private selected authority is still validated for a companion-only vector")
	// In-flight input authority is validated, not rejected for Accepted alone.
	next := state
	next.InputFingerprint, next.Accepted = "pending", false
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &state, next, nil))
	pending, fresh, err := capture(t.Context(), actor, "fixture")
	require.NoError(t, err)
	defer fresh.Release()
	require.False(t, pending.State.Accepted)
	require.NoError(t, pending.Validate(t.Context()))
}

func TestContractLinkedInputCaptureFailureClosesPartialMaterialization(t *testing.T) {
	f := newRouteConstantFixture(t)
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	newContractLinkedWorktree(t, f, files, "helper.go")
	capture := NewContractLinkedInputCapture(f.options.Materializer)
	before := f.options.Materializer.Leases.Held()
	// fixture sorts before missing: selected input capture succeeds before the
	// companion has no certified baseline and refuses the partial capture.
	_, source, err := capture(t.Context(), NewContractActor(contractLinkedCheckout, []string{"fixture", "missing"}), "fixture")
	require.Error(t, err)
	require.Nil(t, source)
	require.Equal(t, before, f.options.Materializer.Leases.Held())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, source, err = capture(ctx, NewContractActor(contractLinkedCheckout, []string{"fixture"}), "fixture")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, source)
	require.Equal(t, before, f.options.Materializer.Leases.Held())
	_, source, err = capture(t.Context(), NewContractActor(contractLinkedCheckout, []string{"fixture"}), "other")
	require.ErrorIs(t, err, graph.ErrContractInputVector)
	require.Nil(t, source)
	require.Equal(t, before, f.options.Materializer.Leases.Held())
	_, source, err = capture(t.Context(), NewContractActor("", []string{"fixture"}), "fixture")
	require.ErrorIs(t, err, graph.ErrContractInputVector, "a primary capture cannot replace linked authority")
	require.Nil(t, source)
}

func TestContractLinkedInputsRequireTheExactActorCohort(t *testing.T) {
	actor := NewContractActor("linked", []string{"a", "b"})
	inputs := schedulerInputs([]string{"a", "b"}, "k")
	inputs.State.CheckoutID = "linked"
	require.True(t, contractLinkedInputsMatch(actor, "a", inputs))
	wrong := *inputs
	wrong.State.CheckoutID = "other"
	require.False(t, contractLinkedInputsMatch(actor, "a", &wrong))
	missing := schedulerInputs([]string{"a"}, "k")
	missing.State.CheckoutID = "linked"
	require.False(t, contractLinkedInputsMatch(actor, "a", missing))
	extra := schedulerInputs([]string{"a", "b", "c"}, "k")
	extra.State.CheckoutID = "linked"
	require.False(t, contractLinkedInputsMatch(actor, "a", extra))
}
