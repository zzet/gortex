package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/semantic"
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
	c.compaction.quiet = -1
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

func TestDirtyChain50EditsBoundedRetention(t *testing.T) {
	ctx := context.Background()
	f := newCoordinatorFixtureWithTree(t, retentionTree())
	const retain = defaultRetainedCommitLayers
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	if out := coordinatorReconcile(t, c); out.CommitGenerationID == 0 || out.DirtyGenerationID == 0 {
		t.Fatalf("initial reconcile: %+v", out)
	}

	type unitState struct{ body, comment bool }
	states := map[string]*unitState{}
	dirty := func() int {
		n := 0
		for _, s := range states {
			if s.body || s.comment {
				n++
			}
		}
		return n
	}
	var (
		records     []retentionEditRecord
		pinned      *graphview.RepoView
		pinnedNodes []string
		pinnedEdges []string
		pinnedGens  []int64
		fallbacks   = map[string]int{}
		compactions = map[string]int{}
	)
	// A restart hands everything the old coordinator still owed or held — its
	// backlog, its reuse cache, its routed generation — to the retirement
	// backlog, the way the lifecycle drains a coordinator it stops
	// (DrainRetirements -> oweRetirement); here the new coordinator's backlog
	// stands in for the lifecycle's. What the new route still reads is refused
	// by the catalog until it is released.
	restart := func(full bool) {
		t.Helper()
		owed := c.DrainRetirements()
		if full {
			f.reopenStore(t)
		}
		c = chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
		for _, id := range owed {
			c.deferRetire(id, "drained by the restart")
		}
	}
	const edits = 51 // 50 edits, then the edit after the restart at 50
	for k := 1; k <= edits; k++ {
		layout := accumulatedDirtyIndependent
		if k%2 == 0 {
			layout = accumulatedDirtySamePackage
		}
		unit := (k * 7) % retentionUnits
		path := accumulatedDirtyUnitPath(layout, unit)
		s := states[path]
		if s == nil {
			s = &unitState{}
			states[path] = s
		}
		s.body = !s.body
		if k%10 == 0 {
			s.comment = !s.comment
		}
		accumulatedDirtyWriteUnit(t, f.worktree, layout, unit, s.body, s.comment)

		samples := c.sampler.SamplesTaken()
		started := time.Now()
		out := coordinatorReconcile(t, c)
		rec := retentionEditRecord{
			Edit: k, Path: path, Body: s.body, Comment: s.comment, DirtyFiles: dirty(),
			GenerationID: out.DirtyGenerationID, Built: out.DirtyBuilt, Reused: out.DirtyReused,
			Parent: out.DirtyParentGenerationID, Depth: out.DirtyChainDepth, Preferred: out.DirtyParentPreferred,
			BuildMS: ms(time.Since(started)), Samples: c.sampler.SamplesTaken() - samples,
		}
		if out.DirtyBuilt && out.DirtyParentGenerationID == 0 {
			rec.Fallback = out.DirtyChainReason
			fallbacks[out.DirtyChainReason]++
		}
		if w := out.DirtyWork; w != nil {
			rec.ParserInputs, rec.AdmissionStats, rec.AdmissionOpens = w.ParserInputs, w.AdmissionStats, w.AdmissionOpens
		}
		if out.CompactionScheduled {
			report := c.compactDirtyChain(ctx, out)
			rec.Compaction, rec.CompactionMS = report.Outcome, ms(report.Duration)
			compactions[report.Outcome]++
			if report.Outcome != dirtyChainCompactionFlipped {
				t.Errorf("edit %d: compaction = %+v, want a flip (nothing else moves the route here)", k, report)
			}
		}
		rec.Swept = c.SweepRetirements(ctx)
		rec.Backlog = len(c.retirementBacklog())

		// The first edit after each restart chains over the routed top.
		if k == 2 || k == 26 || k == 51 {
			if !out.DirtyBuilt || out.DirtyParentGenerationID == 0 {
				t.Errorf("edit %d after a restart = %+v, want a chained build over the routed top", k, out)
			}
		}
		// A reader pinned across edits 24–26 keeps the snapshot it pinned.
		if k == 24 {
			pinned = chainMaterialize(t, f)
			pinnedNodes, pinnedEdges = builderRenderNodes(pinned.Reader.AllNodes()), builderRenderEdges(pinned.Reader.AllEdges())
			pinnedGens = slices.Clone(pinned.Generations())
		}
		if pinned != nil {
			rec.PinnedReaderHeld = true
			for _, id := range pinnedGens {
				if _, found := f.generation(id); !found {
					t.Fatalf("edit %d: generation %d a pinned reader holds was retired", k, id)
				}
			}
		}
		if k == 26 {
			if got := builderRenderNodes(pinned.Reader.AllNodes()); !slices.Equal(got, pinnedNodes) {
				t.Fatalf("the reader pinned at edit 24 changed by edit 26 (nodes)")
			}
			if got := builderRenderEdges(pinned.Reader.AllEdges()); !slices.Equal(got, pinnedEdges) {
				t.Fatalf("the reader pinned at edit 24 changed by edit 26 (edges)")
			}
			pinned.Close()
			pinned = nil
			rec.Swept += c.SweepRetirements(ctx)
			rec.Backlog = len(c.retirementBacklog())
		}
		if k == 10 || k == 24 || k == 37 || k == 50 || k == 51 {
			chainAssertFlat(t, f, fmt.Sprintf("retention-%d", k))
			rec.ParityChecked = true
		}

		gens := chainDirtyGenerations(t, f)
		rec.Generations = len(gens)
		for _, g := range gens {
			if servableGeneration(g.State) {
				rec.ServableGens++
			}
		}
		rec.RetainedRows, rec.LargestGenRows = chainRetainedRows(t, f, gens)
		rec.UsedBytes = chainStoreUsedBytes(t, f)

		// Bounded: the routed chain (at most the hard depth), the reuse
		// cache, one compaction in hand, and — while a reader pins it — one
		// more chain.
		bound := maxDirtyChainDepth + retain + 1
		if rec.PinnedReaderHeld {
			bound += maxDirtyChainDepth
		}
		if rec.Generations > bound {
			t.Errorf("edit %d: %d working-tree generations retained, bound %d", k, rec.Generations, bound)
		}
		if rec.RetainedRows > int64(bound)*max(rec.LargestGenRows, 1) {
			t.Errorf("edit %d: %d payload rows retained over %d generations", k, rec.RetainedRows, rec.Generations)
		}

		switch k {
		case 1, 50:
			rec.Restart = "store+coordinator"
			restart(true)
		case 25:
			// The pinned reader holds the store open, so this restart is the
			// coordinator's alone: a fresh coordinator with no cache, no
			// backlog and no memory of the chain.
			rec.Restart = "coordinator"
			restart(false)
		}
		records = append(records, rec)
	}

	// No linear growth: the second half retains no more generations than the
	// bound the first half already reached.
	firstHalf, secondHalf := 0, 0
	for _, r := range records {
		if r.Edit <= 25 {
			firstHalf = max(firstHalf, r.Generations)
		} else if r.Edit <= 50 {
			secondHalf = max(secondHalf, r.Generations)
		}
	}
	if secondHalf > firstHalf+2 {
		t.Errorf("retained generations grow with edits: max %d in edits 1-25, %d in 26-50", firstHalf, secondHalf)
	}
	for _, r := range records {
		t.Logf("retention edit=%d path=%s body=%t comment=%t dirty=%d gen=%d built=%t reused=%t parent=%d depth=%d preferred=%t fallback=%q "+
			"parser=%d admission{stats=%d opens=%d} samples=%d build_ms=%.1f compaction=%q/%.1fms swept=%d backlog=%d "+
			"gens=%d servable=%d rows=%d largest=%d bytes=%d restart=%q parity=%t pinned=%t",
			r.Edit, r.Path, r.Body, r.Comment, r.DirtyFiles, r.GenerationID, r.Built, r.Reused, r.Parent, r.Depth, r.Preferred, r.Fallback,
			r.ParserInputs, r.AdmissionStats, r.AdmissionOpens, r.Samples, r.BuildMS, r.Compaction, r.CompactionMS,
			r.Swept, r.Backlog, r.Generations, r.ServableGens, r.RetainedRows, r.LargestGenRows, r.UsedBytes, r.Restart, r.ParityChecked, r.PinnedReaderHeld)
	}
	t.Logf("retention fallbacks=%v compactions=%v", fallbacks, compactions)
	writeChainArtifact(t, "dirty-chain-50-edit-retention.json", map[string]any{
		"records": records, "fallbacks": fallbacks, "compactions": compactions,
		"bound": map[string]int{"hard_depth": maxDirtyChainDepth, "soft_depth": dirtyChainCompactionDepth, "retain": retain},
	})
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

func TestDirtyChainCoordinatorAcceptanceIndependentPackages(t *testing.T) {
	runChainCoordinatorAcceptance(t, accumulatedDirtyIndependent)
}

func TestDirtyChainCoordinatorAcceptanceSamePackage(t *testing.T) {
	runChainCoordinatorAcceptance(t, accumulatedDirtySamePackage)
}

// chainAcceptanceSizes defaults to 1 and 25; GX_SUBSECOND_SIZES (the counters
// harness's knob) widens it, e.g. "1,25,200".
func chainAcceptanceSizes(t *testing.T) []int {
	if os.Getenv("GX_SUBSECOND_SIZES") == "" {
		return []int{1, 25}
	}
	return accumulatedEditSizes(t)
}

func runChainCoordinatorAcceptance(t *testing.T, layout accumulatedDirtyLayout) {
	var records []chainAcceptanceRecord
	for _, n := range chainAcceptanceSizes(t) {
		records = append(records, chainCoordinatorAcceptanceAt(t, layout, n)...)
	}
	for _, r := range records {
		w := r.Work
		t.Logf("coordinator layout=%s case=%s N=%d gen=%d parent=%d depth=%d preferred=%t fallback=%q "+
			"plan{indexed=%d context=%d output=%d} parser{inputs=%d bytes=%d accumulated_parsed=%d} "+
			"reuse{reused=%d rebuilt=%d} store{stmt_rows=%d} census{nodes=%d edges=%d fts=%d files=%d masks=%d node_files=%d total=%d} "+
			"manifest_rows=%d admission{stats=%d opens=%d} samples=%d cycle_ms=%.1f compactions=%v compaction_ms=%.1f fallbacks=%v parity=%t semantic=%t go_types_ran=%t go_types_loads=%v",
			r.Layout, r.Case, r.Accumulated, r.GenerationID, r.Parent, r.Depth, r.Preferred, r.Fallback,
			w.PlanIndexed, w.PlanContext, w.PlanOutput, w.ParserInputs, w.ParserInputBytes, r.AccumParsed,
			w.ReusedPriorPayloadFiles, w.RebuiltFiles, w.Store.StatementRowChanges,
			r.Census.Tables["nodes"], r.Census.Tables["edges"], r.Census.Tables["symbol_fts_rowid"], r.Census.Tables["files"],
			r.Census.Tables["generation_file_masks"], r.Census.NodeFiles, r.CensusTotal,
			w.ManifestEntriesWritten, w.AdmissionStats, w.AdmissionOpens, r.Samples, r.CycleMS,
			r.Compactions, r.CompactionMS, r.Fallbacks, r.Parity, r.Semantic, r.GoTypesRan, r.GoTypesLoads)
	}
	writeChainArtifact(t, "dirty-chain-coordinator-acceptance-"+string(layout)+".json", records)

	// The per-edit bounds the builder-level acceptance holds, held here with
	// every parent chosen by the coordinator.
	smallest := map[accumulatedEditCase]chainAcceptanceRecord{}
	for _, r := range records {
		if prev, ok := smallest[r.Case]; !ok || r.Accumulated < prev.Accumulated {
			smallest[r.Case] = r
		}
	}
	for _, r := range records {
		w := r.Work
		if r.Parent == 0 {
			t.Errorf("%s/%s N=%d: the edit was built direct (%s), want a chained build", r.Layout, r.Case, r.Accumulated, r.Fallback)
		}
		if w.ParserInputs > 1+w.PlanContext {
			t.Errorf("%s/%s N=%d: %d parser inputs, want at most 1 + %d context", r.Layout, r.Case, r.Accumulated, w.ParserInputs, w.PlanContext)
		}
		if r.AccumParsed != 0 {
			t.Errorf("%s/%s N=%d: %d accumulated dirty files parsed, want 0", r.Layout, r.Case, r.Accumulated, r.AccumParsed)
		}
		if r.Semantic && !r.GoTypesRan {
			t.Errorf("%s/%s N=%d: the captured build ran no go/types pass", r.Layout, r.Case, r.Accumulated)
		}
		if !r.Parity {
			t.Errorf("%s/%s N=%d: the served view does not match a clean index", r.Layout, r.Case, r.Accumulated)
		}
		base := smallest[r.Case]
		for _, table := range []string{"nodes", "edges", "symbol_fts_rowid"} {
			if got, want := r.Census.Tables[table], base.Census.Tables[table]; got != want {
				t.Errorf("%s/%s: %s rows %d at N=%d vs %d at N=%d, want independent of N",
					r.Layout, r.Case, table, got, r.Accumulated, want, base.Accumulated)
			}
		}
		if w.ParserInputs != base.Work.ParserInputs {
			t.Errorf("%s/%s: parser inputs %d at N=%d vs %d at N=%d, want independent of N",
				r.Layout, r.Case, w.ParserInputs, r.Accumulated, base.Work.ParserInputs, base.Accumulated)
		}
	}
}

// chainCoordinatorAcceptanceAt accumulates n dirty body edits one coordinator
// cycle each (compacting at the soft depth and sweeping after every edit),
// then captures a body edit and — after the body target's revert publishes
// on its own — a comment edit.
func chainCoordinatorAcceptanceAt(t *testing.T, layout accumulatedDirtyLayout, n int) []chainAcceptanceRecord {
	t.Helper()
	ctx := context.Background()
	// GX_SUBSECOND_SEMANTIC=1 runs the base corpus, every working-tree build
	// and every compaction with the go/types pass, and holds parity against a
	// clean semantic index.
	semanticRun := os.Getenv("GX_SUBSECOND_SEMANTIC") == "1"
	var (
		f   *coordinatorFixture
		c   *CheckoutCoordinator
		mgr *semantic.Manager
	)
	if semanticRun {
		f, c, mgr = semanticChainFixture(t, accumulatedDirtyTree(layout))
	} else {
		f = newCoordinatorFixtureWithTree(t, accumulatedDirtyTree(layout))
		c = chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	}
	coordinatorReconcile(t, c)
	goTypesLoads := map[string]int{}

	compactions, fallbacks := map[string]int{}, map[string]int{}
	var compactionMS float64
	step := func() (CheckoutCycle, time.Duration, uint64) {
		t.Helper()
		samples := c.sampler.SamplesTaken()
		started := time.Now()
		out := coordinatorReconcile(t, c)
		elapsed := time.Since(started)
		taken := c.sampler.SamplesTaken() - samples
		if out.DirtyBuilt && out.DirtyParentGenerationID == 0 {
			fallbacks[out.DirtyChainReason]++
		}
		if goTypesRan(out.DirtyWork) {
			if out.DirtyParentGenerationID > 0 {
				goTypesLoads["chained_edit"]++
			} else {
				goTypesLoads["direct_edit"]++
			}
		}
		if out.CompactionScheduled {
			report := c.compactDirtyChain(ctx, out)
			compactions[report.Outcome]++
			compactionMS += ms(report.Duration)
		}
		c.SweepRetirements(ctx)
		return out, elapsed, taken
	}
	for i := 0; i < n; i++ {
		accumulatedDirtyWriteUnit(t, f.worktree, layout, i, true, false)
		step()
	}
	accumulated := map[string]bool{}
	for i := 0; i < n; i++ {
		accumulated[accumulatedDirtyUnitPath(layout, i)] = true
	}
	capture := func(kind accumulatedEditCase) chainAcceptanceRecord {
		t.Helper()
		out, elapsed, samples := step()
		if !out.DirtyBuilt || out.DirtyWork == nil {
			t.Fatalf("%s/%s N=%d: the capture built nothing: %+v", layout, kind, n, out)
		}
		census, err := f.store.GenerationPayloadRowCensus(ctx, out.DirtyGenerationID)
		if err != nil {
			t.Fatal(err)
		}
		parsed := 0
		for _, p := range out.DirtyWork.ParserInputPaths {
			if accumulated[p] {
				parsed++
			}
		}
		parity := t.Run(fmt.Sprintf("parity-%s-%s-%d", layout, kind, n), func(t *testing.T) {
			label := fmt.Sprintf("acceptance-%s-%s-%d", layout, kind, n)
			if semanticRun {
				assertSemanticParity(t, f, mgr, label)
				return
			}
			chainAssertFlat(t, f, label)
		})
		return chainAcceptanceRecord{
			Layout: layout, Case: kind, Accumulated: n, GenerationID: out.DirtyGenerationID,
			Parent: out.DirtyParentGenerationID, Depth: out.DirtyChainDepth, Preferred: out.DirtyParentPreferred,
			Fallback: out.DirtyChainReason, Work: out.DirtyWork, Census: census, CensusTotal: census.Total(),
			AccumParsed: parsed, Samples: samples, CycleMS: ms(elapsed),
			Compactions: cloneCounts(compactions), CompactionMS: compactionMS,
			Fallbacks: cloneCounts(fallbacks), Parity: parity,
			Semantic: semanticRun, GoTypesRan: goTypesRan(out.DirtyWork), GoTypesLoads: cloneCounts(goTypesLoads),
		}
	}
	var out []chainAcceptanceRecord
	accumulatedDirtyWriteUnit(t, f.worktree, layout, accumulatedDirtyBodyTarget, true, false)
	out = append(out, capture(accumulatedEditBody))
	accumulatedDirtyWriteUnit(t, f.worktree, layout, accumulatedDirtyBodyTarget, false, false)
	step()
	accumulatedDirtyWriteUnit(t, f.worktree, layout, accumulatedDirtyCommentTarget, false, true)
	out = append(out, capture(accumulatedEditComment))
	return out
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
