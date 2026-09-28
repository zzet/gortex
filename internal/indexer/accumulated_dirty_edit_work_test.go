package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// These tests characterise how much physical work ONE independent edit costs
// when N other files already differ from the committed base, with the dirty
// state reached exactly as the daemon reaches it: one working-tree layer build
// (BuildDirtyLayer over the committed corpus) after every edit, generation
// after generation.
//
// They record, per (layout, case, N): the plan sizes, the extraction parser
// inputs, admission reads, compiler context, reuse, the store writer's own
// row accounting and the new generation's physical row census, per-phase wall
// time, and clean-index parity. They PASS on today's behaviour.
//
// The decisive per-edit bounds are asserted only under GX_SUBSECOND_ACCEPT=1,
// so the red-to-green transition of the true-delta work is an explicit switch:
//
//   - extraction parser inputs are 1 + declared context and do not grow with N;
//   - the new generation's node / edge / symbol-FTS rows are bounded by the
//     edited file plus the explicitly invalidated closure, independent of N;
//   - accumulated dirty files are reported reused and are not parser inputs.
//
// Optional knobs: GX_SUBSECOND_SIZES (comma list, default "1,25,200"),
// GX_SUBSECOND_EVERY_EDIT=1 (one intermediate generation per edit at every
// size; see accumulatedGenerationStride), GX_SUBSECOND_COUNTERS_OUT (a
// directory the JSON artifact is also copied to) and GX_SUBSECOND_DIRECT=1.
//
// The builds run with index.dirty_chain.enabled on: each working-tree build
// stands on the previous published one when the builder accepts the delta
// (dirtyChainBuilder), the way the coordinator builds with the switch on.
// GX_SUBSECOND_DIRECT=1 turns the switch off and measures the direct build,
// every generation over the committed corpus.

type accumulatedEditCase string

const (
	accumulatedEditBody    accumulatedEditCase = "body"
	accumulatedEditComment accumulatedEditCase = "comment"
)

// accumulatedSeriesPoint is one intermediate accumulation build.
type accumulatedSeriesPoint struct {
	Dirty               int     `json:"dirty"`
	GenerationID        int64   `json:"generation_id"`
	PlanIndexed         int     `json:"plan_indexed"`
	ParserInputs        int     `json:"parser_inputs"`
	StatementRowChanges int64   `json:"statement_row_changes"`
	BulkNodeRows        int64   `json:"bulk_node_rows"`
	BulkEdgeRows        int64   `json:"bulk_edge_rows"`
	ElapsedMS           float64 `json:"elapsed_ms"`
}

// accumulatedEditRecord is one captured latest-edit generation.
type accumulatedEditRecord struct {
	Chained         bool                                    `json:"chained"`
	Chain           []int64                                 `json:"chain"`
	Compactions     int                                     `json:"harness_compactions"`
	Fallbacks       map[string]int                          `json:"chain_fallbacks"`
	Layout          accumulatedDirtyLayout                  `json:"layout"`
	Case            accumulatedEditCase                     `json:"case"`
	Accumulated     int                                     `json:"accumulated_dirty"`
	DirtyAtCapture  int                                     `json:"dirty_files_at_capture"`
	EditedPath      string                                  `json:"edited_path"`
	GenerationID    int64                                   `json:"generation_id"`
	Fingerprint     string                                  `json:"lower_view_fingerprint"`
	Report          accumulatedReportSummary                `json:"report"`
	Work            *GenerationWorkCounters                 `json:"work"`
	Census          store_sqlite.GenerationPayloadRowCensus `json:"census"`
	CensusTotal     int64                                   `json:"census_total"`
	AccumulatedRead int                                     `json:"accumulated_dirty_files_parsed"`
	ElapsedMS       float64                                 `json:"build_elapsed_ms"`
	Parity          cleanParityResult                       `json:"parity"`
	Series          []accumulatedSeriesPoint                `json:"accumulation_series,omitempty"`
	Stride          int                                     `json:"accumulation_generation_stride"`
}

type accumulatedReportSummary struct {
	ChangedFiles     int      `json:"changed_files"`
	AddedFiles       int      `json:"added_files"`
	DeletedFiles     int      `json:"deleted_files"`
	ClosureFiles     int      `json:"closure_files"`
	ClosurePaths     []string `json:"closure_paths"`
	ClosureTruncated bool     `json:"closure_truncated"`
	IndexedFiles     int      `json:"indexed_files"`
	ContextPaths     []string `json:"context_paths"`
	RetainedPaths    []string `json:"context_retained_paths"`
	NodeCount        int      `json:"node_count"`
	EdgeCount        int      `json:"edge_count"`
	ReplaceMasks     int      `json:"replace_masks"`
	DeleteMasks      int      `json:"delete_masks"`
	PlanningMS       float64  `json:"planning_ms"`
	DurationMS       float64  `json:"duration_ms"`
}

func summarizeBuildReport(r BuildReport) accumulatedReportSummary {
	return accumulatedReportSummary{
		ChangedFiles: r.ChangedFiles, AddedFiles: r.AddedFiles, DeletedFiles: r.DeletedFiles,
		ClosureFiles: r.ClosureFiles, ClosurePaths: r.ClosurePaths, ClosureTruncated: r.ClosureTruncated,
		IndexedFiles: len(r.IndexedPaths), ContextPaths: r.ContextPaths, RetainedPaths: r.ContextRetainedPaths,
		NodeCount: r.NodeCount, EdgeCount: r.EdgeCount, ReplaceMasks: r.ReplaceMasks, DeleteMasks: r.DeleteMasks,
		PlanningMS: ms(r.PlanningDuration), DurationMS: ms(r.Duration),
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func accumulatedEditSizes(t *testing.T) []int {
	t.Helper()
	raw := os.Getenv("GX_SUBSECOND_SIZES")
	if raw == "" {
		return []int{1, 25, 200}
	}
	var sizes []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > accumulatedDirtyUnits {
			t.Fatalf("GX_SUBSECOND_SIZES entry %q is not in 1..%d", part, accumulatedDirtyUnits)
		}
		sizes = append(sizes, n)
	}
	return sizes
}

func TestAccumulatedDirtyEditWorkIndependentPackages(t *testing.T) {
	runAccumulatedDirtyEditWork(t, accumulatedDirtyIndependent)
}

func TestAccumulatedDirtyEditWorkSamePackage(t *testing.T) {
	runAccumulatedDirtyEditWork(t, accumulatedDirtySamePackage)
}

// The clean-parity oracle, asserted. Each case composes its latest-edit
// generation over the committed corpus and compares it with an independent
// clean index of the same checkout, surface by surface (see
// assertCleanIndexParity for exactly what is compared and normalised). It runs
// at N=25 accumulated dirty files, where the dirty state is large enough to
// matter and the exhaustive reader probe stays cheap.
func TestAccumulatedDirtyEditCleanParityIndependentPackages(t *testing.T) {
	accumulatedDirtyEditAtSize(t, accumulatedDirtyIndependent, 25, true)
}

func TestAccumulatedDirtyEditCleanParitySamePackage(t *testing.T) {
	accumulatedDirtyEditAtSize(t, accumulatedDirtySamePackage, 25, true)
}

func runAccumulatedDirtyEditWork(t *testing.T, layout accumulatedDirtyLayout) {
	var records []accumulatedEditRecord
	for _, n := range accumulatedEditSizes(t) {
		records = append(records, accumulatedDirtyEditAtSize(t, layout, n, false)...)
	}
	for _, r := range records {
		w := r.Work
		t.Logf("chain layout=%s case=%s N=%d chained=%t parent=%d depth=%d fallback=%q manifest_rows=%d chain=%v compactions=%d fallbacks=%v",
			r.Layout, r.Case, r.Accumulated, r.Chained, w.ParentGenerationID, w.ChainDepth, w.ChainFallbackReason,
			w.ManifestEntriesWritten, r.Chain, r.Compactions, r.Fallbacks)
		t.Logf("counters layout=%s case=%s N=%d gen=%d plan{indexed=%d context=%d closure=%d deleted=%d output=%d} "+
			"parser{inputs=%d opens=%d bytes=%d accumulated_parsed=%d} admission{stats=%d opens=%d bytes=%d} "+
			"compiler{requested=%t measured=%t pkgs=%d files=%d} reuse{reused=%d rebuilt=%d} "+
			"store{window=%t stmt_rows=%d bulk_nodes=%d bulk_edges=%d batches=%d} "+
			"census{nodes=%d edges=%d symbol_fts=%d files=%d file_masks=%d node_files=%d total=%d} "+
			"elapsed_ms=%.1f parity=%t",
			r.Layout, r.Case, r.Accumulated, r.GenerationID,
			w.PlanIndexed, w.PlanContext, w.PlanClosure, w.PlanDeleted, w.PlanOutput,
			w.ParserInputs, w.ParserInputOpens, w.ParserInputBytes, r.AccumulatedRead,
			w.AdmissionStats, w.AdmissionOpens, w.AdmissionOpenBytes,
			w.CompilerContext.Requested, w.CompilerContext.Measured, w.CompilerContext.Packages, w.CompilerContext.Files,
			w.ReusedPriorPayloadFiles, w.RebuiltFiles,
			w.StoreMeasured, w.Store.StatementRowChanges, w.Store.BulkNodeRows, w.Store.BulkEdgeRows, w.Store.Batches,
			r.Census.Tables["nodes"], r.Census.Tables["edges"], r.Census.Tables["symbol_fts_rowid"],
			r.Census.Tables["files"], r.Census.Tables["generation_file_masks"], r.Census.NodeFiles, r.CensusTotal,
			r.ElapsedMS, r.Parity.ok())
		var phases []string
		for _, p := range w.Phases {
			phases = append(phases, fmt.Sprintf("%s=%.1fms", p.Name, ms(p.Duration)))
		}
		t.Logf("phases layout=%s case=%s N=%d %s", r.Layout, r.Case, r.Accumulated, strings.Join(phases, " "))
	}
	writeAccumulatedEditArtifact(t, layout, records)

	if os.Getenv("GX_SUBSECOND_ACCEPT") == "1" {
		assertAccumulatedEditBounds(t, records)
	}
}

// accumulatedDirtyEditAtSize builds the base, accumulates n dirty body edits
// one generation at a time, then captures the body-edit and comment-edit
// generations.
func accumulatedDirtyEditAtSize(t *testing.T, layout accumulatedDirtyLayout, n int, strictParity bool) []accumulatedEditRecord {
	t.Helper()
	ctx := context.Background()
	store := builderOpenStore(t, fmt.Sprintf("base-%s-%d", layout, n))
	repoDir := accumulatedDirtyRepo(t, layout, store)
	builder := builderNewBuilder(store)
	chains := newDirtyChainBuilder(t, builder, store, repoDir, os.Getenv("GX_SUBSECOND_DIRECT") != "1")
	var lastChain []int64

	build := func() (int64, BuildReport, time.Duration) {
		t.Helper()
		started := time.Now()
		id, report, chain := chains.build()
		elapsed := time.Since(started)
		if report.Work == nil {
			t.Fatalf("BuildDirtyLayer (%s, N=%d) reported no work counters", layout, n)
		}
		lastChain = chain
		// The harness compaction runs after the publication and off the
		// edit's clock, before the next edit lands: the design runs it in the
		// background between edits.
		chains.settle()
		return id, report, elapsed
	}

	var series []accumulatedSeriesPoint
	stride := accumulatedGenerationStride(n)
	for i := 0; i < n; i++ {
		accumulatedDirtyWriteUnit(t, repoDir, layout, i, true, false)
		if (i+1)%stride != 0 && i+1 != n {
			continue
		}
		id, report, elapsed := build()
		w := report.Work
		series = append(series, accumulatedSeriesPoint{
			Dirty: i + 1, GenerationID: id, PlanIndexed: w.PlanIndexed, ParserInputs: w.ParserInputs,
			StatementRowChanges: w.Store.StatementRowChanges, BulkNodeRows: w.Store.BulkNodeRows,
			BulkEdgeRows: w.Store.BulkEdgeRows, ElapsedMS: ms(elapsed),
		})
	}
	accumulated := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		accumulated[accumulatedDirtyUnitPath(layout, i)] = struct{}{}
	}

	capture := func(c accumulatedEditCase, edited string, dirty int) accumulatedEditRecord {
		t.Helper()
		id, report, elapsed := build()
		census, err := store.GenerationPayloadRowCensus(ctx, id)
		if err != nil {
			t.Fatalf("row census for generation %d: %v", id, err)
		}
		read := 0
		for _, p := range report.Work.ParserInputPaths {
			if _, ok := accumulated[p]; ok {
				read++
			}
		}
		label := fmt.Sprintf("%s-%s-%d", layout, c, n)
		return accumulatedEditRecord{
			Chained: chains.chained, Chain: lastChain, Compactions: chains.compactions,
			Fallbacks: maps.Clone(chains.fallbacks),
			Layout:    layout, Case: c, Accumulated: n, DirtyAtCapture: dirty, EditedPath: edited,
			GenerationID: id, Fingerprint: report.Work.LowerViewFingerprint,
			Report: summarizeBuildReport(report), Work: report.Work,
			Census: census, CensusTotal: census.Total(), AccumulatedRead: read, ElapsedMS: ms(elapsed),
			Parity: assertCleanIndexParityChain(t, store, lastChain, repoDir, label, strictParity),
		}
	}

	var out []accumulatedEditRecord
	// Body: one local statement in the unexported caller of a unit that is not
	// part of the accumulated set.
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyBodyTarget, true, false)
	body := capture(accumulatedEditBody, accumulatedDirtyUnitPath(layout, accumulatedDirtyBodyTarget), n+1)
	body.Series, body.Stride = series, stride
	out = append(out, body)

	// Comment: the body target goes back to its committed bytes (it leaves the
	// dirty set), and an ordinary comment changes in a second fresh unit.
	//
	// With chaining on, the revert is its own edit and gets its own
	// publication, as it would in the daemon: a delta over a parent that
	// carries the body edit must re-emit the committed bytes (a partial undo),
	// and folding that into the comment's build would measure two edits as
	// one. Direct builds plan against the committed corpus, where the revert
	// is simply absent, so they keep the single capture build.
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyBodyTarget, false, false)
	if chains.chained {
		build()
	}
	accumulatedDirtyWriteUnit(t, repoDir, layout, accumulatedDirtyCommentTarget, false, true)
	comment := capture(accumulatedEditComment, accumulatedDirtyUnitPath(layout, accumulatedDirtyCommentTarget), n+1)
	comment.Stride = stride
	out = append(out, comment)
	return out
}

// accumulatedGenerationStride is how many accumulated edits land between two
// intermediate working-tree builds. Every edit gets its own generation up to
// 25 accumulated files; above that the default builds one generation per
// n/25 edits (and always one after the last), which keeps a 200-file run
// inside the per-test budget. GX_SUBSECOND_EVERY_EDIT=1 restores one build
// per edit at every size.
//
// The stride cannot change the captured generation today: a working-tree
// build plans against the committed corpus, not against the previous dirty
// generation, so its inputs are the checkout state alone. Once a build plans
// against the previous published dirty state, run with
// GX_SUBSECOND_EVERY_EDIT=1 so every ancestor is exercised.
func accumulatedGenerationStride(n int) int {
	if os.Getenv("GX_SUBSECOND_EVERY_EDIT") == "1" || n <= 25 {
		return 1
	}
	return max(1, n/25)
}

func writeAccumulatedEditArtifact(t *testing.T, layout accumulatedDirtyLayout, records []accumulatedEditRecord) {
	t.Helper()
	data, err := json.MarshalIndent(struct {
		Layout  accumulatedDirtyLayout  `json:"layout"`
		Records []accumulatedEditRecord `json:"records"`
	}{layout, records}, "", "  ")
	if err != nil {
		t.Fatalf("marshal counters: %v", err)
	}
	name := "accumulated-dirty-edit-work-" + string(layout) + ".json"
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("counters artifact: %s", path)
	if dir := os.Getenv("GX_SUBSECOND_COUNTERS_OUT"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		out := filepath.Join(dir, name)
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("counters artifact copied: %s", out)
	}
}

// assertAccumulatedEditBounds holds the true per-edit bounds. It is expected
// to fail until the builder plans against the previous published dirty state
// and physically reuses unchanged payload.
func assertAccumulatedEditBounds(t *testing.T, records []accumulatedEditRecord) {
	t.Helper()
	smallest := map[string]accumulatedEditRecord{}
	for _, r := range records {
		key := string(r.Layout) + "/" + string(r.Case)
		if prev, ok := smallest[key]; !ok || r.Accumulated < prev.Accumulated {
			smallest[key] = r
		}
	}
	for _, r := range records {
		w := r.Work
		if w.ParserInputs > 1+w.PlanContext {
			t.Errorf("%s/%s N=%d: %d parser inputs, want at most 1 + %d declared context",
				r.Layout, r.Case, r.Accumulated, w.ParserInputs, w.PlanContext)
		}
		if r.AccumulatedRead != 0 {
			t.Errorf("%s/%s N=%d: %d accumulated dirty files were parser inputs, want 0",
				r.Layout, r.Case, r.Accumulated, r.AccumulatedRead)
		}
		if w.ReusedPriorPayloadFiles < r.Accumulated {
			t.Errorf("%s/%s N=%d: %d files reported reused, want at least the %d accumulated dirty files",
				r.Layout, r.Case, r.Accumulated, w.ReusedPriorPayloadFiles, r.Accumulated)
		}
		base := smallest[string(r.Layout)+"/"+string(r.Case)]
		if r.Accumulated == base.Accumulated {
			continue
		}
		if w.ParserInputs != base.Work.ParserInputs {
			t.Errorf("%s/%s: parser inputs %d at N=%d vs %d at N=%d, want independent of N",
				r.Layout, r.Case, w.ParserInputs, r.Accumulated, base.Work.ParserInputs, base.Accumulated)
		}
		for _, table := range []string{"nodes", "edges", "symbol_fts_rowid"} {
			if got, want := r.Census.Tables[table], base.Census.Tables[table]; got != want {
				t.Errorf("%s/%s: %s rows %d at N=%d vs %d at N=%d, want independent of N",
					r.Layout, r.Case, table, got, r.Accumulated, want, base.Accumulated)
			}
		}
		if got, limit := r.Census.NodeFiles, int64(1+w.PlanContext); got > limit {
			t.Errorf("%s/%s N=%d: node rows at %d files, want at most the edited file plus %d context",
				r.Layout, r.Case, r.Accumulated, got, w.PlanContext)
		}
	}
}
