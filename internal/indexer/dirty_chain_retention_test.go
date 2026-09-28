package indexer

import (
	"context"
	"encoding/json"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"os"
	"path/filepath"
	"testing"
)

// Long edit sequences through the coordinator with chaining on: the real
// materializer composes every parent, the real compactor runs at the soft
// depth, and the real sweep retires what the route and the reuse cache let
// go. Two questions: does what the store retains stay bounded however many
// edits land (TestDirtyChain50EditsBoundedRetention), and what does one edit
// cost at 1, 25 and 200 accumulated dirty files when the coordinator — not a
// builder harness — decides every parent (TestDirtyChainCoordinatorAcceptance*).
//
// The compactions run synchronously between edits (compactDirtyChain, the
// body the background goroutine runs), off the edit's measured build: the
// coordinator schedules one after every publication at the soft depth, and a
// quiet interval between edits is exactly when it would run.

// chainCoordinatorOn is an inert chained coordinator over f. Its
// compactions start without waiting for a quiet interval: the harness runs
// them between edits, when nothing else runs.
func chainCoordinatorOn(t *testing.T, f *coordinatorFixture, cfg CheckoutCoordinatorConfig) *CheckoutCoordinator {
	t.Helper()
	c := f.inertCoordinator(t, cfg)
	return c
}

// chainDirtyGenerations lists every working-tree generation the catalog holds
// for the fixture's checkout, whatever its state.
func chainDirtyGenerations(t *testing.T, f *coordinatorFixture) []store_sqlite.ViewGeneration {
	t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{CheckoutID: f.checkoutID})
	if err != nil {
		t.Fatalf("list generations: %v", err)
	}
	var out []store_sqlite.ViewGeneration
	for _, row := range rows {
		if row.GenerationKind == DirtyLayerGenerationKind {
			out = append(out, row)
		}
	}
	return out
}

// chainRetainedRows sums the physical payload rows of the given generations,
// and returns the largest single generation's count.
func chainRetainedRows(t *testing.T, f *coordinatorFixture, rows []store_sqlite.ViewGeneration) (total, largest int64) {
	t.Helper()
	for _, row := range rows {
		census, err := f.store.GenerationPayloadRowCensus(context.Background(), row.GenerationID)
		if err != nil {
			t.Fatalf("census of %d: %v", row.GenerationID, err)
		}
		total += census.Total()
		largest = max(largest, census.Total())
	}
	return total, largest
}

// chainStoreUsedBytes is the database's used page bytes: dbstat when the
// driver has it, page_count minus freelist otherwise. The write-ahead log is
// not counted.
func chainStoreUsedBytes(t *testing.T, f *coordinatorFixture) int64 {
	t.Helper()
	db := parityOpenRaw(t, f.store)
	defer func() { _ = db.Close() }()
	var used int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat`).Scan(&used); err == nil && used > 0 {
		return used
	}
	var pages, free, size int64
	_ = db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = db.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	_ = db.QueryRow(`PRAGMA page_size`).Scan(&size)
	return (pages - free) * size
}

// retentionTree is 20 one-file packages and 20 files of one package, plus the
// module file: independent and same-package edits in one checkout.
func retentionTree() map[string]string {
	tree := map[string]string{"go.mod": "module " + accumulatedDirtyModule + "\n\ngo 1.22\n"}
	for i := 0; i < retentionUnits; i++ {
		for _, layout := range []accumulatedDirtyLayout{accumulatedDirtyIndependent, accumulatedDirtySamePackage} {
			tree[accumulatedDirtyUnitPath(layout, i)] = accumulatedDirtyUnitSource(layout, i, false, false)
		}
	}
	return tree
}

const retentionUnits = 20

// retentionEditRecord is one edit of the retention sequence.
type retentionEditRecord struct {
	Edit             int     `json:"edit"`
	Path             string  `json:"path"`
	Body             bool    `json:"body"`
	Comment          bool    `json:"comment"`
	DirtyFiles       int     `json:"dirty_files"`
	GenerationID     int64   `json:"generation_id"`
	Built            bool    `json:"built"`
	Reused           bool    `json:"reused"`
	Parent           int64   `json:"parent"`
	Depth            int     `json:"depth"`
	Fallback         string  `json:"fallback,omitempty"`
	Preferred        bool    `json:"preferred_parent"`
	ParserInputs     int     `json:"parser_inputs"`
	AdmissionStats   int     `json:"admission_stats"`
	AdmissionOpens   int     `json:"admission_opens"`
	Samples          uint64  `json:"working_copy_samples"`
	BuildMS          float64 `json:"cycle_ms"`
	Compaction       string  `json:"compaction,omitempty"`
	CompactionMS     float64 `json:"compaction_ms,omitempty"`
	Swept            int     `json:"swept"`
	Backlog          int     `json:"backlog"`
	Generations      int     `json:"dirty_generations"`
	ServableGens     int     `json:"servable_dirty_generations"`
	RetainedRows     int64   `json:"retained_payload_rows"`
	LargestGenRows   int64   `json:"largest_generation_rows"`
	UsedBytes        int64   `json:"store_used_bytes"`
	Restart          string  `json:"restart,omitempty"`
	ParityChecked    bool    `json:"parity_checked"`
	PinnedReaderHeld bool    `json:"pinned_reader_held"`
}

// --- coordinator-level acceptance counters --------------------------------

// chainAcceptanceRecord is one captured edit at one accumulated size.
type chainAcceptanceRecord struct {
	Layout       accumulatedDirtyLayout                  `json:"layout"`
	Case         accumulatedEditCase                     `json:"case"`
	Accumulated  int                                     `json:"accumulated_dirty"`
	GenerationID int64                                   `json:"generation_id"`
	Parent       int64                                   `json:"parent"`
	Depth        int                                     `json:"depth"`
	Preferred    bool                                    `json:"preferred_parent"`
	Fallback     string                                  `json:"fallback,omitempty"`
	Work         *GenerationWorkCounters                 `json:"work"`
	Census       store_sqlite.GenerationPayloadRowCensus `json:"census"`
	CensusTotal  int64                                   `json:"census_total"`
	AccumParsed  int                                     `json:"accumulated_dirty_files_parsed"`
	Samples      uint64                                  `json:"working_copy_samples"`
	CycleMS      float64                                 `json:"cycle_ms"`
	Compactions  map[string]int                          `json:"compactions_during_run"`
	CompactionMS float64                                 `json:"compaction_ms_total"`
	Fallbacks    map[string]int                          `json:"fallbacks_during_run"`
	Parity       bool                                    `json:"parity_strict"`
	// Semantic runs record the go/types pass: whether the captured build ran
	// it, and how many builds ran it during the whole run (edits and
	// compactions counted apart).
	Semantic     bool           `json:"semantic_manager"`
	GoTypesRan   bool           `json:"go_types_ran"`
	GoTypesLoads map[string]int `json:"go_types_loads_during_run,omitempty"`
}

// chainAcceptanceSizes defaults to 1 and 25; GX_SUBSECOND_SIZES (the counters
// harness's knob) widens it, e.g. "1,25,200".
func chainAcceptanceSizes(t *testing.T) []int {
	if os.Getenv("GX_SUBSECOND_SIZES") == "" {
		return []int{1, 25}
	}
	return accumulatedEditSizes(t)
}

func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// writeChainArtifact writes a JSON artifact to the test's temp dir and, when
// GX_SUBSECOND_COUNTERS_OUT names a directory, copies it there.
func writeChainArtifact(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	dirs := []string{t.TempDir()}
	if dir := os.Getenv("GX_SUBSECOND_COUNTERS_OUT"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("artifact %s (%d bytes)", name, len(data))
}
