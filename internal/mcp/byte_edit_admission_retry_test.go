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
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func byteAdmissionPinMovement() error {
	return fmt.Errorf("%w: %w", indexer.ErrCheckoutMutationRouteMoved,
		graphview.NewViewError(graphview.CodeViewBuilding, "checkout route moved before ancestry pin"))
}

func TestByteEditAdmissionRetryStopsAfterOneCleanReentry(t *testing.T) {
	for _, next := range []string{"admitted", "second pin movement", "missing retained anchor", "storage failure"} {
		t.Run(next, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			admitted := &indexer.CheckoutMutation{}
			secondErr := byteAdmissionPinMovement()
			switch next {
			case "admitted":
				secondErr = nil
			case "missing retained anchor":
				secondErr = indexer.ErrCheckoutByteAnchorUnavailable
			case "storage failure":
				secondErr = errors.New("catalog read failed")
			}
			calls := 0
			mutation, err := beginByteEditAdmission(ctx, func(got context.Context) (*indexer.CheckoutMutation, error) {
				require.Same(t, ctx, got)
				gotDeadline, _ := got.Deadline()
				require.Equal(t, deadline, gotDeadline)
				calls++
				if calls == 1 {
					return nil, byteAdmissionPinMovement()
				}
				require.Equal(t, 2, calls, "a second refusal must return to the original fallback")
				if secondErr == nil {
					return admitted, nil
				}
				return nil, secondErr
			})
			require.Equal(t, 2, calls)
			if secondErr == nil {
				require.Same(t, admitted, mutation)
				require.NoError(t, err)
			} else {
				require.Nil(t, mutation)
				require.Same(t, secondErr, err, "do not translate the second admission outcome")
				wantFallback := next == "second pin movement" || next == "missing retained anchor"
				require.Equal(t, wantFallback, errors.Is(err, indexer.ErrCheckoutByteAnchorUnavailable) || byteEditAdmissionCanReselect(ctx, mutation, err))
			}
		})
	}
}

func TestByteEditAdmissionRetryPreservesOtherOutcomes(t *testing.T) {
	for _, kind := range []string{"success", "admitted lease with pin movement", "ordinary route movement", "disk movement", "missing anchor", "generic building", "wrong root or incarnation", "storage failure", "cancellation error", "deadline error", "caller canceled after pin movement", "caller expired"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mutation *indexer.CheckoutMutation
			var original error
			switch kind {
			case "success":
				mutation = &indexer.CheckoutMutation{}
			case "admitted lease with pin movement":
				mutation, original = &indexer.CheckoutMutation{}, byteAdmissionPinMovement()
			case "ordinary route movement":
				original = fmt.Errorf("primary base: %w", indexer.ErrCheckoutMutationRouteMoved)
			case "disk movement":
				original = indexer.ErrCheckoutMutationDiskMoved
			case "missing anchor":
				original = indexer.ErrCheckoutByteAnchorUnavailable
			case "generic building":
				original = graphview.NewViewError(graphview.CodeViewBuilding, "ancestry incomplete")
			case "wrong root or incarnation":
				original = fmt.Errorf("%w: checkout identity changed", indexer.ErrCheckoutMutationStale)
			case "storage failure":
				original = errors.New("catalog I/O failure")
			case "cancellation error":
				original = context.Canceled
			case "deadline error":
				original = context.DeadlineExceeded
			case "caller canceled after pin movement":
				original = byteAdmissionPinMovement()
			case "caller expired":
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Time{})
				defer stop()
				original = byteAdmissionPinMovement()
			}
			calls := 0
			got, err := beginByteEditAdmission(ctx, func(passed context.Context) (*indexer.CheckoutMutation, error) {
				require.Same(t, ctx, passed)
				calls++
				if kind == "caller canceled after pin movement" {
					cancel()
				}
				return mutation, original
			})
			require.Equal(t, 1, calls)
			require.True(t, mutation == got, "preserve the original admitted mutation")
			if original == nil {
				require.NoError(t, err)
			} else if kind == "deadline error" {
				// DeadlineExceeded is a value error, not a pointer sentinel.
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.True(t, err == original, "return the exact original deadline error")
			} else {
				require.Same(t, original, err)
			}
		})
	}
}

// Reproduce a real exported pin refusal against an epoch-moved route, then
// reenter the real lifecycle and edit handler. The indexer and graphview pin
// barriers separately prove cleanup of the complete Begin path's first refusal.
func TestByteEditAdmissionRetryUsesCurrentRealCheckoutAndGuardedWrite(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	ctx := WithSessionCWD(WithSessionID(t.Context(), "pin-admission-retry"), f.worktree)
	catalog := f.store.Catalog()
	old, found, err := catalog.GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	checkout, found, err := catalog.GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	path := filepath.Join(f.worktree, "edit.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	calls := 0
	mutation, err := beginByteEditAdmission(ctx, func(ctx context.Context) (*indexer.CheckoutMutation, error) {
		calls++
		if calls == 1 {
			// Same payload, different epoch: neither an old route nor its pins
			// may be promoted to authority for the second admission.
			require.NoError(t, catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{CheckoutID: f.checkoutID, Slot: store_sqlite.RouteSlotDirty, GenerationID: old.DirtyGenerationID, ExpectedRouteEpoch: old.RouteEpoch, State: store_sqlite.RouteActive}))
			lease, moved, pinErr := f.srv.Materializer().PinCheckoutRoute(ctx, f.checkoutID, old)
			require.Nil(t, lease)
			require.True(t, moved)
			require.ErrorIs(t, pinErr, graphview.ErrViewBuilding)
			return nil, fmt.Errorf("%w: %w", indexer.ErrCheckoutMutationRouteMoved, pinErr)
		}
		return f.srv.lifecycle.BeginCheckoutByteHandoff(ctx, f.checkoutID, f.worktree, checkout.Incarnation)
	})
	require.Equal(t, 2, calls)
	view := &requestView{kind: requestViewKindWorktree, viewRoot: f.worktree}
	if errors.Is(err, indexer.ErrCheckoutByteAnchorUnavailable) {
		// Active does not imply idle: the initial real cycle may still be
		// draining, and some HEAD layouts cannot supply optional byte evidence.
		// This exact decline retains the original fully materialized fallback.
		require.Nil(t, mutation)
		require.NoError(t, ctx.Err())
		materialized, materializeErr := f.srv.Materializer().MaterializeCheckout(ctx, f.checkoutID)
		require.NoError(t, materializeErr)
		defer materialized.Close()
		view.materialized = materialized
		mutation, err = f.srv.lifecycle.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, materialized.CheckoutRouteEpoch)
	}
	require.NoError(t, err)
	require.NotNil(t, mutation)
	defer mutation.Close()
	current, found, err := catalog.GetCheckoutRoute(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, old.RouteEpoch, current.RouteEpoch)
	ctx = withCheckoutMutation(ctx, mutation, f.worktree)
	ctx = withRequestView(ctx, view)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": path, "old_string": "func New() {}", "new_string": "func PinRetryWritten() {}", "base_sha": gitBlobSHA(before), "expected_occurrences": 1, "format": "json"}
	result, err := f.srv.handleEditFile(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	payload := decodeFileOpsResult(t, result)
	require.Equal(t, "applied", payload["status"])
	require.EqualValues(t, 1, payload["replacements"])
	mutation.Close()
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(disk), "func PinRetryWritten() {}")
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primaryBefore, primaryAfter)
	f.awaitMutation(t, f.worktree, result)
}
