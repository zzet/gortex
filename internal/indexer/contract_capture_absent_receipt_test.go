package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// deleteContractReceiptRow removes a primary file's boundary receipt row
// outright, the state a namespace baseline accepted without ever writing a
// row for the file leaves behind.
func deleteContractReceiptRow(t *testing.T, f *contractJobFixture, path string) {
	t.Helper()
	_, err := parityOpenRaw(t, f.store).Exec(`DELETE FROM generation_contract_boundary_receipt WHERE view_gen = 0 AND repo_prefix = ? AND checkout_id = '' AND file_path = ?`, "fixture", path)
	require.NoError(t, err)
	f.store.ForgetContractReceiptWritesForTest("fixture", "", []string{path})
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
	require.NoError(t, err)
	require.Nil(t, row, "the receipt row is gone")
}

// A census file with no receipt row at all cannot be admitted, and nothing a
// live mutation does will write one: its file did not move. The capture
// reports it to the repair and refuses as stale, so the request waits for the
// repair's publication instead of failing for good on an empty source proof.
func TestContractCaptureReportsAnAbsentReceiptAsASuspect(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newRouteConstantFixture(t)
	var suspects [][]string
	options := f.options
	options.SuspectStuck = func(repo string, paths []string) {
		require.Equal(t, "fixture", repo)
		suspects = append(suspects, paths)
	}
	capture := func() error {
		inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
		require.NoError(t, err)
		snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
		if err == nil {
			snapshot.Release()
		}
		return err
	}
	require.NoError(t, capture())
	require.Empty(t, suspects)

	deleteContractReceiptRow(t, f, helper)
	err := capture()
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.NotErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Equal(t, [][]string{{helper}}, suspects, "the absent row is reported once")
}

// TestContractAbsentReceiptIsRepaired: a file whose receipt row is absent
// blocks every capture of its cohort until something writes the row. A route
// edit of another file and one strict call later, the repair has reparsed the
// file, its row is back and accepted, and the answer equals a cold build.
func TestContractAbsentReceiptIsRepaired(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newContractRepairFixture(t, nil)
	f.strict(t, time.Minute)
	repaired := repairCount("stuck_receipt")
	deleteContractReceiptRow(t, f.contractJobFixture, helper)
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	f.save("routes.go", routes)
	got := f.strict(t, 20*time.Second)
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": jobConsts, "helper.go": jobHelper})), contractIDs(got))
	require.Equal(t, repaired+1, repairCount("stuck_receipt"), "one forced reindex wrote the row")
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", helper)
	require.NoError(t, err)
	require.NotNil(t, row, "the repair wrote the row again")
	require.True(t, row.Accepted)
}

// A file with no row is repaired only while it is on disk: a deleted file
// needs no row, and its deletion takes it out of the census.
func TestContractRepairTakesAnAbsentRowOnlyForAFileOnDisk(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newContractRepairFixture(t, nil)
	store := f.store.AtGeneration(0)
	require.False(t, f.r.absentRow(t.Context(), store, "fixture", helper), "a file with its row is not absent")
	deleteContractReceiptRow(t, f.contractJobFixture, helper)
	require.True(t, f.r.absentRow(t.Context(), store, "fixture", helper))
	require.NoError(t, os.Remove(filepath.Join(f.root, "helper.go")))
	require.False(t, f.r.absentRow(t.Context(), store, "fixture", helper), "a deleted file needs no row")
}

// absentReceiptCapture runs one production primary capture of the fixture,
// recording what it reports to the repair.
func absentReceiptCapture(t *testing.T, f *contractJobFixture) ([][]string, error) {
	t.Helper()
	var suspects [][]string
	options := f.options
	options.SuspectStuck = func(_ string, paths []string) { suspects = append(suspects, paths) }
	inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
	if err == nil {
		snapshot.Release()
	}
	return suspects, err
}

// Only a missing row in a current baseline is a repair's. A namespace whose
// baseline is of an older receipt version, as a version change leaves every
// namespace, is the baseline rebuild's to replace: it never turns into one
// forced reindex of the whole repository.
func TestContractCaptureLeavesAnOlderBaselineToTheRebuild(t *testing.T) {
	const helper = "fixture/helper.go"
	repair := newContractRepairFixture(t, nil)
	f := repair.contractJobFixture
	repair.strict(t, time.Minute)
	deleteContractReceiptRow(t, f, helper)
	store := f.store.AtGeneration(0)
	require.True(t, repair.r.absentRow(t.Context(), store, "fixture", helper), "a missing row in a current baseline is the repair's")
	_, err := parityOpenRaw(t, f.store).Exec(`UPDATE generation_contract_boundary_baseline SET version = 'older' WHERE view_gen = 0 AND repo_prefix = ? AND checkout_id = ''`, "fixture")
	require.NoError(t, err)
	suspects, err := absentReceiptCapture(t, f)
	require.NotErrorIs(t, err, graph.ErrContractProjectionStale, "an older baseline is not waited on: %v", err)
	require.Empty(t, suspects, "a missing row under an older baseline is not reported as absent")
	require.False(t, repair.r.absentRow(t.Context(), store, "fixture", helper), "the repair does not take it either")
}

// A capture reports an absent row only for a file the repair would take: one
// still on disk. A census file already gone from disk keeps the capture's
// earlier refusal, so no request waits on a repair that declines it.
func TestContractCaptureReportsAnAbsentRowOnlyForAFileOnDisk(t *testing.T) {
	const helper = "fixture/helper.go"
	f := newRouteConstantFixture(t)
	deleteContractReceiptRow(t, f, helper)
	require.NoError(t, os.Remove(filepath.Join(f.root, "helper.go")))
	suspects, err := absentReceiptCapture(t, f)
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Empty(t, suspects)
}

// A linked actor's capture reports an absent primary row of a file on the
// primary disk to the repair, but keeps its earlier refusal: it fails once,
// and the repair's accept moves the primary witness its inputs select. The
// primary capture of the same store reports it and waits instead.
func TestContractLinkedCaptureReportsAnAbsentPrimaryRowAndRefuses(t *testing.T) {
	const helper = "fixture/helper.go"
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "consts.go")
	view := w.view(t)
	deleteContractReceiptRow(t, f, helper)
	var suspects [][]string
	options := f.options
	options.SuspectStuck = func(_ string, paths []string) { suspects = append(suspects, paths) }
	inputs, err := options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), view, inputs, "fixture", contractLinkedCheckout)
	if err == nil {
		snapshot.Release()
	}
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete, "a linked capture keeps its earlier refusal")
	require.Equal(t, [][]string{{helper}}, suspects, "a linked capture still reports the row to the primary repair")
	suspects, err = absentReceiptCapture(t, f)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Equal(t, [][]string{{helper}}, suspects)
}

// A linked actor's file that only its worktree has is not on the primary
// disk: the linked capture never reports it to the primary repair.
func TestContractLinkedCaptureDoesNotReportAWorktreeOnlyFile(t *testing.T) {
	const helper = "fixture/helper.go"
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "consts.go")
	view := w.view(t)
	deleteContractReceiptRow(t, f, helper)
	require.NoError(t, os.Remove(filepath.Join(f.root, "helper.go")))
	var suspects [][]string
	options := f.options
	options.SuspectStuck = func(_ string, paths []string) { suspects = append(suspects, paths) }
	inputs, err := options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), view, inputs, "fixture", contractLinkedCheckout)
	if err == nil {
		snapshot.Release()
	}
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Empty(t, suspects, "a file the primary disk does not have is not the primary repair's")
}

// A repair whose reindex never writes the absent row is bounded like any
// stuck receipt: three forced reindexes, then a failure envelope excludes the
// file, and the job publishes equal to a cold build without it.
func TestContractAbsentReceiptRepairIsBoundedByTheEnvelope(t *testing.T) {
	const helper = "fixture/helper.go"
	var calls atomic.Int64
	var dropWrites atomic.Bool
	f := newContractRepairFixture(t, func(_ *contractRepairFixture, r *ContractStuckRepair) {
		r.reindex = func(ctx context.Context, repo string, paths []string) error {
			if dropWrites.Load() {
				calls.Add(1)
				return nil
			}
			return r.realReindex(ctx, repo, paths)
		}
	})
	f.strict(t, time.Minute)
	repaired, enveloped := repairCount("stuck_receipt"), repairCount("stuck_envelope")
	dropWrites.Store(true)
	deleteContractReceiptRow(t, f.contractJobFixture, helper)
	routes := "package fixture\nfunc register(r Router) { r.GET(RoutePath, users); r.POST(\"/edited\", users) }\nfunc users() {}\n"
	f.save("routes.go", routes)
	got := f.strict(t, 30*time.Second)
	require.Equal(t, contractIDs(coldContracts(t, map[string]string{"routes.go": routes, "consts.go": jobConsts})), contractIDs(got))
	require.EqualValues(t, 3, calls.Load(), "three forced reindexes before the envelope")
	require.Equal(t, repaired+3, repairCount("stuck_receipt"))
	require.Equal(t, enveloped+1, repairCount("stuck_envelope"))
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", helper)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.True(t, row.Accepted && contractReceiptIsFailureEnvelope(row), "the file is excluded by an accepted envelope")
}
