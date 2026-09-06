package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func (s *Server) handleCheckoutControlReindex(ctx context.Context, req mcp.CallToolRequest, control *checkoutControlScope, paths []string) (*mcp.CallToolResult, error) {
	if control.Checkout.State != store_sqlite.CheckoutStateReady {
		return mcp.NewToolResultError(graphview.NewViewError(graphview.CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is %s", control.Checkout.CheckoutID, control.Checkout.State)).Error()), nil
	}
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout recovery is unavailable: no checkout lifecycle is attached"), nil
	}
	eligible := s.failedCheckoutRefreshReceiptsBefore(control.Checkout.CheckoutID, control.Checkout.Incarnation)
	ticket, err := s.lifecycle.RequestCheckoutRefresh(ctx, control.Checkout.CheckoutID, control.Checkout.RootPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	receipt := s.trackCheckoutRecoveryTicket(ticket, eligible)
	outcome := receipt.outcome(true)
	payload := map[string]any{
		"scope": "checkout", "status": "queued",
		"repo": control.RepoPrefix, "repo_root": control.Checkout.RootPath,
		"checkout_id": control.Checkout.CheckoutID, "checkout_incarnation": control.Checkout.Incarnation,
		"detail": "the selected checkout coordinator will refresh its sparse layers; inspect reindex_receipt for publication",
	}
	if len(paths) > 0 {
		// The coordinator refreshes one coherent checkout snapshot. These are
		// validated recovery hints, not a claim that a partial result was built.
		payload["requested_paths"] = paths
	}
	s.attachMutationFreshness(payload, "", "", outcome)
	return s.respondJSONOrTOON(ctx, req, payload)
}
