package graphview

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// RegisteredCheckoutForPath binds an ordinary registered CWD without reading
// the other repositories' families. A lexical match keeps CheckoutForPath's
// precedence: only a complete lexical miss may resolve canonical aliases.
func RegisteredCheckoutForPath(ctx context.Context, catalog *store_sqlite.Catalog, path string) (store_sqlite.Checkout, bool, error) {
	if catalog == nil || path == "" {
		return store_sqlite.Checkout{}, false, nil
	}
	cleaned := filepath.Clean(path)
	// A repo-root CWD is the common case and needs only one seek. Its exact
	// root is necessarily more specific than every lexical ancestor.
	candidates, err := catalog.CheckoutsByRootPaths(ctx, []string{cleaned})
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	if best, found := checkoutForPathSpelling(candidates, cleaned, false); found {
		return best, true, nil
	}
	var ancestors []string
	for parent := filepath.Dir(cleaned); parent != cleaned; parent = filepath.Dir(parent) {
		ancestors = append(ancestors, parent)
		if next := filepath.Dir(parent); next == parent {
			break
		}
	}
	candidates, err = catalog.CheckoutsByRootPaths(ctx, ancestors)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	if best, found := checkoutForPathSpelling(candidates, cleaned, false); found {
		return best, true, nil
	}
	// A legacy root may itself be a symlink spelling. Resolving only the
	// requested path would miss that root, or incorrectly prefer a canonical
	// outer checkout over a deeper aliased one. Preserve the alias-aware
	// catalog discovery only for a complete indexed lexical miss.
	families, err := catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	ids := make([]string, len(families))
	for i, family := range families {
		ids[i] = family.FamilyID
	}
	return CheckoutForPath(ctx, catalog, ids, path)
}

// CheckoutForPath finds the registered checkout a filesystem path sits in.
//
// The catalog indexes checkouts by family, so the caller passes the families
// its corpus reaches rather than the table being scanned whole. The longest
// root wins: worktrees nest inside other checkouts, and the innermost one is
// the working copy a path actually belongs to.
//
// A path that lies in no registered checkout is not an error — it is the
// ordinary case for every directory nobody has tracked.
func CheckoutForPath(
	ctx context.Context,
	catalog *store_sqlite.Catalog,
	familyIDs []string,
	path string,
) (store_sqlite.Checkout, bool, error) {
	if catalog == nil || path == "" {
		return store_sqlite.Checkout{}, false, nil
	}
	cleaned := filepath.Clean(path)
	var candidates []store_sqlite.Checkout
	for _, familyID := range familyIDs {
		if familyID == "" {
			continue
		}
		checkouts, err := catalog.ListCheckouts(ctx, familyID)
		if err != nil {
			return store_sqlite.Checkout{}, false, WrapViewError(CodeCheckoutInaccessible,
				fmt.Sprintf("list the checkouts of family %q", familyID), err)
		}
		candidates = append(candidates, checkouts...)
	}
	// Keep the ordinary request path filesystem-free. Only a complete lexical
	// miss enters the alias-aware pass; there the candidate is canonicalized
	// once and roots are ranked by canonical length, not by the arbitrary
	// length of their symlink spelling.
	if best, found := checkoutForPathSpelling(candidates, cleaned, false); found {
		return best, true, nil
	}
	best, found := checkoutForPathSpelling(candidates, pathkey.CanonicalPath(cleaned), true)
	return best, found, nil
}

func checkoutForPathSpelling(checkouts []store_sqlite.Checkout, path string, canonicalRoots bool) (store_sqlite.Checkout, bool) {
	var (
		best    store_sqlite.Checkout
		bestLen = -1
	)
	for _, checkout := range checkouts {
		if checkout.RootPath == "" {
			continue
		}
		root := filepath.Clean(checkout.RootPath)
		if canonicalRoots {
			root = pathkey.CanonicalPath(root)
		}
		if pathkey.HasPathPrefix(path, root) && len(root) > bestLen {
			best, bestLen = checkout, len(root)
		}
	}
	return best, bestLen >= 0
}

// ServesAutomaticView reports whether a checkout is one the shared lane
// answers for: it is live, and it is served from the family's automatic
// graph rather than from a dedicated one. A dedicated checkout and the
// primary are read from the indexed corpus directly, so neither has a
// composed view to route to.
func ServesAutomaticView(checkout store_sqlite.Checkout) bool {
	return checkout.State == store_sqlite.CheckoutStateReady &&
		checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic
}

// RouteReady reports whether a route can serve a composed view: it must be
// active and name both generation slots. A route missing the working-tree
// slot is mid-build — serving the commit generation alone would answer out
// of a state of the world the checkout is not in.
func RouteReady(route store_sqlite.CheckoutRoute) bool {
	return route.State == store_sqlite.RouteActive &&
		route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
}
