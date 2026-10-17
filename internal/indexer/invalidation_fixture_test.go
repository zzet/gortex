package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The invalidation-class fixture.
//
// Each invalidation test starts from the accumulated-dirty-edit module
// (accumulated_dirty_fixture_test.go, independent-packages layout): a
// committed base indexed into a private store, then N accumulated body edits
// in unrelated units, published one working-tree generation at a time exactly
// as the daemon publishes them. The case edit then lands on the narrow chain
// a -> b -> c -> d (and its one importing consumer), one more generation is
// built through the real sparse path, and the test asserts:
//
//   - the named frontier: the paths the generation re-derived or claimed
//     OUTSIDE the accumulated set (its masks, closure and context), which must
//     hold the case's required dependents and nothing unrelated;
//   - strict clean parity with an independent whole index of the same
//     checkout (assertCleanIndexParity);
//   - the case's own absence/visibility facts through the composed view;
//
// and records the build's work counters (BuildReport.Work) and the
// generation's physical row census (Store.GenerationPayloadRowCensus).
//
// The frontier is defined over masks and the plan, not over the parser input
// set, so it is the same whether a build plans against the committed base or
// against the previously published dirty generation: the accumulated units
// are excluded from it by construction, and a build that re-derives them is
// judged by the separate accumulated-edit work bounds instead.
//
// GX_INVALIDATION_ACCUMULATED sets N (default 25, 1..accumulatedDirtyUnits);
// GX_INVALIDATION_COUNTERS_OUT copies each case's JSON record to a directory.
//
// Builds are chained (dirtyChainBuilder): each one
// stands on the previous published generation when the builder accepts the
// delta, and every composed probe and the parity oracle walk the generation's
// whole chain. GX_INVALIDATION_DIRECT=1 builds every generation direct over
// the committed corpus instead.

const invalidationLayout = accumulatedDirtyIndependent

func invalidationAccumulated(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("GX_INVALIDATION_ACCUMULATED")
	if raw == "" {
		return 25
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > accumulatedDirtyUnits {
		t.Fatalf("GX_INVALIDATION_ACCUMULATED %q is not in 1..%d", raw, accumulatedDirtyUnits)
	}
	return n
}

// invalidationRun is one private repository, its private store, and the
// working-tree generations built over it.
type invalidationRun struct {
	t           *testing.T
	ctx         context.Context
	store       *store_sqlite.Store
	builder     *SparseGenerationBuilder
	repoDir     string
	accumulated map[string]struct{}
	builds      int
	// chains builds each generation (chained or direct); chainOf remembers
	// the chain, oldest first, every built generation tops.
	chains  *dirtyChainBuilder
	chainOf map[int64][]int64
}

// newInvalidationRun commits and indexes the module, then accumulates n body
// edits in units 0..n-1, building one working-tree generation per stride.
func newInvalidationRun(t *testing.T, name string, n int) *invalidationRun {
	t.Helper()
	store := builderOpenStore(t, "invalidation-"+name)
	r := &invalidationRun{
		t: t, ctx: context.Background(), store: store,
		builder:     builderNewBuilder(store),
		repoDir:     accumulatedDirtyRepo(t, invalidationLayout, store),
		accumulated: make(map[string]struct{}, n),
		chainOf:     map[int64][]int64{},
	}
	r.chains = newDirtyChainBuilder(t, r.builder, store, r.repoDir, os.Getenv("GX_INVALIDATION_DIRECT") != "1")
	stride := accumulatedGenerationStride(n)
	for i := 0; i < n; i++ {
		accumulatedDirtyWriteUnit(t, r.repoDir, invalidationLayout, i, true, false)
		r.accumulated[accumulatedDirtyUnitPath(invalidationLayout, i)] = struct{}{}
		if (i+1)%stride == 0 || i+1 == n {
			r.build()
		}
	}
	return r
}

func (r *invalidationRun) build() (int64, BuildReport) {
	r.t.Helper()
	id, report, chain := r.chains.build()
	r.chainOf[id] = chain
	r.builds++
	// Off the build's clock, before the next edit: the harness compaction.
	r.chains.settle()
	return id, report
}

// composed is the view generation id serves: its whole chain over the base.
func (r *invalidationRun) composed(id int64) graph.Reader {
	r.t.Helper()
	return dirtyChainComposed(r.t, r.store, r.chainFor(id))
}

func (r *invalidationRun) chainFor(id int64) []int64 {
	if chain, ok := r.chainOf[id]; ok {
		return chain
	}
	return []int64{id}
}

func (r *invalidationRun) write(rel, body string) {
	r.t.Helper()
	full := filepath.Join(r.repoDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *invalidationRun) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.repoDir, filepath.FromSlash(rel))); err != nil {
		r.t.Fatalf("remove %s: %v", rel, err)
	}
}

// committed is the base bytes of a path in the fixture's committed tree.
func invalidationCommitted(t *testing.T, rel string) string {
	t.Helper()
	body, ok := accumulatedDirtyTree(invalidationLayout)[rel]
	if !ok {
		t.Fatalf("%s is not in the committed tree", rel)
	}
	return body
}

// invalidationFrontier is what one generation named, restricted to the paths
// outside the accumulated set: every file mask (with its mode), the plan's
// closure, and the context it read and withheld or retained.
type invalidationFrontier struct {
	Masks    map[string]string `json:"masks"`
	Closure  []string          `json:"closure"`
	Context  []string          `json:"context"`
	Retained []string          `json:"retained"`
	// Named is the union of the four, sorted: the frontier as one set.
	Named []string `json:"named"`
}

func (r *invalidationRun) frontier(id int64, report BuildReport) invalidationFrontier {
	r.t.Helper()
	db := parityOpenRaw(r.t, r.store)
	rows, err := db.Query(`SELECT file_path, ownership_mode FROM generation_file_masks WHERE view_gen = ?`, id)
	if err != nil {
		r.t.Fatalf("masks of generation %d: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	f := invalidationFrontier{Masks: map[string]string{}}
	named := map[string]struct{}{}
	add := func(rel string) bool {
		if _, acc := r.accumulated[rel]; acc {
			return false
		}
		named[rel] = struct{}{}
		return true
	}
	for rows.Next() {
		var path, mode string
		if err := rows.Scan(&path, &mode); err != nil {
			r.t.Fatalf("scan mask: %v", err)
		}
		rel := strings.TrimPrefix(path, builderRepoPrefix+"/")
		if add(rel) {
			f.Masks[rel] = mode
		}
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("masks rows: %v", err)
	}
	keep := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			rel := strings.TrimPrefix(p, builderRepoPrefix+"/")
			if add(rel) {
				out = append(out, rel)
			}
		}
		slices.Sort(out)
		return out
	}
	f.Closure = keep(report.ClosurePaths)
	f.Context = keep(report.ContextPaths)
	f.Retained = keep(report.ContextRetainedPaths)
	for p := range named {
		f.Named = append(f.Named, p)
	}
	slices.Sort(f.Named)
	return f
}

// assertFrontier requires every path in required to be named and every named
// path to be in allowed (required is implicitly allowed). The accumulated
// units are never allowed.
func assertInvalidationFrontier(t *testing.T, f invalidationFrontier, required, allowed []string) {
	t.Helper()
	for _, p := range required {
		if !slices.Contains(f.Named, p) {
			t.Errorf("frontier %v does not name %s, which the change invalidates", f.Named, p)
		}
	}
	for _, p := range f.Named {
		if !slices.Contains(required, p) && !slices.Contains(allowed, p) {
			t.Errorf("frontier names %s, which the change cannot reach (frontier %v)", p, f.Named)
		}
	}
}

// invalidationRecord is one case's captured generation.
type invalidationRecord struct {
	Case         string                                  `json:"case"`
	Accumulated  int                                     `json:"accumulated_dirty"`
	Builds       int                                     `json:"generations_built"`
	GenerationID int64                                   `json:"generation_id"`
	Frontier     invalidationFrontier                    `json:"frontier"`
	Report       accumulatedReportSummary                `json:"report"`
	Work         *GenerationWorkCounters                 `json:"work"`
	Census       store_sqlite.GenerationPayloadRowCensus `json:"census"`
	CensusTotal  int64                                   `json:"census_total"`
	Parity       cleanParityResult                       `json:"parity"`
	ElapsedMS    float64                                 `json:"case_build_elapsed_ms"`
	Chain        []int64                                 `json:"chain"`
}

// capture builds the case generation, asserts strict clean parity, and
// records the counters.
func (r *invalidationRun) capture(name string, n int) (int64, BuildReport, invalidationRecord) {
	r.t.Helper()
	started := time.Now()
	id, report := r.build()
	elapsed := time.Since(started)
	census, err := r.store.GenerationPayloadRowCensus(r.ctx, id)
	if err != nil {
		r.t.Fatalf("row census for generation %d: %v", id, err)
	}
	rec := invalidationRecord{
		Case: name, Accumulated: n, Builds: r.builds, GenerationID: id,
		Frontier: r.frontier(id, report), Report: summarizeBuildReport(report), Work: report.Work,
		Census: census, CensusTotal: census.Total(), ElapsedMS: ms(elapsed),
	}
	builderAssertMasksValidate(r.t, r.store, id)
	rec.Chain = r.chainFor(id)
	rec.Parity = assertCleanIndexParityChain(r.t, r.store, rec.Chain, r.repoDir, "invalidation-"+name, true)
	r.log(rec)
	return id, report, rec
}

func (r *invalidationRun) log(rec invalidationRecord) {
	r.t.Helper()
	w := rec.Work
	if w == nil {
		w = &GenerationWorkCounters{}
	}
	r.t.Logf("invalidation case=%s N=%d gen=%d frontier=%v masks=%v closure=%v context=%v retained=%v",
		rec.Case, rec.Accumulated, rec.GenerationID, rec.Frontier.Named, rec.Frontier.Masks,
		rec.Frontier.Closure, rec.Frontier.Context, rec.Frontier.Retained)
	r.t.Logf("invalidation case=%s chain=%v parent=%d depth=%d fallback=%q manifest_rows=%d fallbacks=%v compactions=%d",
		rec.Case, rec.Chain, w.ParentGenerationID, w.ChainDepth, w.ChainFallbackReason, w.ManifestEntriesWritten,
		r.chains.fallbacks, r.chains.compactions)
	r.t.Logf("invalidation case=%s plan{changed=%d added=%d deleted=%d indexed=%d context=%d closure=%d output=%d} "+
		"parser{inputs=%d bytes=%d} reuse{reused=%d rebuilt=%d} masks{replace=%d delete=%d tombstones=%d} "+
		"census{nodes=%d edges=%d symbol_fts=%d files=%d file_masks=%d node_files=%d total=%d} "+
		"elapsed_ms=%.1f parity=%t",
		rec.Case, w.PlanChanged, w.PlanAdded, w.PlanDeleted, w.PlanIndexed, w.PlanContext, w.PlanClosure, w.PlanOutput,
		w.ParserInputs, w.ParserInputBytes, w.ReusedPriorPayloadFiles, w.RebuiltFiles,
		rec.Report.ReplaceMasks, rec.Report.DeleteMasks, rec.Census.Tables["generation_node_tombstones"],
		rec.Census.Tables["nodes"], rec.Census.Tables["edges"], rec.Census.Tables["symbol_fts_rowid"],
		rec.Census.Tables["files"], rec.Census.Tables["generation_file_masks"], rec.Census.NodeFiles, rec.CensusTotal,
		rec.ElapsedMS, rec.Parity.ok())
	if dir := os.Getenv("GX_INVALIDATION_COUNTERS_OUT"); dir != "" {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			r.t.Fatalf("marshal record: %v", err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.t.Fatalf("mkdir %s: %v", dir, err)
		}
		out := filepath.Join(dir, fmt.Sprintf("invalidation-%s-N%d.json", rec.Case, rec.Accumulated))
		if err := os.WriteFile(out, data, 0o644); err != nil {
			r.t.Fatalf("write %s: %v", out, err)
		}
	}
}

// --- composed-view probes -----------------------------------------------------

// invalidationNodesAt is every composed node whose file is rel.
func invalidationNodesAt(composed graph.Reader, rel string) []string {
	want := builderRepoPrefix + "/" + rel
	var out []string
	for _, n := range composed.AllNodes() {
		if n != nil && n.FilePath == want {
			out = append(out, n.ID)
		}
	}
	slices.Sort(out)
	return out
}

// invalidationEdgesTouching is every composed edge whose endpoint, or whose
// own file, is at rel.
func invalidationEdgesTouching(composed graph.Reader, rel string) []string {
	file := builderRepoPrefix + "/" + rel
	var out []string
	for _, e := range composed.AllEdges() {
		if e == nil {
			continue
		}
		if e.FilePath == file || e.From == file || e.To == file ||
			strings.HasPrefix(e.From, file+"::") || strings.HasPrefix(e.To, file+"::") {
			out = append(out, e.From+" -"+string(e.Kind)+"-> "+e.To)
		}
	}
	slices.Sort(out)
	return out
}

// invalidationComposedDocsAt is every composed symbol-FTS document whose node
// id sits at rel, composed from the store's raw rows the way the oracle does.
func (r *invalidationRun) invalidationComposedDocsAt(id int64, rel string) []string {
	r.t.Helper()
	db := parityOpenRaw(r.t, r.store)
	file := builderRepoPrefix + "/" + rel
	var out []string
	for _, doc := range parityComposeFTSChain(r.t, db, r.chainFor(id)) {
		nodeID, _, _ := strings.Cut(doc, "\t")
		if nodeID == file || strings.HasPrefix(nodeID, file+"::") {
			out = append(out, doc)
		}
	}
	return out
}

// invalidationGenerationRowsAt counts the rows generation id itself physically
// stores at rel, per payload table that is keyed by path.
func (r *invalidationRun) invalidationGenerationRowsAt(id int64, rel string) map[string]int {
	r.t.Helper()
	db := parityOpenRaw(r.t, r.store)
	file := builderRepoPrefix + "/" + rel
	out := map[string]int{}
	for table, query := range map[string]string{
		"nodes":             `SELECT COUNT(*) FROM nodes WHERE view_gen = ? AND file_path = ?`,
		"edges":             `SELECT COUNT(*) FROM edges WHERE view_gen = ? AND file_path = ?`,
		"files":             `SELECT COUNT(*) FROM files WHERE view_gen = ? AND file_path = ?`,
		"content_fts_rowid": `SELECT COUNT(*) FROM content_fts_rowid WHERE view_gen = ? AND file_path = ?`,
		"symbol_fts_rowid": `SELECT COUNT(*) FROM symbol_fts_rowid AS s JOIN nodes AS n
			ON n.id = s.node_id AND n.view_gen = s.view_gen WHERE s.view_gen = ? AND n.file_path = ?`,
	} {
		var count int
		if err := db.QueryRow(query, id, file).Scan(&count); err != nil {
			r.t.Fatalf("%s rows at %s in generation %d: %v", table, rel, id, err)
		}
		out[table] = count
	}
	return out
}
