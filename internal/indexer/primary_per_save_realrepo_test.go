package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The primary checkout's per-save path measured on a real repository. It is
// skipped unless GX_REALREPO_TREE names a private clone and
// GX_REALREPO_STORE_DIR a directory holding its base index (base.sqlite, built
// by TestRealRepoResolverScope with GX_REALREPO_BASE_ONLY=1). Every run works
// on an APFS clone of the base that is removed afterwards (kept as
// exp-persave-<file>.sqlite with GX_REALREPO_KEEP=1).
//
// Per file it performs GX_PERSAVE_SAVES (default 4) consecutive body edits,
// each a single IndexFile call at GOMAXPROCS=1, and records per save:
// the IndexFile wall time, the reconcile phases the batch pipeline logs, the
// wall time between the incremental catch-up stages (resolve, dataflow,
// ref_facts, semantic, derived) and the derived coordinator's per-family
// report. The first save of a file is the one after a whole index; the later
// saves are the warm steady state. GX_PERSAVE_EDIT_ONCE=1 performs exactly one
// save with the resolver-scope body edit, so a kept store is comparable with
// TestRealRepoResolverScope's clean arm of the same edit.
// GX_PERSAVE_FIRST_UNCHANGED=1 makes the first save rewrite the unchanged
// bytes.

type perSaveSample struct {
	Save       int                `json:"save"`
	IndexFile  float64            `json:"index_file_ms"`
	Stages     map[string]float64 `json:"stages_ms"`
	Reconcile  map[string]float64 `json:"reconcile_ms"`
	Derived    map[string]any     `json:"derived,omitempty"`
	OtherSlowS []string           `json:"slow_lines,omitempty"`
}

type perSaveRecord struct {
	File    string          `json:"file"`
	Samples []perSaveSample `json:"samples"`
}

func TestRealRepoPrimaryPerSave(t *testing.T) {
	tree := os.Getenv("GX_REALREPO_TREE")
	storeDir := os.Getenv("GX_REALREPO_STORE_DIR")
	if tree == "" || storeDir == "" {
		t.Skip("set GX_REALREPO_TREE and GX_REALREPO_STORE_DIR to a private clone and store directory")
	}
	base := filepath.Join(storeDir, "base.sqlite")
	if _, err := os.Stat(base); err != nil {
		t.Skipf("no base store at %s: %v", base, err)
	}
	builderIsolateGit(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's config: %v", err)
	}
	runtime.GOMAXPROCS(1)
	files := strings.Split(envOr("GX_PERSAVE_FILES", "internal/config/config.go,internal/mcp/checkout_binding.go"), ",")
	saves := 4
	if n, err := strconv.Atoi(os.Getenv("GX_PERSAVE_SAVES")); err == nil && n > 0 {
		saves = n
	}
	once := os.Getenv("GX_PERSAVE_EDIT_ONCE") == "1"
	if once {
		saves = 1
	}
	var out []perSaveRecord
	for _, rel := range files {
		out = append(out, runPrimaryPerSave(t, tree, storeDir, base, cfg.Index, rel, saves, once))
	}
	if path := os.Getenv("GX_REALREPO_OUT"); path != "" {
		data, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func runPrimaryPerSave(
	t *testing.T, tree, storeDir, base string, indexCfg config.IndexConfig, rel string, saves int, once bool,
) perSaveRecord {
	t.Helper()
	label := strings.ReplaceAll(rel, "/", "_")
	expPath := filepath.Join(storeDir, "exp.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(expPath + suffix)
	}
	if out, err := exec.Command("cp", "-c", base, expPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store, err := store_sqlite.Open(expPath)
	if err != nil {
		t.Fatalf("open the experiment store: %v", err)
	}
	defer func() {
		_ = store.Close()
		if os.Getenv("GX_REALREPO_KEEP") == "1" {
			_ = os.Rename(expPath, filepath.Join(storeDir, "exp-persave-"+label+".sqlite"))
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(expPath + suffix)
		}
	}()

	logPath := filepath.Join(storeDir, "persave-"+label+".log")
	_ = os.Remove(logPath)
	fileLogger := realRepoLogger(t, logPath)
	core, observed := observer.New(zapcore.InfoLevel)
	logger := zap.New(zapcore.NewTee(fileLogger.Core(), core))

	full := filepath.Join(tree, filepath.FromSlash(rel))
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	defer func() {
		if err := os.WriteFile(full, original, 0o644); err != nil {
			t.Fatalf("restore %s: %v", rel, err)
		}
	}()

	idx := New(store, builderRegistry(), indexCfg, logger)
	idx.resolver.SetLogger(logger)
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	idx.rootPath = tree
	defer idx.Close()

	type mark struct {
		kind string
		at   time.Time
	}
	var marks []mark
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		marks = append(marks, mark{kind: kind, at: time.Now()})
	}

	rec := perSaveRecord{File: rel}
	for save := 1; save <= saves; save++ {
		marker := fmt.Sprintf("primary-per-save-%d", save)
		if once {
			marker = "resolver-scope-body-edit"
		}
		content := perSaveBodyEdit(t, string(original), marker)
		if save == 1 && os.Getenv("GX_PERSAVE_FIRST_UNCHANGED") == "1" {
			// A re-save of the unchanged bytes: proves the whole index stamped
			// the fingerprints the per-save path compares against.
			content = string(original)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		marks = marks[:0]
		first := observed.Len()
		started := time.Now()
		realRepoProfile(t, storeDir, label, fmt.Sprintf("persave_%d", save), func() {
			if err := idx.IndexFile(full); err != nil {
				t.Fatalf("%s save %d: IndexFile: %v", rel, save, err)
			}
		})
		ended := time.Now()
		sample := perSaveSample{
			Save:      save,
			IndexFile: ms(ended.Sub(started)),
			Stages:    map[string]float64{},
			Reconcile: map[string]float64{},
		}
		prev := mark{kind: "reconcile", at: started}
		for _, m := range marks {
			sample.Stages[prev.kind] += ms(m.at.Sub(prev.at))
			prev = m
		}
		sample.Stages[prev.kind] += ms(ended.Sub(prev.at))
		for _, entry := range observed.All()[first:] {
			fields := entry.ContextMap()
			switch entry.Message {
			case "indexer: reconcile phase complete":
				phase, _ := fields["phase"].(string)
				if elapsed, ok := fields["elapsed"].(time.Duration); ok {
					sample.Reconcile[phase] += ms(elapsed)
				}
			case "incremental derived passes complete":
				derived := map[string]any{}
				for k, v := range fields {
					if k == "framework_per_synth" {
						continue
					}
					derived[k] = v
				}
				sample.Derived = derived
			case "indexer: slow incremental stages":
				data, _ := json.Marshal(fields)
				sample.OtherSlowS = append(sample.OtherSlowS, string(data))
			}
		}
		summary, _ := json.Marshal(sample)
		t.Logf("%s save %d: %s", rel, save, summary)
		rec.Samples = append(rec.Samples, sample)
	}
	_ = fileLogger.Sync()
	return rec
}

// perSaveBodyEdit inserts one statement carrying marker at the top of the
// first function body whose signature line ends in "{": a body-only change.
func perSaveBodyEdit(t testing.TB, src, marker string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "func ") && strings.HasSuffix(strings.TrimSpace(line), "{") {
			edited := append([]string(nil), lines[:i+1]...)
			edited = append(edited, "\t_ = len(\""+marker+"\")")
			edited = append(edited, lines[i+1:]...)
			return strings.Join(edited, "\n")
		}
	}
	t.Fatalf("no function body")
	return ""
}
