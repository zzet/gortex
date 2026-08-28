package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	testCommitA = "1111111111111111111111111111111111111111"
	testCommitB = "2222222222222222222222222222222222222222"
	testTreeA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTreeB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testBlob    = "cccccccccccccccccccccccccccccccccccccccc"
)

type dirtyCommandCall struct {
	dir  string
	args []string
}

type dirtyCommandResult struct {
	out []byte
	err error
}

type scriptedDirtyCommands struct {
	mu      sync.Mutex
	results []dirtyCommandResult
	calls   []dirtyCommandCall
	before  func(int)
}

func (s *scriptedDirtyCommands) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	s.mu.Lock()
	call := len(s.calls)
	s.calls = append(s.calls, dirtyCommandCall{dir: dir, args: append([]string(nil), args...)})
	before := s.before
	if call >= len(s.results) {
		s.mu.Unlock()
		return nil, fmt.Errorf("unexpected git command %d: %v", call+1, args)
	}
	result := s.results[call]
	s.mu.Unlock()
	if before != nil {
		before(call)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result.out, result.err
}

func (s *scriptedDirtyCommands) snapshotCalls() []dirtyCommandCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dirtyCommandCall(nil), s.calls...)
}

func porcelainBranch(oid, head string, records ...string) []byte {
	all := append([]string{"# branch.oid " + oid, "# branch.head " + head}, records...)
	return []byte(strings.Join(all, "\x00") + "\x00")
}

func TestDirtySamplerAttachedDerivesHeadAndDirtyFromStatus(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "tracked.go", "changed\n")
	writeIn(t, root, "loose.go", "new\n")
	commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
		{out: porcelainBranch(testCommitA, "main",
			"1 .M N... 100644 100644 100644 "+testBlob+" "+testBlob+" tracked.go",
			"? loose.go")},
		{out: []byte(testTreeA + "\n")},
	}}
	sampler := newDirtySampler(root, "", "", commands.run)

	snap, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if snap.HeadRef != "refs/heads/main" || snap.HeadCommit != testCommitA || snap.HeadTree != testTreeA {
		t.Fatalf("head = %+v", snap)
	}
	if got := dirtyEntryFor(t, snap, "tracked.go"); got.Kind != DirtyModified || got.Staged || !got.Unstaged {
		t.Fatalf("tracked entry = %+v", got)
	}
	if got := dirtyEntryFor(t, snap, "loose.go"); got.Kind != DirtyUntracked || got.Staged || !got.Unstaged {
		t.Fatalf("untracked entry = %+v", got)
	}
	calls := commands.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("commands = %d, want status + tree", len(calls))
	}
	wantStatus := []string{"--no-optional-locks", "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--renames"}
	if !reflect.DeepEqual(calls[0].args, wantStatus) {
		t.Fatalf("status args = %q, want %q", calls[0].args, wantStatus)
	}
	wantTree := []string{"rev-parse", "--verify", "-q", testCommitA + "^{tree}"}
	if !reflect.DeepEqual(calls[1].args, wantTree) {
		t.Fatalf("tree args = %q, want %q", calls[1].args, wantTree)
	}
}

func TestDirtySamplerSameCommitDifferentBranchReusesTree(t *testing.T) {
	commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
		{out: porcelainBranch(testCommitA, "main")},
		{out: []byte(testTreeA + "\n")},
		{out: porcelainBranch(testCommitA, "topic/same-tree")},
	}}
	sampler := newDirtySampler(t.TempDir(), "", "", commands.run)

	first, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	second, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if first.HeadTree != testTreeA || second.HeadTree != testTreeA {
		t.Fatalf("trees = %q / %q, want cached %q", first.HeadTree, second.HeadTree, testTreeA)
	}
	if second.HeadRef != "refs/heads/topic/same-tree" {
		t.Fatalf("second ref = %q", second.HeadRef)
	}
	calls := commands.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("two samples ran %d commands, want status+tree then status", len(calls))
	}
	if len(calls[2].args) < 2 || calls[2].args[1] != "status" {
		t.Fatalf("unchanged second sample ran %q, want only status", calls[2].args)
	}
}

func TestDirtySamplerDetachedAndUnbornHeads(t *testing.T) {
	t.Run("detached", func(t *testing.T) {
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch(testCommitA, "(detached)")},
			{out: []byte(testTreeA + "\n")},
		}}
		sampler := newDirtySampler(t.TempDir(), "", "", commands.run)
		snap, err := sampler.Sample(context.Background())
		if err != nil {
			t.Fatalf("Sample: %v", err)
		}
		if snap.HeadRef != "" || snap.HeadCommit != testCommitA || snap.HeadTree != testTreeA {
			t.Fatalf("detached head = %+v", snap)
		}
	})

	t.Run("unborn", func(t *testing.T) {
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch("(initial)", "main", "? first.go")},
		}}
		sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
		snap, err := sampler.Sample(context.Background())
		if err != nil {
			t.Fatalf("Sample: %v", err)
		}
		if snap.HeadRef != "refs/heads/main" || snap.HeadCommit != "" || snap.HeadTree != "" {
			t.Fatalf("unborn head = %+v", snap)
		}
		if got := dirtyEntryFor(t, snap, "first.go"); got.Kind != DirtyUntracked {
			t.Fatalf("unborn entry = %+v", got)
		}
		if got := len(commands.snapshotCalls()); got != 1 {
			t.Fatalf("unborn sample ran %d commands, want status only", got)
		}
	})
}

func TestDirtySamplerResolvesExactCommitOnlyAfterOIDChange(t *testing.T) {
	commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
		{out: porcelainBranch(testCommitA, "main")},
		{out: porcelainBranch(testCommitB, "main")},
		{out: []byte(testTreeB + "\n")},
		{out: porcelainBranch(testCommitB, "other")},
	}}
	sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)

	unchanged, err := sampler.Sample(context.Background())
	if err != nil || unchanged.HeadTree != testTreeA {
		t.Fatalf("unchanged = %+v, %v", unchanged, err)
	}
	changed, err := sampler.Sample(context.Background())
	if err != nil || changed.HeadCommit != testCommitB || changed.HeadTree != testTreeB {
		t.Fatalf("changed = %+v, %v", changed, err)
	}
	again, err := sampler.Sample(context.Background())
	if err != nil || again.HeadRef != "refs/heads/other" || again.HeadTree != testTreeB {
		t.Fatalf("again = %+v, %v", again, err)
	}

	calls := commands.snapshotCalls()
	if len(calls) != 4 {
		t.Fatalf("three samples ran %d commands, want status, status+tree, status", len(calls))
	}
	want := []string{"rev-parse", "--verify", "-q", testCommitB + "^{tree}"}
	if !reflect.DeepEqual(calls[2].args, want) {
		t.Fatalf("changed tree command = %q, want exact oid %q", calls[2].args, want)
	}
}

func TestDirtySamplerRootAndPublicSubdirectoryCompatibility(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	nested := filepath.Join(repo, "nested", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeIn(t, repo, "nested/deeper/new.go", "package deeper\n")

	rootSnap := sampleDirtyOK(t, repo)
	nestedSnap := sampleDirtyOK(t, nested)
	if !reflect.DeepEqual(rootSnap, nestedSnap) {
		t.Fatalf("subdirectory sample differs from root:\nroot=%+v\nsub=%+v", rootSnap, nestedSnap)
	}

	head, err := SampleHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	sampler, err := NewDirtySampler(repo, head.CommitOID, head.TreeOID)
	if err != nil {
		t.Fatalf("NewDirtySampler: %v", err)
	}
	direct, err := sampler.Sample(context.Background())
	if err != nil || !reflect.DeepEqual(rootSnap, direct) {
		t.Fatalf("root-known sample = %+v, %v; public = %+v", direct, err, rootSnap)
	}
}

func TestDirtySamplerCancellationAndErrors(t *testing.T) {
	t.Run("status error", func(t *testing.T) {
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{{err: errors.New("status broke")}}}
		sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
		snap, err := sampler.Sample(context.Background())
		if !errors.Is(err, ErrDirtyUnavailable) || !strings.Contains(err.Error(), "status broke") {
			t.Fatalf("err = %v", err)
		}
		if !reflect.DeepEqual(snap, DirtySnapshot{}) {
			t.Fatalf("error returned non-zero snapshot: %+v", snap)
		}
	})

	t.Run("missing headers", func(t *testing.T) {
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{{out: []byte("? loose.go\x00")}}}
		sampler := newDirtySampler(t.TempDir(), "", "", commands.run)
		if _, err := sampler.Sample(context.Background()); !errors.Is(err, ErrDirtyUnavailable) {
			t.Fatalf("err = %v, want ErrDirtyUnavailable", err)
		}
	})

	t.Run("canceled tree resolution", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch(testCommitB, "main")},
			{out: []byte(testTreeB)},
		}}
		commands.before = func(call int) {
			if call == 1 {
				cancel()
			}
		}
		sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
		if _, err := sampler.Sample(ctx); !errors.Is(err, ErrDirtyUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want dirty unavailable + canceled", err)
		}
	})

	t.Run("tree error retries", func(t *testing.T) {
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch(testCommitB, "main")},
			{err: errors.New("object temporarily unavailable")},
			{out: porcelainBranch(testCommitB, "main")},
			{out: []byte(testTreeB + "\n")},
		}}
		sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
		first, err := sampler.Sample(context.Background())
		if err != nil || first.HeadTree != "" {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := sampler.Sample(context.Background())
		if err != nil || second.HeadTree != testTreeB {
			t.Fatalf("retry = %+v, %v", second, err)
		}
		if got := len(commands.snapshotCalls()); got != 4 {
			t.Fatalf("resolution failure was cached: %d commands", got)
		}
	})
}
