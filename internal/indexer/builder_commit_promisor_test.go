package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/testutil/gitpromisor"
	"go.uber.org/zap"
)

func TestBuildCommitLayerNeverFetchesPromisedRenameBlobs(t *testing.T) {
	fixture := gitpromisor.New(t)
	subject := fixture.Clone(t, "blob:none")
	if subject.ObjectPresent(t, fixture.BaseBlobOID) || subject.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatal("blob:none subject unexpectedly contains a rename blob")
	}

	legacyControl := fixture.Clone(t, "blob:none")
	if _, err := gitcmd.Run(context.Background(), legacyControl.Dir,
		"diff", "--name-status", "-M", "-C", "-z", fixture.BaseTreeOID, fixture.RootTreeOID); err != nil {
		t.Fatalf("legacy rename-detecting diff: %v", err)
	}
	if got := legacyControl.RequestCount(t); got == 0 {
		t.Fatal("positive control rename diff did not trigger upload-pack")
	}
	if !legacyControl.ObjectPresent(t, fixture.BaseBlobOID) || !legacyControl.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatal("positive control did not materialize both rename blobs")
	}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	builder := &SparseGenerationBuilder{
		Store:    store,
		Registry: registry,
		Config:   config.Default().Index,
		Logger:   zap.NewNop(),
	}
	generationID, _, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:            "ref",
			GraphID:              "promisor-builder-test",
			LayerID:              "promisor-builder-test",
			LowerViewFingerprint: fixture.BaseTreeOID,
			ProvenanceCommitOID:  fixture.CommitOID,
		},
		Base:          store.AtGeneration(0),
		RepoDir:       subject.Dir,
		BaseTreeOID:   fixture.BaseTreeOID,
		TargetTreeOID: fixture.RootTreeOID,
		RootPath:      subject.Dir,
		RepoPrefix:    "promisor-builder-test",
		WorkspaceID:   "promisor-builder-test",
		ProjectID:     "promisor-builder-test",
	})
	if !errors.Is(err, source.ErrObjectMissing) {
		t.Fatalf("BuildCommitLayer() error = %v, want ErrObjectMissing", err)
	}
	if generationID != 0 {
		t.Fatalf("BuildCommitLayer() generation = %d, want 0 on failure", generationID)
	}
	if got := subject.RequestCount(t); got != 0 {
		t.Fatalf("upload-pack requests = %d, want 0", got)
	}
	if subject.ObjectPresent(t, fixture.BaseBlobOID) || subject.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatal("BuildCommitLayer() materialized a promised rename blob")
	}
}

func BenchmarkCommitLayerDiffNoLazyFetch(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missing := fixture.Clone(b, "blob:none")
	benchmarkCommitLayerDiff(b, "complete", complete, fixture.BaseTreeOID, fixture.RootTreeOID)
	benchmarkCommitLayerDiff(b, "missing-blobs", missing, fixture.BaseTreeOID, fixture.RootTreeOID)
}

func benchmarkCommitLayerDiff(b *testing.B, name string, client *gitpromisor.Client, baseTree, targetTree string) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			changes, err := diffTreeChanges(context.Background(), client.Dir, baseTree, targetTree)
			if err != nil {
				b.Fatalf("diffTreeChanges() error = %v", err)
			}
			if len(changes) != 3 {
				b.Fatalf("diffTreeChanges() returned %d changes, want 3", len(changes))
			}
		}
		b.StopTimer()
		requests := client.RequestCount(b)
		b.ReportMetric(float64(requests)/float64(b.N), "upload-pack/op")
		if requests != 0 {
			b.Fatalf("upload-pack requests = %d, want 0", requests)
		}
	})
}
