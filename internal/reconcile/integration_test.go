package reconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// volumeEvidenceUsable reports whether this platform's path evidence carries
// the volume identity a prunable confirmation is built on.
//
// It samples a directory that certainly exists and puts the result through the
// classifier's own predicate, so the answer comes from gitstate's per-platform
// path-identity seam rather than from a hardcoded list of operating systems. A
// platform that grows an implementation of that seam flips this to true on its
// own, and the strong expectations hanging off it start applying with no edit
// here.
func volumeEvidenceUsable(t *testing.T, dir string) bool {
	t.Helper()
	sample := gitstate.SamplePathEvidence(dir)
	if !sample.RootExists {
		t.Fatalf("volume evidence probe of %q found no such directory: %+v", dir, sample)
	}
	return SampledPathEvidence(sample).rootVolumeUsable()
}

// isolateGit pins the git environment for the whole test: no user or system
// config, a fixed identity, no credential prompts. A developer's ~/.gitconfig
// must not be able to change what these fixtures produce.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "reconcile test")
	t.Setenv("GIT_AUTHOR_EMAIL", "reconcile@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "reconcile test")
	t.Setenv("GIT_COMMITTER_EMAIL", "reconcile@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// runGit runs a git command inside dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// tempRoot returns a temp directory with every symlink resolved. On macOS
// t.TempDir() hands back a path under /var, which is a symlink to /private/var;
// git reports the resolved spelling, so fixtures live under the resolved root
// to keep path comparisons exact.
func tempRoot(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}

// TestReconcileFamilyAgainstRealGit drives the whole pass over a real
// repository with a real linked worktree, through the real gitstate readers.
// The fakes elsewhere can assert what the classifier does with a given
// observation; only this proves the observations git actually produces reach
// those branches.
func TestReconcileFamilyAgainstRealGit(t *testing.T) {
	ctx := context.Background()
	root := tempRoot(t)
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "feature")

	runGit(t, "", "init", "-q", "-b", "main", "--", repo)
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, repo, "add", "--", "seed.txt")
	runGit(t, repo, "commit", "-q", "-m", "seed")
	runGit(t, repo, "worktree", "add", "-q", "-b", "feature", "--", worktree)

	inv, err := gitstate.Inventory(ctx, repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Records) != 2 {
		t.Fatalf("fixture produced %d records, want 2", len(inv.Records))
	}
	var adminName string
	for _, record := range inv.Records {
		if !record.IsMain {
			adminName = record.AdminName
		}
	}
	if adminName == "" {
		t.Fatal("git did not report an administrative name for the linked worktree")
	}

	store, err := store_sqlite.Open(filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog := store.Catalog()

	const familyID = "real-fam"
	err = catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: inv.CommonDir,
		State:             "family_ready",
	})
	if err != nil {
		t.Fatalf("seed family: %v", err)
	}
	err = catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:       "graph-primary",
		RepoPrefix:    "real",
		FamilyID:      familyID,
		IsPrimaryBase: true,
		State:         "graph_ready",
	})
	if err != nil {
		t.Fatalf("seed primary graph: %v", err)
	}

	hooks := &recordingHooks{}
	// No overrides: real Inventory, real SamplePathEvidence, real SampleHEAD.
	rec, err := New(catalog, hooks, Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pass := func() FamilyReport {
		t.Helper()
		report, err := rec.ReconcileFamily(ctx, familyID, repo)
		if err != nil {
			t.Fatalf("ReconcileFamily: %v", err)
		}
		if !report.InventoryUsable {
			t.Fatal("the real inventory was rejected as unusable")
		}
		return report
	}
	find := func(report FamilyReport, name string) CheckoutReport {
		t.Helper()
		for _, entry := range report.Checkouts {
			if entry.AdminName == name {
				return entry
			}
		}
		t.Fatalf("report has no entry for %q: %+v", name, report.Checkouts)
		return CheckoutReport{}
	}

	// Both worktrees are present, so both get durable identities.
	report := pass()
	linked := find(report, adminName)
	main := find(report, gitstate.MainAdminName)
	if linked.Action != ActionIdentityAllocated || linked.Classification.Disposition != DispositionPresent {
		t.Fatalf("linked worktree = %+v", linked)
	}
	if !main.Main || main.Action != ActionIdentityAllocated {
		t.Fatalf("main worktree = %+v", main)
	}
	row, ok, err := catalog.GetCheckout(ctx, linked.CheckoutID)
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v %v", ok, err)
	}
	if row.HeadRef != "refs/heads/feature" || row.HeadCommit == "" || row.HeadTree == "" {
		t.Fatalf("real HEAD sample = %+v", row)
	}

	// Whether a deletion below can be proven at all is a platform property,
	// probed through the seam rather than assumed from the operating system.
	volumeEvidence := volumeEvidenceUsable(t, repo)
	storedEvidence, ok, err := catalog.GetCheckoutPathEvidence(ctx, linked.CheckoutID)
	if err != nil || !ok {
		t.Fatalf("GetCheckoutPathEvidence = %v %v", ok, err)
	}
	// The persisted sample is the side of the comparison the classifier reads
	// first, so it has to carry exactly as much as the platform can produce. A
	// platform that can sample a volume token but stores none would otherwise
	// quietly drop this test into the fail-closed arm below.
	if got := StoredPathEvidence(storedEvidence).rootVolumeUsable(); got != volumeEvidence {
		t.Fatalf("persisted root volume evidence usable = %v, want %v on this platform: %+v",
			got, volumeEvidence, storedEvidence)
	}

	// Deleting the directory leaves git listing a prunable worktree whose root
	// is gone from a volume that is still very much mounted.
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}
	pruned, err := gitstate.Inventory(ctx, repo)
	if err != nil {
		t.Fatalf("Inventory after removal: %v", err)
	}
	var listedPrunable bool
	for _, record := range pruned.Records {
		if record.AdminName == adminName {
			listedPrunable = record.Prunable
		}
	}
	if !listedPrunable {
		t.Skipf("this git does not mark a deleted worktree prunable; nothing to classify")
	}

	report = pass()
	linkedAfter := find(report, adminName)
	if volumeEvidence {
		// The ancestor still sits on the volume the root sat on, so the
		// absence is proven and the removal clock starts.
		if linkedAfter.Classification.Evidence != EvidencePrunableConfirmed {
			t.Fatalf("deleted directory classified %+v", linkedAfter.Classification)
		}
		if linkedAfter.Action != ActionRemovalGraceStarted {
			t.Fatalf("deleted directory action = %q", linkedAfter.Action)
		}
	} else {
		// Without volume identity a deleted root and an unmounted volume are
		// the same observation, so the classifier fails closed: inaccessible,
		// no removal evidence, and only the availability clock moves. That is
		// the contract such a platform actually has, so it is pinned here
		// rather than skipped.
		class := linkedAfter.Classification
		if class.Disposition != DispositionInaccessible || class.Evidence != EvidenceNone {
			t.Fatalf("deleted directory classified %+v, want a fail-closed inaccessible verdict", class)
		}
		if class.Code != graphview.CodeCheckoutInaccessible {
			t.Fatalf("deleted directory code = %q, want %q", class.Code, graphview.CodeCheckoutInaccessible)
		}
		if !strings.Contains(class.Detail, "no usable persisted root volume token") {
			t.Fatalf("deleted directory detail = %q, want the missing-volume-token branch", class.Detail)
		}
		if linkedAfter.Action != ActionAvailabilityGraceStarted {
			t.Fatalf("deleted directory action = %q, want the availability clock to start", linkedAfter.Action)
		}
	}
	if linkedAfter.CheckoutID != linked.CheckoutID {
		t.Fatalf("removal re-keyed the identity: %q, want %q", linkedAfter.CheckoutID, linked.CheckoutID)
	}
	if find(report, gitstate.MainAdminName).Classification.Disposition != DispositionPresent {
		t.Fatal("the main worktree was disturbed by the linked worktree's removal")
	}

	// Pruning removes git's administrative record, which is the authoritative
	// omission the classifier trusts most.
	runGit(t, repo, "worktree", "prune")
	report = pass()
	linkedPruned := find(report, adminName)
	if linkedPruned.Classification.Evidence != EvidenceAuthoritativeOmission {
		t.Fatalf("pruned worktree classified %+v", linkedPruned.Classification)
	}
	// Where the deletion was already proven the removal clock is running, so
	// nothing restarts it; the whole point of the second evidence class is
	// that it confirms, not resets. Where it could not be proven, git's
	// omission is the first removal evidence there is and starts the clock.
	wantPrunedAction := ActionRemovalHeld
	if !volumeEvidence {
		wantPrunedAction = ActionRemovalGraceStarted
	}
	if linkedPruned.Action != wantPrunedAction {
		t.Fatalf("pruned worktree action = %q, want %q", linkedPruned.Action, wantPrunedAction)
	}

	// Recreating the worktree under the same administrative name gives the
	// original identity back and clears the removal clock.
	runGit(t, repo, "worktree", "add", "-q", "--", worktree, "feature")
	report = pass()
	revived := find(report, adminName)
	if revived.Action != ActionRemovalCancelled || revived.State != store_sqlite.CheckoutStateReady {
		t.Fatalf("recreated worktree = %+v", revived)
	}
	if revived.CheckoutID != linked.CheckoutID || revived.Incarnation != linked.Incarnation {
		t.Fatalf("recreation re-keyed the identity: %+v", revived)
	}
	if n := hooks.countPrefix("purge:"); n != 0 {
		t.Fatalf("a recovered checkout had its layers purged %d times", n)
	}

	// A directory git knows nothing about cannot be reconciled as this family.
	other := filepath.Join(root, "other")
	runGit(t, "", "init", "-q", "-b", "main", "--", other)
	report, err = rec.ReconcileFamily(ctx, familyID, other)
	if err != nil {
		t.Fatalf("ReconcileFamily against a foreign repo: %v", err)
	}
	if report.InventoryUsable {
		t.Fatal("an inventory of a different repository was accepted")
	}
	for _, entry := range report.Checkouts {
		if entry.Classification.Disposition != DispositionInaccessible {
			t.Fatalf("%s classified %q against a foreign inventory", entry.AdminName, entry.Classification.Disposition)
		}
	}
}
