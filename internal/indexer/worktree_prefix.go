package indexer

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// The naming rule for a worktree that gets a corpus of its own.
//
// A dedicated graph built for a linked worktree is stored under
// `<family base prefix>@<admin name>`: the repo prefix of the family's primary
// dedicated graph, then the name git administers the worktree under.
//
// The admin name is the only stable thing about a worktree. A branch is not:
// WorktreeInstanceName froze the branch that happened to be checked out at
// track time into the prefix, so `feat/x` stayed in the node ids of every node
// in that corpus for as long as it was tracked, however many branches the
// worktree visited afterwards. A declared workspace is not either — it can be
// edited. The admin name is assigned by git when the worktree is created, is
// unique within the family, survives every branch switch, and can be read back
// from `git worktree list` on any machine.
//
// Collisions can only come from outside the family: git will not administer
// two worktrees of one repository under one name, but two families whose
// primaries share a base prefix can each hold a worktree called `review`. The
// loser takes `<base>@<admin>-<hash>`, where the hash is the same short digest
// of the cleaned absolute root path the instance naming has always used. It
// depends on nothing but the path, so the name a checkout would take can be
// worked out offline, from a path alone, without asking the daemon.
//
// A prefix that is already registered is never re-derived. The nodes of a
// tracked corpus carry their prefix in every id, so renaming one is a re-index
// of the repository — which is exactly what a rule change must not cost the
// repositories that were tracked under the old one.

// DedicatedWorktreePrefix names the repo prefix a worktree's own corpus is
// stored under, given its family's base prefix and the name git administers it
// as. taken reports the root path already registered under a candidate prefix,
// and may be nil when nothing is registered yet.
func DedicatedWorktreePrefix(
	basePrefix, adminName, root string,
	taken func(prefix string) (string, bool),
) string {
	tag := sanitizeInstanceTag(adminName)
	if tag == "" {
		tag = sanitizeInstanceTag(filepath.Base(root))
	}
	if tag == "" {
		tag = shortPathHash(root)
	}
	candidate := basePrefix + "@" + tag
	if taken == nil {
		return candidate
	}
	if holder, ok := taken(candidate); ok && !pathkey.SamePathIdentity(holder, root) {
		return candidate + "-" + shortPathHash(root)
	}
	return candidate
}

// dedicatedPrefixFor decides the repo prefix a dedicated registration of one
// checkout root takes, and reports "" when the historical naming applies —
// a path git does not administer as a linked worktree, or a family with no
// primary corpus to derive a base from.
//
// An existing registration wins over the rule, whether the corpus holds it or
// only the catalog does. That is what makes re-tracking a worktree a no-op on
// its name: the prefix is decided once, when the corpus is first built, and
// every later pass reads back the decision instead of re-deriving it.
func (l *CheckoutLifecycle) dedicatedPrefixFor(ctx context.Context, root string) string {
	if l == nil || root == "" {
		return ""
	}
	if prefix := l.registeredPrefixForRoot(root); prefix != "" {
		return prefix
	}
	if l.catalog == nil {
		return ""
	}
	inv, err := gitstate.Inventory(ctx, root)
	if err != nil {
		return ""
	}
	record := recordForRoot(inv, root)
	if record == nil || record.AdminName == "" || record.AdminName == gitstate.MainAdminName {
		// The main worktree is the repository as far as naming goes: it takes
		// the base prefix itself rather than an instance of it.
		return ""
	}
	familyID := FamilyIDFor(inv.CommonDir)
	if prefix := l.boundPrefixForAdminName(ctx, familyID, record.AdminName); prefix != "" {
		return prefix
	}
	base := l.familyBasePrefix(ctx, familyID)
	if base == "" {
		return ""
	}
	return DedicatedWorktreePrefix(base, record.AdminName, root, l.prefixHolder(ctx))
}

// registeredPrefixForRoot reports the prefix a root is already tracked under.
//
// The match is on the root itself rather than on containment: a worktree
// checked out inside its own repository is a different corpus from the one
// around it, and a containment match would answer with the enclosing
// repository's prefix.
func (l *CheckoutLifecycle) registeredPrefixForRoot(root string) string {
	if l == nil || l.mi == nil {
		return ""
	}
	for prefix, meta := range l.mi.AllMetadata() {
		if meta != nil && pathkey.SamePathIdentity(meta.RootPath, root) {
			return prefix
		}
	}
	return ""
}

// boundPrefixForAdminName reads the prefix a family's checkout already owns a
// dedicated graph under, empty when it owns none.
func (l *CheckoutLifecycle) boundPrefixForAdminName(ctx context.Context, familyID, adminName string) string {
	checkout, err := l.checkoutByAdminName(ctx, familyID, adminName)
	if err != nil || checkout == nil {
		return ""
	}
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return ""
	}
	for _, graph := range graphs {
		if graph.OwnerCheckoutID == checkout.CheckoutID {
			return graph.RepoPrefix
		}
	}
	return ""
}

// familyBasePrefix reads the repo prefix of a family's primary corpus.
func (l *CheckoutLifecycle) familyBasePrefix(ctx context.Context, familyID string) string {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return ""
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			return graph.RepoPrefix
		}
	}
	return ""
}

// prefixHolder answers which root a candidate prefix is already spoken for by.
// It reads the corpus first and the catalog second, so a prefix whose
// repository is bound but not indexed yet still counts as taken.
func (l *CheckoutLifecycle) prefixHolder(ctx context.Context) func(string) (string, bool) {
	return func(prefix string) (string, bool) {
		if meta := l.mi.GetMetadata(prefix); meta != nil {
			return meta.RootPath, true
		}
		graph, ok, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
		if err != nil || !ok {
			return "", false
		}
		owner, ok, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
		if err != nil || !ok {
			return "", true
		}
		return owner.RootPath, true
	}
}

// checkoutStateOf reads one checkout row, reporting the not-tracked error
// rather than a bare bool so every flow refuses an unknown identity the same
// way.
func (l *CheckoutLifecycle) checkoutStateOf(ctx context.Context, checkoutID string) (store_sqlite.Checkout, error) {
	if l.catalog == nil {
		return store_sqlite.Checkout{}, errNoCatalog
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return store_sqlite.Checkout{}, err
	}
	if !found {
		return store_sqlite.Checkout{}, fmt.Errorf("%w: checkout %s", ErrCheckoutNotTracked, checkoutID)
	}
	return checkout, nil
}
