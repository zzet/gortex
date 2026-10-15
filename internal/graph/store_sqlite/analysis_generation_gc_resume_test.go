package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Interrupt only after committed child deletion and an unlocked writer gate.
// SQL's context checks inside a chunk cannot interrupt before its commit here.
// Probes use the idle writer connection, not a recursive read-pool acquisition.
type analysisGCInterruptContext struct {
	context.Context
	store  *Store
	cancel context.CancelFunc
	stop   func() bool
}

func (c *analysisGCInterruptContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if c.store.writeMu.TryLock() {
		if c.stop() {
			c.cancel()
		}
		c.store.writeMu.Unlock()
	}
	return c.Context.Err()
}

// Report a deadline at the same committed-chunk boundary as the cancellation
// probe, without depending on wall-clock timing during SQLite work.
type analysisGCDeadlineContext struct {
	context.Context
}

func (c analysisGCDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestAnalysisGenerationGCDeadlineCountsCommittedChunks(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldest := buildMinimalAnalysisGeneration(t, store, "oldest", 4, true)
	buildMinimalAnalysisGeneration(t, store, "fallback", 0, true)
	buildMinimalAnalysisGeneration(t, store, "active", 0, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := &analysisGCInterruptContext{Context: ctx, store: store, cancel: cancel, stop: func() bool {
		var count int
		if err := store.writerDB.QueryRow(`SELECT COUNT(*) FROM analysis_concepts WHERE generation_id = ?`, oldest).Scan(&count); err != nil {
			t.Errorf("observe committed chunk: %v", err)
			return true
		}
		return count < 4
	}}
	removed, err := store.PruneAnalysisGenerations(analysisGCDeadlineContext{interrupted}, 1, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline-limited prune, got %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d want=1 committed row", removed)
	}
	if got := analysisGCCount(t, store, "analysis_concepts", oldest); got != 3 {
		t.Fatalf("remaining concepts=%d want=3", got)
	}
	if got := analysisGCCount(t, store, "analysis_generations", oldest); got != 1 {
		t.Fatalf("partially collected generation was deleted: %d", got)
	}
}

func analysisGCCount(t *testing.T, store *Store, table string, generationID int64) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE generation_id = ?`, generationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAnalysisGenerationGCInterruptedPruneResumesOldest(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldest := buildMinimalAnalysisGeneration(t, store, "oldest", 4, true)
	newer := buildMinimalAnalysisGeneration(t, store, "newer", 4, true)
	buildMinimalAnalysisGeneration(t, store, "fallback", 0, true)
	buildMinimalAnalysisGeneration(t, store, "active", 0, true)

	for run, before := range []int{4, 3} {
		ctx, cancel := context.WithCancel(context.Background())
		interrupted := &analysisGCInterruptContext{Context: ctx, store: store, cancel: cancel, stop: func() bool {
			var count int
			if err := store.writerDB.QueryRow(`SELECT COUNT(*) FROM analysis_concepts WHERE generation_id = ?`, oldest).Scan(&count); err != nil {
				t.Errorf("observe committed chunk: %v", err)
				return true
			}
			return count < before
		}}
		_, err := store.PruneAnalysisGenerations(interrupted, 1, 1)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d: expected interrupted prune, got %v", run, err)
		}
		if got := analysisGCCount(t, store, "analysis_concepts", oldest); got != before-1 {
			t.Fatalf("run %d: oldest remaining=%d want=%d", run, got, before-1)
		}
		if got := analysisGCCount(t, store, "analysis_concepts", newer); got != 4 {
			t.Fatalf("run %d: newer generation was visited before oldest, remaining=%d", run, got)
		}
		// Emulate a new analysis between attempts: newest-first pruning would
		// abandon the partially collected generation for the newer backlog.
		buildMinimalAnalysisGeneration(t, store, fmt.Sprintf("new-active-%d", run), 0, true)
	}
	if _, err := store.PruneAnalysisGenerations(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if got := analysisGCCount(t, store, "analysis_generations", oldest); got != 0 {
		t.Fatalf("oldest generation did not finish: %d", got)
	}
}

func TestAnalysisGenerationGCBacklogReachesRetentionAcrossRuns(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var generations []int64
	for i := range 6 {
		id := buildMinimalAnalysisGeneration(t, store, fmt.Sprintf("stale-%d", i), 2, false)
		if err := store.AbortAnalysisGeneration(id); err != nil {
			t.Fatal(err)
		}
		generations = append(generations, id)
	}
	if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil || found {
		t.Fatalf("empty active pointer found=%v err=%v", found, err)
	}
	if _, err := store.AnalysisNodeMetrics(generations[5], []string{"stale-5-node"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("newest stale generation was readable: %v", err)
	}
	countBacklog := func() (int, error) {
		var count int
		err := store.writerDB.QueryRow(`SELECT COUNT(*) FROM analysis_concepts WHERE generation_id < ?`, generations[4]).Scan(&count)
		return count, err
	}
	finished := false
	interruptions := 0
	for run := 0; run < 9; run++ {
		before, err := countBacklog()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		interrupted := &analysisGCInterruptContext{Context: ctx, store: store, cancel: cancel, stop: func() bool {
			count, err := countBacklog()
			if err != nil {
				t.Errorf("observe committed backlog: %v", err)
				return true
			}
			return count < before
		}}
		_, err = store.PruneAnalysisGenerations(interrupted, 2, 1)
		cancel()
		if err == nil {
			finished = true
			break
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		interruptions++
		after, err := countBacklog()
		if err != nil || after != before-1 {
			t.Fatalf("backlog before=%d after=%d err=%v", before, after, err)
		}
		for _, id := range generations[4:] {
			if got := analysisGCCount(t, store, "analysis_concepts", id); got != 2 {
				t.Fatalf("retained generation %d changed: %d concepts", id, got)
			}
		}
	}
	if !finished || interruptions != 8 {
		t.Fatalf("finished=%v interruptions=%d want=8", finished, interruptions)
	}
	rows, err := store.db.Query(`SELECT generation_id FROM analysis_generations ORDER BY generation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var remaining []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remaining, generations[4:]) {
		t.Fatalf("remaining generations=%v want=%v", remaining, generations[4:])
	}
}

func TestAnalysisGenerationGCProtectsActiveAndBuilding(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	building := buildMinimalAnalysisGeneration(t, store, "building", 3, false)
	collectible := buildMinimalAnalysisGeneration(t, store, "collectible", 3, true)
	buildMinimalAnalysisGeneration(t, store, "fallback", 3, true)
	active := buildMinimalAnalysisGeneration(t, store, "active", 3, true)
	protected := []int64{active, building}
	tables := []string{"analysis_generations", "analysis_concepts", "analysis_nodes", "analysis_communities", "analysis_blobs", "analysis_generation_components"}
	before := make(map[int64][]int)
	for _, id := range protected {
		for _, table := range tables {
			before[id] = append(before[id], analysisGCCount(t, store, table, id))
		}
	}
	if _, err := store.PruneAnalysisGenerations(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if got := analysisGCCount(t, store, "analysis_generations", collectible); got != 0 {
		t.Fatalf("eligible generation was not collected: %d", got)
	}
	for _, id := range protected {
		// Exercise the under-lock recheck as if an old candidate snapshot had
		// selected the ID. Both chunk deletion and finalization must refuse it.
		for _, table := range analysisGenerationGCTables {
			removed, eligible, err := store.pruneAnalysisGenerationChunk(context.Background(), id, table, 1)
			if err != nil || eligible || removed != 0 {
				t.Fatalf("protected %d %s: removed=%d eligible=%v err=%v", id, table.name, removed, eligible, err)
			}
		}
		if _, err := store.finishPruneAnalysisGeneration(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		var after []int
		for _, table := range tables {
			after = append(after, analysisGCCount(t, store, table, id))
		}
		if !reflect.DeepEqual(after, before[id]) {
			t.Fatalf("protected generation %d changed: before=%v after=%v", id, before[id], after)
		}
	}
	header, found, err := store.LoadActiveAnalysisHeader(77)
	if err != nil || !found || header.GenerationID != active {
		t.Fatalf("active after prune=%+v found=%v err=%v", header, found, err)
	}
	var state int
	if err := store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, building).Scan(&state); err != nil || state != analysisGenerationBuilding {
		t.Fatalf("building state=%d err=%v", state, err)
	}
}

func TestAnalysisGenerationGCCanceledChunkRollsBackAndReleasesWriter(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := buildMinimalAnalysisGeneration(t, store, "canceled", 4, false)
	if err := store.AbortAnalysisGeneration(id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var concepts analysisGenerationGCTable
	for _, table := range analysisGenerationGCTables {
		if table.name == "concepts" {
			concepts = table
		}
	}
	removed, _, err := store.pruneAnalysisGenerationChunk(ctx, id, concepts, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled chunk returned %v", err)
	}
	if removed != 0 {
		t.Fatalf("rolled-back chunk counted %d removed rows", removed)
	}
	if !store.writeMu.TryLock() {
		t.Fatal("canceled chunk retained writer lock")
	}
	store.writeMu.Unlock()
	if got := analysisGCCount(t, store, "analysis_concepts", id); got != 4 {
		t.Fatalf("canceled transaction deleted rows: %d remain", got)
	}
}
