package gitstate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/testutil/gitpromisor"
)

func TestLocalGitEnvFromDeduplicatesFixedOverrides(t *testing.T) {
	got := localGitEnvFrom([]string{
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
		t.Fatalf("localGitEnvFrom() = %#v, want %#v", got, want)
	}
}

func TestResolveViewSelectorPromisorObjectsNeverFetch(t *testing.T) {
	fixture := gitpromisor.New(t)
	missingCommit := fixture.Clone(t, "blob:none")
	missingTree := fixture.Clone(t, "tree:0")
	missingBlob := fixture.Clone(t, "blob:none")

	if missingCommit.ObjectPresent(t, fixture.OtherCommitOID) {
		t.Fatalf("commit fixture unexpectedly contains %s", fixture.OtherCommitOID)
	}
	if !missingTree.ObjectPresent(t, fixture.CommitOID) || missingTree.ObjectPresent(t, fixture.RootTreeOID) {
		t.Fatal("tree:0 fixture must contain the selected commit and omit its root tree")
	}
	if !missingBlob.ObjectPresent(t, fixture.RootTreeOID) || missingBlob.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatal("blob:none fixture must contain the root tree and omit its nested blob")
	}

	commitControl := fixture.Clone(t, "blob:none")
	commitControl.FetchAndRequireRequest(t, fixture.OtherCommitOID)
	treeControl := fixture.Clone(t, "tree:0")
	treeControl.FetchAndRequireRequest(t, fixture.RootTreeOID)

	t.Run("missing direct commit", func(t *testing.T) {
		_, err := ResolveViewSelector(context.Background(), missingCommit.Dir, ViewSelectorCommit, fixture.OtherCommitOID)
		if !errors.Is(err, ErrRefNotAvailableLocally) {
			t.Fatalf("ResolveViewSelector() error = %v, want ErrRefNotAvailableLocally", err)
		}
		assertNoSelectorUploadPackRequests(t, missingCommit)
	})

	t.Run("ref points at missing commit", func(t *testing.T) {
		missingCommit.WriteRef(t, "refs/heads/promised", fixture.OtherCommitOID)
		_, err := ResolveViewSelector(context.Background(), missingCommit.Dir, ViewSelectorGitRef, "refs/heads/promised")
		if !errors.Is(err, ErrRefNotAvailableLocally) {
			t.Fatalf("ResolveViewSelector() error = %v, want ErrRefNotAvailableLocally", err)
		}
		assertNoSelectorUploadPackRequests(t, missingCommit)
	})

	t.Run("missing commit tree", func(t *testing.T) {
		_, err := ResolveViewSelector(context.Background(), missingTree.Dir, ViewSelectorCommit, fixture.CommitOID)
		if !errors.Is(err, ErrRefNotAvailableLocally) {
			t.Fatalf("ResolveViewSelector() error = %v, want ErrRefNotAvailableLocally", err)
		}
		assertNoSelectorUploadPackRequests(t, missingTree)
	})

	t.Run("missing blobs do not block selector", func(t *testing.T) {
		got, err := ResolveViewSelector(context.Background(), missingBlob.Dir, ViewSelectorCommit, fixture.CommitOID)
		if err != nil {
			t.Fatalf("ResolveViewSelector() error = %v", err)
		}
		if got.CommitOID != fixture.CommitOID || got.TreeOID != fixture.RootTreeOID {
			t.Fatalf("ResolveViewSelector() = %#v", got)
		}
		assertNoSelectorUploadPackRequests(t, missingBlob)
	})
}

func TestResolveViewSelectorPreservesNonAvailabilityFailures(t *testing.T) {
	_, err := ResolveViewSelector(context.Background(), t.TempDir(), ViewSelectorCommit, strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("ResolveViewSelector() succeeded outside a Git repository")
	}
	if errors.Is(err, ErrRefNotAvailableLocally) {
		t.Fatalf("ResolveViewSelector() error = %v, unexpectedly ErrRefNotAvailableLocally", err)
	}

	fixture := gitpromisor.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ResolveViewSelector(ctx, fixture.CompleteDir, ViewSelectorCommit, fixture.CommitOID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveViewSelector() cancellation error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrRefNotAvailableLocally) {
		t.Fatalf("ResolveViewSelector() cancellation error = %v, unexpectedly ErrRefNotAvailableLocally", err)
	}
}

func assertNoSelectorUploadPackRequests(t testing.TB, client *gitpromisor.Client) {
	t.Helper()
	if got := client.RequestCount(t); got != 0 {
		t.Fatalf("upload-pack requests = %d, want 0", got)
	}
}
