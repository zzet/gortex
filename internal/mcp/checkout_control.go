package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

type checkoutControlKey struct{}

// checkoutControlScope is catalog authority, not a graph reader or edit lease.
// Recovery and receipts must remain reachable while publication is pending,
// but may observe only the checkout the session is authorized to address.
type checkoutControlScope struct {
	Checkout       store_sqlite.Checkout
	RepoPrefix     string
	GraphID        string
	Route          *store_sqlite.CheckoutRoute
	Selector       graphview.Selector
	CheckoutScoped bool
}

func checkoutControlFromContext(ctx context.Context) *checkoutControlScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(checkoutControlKey{}).(*checkoutControlScope)
	return scope
}

func withCheckoutControl(ctx context.Context, scope *checkoutControlScope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, checkoutControlKey{}, scope)
}

func (s *Server) checkoutControlOperation(req *mcp.CallToolRequest) string {
	name := req.Params.Name
	if isFacadeToolName(name) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok {
			return ""
		}
		name = spec.Legacy
	}
	switch name {
	case "mutation_status", "reindex_repository", "detect_changes":
		return name
	default:
		return ""
	}
}

func catalogOnlyCheckoutControl(operation string) bool {
	return operation == "mutation_status" || operation == "reindex_repository"
}

// Catalog lookup must not depend on a ready corpus listing its repository:
// that is precisely the missing state a recovery request is meant to repair.
func (s *Server) registeredCheckoutForPath(ctx context.Context, path string) (store_sqlite.Checkout, bool, error) {
	families, err := s.materializer.Catalog.ListRepositoryFamilies(ctx)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	ids := make([]string, len(families))
	for i, family := range families {
		ids[i] = family.FamilyID
	}
	return graphview.CheckoutForPath(ctx, s.materializer.Catalog, ids, path)
}

// registeredWorktreeSelector shares exact-root matching between control and
// graph requests. In particular, a new nested worktree cannot resolve to its
// already registered parent while discovery is still pending.
func (s *Server) registeredWorktreeSelector(ctx context.Context, selector graphview.Selector) (store_sqlite.Checkout, error) {
	var checkout store_sqlite.Checkout
	var found bool
	var err error
	subject := selector.CheckoutID
	if selector.Path != "" {
		subject = selector.Path
		root := canonicalWorktreeSelectorRoot(selector.Path)
		checkout, found, err = s.checkoutForRequestPath(ctx, root)
		if err == nil && found {
			found = pathkey.EqualPaths(root, canonicalWorktreeSelectorRoot(checkout.RootPath))
		}
	} else {
		checkout, found, err = s.materializer.Catalog.GetCheckout(ctx, selector.CheckoutID)
	}
	if err != nil {
		return checkout, graphview.WrapViewError(graphview.CodeCheckoutInaccessible, fmt.Sprintf("read checkout %q", subject), err)
	}
	if found {
		return checkout, nil
	}
	if selector.Path == "" && filepath.IsAbs(selector.CheckoutID) {
		return checkout, graphview.NewViewError(graphview.CodeInvalidViewSelector,
			fmt.Sprintf("view.checkout_id expects a registered identifier, not a filesystem path; use view:{kind:\"worktree\",path:%q} or workspace(operation:\"checkouts\") to find its checkout_id", selector.CheckoutID))
	}
	if selector.Path != "" {
		return checkout, graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("no registered worktree root matches path %q; supply the worktree root, and if automatic discovery is pending inspect workspace(operation:\"checkouts\") and retry", selector.Path))
	}
	return checkout, graphview.NewViewError(graphview.CodeCheckoutInaccessible,
		fmt.Sprintf("checkout %q is not registered; use view.path for a filesystem path or workspace(operation:\"checkouts\") to find its checkout_id", selector.CheckoutID))
}

// checkoutControlPath reads the reindex selector before facade lowering. The
// legacy handler still validates the complete lowered path and paths arguments.
func checkoutControlPath(req *mcp.CallToolRequest) string {
	if path := req.GetString("path", ""); path != "" {
		return path
	}
	for _, container := range []string{"arguments", "options"} {
		fields, _ := req.GetArguments()[container].(map[string]any)
		if path, _ := fields["path"].(string); path != "" {
			return path
		}
	}
	return ""
}

func (s *Server) resolveCheckoutControlScope(ctx context.Context, selector graphview.Selector, req *mcp.CallToolRequest) (*checkoutControlScope, error) {
	if s.materializer == nil || s.materializer.Catalog == nil {
		if selector.Kind == graphview.SelectorAuto {
			return nil, nil
		}
		return nil, graphview.NewViewError(graphview.CodeCapabilityUnavailable, "checkout control requires a checkout catalog")
	}
	var checkout store_sqlite.Checkout
	var found bool
	var err error
	switch selector.Kind {
	case graphview.SelectorWorktree:
		checkout, err = s.registeredWorktreeSelector(ctx, selector)
		found = err == nil
	case graphview.SelectorAuto:
		path := SessionCWDFromContext(ctx)
		if s.checkoutControlOperation(req) == "reindex_repository" {
			if explicit := checkoutControlPath(req); explicit != "" {
				path, err = s.checkoutControlReindexPath(ctx, path, explicit)
				if err != nil {
					return nil, err
				}
			}
		}
		if path == "" {
			return nil, nil
		}
		checkout, found, err = s.checkoutForRequestPath(ctx, canonicalWorktreeSelectorRoot(path))
	case graphview.SelectorBase:
		return nil, graphview.NewViewError(graphview.CodeViewReadOnly, "base graph selectors do not select a working tree; use an explicit worktree view for checkout recovery, receipts, or working-tree change detection")
	default:
		return nil, graphview.NewViewError(graphview.CodeViewReadOnly, "checkout controls require a live checkout; immutable ref and commit views cannot be recovered or inspected as a working tree")
	}
	if err != nil {
		return nil, err
	}
	if !found {
		if selector.Kind == graphview.SelectorAuto {
			return nil, nil
		}
		return nil, graphview.NewViewError(graphview.CodeCheckoutInaccessible, "the selected checkout is not registered")
	}
	if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
		return nil, err
	}
	scope := &checkoutControlScope{
		Checkout: checkout, RepoPrefix: s.repoPrefixForCheckout(ctx, checkout), Selector: selector,
		CheckoutScoped: checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic || selector.Kind == graphview.SelectorWorktree,
	}
	if selector.Kind == graphview.SelectorAuto {
		scope.Selector = graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: checkout.CheckoutID}
	}
	route, present, err := s.materializer.Catalog.GetCheckoutRoute(ctx, checkout.CheckoutID)
	if err != nil {
		return nil, graphview.WrapViewError(graphview.CodeCheckoutInaccessible, "read checkout control route", err)
	}
	if present {
		scope.Route = &route
		scope.GraphID = route.GraphID
	}
	if scope.GraphID == "" {
		graphs, graphErr := s.materializer.Catalog.ListDedicatedGraphs(ctx, checkout.FamilyID)
		if graphErr != nil {
			return nil, graphErr
		}
		for _, graph := range graphs {
			if graph.RepoPrefix == scope.RepoPrefix {
				scope.GraphID = graph.GraphID
				break
			}
		}
	}
	return scope, nil
}

func (s *Server) checkoutControlReindexPath(ctx context.Context, cwd, path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if cwd != "" {
		checkout, found, err := s.checkoutForRequestPath(ctx, canonicalWorktreeSelectorRoot(cwd))
		if err != nil {
			return "", err
		}
		if found && graphview.ServesAutomaticView(checkout) {
			prefix := s.repoPrefixForCheckout(ctx, checkout)
			if prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
				return filepath.Join(checkout.RootPath, strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")), nil
			}
		}
	}
	if s.multiIndexer != nil {
		if root, ok := s.multiIndexer.RepoRoot(path); ok {
			return root, nil
		}
	}
	if cwd != "" {
		return filepath.Join(cwd, path), nil
	}
	return path, nil
}

func (scope *checkoutControlScope) validateRepoSelector(selector string) error {
	if selector == "" || selector == scope.RepoPrefix || pathkey.EqualPaths(canonicalWorktreeSelectorRoot(selector), canonicalWorktreeSelectorRoot(scope.Checkout.RootPath)) {
		return nil
	}
	return graphview.NewViewError(graphview.CodeSelectorConflict, "repository selector conflicts with the selected checkout")
}

func (scope *checkoutControlScope) validateReindexPaths(path string, paths []string) error {
	root, err := checkoutControlCanonicalPath(scope.Checkout.RootPath)
	if err != nil {
		return graphview.WrapViewError(graphview.CodeCheckoutInaccessible, "resolve selected checkout root", err)
	}
	if path != "" && path != scope.RepoPrefix {
		if !filepath.IsAbs(path) {
			path = filepath.Join(scope.Checkout.RootPath, path)
		}
		resolved, err := checkoutControlCanonicalPath(path)
		if err != nil || !pathContainedIn(resolved, root) {
			return graphview.NewViewError(graphview.CodeSelectorOutOfScope, "reindex path is outside the selected checkout")
		}
	}
	for _, path := range paths {
		path = strings.TrimPrefix(path, scope.RepoPrefix+"/")
		if !filepath.IsAbs(path) {
			path = filepath.Join(scope.Checkout.RootPath, path)
		}
		resolved, err := checkoutControlCanonicalPath(path)
		if err != nil || !pathContainedIn(resolved, root) {
			return graphview.NewViewError(graphview.CodeSelectorOutOfScope, "reindex paths must remain inside the selected checkout")
		}
	}
	return nil
}

// Resolve the longest existing ancestor, so aliases remain valid for new files
// while a nonexistent child of an escaping symlink is still refused.
func checkoutControlCanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := abs
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			rel, err := filepath.Rel(ancestor, abs)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, rel), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("no accessible ancestor for %q", path)
		}
		ancestor = parent
	}
}

// A catalog's containing parent is not authority for a newly created nested
// checkout that discovery has not registered yet. Fail closed until that
// checkout has its own identity rather than recovering the parent's graph.
func checkoutControlRootOwnsPath(root, path string) error {
	root, rootErr := checkoutControlCanonicalPath(root)
	path, pathErr := checkoutControlCanonicalPath(path)
	if rootErr != nil || pathErr != nil || !pathContainedIn(path, root) {
		return graphview.NewViewError(graphview.CodeCheckoutInaccessible, "checkout control path cannot be resolved inside its registered root")
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}
	for path != root {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return graphview.NewViewError(graphview.CodeCheckoutInaccessible, "the selected nested checkout is not registered yet; automatic discovery must establish its identity before checkout controls can run")
		} else if !os.IsNotExist(err) {
			return graphview.WrapViewError(graphview.CodeCheckoutInaccessible, "inspect checkout control path", err)
		}
		path = filepath.Dir(path)
	}
	return nil
}

func (s *Server) attachCheckoutControlScope(ctx context.Context, res *mcp.CallToolResult) *mcp.CallToolResult {
	scope := checkoutControlFromContext(ctx)
	if scope == nil || res == nil {
		return res
	}
	// Identity metadata deliberately carries no `exact:true` graph claim.
	return mergeResultMeta(res, map[string]any{"checkout_scope": map[string]any{
		"checkout_id": scope.Checkout.CheckoutID, "incarnation": scope.Checkout.Incarnation,
		"repo": scope.RepoPrefix, "root": scope.Checkout.RootPath, "graph_id": scope.GraphID,
		"state": string(scope.Checkout.State),
	}})
}
