package indexer

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

// The per-file delta path on a real repository.
//
// Skipped unless GX_DELTA_REAL_TREE names a clean private git clone (the test
// edits files in place and restores them) and GX_DELTA_REAL_STORE names its
// base store; the first run indexes the clone into it. For each file of
// GX_DELTA_REAL_FILES (comma-separated, repository-relative; default the four
// reference files) it clones the base store once and applies
// GX_DELTA_REAL_EDITS (default 5) successive body edits of the same file, each
// one working-tree generation over the previous one exactly as the coordinator
// chains them, with index.edit_delta on. Every edit's wall time, the delta's
// own phases and counters and the build's physical phases are logged and, with
// GX_DELTA_REAL_OUT, written as one JSON record per edit. GX_DELTA_REAL_LOG
// receives the builder's info-level log. GX_DELTA_REAL_SEMANTIC=1 installs the
// go/types enrichment manager. GX_DELTA_REAL_EDIT=inplace makes every save an
// edit inside the first function body that keeps every line where it was. Each superseded generation is retired after the
// next one is published, as the coordinator does, unless GX_DELTA_REAL_RETIRE=0;
// the retirement's log writes are recorded apart from the edit's.
// GX_DELTA_REAL_LAYER=N first publishes a working-tree generation of N other
// dirty Go files (a comment appended to each, kept for the file's edits, never
// compacted away) so every measured edit composes over a large immutable
// layer, as a diverged branch's layers below a live checkout are.
// GX_DELTA_REAL_PROF names a file that receives a CPU profile of the timed
// builds.

var editDeltaRealDefaultFiles = []string{
	"internal/config/config.go",
	"internal/gitstate/dirty.go",
	"internal/indexer/checkout_refresh.go",
	"internal/mcp/checkout_binding.go",
}

type editDeltaRealRecord struct {
	File   string  `json:"file"`
	Edit   int     `json:"edit"`
	Gen    int64   `json:"generation"`
	WallMS float64 `json:"wall_ms"`
	// CPUMS is the process's user+system CPU time over the build: on a
	// loaded host the wall time includes waiting for a core, the CPU time
	// does not.
	CPUMS     float64           `json:"cpu_ms"`
	Delta     *EditDeltaReport  `json:"delta"`
	Phases    []GenerationPhase `json:"phases"`
	PassSteps []GenerationPhase `json:"pass_steps"`
	Plan      []GenerationPhase `json:"plan_steps"`
	Census    map[string]int64  `json:"census"`
	Nodes     int               `json:"nodes"`
	Edges     int               `json:"edges"`
	WALValid  bool              `json:"wal_valid"`
	WALFrames int64             `json:"wal_frames"`
	WALBytes  int64             `json:"wal_bytes"`
	WALReset  bool              `json:"wal_reset"`
	// Retired counts the superseded generations retired after the edit, as
	// the coordinator retires what a working tree's new generation no longer
	// stands on; RetireFrames/RetireBytes are what that appended to the log.
	Retired      int     `json:"retired,omitempty"`
	RetireMS     float64 `json:"retire_ms,omitempty"`
	RetireFrames int64   `json:"retire_wal_frames,omitempty"`
	RetireBytes  int64   `json:"retire_wal_bytes,omitempty"`
}

func TestRealRepoEditDelta(t *testing.T) {
	tree := os.Getenv("GX_DELTA_REAL_TREE")
	if tree == "" {
		t.Skip("GX_DELTA_REAL_TREE is not set: no private real-repository clone to measure")
	}
	storePath := os.Getenv("GX_DELTA_REAL_STORE")
	if storePath == "" {
		t.Fatal("GX_DELTA_REAL_STORE must name the base store path")
	}
	builderIsolateGit(t)
	if out := builderGit(t, tree, "status", "--porcelain"); out != "" {
		t.Fatalf("the clone at %s is not clean:\n%s", tree, out)
	}
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's configuration: %v", err)
	}
	logger := editDeltaRealLogger(t)
	// GX_DELTA_REAL_BASE=dedicated measures in the production shape: the corpus
	// is a published dedicated base generation in GX_DELTA_REAL_STORE (built on
	// first use), and every build stands on a materialized view of it, so the
	// per-stack caches are on. Otherwise the corpus is generation zero and
	// they are off.
	var dedicatedBase int64
	if os.Getenv("GX_DELTA_REAL_BASE") == "dedicated" {
		dedicatedBase = editDeltaRealDedicatedBase(t, tree, storePath, cfg.Index, logger)
	} else if _, err := os.Stat(storePath); errors.Is(err, os.ErrNotExist) {
		started := time.Now()
		base := builderOpenStoreAt(t, storePath)
		idx := New(base, builderRegistry(), cfg.Index, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(tree); err != nil {
			idx.Close()
			_ = base.Close()
			t.Fatalf("index the clone: %v", err)
		}
		idx.Close()
		if err := base.Close(); err != nil {
			t.Fatalf("close the base store: %v", err)
		}
		t.Logf("base index of %s: %s", tree, time.Since(started))
	}
	files := editDeltaRealDefaultFiles
	if raw := os.Getenv("GX_DELTA_REAL_FILES"); raw != "" {
		if raw == "none" {
			return
		}
		files = strings.Split(raw, ",")
	}
	edits := 5
	if raw := os.Getenv("GX_DELTA_REAL_EDITS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			edits = n
		}
	}
	editCfg := cfg.Index
	var mgr *semantic.Manager
	if os.Getenv("GX_DELTA_REAL_SEMANTIC") == "1" {
		mgr = semantic.NewManager(semantic.Config{Enabled: true}, logger)
		mgr.RegisterProvider(goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, logger))
		t.Cleanup(func() { _ = mgr.Close() })
	}
	for _, rel := range files {
		rel := strings.TrimSpace(rel)
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			editDeltaRealFile(t, tree, storePath, rel, editCfg, logger, mgr, edits, dedicatedBase)
		})
	}
}

func editDeltaRealFile(
	t *testing.T, tree, storePath, rel string, cfg config.IndexConfig,
	logger *zap.Logger, mgr *semantic.Manager, edits int, dedicatedBase int64,
) {
	full := filepath.Join(tree, filepath.FromSlash(rel))
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(full, original, 0o644); err != nil {
			t.Errorf("restore %s: %v", rel, err)
		}
	})
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	// A daemon's store is long lived and its pages are in the OS cache; a
	// just-cloned file's are not. GX_DELTA_REAL_WARM=1 reads the copy once,
	// outside the timed region, so the builds measure warm reads.
	if os.Getenv("GX_DELTA_REAL_WARM") == "1" {
		if file, err := os.Open(copyPath); err == nil {
			_, _ = io.Copy(io.Discard, file)
			_ = file.Close()
		}
	}
	store := builderOpenStoreAt(t, copyPath)
	t.Cleanup(func() { _ = store.Close() })
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg, Logger: logger}
	if mgr != nil {
		builder.Semantic = mgr
	}
	chains := newDirtyChainBuilder(t, builder, store, tree, true)
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatalf("open the checkout's sampler: %v", err)
	}
	chains.sampler = sampler
	if dedicatedBase > 0 {
		chains.useDedicatedBase(dedicatedBase)
	}
	if n, _ := strconv.Atoi(os.Getenv("GX_DELTA_REAL_LAYER")); n > 0 {
		editDeltaRealLayerBelow(t, tree, rel, n, chains)
	}
	var records []editDeltaRealRecord
	retire := os.Getenv("GX_DELTA_REAL_RETIRE") != "0"
	var published []int64
	prevKey := ""
	if prof := os.Getenv("GX_DELTA_REAL_PROF"); prof != "" {
		file, err := os.Create(strings.TrimSuffix(prof, ".pprof") + "-" + strings.ReplaceAll(rel, "/", "_") + ".pprof")
		if err != nil {
			t.Fatalf("create the profile: %v", err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatalf("start the profile: %v", err)
		}
		t.Cleanup(func() {
			pprof.StopCPUProfile()
			_ = file.Close()
		})
	}
	for i := 1; i <= edits; i++ {
		edit := editDeltaRealBodyEdit
		switch os.Getenv("GX_DELTA_REAL_EDIT") {
		case "inplace":
			edit = editDeltaRealInPlaceEdit
		case "rename":
			edit = editDeltaRealRenameEdit
		}
		edited, ok := edit(string(original), i)
		if !ok {
			t.Fatalf("%s has no function body to edit", rel)
		}
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		recordLastEditDelta(nil)
		started := time.Now()
		cpuStarted := editDeltaProcessCPU()
		id, report, chain := chains.build()
		wall := time.Since(started)
		cpu := editDeltaProcessCPU() - cpuStarted
		// The edit's own delta, read before settle: a compaction there builds
		// again and would overwrite it.
		delta := LastEditDeltaReport()
		chains.settle()
		rec := editDeltaRealRecord{
			File: rel, Edit: i, Gen: id, WallMS: ms(wall), CPUMS: ms(cpu), Delta: delta,
			PassSteps: report.PassSteps, Plan: report.PlanSteps,
			Nodes: report.NodeCount, Edges: report.EdgeCount,
			WALValid: report.WAL.Valid, WALFrames: report.WAL.Frames, WALBytes: report.WAL.Bytes, WALReset: report.WAL.Reset,
		}
		if report.Work != nil {
			rec.Phases = report.Work.Phases
		}
		if census, err := store.GenerationPayloadRowCensus(t.Context(), id); err == nil {
			rec.Census = census.Tables
		}
		if retire {
			// Every generation the new chain no longer stands on is retired,
			// newest first so a child goes before the parent it references.
			mark := store.WALWriteMark()
			retireStarted := time.Now()
			var kept []int64
			for j := len(published) - 1; j >= 0; j-- {
				gen := published[j]
				if gen == id || slices.Contains(chains.chain, gen) {
					kept = append(kept, gen)
					continue
				}
				if err := store.RetirePayloadGeneration(t.Context(), gen, nil); err != nil {
					kept = append(kept, gen)
					continue
				}
				rec.Retired++
			}
			slices.Reverse(kept)
			published = kept
			if rec.Retired > 0 {
				delta := store_sqlite.WALWrittenBetween(mark, store.WALWriteMark())
				rec.RetireMS = ms(time.Since(retireStarted))
				rec.RetireFrames, rec.RetireBytes = delta.Frames, delta.Bytes
			}
		}
		if !slices.Contains(published, id) {
			published = append(published, id)
		}
		if dedicatedBase > 0 {
			prevKey = editDeltaRealCheckStackKey(t, rel, i, rec.Delta, chain, prevKey)
		}
		records = append(records, rec)
		t.Logf("%s edit %d: gen=%d wall=%.1fms cpu=%.1fms delta=%v wal=%d bytes (%d frames, valid=%t reset=%t) retired=%d (%d bytes) phases=%s pass=%s",
			rel, i, id, rec.WallMS, rec.CPUMS, rec.Delta != nil, rec.WALBytes, rec.WALFrames, rec.WALValid, rec.WALReset, rec.Retired, rec.RetireBytes,
			editDeltaPhaseLine(rec.Phases), editDeltaPhaseLine(rec.PassSteps))
		if rec.Delta != nil {
			t.Logf("%s edit %d: covered=%d claimed=%d materialized=%d/%d payload=%d/%d markers=%d tombstones=%d slow=%v",
				rel, i, rec.Delta.CoveredPaths, rec.Delta.ClaimedSources, rec.Delta.MaterializedNodes,
				rec.Delta.MaterializedEdges, rec.Delta.PayloadNodes, rec.Delta.PayloadEdges,
				rec.Delta.EdgeSources, rec.Delta.Tombstones, rec.Delta.SlowReads)
		}
	}
	if dir := os.Getenv("GX_DELTA_REAL_OUT"); dir != "" {
		data, err := json.MarshalIndent(records, "", "  ")
		if err == nil {
			name := strings.ReplaceAll(rel, "/", "_") + ".json"
			_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
	}
}

func editDeltaPhaseLine(phases []GenerationPhase) string {
	parts := make([]string, 0, len(phases))
	for _, p := range phases {
		parts = append(parts, p.Name+"="+strconv.FormatFloat(ms(p.Duration), 'f', 1, 64))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func editDeltaRealLogger(t *testing.T) *zap.Logger {
	t.Helper()
	path := os.Getenv("GX_DELTA_REAL_LOG")
	if path == "" {
		return zap.NewNop()
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	enc := zap.NewDevelopmentEncoderConfig()
	core := zapcore.NewCore(zapcore.NewConsoleEncoder(enc), zapcore.AddSync(file), zapcore.InfoLevel)
	return zap.New(core)
}

// editDeltaRealBodyEdit is the n-th successive save of a file: n statement
// lines at the top of its first function body. Each save shifts every later
// declaration by one line from the previous save, as typing a line does; a
// save that only changed a literal would be structurally inert.
func editDeltaRealBodyEdit(src string, n int) (string, bool) {
	lines := strings.SplitAfter(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "func ") && strings.HasSuffix(trimmed, "{") {
			out := make([]string, 0, len(lines)+n)
			out = append(out, lines[:i+1]...)
			for k := 1; k <= n; k++ {
				out = append(out, "\t_ = "+strconv.Itoa(k)+"\n")
			}
			out = append(out, lines[i+1:]...)
			return strings.Join(out, ""), true
		}
	}
	return "", false
}

// editDeltaRealRenameEdit is the n-th save of a rename and its undo, as the
// daemon measurement edits: odd saves rename the file's first unexported
// function declaration (its call sites keep the old name), even saves
// restore the original.
func editDeltaRealRenameEdit(src string, n int) (string, bool) {
	m := editDeltaRealFuncDecl.FindStringSubmatchIndex(src)
	if m == nil {
		return "", false
	}
	if n%2 == 0 {
		return src, true
	}
	name := src[m[2]:m[3]]
	return src[:m[2]] + name + "Ren" + strconv.Itoa(n) + src[m[3]:], true
}

var editDeltaRealFuncDecl = regexp.MustCompile(`(?m)^func ([a-z][A-Za-z0-9]*)\(`)

// editDeltaRealInPlaceEdit is the n-th successive save of a file that changes
// the first function body without moving a line: the statement lands on the
// line of the function's opening brace.
func editDeltaRealInPlaceEdit(src string, n int) (string, bool) {
	lines := strings.SplitAfter(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "func ") && strings.HasSuffix(trimmed, "{") {
			lines[i] = trimmed + " _ = " + strconv.Itoa(n) + "\n"
			return strings.Join(lines, ""), true
		}
	}
	return "", false
}

// editDeltaRealLayerBelow dirties n Go files other than rel and publishes them
// as one working-tree generation the file's edits then chain over. The files
// are restored when the test ends.
func editDeltaRealLayerBelow(t *testing.T, tree, rel string, n int, chains *dirtyChainBuilder) {
	t.Helper()
	listed := builderGit(t, tree, "ls-files", "*.go")
	var picked []string
	for _, path := range strings.Split(listed, "\n") {
		path = strings.TrimSpace(path)
		if path == "" || path == rel || strings.HasSuffix(path, "_test.go") {
			continue
		}
		picked = append(picked, path)
		if len(picked) == n {
			break
		}
	}
	for _, path := range picked {
		full := filepath.Join(tree, filepath.FromSlash(path))
		original, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(full, original, 0o644) })
		if err := os.WriteFile(full, append(append([]byte(nil), original...), []byte("\n// layer below\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chains.compact = false
	started := time.Now()
	id, report, _ := chains.build()
	t.Logf("layer below: generation %d over %d dirty files, %d nodes / %d edges, %s", id, len(picked), report.NodeCount, report.EdgeCount, time.Since(started).Round(time.Millisecond))
}
