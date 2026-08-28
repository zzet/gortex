package gitstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runDirtySamplerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Gortex Test",
		"GIT_AUTHOR_EMAIL=gortex@example.invalid",
		"GIT_COMMITTER_NAME=Gortex Test",
		"GIT_COMMITTER_EMAIL=gortex@example.invalid",
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initDirtySamplerRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runDirtySamplerGit(t, repo, "init", "--initial-branch=main", ".")
	writeIn(t, repo, "tracked.go", "package tracked\n")
	runDirtySamplerGit(t, repo, "add", "tracked.go")
	runDirtySamplerGit(t, repo,
		"-c", "user.name=Gortex Test",
		"-c", "user.email=gortex@example.invalid",
		"-c", "core.hooksPath=/dev/null",
		"commit", "--no-gpg-sign", "-m", "seed")
	return repo
}

func TestDirtySamplerRealGitMarkerBranchesAndDetachedHead(t *testing.T) {
	repo := initDirtySamplerRepo(t)
	seed, err := SampleHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	sampler, err := NewDirtySampler(repo, seed.CommitOID, seed.TreeOID)
	if err != nil {
		t.Fatalf("NewDirtySampler: %v", err)
	}

	for _, name := range []string{"(detached)", "(unknown)", "refs/topic"} {
		t.Run("attached "+name, func(t *testing.T) {
			runDirtySamplerGit(t, repo, "checkout", "-B", name)
			snap, err := sampler.Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample: %v", err)
			}
			wantRef := "refs/heads/" + name
			if snap.HeadRef != wantRef || snap.HeadCommit != seed.CommitOID || snap.HeadTree != seed.TreeOID {
				t.Fatalf("head = ref %q commit %q tree %q, want %q %q %q", snap.HeadRef, snap.HeadCommit, snap.HeadTree, wantRef, seed.CommitOID, seed.TreeOID)
			}
		})
	}

	runDirtySamplerGit(t, repo, "checkout", "--detach", "HEAD")
	detached, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatalf("detached Sample: %v", err)
	}
	if detached.HeadRef != "" || detached.HeadCommit != seed.CommitOID || detached.HeadTree != seed.TreeOID {
		t.Fatalf("detached head = %+v", detached)
	}
}

func TestDirtySamplerRealGitUnbornHead(t *testing.T) {
	repo := t.TempDir()
	runDirtySamplerGit(t, repo, "init", "--initial-branch=main", ".")
	writeIn(t, repo, "first.go", "package first\n")
	sampler, err := NewDirtySampler(repo, "", "")
	if err != nil {
		t.Fatalf("NewDirtySampler: %v", err)
	}
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
}

func TestDirtySamplerBranchHeadersStopBeforeRenameSources(t *testing.T) {
	cases := []struct {
		name   string
		record string
		source string
	}{
		{
			name:   "rename source spoofs head",
			record: "2 R. N... 100644 100644 100644 " + testBlob + " " + testBlob + " R100 renamed.go",
			source: "# branch.head spoofed",
		},
		{
			name:   "copy source spoofs oid",
			record: "2 C. N... 100644 100644 100644 " + testBlob + " " + testBlob + " C100 copied.go",
			source: "# branch.oid " + testCommitB,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commands := &scriptedDirtyCommands{results: []dirtyCommandResult{{
				out: porcelainBranch(testCommitA, "main", tc.record, tc.source),
			}}}
			sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
			snap, err := sampler.Sample(context.Background())
			if err != nil {
				t.Fatalf("Sample: %v", err)
			}
			if snap.HeadRef != "refs/heads/main" || snap.HeadCommit != testCommitA || snap.HeadTree != testTreeA {
				t.Fatalf("spoofed head = %+v", snap)
			}
			if got := len(commands.snapshotCalls()); got != 1 {
				t.Fatalf("sample ran %d commands, want status only", got)
			}
		})
	}
}

func TestDirtySamplerMarkerResolutionCancellationAndError(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch(testCommitA, "(detached)")},
			{out: []byte("refs/heads/(detached)\n")},
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

	t.Run("command error", func(t *testing.T) {
		commandErr := errors.New("symbolic-ref broke")
		commands := &scriptedDirtyCommands{results: []dirtyCommandResult{
			{out: porcelainBranch(testCommitA, "(unknown)")},
			{err: commandErr},
		}}
		sampler := newDirtySampler(t.TempDir(), testCommitA, testTreeA, commands.run)
		_, err := sampler.Sample(context.Background())
		if !errors.Is(err, ErrDirtyUnavailable) || !errors.Is(err, commandErr) {
			t.Fatalf("err = %v, want dirty unavailable + command error", err)
		}
	})
}
