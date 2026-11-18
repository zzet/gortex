package graphview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func contractInputGeneration(t *testing.T, store *store_sqlite.Store, kind string, parent int64, inputs ...graph.ContractInputState) int64 {
	t.Helper()
	generation, handle, err := store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: testGraphID, CheckoutID: testCheckoutID,
		LayerID: "contract-inputs", GenerationKind: kind, BaseGenerationID: parent,
		TreeOID: "tree-contract-inputs", CreatedAt: 500,
	})
	require.NoError(t, err)
	for _, input := range inputs {
		require.NoError(t, handle.SetContractInputStateWithWorkContext(t.Context(), nil, input, nil))
	}
	publishTestGeneration(t, store, generation)
	return generation
}

func acceptedContractInput(repo, actor, fingerprint string) graph.ContractInputState {
	return graph.ContractInputState{RepoPrefix: repo, CheckoutID: actor, InputVersion: "contract-input-v1", InputFingerprint: fingerprint, Accepted: true}
}

func acceptPrimaryContractInput(t *testing.T, store *store_sqlite.Store, previous *graph.ContractInputState, next graph.ContractInputState) {
	t.Helper()
	require.NoError(t, store.BeginContractInputMutationContext(t.Context(), previous, next, nil))
	require.NoError(t, store.AcceptContractInputMutationContext(t.Context(), next))
}

func materializeContractInputs(t *testing.T, store *store_sqlite.Store, generation int64) (*Materializer, *RepoView) {
	t.Helper()
	seedStackControlPlane(t, store, generation)
	routeStack(t, store, generation, 0, store_sqlite.RouteActive)
	m := newTestMaterializer(store)
	view, err := m.MaterializeCheckout(t.Context(), testCheckoutID)
	require.NoError(t, err)
	t.Cleanup(view.Close)
	return m, view
}

func TestSelectedContractInputsUseActualInheritedAndDedicatedBaselines(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "dedicated"}[dedicated], func(t *testing.T) {
			store := openTestStore(t)
			primary := acceptedContractInput(stackRepo, "", "primary-contract-inputs")
			acceptPrimaryContractInput(t, store, nil, primary)
			kind := "commit"
			if dedicated {
				kind = "dedicated"
			}
			positive := acceptedContractInput(stackRepo, testCheckoutID, "selected-contract-inputs")
			generation := contractInputGeneration(t, store, kind, 0, positive)
			m, view := materializeContractInputs(t, store, generation)
			require.Equal(t, !dedicated, view.ComposesBaseCorpus())
			capture, err := m.CaptureContractInputs(t.Context(), view, stackRepo, testCheckoutID)
			require.NoError(t, err)
			require.True(t, capture.State.Accepted)
			if dedicated {
				require.Len(t, capture.Witnesses, 1)
				require.Equal(t, positive.InputFingerprint, capture.State.InputFingerprint)
			} else {
				require.Len(t, capture.Witnesses, 2)
				require.Zero(t, capture.Witnesses[0].GenerationID)
				require.Equal(t, "contract-selected-input-v1", capture.State.InputVersion)
			}
			// Unrelated repo changes and graph-only mutations do not move this
			// contract-specific input fence.
			acceptPrimaryContractInput(t, store, nil, acceptedContractInput("other", "", "unrelated"))
			store.AddNode(&graph.Node{ID: "unrelated.go::local", Kind: graph.KindFunction, FilePath: "unrelated.go", RepoPrefix: stackRepo})
			require.NoError(t, capture.Validate(t.Context()))
			next := primary
			next.InputFingerprint = "new-primary-contract-inputs"
			acceptPrimaryContractInput(t, store, &primary, next)
			if dedicated {
				require.NoError(t, capture.Validate(t.Context()))
			} else {
				require.ErrorIs(t, capture.Validate(t.Context()), graph.ErrContractProjectionStale)
			}
		})
	}
}

func TestSelectedContractInputsCannotLaunderUnknownInheritedBaseline(t *testing.T) {
	store := openTestStore(t)
	generation := contractInputGeneration(t, store, "commit", 0, acceptedContractInput(stackRepo, testCheckoutID, "sparse"))
	m, view := materializeContractInputs(t, store, generation)
	capture, err := m.CaptureContractInputs(t.Context(), view, stackRepo, testCheckoutID)
	require.Error(t, err)
	require.Nil(t, capture)
}

func TestSelectedContractInputsCannotLaunderLegacyPositiveLayers(t *testing.T) {
	store := openTestStore(t)
	acceptPrimaryContractInput(t, store, nil, acceptedContractInput(stackRepo, "", "primary-known"))
	root := contractInputGeneration(t, store, "commit", 0)
	upper := contractInputGeneration(t, store, "dirty", root)
	seedStackControlPlane(t, store, root)
	routeStack(t, store, root, upper, store_sqlite.RouteActive)
	m := newTestMaterializer(store)
	view, err := m.MaterializeCheckout(t.Context(), testCheckoutID)
	require.NoError(t, err)
	defer view.Close()
	require.True(t, view.ComposesBaseCorpus())
	require.Len(t, view.GenerationSources(), 2)
	capture, err := m.CaptureContractInputs(t.Context(), view, stackRepo, testCheckoutID)
	require.Equal(t, CodeCapabilityUnavailable, CodeOf(err))
	require.Nil(t, capture, "known primary baseline cannot certify either legacy positive layer contract-inert")
}

func TestSelectedContractInputsRetainCopiedActorAndRejectClaimedKeyMutation(t *testing.T) {
	store := openTestStore(t)
	carried := acceptedContractInput(stackRepo, "original-actor", "carried")
	generation := contractInputGeneration(t, store, "dedicated", 0, carried)
	m, view := materializeContractInputs(t, store, generation)
	capture, err := m.CaptureContractInputs(t.Context(), view, stackRepo, testCheckoutID)
	require.NoError(t, err)
	require.Len(t, capture.Witnesses, 2)
	require.False(t, capture.Witnesses[0].Found)
	require.Equal(t, testCheckoutID, capture.Witnesses[0].State.CheckoutID)
	require.Equal(t, carried, capture.Witnesses[1].State)
	require.Equal(t, carried.InputFingerprint, capture.State.InputFingerprint)
	// No SQL mutation is needed to show the capture cannot bless a caller's
	// different claimed key, even when it uses the same persisted sources.
	capture.State.InputFingerprint = "caller-assertion"
	require.ErrorIs(t, capture.Validate(t.Context()), graph.ErrContractProjectionStale)
}

func TestSelectedContractInputsComposeCompanionsAndFailClosed(t *testing.T) {
	store := openTestStore(t)
	for _, repo := range []string{"a", "b"} {
		acceptPrimaryContractInput(t, store, nil, acceptedContractInput(repo, "", repo+"-inputs"))
	}
	m := newTestMaterializer(store)
	a, err := m.CaptureContractInputs(t.Context(), nil, "a", "linked")
	require.NoError(t, err)
	b, err := m.CaptureContractInputs(t.Context(), nil, "b", "")
	require.NoError(t, err)
	combined, err := ComposeSelectedContractInputs("a", "linked", a, b)
	require.NoError(t, err)
	require.Equal(t, "contract-selected-input-v1", combined.State.InputVersion)
	require.NoError(t, combined.Validate(t.Context()))
	old := b.State
	next := old
	next.InputFingerprint = "b-changed"
	require.NoError(t, store.BeginContractInputMutationContext(t.Context(), &old, next, nil))
	require.ErrorIs(t, combined.Validate(t.Context()), graph.ErrContractProjectionStale)
	pending, err := m.CaptureContractInputs(t.Context(), nil, "b", "")
	require.NoError(t, err)
	require.False(t, pending.State.Accepted)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	capture, err := m.CaptureContractInputs(ctx, nil, "a", "")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, capture)
	require.NoError(t, store.Close())
	capture, err = m.CaptureContractInputs(t.Context(), nil, "a", "")
	require.Error(t, err)
	require.Nil(t, capture)
}

func TestSelectedContractCompanionInputsCaptureActualPositiveAbsence(t *testing.T) {
	store := openTestStore(t)
	companionNode := &graph.Node{ID: "other/companion.go::Handler", Kind: graph.KindFunction, FilePath: "other/companion.go", RepoPrefix: "other"}
	store.AddNode(companionNode)
	store.AddNode(&graph.Node{ID: stackRepo + "/primary.go::Obsolete", Kind: graph.KindFunction, FilePath: stackRepo + "/primary.go", RepoPrefix: stackRepo})
	acceptPrimaryContractInput(t, store, nil, acceptedContractInput("other", "", "other-primary"))
	generation := contractInputGeneration(t, store, "dedicated", 0, acceptedContractInput(stackRepo, testCheckoutID, "selected"))
	m, view := materializeContractInputs(t, store, generation)
	input, err := m.CaptureContractCompanionInputs(t.Context(), view, "other", testCheckoutID)
	require.NoError(t, err)
	require.Equal(t, testCheckoutID, input.State.CheckoutID)
	require.Len(t, input.Witnesses, 2)
	require.Zero(t, input.Witnesses[0].GenerationID)
	require.True(t, input.Witnesses[0].Found)
	require.Equal(t, generation, input.Witnesses[1].GenerationID)
	require.False(t, input.Witnesses[1].Found)
	require.Equal(t, testCheckoutID, input.Witnesses[1].State.CheckoutID)
	require.NoError(t, input.Validate(t.Context()))
	selected, err := m.CaptureContractInputs(t.Context(), view, stackRepo, testCheckoutID)
	require.NoError(t, err)
	cohort, err := ComposeSelectedContractInputs(stackRepo, testCheckoutID, selected, input)
	require.NoError(t, err)
	readers := cohort.SourceReaders()
	require.Same(t, view.Reader, readers[stackRepo], "dedicated selected repository must never substitute primary source")
	require.Nil(t, readers[stackRepo].GetNode(stackRepo+"/primary.go::Obsolete"))
	require.Equal(t, companionNode.ID, readers["other"].GetNode(companionNode.ID).ID, "companion base-zero evidence survives dedicated-root exclusion")
	delete(readers, "other")
	require.NotNil(t, cohort.SourceReaders()["other"], "caller changes cannot alter captured source admission")
	// The physical namespace absence cannot be rewritten by the caller to
	// publish under a different actor, even if the logical key remains equal.
	input.Witnesses[1].State.CheckoutID = "another-actor"
	require.ErrorIs(t, input.Validate(t.Context()), graph.ErrContractProjectionStale)
	input.Witnesses[1].State.CheckoutID = testCheckoutID
	// Companion primary changes remain part of the publication fence even
	// though the selected repo has a dedicated root excluding its own base0.
	old, found, err := store.ContractInputStateContext(t.Context(), "other", "")
	require.NoError(t, err)
	require.True(t, found)
	next := old
	next.InputFingerprint = "other-new"
	acceptPrimaryContractInput(t, store, &old, next)
	require.ErrorIs(t, input.Validate(t.Context()), graph.ErrContractProjectionStale)
}
