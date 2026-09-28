package store_sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProbeWALShrinkOnAMultiGigabyteLog measures, on a real multi-gigabyte
// log, what the reclaim's reset holds the writer for with the in-place reset
// and the sliced shrink, against a plain TRUNCATE of an equally large log, and
// the reader wait (ReaderWaitMark) readers pay meanwhile. An operator probe:
// skipped unless GORTEX_WAL_SHRINK_PROBE_DIR names a scratch directory
// (GORTEX_WAL_SHRINK_PROBE_MB sets the log size, default 3072).
func TestProbeWALShrinkOnAMultiGigabyteLog(t *testing.T) {
	dir := os.Getenv("GORTEX_WAL_SHRINK_PROBE_DIR")
	if dir == "" {
		t.Skip("set GORTEX_WAL_SHRINK_PROBE_DIR")
	}
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_ROW_COUNTERS", "0")
	sizeMB := 3072
	if v, err := strconv.Atoi(os.Getenv("GORTEX_WAL_SHRINK_PROBE_MB")); err == nil && v > 0 {
		sizeMB = v
	}
	ctx := context.Background()
	for _, mode := range []string{"truncate", "in_place"} {
		func() {
			path := filepath.Join(dir, "shrink-"+mode+".sqlite")
			s, err := openPristine(t, path)
			require.NoError(t, err)
			defer func() {
				_ = s.Close()
				for _, suffix := range []string{"", "-wal", "-shm", ".close-rate", ".close-progress"} {
					_ = os.Remove(path + suffix)
				}
			}()
			seedWALChurnTable(t, s)
			ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
			require.NoError(t, err)
			defer ckpt.Close()
			pinned, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var n int
			require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
			growStart := time.Now()
			for k := 0; walFileSize(path+"-wal") < int64(sizeMB)<<20; k++ {
				require.NoError(t, churnWriteOnce(s, k))
			}
			require.NoError(t, pinned.Rollback())
			walBytes := walFileSize(path + "-wal")
			t.Logf("[%s] log %.2f GiB grown in %s", mode, float64(walBytes)/(1<<30), time.Since(growStart).Round(time.Second))
			prevInPlace := walShrinkInPlaceBytes
			if mode == "truncate" {
				walShrinkInPlaceBytes = 1 << 62
			}
			defer func() { walShrinkInPlaceBytes = prevInPlace }()

			// Readers throughout: each read's own latency, and the mark.
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var maxRead atomic.Int64
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					start := time.Now()
					var c int
					_ = s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE id < 50`).Scan(&c)
					if d := int64(time.Since(start)); d > maxRead.Load() {
						maxRead.Store(d)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			mark := s.ReaderWaitMark()
			cfg := walReclaimConfig{thresholdBytes: 16 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
			start := time.Now()
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			resetElapsed := time.Since(start)
			var slices int
			shrinkStart := time.Now()
			if s.walShrinkNeeded() {
				slices, _, err = s.shrinkWAL(ctx, ckpt)
				require.NoError(t, err)
			}
			close(stop)
			wg.Wait()
			st := s.WALReclaimStats()
			t.Logf("SHRINK mode=%s wal_gib=%.2f outcome=%s reason=%q reset_writer_hold=%s reset_elapsed=%s slices=%d max_slice_hold=%s shrink_elapsed=%s wal_after_mib=%.0f reader_wait=%s max_read=%s",
				mode, float64(walBytes)/(1<<30), res.outcome, res.reason, res.writerHold.Round(time.Millisecond), resetElapsed.Round(time.Millisecond),
				slices, st.ShrinkSliceHoldMax.Round(time.Millisecond), time.Since(shrinkStart).Round(time.Millisecond),
				float64(walFileSize(path+"-wal"))/(1<<20), s.ReaderWaitMark().Since(mark).Round(time.Millisecond), time.Duration(maxRead.Load()).Round(time.Millisecond))
		}()
	}
}
