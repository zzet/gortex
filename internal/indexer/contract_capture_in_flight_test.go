package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestContractCaptureAdmitsAFileInFlight: an apply staged the route's owner's
// receipt and holds its core-write window open when the job is captured. The
// one capture admits the file in flight instead of refusing it; the job's
// first read of the owner waits for the window, the apply's end wakes it, and
// the publish equals a cold build.
func TestContractCaptureAdmitsAFileInFlight(t *testing.T) {
	const owner = "fixture/consts.go"
	f := newRouteConstantFixture(t)
	f.stageInFlight(owner)
	captures := 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		captures++
		require.Equal(t, []string{owner}, snapshot.InFlight, "the capture names the file it admitted in flight")
	}
	closed := false
	f.onWait = func() {
		if !closed {
			closed = true
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{owner}))
		}
	}
	woke := 0
	report, err := f.job(&contractConvergenceHooks{roundWait: 5 * time.Second, woke: func() { woke++ }}, contractJobDeadline(time.Minute))
	f.onWait = nil
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 1, captures, "one capture")
	require.True(t, closed, "the job waited for the in-flight window")
	require.GreaterOrEqual(t, woke, 1, "the window's close woke the job")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})), contractMeta(f.contracts()))
}

// TestContractCaptureInFlightAdmissionRequiresAScan: a file in flight is
// admitted only into a job that takes stamp scans. The row rule refuses it
// without one, and a captured snapshot that names a file in flight and is
// then frozen (no Scan: every census file read as captured) is refused before
// the job reads anything: it could not wait for the window.
func TestContractCaptureInFlightAdmissionRequiresAScan(t *testing.T) {
	t.Run("rule", func(t *testing.T) {
		accepted := &graph.ContractBoundaryReceipt{Accepted: true}
		staged := &graph.ContractBoundaryReceipt{}
		deleted := &graph.ContractBoundaryReceipt{Deleted: true}
		for _, row := range []struct {
			name    string
			row     *graph.ContractBoundaryReceipt
			state   store_sqlite.ContractReceiptWriteState
			scanned bool
			want    contractCaptureAdmission
		}{
			{"accepted", accepted, store_sqlite.ContractReceiptWriteState{}, false, contractCaptureAdmitted},
			{"accepted_after_restart", accepted, store_sqlite.ContractReceiptWriteState{}, true, contractCaptureAdmitted},
			{"settled", staged, store_sqlite.ContractReceiptWriteState{Seq: 3, Settled: true}, false, contractCaptureAdmitted},
			{"written", staged, store_sqlite.ContractReceiptWriteState{Seq: 3}, true, contractCaptureAdmitted},
			{"in_flight_scanned", staged, store_sqlite.ContractReceiptWriteState{Seq: 3, Writing: true}, true, contractCaptureInFlight},
			{"in_flight_frozen", staged, store_sqlite.ContractReceiptWriteState{Seq: 3, Writing: true}, false, contractCaptureStale},
			{"left_by_an_earlier_process", staged, store_sqlite.ContractReceiptWriteState{}, true, contractCaptureStuck},
			{"deleted", deleted, store_sqlite.ContractReceiptWriteState{Seq: 3}, true, contractCaptureStuck},
		} {
			require.Equal(t, row.want, contractCaptureRowAdmission(row.row, row.state, row.scanned), row.name)
		}
	})
	t.Run("frozen_snapshot", func(t *testing.T) {
		const owner = "fixture/consts.go"
		f := newRouteConstantFixture(t)
		f.stageInFlight(owner)
		f.adjust = func(snapshot *ContractFollowupSnapshot) {
			require.Equal(t, []string{owner}, snapshot.InFlight)
			snapshot.Scan = nil
		}
		read := false
		report, err := f.job(&contractConvergenceHooks{beforeExtract: func(string) { read = true }}, contractJobDeadline(time.Minute))
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
		require.False(t, report.Published)
		require.False(t, read, "nothing was read from the frozen snapshot")
	})
}

// TestContractCaptureReportsAStuckRowAsASuspect: a census row no live
// mutation of this process will move (here a deleted row of a file the
// census still holds) is refused and reported as a suspect for the repair;
// an in-flight row is admitted and reported nowhere.
func TestContractCaptureReportsAStuckRowAsASuspect(t *testing.T) {
	const owner = "fixture/consts.go"
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
	f.stageInFlight(owner)
	require.NoError(t, capture())
	require.Empty(t, suspects, "a row in flight is no suspect")

	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	gone := *row
	gone.Accepted, gone.Deleted, gone.Previous = false, true, nil
	require.NoError(t, f.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{gone}))
	require.ErrorIs(t, capture(), graph.ErrContractProjectionStale)
	require.Equal(t, [][]string{{owner}}, suspects, "the stuck row is reported once")
}

// TestContractJobLinkedWorktreeCapturedWithABaseFileInFlightEqualsCold is the
// worktree chain's in-flight row: a linked worktree's actor reads its own
// commit generation over the base corpus, and a primary apply holds the
// window of a base file the worktree did not change open when the actor's job
// is captured. The capture admits it in flight, the job waits for the window,
// and the publish equals a cold build of the worktree.
func TestContractJobLinkedWorktreeCapturedWithABaseFileInFlightEqualsCold(t *testing.T) {
	const base = "fixture/routes.go"
	f := newRouteConstantFixture(t)
	worktree := map[string]string{"routes.go": jobRoutes, "consts.go": "package fixture\nconst RoutePath = \"/linked\"\n", "helper.go": jobHelper}
	w := newContractLinkedWorktree(t, f, worktree, "consts.go")
	f.stageInFlight(base)
	closed := false
	f.onWait = func() {
		if !closed {
			closed = true
			f.store.EndContractCoreWrites("fixture", "", f.store.ContractReceiptWriteSeqs("fixture", "", []string{base}))
		}
	}
	view := w.view(t)
	report, err := w.job(t, view, &contractConvergenceHooks{roundWait: 5 * time.Second})
	f.onWait = nil
	require.NoError(t, err)
	require.True(t, report.Published)
	require.True(t, closed, "the job waited for the base file's window")
	require.Equal(t, contractIDs(coldContracts(t, worktree)), contractIDs(w.contracts(t, view)))
}

// TestContractCaptureSourceIsReleasedOnEveryReturn: a prepared capture source
// holds the view's generations independently of the view. A capture that
// takes it releases it with its snapshot, or at once when it fails; a source
// no capture took is released by its owner, once. Once the view itself is
// closed, nothing holds its generations.
func TestContractCaptureSourceIsReleasedOnEveryReturn(t *testing.T) {
	f := newRouteConstantFixture(t)
	w := newContractLinkedWorktree(t, f, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts + "// linked\n", "helper.go": jobHelper}, "consts.go")
	leases := f.options.Materializer.Leases
	capture := NewContractFollowupSourceCapture(f.options)
	for _, row := range []string{"captured", "refused", "never_taken"} {
		t.Run(row, func(t *testing.T) {
			view, err := f.options.Materializer.MaterializeCheckout(t.Context(), contractLinkedCheckout)
			require.NoError(t, err)
			inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
			require.NoError(t, err)
			prepared, err := PrepareContractCaptureSource(view)
			require.NoError(t, err)
			view.Close()
			require.True(t, leases.InUse(w.commit), "the source holds the view's generations after the view closed")
			switch row {
			case "captured":
				snapshot, targets, err := capture(t.Context(), prepared, inputs, "fixture", contractLinkedCheckout)
				require.NoError(t, err)
				require.Len(t, targets, 1)
				require.True(t, leases.InUse(w.commit), "the snapshot holds them")
				snapshot.Release()
			case "refused":
				_, _, err := capture(t.Context(), prepared, nil, "fixture", contractLinkedCheckout)
				require.ErrorIs(t, err, graph.ErrContractInputVector)
			case "never_taken":
				prepared.Release()
				prepared.Release()
			}
			require.False(t, leases.InUse(w.commit), "nothing holds the generations")
		})
	}
	none, err := PrepareContractCaptureSource(nil)
	require.NoError(t, err)
	require.Nil(t, none, "a primary actor has no source")
	none.Release()
}

// contractInFlightWait is a job's Published wait for a fixture without a
// Published source: its first call ends the in-flight apply of path (its
// window closes) and reports an event; later calls wait out their deadline.
func contractInFlightWait(store *store_sqlite.Store, repo, path string, closed *bool) func(context.Context, uint64, time.Time) bool {
	return func(ctx context.Context, _ uint64, until time.Time) bool {
		if !*closed {
			*closed = true
			store.EndContractCoreWrites(repo, "", store.ContractReceiptWriteSeqs(repo, "", []string{path}))
			return true
		}
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		return false
	}
}
