package gitstate

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/gitcmd"
)

// A detached HEAD is read from the worktree's HEAD file: a sample of a
// detached checkout spends no symbolic-ref call, and still reports no ref.
// A branch literally named "(detached)" still asks git.
func TestDirtySamplerDetachedHeadNeedsNoSymbolicRef(t *testing.T) {
	repo := initDirtySamplerRepo(t)
	seed, err := SampleHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	var symbolic int
	run := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "symbolic-ref" {
				symbolic++
			}
		}
		return gitcmd.Run(ctx, dir, args...)
	}
	sampler := newDirtySampler(repo, seed.CommitOID, seed.TreeOID, run)

	runDirtySamplerGit(t, repo, "checkout", "--detach", "HEAD")
	writeIn(t, repo, "tracked.go", "package tracked\n\nfunc Dirty() {}\n")
	snap, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("detached Sample: %v", err)
	}
	if snap.HeadRef != "" || snap.HeadCommit != seed.CommitOID || snap.HeadTree != seed.TreeOID {
		t.Fatalf("detached head = ref %q commit %q tree %q", snap.HeadRef, snap.HeadCommit, snap.HeadTree)
	}
	if symbolic != 0 {
		t.Fatalf("a detached sample ran symbolic-ref %d time(s); the HEAD file already says detached", symbolic)
	}

	runDirtySamplerGit(t, repo, "checkout", "-B", "(detached)")
	snap, err = sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("marker-branch Sample: %v", err)
	}
	if snap.HeadRef != "refs/heads/(detached)" {
		t.Fatalf("a branch named (detached) sampled as ref %q", snap.HeadRef)
	}
	if symbolic != 1 {
		t.Fatalf("a branch named (detached) ran symbolic-ref %d time(s), want 1", symbolic)
	}
}
