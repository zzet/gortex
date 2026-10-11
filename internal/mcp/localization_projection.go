package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func (s *Server) localizationNodeScope(
	ctx context.Context,
	opts query.QueryOptions,
	kinds ...graph.NodeKind,
) graph.LocalizationNodeScope {
	return s.localizationNodeScopeWithTests(ctx, opts, true, kinds...)
}

// localizationNodeScopeWithTests keeps request/session intersection identical
// across localization projections while letting file ownership include test
// declarations without transferring metadata from SQLite.
func (s *Server) localizationNodeScopeWithTests(
	ctx context.Context,
	opts query.QueryOptions,
	excludeTests bool,
	kinds ...graph.NodeKind,
) graph.LocalizationNodeScope {
	if session, bound := s.sessionScopeOptions(ctx); bound {
		switch {
		case opts.WorkspaceID == "":
			opts.WorkspaceID = session.WorkspaceID
		case session.WorkspaceID != "" && opts.WorkspaceID != session.WorkspaceID:
			opts.WorkspaceID = unresolvedWorkspacePrefix + "disjoint-localization-scope"
			opts.RepoAllow = nil
		}
		switch {
		case len(opts.RepoAllow) == 0:
			opts.RepoAllow = session.RepoAllow
		case len(session.RepoAllow) == 0:
			// The session workspace boundary alone remains authoritative.
		default:
			if merged := intersectRepoSets(opts.RepoAllow, session.RepoAllow); len(merged) > 0 {
				opts.RepoAllow = merged
			} else {
				opts.WorkspaceID = unresolvedWorkspacePrefix + "disjoint-localization-repos"
				opts.RepoAllow = nil
			}
		}
	}
	scope := graph.LocalizationNodeScope{
		WorkspaceID:  opts.WorkspaceID,
		ProjectID:    opts.ProjectID,
		RepoAllow:    opts.RepoAllow,
		ExcludeTests: excludeTests,
	}
	if len(kinds) > 0 {
		scope.Kinds = make(map[graph.NodeKind]bool, len(kinds))
		for _, kind := range kinds {
			scope.Kinds[kind] = true
		}
	}
	return scope
}

// boundedLocalizationExactName deliberately has no Reader.FindNodesByName
// fallback. A backend without the typed capability returns no localization
// evidence instead of reintroducing an unbounded full-row read.
func boundedLocalizationExactName(
	ctx context.Context,
	reader graph.Reader,
	name string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, bool) {
	bounded, ok := contractCoreSelectedReader(reader).(graph.BoundedExactNameReader)
	if !ok || ctx.Err() != nil {
		return graph.BoundedNodeProjection{}, false
	}
	page, err := bounded.FindNodesByNameBounded(ctx, name, scope, limit)
	if err != nil {
		return graph.BoundedNodeProjection{}, false
	}
	return page, true
}

// boundedLocalizationFileNodes performs one metadata-free file projection
// under the request-wide localization budget. Incomplete pages are not usable
// evidence: ranked-file recovery makes uniqueness and ownership decisions, so
// accepting a prefix could fabricate an unambiguous declaration.
func boundedLocalizationFileNodes(
	ctx context.Context,
	reader graph.Reader,
	budget *localizationFileRequestBudget,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, bool) {
	bounded, ok := reader.(graph.BoundedFileNodeReader)
	if !ok || path == "" || ctx.Err() != nil {
		return graph.BoundedNodeProjection{}, false
	}
	if limit > localizationFileNodeLimit {
		limit = localizationFileNodeLimit
	}
	reserved := budget.reserve(limit)
	if reserved <= 0 {
		return graph.BoundedNodeProjection{}, false
	}
	page, err := bounded.FindFileNodesBounded(ctx, path, scope, reserved)
	if err != nil || ctx.Err() != nil {
		return graph.BoundedNodeProjection{}, false
	}
	consumed := page.Total
	if len(page.Nodes) > consumed {
		consumed = len(page.Nodes)
	}
	budget.finish(reserved, consumed)
	if page.Truncated {
		return graph.BoundedNodeProjection{}, false
	}
	return page, true
}
