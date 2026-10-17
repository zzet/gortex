package indexer

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestWorkingTreeBuildLogsWhereItsPlanWent pins the one attribution line a
// working-tree build leaves on a live daemon: the preparation stages of the
// plan, the physical phases, and the sizes that drive them — so a slow plan
// names the read it waited on without a profiler attached — beside the
// delta's own line.
func TestWorkingTreeBuildLogsWhereItsPlanWent(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)
	core, logs := observer.New(zapcore.InfoLevel)
	builder := builderNewBuilder(store)
	builder.Logger = zap.New(core)

	generationID, report, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildDirtyLayer: %v", err)
	}
	entries := logs.FilterMessage("indexer: working-tree build phases").All()
	if len(entries) != 1 {
		t.Fatalf("%d build-phase lines, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if got, _ := fields["generation"].(int64); got != generationID {
		t.Fatalf("logged generation %v, want %d", fields["generation"], generationID)
	}
	for _, key := range []string{
		"plan_ms", "closure", "dependents", "declared", "parser_inputs",
		"plan_prepare_sample_ms", "plan_prepare_manifest_ms", "plan_prepare_disk_truth_ms",
		"phase_plan_ms", "phase_pass_ms", "phase_publish_ms",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("build-phase line lacks %q: %v", key, fields)
		}
	}
	// The per-file delta walks no closure: the fixture's caller.go is re-bound,
	// not re-derived.
	if got, _ := fields["dependents"].(int64); int(got) != len(report.ClosureDependentPaths) || got != 0 {
		t.Errorf("logged dependents %v, report %d, want none", fields["dependents"], len(report.ClosureDependentPaths))
	}
	if delta := logs.FilterMessage("indexer: working-tree edit delta").All(); len(delta) != 1 {
		t.Errorf("%d delta lines, want the one the build logs its own phases on", len(delta))
	}
}

// TestWorkingTreeBuildStampsProvenanceFromItsSample pins that a working-tree
// generation's repo index state carries the HEAD commit and dirty bit of the
// sample the build describes, and that the pass takes them from that sample
// rather than from git: with the provenance set, an Indexer rooted at a
// directory git knows nothing about still stamps the sampled commit.
func TestWorkingTreeBuildStampsProvenanceFromItsSample(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)
	generationID, _, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity:     builderDirtyIdentity(),
		Base:         store,
		CheckoutRoot: repoDir,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildDirtyLayer: %v", err)
	}
	head := builderGit(t, repoDir, "rev-parse", "HEAD")
	state, ok, err := store.AtGeneration(generationID).GetRepoIndexState(builderRepoPrefix)
	if err != nil || !ok {
		t.Fatalf("generation %d has no repo index state (ok=%v err=%v)", generationID, ok, err)
	}
	if state.IndexedSHA != head || !state.Dirty {
		t.Fatalf("generation provenance = %q dirty=%v, want the sampled HEAD %q and dirty", state.IndexedSHA, state.Dirty, head)
	}

	idx := New(store, builderRegistry(), builderNewBuilder(store).Config, zap.NewNop())
	defer idx.Close()
	idx.headProvenance = &repoHeadProvenance{sha: "0123456789abcdef0123456789abcdef01234567", dirty: true}
	if sha, dirty := idx.headAndDirty(t.TempDir()); sha != idx.headProvenance.sha || !dirty {
		t.Fatalf("headAndDirty ignored the provenance: %q %v", sha, dirty)
	}
	if sha := idx.head(t.TempDir()); sha != idx.headProvenance.sha {
		t.Fatalf("head ignored the provenance: %q", sha)
	}
}
