package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func requestRequiresExactCheckoutView(req *mcp.CallToolRequest) bool {
	if req.GetBool("require_exact", false) {
		return true
	}
	for _, container := range []string{"options", "arguments"} {
		fields, _ := req.GetArguments()[container].(map[string]any)
		if exact, _ := fields["require_exact"].(bool); exact {
			return true
		}
	}
	return false
}

// checkoutForRequestPath is the shared catalog boundary for daemon admission,
// session scope and request view selection. A cache miss observes only this
// checkout of an already known Git family; it never tracks a new repository or
// waits for parsing. Existing identities take the catalog-only fast path.
func (s *Server) checkoutForRequestPath(ctx context.Context, path string) (store_sqlite.Checkout, bool, error) {
	checkout, found, err := s.registeredCheckoutForPath(ctx, path)
	if err != nil {
		return checkout, false, err
	}
	var parentErr error
	if found {
		parentErr = checkoutControlRootOwnsPath(checkout.RootPath, path)
		if parentErr == nil {
			return checkout, true, nil
		}
	}
	if s.lifecycle == nil {
		return store_sqlite.Checkout{}, false, parentErr
	}
	// CWD observation establishes a session's scope. A different explicit
	// path must pass that scope before allocation or coordinator activation,
	// not merely before its graph data is returned. The authorizer is called
	// outside lifecycle locks; deriving the scope may bind the own CWD once.
	var authorize []func(string) error
	if cwd := SessionCWDFromContext(ctx); cwd != "" && !pathkey.EqualPaths(canonicalWorktreeSelectorRoot(cwd), canonicalWorktreeSelectorRoot(path)) {
		authorize = append(authorize, func(prefix string) error {
			return s.repoPrefixInSessionScope(ctx, prefix, prefix)
		})
	}
	observed, present, observeErr := s.lifecycle.ObserveCheckoutPath(ctx, path, authorize...)
	if observeErr != nil {
		return store_sqlite.Checkout{}, false, observeErr
	}
	if !present {
		return store_sqlite.Checkout{}, false, parentErr
	}
	return observed, true, nil
}
