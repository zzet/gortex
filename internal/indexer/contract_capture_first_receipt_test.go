package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// stageFirstReceipt replaces path's accepted receipt row with an unaccepted
// one that has no accepted predecessor: the row a file's first save stages,
// and the row a forced reindex writes for a file whose earlier row an
// interrupted process never accepted. The write opens its core-write window.
func stageFirstReceipt(t *testing.T, f *contractJobFixture, path string) {
	t.Helper()
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
	require.NoError(t, err)
	require.NotNil(t, row)
	_, err = parityOpenRaw(t, f.store).Exec(`DELETE FROM generation_contract_boundary_receipt WHERE view_gen = 0 AND repo_prefix = ? AND checkout_id = '' AND file_path = ?`, "fixture", path)
	require.NoError(t, err)
	f.store.ForgetContractReceiptWritesForTest("fixture", "", []string{path})
	staged := *row
	staged.Accepted, staged.Previous = false, nil
	require.NoError(t, f.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{staged}))
	stored, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
	require.NoError(t, err)
	require.False(t, stored.Accepted)
	require.Nil(t, stored.Previous, "the row has no accepted predecessor")
	require.True(t, f.store.ContractReceiptWriteStates("fixture", "", []string{path})[path].Writing, "the write opened its window")
}

// firstReceiptCapture runs one production primary capture of the fixture,
// recording what it reports to the repair.
func firstReceiptCapture(t *testing.T, f *contractJobFixture, suspects *[][]string) error {
	t.Helper()
	options := f.options
	options.SuspectStuck = func(_ string, paths []string) { *suspects = append(*suspects, paths) }
	inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
	if err == nil {
		snapshot.Release()
	}
	return err
}

// A census file whose only receipt row is unaccepted, with no accepted
// predecessor, belongs to a save of this process still to be accepted.
// Whether its core-write window is open or its writes have ended, the capture
// refuses it as stale, so the request waits for the save's accept; it does not
// fail as an incomplete projection, and nothing is reported to the repair.
// Once the save accepts the row, the capture admits it.
func TestContractCaptureRefusesAnUnacceptedFirstReceiptAsStale(t *testing.T) {
	const owner = "fixture/consts.go"
	f := newRouteConstantFixture(t)
	var suspects [][]string
	require.NoError(t, firstReceiptCapture(t, f, &suspects))

	stageFirstReceipt(t, f, owner)
	err := firstReceiptCapture(t, f, &suspects)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "a first receipt in flight is waited for")
	require.NotErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Empty(t, suspects, "a save in flight is no repair's")

	f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
	err = firstReceiptCapture(t, f, &suspects)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "a first receipt whose writes ended is waited for")
	require.NotErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Empty(t, suspects, "a save still to be accepted is no repair's")

	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.NoError(t, f.store.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{*row}))
	require.NoError(t, firstReceiptCapture(t, f, &suspects), "the save's accept admits the row")
}

// A first receipt whose mutation settled was overtaken and will never be
// accepted: the capture keeps refusing it as incomplete instead of waiting.
func TestContractCaptureKeepsRefusingASettledFirstReceiptAsIncomplete(t *testing.T) {
	const owner = "fixture/consts.go"
	f := newRouteConstantFixture(t)
	var suspects [][]string
	stageFirstReceipt(t, f, owner)
	f.store.SettleContractReceiptWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
	require.True(t, f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner].Settled)
	err := firstReceiptCapture(t, f, &suspects)
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.NotErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Empty(t, suspects, "a settled row is no repair's")
}

// A linked actor's stale capture is re-enqueued at once, not on the save's
// Published, so it keeps the earlier refusal of a primary first receipt
// instead of recapturing until the accept; the primary capture of the same
// store waits for it.
func TestContractLinkedCaptureKeepsRefusingAPrimaryFirstReceipt(t *testing.T) {
	const helper = "fixture/helper.go"
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "consts.go")
	view := w.view(t)
	stageFirstReceipt(t, f, helper)
	f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{helper}))
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(f.options)(t.Context(), view, inputs, "fixture", contractLinkedCheckout)
	if err == nil {
		snapshot.Release()
	}
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete, "a linked capture keeps its earlier refusal")
	require.NotErrorIs(t, err, graph.ErrContractProjectionStale)
	var suspects [][]string
	require.ErrorIs(t, firstReceiptCapture(t, f, &suspects), graph.ErrContractProjectionStale, "the primary capture waits")
}
