package store_sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func openEstimateStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addRepoNodes(t *testing.T, s *store_sqlite.Store, repo string, n int) {
	t.Helper()
	batch := make([]*graph.Node, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, &graph.Node{
			ID:         fmt.Sprintf("%s/a.go::F%d", repo, i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("F%d", i),
			FilePath:   repo + "/a.go",
			RepoPrefix: repo,
		})
	}
	s.AddBatch(batch, nil)
}

// The estimates must come from the persisted counters, not from a corpus
// scan. Making the counter disagree with the stored nodes is the only way to
// tell the two sources apart from the outside — and it is exactly the drift
// the --exact path exists to detect.
func TestAllRepoMemoryEstimatesReadsCountersNotTheCorpus(t *testing.T) {
	s := openEstimateStore(t)
	addRepoNodes(t, s, "r1", 40)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: 7, EdgeCount: 3,
	}))

	got := s.AllRepoMemoryEstimates()
	require.Equal(t, 7, got["r1"].NodeCount,
		"estimates must report the counter, not a recount of the 40 stored nodes")
	require.Equal(t, 3, got["r1"].EdgeCount)
	require.NotZero(t, got["r1"].NodeBytes)

	// Byte estimates are the count times a fixed per-record figure, so they
	// have to track the counter they are derived from.
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r2", NodeCount: 14, EdgeCount: 6,
	}))
	got = s.AllRepoMemoryEstimates()
	require.Equal(t, 2*got["r1"].NodeBytes, got["r2"].NodeBytes)
	require.Equal(t, 2*got["r1"].EdgeBytes, got["r2"].EdgeBytes)
}

// A repo with no counter row contributes nothing rather than triggering a
// scan. On a real store that is the empty prefix and the synthetic
// external-call bucket; neither is a tracked repo.
func TestAllRepoMemoryEstimatesOmitsReposWithoutCounters(t *testing.T) {
	s := openEstimateStore(t)
	addRepoNodes(t, s, "r1", 5)
	addRepoNodes(t, s, "r2", 5)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: 5,
	}))

	got := s.AllRepoMemoryEstimates()
	require.Len(t, got, 1)
	require.Contains(t, got, "r1")
	require.NotContains(t, got, "r2")
}

// The scan is the audit path: it reports what the corpus actually holds, so a
// counter that has drifted is visible by comparing the two.
func TestScanRepoMemoryEstimatesReportsTheCorpusAndExposesDrift(t *testing.T) {
	s := openEstimateStore(t)
	addRepoNodes(t, s, "r1", 40)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: 7,
	}))

	scanned, err := s.ScanRepoMemoryEstimates(context.Background())
	require.NoError(t, err)
	require.Equal(t, 40, scanned["r1"].NodeCount, "the scan must recount the corpus")

	counters := s.AllRepoMemoryEstimates()
	require.NotEqual(t, counters["r1"].NodeCount, scanned["r1"].NodeCount,
		"the two sources must be distinguishable, otherwise --exact could not detect drift")
}

// A scan that runs out of budget must report the failure. Returning the short
// map it had built would hand back a plausible-looking wrong count — the
// defect the bounded-scan design walked into, now impossible by construction
// because the caller owns the deadline and gets an error instead of numbers.
func TestScanRepoMemoryEstimatesFailsRatherThanTruncating(t *testing.T) {
	s := openEstimateStore(t)
	addRepoNodes(t, s, "r1", 2)
	addRepoNodes(t, s, "r2", 3)

	// A 1 ns timer can fire after a fast scan completes, especially on
	// platforms with coarse clock/timer resolution. Inject the failure before
	// querying so this checks the error contract, not a scheduling race.
	for _, tc := range []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		wantErr    error
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "expired_deadline",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Unix(0, 0))
			},
			wantErr: context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.newContext()
			defer cancel()
			require.ErrorIs(t, ctx.Err(), tc.wantErr, "the scan must start with the failure already observable")
			got, err := s.ScanRepoMemoryEstimates(ctx)
			require.ErrorIs(t, err, tc.wantErr, "an unfinished scan must not be reported as a result")
			require.Nil(t, got, "failed scans must not expose partial estimates")
		})
	}
}

// Whatever the scan reports can be written straight back, which is how
// --exact heals a drifted counter.
func TestScanResultCanHealTheCounters(t *testing.T) {
	s := openEstimateStore(t)
	addRepoNodes(t, s, "r1", 40)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "r1", NodeCount: 7,
	}))

	scanned, err := s.ScanRepoMemoryEstimates(context.Background())
	require.NoError(t, err)
	require.NoError(t, s.ReconcileRepoCounters(scanned))

	require.Equal(t, 40, s.AllRepoMemoryEstimates()["r1"].NodeCount,
		"counters must agree with the corpus after a reconcile")
}
