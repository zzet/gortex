package indexer

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestWorkingTreeBuildReportsTheWALItWrote pins the per-edit log counter: a
// working-tree build's report and its build-phase line carry the write-ahead
// log the store appended while it ran (frames and bytes, from the wal-index
// header), and a build that publishes a generation appended some.
func TestWorkingTreeBuildReportsTheWALItWrote(t *testing.T) {
	store := builderOpenStore(t, "base")
	repoDir := builderDirtyCheckout(t, store)
	core, logs := observer.New(zapcore.InfoLevel)
	builder := builderNewBuilder(store)
	builder.Logger = zap.New(core)

	_, report, err := builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
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
	if !report.WAL.Valid || report.WAL.Frames <= 0 || report.WAL.Bytes <= report.WAL.Frames {
		t.Fatalf("report.WAL = %+v, want the frames and bytes a published build appended", report.WAL)
	}
	entries := logs.FilterMessage("indexer: working-tree build phases").All()
	if len(entries) != 1 {
		t.Fatalf("%d build-phase lines, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if got, _ := fields["wal_frames"].(int64); got != report.WAL.Frames {
		t.Errorf("logged wal_frames %v, report %d", fields["wal_frames"], report.WAL.Frames)
	}
	if got, _ := fields["wal_bytes"].(int64); got != report.WAL.Bytes {
		t.Errorf("logged wal_bytes %v, report %d", fields["wal_bytes"], report.WAL.Bytes)
	}
	for _, key := range []string{"wal_valid", "wal_reset"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("build-phase line lacks %q", key)
		}
	}
}
