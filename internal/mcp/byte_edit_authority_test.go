package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func byteEditFixtureHeadProof(t *testing.T, f *realCheckoutMutationFixture, forceUnavailable bool) bool {
	t.Helper()
	if forceUnavailable {
		// Git keeps its configured files ref store; only the optional stat proof
		// declines this unsupported layout. The ordinary full sample still works.
		require.NoError(t, os.Mkdir(filepath.Join(f.primary, ".git", "reftable"), 0o755))
	}
	sampler, err := gitstate.NewDirtySampler(f.worktree, "", "")
	require.NoError(t, err)
	usable := sampler.CaptureHeadEvidence().Usable()
	if forceUnavailable {
		require.False(t, usable)
	}
	return usable
}

func byteEditFixtureViewMatches(ctx context.Context, f *realCheckoutMutationFixture) bool {
	view := requestViewFromContext(ctx)
	if view == nil || checkoutMutationFromContext(ctx) == nil || !pathkey.EqualPaths(view.viewRoot, f.worktree) {
		return false
	}
	if view.sourceScope == "file" {
		return view.materialized == nil && view.sourceGraphPending
	}
	// These MCP fixtures release the old parser immediately after starting the
	// request. Ready ordinary grants and pending byte handoffs both use checked
	// file carriers; an unavailable optional path may still materialize a view;
	// strict pending/attempt handoff is separately held by indexer barriers.
	return view.sourceScope == "" && view.materialized != nil && !view.sourceGraphPending
}

func TestPlainByteEditUsesSourceAuthorityAcrossPendingPublication(t *testing.T) {
	for _, forceUnavailable := range []bool{false, true} {
		name := "actual HEAD capability"
		if forceUnavailable {
			name = "unavailable HEAD evidence"
		}
		t.Run(name, func(t *testing.T) { testPlainByteEditAcrossPendingPublication(t, forceUnavailable) })
	}
}

func testPlainByteEditAcrossPendingPublication(t *testing.T, forceUnavailable bool) {
	f, blocked := newBlockedLifecycleFixture(t)
	byteEditFixtureHeadProof(t, f, forceUnavailable)
	old := f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}", "format": "json"})
	require.False(t, old.IsError, viewResultText(t, old))
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("old publication never entered parser")
	}
	target := filepath.Join(f.worktree, "byte.go")
	before := []byte("package repo\nfunc ByteBefore() {}\n")
	require.NoError(t, os.WriteFile(target, before, 0600))
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": "repo/byte.go", "old_string": "ByteBefore", "new_string": "ByteAfter", "base_sha": gitBlobSHA(before), "expected_occurrences": 1, "format": "json"}
	entered := make(chan struct{})
	result := make(chan *mcplib.CallToolResult, 1)
	ctx := WithSessionCWD(WithSessionID(t.Context(), "plain-byte-handoff"), f.worktree)
	go func() {
		r, err := f.srv.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if !byteEditFixtureViewMatches(ctx, f) {
				return mcplib.NewToolResultError("wrong mutation authority for actual HEAD capability"), nil
			}
			close(entered)
			return f.srv.handleEditFile(ctx, req)
		})(ctx, req)
		if err != nil {
			result <- mcplib.NewToolResultError(err.Error())
			return
		}
		result <- r
	}()
	select {
	case <-entered:
		t.Fatal("edit entered before old parser drained")
	case <-result:
		t.Fatal("edit returned before old parser drained")
	default:
	}
	disk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, disk)
	blocked.unblock()
	var r *mcplib.CallToolResult
	select {
	case r = <-result:
	case <-time.After(20 * time.Second):
		t.Fatal("byte edit did not finish after drain")
	}
	require.False(t, r.IsError, viewResultText(t, r))
	select {
	case <-entered:
	default:
		t.Fatal("handler never received byte authority")
	}
	payload := decodeFileOpsResult(t, r)
	require.Equal(t, "applied", payload["status"])
	fresh := resultFreshness(t, r)
	if fresh["freshness_scope"] == "file" {
		require.Equal(t, "pending", fresh["graph_freshness"])
	} else {
		require.NotEqual(t, "file", fresh["freshness_scope"], "ordinary sampled admission must not claim source-only byte authority")
	}
	require.Equal(t, f.checkoutID, fresh["checkout_id"])
	disk, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(disk), "ByteAfter")
	prior, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(prior), "LifecycleBlocked")
	f.awaitMutation(t, f.worktree, r)
}

func TestByteEditAuthorityKeepsTargetGuardsAndExcludedShapes(t *testing.T) {
	for _, forceUnavailable := range []bool{false, true} {
		name := "actual HEAD capability"
		if forceUnavailable {
			name = "unavailable HEAD evidence"
		}
		t.Run(name, func(t *testing.T) { testByteEditTargetGuardsAndExcludedShapes(t, forceUnavailable) })
	}
}

func testByteEditTargetGuardsAndExcludedShapes(t *testing.T, forceUnavailable bool) {
	f := newRealCheckoutMutationFixture(t)
	byteEditFixtureHeadProof(t, f, forceUnavailable)
	original, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	args := func() map[string]any {
		return map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func EditedByte() {}", "base_sha": gitBlobSHA(original), "expected_occurrences": 1, "format": "json"}
	}
	for _, kind := range []string{"sha", "occurrence", "parse", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			a := args()
			switch kind {
			case "sha":
				a["base_sha"] = "0000000000000000000000000000000000000000"
			case "occurrence":
				a["expected_occurrences"] = 2
			case "parse":
				a["new_string"] = "func Broken("
			case "foreign":
				a["path"] = filepath.Join(f.primary, "edit.go")
			}
			r := f.edit(t, f.worktree, a)
			require.True(t, r.IsError, viewResultText(t, r))
			disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, original, disk)
		})
	}
	for _, kind := range []string{"no_sha", "dry_run", "symbol", "batch", "fresh", "capability"} {
		t.Run(kind, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "edit_file"
			a := args()
			req.Params.Arguments = a
			freshness := requestFreshness{}
			caps := capabilityRequest{}
			switch kind {
			case "no_sha":
				delete(a, "base_sha")
			case "dry_run":
				a["dry_run"] = true
			case "symbol":
				req.Params.Name = "edit_symbol"
			case "batch":
				req.Params.Name = "batch_edit"
			case "fresh":
				freshness.requireFresh = true
			case "capability":
				caps.required = []graphview.CapabilityID{graphview.CapSyntaxGraph}
			}
			_, eligible := f.srv.byteEditRequest(&req, freshness, caps)
			require.False(t, eligible)
		})
	}
}

func TestByteEditFacadeUsesNormalizedGuardAndSelectedDisk(t *testing.T) {
	for _, forceUnavailable := range []bool{false, true} {
		name := "actual HEAD capability"
		if forceUnavailable {
			name = "unavailable HEAD evidence"
		}
		t.Run(name, func(t *testing.T) { testByteEditFacadeUsesNormalizedGuardAndSelectedDisk(t, forceUnavailable) })
	}
}

func testByteEditFacadeUsesNormalizedGuardAndSelectedDisk(t *testing.T, forceUnavailable bool) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	byteEditFixtureHeadProof(t, f, forceUnavailable)
	before, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	primary, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	// The ready grant owns ordinary checked mutation authority without
	// materializing a graph reader. No later whole-view admission is needed.
	priorHook := mutationBeforeAdmission
	defer func() { mutationBeforeAdmission = priorHook }()
	materialized := false
	mutationBeforeAdmission = func(context.Context) { materialized = true }
	r := f.facade(t, f.primary, "edit", map[string]any{"operation": "file", "target": map[string]any{"file": "repo/edit.go"}, "match": "func New() {}", "replacement": "func ByteFacade() {}", "guard": map[string]any{"base_sha": gitBlobSHA(before), "expected_occurrences": 1}, "view": map[string]any{"kind": "worktree", "path": f.worktree}, "format": "json"})
	require.False(t, r.IsError, viewResultText(t, r))
	fresh := resultFreshness(t, r)
	require.False(t, materialized, "ready file grant must not re-enter materialized admission")
	require.Equal(t, "file", fresh["freshness_scope"])
	require.Equal(t, "pending", fresh["graph_freshness"])
	require.Equal(t, f.checkoutID, fresh["checkout_id"])
	disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(disk), "ByteFacade")
	disk, err = os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primary, disk)
	f.awaitMutation(t, f.primary, r)
}

func TestRejectedByteEditResumesEarlierPendingPublication(t *testing.T) {
	for _, forceUnavailable := range []bool{false, true} {
		name := "actual HEAD capability"
		if forceUnavailable {
			name = "unavailable HEAD evidence"
		}
		t.Run(name, func(t *testing.T) { testRejectedByteEditResumesEarlierPendingPublication(t, forceUnavailable) })
	}
}

func testRejectedByteEditResumesEarlierPendingPublication(t *testing.T, forceUnavailable bool) {
	f, blocked := newBlockedLifecycleFixture(t)
	byteEditFixtureHeadProof(t, f, forceUnavailable)
	prior := f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}", "format": "json"})
	require.False(t, prior.IsError, viewResultText(t, prior))
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("earlier publication never reached barrier")
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": "repo/edit.go", "old_string": "LifecycleBlocked", "new_string": "MustNotWrite", "base_sha": "0000000000000000000000000000000000000000", "expected_occurrences": 1, "format": "json"}
	ctx := WithSessionCWD(WithSessionID(t.Context(), "rejected-byte-handoff"), f.worktree)
	result := make(chan *mcplib.CallToolResult, 1)
	go func() {
		r, err := f.srv.wrapToolHandler(func(ctx context.Context, r mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if !byteEditFixtureViewMatches(ctx, f) {
				return mcplib.NewToolResultError("wrong rejected-edit authority for actual HEAD capability"), nil
			}
			return f.srv.handleEditFile(ctx, r)
		})(ctx, req)
		if err != nil {
			result <- mcplib.NewToolResultError(err.Error())
			return
		}
		result <- r
	}()
	blocked.unblock()
	var r *mcplib.CallToolResult
	select {
	case r = <-result:
	case <-time.After(20 * time.Second):
		t.Fatal("rejected byte request did not close")
	}
	require.True(t, r.IsError)
	require.Contains(t, viewResultText(t, r), "base_sha mismatch")
	disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(disk), "LifecycleBlocked")
	require.NotContains(t, string(disk), "MustNotWrite")
	f.awaitMutation(t, f.worktree, prior)
}

func TestByteEditReselectionRefusesDeadCallerOtherErrorsAndAdmittedLease(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	deadline, stop := context.WithDeadline(t.Context(), time.Time{})
	defer stop()
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		mutation *indexer.CheckoutMutation
		err      error
		want     bool
	}{
		{name: "wrapped route motion before admission", ctx: t.Context(), err: fmt.Errorf("catalog: %w", indexer.ErrCheckoutMutationRouteMoved), want: true},
		{name: "canceled caller", ctx: canceled, err: indexer.ErrCheckoutMutationRouteMoved},
		{name: "expired deadline", ctx: deadline, err: indexer.ErrCheckoutMutationRouteMoved},
		{name: "generic stale", ctx: t.Context(), err: indexer.ErrCheckoutMutationStale},
		{name: "wrapped disk motion before admission", ctx: t.Context(), err: fmt.Errorf("sample: %w", indexer.ErrCheckoutMutationDiskMoved), want: true},
		{name: "disk motion with canceled caller", ctx: canceled, err: indexer.ErrCheckoutMutationDiskMoved},
		{name: "disk motion with expired caller", ctx: deadline, err: indexer.ErrCheckoutMutationDiskMoved},
		{name: "disk motion after admission", ctx: t.Context(), mutation: &indexer.CheckoutMutation{}, err: indexer.ErrCheckoutMutationDiskMoved},
		{name: "permanent failure", ctx: t.Context(), err: errors.New("catalog read failure")},
		{name: "cancellation", ctx: t.Context(), err: context.Canceled},
		{name: "deadline error", ctx: t.Context(), err: context.DeadlineExceeded},
		{name: "admitted lease", ctx: t.Context(), mutation: &indexer.CheckoutMutation{}, err: indexer.ErrCheckoutMutationRouteMoved},
		{name: "successful admission", ctx: t.Context(), mutation: &indexer.CheckoutMutation{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, byteEditAdmissionCanReselect(tc.ctx, tc.mutation, tc.err))
		})
	}
}

// Without optional HEAD evidence, admission must either recompose the unrelated
// save or use a route a concurrent real coordinator poll already published.
// Exact DiskMoved refusal and its cleanup are proved by the parked indexer test;
// this public integration retains both legitimate authority branches.
func TestGuardedReadyByteEditAfterUnrelatedSaveUsesCheckedAuthority(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	byteEditFixtureHeadProof(t, f, true)
	target := filepath.Join(f.worktree, "edit.go")
	original, err := os.ReadFile(target)
	require.NoError(t, err)
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	unrelated := []byte("package repo\nfunc ConcurrentBeforeAdmission() {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "concurrent.go"), unrelated, 0600))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func AfterSampledMotion() {}", "base_sha": gitBlobSHA(original), "expected_occurrences": 1, "format": "json"}
	_, eligible := f.srv.byteEditRequest(&req, requestFreshness{}, capabilityRequest{})
	require.True(t, eligible)
	ordinaryAdmissions := 0
	previous := mutationBeforeAdmission
	mutationBeforeAdmission = func(ctx context.Context) {
		ordinaryAdmissions++
		view := requestViewFromContext(ctx)
		require.NotNil(t, view)
		require.NotNil(t, view.materialized)
		require.Empty(t, view.sourceScope)
		require.Nil(t, checkoutMutationFromContext(ctx))
	}
	t.Cleanup(func() { mutationBeforeAdmission = previous })
	ctx := WithSessionCWD(WithSessionID(t.Context(), "ready-sampled-disk-motion"), f.worktree)
	handlerCalls := 0
	result, err := f.srv.wrapToolHandler(func(handlerCtx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		handlerCalls++
		require.True(t, byteEditFixtureViewMatches(handlerCtx, f))
		view := requestViewFromContext(handlerCtx)
		if view.sourceScope == "file" {
			require.Zero(t, ordinaryAdmissions, "the ready grant never re-enters materialized admission")
			require.Nil(t, view.materialized)
			require.True(t, view.sourceGraphPending)
		} else {
			require.Positive(t, ordinaryAdmissions, "fallback uses original materialized admission")
			require.NotNil(t, view.materialized)
		}
		return f.srv.handleEditFile(handlerCtx, request)
	})(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, viewResultText(t, result))
	require.Equal(t, 1, handlerCalls, "pre-admission reselection must never replay the handler")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(data), "func AfterSampledMotion() {}")
	data, err = os.ReadFile(filepath.Join(f.worktree, "concurrent.go"))
	require.NoError(t, err)
	require.Equal(t, unrelated, data)
	data, err = os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primaryBefore, data)
	f.awaitMutation(t, f.worktree, result)
}
