package mcp

import (
	"context"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graphview"
)

// scopeForAutomaticCheckout resolves a working directory that lies inside no
// tracked repository but inside a registered automatic checkout — a worktree
// added after the family was indexed, whose files no repository entry covers.
//
// Such a checkout is not a repository of its own: it is served from its
// family's primary base graph, which is exactly what its routed view composes
// over. In scope terms it therefore occupies the primary's repository, and the
// binding it gets here is the binding that repository's own root produces —
// same workspace slug, same project, same home repo. The ceiling gains no
// revision dimension: a checkout narrows what a request READS, never what it
// is permitted to reach.
//
// The catalog lookup is the one view selection itself uses
// (graphview.CheckoutForPath over the same families), so a session cannot bind
// its scope to one checkout and read through another.
//
// During availability/removal grace, the checkout no longer has an exact
// routed view, but eligible reads deliberately fall back to the sealed family
// primary. Those states therefore bind through the primary registration too.
// In particular, they must not use repoPrefixForCheckout: a disappearing
// checkout can still own a stale dedicated graph row, while the answer being
// served belongs to the surviving primary corpus.
//
// ok is false when no view catalog is wired, when the path lies in no
// registered checkout, when the checkout is neither a live automatic checkout
// nor one of the grace states served from the primary, or when the selected
// repository is not tracked. Every one of those leaves the caller on the
// fail-closed path it took before.
func (s *Server) scopeForAutomaticCheckout(ctx context.Context, cwd string) (workspaceID, projectID, repoPrefix string, ok bool) {
	if s == nil || s.multiIndexer == nil || s.materializer == nil || s.materializer.Catalog == nil {
		return "", "", "", false
	}
	checkout, found, err := graphview.CheckoutForPath(ctx, s.materializer.Catalog, s.viewFamilies(ctx), cwd)
	if err != nil {
		// A catalog that cannot be read leaves the cwd unresolved, which is
		// the fail-closed answer — never a wider one.
		if s.logger != nil {
			s.logger.Debug("session scope: could not bind the cwd to a checkout", zap.Error(err))
		}
		return "", "", "", false
	}
	if !found {
		return "", "", "", false
	}

	var prefix string
	switch {
	case checkoutStateAllowsBaseFallback(checkout.State):
		primary, primaryErr := s.familyPrimaryRegistration(ctx, checkout.FamilyID)
		if primaryErr != nil {
			if s.logger != nil {
				s.logger.Debug("session scope: could not resolve the checkout's family primary", zap.Error(primaryErr))
			}
			return "", "", "", false
		}
		prefix = primary.RepoPrefix
	case graphview.ServesAutomaticView(checkout):
		prefix = s.repoPrefixForCheckout(ctx, checkout)
	default:
		return "", "", "", false
	}
	if prefix == "" {
		return "", "", "", false
	}
	root, known := s.multiIndexer.RepoRoot(prefix)
	if !known {
		return "", "", "", false
	}
	return s.multiIndexer.ScopeForCWD(root)
}

// CheckoutServesCWD reports whether a working directory binds to a registered
// checkout this server can scope a session to.
//
// It is the admission question the daemon's dispatcher asks before handing a
// frame over, and it answers from scopeForAutomaticCheckout so the gate and
// the scope behind it cannot disagree: a cwd admitted here is one sessionScope
// binds to the family's primary, and a cwd refused here is one it would have
// left unresolved.
func (s *Server) CheckoutServesCWD(ctx context.Context, cwd string) bool {
	_, _, _, ok := s.scopeForAutomaticCheckout(ctx, cwd)
	return ok
}
