package gitstate

import (
	"context"
	"testing"
	"time"
)

// SampleSince shares a sample only when its git status began at or after the
// caller's instant, so a freshness check that passes the instant its request
// arrived never accepts a sample taken before it.
func TestSampleSinceSharesOnlyASampleTakenAfterTheInstant(t *testing.T) {
	root := t.TempDir()
	clean := dirtyCommandResult{out: porcelainBranch(testCommitA, "main")}
	commands := &scriptedDirtyCommands{results: []dirtyCommandResult{clean, clean, clean}}
	sampler := newDirtySampler(root, testCommitA, testTreeA, commands.run)
	ctx := context.Background()

	beforeFirst := time.Now()
	first, err := sampler.Sample(ctx)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got := len(commands.snapshotCalls()); got != 1 {
		t.Fatalf("a clean sample ran %d git commands, want 1", got)
	}

	shared, err := sampler.SampleSince(ctx, beforeFirst)
	if err != nil {
		t.Fatalf("SampleSince(before the first sample): %v", err)
	}
	if got := len(commands.snapshotCalls()); got != 1 {
		t.Fatalf("SampleSince re-sampled although a sample started after the instant: %d commands", got)
	}
	if shared.Fingerprint != first.Fingerprint || shared.HeadTree != first.HeadTree {
		t.Fatalf("SampleSince returned %+v, want the shared sample %+v", shared, first)
	}
	if got := sampler.SamplesTaken(); got != 1 {
		t.Fatalf("SamplesTaken = %d after one sample and one share, want 1", got)
	}

	afterFirst := time.Now()
	if _, err := sampler.SampleSince(ctx, afterFirst); err != nil {
		t.Fatalf("SampleSince(after the first sample): %v", err)
	}
	if got := len(commands.snapshotCalls()); got != 2 {
		t.Fatalf("SampleSince shared a sample that started before the instant: %d commands, want 2", got)
	}
	if got := sampler.SamplesTaken(); got != 2 {
		t.Fatalf("SamplesTaken = %d, want 2", got)
	}
	if started := sampler.LastSampleStarted(); started.Before(afterFirst) {
		t.Fatalf("the remembered sample started at %v, before the instant %v it was taken for", started, afterFirst)
	}

	// A zero instant is not "any sample": it takes a new one.
	if _, err := sampler.SampleSince(ctx, time.Time{}); err != nil {
		t.Fatalf("SampleSince(zero): %v", err)
	}
	if got := len(commands.snapshotCalls()); got != 3 {
		t.Fatalf("SampleSince(zero) shared a sample: %d commands, want 3", got)
	}
}

// A sample that fails a fence is never shared.
func TestSampleSinceNeverSharesAFailedSample(t *testing.T) {
	root := t.TempDir()
	commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
		{err: context.DeadlineExceeded},
		{out: porcelainBranch(testCommitA, "main")},
	}}
	sampler := newDirtySampler(root, testCommitA, testTreeA, commands.run)
	ctx := context.Background()
	since := time.Now()
	if _, err := sampler.Sample(ctx); err == nil {
		t.Fatal("the scripted status failure did not fail the sample")
	}
	if _, err := sampler.SampleSince(ctx, since); err != nil {
		t.Fatalf("SampleSince after a failed sample: %v", err)
	}
	if got := len(commands.snapshotCalls()); got != 2 {
		t.Fatalf("SampleSince shared a failed sample: %d commands, want 2", got)
	}
	if got := sampler.SamplesTaken(); got != 1 {
		t.Fatalf("SamplesTaken = %d, want 1 (the failed one does not count)", got)
	}
}

// TreeHoldsPaths answers literal HEAD membership, including for a path whose
// name would be a pathspec pattern.
func TestTreeHoldsPathsAnswersLiteralMembership(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "--initial-branch=main")
	writeIn(t, dir, "kept.go", "package x\n")
	writeIn(t, dir, "sub/deep.go", "package sub\n")
	writeIn(t, dir, "star*.go", "package x\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	tree := git(t, dir, "rev-parse", "HEAD^{tree}")

	held, err := TreeHoldsPaths(context.Background(), dir, tree,
		[]string{"kept.go", "sub/deep.go", "star*.go", "stars.go", "missing.go", "sub"})
	if err != nil {
		t.Fatalf("TreeHoldsPaths: %v", err)
	}
	want := map[string]bool{
		"kept.go": true, "sub/deep.go": true, "star*.go": true,
		"stars.go": false, "missing.go": false, "sub": false,
	}
	for p, w := range want {
		if held[p] != w {
			t.Errorf("TreeHoldsPaths[%q] = %v, want %v", p, held[p], w)
		}
	}
	if _, err := TreeHoldsPaths(context.Background(), dir, "not-a-tree", []string{"kept.go"}); err == nil {
		t.Error("TreeHoldsPaths accepted a non-oid tree")
	}
}
