package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
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
