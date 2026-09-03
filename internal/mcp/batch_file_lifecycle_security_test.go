package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graphview"
)

func TestAtomicBatchLifecycleOutsideRootRefused(t *testing.T) {
	repoRoot := t.TempDir()
	outsideRoot := t.TempDir()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
	destination := filepath.Join(outsideRoot, "destination.txt")

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
		atomicFileMove(source, destination, ""),
	}, "file-lifecycle-outside-root")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "outside") {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := readAtomicBatchFixture(t, source); got != "source\n" {
		t.Fatalf("source = %q", got)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination was created: %v", err)
	}
}

func TestAtomicBatchLifecycleInvalidDigestAndOverlapRefused(t *testing.T) {
	t.Run("invalid digest", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		path := writeAtomicBatchFixture(t, t.TempDir(), "source.txt", "source\n")
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileDelete(path, "not-a-sha256"),
		}, "file-lifecycle-invalid-digest")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, path); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})

	t.Run("overlapping path ownership", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		path := writeAtomicBatchFixture(t, dir, "source.txt", "source\n")
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(path, "source", "edited"),
			atomicFileDelete(path, ""),
		}, "file-lifecycle-overlap")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "overlaps") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, path); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})
}

func TestPrepareBatchJournalRequiresExplicitExistenceState(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	path := filepath.Join(t.TempDir(), "empty.txt")
	receipt := batchTransactionReceipt{TransactionID: "unset-existence-state"}
	err := s.prepareBatchJournal(&receipt, map[string]*batchFileBuffer{
		path: {absPath: path, relPath: "empty.txt"},
	}, []string{path})
	if err == nil || !strings.Contains(err.Error(), "existence state is unset") {
		t.Fatalf("prepareBatchJournal error = %v", err)
	}
}

func TestAtomicBatchLifecycleDestinationGuards(t *testing.T) {
	t.Run("symlink to the checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// A symlink whose target is the checkout root is still a symlink
		// component, and a destination behind it still redirects the write.
		// Selecting it as the root would hide it: only components strictly
		// below the root are inspected.
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		if err := os.Symlink(repoRoot, filepath.Join(repoRoot, "self")); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(repoRoot, "self", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-self-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("symlink to another checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Two checkouts are in play and the symlink inside the routed view
		// points at the other one, so its target is a candidate root. It is
		// still a component of the destination and still has to be refused.
		repoRoot := t.TempDir()
		viewRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, viewRoot, "source.txt", "source\n")
		if err := os.Symlink(repoRoot, filepath.Join(viewRoot, "other")); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(viewRoot, "other", "destination.txt")

		ctx := withRequestView(context.Background(), &requestView{
			viewRoot:     viewRoot,
			materialized: &graphview.RepoView{ID: graphview.RepoViewID{RepoPrefix: "repo"}},
		})
		receipt, err := s.runBatchTransaction(ctx, []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-cross-checkout-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("destination symlink", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		target := writeAtomicBatchFixture(t, repoRoot, "target.txt", "target\n")
		destination := filepath.Join(repoRoot, "destination.txt")
		if err := os.Symlink(target, destination); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-destination-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if got := readAtomicBatchFixture(t, target); got != "target\n" {
			t.Fatalf("target = %q", got)
		}
	})

	t.Run("symlinked destination parent", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		realParent := filepath.Join(repoRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(repoRoot, "linked-parent")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(linkedParent, "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-symlinked-parent")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("routed view symlinked destination parent", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		repoRoot := t.TempDir()
		viewRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, viewRoot, "source.txt", "source\n")
		realParent := filepath.Join(viewRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(viewRoot, "linked-parent")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(linkedParent, "destination.txt")

		// A request routed to a worktree view resolves its paths under the
		// view root, which is a repository root the guard must recognise.
		ctx := withRequestView(context.Background(), &requestView{
			viewRoot:     viewRoot,
			materialized: &graphview.RepoView{ID: graphview.RepoViewID{RepoPrefix: "repo"}},
		})
		receipt, err := s.runBatchTransaction(ctx, []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-view-symlinked-parent")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("physically spelled destination under a symlinked checkout", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// The checkout is known by its symlinked spelling and the request names
		// the physical one. Both spell the same directory, so the destination is
		// inside the checkout and the guard owes it the same answer.
		base := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		realRoot := filepath.Join(base, "real")
		if err := os.Mkdir(realRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		linkedRoot := filepath.Join(base, "link")
		if err := os.Symlink(realRoot, linkedRoot); err != nil {
			t.Fatal(err)
		}
		realParent := filepath.Join(realRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(realRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, linkedRoot)
		source := writeAtomicBatchFixture(t, realRoot, "source.txt", "source\n")
		destination := filepath.Join(realRoot, "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-physical-checkout-spelling")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("checkout registered by its resolved spelling", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// The checkout is registered fully resolved while the request keeps the
		// spelling the temp dir handed out. On macOS the two differ at the very
		// first component (/var -> /private/var), so the request resolves none
		// of its own prefix and only an ancestor-by-ancestor match finds the
		// checkout. On Linux the spellings coincide and this degenerates into
		// the plain symlinked-parent case above.
		base := t.TempDir()
		repoRoot := filepath.Join(base, "repo")
		if err := os.Mkdir(repoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		realParent := filepath.Join(repoRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(repoRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, resolvedRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(repoRoot, "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-resolved-checkout-spelling")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("partially resolved destination spelling", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Same arrangement as the physical-spelling case, minus the canonical
		// base: the checkout is known as base/link and the request names
		// base/real, so the request resolves the checkout link but not the
		// temp-dir prefix above it. Neither spelling of the checkout is a
		// prefix of that path, and only its ancestors identify it.
		base := t.TempDir()
		realRoot := filepath.Join(base, "real")
		if err := os.Mkdir(realRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		linkedRoot := filepath.Join(base, "link")
		if err := os.Symlink(realRoot, linkedRoot); err != nil {
			t.Fatal(err)
		}
		realParent := filepath.Join(realRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(realRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, linkedRoot)
		source := writeAtomicBatchFixture(t, realRoot, "source.txt", "source\n")
		destination := filepath.Join(realRoot, "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-partially-resolved-spelling")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("unregistered alias spelling of the checkout", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// The checkout is registered as base/real and nobody registered
		// base/alias, which is a symlink to it. A destination spelled through
		// the alias really lives inside the checkout, so path resolution admits
		// it, but neither registered spelling is a lexical prefix of it and
		// every ancestor between it and the base is a symlink. The alias is the
		// only spelling that identifies the checkout at all, and the components
		// below it are exactly what the guard has to inspect.
		base := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		realRoot := filepath.Join(base, "real")
		if err := os.Mkdir(realRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realRoot, filepath.Join(base, "alias")); err != nil {
			t.Fatal(err)
		}
		realParent := filepath.Join(realRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(realRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, realRoot)
		source := writeAtomicBatchFixture(t, realRoot, "source.txt", "source\n")
		destination := filepath.Join(base, "alias", "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-alias-checkout-spelling")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("alias directory outside the checkout's own base", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Same arrangement, except the unregistered alias lives in a directory
		// of its own: nothing above the destination is shared with the checkout,
		// so the climb has no non-symlink ancestor to land on at all.
		repoRoot := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
			repoRoot = resolved
		}
		outside := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(outside); err == nil {
			outside = resolved
		}
		if err := os.Symlink(repoRoot, filepath.Join(outside, "shortcut")); err != nil {
			t.Fatal(err)
		}
		realParent := filepath.Join(repoRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(repoRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(outside, "shortcut", "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-outside-alias-spelling")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("unregistered alias with a symlink to the checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Two symlink ancestors both resolve into the checkout: the alias
		// nobody registered, and `self` inside the checkout pointing back at
		// its root. If the climb settled for the deepest one, `self` would be
		// the root and never inspected, and the same request spelled through
		// the registered root — refused by "symlink to the checkout root" —
		// would commit through the alias. The outermost symlink has to win so
		// everything below the alias stays in the inspected range.
		base := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		realRoot := filepath.Join(base, "real")
		if err := os.Mkdir(realRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realRoot, filepath.Join(base, "alias")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realRoot, filepath.Join(realRoot, "self")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, realRoot)
		source := writeAtomicBatchFixture(t, realRoot, "source.txt", "source\n")
		destination := filepath.Join(base, "alias", "self", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-alias-self-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realRoot, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("symlinked base with a symlink to the checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Portable pin for the climb's "remembered, not taken" rule. The
		// checkout is base/real/repo and the request comes through base/link,
		// a symlink to base/real, so no registered spelling is a lexical
		// prefix of it on any platform (the sibling test above relies on a
		// symlinked temp prefix that only macOS provides). Climbing from the
		// destination, `self` is a symlink that resolves into the checkout and
		// must be remembered rather than taken, because base/link/repo one
		// level up is a real directory that also resolves into it; taking
		// `self` would put the redirecting link above the inspected range.
		base := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		repoRoot := filepath.Join(base, "real", "repo")
		if err := os.MkdirAll(repoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(repoRoot, filepath.Join(repoRoot, "self")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(base, "link", "repo", "self", "destination.txt")

		if got, _ := s.batchLifecycleRoot(context.Background(), destination); got != filepath.Join(base, "link", "repo") {
			want := filepath.Join(base, "link", "repo")
			t.Fatalf("batchLifecycleRoot = %q, want %q", got, want)
		}
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-symlinked-base-self-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("alias into a checkout subdirectory", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// The alias resolves into the checkout but not onto its root, so a
		// climb that only recognises exact root spellings finds nothing and
		// the guard has no root to walk from. A symlink whose target lies
		// anywhere inside a checkout is an alias into it, and the components
		// below it are what must be inspected.
		base := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		repoRoot := filepath.Join(base, "repo")
		sub := filepath.Join(repoRoot, "sub")
		realParent := filepath.Join(sub, "real-parent")
		if err := os.MkdirAll(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(sub, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(sub, filepath.Join(outside, "shortcut")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(outside, "shortcut", "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-alias-into-subdirectory")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("case-variant spelling of the checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// On a case-insensitive filesystem the upper-cased root names the same
		// directory, and path resolution admits the destination because the
		// symlinked parent resolves to a real path inside the checkout. The
		// spelling matches no candidate lexically, so the root has to be
		// recognised by identity.
		repoRoot := t.TempDir()
		if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
			repoRoot = resolved
		}
		upper := strings.ToUpper(repoRoot)
		if upper == repoRoot {
			t.Skip("temp root has no letters to change case on")
		}
		if _, err := os.Lstat(upper); err != nil {
			t.Skip("filesystem is case-sensitive; the upper-cased root is a different path")
		}
		realParent := filepath.Join(repoRoot, "real-parent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, filepath.Join(repoRoot, "linked-parent")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(upper, "linked-parent", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-case-variant-root")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if _, err := os.Stat(filepath.Join(realParent, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("no matching checkout fails closed", func(t *testing.T) {
		// Path resolution normally refuses a path outside every root before
		// the guard sees it, so the guard is exercised directly: with
		// checkouts known and none containing the spelling it must refuse,
		// and only a server with no checkouts at all keeps the no-op.
		repoRoot := t.TempDir()
		s := newReadGuardServer(t, repoRoot)
		elsewhere := filepath.Join(t.TempDir(), "elsewhere", "destination.txt")
		if err := s.guardBatchLifecycleDestination(context.Background(), elsewhere); err == nil || !strings.Contains(err.Error(), "checkout") {
			t.Fatalf("guard with known checkouts = %v, want a refusal naming the checkout", err)
		}
		if err := (&Server{}).guardBatchLifecycleDestination(context.Background(), elsewhere); err != nil {
			t.Fatalf("guard with no checkouts = %v, want nil", err)
		}
	})

	t.Run("unresolved request spelling with a symlink to the checkout root", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// The checkout is registered fully resolved and the request keeps the
		// spelling the temp dir handed out, so the lexical pass finds nothing
		// and only the ancestor climb identifies the checkout. `self` resolves
		// to the checkout root, which makes it a spelling the climb would
		// happily accept as the root — and accepting it would leave the very
		// symlink that redirects the write above the inspected range. Taking
		// the non-symlink ancestor below it instead is what keeps `self` a
		// component. On macOS /var -> /private/var makes the two spellings
		// differ; on Linux they coincide and the lexical pass answers first.
		repoRoot := t.TempDir()
		resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(repoRoot, filepath.Join(repoRoot, "self")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, resolvedRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		destination := filepath.Join(repoRoot, "self", "destination.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-unresolved-self-symlink")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "symlink") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, "destination.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("dot-dot traversal destination", func(t *testing.T) {
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
		outsideName := "traversal-destination-" + filepath.Base(repoRoot) + ".txt"
		destination := repoRoot + string(os.PathSeparator) + ".." + string(os.PathSeparator) + outsideName

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-dot-dot-destination")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "outside") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(filepath.Clean(destination)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination was created: %v", err)
		}
	})

	t.Run("delete directory", func(t *testing.T) {
		repoRoot := t.TempDir()
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		s := newReadGuardServer(t, repoRoot)
		directory := filepath.Join(repoRoot, "directory")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileDelete(directory, ""),
		}, "file-lifecycle-delete-directory")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory was changed: info=%v err=%v", info, err)
		}
	})
}

// caseAliasPath returns the upper-cased spelling of name when the filesystem
// under dir resolves it to the file that was written as name, and reports
// false on a case-sensitive filesystem where the two spellings are two files.
func caseAliasPath(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	alias := filepath.Join(dir, strings.ToUpper(name))
	if _, err := os.Lstat(alias); err != nil {
		return "", false
	}
	return alias, true
}

func TestAtomicBatchLifecycleAliasedPathsRefused(t *testing.T) {
	t.Run("hard link", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		source := writeAtomicBatchFixture(t, dir, "a.txt", "source\n")
		alias := filepath.Join(dir, "b.txt")
		if err := os.Link(source, alias); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(source, "source", "edited"),
			atomicFileDelete(alias, ""),
		}, "file-lifecycle-hard-link")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "name the same file") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
		if _, err := os.Stat(alias); err != nil {
			t.Fatalf("hard link was removed: %v", err)
		}
	})

	t.Run("case alias", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		source := writeAtomicBatchFixture(t, dir, "a.txt", "source\n")
		alias, ok := caseAliasPath(t, dir, "a.txt")
		if !ok {
			t.Skip("filesystem is case-sensitive; two spellings are two files")
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(source, "source", "edited"),
			atomicFileDelete(alias, ""),
		}, "file-lifecycle-case-alias")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "name the same file") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})

	t.Run("symlink leaf and its target", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// A symlink leaf and the file it points at are two spellings of one
		// file too: editing through the link replaces it with a regular file,
		// so a batch that also names the target ends up with two divergent
		// copies of what it addressed as one.
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		target := writeAtomicBatchFixture(t, dir, "a.txt", "source\n")
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(link, "source", "through the link"),
			atomicFileEdit(target, "source", "through the target"),
		}, "file-lifecycle-symlink-leaf")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "name the same file") {
			t.Fatalf("receipt = %+v", receipt)
		}
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link is no longer a symlink: info=%v err=%v", info, err)
		}
		if got := readAtomicBatchFixture(t, target); got != "source\n" {
			t.Fatalf("target = %q", got)
		}
	})

	t.Run("symlink leaf and a lifecycle op on its target", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		target := writeAtomicBatchFixture(t, dir, "a.txt", "source\n")
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(link, "source", "through the link"),
			atomicFileDelete(target, ""),
		}, "file-lifecycle-symlink-leaf-delete")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "name the same file") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, target); got != "source\n" {
			t.Fatalf("target = %q", got)
		}
	})

	t.Run("case-variant destinations", func(t *testing.T) {
		// Neither destination exists yet, so neither has an identity to compare
		// and the pairwise pass used to skip both. On a case-insensitive
		// filesystem the first move then created the file the second one
		// checked for, and the batch tore itself apart mid-commit. The refusal
		// is unconditional: on a case-sensitive filesystem the two are legal
		// but the batch cannot say which the caller meant.
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		first := writeAtomicBatchFixture(t, dir, "a.txt", "first\n")
		second := writeAtomicBatchFixture(t, dir, "b.txt", "second\n")
		upper := filepath.Join(dir, "X.txt")
		lower := filepath.Join(dir, "x.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(first, upper, ""),
			atomicFileMove(second, lower, ""),
		}, "file-lifecycle-case-variant-destinations")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" ||
			!strings.Contains(receipt.Error, "differ only by case") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, first); got != "first\n" {
			t.Fatalf("first source = %q", got)
		}
		if got := readAtomicBatchFixture(t, second); got != "second\n" {
			t.Fatalf("second source = %q", got)
		}
		for _, destination := range []string{upper, lower} {
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination %s was created: %v", destination, err)
			}
		}
	})

	t.Run("case-variant destinations under a new directory", func(t *testing.T) {
		// Same collision, but the shared parent does not exist yet either, so
		// there is no directory to compare. The nearest existing ancestor and
		// the spelling below it are what identify the pair.
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		first := writeAtomicBatchFixture(t, dir, "a.txt", "first\n")
		second := writeAtomicBatchFixture(t, dir, "b.txt", "second\n")
		newDir := filepath.Join(dir, "newdir")
		upper := filepath.Join(newDir, "X.txt")
		lower := filepath.Join(newDir, "x.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(first, upper, ""),
			atomicFileMove(second, lower, ""),
		}, "file-lifecycle-case-variant-destinations-new-directory")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" ||
			!strings.Contains(receipt.Error, "differ only by case") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, first); got != "first\n" {
			t.Fatalf("first source = %q", got)
		}
		if got := readAtomicBatchFixture(t, second); got != "second\n" {
			t.Fatalf("second source = %q", got)
		}
		if _, err := os.Lstat(newDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination directory was created: %v", err)
		}
	})

	t.Run("normalisation-variant destinations", func(t *testing.T) {
		// NFC and NFD spellings of one name are one directory entry on a
		// normalising filesystem, so two absent destinations that differ only
		// in composition collide at commit exactly like case variants do. The
		// refusal is unconditional for the same reason.
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		first := writeAtomicBatchFixture(t, dir, "a.txt", "first\n")
		second := writeAtomicBatchFixture(t, dir, "b.txt", "second\n")
		composed := filepath.Join(dir, "café.txt")
		decomposed := filepath.Join(dir, "café.txt")

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(first, composed, ""),
			atomicFileMove(second, decomposed, ""),
		}, "file-lifecycle-normalisation-variant-destinations")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" ||
			!strings.Contains(receipt.Error, "one directory entry") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, first); got != "first\n" {
			t.Fatalf("first source = %q", got)
		}
		if got := readAtomicBatchFixture(t, second); got != "second\n" {
			t.Fatalf("second source = %q", got)
		}
		for _, destination := range []string{composed, decomposed} {
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination %s was created: %v", destination, err)
			}
		}
	})

	t.Run("identical names under two spellings of one directory", func(t *testing.T) {
		if os.PathSeparator == '\\' {
			t.Skip("symlink creation is not reliably available on Windows CI")
		}
		// Byte-identical names reached through a symlinked directory and its
		// target are one entry too; the refusal must say so rather than
		// claim the names differ by case.
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}

		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileEdit(filepath.Join(dir, "link", "n.txt"), "a", "b"),
			atomicFileEdit(filepath.Join(real, "n.txt"), "a", "b"),
		}, "file-lifecycle-identical-names-two-spellings")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
			t.Fatalf("receipt = %+v", receipt)
		}
		if !strings.Contains(receipt.Error, "one directory entry") || strings.Contains(receipt.Error, "differ only by case") {
			t.Fatalf("error = %q, want the one-directory-entry wording", receipt.Error)
		}
	})

	t.Run("case-only rename", func(t *testing.T) {
		s := newAtomicBatchTestServer(t, mutationTestWatcher{})
		dir := t.TempDir()
		source := writeAtomicBatchFixture(t, dir, "a.txt", "source\n")
		destination, ok := caseAliasPath(t, dir, "a.txt")
		if !ok {
			t.Skip("filesystem is case-sensitive; two spellings are two files")
		}

		// A case-only rename is one inode under two spellings, so the batch
		// cannot describe it as a move: refuse rather than plan two futures.
		// The refusal itself pre-dates the aliasing rule — the move already
		// failed on "destination already exists", which misdescribes a rename
		// onto the very file being renamed. What the rule replaces is that
		// message.
		receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{
			atomicFileMove(source, destination, ""),
		}, "file-lifecycle-case-only-rename")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" || !strings.Contains(receipt.Error, "name the same file") {
			t.Fatalf("receipt = %+v", receipt)
		}
		if got := readAtomicBatchFixture(t, source); got != "source\n" {
			t.Fatalf("source = %q", got)
		}
	})
}
