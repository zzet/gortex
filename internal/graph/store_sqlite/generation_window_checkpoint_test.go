package store_sqlite

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// setGenerationInlineCheckpointMaxFrames overrides the inline backlog budget
// for one test. The tests that use it are sequential, and parallel tests start
// only after every sequential test of the package has returned.
func setGenerationInlineCheckpointMaxFrames(t *testing.T, frames int64) {
	t.Helper()
	previous := generationInlineCheckpointMaxFrames
	generationInlineCheckpointMaxFrames = frames
	t.Cleanup(func() { generationInlineCheckpointMaxFrames = previous })
}

// seedUncheckpointedBacklog writes rows outside any window so the WAL holds a
// backlog of frames no checkpoint has copied yet (the writer runs with
// wal_autocheckpoint=0), and returns the backlog the wal-index reports.
func seedUncheckpointedBacklog(t *testing.T, store *Store, path string, nodes, edges int) int64 {
	t.Helper()
	n, e := bulkFixture(nodes, edges)
	store.AddBatch(n, e)
	snap, ok := readWALIndexSnapshot(path)
	if !ok {
		t.Fatal("wal-index snapshot unreadable after a write")
	}
	return snap.PendingFrames()
}

// Under the default policy, a generation window that crosses its
// row-checkpoint interval over any backlog must not copy that backlog on the publication
// path: it defers, backs the cadence off for the rest of the window, and the
// window end measures the residue from the wal-index and hands it to the
// maintenance lane's drain.
func TestGenerationWindowDefersALargeBacklogOffTheFlush(t *testing.T) {
	const line = 64
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", strconv.Itoa(line))
	path := filepath.Join(t.TempDir(), "deferred.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	pageSize := pragmaIntDB(t, store.db, "page_size")
	backlog := seedUncheckpointedBacklog(t, store, path, 2048, 4096)
	if backlog <= generationInlineCheckpointMaxFrames {
		t.Fatalf("seed left a backlog of %d frames, want more than the default %d-frame budget", backlog, generationInlineCheckpointMaxFrames)
	}

	var events []bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) { events = append(events, event) }

	const generationID = int64(7)
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
	}
	store.writeMu.Lock()
	store.bulkCheckpointEdgeRows = bulkCheckpointEdgeInterval - 1
	store.writeMu.Unlock()
	nodes, edges := bulkFixture(64, 128)
	handle := store.AtGeneration(generationID)
	if err := handle.AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	store.writeMu.Lock()
	backoff := store.bulkRowCheckpointBackoff
	store.writeMu.Unlock()
	if !backoff {
		t.Fatal("a deferred row-limit checkpoint did not back the cadence off")
	}
	// A second interval inside the same window stays silent.
	store.writeMu.Lock()
	store.bulkCheckpointEdgeRows = bulkCheckpointEdgeInterval - 1
	store.writeMu.Unlock()
	more, moreEdges := bulkFixture(64, 128)
	for _, n := range more {
		n.ID += "#2"
	}
	for _, e := range moreEdges {
		e.From += "#2"
	}
	if err := handle.AddBatchChecked(more, moreEdges); err != nil {
		t.Fatalf("AddBatchChecked (second interval): %v", err)
	}

	requestsBefore := store.walDrainRequests.Load()
	started := time.Now()
	if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
		t.Fatalf("EndGenerationBulkLoadFor: %v", err)
	}
	endElapsed := time.Since(started)

	var deferredRowLimit, deferredEnd, passive int
	for _, event := range events {
		switch {
		case event.Stage == "checkpoint_deferred" && event.Name == "row_limit":
			deferredRowLimit++
			if event.WALFrames <= 0 || event.EdgeRows < bulkCheckpointEdgeInterval {
				t.Errorf("deferred row-limit event lost its measurement: %+v", event)
			}
		case event.Stage == "checkpoint_deferred" && event.Name == "generation_bulk_end":
			deferredEnd++
			if event.WALFrames <= line {
				t.Errorf("window end measured %d frames, want the residue above the %d-page line", event.WALFrames, line)
			}
		case event.Stage == "checkpoint_passive":
			passive++
		}
	}
	if deferredRowLimit != 1 || deferredEnd != 1 || passive != 0 {
		t.Fatalf("checkpoint events: deferred row_limit=%d, deferred end=%d, inline PASSIVE=%d; want 1, 1, 0 (%+v)",
			deferredRowLimit, deferredEnd, passive, events)
	}
	if got := store.walDrainRequests.Load(); got != requestsBefore+1 {
		t.Fatalf("window end posted %d drains, want one", got-requestsBefore)
	}
	// Owed, not started: the drain must not copy the residue under the write
	// gate the publication takes next.
	time.Sleep(200 * time.Millisecond)
	store.maintenanceSched.Lock()
	owed, running := store.maintenanceDrainOwed, store.maintenanceDrainRunning
	store.maintenanceSched.Unlock()
	if !owed || running || store.walDrains.Load() != 0 {
		t.Fatalf("drain before publication: owed=%t running=%t drains=%d; want owed, idle, none run",
			owed, running, store.walDrains.Load())
	}
	store.schedulePublishMaintenance()
	waitForCondition(t, "the publication wakes the owed drain",
		func() bool { return store.walDrains.Load() > 0 && walFileFrames(t, path, pageSize) <= line })
	t.Logf("backlog=%d frames, window end took %s", backlog, endElapsed)
	nodeRows, edgeRows := generationRowCounts(t, store, generationID)
	if nodeRows != 128 || edgeRows == 0 {
		t.Fatalf("generation holds %d nodes / %d edges", nodeRows, edgeRows)
	}
}

// A backlog inside a configured budget keeps the inline PASSIVE, bounded by
// the inline window rather than the one-second routine budget. (The default
// budget is zero frames; this pins the branch the budget selects.)
func TestGenerationWindowCheckpointsABacklogWithinTheBudgetInline(t *testing.T) {
	setGenerationInlineCheckpointMaxFrames(t, 1<<40)
	previousWindow := generationInlineCheckpointWindow
	generationInlineCheckpointWindow = walPassiveCheckpointTimeout
	t.Cleanup(func() { generationInlineCheckpointWindow = previousWindow })
	path := filepath.Join(t.TempDir(), "inline.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.CheckpointWAL(); err != nil {
		t.Fatalf("drain the seed WAL: %v", err)
	}

	var events []bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) { events = append(events, event) }
	const generationID = int64(9)
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
	}
	store.writeMu.Lock()
	store.bulkCheckpointNodeRows = bulkCheckpointNodeInterval - 1
	store.writeMu.Unlock()
	nodes, edges := bulkFixture(8, 8)
	if err := store.AtGeneration(generationID).AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
		t.Fatalf("EndGenerationBulkLoadFor: %v", err)
	}
	var inline []bulkFinalizeEvent
	for _, event := range events {
		switch event.Stage {
		case "checkpoint_deferred":
			t.Fatalf("a small backlog was deferred: %+v", event)
		case "checkpoint_passive":
			inline = append(inline, event)
			if event.Err != nil {
				t.Errorf("inline PASSIVE over a quiet store failed: %+v", event)
			}
			if event.Elapsed > time.Second {
				t.Errorf("inline PASSIVE took %s", event.Elapsed)
			}
		}
	}
	if len(inline) != 2 || inline[0].Name != "row_limit" || inline[1].Name != "generation_bulk_end" {
		t.Fatalf("inline checkpoints = %+v, want row_limit then generation_bulk_end", inline)
	}
}

// The inline budget never exceeds a configured passive window, so a store
// whose routine window is shorter keeps its shorter bound.
func TestGenerationWindowInlineBudgetHonoursAShorterPassiveWindow(t *testing.T) {
	setGenerationInlineCheckpointMaxFrames(t, 1<<40)
	store, _ := openTempStore(t)
	store.passiveCheckpointTimeout = time.Nanosecond
	var events []bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) { events = append(events, event) }
	const generationID = int64(11)
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
	}
	nodes, edges := bulkFixture(8, 8)
	if err := store.AtGeneration(generationID).AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
		t.Fatalf("EndGenerationBulkLoadFor: %v", err)
	}
	for _, event := range events {
		if event.Stage == "checkpoint_passive" && event.Name == "generation_bulk_end" {
			if event.Err == nil {
				t.Fatalf("a nanosecond window let the inline PASSIVE finish: %+v", event)
			}
			return
		}
	}
	t.Fatalf("no inline window-end PASSIVE in %+v", events)
}

// The kill switch restores the former inline policy: a large backlog is
// checkpointed inline at the window end instead of being deferred.
func TestGenerationWindowCheckpointKillSwitchRestoresInlinePolicy(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_GENERATION_CHECKPOINT", "inline")
	setGenerationInlineCheckpointMaxFrames(t, 1)
	path := filepath.Join(t.TempDir(), "switch.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if backlog := seedUncheckpointedBacklog(t, store, path, 512, 1024); backlog <= 1 {
		t.Fatalf("seed left a backlog of %d frames", backlog)
	}
	var passive, deferred int
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		switch event.Stage {
		case "checkpoint_passive":
			passive++
		case "checkpoint_deferred":
			deferred++
		}
	}
	const generationID = int64(17)
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
	}
	nodes, edges := bulkFixture(8, 8)
	if err := store.AtGeneration(generationID).AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
		t.Fatalf("EndGenerationBulkLoadFor: %v", err)
	}
	if passive != 1 || deferred != 0 {
		t.Fatalf("with the inline policy the window end ran %d PASSIVE and %d deferrals, want 1 and 0", passive, deferred)
	}
}

// A window whose build never publishes still gets its drain: the lane is woken
// after generationDrainWakeDelay.
func TestGenerationWindowOwedDrainWakesWithoutAPublication(t *testing.T) {
	const line = 64
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", strconv.Itoa(line))
	previousDelay := generationDrainWakeDelay
	generationDrainWakeDelay = 300 * time.Millisecond
	t.Cleanup(func() { generationDrainWakeDelay = previousDelay })
	path := filepath.Join(t.TempDir(), "unpublished.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	pageSize := pragmaIntDB(t, store.db, "page_size")
	seedUncheckpointedBacklog(t, store, path, 2048, 4096)
	const generationID = int64(19)
	engaged, err := store.BeginGenerationBulkLoad(generationID)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
	}
	nodes, edges := bulkFixture(64, 128)
	if err := store.AtGeneration(generationID).AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("AddBatchChecked: %v", err)
	}
	if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
		t.Fatalf("EndGenerationBulkLoadFor: %v", err)
	}
	waitForCondition(t, "the delayed wake runs the owed drain",
		func() bool { return store.walDrains.Load() > 0 && walFileFrames(t, path, pageSize) <= line })
}
