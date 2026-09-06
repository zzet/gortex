package mcp

import (
	"context"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
// ok is false when no view catalog is wired, when the path lies in no
// registered checkout, when the checkout is not one the shared lane answers
// for, or when its family's primary is not a tracked repository. Every one of
// those leaves the caller on the fail-closed path it took before.
func (s *Server) scopeForAutomaticCheckout(ctx context.Context, cwd string) (workspaceID, projectID, repoPrefix string, ok bool) {
	workspaceID, projectID, repoPrefix, ok, _ = s.scopeForAutomaticCheckoutChecked(ctx, cwd)
	return workspaceID, projectID, repoPrefix, ok
}

func (s *Server) scopeForAutomaticCheckoutChecked(ctx context.Context, cwd string) (workspaceID, projectID, repoPrefix string, ok bool, err error) {
	return s.scopeForAutomaticCheckoutWithPrefix(ctx, cwd, s.repoPrefixForCheckoutChecked)
}

func (s *Server) scopeForAutomaticCheckoutWithPrefix(ctx context.Context, cwd string, lookup func(context.Context, store_sqlite.Checkout) (string, error)) (workspaceID, projectID, repoPrefix string, ok bool, err error) {
	if s == nil || s.multiIndexer == nil || s.materializer == nil || s.materializer.Catalog == nil {
		return "", "", "", false, nil
	}
	checkout, found, err := s.checkoutForRequestPath(ctx, cwd)
	if err != nil {
		// A catalog that cannot be read leaves the cwd unresolved, which is
		// the fail-closed answer — never a wider one.
		if s.logger != nil {
			s.logger.Debug("session scope: could not bind the cwd to a checkout", zap.Error(err))
		}
		return "", "", "", false, err
	}
	if !found || !graphview.ServesAutomaticView(checkout) {
		return "", "", "", false, nil
	}
	prefix, err := lookup(ctx, checkout)
	if err != nil {
		return "", "", "", false, err
	}
	if prefix == "" {
		return "", "", "", false, nil
	}
	root, known := s.multiIndexer.RepoRoot(prefix)
	if !known {
		return "", "", "", false, nil
	}
	workspaceID, projectID, repoPrefix, ok = s.multiIndexer.ScopeForCWD(root)
	return workspaceID, projectID, repoPrefix, ok, nil
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
	ok, _ := s.CheckoutServesCWDChecked(ctx, cwd)
	return ok
}

// CheckoutServesCWDChecked distinguishes an unknown checkout from transient
// catalog/discovery contention. Both remain unadmitted, but a daemon must tell
// the caller to retry the latter rather than incorrectly suggest tracking it.
// No pending observation is treated as a scoped or ready checkout.
func (s *Server) CheckoutServesCWDChecked(ctx context.Context, cwd string) (bool, error) {
	_, _, _, ok, err := s.scopeForAutomaticCheckoutChecked(ctx, cwd)
	return ok, err
}
