package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A git_ref or commit selector names committed state nobody has checked out.
// Serving it is three steps: prove the graph the ref composes over is one this
// session may read and is ready, make the view current, and compose the
// generation that describes the ref's tree onto that graph's corpus.
//
// The stack is one layer and no more. A checkout's view stacks its working
// tree on top of its commit layer because that is what the checkout IS; a ref
// selector means the committed tree by definition, so there is no dirty slot
// and no buffer to add. That is also why the mutation gate refuses it: there
// is no working copy for a write to land in.

// refViewRetryAfterSeconds is the retry hint a building ref view answers with.
// A build is a full extraction pass over the changed part of a tree, so the
// hint is a poll interval rather than a prediction.
const refViewRetryAfterSeconds = 2

// viewForRefSelector serves a view of one graph at a committed selector.
func (s *Server) viewForRefSelector(ctx context.Context, selector graphview.Selector) (*requestView, error) {
	graphID, err := s.refViewGraphID(ctx, selector)
	if err != nil {
		return nil, err
	}
	dedicated, found, err := s.materializer.Catalog.GetDedicatedGraph(ctx, graphID)
	switch {
	case err != nil:
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", graphID), err)
	case !found:
		return nil, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("graph %q is not registered", graphID))
	}
	// The scope ceiling is checked before the state, the order the base
	// selector holds to: a session outside the workspace must not be able to
	// tell a building graph from a ready one in a sibling workspace.
	if err := s.repoPrefixInSessionScope(ctx, dedicated.RepoPrefix, graphID); err != nil {
		return nil, err
	}
	if s.lifecycle == nil {
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable,
			"this server builds no views of committed state")
	}
	if dedicated.State != reconcile.GraphStateReady {
		return nil, graphview.NewViewError(graphview.CodeViewBuilding,
			fmt.Sprintf("graph %q is %s", graphID, dedicated.State))
	}

	result, err := s.lifecycle.EnsureRefView(ctx, indexer.RefViewSelection{
		GraphID:       graphID,
		SelectorKind:  refSelectorKind(selector.Kind),
		SelectorValue: selector.Value,
	})
	if err != nil {
		return nil, refViewError(selector, err)
	}

	rider := graphview.NewViewRider(selector)
	rider.GraphID = graphID
	rider.RequestedRef = selector.Value
	rider.ResolvedRef = result.Resolved.FullRef
	rider.ResolvedCommit = result.Resolved.CommitOID
	rider.ResolvedTree = result.Resolved.TreeOID

	if result.State == store_sqlite.RefViewReady && result.GenerationID > 0 {
		return s.materializeRefView(ctx, dedicated, result.GenerationID, result.Resolved.TreeOID, rider)
	}
	return s.refViewBuilding(ctx, dedicated, result, rider)
}

// refViewBuilding answers a selection whose payload is still being produced.
//
// A view that has never published anything is a plain refusal carrying the
// build to poll. A view that IS serving something older may answer with it,
// but only labelled: the generation describes a tree the selector has moved
// off, so handing it back as the requested view would be a wrong answer that
// looks like a right one.
func (s *Server) refViewBuilding(
	ctx context.Context,
	dedicated store_sqlite.DedicatedGraph,
	result indexer.RefViewResult,
	rider *graphview.ViewRider,
) (*requestView, error) {
	rider.BuildToken = result.BuildToken
	rider.RetryAfter = refViewRetryAfterSeconds

	stored, found, err := s.lifecycle.RefViewGeneration(ctx, result.RefViewID)
	if err == nil && found && stored.ActiveGenerationID > 0 {
		fallback, ferr := s.materializeRefView(ctx, dedicated, stored.ActiveGenerationID, stored.ActiveTree, rider)
		if ferr == nil {
			fallback.rider.ResolvedRef = stored.ActiveRef
			fallback.rider.ResolvedCommit = stored.ActiveCommit
			fallback.rider.ResolvedTree = stored.ActiveTree
			markErr := fallback.rider.MarkFallback(fallback.rider.ActualView,
				fmt.Sprintf("%s: build %s is producing the requested tree",
					graphview.CodeViewBuilding, buildTokenOrUnknown(result.BuildToken)))
			if markErr != nil {
				fallback.close()
				return nil, markErr
			}
			return fallback, nil
		}
	}
	return nil, graphview.NewViewError(graphview.CodeViewBuilding, fmt.Sprintf(
		"ref view %s is building as build %s; retry after %ds",
		result.RefViewID, buildTokenOrUnknown(result.BuildToken), refViewRetryAfterSeconds))
}

// buildTokenOrUnknown names the build a caller polls. An empty token means the
// attempt that was running has just been superseded and the retry claims a new
// one, which is a state the caller must be able to tell from a token it could
// wait on.
func buildTokenOrUnknown(token string) string {
	if token == "" {
		return "(superseded)"
	}
	return token
}

// materializeRefView composes one ref-view generation onto its graph's corpus
// and binds the request's file surface to the tree it describes.
func (s *Server) materializeRefView(
	ctx context.Context,
	dedicated store_sqlite.DedicatedGraph,
	generationID int64,
	treeOID string,
	rider *graphview.ViewRider,
) (*requestView, error) {
	view, err := s.materializer.MaterializeRefView(ctx, dedicated.GraphID, generationID)
	if err != nil {
		return nil, err
	}
	fingerprint := view.ID.Fingerprint()
	rider.MarkExact(fingerprint)
	rider.ViewFingerprint = fingerprint

	routed := &requestView{
		kind:                  viewmetrics.ViewRef,
		reader:                view.Reader,
		materialized:          view,
		rider:                 rider,
		suppressBufferOverlay: true,
	}
	routed.bindSources(view.GenerationSources(), s.graph)
	routed.files = &refViewFiles{
		store:        s.viewStore(),
		fingerprint:  fingerprint,
		repoPrefix:   dedicated.RepoPrefix,
		repoDir:      s.repoDirForPrefix(ctx, dedicated),
		treeOID:      treeOID,
		generationID: generationID,
	}
	return routed, nil
}

// refViewGraphID decides which graph a ref composes over.
//
// A named graph is taken as named; an unnamed one is resolved only when there
// is nothing to choose between. Picking a repository for a caller that reaches
// several would answer about a different repository's branch of the same name,
// which is exactly the confusion a pinned view exists to remove.
func (s *Server) refViewGraphID(ctx context.Context, selector graphview.Selector) (string, error) {
	if selector.GraphID != "" {
		return selector.GraphID, nil
	}
	if s.graph == nil {
		return "", graphview.NewViewError(graphview.CodeInvalidViewSelector,
			"no repository is indexed, so a ref names nothing")
	}
	repos, bound := s.sessionWorkspaceRepoSet(ctx)
	var reachable []string
	for _, prefix := range s.graph.RepoPrefixes() {
		if prefix == "" || (bound && !repos[prefix]) {
			continue
		}
		reachable = append(reachable, prefix)
	}
	sort.Strings(reachable)
	switch len(reachable) {
	case 1:
		return indexer.GraphIDFor(reachable[0]), nil
	case 0:
		return "", graphview.NewViewError(graphview.CodeInvalidViewSelector,
			"this session reaches no repository a ref can be resolved against")
	default:
		return "", graphview.NewViewError(graphview.CodeInvalidViewSelector, fmt.Sprintf(
			"this session reaches %d repositories (%v); name one with graph_id",
			len(reachable), reachable))
	}
}

// repoDirForPrefix resolves the repository a ref view's trees are read from:
// the working copy the graph's owning checkout was indexed from, and otherwise
// whatever root the corpus records for its prefix.
func (s *Server) repoDirForPrefix(ctx context.Context, dedicated store_sqlite.DedicatedGraph) string {
	if dedicated.OwnerCheckoutID != "" {
		checkout, found, err := s.materializer.Catalog.GetCheckout(ctx, dedicated.OwnerCheckoutID)
		if err == nil && found && checkout.RootPath != "" {
			return checkout.RootPath
		}
	}
	if s.multiIndexer != nil {
		if root, ok := s.multiIndexer.RepoRoot(dedicated.RepoPrefix); ok {
			return root
		}
	}
	if s.indexer != nil {
		return s.indexer.RootPath()
	}
	return ""
}

// viewStore is the SQLite handle a withdrawal is recorded through, nil when
// the backend is not one.
func (s *Server) viewStore() *store_sqlite.Store {
	if s == nil || s.materializer == nil {
		return nil
	}
	return s.materializer.Store
}

// refSelectorKind maps the wire selector vocabulary onto the resolver's.
func refSelectorKind(kind graphview.SelectorKind) gitstate.ViewSelectorKind {
	if kind == graphview.SelectorCommit {
		return gitstate.ViewSelectorCommit
	}
	return gitstate.ViewSelectorGitRef
}

// refViewError re-codes a selection failure onto the wire vocabulary. The two
// resolution answers a caller can act on travel verbatim; everything else is
// the repository being unreadable.
func refViewError(selector graphview.Selector, err error) error {
	switch {
	case errors.Is(err, gitstate.ErrRefNotAvailableLocally):
		return graphview.WrapViewError(graphview.CodeRefNotAvailableLocally, selector.String(), err)
	case errors.Is(err, gitstate.ErrRefNotCommit):
		return graphview.WrapViewError(graphview.CodeRefNotCommit, selector.String(), err)
	case errors.Is(err, indexer.ErrRefViewStoreBusy):
		// A saturated writer is somebody else's build, so this is the same
		// answer a build of this view gives: retry. It carries no token,
		// because the selection never got as far as claiming one.
		return graphview.WrapViewError(graphview.CodeViewBuilding, fmt.Sprintf(
			"%s: retry after %ds", selector.String(), refViewRetryAfterSeconds), err)
	case graphview.CodeOf(err) != "":
		return err
	default:
		return graphview.WrapViewError(graphview.CodeCheckoutInaccessible, "serve "+selector.String(), err)
	}
}
