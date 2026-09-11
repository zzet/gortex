package mcp

import (
	"context"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// W5.3. The materializer leases the derived generations of a routed stack, and
// the ancestry walk terminates at generation zero rather than including it — so
// the one layer under every composed reader, and the corpus at index zero of
// every routed content search, was the one nothing held. These tests drive the
// real tool middleware (wrapToolHandler, through callWithView) rather than the
// primitives, because the claim is about a request's lifetime.

// viewTestOwner is the dedicated owner of the fixture's repository: the
// registration a base pin finds when it looks up the view's repo prefix.
func viewTestOwner() graphview.RepositoryOwner {
	return graphview.RepositoryOwner{
		GraphID:     "repo",
		CheckoutID:  viewTestPrimary,
		Incarnation: "inc-primary",
		RepoPrefix:  "repo",
	}
}

func TestRoutedRequestPinsTheBaseCorpusForItsLifetime(t *testing.T) {
	stack := newViewStack(t)
	if stack.leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("the base corpus is pinned before any request ran")
	}

	var (
		pinnedDuring  bool
		witnessedView *requestView
	)
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			if !hasNode(stack.srv.readerFor(hctx), "repo/added.go::Fresh") {
				t.Error("the request did not read through the routed view")
			}
			pinnedDuring = stack.leases.InUse(graphview.BaseCorpusGeneration)
			witnessedView = requestViewFromContext(hctx)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !pinnedDuring {
		t.Fatal("a routed request ran with the base corpus unpinned")
	}
	if witnessedView == nil || witnessedView.basePin == nil {
		t.Fatal("the routed request carried no base pin")
	}
	if !witnessedView.readsBaseCorpus {
		t.Fatal("bindSources did not record the base corpus as a source of the routed view")
	}
	if stack.leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("the request ended with the base corpus still pinned")
	}
}

// TestBaseCorpusOwnerCleanupWaitsForTheRequest is the lifetime claim on the
// production path: closing the repository owner's admission while a routed
// request is reading its corpus must drain behind that request, not through it.
func TestBaseCorpusOwnerCleanupWaitsForTheRequest(t *testing.T) {
	stack := newViewStack(t)
	owner := viewTestOwner()
	if err := stack.leases.RegisterRepositoryOwner(owner); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}

	var drain *graphview.RepositoryDrain
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(hctx)
			if view == nil || !view.basePin.OwnerPinned() {
				t.Error("the routed request did not pin the registered owner of its base corpus")
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			var closeErr error
			drain, closeErr = stack.leases.CloseRepositoryAdmission(owner)
			if closeErr != nil {
				t.Errorf("CloseRepositoryAdmission: %v", closeErr)
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			select {
			case <-drain.Done():
				t.Error("the owner drained while a request was still reading its base corpus")
			default:
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if drain == nil {
		t.Fatal("no drain was opened")
	}
	select {
	case <-drain.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the owner never drained after the request released its base pin")
	}
}

// TestBaseCorpusMutationMidRequestIsLabelledOnTheRider is the truthfulness
// claim. A pin protects lifetime, not bytes: when the corpus the request read
// moves while it is reading, the answer may be stitched from two states of the
// world, and the rider must stop claiming the route was served exactly.
func TestBaseCorpusMutationMidRequestIsLabelledOnTheRider(t *testing.T) {
	stack := newViewStack(t)
	reg, err := stack.leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{
		RepoPrefix:   "repo",
		RootIdentity: stack.repoRoot,
		Incarnation:  "inc-raw",
	}, nil)
	if err != nil {
		t.Fatalf("RegisterRawRepositoryOwnerPrepared: %v", err)
	}
	if _, err := stack.leases.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a"); err != nil {
		t.Fatalf("CaptureInitialRawRepositorySource: %v", err)
	}

	// Control arm: the same routed request with nothing moving underneath it
	// still reports an exact view, so the downgrade below is the mutation's
	// doing and not the pin's presence.
	steady, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("steady call: %v", err)
	}
	if rider := resultFreshness(t, steady); rider["exact"] != true || rider["base_changed"] != nil {
		t.Fatalf("an undisturbed routed request reported %v, want exact with no base change", rider)
	}

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(hctx)
			if view == nil || !view.basePin.Witnessed() {
				t.Error("the routed request did not witness the base corpus source")
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			// The base corpus is re-derived under the reader. The request keeps
			// answering from what it already composed — that is what makes the
			// label, rather than a refusal, the honest outcome.
			write, mutErr := stack.leases.AcquireRawRepositoryMutation(context.Background(), reg)
			if mutErr != nil {
				t.Errorf("AcquireRawRepositoryMutation: %v", mutErr)
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			if err := write.Complete("source-b"); err != nil {
				t.Errorf("Complete: %v", err)
			}
			write.Release()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatal("the response carries no freshness rider")
	}
	if rider["exact"] != false {
		t.Errorf("rider exact = %v, want false after the base corpus moved", rider["exact"])
	}
	if rider["fallback_reason"] != baseChangedFallbackReason {
		t.Errorf("rider fallback_reason = %v, want %q", rider["fallback_reason"], baseChangedFallbackReason)
	}
	if rider["base_changed"] != true {
		t.Errorf("rider base_changed = %v, want true", rider["base_changed"])
	}
	// The route is still named: a client must be able to see which stack it
	// read, not merely that something was wrong.
	if rider["actual_view"] == nil || rider["actual_view"] == "" {
		t.Errorf("rider dropped actual_view: %v", rider)
	}
}

// TestUnwitnessedBaseCorpusNeverClaimsAChange guards the other direction. Most
// repositories carry no source revision authority today, so the pin can only
// say "unknown" — and an unknown must not be rendered as a change, which would
// make every routed answer inexact on no evidence at all.
func TestUnwitnessedBaseCorpusNeverClaimsAChange(t *testing.T) {
	stack := newViewStack(t)
	if err := stack.leases.RegisterRepositoryOwner(viewTestOwner()); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(hctx)
			if view == nil || view.basePin == nil {
				t.Error("the routed request carried no base pin")
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			if view.basePin.Witnessed() {
				t.Error("a dedicated owner reported a source witness it does not have")
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	rider := resultFreshness(t, res)
	if rider["exact"] != true {
		t.Errorf("rider exact = %v, want true: nothing was observed to change", rider["exact"])
	}
	if rider["base_changed"] != nil {
		t.Errorf("rider base_changed = %v, want absent", rider["base_changed"])
	}
}
