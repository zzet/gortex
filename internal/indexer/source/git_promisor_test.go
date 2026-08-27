package source

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/testutil/gitpromisor"
)

func TestGitEnvFromDeduplicatesFixedOverrides(t *testing.T) {
	got := gitEnvFrom([]string{
		"KEEP=first",
		"GIT_NO_LAZY_FETCH=0",
		"GIT_TERMINAL_PROMPT=1",
		"KEEP=second",
		"GIT_NO_LAZY_FETCH=maybe",
		"GIT_OPTIONAL_LOCKS=1",
	})
	want := []string{
		"KEEP=first",
		"KEEP=second",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("gitEnvFrom() = %#v, want %#v", got, want)
	}
}

func TestGitTreeSourcePromisorObjectsNeverFetch(t *testing.T) {
	fixture := gitpromisor.New(t)
	rootMissing := fixture.Clone(t, "tree:0")
	nestedTreeMissing := fixture.Clone(t, "tree:1")
	blobMissing := fixture.Clone(t, "blob:none")

	if rootMissing.ObjectPresent(t, fixture.RootTreeOID) {
		t.Fatalf("tree:0 fixture unexpectedly contains root tree %s", fixture.RootTreeOID)
	}
	if !nestedTreeMissing.ObjectPresent(t, fixture.RootTreeOID) {
		t.Fatalf("tree:1 fixture is missing root tree %s", fixture.RootTreeOID)
	}
	if nestedTreeMissing.ObjectPresent(t, fixture.NestedTreeOID) {
		t.Fatalf("tree:1 fixture unexpectedly contains nested tree %s", fixture.NestedTreeOID)
	}
	if !blobMissing.ObjectPresent(t, fixture.RootTreeOID) || !blobMissing.ObjectPresent(t, fixture.NestedTreeOID) {
		t.Fatal("blob:none fixture must contain the complete tree closure")
	}
	if blobMissing.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatalf("blob:none fixture unexpectedly contains blob %s", fixture.NestedBlobOID)
	}

	rootControl := fixture.Clone(t, "tree:0")
	rootControl.FetchAndRequireRequest(t, fixture.RootTreeOID)
	nestedControl := fixture.Clone(t, "tree:1")
	nestedControl.FetchAndRequireRequest(t, fixture.NestedTreeOID)
	blobControl := fixture.Clone(t, "blob:none")
	blobControl.FetchAndRequireRequest(t, fixture.NestedBlobOID)

	t.Run("missing root tree", func(t *testing.T) {
		_, err := NewGitTreeSource(context.Background(), rootMissing.Dir, fixture.RootTreeOID)
		if !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("NewGitTreeSource() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, rootMissing)
		if err := VerifyGitTreeObjectsLocal(context.Background(), rootMissing.Dir, fixture.RootTreeOID); !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("VerifyGitTreeObjectsLocal() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, rootMissing)
	})

	t.Run("missing descendant tree", func(t *testing.T) {
		_, err := NewGitTreeSource(context.Background(), nestedTreeMissing.Dir, fixture.RootTreeOID)
		if !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("NewGitTreeSource() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, nestedTreeMissing)
		if err := VerifyGitTreeObjectsLocal(context.Background(), nestedTreeMissing.Dir, fixture.RootTreeOID); !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("VerifyGitTreeObjectsLocal() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, nestedTreeMissing)
	})

	t.Run("missing blob through batch", func(t *testing.T) {
		tree, err := NewGitTreeSource(context.Background(), blobMissing.Dir, fixture.RootTreeOID)
		if err != nil {
			t.Fatalf("NewGitTreeSource() error = %v", err)
		}
		defer tree.Close()
		_, _, err = tree.Open("nested/missing.go")
		if !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("Open() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, blobMissing)
		if err := VerifyGitTreeObjectsLocal(context.Background(), blobMissing.Dir, fixture.RootTreeOID); !errors.Is(err, ErrObjectMissing) {
			t.Fatalf("VerifyGitTreeObjectsLocal() error = %v, want ErrObjectMissing", err)
		}
		assertNoUploadPackRequests(t, blobMissing)
	})

	t.Run("complete closure", func(t *testing.T) {
		if err := VerifyGitTreeObjectsLocal(context.Background(), fixture.CompleteDir, fixture.RootTreeOID); err != nil {
			t.Fatalf("VerifyGitTreeObjectsLocal() error = %v", err)
		}
		tree, err := NewGitTreeSource(context.Background(), fixture.CompleteDir, fixture.RootTreeOID)
		if err != nil {
			t.Fatalf("NewGitTreeSource() error = %v", err)
		}
		defer tree.Close()
		reader, _, err := tree.Open("nested/missing.go")
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer reader.Close()
		want := "package renamed\n\nfunc Alpha() int { return 2 }\nfunc Shared() string { return \"same\" }\n"
		if got, err := io.ReadAll(reader); err != nil || string(got) != want {
			t.Fatalf("ReadAll() = %q, %v", got, err)
		}
	})
}

func TestGitTreeSourceDoesNotNormalizeUnrelatedGitFailure(t *testing.T) {
	_, err := NewGitTreeSource(context.Background(), t.TempDir(), strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("NewGitTreeSource() succeeded outside a Git repository")
	}
	if errors.Is(err, ErrObjectMissing) {
		t.Fatalf("NewGitTreeSource() error = %v, unexpectedly ErrObjectMissing", err)
	}
}

func BenchmarkVerifyGitTreeObjectsLocal(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missingTree := fixture.Clone(b, "tree:1")
	missingBlob := fixture.Clone(b, "blob:none")
	if !complete.ObjectPresent(b, fixture.NestedBlobOID) {
		b.Fatal("complete control is missing its nested blob")
	}
	if missingTree.ObjectPresent(b, fixture.NestedTreeOID) {
		b.Fatal("missing-tree benchmark fixture contains its nested tree")
	}
	if missingBlob.ObjectPresent(b, fixture.NestedBlobOID) {
		b.Fatal("missing-blob benchmark fixture contains its nested blob")
	}

	benchmarkVerifyGitTreeObjectsLocal(b, "complete", complete, fixture.RootTreeOID, false)
	benchmarkVerifyGitTreeObjectsLocal(b, "missing-tree", missingTree, fixture.RootTreeOID, true)
	benchmarkVerifyGitTreeObjectsLocal(b, "missing-blob", missingBlob, fixture.RootTreeOID, true)
}

func BenchmarkNewGitTreeSourceNoLazyFetch(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missingTree := fixture.Clone(b, "tree:1")
	benchmarkNewGitTreeSource(b, "complete", complete, fixture.RootTreeOID, false)
	benchmarkNewGitTreeSource(b, "missing-tree", missingTree, fixture.RootTreeOID, true)
}

func BenchmarkGitTreeSourceBlobBatchNoLazyFetch(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missingBlob := fixture.Clone(b, "blob:none")
	benchmarkGitTreeSourceBlobBatch(b, "complete", complete, fixture.RootTreeOID, false)
	benchmarkGitTreeSourceBlobBatch(b, "missing-blob", missingBlob, fixture.RootTreeOID, true)
}

func benchmarkVerifyGitTreeObjectsLocal(b *testing.B, name string, client *gitpromisor.Client, treeOID string, wantMissing bool) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			err := VerifyGitTreeObjectsLocal(context.Background(), client.Dir, treeOID)
			if errors.Is(err, ErrObjectMissing) != wantMissing || (!wantMissing && err != nil) {
				b.Fatalf("VerifyGitTreeObjectsLocal() error = %v, want missing=%t", err, wantMissing)
			}
		}
		b.StopTimer()
		reportZeroUploadPackRequests(b, client)
	})
}

func benchmarkNewGitTreeSource(b *testing.B, name string, client *gitpromisor.Client, treeOID string, wantMissing bool) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			tree, err := NewGitTreeSource(context.Background(), client.Dir, treeOID)
			if errors.Is(err, ErrObjectMissing) != wantMissing || (!wantMissing && err != nil) {
				b.Fatalf("NewGitTreeSource() error = %v, want missing=%t", err, wantMissing)
			}
			if tree != nil {
				_ = tree.Close()
			}
		}
		b.StopTimer()
		reportZeroUploadPackRequests(b, client)
	})
}

func benchmarkGitTreeSourceBlobBatch(b *testing.B, name string, client *gitpromisor.Client, treeOID string, wantMissing bool) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		tree, err := NewGitTreeSource(context.Background(), client.Dir, treeOID)
		if err != nil {
			b.Fatalf("NewGitTreeSource() error = %v", err)
		}
		defer tree.Close()
		_, _, err = tree.Open("nested/missing.go")
		if errors.Is(err, ErrObjectMissing) != wantMissing || (!wantMissing && err != nil) {
			b.Fatalf("warm Open() error = %v, want missing=%t", err, wantMissing)
		}
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			reader, _, err := tree.Open("nested/missing.go")
			if errors.Is(err, ErrObjectMissing) != wantMissing || (!wantMissing && err != nil) {
				b.Fatalf("Open() error = %v, want missing=%t", err, wantMissing)
			}
			if reader != nil {
				_ = reader.Close()
			}
		}
		b.StopTimer()
		reportZeroUploadPackRequests(b, client)
	})
}

func assertNoUploadPackRequests(t testing.TB, client *gitpromisor.Client) {
	t.Helper()
	if got := client.RequestCount(t); got != 0 {
		t.Fatalf("upload-pack requests = %d, want 0", got)
	}
}

func reportZeroUploadPackRequests(b *testing.B, client *gitpromisor.Client) {
	b.Helper()
	requests := client.RequestCount(b)
	b.ReportMetric(float64(requests)/float64(b.N), "upload-pack/op")
	if requests != 0 {
		b.Fatalf("upload-pack requests = %d, want 0", requests)
	}
}
