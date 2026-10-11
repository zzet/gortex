package indexer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// laneRecordingNotifier records, for every analysis, whether it ran on the
// lifecycle's analysis lane and whether an edit cycle was holding the build
// lane at that moment.
type laneRecordingNotifier struct {
	lc          *CheckoutLifecycle
	mu          sync.Mutex
	invalidated int
	onLane      []bool
	duringEdit  []bool
}

func (n *laneRecordingNotifier) InvalidateSessionScopes() {
	n.mu.Lock()
	n.invalidated++
	n.mu.Unlock()
}

func (n *laneRecordingNotifier) RunAnalysis() {
	lane := n.lc.analysis
	lane.mu.Lock()
	running := lane.running
	lane.mu.Unlock()
	edit := n.lc.editCycleHoldsBuildLane()
	n.mu.Lock()
	n.onLane = append(n.onLane, running)
	n.duringEdit = append(n.duringEdit, edit)
	n.mu.Unlock()
}

func (n *laneRecordingNotifier) runs() ([]bool, []bool, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]bool(nil), n.onLane...), append([]bool(nil), n.duringEdit...), n.invalidated
}

func shortAnalysisQuiet(t *testing.T) {
	t.Helper()
	oldQuiet, oldPoll := analysisQuiet, analysisEditCyclePoll
	analysisQuiet, analysisEditCyclePoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { analysisQuiet, analysisEditCyclePoll = oldQuiet, oldPoll })
}

// TestTrackedSetAnalysisRunsOnTheLaneAndWaitsOutEditCycles: a tracked-set
// change invalidates the session scopes at once and hands the whole-graph
// analysis to the maintenance lane, which starts it only once no edit cycle
// holds the build lane; the caller never runs it.
func TestTrackedSetAnalysisRunsOnTheLaneAndWaitsOutEditCycles(t *testing.T) {
	shortAnalysisQuiet(t)
	f := newLifecycleFixture(t)
	defer f.close()
	n := &laneRecordingNotifier{lc: f.lc}
	f.lc.SetNotifier(n)
	var editing atomic.Bool
	editing.Store(true)
	f.lc.analysisEditCycle = editing.Load

	f.lc.notifyTrackedSetChanged()
	onLane, _, invalidated := n.runs()
	require.Equal(t, 1, invalidated, "the session scopes are invalidated at once")
	require.Empty(t, onLane, "the caller ran the analysis")

	time.Sleep(150 * time.Millisecond)
	onLane, _, _ = n.runs()
	require.Empty(t, onLane, "the analysis started while an edit cycle held the build lane")

	editing.Store(false)
	require.True(t, f.lc.waitAnalysisIdle(10*time.Second))
	onLane, duringEdit, _ := n.runs()
	require.Equal(t, []bool{true}, onLane, "the analysis must run once, on the lane")
	require.Equal(t, []bool{false}, duringEdit, "the analysis ran inside an edit cycle")
	stats := f.lc.analysisStats()
	require.GreaterOrEqual(t, stats.EditCycleWaits, 1, "the lane did not record waiting out the edit cycle")
}

// TestFamilyChangeWithoutARepositorySetChangeRunsNoAnalysis: a family
// reconcile that forgot a worktree without moving the tracked repository set
// invalidates the session scopes and runs no whole-graph analysis; one that
// moved the set runs it.
func TestFamilyChangeWithoutARepositorySetChangeRunsNoAnalysis(t *testing.T) {
	shortAnalysisQuiet(t)
	f := newLifecycleFixture(t)
	defer f.close()
	n := &laneRecordingNotifier{lc: f.lc}
	f.lc.SetNotifier(n)

	f.lc.notifyFamilyChanged(f.lc.repoSetFingerprint())
	require.True(t, f.lc.waitAnalysisIdle(10*time.Second))
	onLane, _, invalidated := n.runs()
	require.Equal(t, 1, invalidated)
	require.Empty(t, onLane, "a checkout-only family change ran the whole-graph analysis")
	require.Equal(t, 1, f.lc.analysisStats().SkippedUnchanged)

	f.lc.notifyFamilyChanged("a repository set that is gone")
	require.True(t, f.lc.waitAnalysisIdle(10*time.Second))
	onLane, _, _ = n.runs()
	require.Equal(t, []bool{true}, onLane, "a family change that moved the repository set must rerun the analysis")
}

// TestTrackedSetAnalysisCoalescesABurst: a burst of changes costs one pass.
func TestTrackedSetAnalysisCoalescesABurst(t *testing.T) {
	shortAnalysisQuiet(t)
	f := newLifecycleFixture(t)
	defer f.close()
	n := &laneRecordingNotifier{lc: f.lc}
	f.lc.SetNotifier(n)
	for i := 0; i < 5; i++ {
		f.lc.notifyTrackedSetChanged()
	}
	require.True(t, f.lc.waitAnalysisIdle(10*time.Second))
	onLane, _, invalidated := n.runs()
	require.Equal(t, 5, invalidated)
	require.Equal(t, []bool{true}, onLane)
}
