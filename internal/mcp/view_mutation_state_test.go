package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/indexer"
)

type fakeCheckoutMutationLifecycle struct {
	prepare func(context.Context) error
	refresh func(context.Context) (indexer.CheckoutCycle, error)
}

func (f *fakeCheckoutMutationLifecycle) Prepare(ctx context.Context) error {
	if f.prepare != nil {
		return f.prepare(ctx)
	}
	return nil
}

func (f *fakeCheckoutMutationLifecycle) Refresh(ctx context.Context) (indexer.CheckoutCycle, error) {
	if f.refresh != nil {
		return f.refresh(ctx)
	}
	return indexer.CheckoutCycle{CommitGenerationID: 4, DirtyGenerationID: 5}, nil
}

func TestCheckoutMutationPathConfinement(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	for _, path := range []string{filepath.Join(root, "file.go"), filepath.Join(root, "new", "nested", "file.go")} {
		if err := guardCheckoutMutationPath(ctx, path); err != nil {
			t.Fatalf("selected path %q: %v", path, err)
		}
	}
	for _, path := range []string{root, "file.go", filepath.Join(other, "file.go"), root + "-sibling/file.go"} {
		if err := guardCheckoutMutationPath(ctx, path); err == nil {
			t.Fatalf("accepted path outside selected working copy: %q", path)
		}
	}
	for _, path := range []string{filepath.Join(root, ".git"), filepath.Join(root, ".git", "HEAD"), filepath.Join(root, ".GIT", "config"), filepath.Join(root, "nested", ".git", "HEAD")} {
		if err := guardCheckoutMutationPath(ctx, path); err == nil || !strings.Contains(err.Error(), "Git metadata") {
			t.Fatalf("Git metadata refusal for %q = %v", path, err)
		}
	}

	outsideLink := filepath.Join(root, "outside")
	if err := os.Symlink(other, outsideLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(outsideLink, "new.go"), filepath.Join(outsideLink, "new", "file.go")} {
		if err := guardCheckoutMutationPath(ctx, path); err == nil {
			t.Fatalf("accepted symlink escape: %q", path)
		}
	}
	outsideFile := filepath.Join(other, "file.go")
	if err := os.WriteFile(outsideFile, []byte("package other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	insideLink := filepath.Join(root, "linked.go")
	if err := os.Symlink(outsideFile, insideLink); err != nil {
		t.Fatal(err)
	}
	if err := guardCheckoutMutationPath(ctx, insideLink); err == nil {
		t.Fatal("accepted final symlink reading another checkout's source")
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	aliasCtx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, alias)
	for _, path := range []string{filepath.Join(alias, "new.go"), filepath.Join(root, "new.go")} {
		if err := guardCheckoutMutationPath(aliasCtx, path); err != nil {
			t.Fatalf("root alias %q: %v", path, err)
		}
	}

	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := guardCheckoutMutationPath(ctx, filepath.Join(nested, "new", "file.go")); err == nil || !strings.Contains(err.Error(), "nested checkout") {
		t.Fatalf("nested checkout refusal = %v", err)
	}

	if err := guardCheckoutMutationPath(context.Background(), filepath.Join(other, "file.go")); err != nil {
		t.Fatalf("ordinary mutation was changed: %v", err)
	}
}

func TestCheckoutMutationPathLockRefusesOtherCheckout(t *testing.T) {
	root := t.TempDir()
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	release, err := acquireMutationPath(ctx, filepath.Join(t.TempDir(), "file.go"))
	if err == nil || release != nil {
		t.Fatalf("lock acquired outside selected checkout: release=%v err=%v", release != nil, err)
	}
	release, err = acquireMutationPath(ctx, filepath.Join(root, "file.go"))
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestCheckoutMutationOutsideFinalSymlinkPreserved(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file.go")
	before := []byte("package example\n")
	if err := os.WriteFile(target, before, 0o644); err != nil {
		t.Fatal(err)
	}
	outsideLink := filepath.Join(t.TempDir(), "link.go")
	if err := os.Symlink(target, outsideLink); err != nil {
		t.Fatal(err)
	}
	prepared := false
	lease := &fakeCheckoutMutationLifecycle{prepare: func(context.Context) error {
		prepared = true
		return nil
	}}
	ctx := withCheckoutMutation(context.Background(), lease, root)
	s := &Server{}
	record, err := s.commitFileMutation(ctx, "write_file", "", "test", "link.go", outsideLink, []byte("package changed\n"), 0o644)
	if !errors.Is(err, errMutationNotApplied) || prepared || record.diskStatus() != mutationDiskNotApplied {
		t.Fatalf("outside link was admitted: prepared=%v receipt=%+v err=%v", prepared, record.snapshot(), err)
	}
	link, err := os.Lstat(outsideLink)
	if err != nil || link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("outside symlink was replaced: stat=%v err=%v", link, err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != string(before) {
		t.Fatalf("selected file was changed: %q err=%v", actual, err)
	}
}

func TestCheckoutMutationGitSymlinkEntryRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file.go")
	if err := os.WriteFile(target, []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitLink := filepath.Join(root, ".git")
	if err := os.Symlink(target, gitLink); err != nil {
		t.Fatal(err)
	}
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	if err := guardCheckoutMutationPath(ctx, gitLink); err == nil || !strings.Contains(err.Error(), "Git metadata") {
		t.Fatalf("Git symlink entry accepted because target was ordinary source: %v", err)
	}
}

func TestCheckoutMutationCommitPreparesBeforeDisk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	before := []byte("package example\n")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	prepared := false
	lease := &fakeCheckoutMutationLifecycle{prepare: func(context.Context) error {
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != string(before) {
			t.Fatalf("disk changed before Prepare: %q, %v", actual, err)
		}
		prepared = true
		return nil
	}}
	ctx := withCheckoutMutation(context.Background(), lease, root)
	s := &Server{}
	record, err := s.commitFileMutation(ctx, "write_file", "", "test", "file.go", path, []byte("package changed\n"), 0o644)
	if err != nil || !prepared || record.diskStatus() != mutationDiskCommitted {
		t.Fatalf("commit prepared=%v record=%+v err=%v", prepared, record.snapshot(), err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != "package changed\n" {
		t.Fatalf("disk commit = %q, %v", actual, err)
	}
}

func TestCheckoutMutationPrepareFailureLeavesDiskUntouched(t *testing.T) {
	for _, cancelDuringPrepare := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare_error", true: "cancel_during_prepare"}[cancelDuringPrepare], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file.go")
			before := []byte("package example\n")
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lease := &fakeCheckoutMutationLifecycle{prepare: func(context.Context) error {
				if cancelDuringPrepare {
					cancel()
					return nil
				}
				return errors.New("route invalidation failed")
			}}
			ctx = withCheckoutMutation(ctx, lease, root)
			s := &Server{}
			record, err := s.commitFileMutation(ctx, "write_file", "", "test", "file.go", path, []byte("package changed\n"), 0o644)
			if !errors.Is(err, errMutationNotApplied) || record.diskStatus() != mutationDiskNotApplied {
				t.Fatalf("expected not-applied receipt: %+v, %v", record.snapshot(), err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(before) {
				t.Fatalf("disk changed on prepare failure: %q, %v", actual, err)
			}
		})
	}
}

func TestCheckoutMutationReindexUsesLease(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	refreshed := false
	lease := &fakeCheckoutMutationLifecycle{refresh: func(ctx context.Context) (indexer.CheckoutCycle, error) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("committed mutation inherited client cancellation: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > defaultMutationReindexWait {
			t.Fatalf("refresh has no bounded deadline: %v, %v", deadline, ok)
		}
		refreshed = true
		return indexer.CheckoutCycle{CommitGenerationID: 4, DirtyGenerationID: 9}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withCheckoutMutation(ctx, lease, root)
	cancel()
	// No watcher or canonical indexer is attached: success must come from the
	// selected lease, even for a caller that disconnected after disk commit.
	s := &Server{}
	outcome := s.mutationReindexState(ctx, path)
	if !refreshed || !outcome.Reindexed || outcome.Err != nil || outcome.AppliedGeneration != 9 {
		t.Fatalf("selected refresh = %+v, called=%v", outcome, refreshed)
	}
	resp := map[string]any{}
	s.attachMutationFreshness(resp, "file.go", path, outcome)
	if resp["graph_status"] != mutationGraphFresh {
		t.Fatalf("freshness = %v", resp)
	}
	if _, ok := resp["syntax_health"]; ok {
		t.Fatalf("canonical syntax health leaked into checkout response: %v", resp)
	}
}

func TestCheckoutMutationReindexNeverClaimsPendingCycleFresh(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cycle indexer.CheckoutCycle
		err   error
	}{
		{name: "refresh_error", err: errors.New("publish failed")},
		{name: "deferred", cycle: indexer.CheckoutCycle{Deferred: true, CommitGenerationID: 4, DirtyGenerationID: 5}},
		{name: "rescheduled", cycle: indexer.CheckoutCycle{Rescheduled: true, CommitGenerationID: 4, DirtyGenerationID: 5}},
		{name: "missing_generation", cycle: indexer.CheckoutCycle{CommitGenerationID: 4}},
		{name: "cycle_error", cycle: indexer.CheckoutCycle{Err: errors.New("build failed"), CommitGenerationID: 4, DirtyGenerationID: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			lease := &fakeCheckoutMutationLifecycle{refresh: func(context.Context) (indexer.CheckoutCycle, error) { return tc.cycle, tc.err }}
			ctx := withCheckoutMutation(context.Background(), lease, root)
			s := &Server{}
			outcome := s.mutationReindexState(ctx, filepath.Join(root, "file.go"))
			if outcome.Reindexed || outcome.Err == nil || outcome.AppliedGeneration != 0 {
				t.Fatalf("unpublished cycle claimed fresh: %+v", outcome)
			}
		})
	}
}

func BenchmarkCheckoutMutationPathConfinement(b *testing.B) {
	root := b.TempDir()
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	path := filepath.Join(root, "new", "file.go")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := guardCheckoutMutationPath(ctx, path); err != nil {
			b.Fatal(err)
		}
	}
}
