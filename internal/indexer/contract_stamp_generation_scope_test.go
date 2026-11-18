package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// This is a physical read-proof control, not authority to serve historical
// source bytes or acknowledge a later generation's pending contract work.
func TestContractStampUnroutedGenerationDoesNotMoveLeasedRows(t *testing.T) {
	f := newRouteConstantFixture(t)
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, files, "routes.go", "consts.go", "helper.go")
	view := w.view(t)
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	require.NoError(t, inputs.Validate(t.Context()))
	var handles []*store_sqlite.Store
	if view.ComposesBaseCorpus() {
		handles = append(handles, f.store.AtGeneration(0))
	}
	for _, source := range view.GenerationSources() {
		handles = append(handles, source.Handle)
	}
	scanner := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": handles}, nil)
	const path = "fixture/consts.go"
	before, err := scanner.scan(t.Context(), []string{path})
	require.NoError(t, err)
	require.True(t, before.Settled)
	require.Equal(t, w.commit, before.Stamps[path].Generation)
	require.True(t, before.Stamps[path].Accepted)
	beforeNodes, err := graph.ContractFileNodesContext(t.Context(), view.Reader, "fixture", []string{path})
	require.NoError(t, err)
	// A real builder writes and publishes another generation under the SAME
	// actor/path. It is deliberately never routed over the view being read.
	changedPath := filepath.Join(w.root, "consts.go")
	original, err := os.ReadFile(changedPath)
	require.NoError(t, err)
	writeFile(t, changedPath, jobConsts+"// separate unrouted generation\n")
	next := w.build(t, DirtyLayerGenerationKind, w.commit, view, "consts.go")
	require.NotEqual(t, w.commit, next)
	require.NoError(t, inputs.Validate(t.Context()), "the leased selected input vector is unchanged")
	route, found, err := f.store.Catalog().GetCheckoutRoute(t.Context(), contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, w.commit, route.CommitGenerationID)
	require.Zero(t, route.DirtyGenerationID)
	afterNodes, err := graph.ContractFileNodesContext(t.Context(), view.Reader, "fixture", []string{path})
	require.NoError(t, err)
	require.Equal(t, beforeNodes, afterNodes, "full selected raw node frames did not change")
	require.ErrorIs(t, f.store.AtGeneration(w.commit).AddBatchChecked([]*graph.Node{{ID: path + "::Forbidden", FilePath: path, Kind: graph.KindFunction}}, nil), store_sqlite.ErrPayloadGenerationSealed)
	after, err := scanner.scan(t.Context(), []string{path})
	require.NoError(t, err)
	require.True(t, after.Settled)
	// Restore disk bytes separately; the assertion below concerns selected
	// physical rows, never a bypass of pinned-source verification.
	require.NoError(t, os.WriteFile(changedPath, original, 0600))
	t.Logf("winner before=%+v after=%+v, unrouted=%d", before.Stamps[path], after.Stamps[path], next)
	require.Equal(t, before.Stamps[path], after.Stamps[path], "a write outside the leased selected generation cannot tear its core read")
}

func TestContractStampPublishedPendingRowsAndMutableABAStayGuarded(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "mutable_base"
		if published {
			name = "published_positive"
		}
		t.Run(name, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			const path = "fixture/consts.go"
			handle, actor := f.store, ""
			var selected graph.Reader = f.store
			scanner := newFixtureScanner(t, f)
			if published {
				files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
				w := newContractLinkedWorktree(t, f, files, "routes.go", "consts.go", "helper.go")
				view := w.view(t)
				inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
				require.NoError(t, err)
				var handles []*store_sqlite.Store
				if view.ComposesBaseCorpus() {
					handles = append(handles, f.store.AtGeneration(0))
				}
				for _, source := range view.GenerationSources() {
					handles = append(handles, source.Handle)
				}
				scanner = newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": handles}, nil)
				handle, actor = f.store.AtGeneration(w.commit), contractLinkedCheckout
				selected = view.Reader
			}
			before, err := scanner.scan(t.Context(), []string{path})
			require.NoError(t, err)
			require.True(t, before.Stamps[path].Accepted)
			receipt, found, err := handle.ContractBoundaryReceiptContext(t.Context(), "fixture", actor, path)
			require.NoError(t, err)
			require.True(t, found)
			nodesBefore, err := graph.ContractFileNodesContext(t.Context(), selected, "fixture", []string{path})
			require.NoError(t, err)
			if published {
				// A published payload refuses receipt mutations as well as core
				// mutations; this is the immutability the scanner relies on.
				pending := *receipt
				pending.Accepted = false
				require.ErrorIs(t, handle.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}), store_sqlite.ErrPayloadGenerationSealed)
				after, err := scanner.scan(t.Context(), []string{path})
				require.NoError(t, err)
				require.Equal(t, before.Stamps[path], after.Stamps[path])
				return
			}
			pending := *receipt
			pending.Accepted = false
			pending.Previous = nil
			require.NoError(t, handle.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}))
			during, err := scanner.scan(t.Context(), []string{path})
			require.NoError(t, err)
			require.False(t, during.Stamps[path].Accepted)
			require.True(t, during.Stamps[path].Writing, "pending rows retain the original open-window proof even in a published generation")
			require.Positive(t, during.Stamps[path].Seq)
			require.False(t, contractCoverageBracketHolds(before, during, []string{path}))
			if published {
				require.ErrorIs(t, handle.AddBatchChecked([]*graph.Node{{ID: path + "::Forbidden", FilePath: path, Kind: graph.KindFunction}}, nil), store_sqlite.ErrPayloadGenerationSealed)
			}
			require.NoError(t, handle.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}))
			after, err := scanner.scan(t.Context(), []string{path})
			require.NoError(t, err)
			require.True(t, after.Stamps[path].Accepted)
			nodesAfter, err := graph.ContractFileNodesContext(t.Context(), selected, "fixture", []string{path})
			require.NoError(t, err)
			require.Equal(t, nodesBefore, nodesAfter)
			if published {
				require.Equal(t, before.Stamps[path], after.Stamps[path], "metadata-only ABA cannot tear sealed core rows")
			}
			if !published {
				require.Greater(t, after.Stamps[path].Seq, before.Stamps[path].Seq)
				require.False(t, contractCoverageBracketHolds(before, after, []string{path}), "same-identity mutable ABA remains a refused proof")
			}
			// A different source identity is never hidden by the sealed-core proof.
			pending.SourceFingerprint = contractInputHash([]byte("changed source identity"))
			require.NoError(t, handle.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}))
			require.NoError(t, handle.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}))
			moved, err := scanner.scan(t.Context(), []string{path})
			require.NoError(t, err)
			require.NotEqual(t, after.Stamps[path].SourceFingerprint, moved.Stamps[path].SourceFingerprint)
			require.False(t, contractCoverageBracketHolds(after, moved, []string{path}))
		})
	}
}

func TestContractStampPendingBeforePublicationKeepsItsOpenWindow(t *testing.T) {
	f := newRouteConstantFixture(t)
	const path = "fixture/consts.go"
	generation, handle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "pending-proof-control", CheckoutID: contractLinkedCheckout, LayerID: "pending", GenerationKind: "commit", CreatedAt: 1})
	require.NoError(t, err)
	row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
	require.NoError(t, err)
	require.True(t, found)
	pending := *row
	pending.CheckoutID = contractLinkedCheckout
	pending.Accepted = false
	pending.Previous = nil
	require.NoError(t, handle.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{pending}))
	require.NoError(t, f.store.PublishPayloadGeneration(t.Context(), generation, 2000))
	// This deliberately partial fixture has no selected-input authority. It
	// checks that publication alone cannot bless a pending row's core writes.
	inputs := &graphview.SelectedContractInputs{Witnesses: []graph.ContractInputWitness{{GenerationID: generation, State: graph.ContractInputState{RepoPrefix: "fixture", CheckoutID: contractLinkedCheckout}}}}
	scanner := newContractStampScanner(inputs, map[string][]*store_sqlite.Store{"fixture": {handle}}, nil)
	scan, err := scanner.scan(t.Context(), []string{path})
	require.NoError(t, err)
	require.False(t, scan.Settled)
	stamp := scan.Stamps[path]
	require.Equal(t, generation, stamp.Generation)
	require.False(t, stamp.Accepted)
	require.Positive(t, stamp.Seq)
	require.True(t, stamp.Writing)
	require.False(t, stamp.quiescent(), "published catalog state never certifies a pending receipt")
}
