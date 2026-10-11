package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A known transaction is a catalog operation: its own interrupted publication
// may have withdrawn the route needed for ordinary source edits.
func (s *Server) isBatchContinuation(legacy string, req *mcp.CallToolRequest) bool {
	if legacy != "batch_edit" {
		return false
	}
	args := req.GetArguments()
	if isFacadeToolName(req.Params.Name) {
		if spec, ok := s.viewFacadeOperation(req); ok {
			args = normalizeFacadeArguments(spec, args)
		}
	}
	// A preview still needs the selected graph and path authority, even when
	// its caller happens to reuse an existing transaction identifier.
	if dryRun, _ := args["dry_run"].(bool); dryRun {
		return false
	}
	id, _ := args["transaction_id"].(string)
	if id == "" {
		return false
	}
	if status, _ := args["status_only"].(bool); status {
		return true
	}
	if _, ok := s.batchTransactions.Load(id); ok {
		return true
	}
	_, found, err := readBatchManifest(id)
	return found || err != nil
}

func (s *Server) validateBatchCheckout(ctx context.Context, receipt batchTransactionReceipt) error {
	mutation := checkoutMutationFromContext(ctx)
	control := checkoutControlFromContext(ctx)
	if receipt.CheckoutID == "" {
		if mutation != nil || (control != nil && control.CheckoutScoped) {
			return fmt.Errorf("transaction %q belongs to the primary checkout, not the selected worktree", receipt.TransactionID)
		}
		return nil
	}
	id, inc, root := "", "", ""
	if mutation != nil {
		id, inc, root = mutation.checkoutID, mutation.incarnation, mutation.root
	} else if control != nil {
		id, inc, root = control.Checkout.CheckoutID, control.Checkout.Incarnation, control.Checkout.RootPath
	}
	if id != receipt.CheckoutID || inc != receipt.Incarnation || !pathkey.EqualPaths(resolveNearestExistingAncestor(root), receipt.CheckoutRoot) {
		return fmt.Errorf("transaction %q does not belong to the selected checkout incarnation", receipt.TransactionID)
	}
	if receipt.RootIdentity == "" || gitstate.SamplePathEvidence(receipt.CheckoutRoot).RootIdentity != receipt.RootIdentity {
		return fmt.Errorf("transaction checkout root was replaced or is unavailable")
	}
	return nil
}

func (s *Server) acquireBatchRecovery(ctx context.Context, receipt batchTransactionReceipt) (context.Context, func(), error) {
	if err := s.validateBatchCheckout(ctx, receipt); err != nil {
		return ctx, func() {}, err
	}
	if checkoutMutationFromContext(ctx) != nil {
		return ctx, func() {}, nil
	}
	if s.lifecycle == nil {
		return ctx, func() {}, fmt.Errorf("checkout batch recovery requires its checkout coordinator")
	}
	if batchRecoveryBeforeAdmission != nil {
		batchRecoveryBeforeAdmission(ctx)
	}
	mutation, err := s.lifecycle.BeginCheckoutRecovery(context.WithoutCancel(ctx), receipt.CheckoutID, receipt.Incarnation, receipt.CheckoutRoot, receipt.RootIdentity, receipt.CheckoutGeneration, receipt.HeadRef, receipt.HeadCommit, receipt.HeadTree)
	if err != nil {
		return ctx, func() {}, err
	}
	ctx = withCheckoutMutation(ctx, mutation, receipt.CheckoutRoot)
	for _, file := range receipt.Files {
		if err := guardCheckoutMutationPath(ctx, file.Path); err != nil {
			mutation.Close()
			return ctx, func() {}, err
		}
	}
	return ctx, mutation.Close, nil
}

func batchCheckoutFiles(receipt batchTransactionReceipt) []indexer.CheckoutBatchFile {
	files := make([]indexer.CheckoutBatchFile, 0, len(receipt.Files))
	for _, file := range receipt.Files {
		files = append(files, indexer.CheckoutBatchFile{Path: file.Path, SHA256: file.AfterSHA256, Absent: file.AfterAbsent})
	}
	return files
}

func verifyBatchAfterImages(ctx context.Context, receipt batchTransactionReceipt) error {
	for _, file := range receipt.Files {
		if err := guardCheckoutMutationPath(ctx, file.Path); err != nil {
			return err
		}
		if file.AfterAbsent {
			if _, err := os.Lstat(file.Path); !os.IsNotExist(err) {
				return fmt.Errorf("batch committed absence changed: %s", file.RelativePath)
			}
		} else {
			bytes, err := os.ReadFile(file.Path)
			if err != nil || digestBatchBytes(bytes) != file.AfterSHA256 {
				return fmt.Errorf("batch committed content changed: %s", file.RelativePath)
			}
		}
	}
	return nil
}

type checkoutBatchScheduler interface {
	EnqueueBatchRefresh(context.Context, []indexer.CheckoutBatchFile) (*indexer.CheckoutRefreshTicket, error)
}

func (s *Server) refreshCheckoutBatchGraph(ctx context.Context, state *batchTransactionState, receipt batchTransactionReceipt, admissionErr error) {
	outcome := mutationReindexOutcome{checkoutScoped: true}
	var err error
	if admissionErr != nil {
		outcome.Err = admissionErr
	} else if err = s.validateBatchCheckout(ctx, receipt); err != nil {
		outcome.Err = err
	} else {
		// All paths share one completion signal. Reuse a live receipt without
		// acquiring a lease that could block the publication it represents.
		if len(receipt.Files) > 0 && receipt.Files[0].ReindexReceipt != "" {
			var found bool
			outcome, found = s.mutationReceiptState(receipt.Files[0].ReindexReceipt)
			if !found {
				outcome = mutationReindexOutcome{checkoutScoped: true}
			} else if !outcome.checkoutScoped || outcome.checkoutID != receipt.CheckoutID || outcome.checkoutIncarnation != receipt.Incarnation {
				outcome.Err = fmt.Errorf("batch publication receipt does not belong to committed checkout incarnation")
			}
		}
		if outcome.Receipt == "" {
			if checkoutMutationFromContext(ctx) != nil {
				err = verifyBatchAfterImages(ctx, receipt)
				if err == nil {
					err = prepareBatchRecovery(ctx)
				}
				if err == nil {
					mutation := checkoutMutationFromContext(ctx)
					scheduler, ok := mutation.mutation.(checkoutBatchScheduler)
					if !ok {
						err = fmt.Errorf("checkout coordinator does not support atomic batch publication")
					} else {
						pin := handoffRequestView(ctx, viewmetrics.HandoffCheckoutRefresh)
						ticket, ticketErr := scheduler.EnqueueBatchRefresh(context.WithoutCancel(ctx), batchCheckoutFiles(receipt))
						err = ticketErr
						if err == nil && (ticket == nil || ticket.Ticket == nil || ticket.CheckoutID != receipt.CheckoutID || ticket.Incarnation != receipt.Incarnation || !pathkey.EqualPaths(ticket.Root, receipt.CheckoutRoot)) {
							err = fmt.Errorf("batch publication ticket does not belong to committed checkout")
						}
						if err != nil {
							pin.release()
						} else {
							tracked := s.trackCheckoutRefreshTicket(ticket)
							tracked.pinView(pin)
							outcome = tracked.outcome(true)
						}
					}
				}
			}
			if checkoutMutationFromContext(ctx) == nil && err == nil {
				err = fmt.Errorf("checkout publication has no admitted recovery authority")
			}
			if err != nil {
				outcome.Err = err
			}
		}
	}
	// Never wait under the checkout cycle lease. The handler releases it when
	// returning; status reads observe the shared ticket's eventual completion.
	receipt.GraphStatus = "pending"
	if outcome.Err != nil || (!outcome.Pending && !outcome.Reindexed) {
		receipt.GraphStatus = "failed"
	} else if outcome.Reindexed {
		receipt.GraphStatus = "fresh"
	}
	for i := range receipt.Files {
		receipt.Files[i].ReindexReceipt, receipt.Files[i].ReindexGeneration = outcome.Receipt, outcome.Generation
	}
	for i := range receipt.Results {
		result := &receipt.Results[i]
		result.Reindexed, result.ReindexPending = outcome.Reindexed, outcome.Pending
		result.ReindexReceipt, result.ReindexGeneration, result.ReindexAppliedGeneration = outcome.Receipt, outcome.Generation, outcome.AppliedGeneration
		result.ReindexError = ""
		if outcome.Err != nil {
			result.ReindexError = outcome.Err.Error()
		}
	}
	// Failed tickets may be re-admitted by a later status call after verifying
	// the complete durable after-state again.
	if outcome.Err != nil {
		for i := range receipt.Files {
			receipt.Files[i].ReindexReceipt = ""
		}
	}
	if receipt.CompletedAt == nil {
		now := time.Now().UTC()
		receipt.CompletedAt = &now
	}
	persistErr := s.persistBatchManifest(receipt)
	if persistErr != nil {
		receipt.Error = strings.TrimSpace(receipt.Error + "; terminal journal update failed: " + persistErr.Error())
	}
	state.publish(receipt, true)
	if persistErr == nil && batchReceiptCleanupSafe(receipt) {
		_ = s.cleanupBatchBackups(receipt)
	}
}

type checkoutBatchAuthority interface {
	BatchAuthority() (int64, string, string, string)
}

func prepareBatchRecovery(ctx context.Context) error {
	if mutation := checkoutMutationFromContext(ctx); mutation != nil {
		if recovery, ok := mutation.mutation.(interface{ PrepareRecovery(context.Context) error }); ok {
			return recovery.PrepareRecovery(context.WithoutCancel(ctx))
		}
	}
	return nil
}

// batchRecoveryBeforeAdmission is a test seam for the cycle/transaction lock order.
var batchRecoveryBeforeAdmission func(context.Context)
