package mcp

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

func guardBatchCheckout(ctx context.Context, receipt batchTransactionReceipt) error {
	scope := checkoutMutationFromContext(ctx)
	if receipt.CheckoutID == "" {
		if scope != nil {
			return fmt.Errorf("batch transaction does not belong to the selected checkout")
		}
		return nil
	}
	if scope == nil || scope.checkoutID != receipt.CheckoutID || scope.incarnation != receipt.CheckoutIncarnation || !pathkey.EqualPaths(scope.root, receipt.CheckoutRoot) {
		return fmt.Errorf("batch transaction requires its original checkout incarnation")
	}
	for _, file := range receipt.Files {
		if err := guardCheckoutMutationPath(ctx, file.Path); err != nil {
			return err
		}
	}
	return nil
}

func verifyBatchAfterImages(files []batchTransactionFile) error {
	for _, file := range files {
		info, err := os.Lstat(file.Path)
		if file.AfterAbsent && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read batch after-image %s: %w", file.RelativePath, err)
		}
		if file.AfterAbsent || !info.Mode().IsRegular() {
			return fmt.Errorf("batch file %s changed after disk commit", file.RelativePath)
		}
		content, err := os.ReadFile(file.Path)
		if err != nil {
			return fmt.Errorf("read batch after-image %s: %w", file.RelativePath, err)
		}
		if digestBatchBytes(content) != file.AfterSHA256 {
			return fmt.Errorf("batch file %s changed after disk commit", file.RelativePath)
		}
	}
	return nil
}

// A batch publishes one checkout snapshot, including deletion tombstones. Never
// enqueue its paths on the primary watcher or wait while owning the route lease.
func (s *Server) refreshCheckoutBatchGraph(ctx context.Context, state *batchTransactionState, receipt batchTransactionReceipt) {
	err := guardBatchCheckout(ctx, receipt)
	var outcome mutationReindexOutcome
	if err == nil && len(receipt.Files) == 0 {
		err = fmt.Errorf("checkout batch has no committed files")
	}
	if err == nil && receipt.Files[0].ReindexReceipt != "" {
		if value, found := s.mutationReceipts.Load(receipt.Files[0].ReindexReceipt); found {
			if previous, ok := value.(*mutationReceipt); ok && previous.checkoutScoped && previous.checkoutID == receipt.CheckoutID && previous.checkoutIncarnation == receipt.CheckoutIncarnation && previous.generation == receipt.Files[0].ReindexGeneration && pathkey.EqualPaths(previous.path, receipt.CheckoutRoot) {
				if previousOutcome := previous.outcome(true); previousOutcome.Err == nil {
					outcome = previousOutcome
					goto publish
				}
			}
		}
	}
	if err == nil {
		scope := checkoutMutationFromContext(ctx)
		if scheduler, ok := scope.mutation.(checkoutMutationScheduler); ok {
			err = verifyBatchAfterImages(receipt.Files)
			if err == nil {
				err = prepareCheckoutMutation(ctx, receipt.Files[0].Path)
			}
			if err == nil {
				eligible := s.failedCheckoutRefreshReceiptsBefore(scope.checkoutID, scope.incarnation)
				ticket, ticketErr := scheduler.EnqueueRefresh(context.WithoutCancel(ctx), "")
				if ticketErr == nil && (ticket == nil || ticket.Ticket == nil || ticket.Ticket.Done == nil || ticket.CheckoutID != scope.checkoutID || ticket.Incarnation != scope.incarnation || !pathkey.EqualPaths(ticket.Root, scope.root) || !pathkey.EqualPaths(ticket.Ticket.Path, scope.root)) {
					ticketErr = fmt.Errorf("batch refresh ticket does not belong to the selected checkout")
				}
				if ticketErr == nil {
					outcome = s.trackCheckoutRecoveryTicket(ticket, eligible).outcome(true)
					ticketErr = verifyBatchAfterImages(receipt.Files)
				}
				err = ticketErr
			}
		} else {
			err = prepareCheckoutMutation(ctx, receipt.Files[0].Path)
			if err == nil {
				outcome = s.refreshCheckoutMutation(ctx, receipt.Files[0].Path, scope)
				err = outcome.Err
			}
		}
	}
publish:
	if err != nil {
		outcome = mutationReindexOutcome{Err: err, checkoutScoped: true}
	}
	receipt.Error = ""
	receipt.GraphStatus = "fresh"
	if err != nil {
		receipt.GraphStatus, receipt.Error = "failed", err.Error()
	} else if outcome.Pending {
		receipt.GraphStatus = "pending"
	} else if !outcome.Reindexed {
		receipt.GraphStatus = "failed"
	}
	for i := range receipt.Files {
		receipt.Files[i].ReindexReceipt = outcome.Receipt
		receipt.Files[i].ReindexGeneration = outcome.Generation
	}
	for i := range receipt.Results {
		receipt.Results[i].Reindexed = outcome.Reindexed
		receipt.Results[i].ReindexPending = outcome.Pending
		receipt.Results[i].ReindexReceipt = outcome.Receipt
		receipt.Results[i].ReindexGeneration = outcome.Generation
		receipt.Results[i].ReindexAppliedGeneration = outcome.AppliedGeneration
		receipt.Results[i].ReindexError = ""
		if err != nil {
			receipt.Results[i].ReindexError = err.Error()
		}
	}
	if receipt.GraphStatus != "pending" {
		now := time.Now().UTC()
		receipt.CompletedAt = &now
	}
	if persistErr := s.persistBatchManifest(receipt); persistErr != nil {
		receipt.Error = "disk committed; terminal journal update failed: " + persistErr.Error()
	} else if receipt.GraphStatus == "fresh" {
		_ = s.cleanupBatchBackups(receipt)
	}
	state.publish(receipt, true)
}
