package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The request view's lease dies at `defer view.close()` when the tool handler
// returns (TestRoutedViewLeaseIsHeldForTheRequestOnly pins that). Three
// production paths deliberately carry work past that point on a
// context.WithoutCancel, and the deadline firewall leaves a whole handler
// running after it has answered. Each of those must now hold the payload it
// is still working against through a joined lease — the tests below drive the
// production entrypoints, not the primitive.

// handoffCounter reads one series of the process-wide view registry.
func handoffCounter(t *testing.T, series, consumer, outcome string) int64 {
	t.Helper()
	key := series + "{consumer=" + consumer + "}"
	if outcome != "" {
		key = fmt.Sprintf("%s{consumer=%s,outcome=%s}", series, consumer, outcome)
	}
	snapshot := viewmetrics.Read()
	if series == viewmetrics.HandoffsOutstanding {
		return snapshot.Gauges[key]
	}
	return snapshot.Counters[key]
}

func assertOutstandingHandoffs(t *testing.T, consumer string, want int64) {
	t.Helper()
	if got := handoffCounter(t, viewmetrics.HandoffsOutstanding, consumer, ""); got != want {
		t.Fatalf("outstanding %s handoffs = %d, want %d", consumer, got, want)
	}
}

func waitOutstandingHandoffs(t *testing.T, consumer string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if handoffCounter(t, viewmetrics.HandoffsOutstanding, consumer, "") == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("outstanding %s handoffs = %d, want %d",
		consumer, handoffCounter(t, viewmetrics.HandoffsOutstanding, consumer, ""), want)
}

// dropRoute removes the route so retirement is deciding on the lease alone:
// a generation a live route still names is refused for that reason instead.
func (v *viewStack) dropRoute(t *testing.T) {
	t.Helper()
	if err := v.store.Catalog().DeleteCheckoutRoute(context.Background(), viewTestWorktree); err != nil {
		t.Fatalf("DeleteCheckoutRoute: %v", err)
	}
}

func (v *viewStack) retireDirty(t *testing.T) error {
	t.Helper()
	return v.store.RetirePayloadGeneration(context.Background(), v.dirty, v.leases.InUse)
}

// waitDrainBounded asks the lease manager to drain the stack's generations
// with a short budget, the way a retirement sweep does.
func (v *viewStack) waitDrainBounded(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	return v.leases.WaitDrain(ctx, v.commit, v.dirty)
}

// TestDetachedWorkerKeepsReadingAfterTheRequestReturned is the shape every
// call site below inherits: work started inside a handler, carried past the
// handler's return, still reads a live pinned stack, and retirement waits for
// it rather than collecting the payload under it.
func TestDetachedWorkerKeepsReadingAfterTheRequestReturned(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)

	worker := make(chan struct{})
	workerDone := make(chan struct{})
	var pin *requestViewPin

	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			pin = handoffRequestView(hctx, viewmetrics.HandoffRepositoryIndex)
			if pin == nil {
				return nil, errors.New("the request materialized no view to hand off")
			}
			go func() {
				defer close(workerDone)
				defer pin.release()
				<-worker
			}()
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}

	// The request has returned and its own lease is released; only the
	// detached worker's joined hold is left.
	assertOutstandingHandoffs(t, viewmetrics.HandoffRepositoryIndex, 1)
	if !stack.leases.InUse(stack.dirty) {
		t.Fatal("the detached worker is running over an unpinned generation")
	}
	if !hasNode(pin.reader(), "repo/added.go::Fresh") {
		t.Fatal("the handed-off reader lost the commit layer's own content")
	}
	if !hasNode(pin.reader(), "repo/keep.go::Dirty") {
		t.Fatal("the handed-off reader lost the dirty layer's own content")
	}
	stack.dropRoute(t)
	if err := stack.retireDirty(t); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while a detached worker reads = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}
	if err := stack.waitDrainBounded(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while a detached worker reads = %v, want %v", err, context.DeadlineExceeded)
	}

	close(worker)
	<-workerDone
	assertOutstandingHandoffs(t, viewmetrics.HandoffRepositoryIndex, 0)
	if err := stack.waitDrainBounded(t); err != nil {
		t.Fatalf("WaitDrain after the worker finished: %v", err)
	}
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after the worker finished: %v", err)
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffRepositoryIndex, viewmetrics.HandoffJoined); got != 1 {
		t.Fatalf("joined handoffs = %d, want 1", got)
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffRepositoryIndex, viewmetrics.HandoffRefused); got != 0 {
		t.Fatalf("refused handoffs = %d, want 0", got)
	}
}

// TestHandoffIsRefusedOnceTheRequestEnded pins the refusal half: a worker that
// asks for a pin after its request is over gets nil — never a handle over a
// payload that may already be gone — and the refusal is counted.
func TestHandoffIsRefusedOnceTheRequestEnded(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)

	var escaped context.Context
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			escaped = context.WithoutCancel(hctx)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if stack.leases.InUse(stack.dirty) {
		t.Fatal("the request ended with its lease still held")
	}
	if pin := handoffRequestView(escaped, viewmetrics.HandoffFileMutation); pin != nil {
		t.Fatal("a drained lease handed out a pin that pins nothing")
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffFileMutation, viewmetrics.HandoffRefused); got != 1 {
		t.Fatalf("refused handoffs = %d, want 1", got)
	}
	assertOutstandingHandoffs(t, viewmetrics.HandoffFileMutation, 0)
}

// TestUnroutedRequestHandsOffNothing keeps the base-corpus path free: a
// request that materialized no generation stack has no lifetime to extend, so
// it records neither a join nor a refusal.
func TestUnroutedRequestHandsOffNothing(t *testing.T) {
	viewmetrics.Reset()
	if pin := handoffRequestView(context.Background(), viewmetrics.HandoffFileMutation); pin != nil {
		t.Fatal("a request with no view handed out a pin")
	}
	pin := handoffRequestView(withRequestView(context.Background(), &requestView{}), viewmetrics.HandoffFileMutation)
	if pin != nil {
		t.Fatal("a reader-less fallback view handed out a pin")
	}
	// Nil-safe on every method, so no call site needs a branch.
	pin.release()
	if got := pin.generations(); got != nil {
		t.Fatalf("a nil pin names generations %v", got)
	}
	if got := pin.reader(); got != nil {
		t.Fatal("a nil pin hands out a reader")
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffFileMutation, viewmetrics.HandoffJoined); got != 0 {
		t.Fatalf("joined handoffs = %d, want 0", got)
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffFileMutation, viewmetrics.HandoffRefused); got != 0 {
		t.Fatalf("refused handoffs = %d, want 0", got)
	}
}

// TestCheckoutRefreshAdmissionPinsTheViewUntilPublication drives the
// production path: an approved worktree source mutation commits to disk,
// admits publication on the checkout coordinator (which runs long after this
// handler returns), and must keep the payload it was admitted against alive
// until that ticket reports.
func TestCheckoutRefreshAdmissionPinsTheViewUntilPublication(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	mutation, done, path := newReceiptCheckoutMutation(t)

	var outcome mutationReindexOutcome
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			if view := requestViewFromContext(hctx); view == nil || view.materialized == nil {
				return nil, errors.New("the request did not materialize a routed view")
			}
			outcome = stack.srv.mutationReindexState(
				withCheckoutMutation(hctx, mutation, filepath.Dir(path)), path)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !outcome.Pending || outcome.Err != nil || outcome.Receipt == "" {
		t.Fatalf("publication was not admitted as pending: %+v", outcome)
	}

	// The handler has returned; the admitted publication is what holds the
	// payload now.
	assertOutstandingHandoffs(t, viewmetrics.HandoffCheckoutRefresh, 1)
	stack.dropRoute(t)
	if err := stack.retireDirty(t); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while publication is outstanding = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}
	if err := stack.waitDrainBounded(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDrain while publication is outstanding = %v, want %v", err, context.DeadlineExceeded)
	}

	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 29, Reindexed: true}
	waitCheckoutReceipt(t, stack.srv, outcome.Receipt)
	// The pin is dropped before the receipt closes, so a caller that waited
	// on the receipt observes a drained lease rather than racing for it.
	assertOutstandingHandoffs(t, viewmetrics.HandoffCheckoutRefresh, 0)
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after publication reported: %v", err)
	}
}

// TestRejectedCheckoutRefreshAdmissionReleasesItsPin keeps the pin bounded:
// an admission that yields no completion ticket has nothing that will ever
// report, so holding the payload for it would pin a generation forever.
func TestRejectedCheckoutRefreshAdmissionReleasesItsPin(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	mutation, _, path := newReceiptCheckoutMutation(t)
	mutation.ticket.Ticket.Done = nil

	var outcome mutationReindexOutcome
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			outcome = stack.srv.mutationReindexState(
				withCheckoutMutation(hctx, mutation, filepath.Dir(path)), path)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if outcome.Err == nil {
		t.Fatalf("a ticketless admission was accepted: %+v", outcome)
	}
	assertOutstandingHandoffs(t, viewmetrics.HandoffCheckoutRefresh, 0)
	stack.dropRoute(t)
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after a rejected admission: %v", err)
	}
}

// TestScheduledFileMutationPinsTheViewUntilTheTicketReports is the watcher
// half of the same contract: EnqueueFileMutation is admitted on a detached
// context and the reindex runs on the watcher's own loop.
func TestScheduledFileMutationPinsTheViewUntilTheTicketReports(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	done := make(chan indexer.MutationResult, 1)
	stack.srv.SetWatcher(mutationTestWatcher{done: done, generation: 11})
	stack.srv.mutationReindexWait = time.Millisecond

	var outcome mutationReindexOutcome
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			outcome = stack.srv.mutationReindexState(hctx, filepath.Join(t.TempDir(), "edit.go"))
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !outcome.Pending || outcome.Err != nil {
		t.Fatalf("the scheduled reindex did not stay pending: %+v", outcome)
	}

	assertOutstandingHandoffs(t, viewmetrics.HandoffFileMutation, 1)
	stack.dropRoute(t)
	if err := stack.retireDirty(t); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the reindex is outstanding = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	done <- indexer.MutationResult{RequestedGeneration: 11, AppliedGeneration: 11, Reindexed: true}
	close(done)
	waitOutstandingHandoffs(t, viewmetrics.HandoffFileMutation, 0)
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after the reindex reported: %v", err)
	}
}

// TestTrackRepositoryPinsTheViewForItsDetachedIndex traces the third detach.
// handleTrackRepository answers `accepted` and leaves the first index running
// on a context.WithoutCancel; the counters prove the production handler — not
// a test-local copy of it — reaches the handoff and releases it.
func TestTrackRepositoryPinsTheViewForItsDetachedIndex(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "track_repository"
	req.Params.Arguments = map[string]any{"path": stack.otherRoot}

	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			// An already-spent deadline makes the handler answer `accepted`
			// while its index keeps running, which is the state this item is
			// about. WithoutCancel drops the deadline for the detached work.
			deadlined, cancel := context.WithDeadline(hctx, time.Now().Add(-time.Second))
			defer cancel()
			return stack.srv.handleTrackRepository(deadlined, req)
		}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffRepositoryIndex, viewmetrics.HandoffJoined); got != 1 {
		t.Fatalf("the detached first index took %d view handoffs, want 1", got)
	}
	waitOutstandingHandoffs(t, viewmetrics.HandoffRepositoryIndex, 0)
	stack.dropRoute(t)
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after the detached index finished: %v", err)
	}
}

// TestAbandonedHandlerRetainsAndNamesItsView is the deadline firewall's half.
// The handler is still running and still reading its view when the firewall
// answers, so the frame joins the lease for it, says so, and the pin is
// released only when that goroutine actually exits.
func TestAbandonedHandlerRetainsAndNamesItsView(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	stack.srv.ToolCallTimeout = 150 * time.Millisecond

	release := make(chan struct{})
	handlerDone := make(chan struct{})
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			defer close(handlerDone)
			<-release
			if !hasNode(stack.srv.readerFor(hctx), "repo/added.go::Fresh") {
				return nil, errors.New("the abandoned handler lost its view")
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, "abandoned") {
		t.Fatalf("the deadline did not abandon the handler: %s", text)
	}
	if !strings.Contains(text, "still holding the view it read") {
		t.Fatalf("the abandoned answer does not report its retained lease: %s", text)
	}
	named := retainedGenerationsNamed(t, text)
	for _, id := range []int64{stack.commit, stack.dirty} {
		if !named[id] {
			t.Fatalf("the abandoned answer does not name retained generation %d: %s", id, text)
		}
	}
	if len(named) != 2 {
		t.Fatalf("the abandoned answer names %d retained generations, want the view's 2: %s", len(named), text)
	}

	// Still held while the handler runs, and retirement still refuses.
	assertOutstandingHandoffs(t, viewmetrics.HandoffAbandonedHandler, 1)
	stack.dropRoute(t)
	if err := stack.retireDirty(t); !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while a handler is abandoned = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}

	close(release)
	<-handlerDone
	waitOutstandingHandoffs(t, viewmetrics.HandoffAbandonedHandler, 0)
	if err := stack.retireDirty(t); err != nil {
		t.Fatalf("retire after the abandoned handler exited: %v", err)
	}
}

// TestCompletedHandlerRetainsNothing keeps the firewall's normal path free of
// pins: a call that answers inside its budget hands off nothing at all.
func TestCompletedHandlerRetainsNothing(t *testing.T) {
	viewmetrics.Reset()
	stack := newViewStack(t)
	stack.srv.ToolCallTimeout = 5 * time.Second

	var reader graph.Reader
	if _, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil,
		captureReader(stack.srv, &reader)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Fatal("the request did not read through the routed view")
	}
	if got := handoffCounter(t, viewmetrics.HandoffTotal, viewmetrics.HandoffAbandonedHandler, viewmetrics.HandoffJoined); got != 0 {
		t.Fatalf("a completed handler took %d view handoffs, want 0", got)
	}
	assertOutstandingHandoffs(t, viewmetrics.HandoffAbandonedHandler, 0)
	if stack.leases.InUse(stack.dirty) {
		t.Fatal("a completed request left its lease held")
	}
}

// TestRetainedLeaseNoteIsSilentWithoutARetainedView pins the message contract
// for every call that pinned nothing — the overwhelming majority.
func TestRetainedLeaseNoteIsSilentWithoutARetainedView(t *testing.T) {
	if got := retainedLeaseNote(nil); got != "" {
		t.Fatalf("retainedLeaseNote(nil) = %q, want the empty string", got)
	}
	if got := abandonedMessage("resource", "gortex://report", time.Second) + retainedLeaseNote(nil); strings.Contains(got, "still holding") {
		t.Fatalf("an unpinned resource read claims a retained lease: %s", got)
	}
}

// retainedGenerationsNamed parses the generation ids out of the retained-lease
// sentence, so the assertion is on the ids themselves rather than on a
// substring that any number in the message would satisfy.
func retainedGenerationsNamed(t *testing.T, message string) map[int64]bool {
	t.Helper()
	lead := "payload generations "
	tail := " stay pinned"
	start := strings.Index(message, lead)
	if start < 0 {
		lead, tail = "payload generation ", " stays pinned"
		start = strings.Index(message, lead)
	}
	if start < 0 {
		t.Fatalf("no retained-lease sentence in %q", message)
	}
	rest := message[start+len(lead):]
	end := strings.Index(rest, tail)
	if end < 0 {
		t.Fatalf("unterminated retained-lease sentence in %q", message)
	}
	out := map[int64]bool{}
	for _, field := range strings.Split(rest[:end], ", ") {
		id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			t.Fatalf("retained-lease sentence carries a non-id %q: %v", field, err)
		}
		out[id] = true
	}
	return out
}
