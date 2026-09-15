package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
)

// viewlessCalls is the catalog-only surface, through both doors: the legacy
// tool names and the compact facade operation that lowers onto one of them.
func viewlessCalls(target string) []struct {
	name string
	tool string
	args map[string]any
} {
	return []struct {
		name string
		tool string
		args map[string]any
	}{
		{"untrack_repository", "untrack_repository", map[string]any{"path": target}},
		{"forget_checkout", "forget_checkout", map[string]any{"path": target}},
		{"explain_view", "explain_view", map[string]any{"path": target}},
		{"facade_untrack", "workspace_admin", map[string]any{"operation": "untrack", "path": target}},
	}
}

// selectToolSurface puts the fixture session on the surface that publishes
// the tool under test. The surface gate refuses a name the session's preset
// does not advertise, and it fires before view routing — a different gate to
// the one these tests are about.
func selectToolSurface(srv *Server, tool string) {
	surface := "full"
	if isFacadeToolName(tool) {
		surface = FacadeSurfaceVersion
	}
	srv.NoteSessionToolPolicy(viewTestSession, surface, "")
}

// TestCheckoutRemovalRunsThroughAnUnbindableCWD is the reported failure:
// `gortex untrack <path>` relays through an MCP session whose working
// directory IS the checkout being removed, and the middleware refused the call
// before the handler ran because that directory could not be bound to a
// checkout view.
//
// A nested directory carrying its own .git is a checkout automatic discovery
// has not registered. Its parent is deliberately not authority for it, so
// binding fails for every request — which is precisely the state a removal has
// to survive, and the one graph reads must still refuse.
func TestCheckoutRemovalRunsThroughAnUnbindableCWD(t *testing.T) {
	stack := newViewStack(t)
	nested := filepath.Join(stack.repoRoot, "vendored-theme")
	require.NoError(t, os.MkdirAll(filepath.Join(nested, ".git"), 0o755))

	// The gate is real and stays real: a graph read through the same cwd is
	// still refused rather than answered off the parent's corpus.
	readRan := false
	res, err := stack.callWithView(t, nested, "get_symbol", nil,
		func(context.Context) (*mcplib.CallToolResult, error) {
			readRan = true
			return mcplib.NewToolResultText(`{}`), nil
		})
	require.NoError(t, err)
	require.False(t, readRan, "get_symbol reached its handler through an unbindable cwd")
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)

	for _, call := range viewlessCalls(nested) {
		t.Run(call.name, func(t *testing.T) {
			selectToolSurface(stack.srv, call.tool)
			ran, bound := false, false
			res, err := stack.callWithView(t, nested, call.tool, call.args,
				func(ctx context.Context) (*mcplib.CallToolResult, error) {
					ran, bound = true, requestViewFromContext(ctx) != nil
					return mcplib.NewToolResultText(`{"status":"untracked"}`), nil
				})
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.True(t, ran, "the catalog-only call never reached its handler")
			require.False(t, bound, "a catalog-only call must not claim a view it never needed")
		})
	}
}

// TestCheckoutRemovalNeverBindsTheSessionCWD pins the skip as unconditional.
//
// The reported error was one binder outcome of several — automatic discovery
// still pending, its 250ms slice spent — and a removal that skipped only that
// outcome would still be hostage to the next one. A cwd that binds perfectly
// well proves the difference: the same session gets a routed view for a graph
// read and no view at all for a removal.
func TestCheckoutRemovalNeverBindsTheSessionCWD(t *testing.T) {
	stack := newViewStack(t)

	var reader graph.Reader
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	require.True(t, hasNode(reader, "repo/added.go::Fresh"),
		"the fixture cwd binds to a routed view, so the skip below is what is being observed")

	for _, call := range viewlessCalls(stack.worktreeRoot) {
		t.Run(call.name, func(t *testing.T) {
			selectToolSurface(stack.srv, call.tool)
			ran, bound := false, false
			res, err := stack.callWithView(t, stack.worktreeRoot, call.tool, call.args,
				func(ctx context.Context) (*mcplib.CallToolResult, error) {
					ran, bound = true, requestViewFromContext(ctx) != nil
					return mcplib.NewToolResultText(`{"status":"untracked"}`), nil
				})
			require.NoError(t, err)
			require.False(t, res.IsError, viewResultText(t, res))
			require.True(t, ran)
			require.False(t, bound, "a catalog-only call bound the session cwd to a view anyway")
		})
	}
}

// TestViewlessCatalogToolRecognisesBothDoors keeps the predicate honest about
// what it exempts, composing exactly the pair the middleware composes: the
// three catalog-only verbs and the facade operation that lowers onto one of
// them, and nothing that reads a graph.
//
// track_repository is the deliberate near-miss. It is the same facade group and
// the same shape of argument, but it creates a checkout rather than reporting
// on or removing one, and it has no reason to run through a cwd the daemon
// cannot bind.
func TestViewlessCatalogToolRecognisesBothDoors(t *testing.T) {
	stack := newViewStack(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want bool
	}{
		{"untrack_repository", map[string]any{"path": "/tmp/x"}, true},
		{"forget_checkout", map[string]any{"path": "/tmp/x"}, true},
		{"explain_view", map[string]any{"path": "/tmp/x"}, true},
		{"workspace_admin", map[string]any{"operation": "untrack", "path": "/tmp/x"}, true},
		{"workspace_admin", map[string]any{"operation": "track", "path": "/tmp/x"}, false},
		{"track_repository", map[string]any{"path": "/tmp/x"}, false},
		{"list_checkouts", map[string]any{}, false},
		{"reconcile_checkouts", map[string]any{}, false},
		{"get_symbol", map[string]any{"id": "repo/keep.go::Keeper"}, false},
	} {
		req := mcplib.CallToolRequest{}
		req.Params.Name = tc.tool
		req.Params.Arguments = tc.args
		name, _ := stack.srv.legacyToolName(&req)
		require.Equalf(t, tc.want, viewlessCatalogTool(name), "%s %v", tc.tool, tc.args)
	}
}

// TestUntrackSurvivesPendingCheckoutDiscovery reproduces the reported failure
// with the error it actually carried, rather than a stand-in for it.
//
// A linked worktree that automatic discovery has not finished registering is
// bound by observing it, and observation gets a 250ms slice. Blocking the HEAD
// sampler spends that slice without finishing, which is the busy outcome a slow
// Windows or network checkout produces on its own. A graph read through that
// cwd is refused with the reported `view_building` message; the removal, whose
// answer comes from catalog rows, runs.
// wedgeCheckoutDiscovery adds a linked worktree and blocks the HEAD sampler so
// automatic discovery for it cannot finish inside its 250ms slice. That is the
// busy outcome a slow Windows or network checkout produces on its own.
func wedgeCheckoutDiscovery(t *testing.T, f *realCheckoutMutationFixture, branch string) string {
	t.Helper()
	root := filepath.Join(filepath.Dir(f.primary), branch)
	checkoutMutationGit(t, f.primary, "worktree", "add", "-b", branch, root)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	reconcile.WithHEADSampler(func(ctx context.Context, root string) (gitstate.HEADState, error) {
		select {
		case <-ctx.Done():
			return gitstate.HEADState{}, errors.New("injected Git subprocess killed")
		case <-release:
			return gitstate.SampleHEAD(ctx, root)
		}
	})(f.srv.lifecycle.Reconciler())
	t.Cleanup(unblock)
	return root
}

// assertReportedRefusal fails unless res carries the refusal from the report.
func assertReportedRefusal(t *testing.T, res *mcplib.CallToolResult) {
	t.Helper()
	assertToolError(t, res, graphview.CodeViewBuilding)
	require.Contains(t, viewResultText(t, res), "checkout mutation lane is busy",
		"the control read must carry the reported cause, not some other refusal")
}

func TestUntrackSurvivesPendingCheckoutDiscovery(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	root := wedgeCheckoutDiscovery(t, f, "discovery-pending")

	// The binder really is wedged, and it fails the way the report did.
	assertReportedRefusal(t, f.facade(t, root, "search", map[string]any{
		"operation": "symbols", "query": "Old",
	}))

	// Same session, same wedged cwd: the removal reaches its handler and answers
	// from the catalog. `gortex untrack` is exactly this call.
	removal := f.facade(t, root, "workspace_admin", map[string]any{
		"operation": "untrack", "path": f.primary,
	})
	require.False(t, removal.IsError, viewResultText(t, removal))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, removal)), &payload))
	require.Equal(t, "preview", payload["status"])
	require.Equal(t, string(indexer.UntrackPlanPrimaryClosure), payload["plan"],
		"the removal must return a real catalog-derived plan, not a refusal")
}

// TestExplainViewSurvivesPendingCheckoutDiscovery is the diagnosability half.
//
// explain_view exists to answer "why is this path not being served the way I
// expect" — so a binding failure is its subject, not a reason to refuse it.
// Through the same wedged cwd that refuses a graph read, it names the checkout
// and the step that could not be taken.
func TestExplainViewSurvivesPendingCheckoutDiscovery(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	root := wedgeCheckoutDiscovery(t, f, "explain-pending")

	legacy := func(name string, handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error), args map[string]any) *mcplib.CallToolResult {
		t.Helper()
		req := mcplib.CallToolRequest{}
		req.Params.Name, req.Params.Arguments = name, args
		ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-lifecycle"), root)
		res, err := f.srv.wrapToolHandler(handler)(ctx, req)
		require.NoError(t, err)
		return res
	}

	assertReportedRefusal(t, legacy("get_symbol", f.srv.handleGetSymbol,
		map[string]any{"id": "repo/edit.go::Old"}))

	explained := legacy("explain_view", f.srv.handleExplainView, map[string]any{"path": f.primary})
	require.False(t, explained.IsError, viewResultText(t, explained))
	var binding map[string]any
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, explained)), &binding))
	require.Equal(t, true, binding["matched"],
		"explain_view must answer from the catalog, not refuse for the reason it was called to report")
	require.NotEmpty(t, binding["chain"], "the explanation carries the chain it walked")
}
