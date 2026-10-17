package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// A freshness wait must end on a bound the request itself set. A require_fresh
// wait bounds itself twice: the caller's request context, and the caller's
// wait_deadline. The coordinator it waits on bounds
// itself a third time — RequestCheckoutRefresh wraps whatever context it is
// handed in its own checkoutRefreshCaptureTimeout (5s) and in its lifetime
// context (internal/indexer/checkout_refresh.go), and the git sampler wraps
// that context's error with %w (internal/gitstate/dirty.go) — and the cycle
// that completes an admitted ticket runs under the coordinator's context
// rather than the caller's at all.
//
// So `errors.Is(err, context.DeadlineExceeded)` out of the coordinator is NOT
// evidence that the caller's bound expired. Treating it as such:
//
//   - ended a require_fresh wait with wait_deadline 60s away after 5s, ~12x
//     early, whenever a `git status` was slow (large tree, cold FS cache); and
//   - told the caller its bound had expired while printing a still-FUTURE
//     timestamp as the deadline that was not met, sending it to extend the one
//     knob that could not help.
//
// The fix asks the wait's own two bounds instead of the error
// (freshnessBoundExpiry), retries the admission while the caller's bound
// survives, and gives a persistently abandoned admission its own reason
// (freshReasonRefreshAdmissionAbandoned) rather than borrowing
// deadline_exceeded's.

// freshnessWaitCheckout is the routed automatic checkout the fixture's wait
// targets, read from the catalog the way the request path reads it.
func freshnessWaitCheckout(t *testing.T, stack *viewStack) store_sqlite.Checkout {
	t.Helper()
	checkout, found, err := stack.store.Catalog().GetCheckout(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	require.True(t, found, "the fixture must register the routed checkout the wait is admitted against")
	return checkout
}

// foreignBoundTicket is an admitted ticket the coordinator then abandons on a
// context of its own: the cycle that completes tickets runs under the
// coordinator's context, so this is the ticket-side shape of the same defect.
func foreignBoundTicket(checkoutID, root string, err error) *indexer.CheckoutRefreshTicket {
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{Err: err}
	close(done)
	return &indexer.CheckoutRefreshTicket{
		CheckoutID: checkoutID,
		Root:       root,
		Ticket:     &indexer.MutationTicket{Path: root, Generation: 1, Done: done},
	}
}

// The headline: a context error the caller's bounds did not cause is retried
// within the caller's bound, and the wait goes on to succeed.
//
// Before the fix each of these ended the wait at the first attempt with
// fresh:false / deadline_exceeded, with tens of seconds of the caller's
// wait_deadline unspent.
func TestACoordinatorCaptureTimeoutIsNotTheCallersDeadline(t *testing.T) {
	cases := map[string]struct {
		// fail is what the coordinator hands back for the first two
		// admissions, having run out of a bound of its own.
		fail func(checkoutID, root string) (*indexer.CheckoutRefreshTicket, error)
		// attempts is how many admissions the wait must make in total: the
		// failures plus the one that settles.
		attempts int
	}{
		"the capture timeout comes back bare": {
			fail: func(string, string) (*indexer.CheckoutRefreshTicket, error) {
				return nil, context.DeadlineExceeded
			},
			attempts: 3,
		},
		"the sampler wraps the capture timeout": {
			fail: func(string, string) (*indexer.CheckoutRefreshTicket, error) {
				return nil, fmt.Errorf("sample checkout: %w", context.DeadlineExceeded)
			},
			attempts: 3,
		},
		"the coordinator's lifetime context is cancelled": {
			fail: func(string, string) (*indexer.CheckoutRefreshTicket, error) {
				return nil, context.Canceled
			},
			attempts: 3,
		},
		"an admitted ticket is abandoned on the coordinator's context": {
			fail: func(checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				return foreignBoundTicket(checkoutID, root, context.DeadlineExceeded), nil
			},
			attempts: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			waiter := &fakeFreshnessWaiter{
				answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
					if call < tc.attempts {
						return tc.fail(checkoutID, root)
					}
					return settledTicket(checkoutID, root, 1), nil
				},
			}
			stack.srv.freshnessWaiter = waiter

			fresh, reason := stack.srv.awaitCheckoutFreshness(
				context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(10*time.Second))
			require.True(t, fresh,
				"a bound the caller did not set ended the caller's wait; reason=%q", reason)
			require.Empty(t, reason)
			require.Len(t, waiter.observed(), tc.attempts,
				"the wait gave up instead of re-admitting inside the caller's bound")
		})
	}
}

// A capture that never stops timing out is not the caller's deadline being too
// short, and must not be reported as it: the one move deadline_exceeded asks
// for — a bigger wait_deadline — is the one move that cannot help here.
func TestAPersistentlyAbandonedAdmissionGetsItsOwnReason(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
			return nil, context.DeadlineExceeded
		},
	}
	stack.srv.freshnessWaiter = waiter

	fresh, reason := stack.srv.awaitCheckoutFreshness(
		context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(250*time.Millisecond))
	require.False(t, fresh)
	require.Equal(t, freshReasonRefreshAdmissionAbandoned, reason,
		"a coordinator that never admitted a refresh was reported as the caller's bound expiring")
	require.NotEqual(t, freshReasonDeadlineExceeded, reason)
	require.GreaterOrEqual(t, len(waiter.observed()), 2,
		"the wait gave up on the first foreign bound instead of retrying until its own expired")
}

// The other half of the same contract, and the one an over-correction breaks:
// when a bound the wait DOES own ends, it is still reported as that bound. The
// classifier reads the bounds, so it cannot mistake the caller's for the
// coordinator's either way round.
func TestTheCallersOwnBoundStillEndsTheWait(t *testing.T) {
	t.Run("the caller's deadline passes", func(t *testing.T) {
		stack := newViewStack(t)
		waiter := &fakeFreshnessWaiter{
			answerCtx: func(ctx context.Context, _ int, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
				// A coordinator whose only bound is the caller's: it runs
				// until the admitted context ends, and reports that end.
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		stack.srv.freshnessWaiter = waiter

		fresh, reason := stack.srv.awaitCheckoutFreshness(
			context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(200*time.Millisecond))
		require.False(t, fresh)
		require.Equal(t, freshReasonDeadlineExceeded, reason,
			"the caller's own wait_deadline expiring must keep its own reason")
		require.Len(t, waiter.observed(), 1, "an expired bound must not be re-admitted")
	})

	t.Run("the caller's request ends", func(t *testing.T) {
		stack := newViewStack(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waiter := &fakeFreshnessWaiter{
			answerCtx: func(waitCtx context.Context, _ int, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
				cancel()
				<-waitCtx.Done()
				return nil, waitCtx.Err()
			},
		}
		stack.srv.freshnessWaiter = waiter

		fresh, reason := stack.srv.awaitCheckoutFreshness(
			ctx, freshnessWaitCheckout(t, stack), time.Now().Add(10*time.Second))
		require.False(t, fresh)
		require.Equal(t, freshReasonInterrupted, reason,
			"a request that ended was reported as a bound that did not")
		require.Len(t, waiter.observed(), 1, "a dead request must not be re-admitted")
	})

	t.Run("the request ends after a foreign bound", func(t *testing.T) {
		stack := newViewStack(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waiter := &fakeFreshnessWaiter{
			answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
				// The coordinator's own bound ended this admission, and the
				// request dies before the retry: the request ending is the
				// stronger fact and wins.
				cancel()
				return nil, context.DeadlineExceeded
			},
		}
		stack.srv.freshnessWaiter = waiter

		fresh, reason := stack.srv.awaitCheckoutFreshness(
			ctx, freshnessWaitCheckout(t, stack), time.Now().Add(10*time.Second))
		require.False(t, fresh)
		require.Equal(t, freshReasonInterrupted, reason,
			"an interrupted request was reported as the coordinator's admission: reason=%q", reason)
	})
}

// The order the three classifiers run in at the admission is load-bearing, and
// was assertion rather than evidence: hoisting freshnessCheckoutErrorReason
// above freshnessWaitRetryable and freshnessContextExpiry left the whole
// freshness suite green.
//
// It is reachable, not hypothetical: internal/indexer/checkout_mutation.go:400
// returns `fmt.Errorf("%w: sample checkout: %w", ErrCheckoutMutationStale,
// err)` where err is the sampler's wrapped context error, so ONE error
// satisfies both errors.Is predicates. Which arm claims it decides whether the
// wait retries or reports "this checkout's root changed" — a false statement
// about the checkout when all that happened is that a git sample ran out of
// the coordinator's 5s.
func TestFreshnessAdmissionReasonOrderingIsLoadBearing(t *testing.T) {
	cases := map[string]struct {
		first    error
		fresh    bool
		reason   string
		attempts int
	}{
		// context-expiry outranks the sentinel map.
		"a stale sentinel wrapping a context error": {
			first: fmt.Errorf("%w: sample checkout: %w",
				indexer.ErrCheckoutMutationStale, context.DeadlineExceeded),
			fresh:    true,
			attempts: 2,
		},
		// retryable outranks the sentinel map. The composition is deliberate:
		// it asserts the documented precedence directly rather than relying on
		// a production site to keep producing it.
		"a busy sentinel wrapping a stale sentinel": {
			first: fmt.Errorf("%w: lane busy: %w",
				indexer.ErrCheckoutMutationBusy, indexer.ErrCheckoutMutationStale),
			fresh:    true,
			attempts: 2,
		},
		// The control: with nothing outranking it, the sentinel map still
		// fires and is still terminal, so the ordering above is not being
		// satisfied by a dead mapping.
		"a stale sentinel on its own": {
			first:    fmt.Errorf("%w: checkout root changed", indexer.ErrCheckoutMutationStale),
			fresh:    false,
			reason:   freshReasonCheckoutRootChanged,
			attempts: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := newViewStack(t)
			waiter := &fakeFreshnessWaiter{
				answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
					if call == 1 {
						return nil, tc.first
					}
					return settledTicket(checkoutID, root, 1), nil
				},
			}
			stack.srv.freshnessWaiter = waiter

			fresh, reason := stack.srv.awaitCheckoutFreshness(
				context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(10*time.Second))
			require.Equal(t, tc.fresh, fresh, "reason=%q", reason)
			require.Equal(t, tc.reason, reason)
			require.Len(t, waiter.observed(), tc.attempts,
				"the wrong classifier claimed the error: reason=%q", reason)
		})
	}
}

// The production entrypoint, end to end: a real tools/call frame carrying the
// request-level knobs, through the middleware, to the rider and the refusal.
//
// Both arms are the refusal side of the defect. Without require_exact the
// rider used to say deadline_exceeded; with it, the caller got
// freshnessDeadlineRefusal, which names a wait_deadline that had NOT been
// reached — a future timestamp presented as the bound that was missed.
func TestAnAbandonedAdmissionReachesTheRiderAndTheRefusal(t *testing.T) {
	abandoning := func() *fakeFreshnessWaiter {
		return &fakeFreshnessWaiter{
			answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
				return nil, context.DeadlineExceeded
			},
		}
	}

	t.Run("the rider names the abandoned admission", func(t *testing.T) {
		stack := newViewStack(t)
		waiter := abandoning()
		stack.srv.freshnessWaiter = waiter
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
			freshArgs(nil, 250*time.Millisecond), captureReader(stack.srv, new(graph.Reader)))
		require.NoError(t, err)
		require.False(t, res.IsError, viewResultText(t, res))
		rider := resultFreshness(t, res)
		require.Equal(t, false, rider["fresh"], "rider = %v", rider)
		require.Equal(t, freshReasonRefreshAdmissionAbandoned, rider["fresh_reason"], "rider = %v", rider)
		require.GreaterOrEqual(t, len(waiter.observed()), 2,
			"the middleware's wait gave up on the first foreign bound")
	})

	t.Run("require_exact refuses without blaming the deadline", func(t *testing.T) {
		stack := newViewStack(t)
		stack.srv.freshnessWaiter = abandoning()
		deadline := time.Now().Add(250 * time.Millisecond)
		args := map[string]any{
			requireExactArgName: true,
			requireFreshArgName: true,
			waitDeadlineArgName: deadline.Format(time.RFC3339Nano),
		}
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args,
			captureReader(stack.srv, new(graph.Reader)))
		require.NoError(t, err)
		assertToolError(t, res, graphview.CodeViewBuilding)
		text := viewResultText(t, res)
		require.Contains(t, text, freshReasonRefreshAdmissionAbandoned,
			"the refusal must name what actually stopped the wait: %s", text)
		require.NotContains(t, text, deadline.UTC().Format(time.RFC3339),
			"the refusal presented a bound that was never reached as the one that was missed: %s", text)
		require.NotContains(t, text, "did not reach the current working copy before",
			"the refusal is still freshnessDeadlineRefusal's: %s", text)
	})
}

// The ticket arm of the same contract, which shipped unpinned.
//
// awaitCheckoutFreshness reaches a foreign bound two ways: the ADMISSION can
// fail with a context error (covered by
// TestAPersistentlyAbandonedAdmissionGetsItsOwnReason), and an ADMITTED ticket
// can be failed with one — the cycle that completes tickets runs under the
// coordinator's context, not the caller's (internal/indexer/checkout_refresh.go
// completeCheckoutRefreshTickets). Only the second path sets awaitFreshnessTicket's
// fourth return, and flipping that return to false left the whole freshness
// suite green while production output changed: a wait whose admitted tickets
// are persistently abandoned on the coordinator's context reported
// deadline_exceeded — the very defect separating the two bounds exists to
// remove — instead of
// refresh_admission_abandoned. The two reasons ask the caller for different
// moves, and only one of them can help.
func TestAPersistentlyAbandonedTicketGetsItsOwnReason(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(_ int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
			return foreignBoundTicket(checkoutID, root, context.DeadlineExceeded), nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	fresh, reason := stack.srv.awaitCheckoutFreshness(
		context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(250*time.Millisecond))
	require.False(t, fresh)
	require.Equal(t, freshReasonRefreshAdmissionAbandoned, reason,
		"every admitted ticket was abandoned on a bound the caller did not set, and the caller's bound was blamed")
	require.NotEqual(t, freshReasonDeadlineExceeded, reason)
	require.GreaterOrEqual(t, len(waiter.observed()), 2,
		"the wait gave up on the first abandoned ticket instead of retrying until its own bound expired")
}

// The converse, and the reason the abandoned flag is a LAST-attempt fact
// rather than a sticky one: a foreign bound followed by ordinary retryable
// refusals until the caller's deadline is the caller's deadline expiring.
// Without the reset on the retryable arm, one early coordinator timeout would
// relabel every later wait_deadline expiry as an abandoned admission.
func TestAForeignBoundDoesNotStickToALaterDeadline(t *testing.T) {
	stack := newViewStack(t)
	waiter := &fakeFreshnessWaiter{
		answer: func(call int, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
			if call == 1 {
				return nil, context.DeadlineExceeded
			}
			return nil, fmt.Errorf("%w: lane busy", indexer.ErrCheckoutMutationBusy)
		},
	}
	stack.srv.freshnessWaiter = waiter

	fresh, reason := stack.srv.awaitCheckoutFreshness(
		context.Background(), freshnessWaitCheckout(t, stack), time.Now().Add(250*time.Millisecond))
	require.False(t, fresh)
	require.Equal(t, freshReasonDeadlineExceeded, reason,
		"a wait that ended on its own bound was reported as an abandoned admission")
	require.GreaterOrEqual(t, len(waiter.observed()), 2,
		"the wait did not retry past the first foreign bound")
}
