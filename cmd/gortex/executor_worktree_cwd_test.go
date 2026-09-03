package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// fakeLinkedWorktree lays out on disk what `git worktree add` leaves behind:
// the main checkout with a real `.git` directory, and a linked worktree whose
// `.git` is a FILE pointing at a per-worktree gitdir that carries a
// `commondir` back to the shared one. That indirection is the only thing
// separating a linked worktree from a submodule, so the fixture builds it
// exactly rather than approximating it.
func fakeLinkedWorktree(t *testing.T, dir string) (mainRepo, worktree string) {
	t.Helper()
	mainRepo = filepath.Join(dir, "main")
	worktree = filepath.Join(dir, "wt", "feature")
	wtGitDir := filepath.Join(mainRepo, ".git", "worktrees", "feature")

	for _, d := range []string{filepath.Join(mainRepo, ".git"), wtGitDir, worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	return mainRepo, worktree
}

// fakeBareHubFamily lays out the family a bare hub owns: `git clone --bare
// hub.git` followed by `git worktree add`. The shared git directory is
// `hub.git`, NOT a directory named `.git`, and every working copy of the
// family is a linked worktree — the family has no main checkout at all, so
// the only working copy that can ever be tracked is a worktree.
func fakeBareHubFamily(t *testing.T, dir string) (hub, tracked, worktree string) {
	t.Helper()
	hub = filepath.Join(dir, "hub.git")
	tracked = filepath.Join(dir, "wt", "trunk")
	worktree = filepath.Join(dir, "wt", "feature")

	for name, root := range map[string]string{"trunk": tracked, "feature": worktree} {
		wtGitDir := filepath.Join(hub, "worktrees", name)
		for _, d := range []string{wtGitDir, root} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, ".git"),
			[]byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
			t.Fatalf("write %s/.git: %v", root, err)
		}
		// `../..` from hub.git/worktrees/<name> is hub.git itself.
		if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
			t.Fatalf("write commondir: %v", err)
		}
	}
	return hub, tracked, worktree
}

// TestLinkedWorktreeAt_RealGitLayouts runs the classifier against layouts git
// itself produced, so the hand-built fixtures above cannot drift into
// agreeing with a wrong answer about the shape they imitate.
//
// The bare-hub arm is the one that matters: its shared git directory is not
// named `.git`, which is exactly the case that left a worktree resolved as
// its own main checkout.
func TestLinkedWorktreeAt_RealGitLayouts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// Resolve the temp root once: git records the path it is handed, and a
	// symlinked temp dir would otherwise make two spellings of one directory.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval temp dir: %v", err)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}

	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	git(seed, "init", "-q")
	git(seed, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	git(seed, "add", "a.txt")
	git(seed, "commit", "-qm", "initial")

	t.Run("main checkout and its worktree", func(t *testing.T) {
		wt := filepath.Join(root, "plain-wt")
		git(seed, "worktree", "add", "-q", "-b", "plain", wt)

		if _, ok := linkedWorktreeAt(seed); ok {
			t.Fatal("a main checkout is not a linked worktree")
		}
		fam, ok := linkedWorktreeAt(wt)
		if !ok {
			t.Fatal("git worktree add produced a directory the classifier does not recognise")
		}
		if fam.mainRepo != seed {
			t.Fatalf("main checkout: got %q want %q", fam.mainRepo, seed)
		}
		if want := filepath.Join(seed, ".git"); fam.commonDir != want {
			t.Fatalf("common dir: got %q want %q", fam.commonDir, want)
		}
	})

	t.Run("bare hub has no main checkout", func(t *testing.T) {
		hub := filepath.Join(root, "hub.git")
		git(root, "clone", "-q", "--bare", seed, hub)
		trunk := filepath.Join(root, "wt", "trunk")
		feature := filepath.Join(root, "wt", "feature")
		git(hub, "worktree", "add", "-q", "-b", "trunk", trunk)
		git(hub, "worktree", "add", "-q", "-b", "feature", feature)

		fam, ok := linkedWorktreeAt(feature)
		if !ok {
			t.Fatal("a bare hub's worktree must classify as a linked worktree")
		}
		if fam.mainRepo != "" {
			t.Fatalf("a bare hub owns no main checkout, got %q", fam.mainRepo)
		}
		if fam.commonDir != hub {
			t.Fatalf("common dir: got %q want %q", fam.commonDir, hub)
		}

		// The only working copy such a family can offer is a sibling
		// worktree, so that is what the remedy has to be able to name.
		st := daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{{Path: trunk}}}
		if got := familyRepoIn(st, fam); got != trunk {
			t.Fatalf("tracked working copy of the family: got %q want %q", got, trunk)
		}
		if got := familyRepoIn(daemon.StatusResponse{}, fam); got != "" {
			t.Fatalf("nothing tracked must resolve to no repository, got %q", got)
		}
	})
}

// TestRequireDaemonTool_BareHubWorktreeNamesTheTrackedSibling covers the
// family whose hub is bare. No working copy of it is the "main checkout", so
// the remedy has to be derived from what the daemon actually tracks — a
// sibling worktree — and never from the cwd itself.
//
// Resolving a main checkout by stripping a trailing `/.git` leaves a bare
// hub's worktree as its own main, which turned the remedy into the one
// suggestion that must never appear: track this worktree as a repository, in
// a message that also called it a linked worktree of itself.
func TestRequireDaemonTool_BareHubWorktreeNamesTheTrackedSibling(t *testing.T) {
	dir := t.TempDir()
	_, tracked, worktree := fakeBareHubFamily(t, dir)

	startStubDaemon(t, []string{tracked})

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an unbound worktree must fail rather than answer from the wrong working copy")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track") {
		t.Fatalf("the daemon already tracks the family — the remedy must not be a track: %q", msg)
	}
	if strings.Contains(msg, "worktree of "+worktree) {
		t.Fatalf("the worktree must never be named as its own main checkout: %q", msg)
	}
	if !strings.Contains(msg, "gortex repos reconcile "+tracked) {
		t.Fatalf("the error must point at the tracked working copy of the family: %q", msg)
	}
}

// TestRequireDaemonTool_UntrackedBareHubWorktreeNeverTracksItself is the same
// layout with nothing of the family tracked. There is still no main checkout
// to name, so the remedy names the family's shared git directory and asks for
// a working copy — the one thing it must not do is offer the cwd.
func TestRequireDaemonTool_UntrackedBareHubWorktreeNeverTracksItself(t *testing.T) {
	dir := t.TempDir()
	hub, _, worktree := fakeBareHubFamily(t, dir)

	startStubDaemon(t, []string{filepath.Join(t.TempDir(), "elsewhere")})

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an untracked worktree must fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track "+worktree) {
		t.Fatalf("the error must not tell the user to track the worktree: %q", msg)
	}
	if strings.Contains(msg, "worktree of "+worktree) {
		t.Fatalf("the worktree must never be named as its own main checkout: %q", msg)
	}
	if !strings.Contains(msg, hub) {
		t.Fatalf("the error must name the family the worktree belongs to: %q", msg)
	}
}

// TestResolveExecutor_RegisteredCheckoutCWDReachesTheDaemon pins the fix: a
// working directory that lies inside no tracked repo root but inside a
// registered checkout of a tracked family passes the pre-flight, and the call
// carries that cwd so the daemon-side binding can serve the worktree's view.
//
// Tracked-root membership alone answers "no" here, which is what refused every
// CLI query run from a linked worktree — with a remedy (`gortex track
// <worktree>`) that would have indexed the worktree a second time.
func TestResolveExecutor_RegisteredCheckoutCWDReachesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	tracked := filepath.Join(dir, "main")
	worktree := filepath.Join(dir, "wt", "feature")

	stub := startStubDaemon(t, []string{tracked})
	stub.serveCheckouts(worktree)

	exec, err := resolveExecutor(worktree)
	if err != nil {
		t.Fatalf("a registered checkout cwd must pass the pre-flight: %v", err)
	}
	defer exec.Close()

	if _, ok := exec.(*daemonExecutor); !ok {
		t.Fatalf("the daemon-first path must return a *daemonExecutor, got %T", exec)
	}
	if hs := stub.seenMCPHandshake(); hs.CWD != worktree {
		t.Fatalf("the call must carry the worktree cwd, daemon saw %q want %q", hs.CWD, worktree)
	}
	probes := stub.seenCoverageProbes()
	if len(probes) == 0 || probes[0] != worktree {
		t.Fatalf("the pre-flight must ask the daemon about the cwd, probes=%v", probes)
	}
}

// TestRequireDaemonTool_FamilyWorktreeNeverSuggestsTrackingItself covers the
// worktree the daemon tracks the family of but has not bound to a checkout
// view. The call still fails — there is nothing to answer with — but the
// remedy must name the main checkout, never the worktree: tracking a linked
// worktree as its own repository is what the family model exists to avoid.
func TestRequireDaemonTool_FamilyWorktreeNeverSuggestsTrackingItself(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	// The family is tracked; the daemon has registered no checkout for the
	// worktree, so file_coverage names none.
	startStubDaemon(t, []string{mainRepo})

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an unbound worktree must fail rather than answer from the wrong working copy")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track "+worktree) {
		t.Fatalf("the error must not tell the user to track the worktree: %q", msg)
	}
	if !strings.Contains(msg, mainRepo) {
		t.Fatalf("the error must name the main checkout the worktree belongs to: %q", msg)
	}
}

// TestRequireDaemonTool_WorktreeRefusedByTheDaemonNamesTheReconcile covers the
// skew case: the pre-flight passes because a checkout is registered, but the
// daemon answering the call still refuses it. The remedy has to stay the
// family's — reconcile the main checkout — because the daemon demonstrably
// knows that repository already.
func TestRequireDaemonTool_WorktreeRefusedByTheDaemonNamesTheReconcile(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	stub := startStubDaemon(t, []string{mainRepo})
	stub.serveCheckouts(worktree)
	stub.mcpError = &stubRPCError{Code: -32000, Message: "repository not tracked", ErrorCode: "repo_not_tracked"}

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("a refused call must surface an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track") {
		t.Fatalf("the daemon already tracks the family — the remedy must not be a track: %q", msg)
	}
	if !strings.Contains(msg, "gortex repos reconcile "+mainRepo) {
		t.Fatalf("the error must point at the family's reconcile: %q", msg)
	}
}

// TestRequireDaemonTool_UntrackedCWDKeepsTheTrackSuggestion pins the message a
// genuinely uncovered directory still gets. `gortex track <path>` is the right
// remedy there, and widening the worktree arm must not blunt it.
func TestRequireDaemonTool_UntrackedCWDKeepsTheTrackSuggestion(t *testing.T) {
	stranger := t.TempDir() // no .git anywhere above it, tracked by nobody
	startStubDaemon(t, []string{filepath.Join(t.TempDir(), "elsewhere")})

	_, err := requireDaemonTool(stranger, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an untracked cwd must fail")
	}
	want := fmt.Sprintf("the gortex daemon does not track %s — add it with `gortex track %s`", stranger, stranger)
	if err.Error() != want {
		t.Fatalf("untracked message changed:\n got %q\nwant %q", err.Error(), want)
	}
}

// TestCheckoutVerbs_UnboundWorktreeCWDRelaysThroughTheFamily pins that the
// remedy an unbound worktree is given can be run from the worktree it is
// given in.
//
// The checkout verbs relay through requireDaemonTool with --index defaulting
// to ".", so routing them on the cwd made `gortex repos reconcile` fail with
// the very error that recommended it — and took `explain-view` and `families`,
// the verbs that diagnose exactly this state, down with it.
func TestCheckoutVerbs_UnboundWorktreeCWDRelaysThroughTheFamily(t *testing.T) {
	t.Run("main checkout tracked", func(t *testing.T) {
		dir := t.TempDir()
		mainRepo, worktree := fakeLinkedWorktree(t, dir)
		stub := startStubDaemon(t, []string{mainRepo})

		if _, err := checkoutsDaemonTool(worktree, "reconcile_checkouts", map[string]any{}); err != nil {
			t.Fatalf("the recommended remedy must run from the worktree it is recommended in: %v", err)
		}
		if hs := stub.seenMCPHandshake(); hs.CWD != mainRepo {
			t.Fatalf("the verb must relay through the family's tracked repo, daemon saw %q want %q", hs.CWD, mainRepo)
		}
		if tool, _ := stub.seenTool(); tool != "reconcile_checkouts" {
			t.Fatalf("relayed the wrong tool: %q", tool)
		}
	})

	t.Run("bare hub tracked through a sibling worktree", func(t *testing.T) {
		dir := t.TempDir()
		_, tracked, worktree := fakeBareHubFamily(t, dir)
		stub := startStubDaemon(t, []string{tracked})

		if _, err := checkoutsDaemonTool(worktree, "explain_view", map[string]any{"path": worktree}); err != nil {
			t.Fatalf("the diagnostic verb for this state must run in it: %v", err)
		}
		if hs := stub.seenMCPHandshake(); hs.CWD != tracked {
			t.Fatalf("the verb must relay through the family's tracked worktree, daemon saw %q want %q", hs.CWD, tracked)
		}
	})

	t.Run("a bound worktree keeps its own cwd", func(t *testing.T) {
		dir := t.TempDir()
		mainRepo, worktree := fakeLinkedWorktree(t, dir)
		stub := startStubDaemon(t, []string{mainRepo})
		stub.serveCheckouts(worktree)

		if _, err := checkoutsDaemonTool(worktree, "explain_view", map[string]any{"path": worktree}); err != nil {
			t.Fatalf("a bound worktree serves the verb itself: %v", err)
		}
		if hs := stub.seenMCPHandshake(); hs.CWD != worktree {
			t.Fatalf("a worktree the daemon serves must not be relayed away from its own view: got %q want %q",
				hs.CWD, worktree)
		}
	})

	t.Run("a tracked cwd is relayed unchanged", func(t *testing.T) {
		repo := t.TempDir()
		stub := startStubDaemon(t, []string{repo})

		if _, err := checkoutsDaemonTool(repo, "list_checkouts", map[string]any{}); err != nil {
			t.Fatalf("a tracked cwd must relay as itself: %v", err)
		}
		if hs := stub.seenMCPHandshake(); hs.CWD != repo {
			t.Fatalf("the relay rewrote a tracked cwd: got %q want %q", hs.CWD, repo)
		}
	})
}

// TestResolveExecutor_BusyCoverageProbeKeepsAskingTheDaemon pins the
// fail-open the routing probe documents for its status half onto its coverage
// half. A daemon too busy to answer "which view serves this path" has not
// said the worktree is unbound — it has said nothing, and reporting the
// reconcile remedy on that silence is the same lie the status probe refuses
// to tell.
func TestResolveExecutor_BusyCoverageProbeKeepsAskingTheDaemon(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	stub := startStubDaemon(t, []string{mainRepo})
	stub.serveCoverageBusy()

	exec, err := resolveExecutor(worktree)
	if err != nil {
		t.Fatalf("an indeterminate coverage probe must fall through to the daemon: %v", err)
	}
	defer exec.Close()
	if hs := stub.seenMCPHandshake(); hs.CWD != worktree {
		t.Fatalf("the call must carry the worktree cwd, daemon saw %q want %q", hs.CWD, worktree)
	}
}
