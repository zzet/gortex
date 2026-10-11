//go:build acceptance

package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// TestAcceptanceHistoryReplayRealRepository is the gated real-repository arm. Knobs:
// GX_REPLAY_REAL_TREE (the private clone), GX_REPLAY_BASE (a whole-index store
// of its HEAD, cloned per run, never written), GX_REPLAY_STORE_DIR (scratch),
// GX_REPLAY_OUT (a JSON artifact of the step records).
func TestAcceptanceHistoryReplayRealRepository(t *testing.T) {
	tree := os.Getenv("GX_REPLAY_REAL_TREE")
	base := os.Getenv("GX_REPLAY_BASE")
	storeDir := os.Getenv("GX_REPLAY_STORE_DIR")
	if tree == "" || base == "" || storeDir == "" {
		t.Skip("set GX_REPLAY_REAL_TREE, GX_REPLAY_BASE and GX_REPLAY_STORE_DIR to a private clone, its base store and a scratch directory")
	}
	builderIsolateGit(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	full, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's config: %v", err)
	}
	if status := builderGit(t, tree, "status", "--porcelain"); status != "" {
		t.Fatalf("the clone is not clean at HEAD:\n%s", status)
	}

	// The linked worktree's directory name is its git admin name, which the
	// fixture's catalog identity records.
	worktree := filepath.Join(storeDir, coordinatorAdminName)
	storePath := filepath.Join(storeDir, "replay.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(storePath + suffix)
	}
	_ = exec.Command("git", "-C", tree, "worktree", "remove", "--force", worktree).Run()
	_ = os.RemoveAll(worktree)
	branch := fmt.Sprintf("replay-%d", time.Now().UnixNano())
	builderGit(t, tree, "worktree", "add", "-b", branch, worktree)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", tree, "worktree", "remove", "--force", worktree).Run()
		_ = exec.Command("git", "-C", tree, "branch", "-D", branch).Run()
		if os.Getenv("GX_REPLAY_KEEP") != "1" {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(storePath + suffix)
			}
		}
	})
	logPath := filepath.Join(storeDir, "replay.log")
	logger := realRepoLogger(t, logPath)
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		started := time.Now()
		baseStore := builderOpenStoreAt(t, base+".tmp")
		idx := New(baseStore, builderRegistry(), full.Index, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(tree); err != nil {
			t.Fatalf("index the base corpus: %v", err)
		}
		idx.Close()
		if err := baseStore.Close(); err != nil {
			t.Fatalf("close the base store: %v", err)
		}
		if err := os.Rename(base+".tmp", base); err != nil {
			t.Fatalf("publish the base store: %v", err)
		}
		t.Logf("indexed the base corpus in %v", time.Since(started))
	}
	if out, err := exec.Command("cp", "-c", base, storePath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatalf("open the replay store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	f := &coordinatorFixture{
		t: t, store: store, storePath: storePath, catalog: store.Catalog(), leases: graphview.NewLeaseManager(),
		primary: tree, worktree: worktree,
		familyID: "family-replay", graphID: GraphIDFor(builderRepoPrefix),
		primaryID: "checkout-primary", checkoutID: "checkout-replay",
		treeA: builderGit(t, tree, "rev-parse", "HEAD^{tree}"),
	}
	f.writeCatalogIdentity()
	f.registerRosterOwner(t)
	c := replayRealCoordinator(t, f, full, logger)
	coordinatorReconcile(t, c)

	o := newReplayOriginals(worktree)
	steps := []replayStep{
		replayBodyEdit(o, "internal/config/config.go", "replay-body-config"),
		replayBodyEdit(o, "internal/mcp/checkout_binding.go", "replay-body-binding"),
		replaySignatureChange(o, "internal/config/config.go"),
		replayAddDeclaration(o, "internal/config/config.go"),
		replayRenameInPackage(o, "internal/config/config.go"),
		replayAddFile(o, "internal/config/replay_added.go", "package config\n\n// ReplayAdded is a file added by the replay oracle.\nfunc ReplayAdded() *Config { return Default() }\n"),
		replayRemoveFile(o, "internal/progress/glyphs.go"),
		replayImportEdit(o, "internal/config/config.go", "hash/adler32", "var replayChecksum = adler32.Checksum(nil)"),
		replayUndo(o, "internal/mcp/checkout_binding.go"),
		replayUndoAll(o),
	}
	// GX_REPLAY_STEPS=n runs the first n steps, then the full undo.
	if n, err := strconv.Atoi(os.Getenv("GX_REPLAY_STEPS")); err == nil && n > 0 && n < len(steps) {
		steps = append(steps[:n:n], replayUndoAll(o))
	}
	h := &replayHarness{f: f, c: c, sampleLimit: 60, clean: func(t *testing.T, label string) (graph.Reader, func()) {
		// One clean store at a time: a repository-sized whole index is
		// ~1 GB, so each is removed as soon as its step is compared.
		path := filepath.Join(storeDir, label+".clean.sqlite")
		remove := func() {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
		}
		remove()
		clean := builderOpenStoreAt(t, path)
		release := func() {
			_ = clean.Close()
			remove()
		}
		idx := New(clean, builderRegistry(), full.Index, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(worktree); err != nil {
			release()
			t.Fatalf("%s: whole index of the worktree: %v", label, err)
		}
		idx.Close()
		return clean, release
	}}
	records := h.run(t, steps)
	_ = logger.Sync()
	if path := os.Getenv("GX_REPLAY_OUT"); path != "" {
		data, _ := json.MarshalIndent(records, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// replayRealCoordinator is fixture.coordinator with the clone's own index
// configuration (excludes, languages, dirty-chain switch) instead of the
// defaults, so the coordinator builds what the base index was built with.
func replayRealCoordinator(t *testing.T, f *coordinatorFixture, full *config.Config, logger *zap.Logger) *CheckoutCoordinator {
	t.Helper()
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:   f.checkoutID,
		CheckoutRoot: f.worktree,
		FamilyID:     f.familyID,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Store:        f.store,
		Builder: &SparseGenerationBuilder{
			Store: f.store, Registry: builderRegistry(), Config: full.Index, Logger: logger,
		},
		Leases:         f.leases,
		Config:         full.Index,
		ConfigSections: dedicatedBaseConfigSections(full),
		Logger:         logger,
		PollInterval:   -1,
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the coordinator loop: %v", err)
	}
	coordinator.compaction.quiet = -1
	return coordinator
}

func realRepoLogger(t testing.TB, path string) *zap.Logger {
	t.Helper()
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	cfg.OutputPaths = []string{path}
	cfg.ErrorOutputPaths = []string{path}
	cfg.Sampling = nil
	logger, err := cfg.Build()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return logger
}
