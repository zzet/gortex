package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func exactCheckoutMutationResult(t *testing.T, result *mcplib.CallToolResult, checkoutID string) map[string]any {
	t.Helper()
	if result == nil {
		t.Fatal("checkout mutation returned no result")
	}
	if result.IsError {
		t.Fatalf("checkout mutation was refused: %s", singleTextOrFail(t, result))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(singleTextOrFail(t, result)), &payload); err != nil {
		t.Fatalf("decode checkout mutation result: %v", err)
	}
	routePayload := payload
	if _, ok := routePayload["checkout_id"]; !ok {
		if files, _ := payload["files"].([]any); len(files) > 0 {
			if nested, ok := files[0].(map[string]any); ok {
				routePayload = nested
			}
		}
	}
	if got, _ := routePayload["checkout_id"].(string); got != checkoutID {
		t.Fatalf("checkout_id = %q, want %q; payload=%v", got, checkoutID, payload)
	}
	if got, _ := routePayload["graph_status"].(string); got != mutationGraphFresh {
		t.Fatalf("graph_status = %q, want %q; payload=%v", got, mutationGraphFresh, payload)
	}
	if epoch, _ := routePayload["published_route_epoch"].(float64); epoch <= 0 {
		t.Fatalf("published_route_epoch is missing: %v", payload)
	}
	return payload
}

func requireSelectedCheckoutRouteMoved(
	t *testing.T,
	stack *worktreeSearchStack,
	previous int64,
) {
	t.Helper()
	stack.awaitDirtyGenerationAfter(t, previous)
	if current := stack.dirtyGeneration(t); current == previous {
		t.Fatalf("selected checkout route kept dirty generation %d after mutation", previous)
	}
}

func TestReadyWorktreeMutationsPublishOnlyTheSelectedCheckout(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	// These tiny fixture builds normally complete well inside the production
	// three-second bound. Give slower race/CI machines enough room so the test
	// checks exact publication rather than the separately-tested pending path.
	stack.srv.mutationReindexWait = 15 * time.Second
	if !stack.lifecycle.CheckoutMutationReady(stack.checkoutID, stack.worktree) {
		t.Fatal("ready routed worktree has no exact mutation publisher")
	}

	primaryKeep := filepath.Join(stack.primary, "keep.go")
	worktreeKeep := filepath.Join(stack.worktree, "keep.go")
	primaryBefore, err := os.ReadFile(primaryKeep)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}

	t.Run("edit_file", func(t *testing.T) {
		previous := stack.dirtyGeneration(t)
		result := stack.callFrom(t, stack.worktree, "edit_file", map[string]any{
			"path":       "keep.go",
			"old_string": "func Keeper() {}",
			"new_string": "func WorktreeKeeper() {}",
		}, stack.srv.handleEditFile)
		exactCheckoutMutationResult(t, result, stack.checkoutID)
		requireSelectedCheckoutRouteMoved(t, stack, previous)

		worktreeBytes, readErr := os.ReadFile(worktreeKeep)
		if readErr != nil || !strings.Contains(string(worktreeBytes), "WorktreeKeeper") {
			t.Fatalf("selected worktree was not edited: bytes=%q err=%v", worktreeBytes, readErr)
		}
		primaryAfter, readErr := os.ReadFile(primaryKeep)
		if readErr != nil || string(primaryAfter) != string(primaryBefore) {
			t.Fatalf("worktree edit changed primary bytes: before=%q after=%q err=%v", primaryBefore, primaryAfter, readErr)
		}
	})

	t.Run("write_file", func(t *testing.T) {
		previous := stack.dirtyGeneration(t)
		result := stack.callFrom(t, stack.worktree, "write_file", map[string]any{
			"path":    refTestPrefix + "/created.go",
			"content": "package repo\n\nfunc Created() {}\n",
		}, stack.srv.handleWriteFile)
		exactCheckoutMutationResult(t, result, stack.checkoutID)
		requireSelectedCheckoutRouteMoved(t, stack, previous)

		if bytes, readErr := os.ReadFile(filepath.Join(stack.worktree, "created.go")); readErr != nil ||
			!strings.Contains(string(bytes), "func Created()") {
			t.Fatalf("selected worktree file was not created: bytes=%q err=%v", bytes, readErr)
		}
		if _, statErr := os.Stat(filepath.Join(stack.primary, "created.go")); !os.IsNotExist(statErr) {
			t.Fatalf("write_file created a primary file: %v", statErr)
		}
	})

	t.Run("edit_symbol", func(t *testing.T) {
		previous := stack.dirtyGeneration(t)
		result := stack.callFrom(t, stack.worktree, "edit_symbol", map[string]any{
			"id":         refTestPrefix + "/created.go::Created",
			"old_source": "func Created() {}",
			"new_source": "func CreatedEdited() {}",
		}, stack.srv.handleEditSymbol)
		exactCheckoutMutationResult(t, result, stack.checkoutID)
		requireSelectedCheckoutRouteMoved(t, stack, previous)

		bytes, readErr := os.ReadFile(filepath.Join(stack.worktree, "created.go"))
		if readErr != nil || !strings.Contains(string(bytes), "func CreatedEdited()") {
			t.Fatalf("selected worktree symbol was not edited: bytes=%q err=%v", bytes, readErr)
		}
		if _, statErr := os.Stat(filepath.Join(stack.primary, "created.go")); !os.IsNotExist(statErr) {
			t.Fatalf("edit_symbol created or changed a primary file: %v", statErr)
		}
	})
}

func TestReadyWorktreeParseErrorReportsSelectedGenerationHealth(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.srv.mutationReindexWait = 15 * time.Second
	if !stack.lifecycle.CheckoutMutationReady(stack.checkoutID, stack.worktree) {
		t.Fatal("ready routed worktree has no exact mutation publisher")
	}

	primaryPath := filepath.Join(stack.primary, "keep.go")
	primaryBefore, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}
	ctx := context.Background()
	catalog := stack.store.Catalog()
	primaryCheckout, found, err := graphview.CheckoutForPath(ctx, catalog, stack.srv.viewFamilies(ctx), stack.primary)
	if err != nil || !found {
		t.Fatalf("resolve primary checkout: found=%v err=%v", found, err)
	}
	primaryRouteBefore, routed, err := catalog.GetCheckoutRoute(ctx, primaryCheckout.CheckoutID)
	if err != nil || !routed {
		t.Fatalf("read primary route: routed=%v err=%v", routed, err)
	}
	selectedBefore := stack.dirtyGeneration(t)

	result := stack.callFrom(t, stack.worktree, "edit_file", map[string]any{
		"path":               "keep.go",
		"old_string":         "func Keeper() {}",
		"new_string":         "func Keeper( {",
		"allow_parse_errors": true,
	}, stack.srv.handleEditFile)
	payload := exactCheckoutMutationResult(t, result, stack.checkoutID)
	health, ok := payload["syntax_health"].(map[string]any)
	if !ok {
		t.Fatalf("completed selected-checkout edit omitted syntax_health: %v", payload)
	}
	if healthy, _ := health["healthy"].(bool); healthy {
		t.Fatalf("syntax_health reported a broken selected file as healthy: %v", health)
	}
	if count, _ := health["parse_errors"].(float64); count <= 0 {
		t.Fatalf("syntax_health parse_errors = %v, want > 0", health["parse_errors"])
	}
	requireSelectedCheckoutRouteMoved(t, stack, selectedBefore)

	selectedBytes, readErr := os.ReadFile(filepath.Join(stack.worktree, "keep.go"))
	if readErr != nil || !strings.Contains(string(selectedBytes), "func Keeper( {") {
		t.Fatalf("selected worktree did not retain the parse-error edit: bytes=%q err=%v", selectedBytes, readErr)
	}
	primaryAfter, readErr := os.ReadFile(primaryPath)
	if readErr != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("selected parse-error edit changed primary bytes: before=%q after=%q err=%v", primaryBefore, primaryAfter, readErr)
	}
	primaryRouteAfter, routed, err := catalog.GetCheckoutRoute(ctx, primaryCheckout.CheckoutID)
	if err != nil || !routed {
		t.Fatalf("re-read primary route: routed=%v err=%v", routed, err)
	}
	if primaryRouteAfter.RouteEpoch != primaryRouteBefore.RouteEpoch ||
		primaryRouteAfter.CommitGenerationID != primaryRouteBefore.CommitGenerationID ||
		primaryRouteAfter.DirtyGenerationID != primaryRouteBefore.DirtyGenerationID ||
		primaryRouteAfter.GraphID != primaryRouteBefore.GraphID {
		t.Fatalf("selected parse-error edit moved primary route: before=%+v after=%+v", primaryRouteBefore, primaryRouteAfter)
	}
	for _, node := range stack.srv.graph.GetFileNodes(refTestPrefix + "/keep.go") {
		if node == nil || node.Kind != "file" || node.Meta == nil {
			continue
		}
		if broken, _ := node.Meta["has_parse_errors"].(bool); broken {
			t.Fatalf("selected parse-error edit contaminated primary graph health: %v", node.Meta)
		}
	}
}

func TestReadyWorktreeRenameSymbolFailsClosedBeforePlanning(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	primaryPath := filepath.Join(stack.primary, "keep.go")
	worktreePath := filepath.Join(stack.worktree, "keep.go")
	primaryBefore, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}
	worktreeBefore, err := os.ReadFile(worktreePath)
	if err != nil {
		t.Fatalf("read worktree keep.go: %v", err)
	}

	req := mcplib.CallToolRequest{}
	req.Params.Name = "rename_symbol"
	req.Params.Arguments = map[string]any{
		"id":       refTestPrefix + "/keep.go::Keeper",
		"new_name": "UnsafeCrossViewRename",
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	result, callErr := stack.srv.wrapToolHandler(stack.srv.handleRenameSymbol)(ctx, req)
	if callErr != nil {
		t.Fatalf("call rename_symbol: %v", callErr)
	}
	if result == nil || !result.IsError || !strings.Contains(toolResultText(result), graphview.CodeViewReadOnly) {
		t.Fatalf("routed rename did not fail closed before planning: %v", result)
	}

	primaryAfter, primaryErr := os.ReadFile(primaryPath)
	worktreeAfter, worktreeErr := os.ReadFile(worktreePath)
	if primaryErr != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("refused rename changed primary: before=%q after=%q err=%v", primaryBefore, primaryAfter, primaryErr)
	}
	if worktreeErr != nil || string(worktreeAfter) != string(worktreeBefore) {
		t.Fatalf("refused rename changed selected checkout: before=%q after=%q err=%v", worktreeBefore, worktreeAfter, worktreeErr)
	}
}

func TestReadyWorktreeMutationRefusesPrimaryAbsolutePath(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	primaryPath := filepath.Join(stack.primary, "keep.go")
	primaryBefore, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{
		"path":       primaryPath,
		"old_string": "func Keeper() {}",
		"new_string": "func Escaped() {}",
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	result, callErr := stack.srv.wrapToolHandler(stack.srv.handleEditFile)(ctx, req)
	if callErr != nil {
		t.Fatalf("call edit_file: %v", callErr)
	}
	if result == nil || !result.IsError {
		t.Fatalf("sibling absolute path was not refused: %v", result)
	}
	primaryAfter, readErr := os.ReadFile(primaryPath)
	if readErr != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("refused sibling absolute mutation changed primary bytes: before=%q after=%q err=%v", primaryBefore, primaryAfter, readErr)
	}
}

func TestReadyWorktreeMutationAttributesSelectedAbsolutePath(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.srv.mutationReindexWait = 15 * time.Second
	worktreePath := filepath.Join(stack.worktree, "keep.go")
	primaryPath := filepath.Join(stack.primary, "keep.go")
	primaryBefore, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}

	previous := stack.dirtyGeneration(t)
	result := stack.callFrom(t, stack.worktree, "edit_file", map[string]any{
		"path":       worktreePath,
		"old_string": "func Keeper() {}",
		"new_string": "func AbsoluteWorktreeKeeper() {}",
	}, stack.srv.handleEditFile)
	payload := exactCheckoutMutationResult(t, result, stack.checkoutID)
	requireSelectedCheckoutRouteMoved(t, stack, previous)
	if got, _ := payload["path"].(string); got != filepath.Join(refTestPrefix, "keep.go") {
		t.Fatalf("absolute selected-checkout path attributed as %q, want graph path %q; payload=%v",
			got, filepath.Join(refTestPrefix, "keep.go"), payload)
	}

	worktreeAfter, readErr := os.ReadFile(worktreePath)
	if readErr != nil || !strings.Contains(string(worktreeAfter), "AbsoluteWorktreeKeeper") {
		t.Fatalf("selected absolute path was not edited: bytes=%q err=%v", worktreeAfter, readErr)
	}
	primaryAfter, readErr := os.ReadFile(primaryPath)
	if readErr != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("selected absolute mutation changed primary bytes: before=%q after=%q err=%v", primaryBefore, primaryAfter, readErr)
	}
}

func TestReadyWorktreeViewRefusesSymlinkIntoPrimary(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	primaryPath := filepath.Join(stack.primary, "keep.go")
	primaryBefore, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatalf("read primary keep.go: %v", err)
	}

	linkPath := filepath.Join(stack.worktree, "primary-link.go")
	if err := os.Symlink(primaryPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	callExpectedRefusal := func(
		t *testing.T,
		tool string,
		args map[string]any,
		handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
	) *mcplib.CallToolResult {
		t.Helper()
		req := mcplib.CallToolRequest{}
		req.Params.Name = tool
		req.Params.Arguments = args
		result, callErr := stack.srv.wrapToolHandler(handler)(ctx, req)
		if callErr != nil {
			t.Fatalf("call %s: %v", tool, callErr)
		}
		return result
	}

	for _, tc := range []struct {
		name    string
		tool    string
		args    map[string]any
		handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
	}{
		{
			name:    "read_file",
			tool:    "read_file",
			args:    map[string]any{"path": linkPath},
			handler: stack.srv.handleReadFile,
		},
		{
			name: "edit_file",
			tool: "edit_file",
			args: map[string]any{
				"path":       linkPath,
				"old_string": "func Keeper() {}",
				"new_string": "func EscapedKeeper() {}",
			},
			handler: stack.srv.handleEditFile,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := callExpectedRefusal(t, tc.tool, tc.args, tc.handler)
			if result == nil || !result.IsError {
				t.Fatalf("symlink into primary was not refused: %v", result)
			}
			if got := toolResultText(result); !strings.Contains(got, "outside the selected checkout root") {
				t.Fatalf("unexpected symlink refusal: %s", got)
			}
		})
	}

	// Symbol mutations resolve graph paths through resolveNodePath rather than
	// resolveFilePath. Replace the routed file with a sibling-targeting link to
	// prove that resolver is held to the same selected-checkout boundary.
	worktreePath := filepath.Join(stack.worktree, "keep.go")
	if err := os.Remove(worktreePath); err != nil {
		t.Fatalf("remove selected worktree file: %v", err)
	}
	if err := os.Symlink(primaryPath, worktreePath); err != nil {
		t.Fatalf("create symbol-path symlink: %v", err)
	}
	result := callExpectedRefusal(t, "edit_symbol", map[string]any{
		"id":         refTestPrefix + "/keep.go::Keeper",
		"old_source": "func Keeper() {}",
		"new_source": "func SymbolEscapedKeeper() {}",
	}, stack.srv.handleEditSymbol)
	if result == nil || !result.IsError {
		t.Fatalf("symbol resolver followed symlink into primary: %v", result)
	}
	if got := toolResultText(result); !strings.Contains(got, "outside the selected checkout root") {
		t.Fatalf("unexpected symbol symlink refusal: %s", got)
	}

	primaryAfter, readErr := os.ReadFile(primaryPath)
	if readErr != nil || string(primaryAfter) != string(primaryBefore) {
		t.Fatalf("refused symlink access changed primary bytes: before=%q after=%q err=%v", primaryBefore, primaryAfter, readErr)
	}
}

func TestReadyDedicatedCheckoutMutationPublishesItsOwnRoute(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.srv.mutationReindexWait = 15 * time.Second
	automatic, found, err := stack.store.Catalog().GetCheckout(context.Background(), stack.checkoutID)
	if err != nil || !found {
		t.Fatalf("read automatic checkout: found=%v err=%v", found, err)
	}
	checkouts, err := stack.store.Catalog().ListCheckouts(context.Background(), automatic.FamilyID)
	if err != nil {
		t.Fatalf("list family checkouts: %v", err)
	}
	primaryID := ""
	for _, checkout := range checkouts {
		if filepath.Clean(checkout.RootPath) == filepath.Clean(stack.primary) {
			primaryID = checkout.CheckoutID
			break
		}
	}
	if primaryID == "" {
		t.Fatal("family has no dedicated primary checkout")
	}
	if !stack.lifecycle.CheckoutMutationReady(primaryID, stack.primary) {
		t.Fatalf("dedicated checkout %q has no exact mutation publisher", primaryID)
	}

	worktreePath := filepath.Join(stack.worktree, "keep.go")
	worktreeBefore, err := os.ReadFile(worktreePath)
	if err != nil {
		t.Fatalf("read automatic worktree keep.go: %v", err)
	}
	result := stack.callFrom(t, stack.primary, "edit_file", map[string]any{
		"path":       "keep.go",
		"old_string": "func Keeper() {}",
		"new_string": "func DedicatedKeeper() {}",
		"view": map[string]any{
			"kind":        "worktree",
			"checkout_id": primaryID,
		},
	}, stack.srv.handleEditFile)
	exactCheckoutMutationResult(t, result, primaryID)

	primaryBytes, readErr := os.ReadFile(filepath.Join(stack.primary, "keep.go"))
	if readErr != nil || !strings.Contains(string(primaryBytes), "DedicatedKeeper") {
		t.Fatalf("dedicated checkout was not edited: bytes=%q err=%v", primaryBytes, readErr)
	}
	worktreeAfter, readErr := os.ReadFile(worktreePath)
	if readErr != nil || string(worktreeAfter) != string(worktreeBefore) {
		t.Fatalf("dedicated edit changed automatic sibling bytes: before=%q after=%q err=%v", worktreeBefore, worktreeAfter, readErr)
	}
}

func TestCheckoutMutationTimeoutAndCancellationReturnRouteBoundPendingReceipt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context func() context.Context
		wait    time.Duration
	}{
		{
			name: "timeout",
			context: func() context.Context {
				return context.Background()
			},
			wait: time.Nanosecond,
		},
		{
			name: "request cancellation",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wait: time.Hour,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan indexer.MutationResult, 1)
			srv := &Server{mutationReindexWait: tc.wait}
			outcome := srv.awaitMutationTicket(tc.context(), &indexer.MutationTicket{
				Path:               "/checkout/file.go",
				Generation:         3,
				CheckoutID:         "checkout-route",
				ObservedRouteEpoch: 9,
				Done:               done,
			})
			if !outcome.Pending || outcome.Receipt == "" || outcome.CheckoutID != "checkout-route" ||
				outcome.ObservedRouteEpoch != 9 {
				t.Fatalf("pending outcome lost route identity: %+v", outcome)
			}

			done <- indexer.MutationResult{
				RequestedGeneration: 3,
				AppliedGeneration:   3,
				CheckoutID:          "checkout-route",
				PublishedRouteEpoch: 10,
				Reindexed:           true,
			}
			close(done)
			deadline := time.Now().Add(time.Second)
			for {
				published, found := srv.mutationReceiptState(outcome.Receipt)
				if found && !published.Pending {
					if published.Err != nil || !published.Reindexed || published.PublishedRouteEpoch != 10 {
						t.Fatalf("pending receipt resolved without publication truth: %+v", published)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("pending checkout mutation receipt did not resolve")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestRoutedMutationRegistryAdmitsOnlyCheckoutAwareDispatches(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	baseCtx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	view, err := stack.srv.resolveRequestView(baseCtx, graphview.Selector{
		Kind:       graphview.SelectorWorktree,
		CheckoutID: stack.checkoutID,
	}, requestViewPolicy{})
	if err != nil {
		t.Fatalf("materialize exact checkout view: %v", err)
	}
	defer view.close()
	viewCtx := withRequestView(baseCtx, view)

	seen := map[string]bool{}
	for _, spec := range facadeOperationSpecs() {
		if !sourceMutatingFacades[spec.Facade] {
			continue
		}
		name := spec.Facade + "/" + spec.Operation
		t.Run(name, func(t *testing.T) {
			allowed := routedCheckoutMutationTools[spec.Legacy]

			// Compact facade dispatch must resolve to the same legacy handler
			// classification as a compatibility call by that handler's name.
			facadeReq := mcplib.CallToolRequest{}
			facadeReq.Params.Name = spec.Facade
			facadeReq.Params.Arguments = map[string]any{"operation": spec.Operation}
			if got := stack.srv.routedMutationLegacy(&facadeReq); allowed && got != spec.Legacy {
				t.Fatalf("checkout-aware facade resolved to %q, want %q", got, spec.Legacy)
			}
			facadeRefusal := stack.srv.refuseRoutedViewMutation(viewCtx, &facadeReq)
			if allowed != (facadeRefusal == nil) {
				t.Fatalf("facade admission mismatch: legacy=%s allowed=%v refusal=%v", spec.Legacy, allowed, facadeRefusal)
			}

			// Exercise the real request middleware for every legacy dispatch.
			// Unsafe handlers must be stopped before the leaf can resolve or
			// cache a canonical path; checkout-aware handlers reach it.
			legacyReq := mcplib.CallToolRequest{}
			legacyReq.Params.Name = spec.Legacy
			legacyReq.Params.Arguments = map[string]any{}
			ran := false
			leaf := func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				ran = true
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			result, callErr := stack.srv.wrapToolHandler(leaf)(baseCtx, legacyReq)
			if callErr != nil {
				t.Fatalf("legacy dispatch: %v", callErr)
			}
			if allowed {
				if !ran || result == nil || result.IsError {
					t.Fatalf("checkout-aware handler was not admitted: ran=%v result=%v", ran, result)
				}
			} else if ran || result == nil || !result.IsError {
				t.Fatalf("unsafe handler reached dispatch: ran=%v result=%v", ran, result)
			}
		})
		seen[spec.Legacy] = true
	}
	for legacy := range routedCheckoutMutationTools {
		if !seen[legacy] {
			t.Errorf("checkout-aware mutation %q is absent from source-mutating facade registry", legacy)
		}
	}
}
