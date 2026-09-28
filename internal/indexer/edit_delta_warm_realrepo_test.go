package indexer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

// TestRealRepoWarmEditDelta measures the daemon's warm edit in process: an
// edit of one file, built over the checkout's base with the per-stack caches
// on (a fixed stack key, as a routed commit layer has), then the file put back
// without a build — the daemon's undo routes back to the base — so the next
// edit stands on the same stack, as in the edit-then-undo windows. (The
// chained real-repository test builds without a stack key, so every per-stack
// cache is off there.) Opt-in: GX_DELTA_WARM_TREE (a private clone) and
// GX_DELTA_WARM_STORE (its base store, as TestRealRepoEditDelta builds it);
// GX_DELTA_WARM_FILES, GX_DELTA_WARM_EDITS (default 6; the first edit of each
// file is the cold one), GX_DELTA_WARM_SEMANTIC=1, GX_DELTA_WARM_OUT and
// GX_DELTA_WARM_PROF as the chained test's.
func TestRealRepoWarmEditDelta(t *testing.T) {
	tree := os.Getenv("GX_DELTA_WARM_TREE")
	storePath := os.Getenv("GX_DELTA_WARM_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_DELTA_WARM_TREE / GX_DELTA_WARM_STORE are not set")
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
	files := editDeltaRealDefaultFiles
	if raw := os.Getenv("GX_DELTA_WARM_FILES"); raw != "" {
		files = strings.Split(raw, ",")
	}
	edits := 6
	if n, err := strconv.Atoi(os.Getenv("GX_DELTA_WARM_EDITS")); err == nil && n > 0 {
		edits = n
	}
	var mgr *semantic.Manager
	if os.Getenv("GX_DELTA_WARM_SEMANTIC") == "1" {
		mgr = semantic.NewManager(semantic.Config{Enabled: true}, logger)
		mgr.RegisterProvider(goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, logger))
		t.Cleanup(func() { _ = mgr.Close() })
	}
	for _, rel := range files {
		rel := strings.TrimSpace(rel)
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			editDeltaWarmFile(t, tree, storePath, rel, cfg.Index, mgr, edits)
		})
	}
}

// editDeltaWarmRecord is one warm edit's measurement.
type editDeltaWarmRecord struct {
	File   string             `json:"file"`
	Edit   int                `json:"edit"`
	WallMS float64            `json:"wall_ms"`
	CPUMS  float64            `json:"cpu_ms"`
	Phases map[string]float64 `json:"phases_ms"`
	CPU    map[string]float64 `json:"phase_cpu_ms"`
	Build  map[string]float64 `json:"build_phases_ms"`
}

func editDeltaWarmFile(t *testing.T, tree, storePath, rel string, cfg config.IndexConfig, mgr *semantic.Manager, edits int) {
	full := filepath.Join(tree, filepath.FromSlash(rel))
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	t.Cleanup(func() { _ = os.WriteFile(full, original, 0o644) })
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	if file, err := os.Open(copyPath); err == nil {
		_, _ = io.Copy(io.Discard, file)
		_ = file.Close()
	}
	store := builderOpenStoreAt(t, copyPath)
	t.Cleanup(func() { _ = store.Close() })
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg, Logger: editDeltaRealLogger(t)}
	if mgr != nil {
		builder.Semantic = mgr
	}
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatalf("open the checkout's sampler: %v", err)
	}
	if prof := os.Getenv("GX_DELTA_WARM_PROF"); prof != "" {
		file, err := os.Create(strings.TrimSuffix(prof, ".pprof") + "-" + strings.ReplaceAll(rel, "/", "_") + ".pprof")
		if err != nil {
			t.Fatalf("create the profile: %v", err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatalf("start the profile: %v", err)
		}
		t.Cleanup(func() { pprof.StopCPUProfile(); _ = file.Close() })
	}
	var records []editDeltaWarmRecord
	for i := 1; i <= edits; i++ {
		edited, ok := editDeltaRealBodyEdit(string(original), i)
		if !ok {
			t.Fatalf("%s has no function body to edit", rel)
		}
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		recordLastEditDelta(nil)
		started, cpuStarted := time.Now(), editDeltaProcessCPU()
		_, report, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
			Identity: builderDirtyIdentity(),
			// A fixed stack key: the per-stack caches are on, and every edit
			// stands on the same stack, as after the daemon's undo.
			Base:         commitLayerBase{Reader: store, corpus: store, stack: []int64{1}},
			CheckoutRoot: tree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			Sampler: sampler,
		})
		if err != nil {
			t.Fatalf("%s edit %d: %v", rel, i, err)
		}
		rec := editDeltaWarmRecord{File: rel, Edit: i, WallMS: ms(time.Since(started)), CPUMS: ms(editDeltaProcessCPU() - cpuStarted),
			Phases: map[string]float64{}, CPU: map[string]float64{}, Build: map[string]float64{}}
		if d := LastEditDeltaReport(); d != nil {
			for _, p := range d.Phases {
				rec.Phases[p.Name] += ms(p.Duration)
			}
			for name, c := range d.PhaseCPU {
				rec.CPU[name] = ms(c)
			}
		}
		if report.Work != nil {
			for _, p := range report.Work.Phases {
				rec.Build[p.Name] += ms(p.Duration)
			}
		}
		records = append(records, rec)
		t.Logf("%s warm edit %d: wall=%.1fms cpu=%.1fms %s", rel, i, rec.WallMS, rec.CPUMS, editDeltaWarmLine(rec.Phases))
		// The undo: the file back, no build (the daemon routes to the base).
		if err := os.WriteFile(full, original, 0o644); err != nil {
			t.Fatalf("restore %s: %v", rel, err)
		}
	}
	if dir := os.Getenv("GX_DELTA_WARM_OUT"); dir != "" {
		if data, err := json.MarshalIndent(records, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dir, strings.ReplaceAll(rel, "/", "_")+".json"), data, 0o644)
		}
	}
}

func editDeltaWarmLine(phases map[string]float64) string {
	parts := make([]string, 0, len(phases))
	for name, v := range phases {
		if v >= 5 {
			parts = append(parts, name+"="+strconv.FormatFloat(v, 'f', 0, 64))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
