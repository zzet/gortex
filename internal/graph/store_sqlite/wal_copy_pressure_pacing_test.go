package store_sqlite

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A copy budget below the writers' rate, while a checkout is being edited:
// under the pressure mark the copy is paced and falls behind, so the log
// grows to the mark; over it the copy is not paced, and the log is reset and
// stays bounded. Scaled: threshold 4 MiB, pressure mark 16 MiB (the fold's
// mark stays 1 GiB, so only the pressure mark can end the pacing here), a
// budget of 0.5 MiB/s against a writer of ~4 MiB/s, and edits of 300 ms
// every 2.2 s. When only the fold's mark ended the pacing, no attempt
// converged: the log reached 61 MiB in 15 s with no reset.
func TestTheCopyIsNotPacedOverThePressureMark(t *testing.T) {
	diag := newWindowsWALDiagnostic(t)
	diag.install()
	defer diag.finish()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)
	s, path := openWALReclaimStore(t)
	diag.store.Store(s)
	defer func() { _ = s.Close() }()
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	slowCopy(t, s, 30<<20) // 0.5 MiB/s while editing

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var maxWAL int64
	var writeErr error
	wg.Add(2)
	go func() { // the edits
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2200 * time.Millisecond):
			}
			lane.held.Store(true)
			time.Sleep(300 * time.Millisecond)
			lane.held.Store(false)
		}
	}()
	go func() { // about 4 MiB/s of page rewrites
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if err := churnWriteOnce(s, k); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
			mu.Lock()
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
			mu.Unlock()
		}
	}()
	time.Sleep(15 * time.Second)
	close(stop)
	wg.Wait()
	st, cp := s.WALReclaimStats(), s.WALCopyStats()
	t.Logf("wal_max=%.1fMiB resets=%d pressure_runs=%d pressure_resets=%d budget_wait=%s written=%.1fMiB",
		float64(maxWAL)/(1<<20), st.Resets, cp.PressureRuns, cp.PressureResets, cp.BudgetWait.Round(time.Millisecond), float64(cp.WrittenBytes)/(1<<20))
	require.NoError(t, writeErr)
	require.Positive(t, st.Resets, "the log was never reset with the budget below the writers' rate")
	require.Less(t, maxWAL, int64(32<<20), "the log outgrew twice its pressure mark")
}
