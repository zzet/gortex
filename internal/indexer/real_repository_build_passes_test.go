package indexer

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/progress"
)

// Real-repository measurement of one working-tree body edit, run against a
// private clone of a real checkout. It is skipped unless
// GORTEX_TEST_REAL_REPO_CLONE names a git working tree (a standalone
// repository whose HEAD is the committed state) and
// GORTEX_TEST_REAL_REPO_STORE names where the base store lives (indexed from
// the clone's HEAD on first use and reused afterwards; every experiment runs
// on an APFS copy of it). The clone's own .gortex.yaml supplies the index
// configuration.
//
// For each target file it applies a one-statement body edit, runs one direct
// BuildDirtyLayer over the committed corpus, records the build's phases, the
// indexer's stage wall times, the framework-synthesis report, every bulk
// finalize event and the generation's row census, dumps the generation's own
// payload rows (for a byte-level differential between two code states), and
// restores the file.
//
// Optional: GORTEX_TEST_REAL_REPO_OUT (a directory for the JSON record, the
// payload dump and the captured logs), GORTEX_TEST_REAL_REPO_LABEL (a name
// for this run inside that directory), GORTEX_TEST_REAL_REPO_TARGETS (a comma
// list of slash paths relative to the clone), GORTEX_TEST_REAL_REPO_SHADOW_BUSY
// (non-empty: a whole-repository-sized lease holds the process shadow slot for
// the duration of the build, the shape of a concurrent cold drain),
// GORTEX_TEST_REAL_REPO_WARM_CACHE (non-empty: read the store copy once before
// the build, so it starts with the warm page cache a running daemon has).

type realRepoStageTime struct {
	Stage string  `json:"stage"`
	MS    float64 `json:"ms"`
}

type realRepoRecord struct {
	Label        string                                  `json:"label"`
	Target       string                                  `json:"target"`
	GOMAXPROCS   int                                     `json:"gomaxprocs"`
	GenerationID int64                                   `json:"generation_id"`
	BuildMS      float64                                 `json:"build_ms"`
	Report       accumulatedReportSummary                `json:"report"`
	Phases       []realRepoStageTime                     `json:"phases"`
	Stages       []realRepoStageTime                     `json:"indexer_stages"`
	Census       store_sqlite.GenerationPayloadRowCensus `json:"census"`
	PayloadHash  string                                  `json:"payload_digest"`
	PayloadRows  int                                     `json:"payload_rows"`
	InMemory     bool                                    `json:"context_held_in_memory"`
	ShadowBusy   bool                                    `json:"shadow_slot_held_by_drain"`
	LogLines     []string                                `json:"selected_log_lines"`
	Retention    realRepoRetention                       `json:"context_retention"`
}

// realRepoRetention classifies why the generation carries the files other
// than the edited one: for every such path it compares the generation's rows
// at the path with the base corpus's rows there.
type realRepoRetention struct {
	Files          int            `json:"files"`
	NodesDiffer    int            `json:"files_whose_nodes_differ"`
	EdgesOnly      int            `json:"files_whose_only_difference_is_edges"`
	Identical      int            `json:"files_identical_to_base"`
	EdgeDiffByKind map[string]int `json:"edge_rows_differing_by_kind_origin_side"`
	Samples        []string       `json:"samples,omitempty"`
}

// realRepoStageClock records the wall time between consecutive indexer
// progress stages.
type realRepoStageClock struct {
	mu     sync.Mutex
	last   string
	at     time.Time
	stages []realRepoStageTime
}

func (c *realRepoStageClock) Report(stage string, _, _ int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if stage == c.last {
		return
	}
	if c.last != "" {
		c.stages = append(c.stages, realRepoStageTime{Stage: c.last, MS: ms(now.Sub(c.at))})
	}
	c.last, c.at = stage, now
}

func (c *realRepoStageClock) finish() []realRepoStageTime {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last != "" {
		c.stages = append(c.stages, realRepoStageTime{Stage: c.last + " (to end)", MS: ms(time.Since(c.at))})
	}
	return c.stages
}

func TestRealRepositoryBodyEditBuildPasses(t *testing.T) {
	clone := os.Getenv("GORTEX_TEST_REAL_REPO_CLONE")
	basePath := os.Getenv("GORTEX_TEST_REAL_REPO_STORE")
	if clone == "" || basePath == "" {
		t.Skip("GORTEX_TEST_REAL_REPO_CLONE / GORTEX_TEST_REAL_REPO_STORE not set")
	}
	out := os.Getenv("GORTEX_TEST_REAL_REPO_OUT")
	if out == "" {
		out = t.TempDir()
	}
	label := os.Getenv("GORTEX_TEST_REAL_REPO_LABEL")
	if label == "" {
		label = "run"
	}
	targets := []string{"internal/config/config.go", "internal/mcp/checkout_binding.go"}
	if raw := os.Getenv("GORTEX_TEST_REAL_REPO_TARGETS"); raw != "" {
		targets = strings.Split(raw, ",")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(clone, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's config: %v", err)
	}
	indexCfg := cfg.Index

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		started := time.Now()
		base, err := store_sqlite.Open(basePath)
		if err != nil {
			t.Fatalf("open base store: %v", err)
		}
		idx := New(base, builderRegistry(), indexCfg, zap.NewNop())
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(clone); err != nil {
			t.Fatalf("index the clone: %v", err)
		}
		idx.Close()
		if err := base.Close(); err != nil {
			t.Fatalf("close base store: %v", err)
		}
		t.Logf("base index of %s took %s", clone, time.Since(started))
	}

	for _, target := range targets {
		target = strings.TrimSpace(target)
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			rec := realRepoMeasureBodyEdit(t, clone, basePath, out, label, target, indexCfg)
			raw, _ := json.MarshalIndent(rec, "", "  ")
			name := fmt.Sprintf("%s-%s.json", label, strings.ReplaceAll(target, "/", "_"))
			if err := os.WriteFile(filepath.Join(out, name), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("record %s", raw)
		})
	}
}

func realRepoMeasureBodyEdit(
	t *testing.T, clone, basePath, out, label, target string, indexCfg config.IndexConfig,
) realRepoRecord {
	t.Helper()
	ctx := context.Background()
	slug := label + "-" + strings.ReplaceAll(target, "/", "_")
	storePath := filepath.Join(t.TempDir(), "store.sqlite")
	if raw, err := exec.Command("cp", "-c", basePath, storePath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v: %s", err, raw)
	}
	if os.Getenv("GORTEX_TEST_REAL_REPO_WARM_CACHE") != "" {
		// An APFS clone shares blocks but not the page cache, so every
		// experiment copy starts cold. A daemon's store is warm; read the
		// copy once so SQL-bound phases are measured the way it pays them.
		if raw, err := exec.Command("sh", "-c", `cat "$0" >/dev/null`, storePath).CombinedOutput(); err != nil {
			t.Fatalf("warm the store copy: %v: %s", err, raw)
		}
	}
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("open store copy: %v", err)
	}
	defer func() { _ = store.Close() }()

	full := filepath.Join(clone, filepath.FromSlash(target))
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	edited, ok := realRepoBodyEdit(string(original))
	if !ok {
		t.Fatalf("%s has no function body to edit", target)
	}
	if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(full, original, 0o644); err != nil {
			t.Errorf("restore %s: %v", target, err)
		}
	}()

	logPath := filepath.Join(out, slug+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	stdLogPrev := log.Writer()
	log.SetOutput(logFile)
	defer log.SetOutput(stdLogPrev)
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(logFile), zapcore.DebugLevel))

	builder := &SparseGenerationBuilder{
		Store:    store,
		Registry: builderRegistry(),
		Config:   indexCfg,
		Logger:   logger,
	}
	busy := os.Getenv("GORTEX_TEST_REAL_REPO_SHADOW_BUSY") != ""
	if busy {
		// A whole-repository drain of this corpus charges ~0.9 GiB; hold the
		// process slot the way one would for the whole build.
		drain, err := processShadowAdmission.acquire(ctx, shadowAdmissionWeight(5000, 90<<20))
		if err != nil || drain == nil {
			t.Fatalf("hold the process shadow slot: lease=%v err=%v", drain, err)
		}
		defer drain.Release()
	}
	clock := &realRepoStageClock{}
	buildCtx := progress.WithReporter(ctx, clock)
	started := time.Now()
	id, report, err := builder.BuildDirtyLayer(buildCtx, DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: clone,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("BuildDirtyLayer: %v", err)
	}
	_ = logger.Sync()
	rec := realRepoRecord{
		Label: label, Target: target, GOMAXPROCS: runtime.GOMAXPROCS(0),
		GenerationID: id, BuildMS: ms(elapsed), Report: summarizeBuildReport(report),
		Stages: clock.finish(), InMemory: report.ContextHeldInMemory, ShadowBusy: busy,
	}
	rec.Report.ClosurePaths = nil
	rec.Report.ContextPaths = nil
	rec.Report.RetainedPaths = nil
	if report.Work != nil {
		for _, p := range report.Work.Phases {
			rec.Phases = append(rec.Phases, realRepoStageTime{Stage: p.Name, MS: ms(p.Duration)})
		}
	}
	if rec.Census, err = store.GenerationPayloadRowCensus(ctx, id); err != nil {
		t.Fatalf("row census: %v", err)
	}
	rec.Retention = realRepoRetentionCensus(t, storePath, id, builderGraphPath(builderRepoPrefix, target))
	rows := realRepoPayloadDump(t, storePath, id)
	rec.PayloadRows = len(rows)
	rec.PayloadHash = parityDigest(rows)
	if err := os.WriteFile(filepath.Join(out, slug+".payload.txt"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.LogLines = realRepoSelectLogLines(t, logPath)
	return rec
}

// realRepoBodyEdit inserts one statement at the top of the first function
// body whose opening line ends with "{".
func realRepoBodyEdit(src string) (string, bool) {
	lines := strings.SplitAfter(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\n")
		if strings.HasPrefix(trimmed, "func ") && strings.HasSuffix(trimmed, "{") {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, "\t_ = 0 // measured body edit\n")
			out = append(out, lines[i+1:]...)
			return strings.Join(out, ""), true
		}
	}
	return "", false
}

// realRepoPayloadDump renders the generation's own payload rows, stable
// across runs (edge row ids excluded).
func realRepoPayloadDump(t *testing.T, path string, generationID int64) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	queries := []struct{ name, query string }{
		{"node", `SELECT * FROM nodes WHERE view_gen = ? ORDER BY id`},
		{"edge", `SELECT from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta FROM edges WHERE view_gen = ? ORDER BY from_id, to_id, kind, file_path, line`},
		{"file", `SELECT * FROM files WHERE view_gen = ? ORDER BY file_path`},
		{"mask", `SELECT * FROM generation_file_masks WHERE view_gen = ? ORDER BY file_path`},
		{"tombstone", `SELECT * FROM generation_node_tombstones WHERE view_gen = ? ORDER BY node_id`},
		{"symbol_fts", `SELECT r.node_id, f.tokens FROM symbol_fts_rowid AS r JOIN symbol_fts AS f ON f.rowid = r.fts_rowid WHERE r.view_gen = ? ORDER BY r.node_id`},
		{"constant", `SELECT * FROM constant_values WHERE view_gen = ? ORDER BY node_id`},
		{"edge_source", `SELECT * FROM generation_edge_sources WHERE view_gen = ? ORDER BY 2`},
		{"producer", `SELECT * FROM generation_producer_completeness WHERE view_gen = ? ORDER BY producer`},
		// The catalog row minus its wall-clock stamps: state and the publish
		// rollup (covered/affected files, storage bytes) are part of the
		// published generation's identity.
		{"catalog", `SELECT generation_id, state, generation_kind, covered_files, affected_files, storage_bytes FROM view_generations WHERE generation_id = ?`},
	}
	var out []string
	for _, q := range queries {
		rows, err := db.Query(q.query, generationID)
		if err != nil {
			t.Fatalf("dump %s: %v", q.name, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s: %v", q.name, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				switch x := v.(type) {
				case []byte:
					parts[i] = "x" + hex.EncodeToString(x)
				default:
					parts[i] = fmt.Sprint(x)
				}
			}
			out = append(out, q.name+"\t"+strings.Join(parts, "\t"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %s: %v", q.name, err)
		}
		_ = rows.Close()
	}
	return out
}

func realRepoSelectLogLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	keep := []string{
		"bulk finalize", "framework dispatch", "resolver: incremental phase",
		"test edges emitted", "capability edges", "clone edges", "external-call", "speculative dispatch",
		"sparse generation closure", "drain start", "incremental derived",
		"publish generation", "shadow-swap decision", "shadow slot", "memory envelope",
	}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, k := range keep {
			if strings.Contains(line, k) {
				if len(line) > 4000 {
					line = line[:4000] + "…"
				}
				out = append(out, line)
				break
			}
		}
	}
	return out
}

func realRepoRetentionCensus(t *testing.T, path string, generationID int64, edited string) realRepoRetention {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	out := realRepoRetention{EdgeDiffByKind: map[string]int{}}
	files := parityColumn(t, db, `SELECT DISTINCT file_path FROM nodes WHERE view_gen = ? ORDER BY file_path`, generationID)
	rowsAt := func(query string, args ...any) map[string]int {
		set := map[string]int{}
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("retention query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					parts[i] = hex.EncodeToString(b)
				} else {
					parts[i] = fmt.Sprint(v)
				}
			}
			set[strings.Join(parts, "\t")]++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("retention rows: %v", err)
		}
		return set
	}
	const nodeQ = `SELECT id, kind, name, qual_name, start_line, end_line, start_column, end_column, signature, meta FROM nodes WHERE file_path = ? AND view_gen = ?`
	const edgeQ = `SELECT kind, origin, confidence_label, from_id, to_id, line, confidence, tier, cross_repo, meta FROM edges WHERE file_path = ? AND view_gen = ?`
	for _, file := range files {
		if file == edited {
			continue
		}
		out.Files++
		genNodes, baseNodes := rowsAt(nodeQ, file, generationID), rowsAt(nodeQ, file, 0)
		genEdges, baseEdges := rowsAt(edgeQ, file, generationID), rowsAt(edgeQ, file, 0)
		nodesSame := realRepoSameMultiset(genNodes, baseNodes)
		edgesSame := realRepoSameMultiset(genEdges, baseEdges)
		switch {
		case !nodesSame:
			out.NodesDiffer++
		case !edgesSame:
			out.EdgesOnly++
		default:
			out.Identical++
		}
		if edgesSame {
			continue
		}
		for side, pair := range map[string][2]map[string]int{"generation_only": {genEdges, baseEdges}, "base_only": {baseEdges, genEdges}} {
			for row, n := range pair[0] {
				if extra := n - pair[1][row]; extra > 0 {
					fields := strings.SplitN(row, "\t", 4)
					out.EdgeDiffByKind[side+" "+fields[0]+" origin="+fields[1]+" label="+fields[2]] += extra
					if len(out.Samples) < 12 {
						out.Samples = append(out.Samples, side+" "+file+" "+row)
					}
				}
			}
		}
	}
	return out
}

func realRepoSameMultiset(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}
