package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type mcpSlotLeaseHolder struct {
	releases int // ReleaseMCPDispatchSlot calls the handler makes on entry
	// ctx and early are written before started closes.
	ctx      context.Context
	early    []bool
	started  chan struct{}
	release  chan struct{}
	unblock  func()
	returned chan struct{}
}

type mcpSlotLeaseFixture struct {
	pool    chan struct{}
	conn    net.Conn
	reader  *bufio.Reader
	holders map[string]*mcpSlotLeaseHolder
}

// startMCPSlotLeaseFixture serves one connection over its own pool with the
// dispatch queue disabled, so a request that finds every slot taken is refused
// at once. Each holder id calls ReleaseMCPDispatchSlot the given number of
// times, publishes its context and results, and blocks until unblocked; every
// other request returns at once.
func startMCPSlotLeaseFixture(t *testing.T, slots int, holders map[string]int) *mcpSlotLeaseFixture {
	t.Helper()
	f := &mcpSlotLeaseFixture{pool: newMCPDispatchSlots(slots), holders: map[string]*mcpSlotLeaseHolder{}}
	for id, releases := range holders {
		release := make(chan struct{})
		holder := &mcpSlotLeaseHolder{
			releases: releases,
			started:  make(chan struct{}),
			release:  release,
			unblock:  sync.OnceFunc(func() { close(release) }),
			returned: make(chan struct{}),
		}
		t.Cleanup(holder.unblock)
		f.holders[id] = holder
	}
	var zero time.Duration
	dispatch := func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		id := string(envelope.ID)
		if holder := f.holders[id]; holder != nil {
			defer close(holder.returned)
			for range holder.releases {
				holder.early = append(holder.early, ReleaseMCPDispatchSlot(ctx))
			}
			holder.ctx = ctx
			close(holder.started)
			<-holder.release
		}
		return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"ok":true}}`), nil
	}
	f.conn, f.reader, _ = startMCPConnectionWithHooksTest(t, 5*time.Second, dispatch, &mcpConnectionHooks{
		dispatchSlots:     f.pool,
		dispatchQueueWait: &zero,
	})
	return f
}

func (f *mcpSlotLeaseFixture) start(t *testing.T, id string) *mcpSlotLeaseHolder {
	t.Helper()
	holder := f.holders[id]
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall(id))
	awaitMCPTestSignal(t, holder.started, "holder "+id+" did not start")
	return holder
}

func TestServeMCPReleasedDispatchSlotAdmitsNextRequestWhileHandlerRuns(t *testing.T) {
	f := startMCPSlotLeaseFixture(t, 1, map[string]int{"1": 1})
	holder := f.start(t, "1")
	if len(holder.early) != 1 || !holder.early[0] {
		t.Fatalf("release from the running handler = %v, want [true]", holder.early)
	}

	// The queue is disabled: request 2 is admitted only through the slot the
	// still-running handler gave back, or refused as busy.
	writeMCPTestFrame(t, f.conn, mcpDispatchQueueToolCall("2"))
	assertMCPDispatchQueueSucceeded(t, readMCPTestResponsesByID(t, f.conn, f.reader, 1), "2")

	holder.unblock()
	assertMCPDispatchQueueSucceeded(t, readMCPTestResponsesByID(t, f.conn, f.reader, 1), "1")
	if got := len(f.pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots after both replies", got)
	}
}

func TestServeMCPDispatchSlotReturnsOncePerDispatchAcrossCancellation(t *testing.T) {
	f := startMCPSlotLeaseFixture(t, 2, map[string]int{"1": 2, "2": 0})
	early := f.start(t, "1")
	if len(early.early) != 2 || !early.early[0] || early.early[1] {
		t.Fatalf("repeated release = %v, want [true false]", early.early)
	}
	if got := len(f.pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots after the early release, want 0", got)
	}
	// Request 2 takes its slot after request 1 gave its own back, so a lease
	// shared across the connection would hand request 1 the newer handle.
	keeper := f.start(t, "2")
	if got := len(f.pool); got != 1 {
		t.Fatalf("dispatch pool holds %d slots with one holder, want 1", got)
	}

	writeMCPTestFrame(t, f.conn, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	response := readMCPTestResponse(t, f.conn, f.reader)
	if string(response.ID) != "1" || response.Error == nil || response.Error.Code != mcpRequestCancelledCode {
		t.Fatalf("cancellation response = %#v", response)
	}
	if response.Error.Data["delivery_unknown"] != true {
		t.Fatalf("cancellation of a started request data = %#v", response.Error.Data)
	}

	early.unblock()
	awaitMCPTestSignal(t, early.returned, "cancelled handler did not return")
	if got := len(f.pool); got != 1 {
		t.Fatalf("dispatch pool holds %d slots after the cancelled handler returned, want 1", got)
	}
	if ReleaseMCPDispatchSlot(early.ctx) {
		t.Fatal("release on a finished handler's context returned a slot")
	}
	if got := len(f.pool); got != 1 {
		t.Fatalf("late release changed the pool to %d slots, want 1", got)
	}

	// A handler that never released gives its slot back before its reply, and
	// its handle is detached by then.
	keeper.unblock()
	assertMCPDispatchQueueSucceeded(t, readMCPTestResponsesByID(t, f.conn, f.reader, 1), "2")
	if ReleaseMCPDispatchSlot(keeper.ctx) {
		t.Fatal("release after the reply returned a slot")
	}
	if got := len(f.pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots after every reply", got)
	}
}

func TestReleaseMCPDispatchSlotWithoutABoundedDispatchIsANoop(t *testing.T) {
	if ReleaseMCPDispatchSlot(context.Background()) {
		t.Fatal("release without a daemon dispatch returned a slot")
	}

	pool := newMCPDispatchSlots(1)
	released := make(chan bool, 2)
	conn, reader, done := startMCPConnectionWithHooksTest(t, 5*time.Second, func(ctx context.Context, frame []byte) ([]byte, error) {
		var envelope mcpRequestEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return nil, err
		}
		released <- ReleaseMCPDispatchSlot(ctx)
		if len(envelope.ID) == 0 {
			return nil, nil
		}
		return []byte(`{"jsonrpc":"2.0","id":` + string(envelope.ID) + `,"result":{}}`), nil
	}, &mcpConnectionHooks{dispatchSlots: pool})

	// The notification and the unbounded subscriptions/listen dispatch run
	// concurrently, so their results arrive in either order.
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","method":"`+mcpDispatchQueueNotification+`"}`)
	writeMCPTestFrame(t, conn, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`)
	for range 2 {
		select {
		case got := <-released:
			if got {
				t.Fatal("release inside a notification or unbounded dispatch returned a slot")
			}
		case <-time.After(time.Second):
			t.Fatal("notification or unbounded dispatch did not run")
		}
	}
	if response := readMCPTestResponse(t, conn, reader); string(response.ID) != "1" || response.Error != nil {
		t.Fatalf("subscriptions/listen response = %#v", response)
	}
	_ = conn.Close()
	awaitMCPTestSignal(t, done, "connection did not stop")
	if got := len(pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots", got)
	}
}

func TestSessionDispatchSlotReleaseAcrossFollowerRetryFreesOnlyItsOwnSlot(t *testing.T) {
	pool := newMCPDispatchSlots(2)
	base := withMCPDispatchSlots(context.Background(), pool)
	sess := &Session{LogicalSessionID: "reconnect-release"}
	request := []byte(`{"jsonrpc":"2.0","id":78,"method":"tools/call","params":{"name":"search"}}`)
	oldParent, cancelOld := context.WithCancel(base)
	defer cancelOld()
	oldCtx := withMCPDispatchSlotLease(oldParent)
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	unblockOld := sync.OnceFunc(func() { close(releaseOld) })
	t.Cleanup(unblockOld)
	oldHandlerReturned := make(chan struct{})
	oldDone := make(chan error, 1)
	go func() {
		_, _, err := sess.dispatchMCPOnceContext(oldCtx, request, func() ([]byte, error) {
			close(oldStarted)
			<-releaseOld
			close(oldHandlerReturned)
			return []byte(`{"jsonrpc":"2.0","id":78,"result":{"old":true}}`), nil
		})
		oldDone <- err
	}()
	awaitMCPTestSignal(t, oldStarted, "old connection flight did not start")

	// The follower signals admission when it joins the live flight.
	admission := newMCPDispatchAdmission()
	newCtx := withMCPDispatchSlotLease(withMCPDispatchAdmission(base, admission))
	type handlerView struct {
		held, after int
		returned    bool
	}
	view := make(chan handlerView, 1)
	newDone := make(chan error, 1)
	go func() {
		_, _, err := sess.dispatchMCPOnceContext(newCtx, request, func() ([]byte, error) {
			held := len(pool)
			returned := ReleaseMCPDispatchSlot(newCtx)
			view <- handlerView{held: held, returned: returned, after: len(pool)}
			return []byte(`{"jsonrpc":"2.0","id":78,"result":{"new":true}}`), nil
		})
		newDone <- err
	}()
	awaitMCPTestSignal(t, admission.done, "new connection did not join the live flight")
	select {
	case <-view:
		t.Fatal("new connection bypassed the live old flight")
	default:
	}

	cancelOld()
	select {
	case err := <-oldDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old flight error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old cancelled flight did not detach")
	}
	var retried handlerView
	select {
	case retried = <-view:
	case <-time.After(time.Second):
		t.Fatal("new connection did not retry the cancelled flight")
	}
	if retried.held != 1 || !retried.returned || retried.after != 0 {
		t.Fatalf("retrying follower = %+v, want only its own active slot returned", retried)
	}
	if err := <-newDone; err != nil {
		t.Fatalf("retried flight error = %v", err)
	}
	if got := len(pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots while the orphaned owner drains, want 0", got)
	}
	if got := mcpSchedulerFor(pool).snapshot().Lanes["fast"].Draining; got != 1 {
		t.Fatalf("orphaned owner draining count = %d, want 1", got)
	}
	keeper := takeScheduled(t, mcpSchedulerFor(pool), MCPDispatchFast, "unrelated-keeper")

	if ReleaseMCPDispatchSlot(oldCtx) {
		t.Fatal("cancelled owner's late release claimed active capacity")
	}
	if got := len(pool); got != 1 {
		t.Fatalf("owner's stale release changed keeper's slot count to %d", got)
	}
	if ReleaseMCPDispatchSlot(newCtx) {
		t.Fatal("follower's second release returned a slot")
	}
	unblockOld()
	awaitMCPTestSignal(t, oldHandlerReturned, "old orphan handler did not return")
	if got := len(pool); got != 1 {
		t.Fatalf("orphan completion changed keeper's slot count to %d", got)
	}
	keeper.complete()
	if got := len(pool); got != 0 {
		t.Fatalf("dispatch pool holds %d slots after the orphan returned", got)
	}
}
