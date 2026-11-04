package mcp

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// Capture the selected checkout's exact per-file write fact before a later
// commit updates the request's last-write state. No lease or path lock is new.
type sourceMutationTarget struct {
	key      string
	path     string
	checkout *checkoutMutationState
}

func captureSourceMutationTarget(ctx context.Context, path string) sourceMutationTarget {
	target := sourceMutationTarget{path: path, key: pathkey.MutationKey(path)}
	if state := checkoutMutationFromContext(ctx); state != nil {
		copy := *state
		target.checkout = &copy
	}
	return target
}

// Admit the complete committed frontier while its original path set is held.
// A partial/error response cannot be retried pointwise: some files may already
// be admitted. Only the explicit nil-map/no-admission result permits fallback.
func (s *Server) admitSourceMutationFrontier(ctx context.Context, targets []sourceMutationTarget) []sourceMutationAdmission {
	admissions := make([]sourceMutationAdmission, len(targets))
	policy := s.sourceAckPolicy(ctx)
	defer func() {
		for i := range admissions {
			admissions[i].policy = policy
		}
	}()
	if len(targets) > 1 && checkoutMutationFromContext(ctx) == nil {
		if scheduler, ok := s.currentWatcher().(mutationSetScheduler); ok {
			paths := make([]string, len(targets))
			for i, target := range targets {
				paths[i] = target.path
			}
			// Reuse the same exact note/path/ledger owner selection as point
			// admission. The context is ephemeral; tickets retain only their own
			// existing record, never another file's or the whole map.
			records := make(map[string]*indexer.PublicationPhaseRecord, len(paths))
			for _, path := range paths {
				if record := s.committedPublicationRecord(ctx, path); record != nil {
					records[path] = record
				}
			}
			ctx = indexer.WithFilePublicationRecords(ctx, records)
			lap := s.beginAcknowledgementLap(ctx, editAckTicketAdmission)
			tickets, err := scheduler.EnqueueFileMutations(context.WithoutCancel(ctx), paths)
			lap.finish()
			if err == nil && tickets != nil {
				for _, path := range paths {
					ticket := tickets[filepath.Clean(path)]
					if ticket == nil || ticket.Done == nil || filepath.Clean(ticket.Path) != filepath.Clean(path) {
						err = errors.New("mutation set scheduler returned incomplete or mismatched coverage")
						break
					}
				}
			}
			if err != nil {
				for _, record := range records {
					failMutationPhases(record)
				}
				for i := range admissions {
					admissions[i].immediate.Err = err
				}
				return admissions
			}
			if tickets != nil {
				for i, path := range paths {
					ticket := tickets[filepath.Clean(path)]
					pin := handoffRequestView(ctx, viewmetrics.HandoffFileMutation)
					receipt := s.trackMutationTicket(ticket)
					receipt.pinView(pin)
					s.bindReadRepairAdmission(path, ticket.PublicationRecord, receipt)
					admissions[i].receipt = receipt
				}
				return admissions
			}
		}
	}
	for i, target := range targets {
		admissionCtx := ctx
		if target.checkout != nil {
			admissionCtx = context.WithValue(ctx, checkoutMutationContextKey{}, target.checkout)
		}
		admissions[i] = s.admitSourceMutations(admissionCtx, target.path)
	}
	return admissions
}

// Call only after every source path has been released. Inline compatibility
// work completes before returning dispatch capacity; queued work shares one
// parent deadline, one dispatch handoff and the existing receipt identities.
func (s *Server) awaitSourceMutationFrontier(ctx context.Context, admissions []sourceMutationAdmission) []mutationReindexOutcome {
	waitCtx, cancel := context.WithTimeout(ctx, s.mutationWaitDuration())
	defer cancel()
	outcomes := make([]mutationReindexOutcome, len(admissions))
	var files []batchTransactionFile
	for i, admission := range admissions {
		if admission.receipt == nil {
			if admission.inlinePath == "" && admission.checkout == nil {
				outcomes[i] = admission.immediate
			} else {
				outcomes[i] = s.awaitSourceMutations(withoutDispatchSlotRelease(waitCtx), admission)
			}
		} else {
			files = append(files, batchTransactionFile{Path: admission.receipt.path, ReindexReceipt: admission.receipt.id})
		}
	}
	if len(files) == 0 {
		// A selected checkout fast-ACKs one admitted operation. Even when
		// it touched many files, the tagged legacy regression waits once.
		for _, outcome := range outcomes {
			if outcome.Err == nil && outcome.Receipt != "" {
				err := editGateCheckoutACKSeed(ctx, s)
				if err != nil {
					for i := range outcomes {
						if outcomes[i].Receipt != "" {
							outcomes[i].Err = err
						}
					}
				}
				break
			}
		}
		return outcomes
	}
	resumeIntent := suspendSourceMutationWriteIntent(ctx)
	defer resumeIntent()
	if len(admissions) > 0 && admissions[0].policy.freshness.requireFresh {
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		err := s.awaitSourceAckTier(ctx, admissions[0].policy, paths, s.batchReceiptsPublished(ctx, files))
		for i, admission := range admissions {
			if admission.receipt != nil {
				outcomes[i] = admission.receipt.outcome(true)
				if err != nil {
					outcomes[i].Err = err
				}
			}
		}
		return outcomes
	}
	if len(admissions) > 0 && admissions[0].policy.local() {
		for i, admission := range admissions {
			if admission.receipt != nil {
				outcomes[i] = admission.receipt.outcome(true)
			}
		}
		if err := editGateCheckoutACKSeed(ctx, s); err != nil {
			for i := range outcomes {
				if admissions[i].receipt != nil {
					outcomes[i].Err = err
				}
			}
		}
		return outcomes
	}
	lap := s.beginAcknowledgementLap(ctx, editAckReindexWait)
	if err := editGateACKSeed(ctx, s); err != nil {
		lap.finish()
		for i, admission := range admissions {
			if admission.receipt != nil {
				outcomes[i] = admission.receipt.outcome(true)
				outcomes[i].Err = err
			}
		}
		return outcomes
	}

	if s.releaseDispatchSlotForWait(waitCtx, s.batchReceiptsPublished(waitCtx, files)) {
	wait:
		for _, admission := range admissions {
			if admission.receipt != nil {
				select {
				case <-admission.receipt.done:
				case <-waitCtx.Done():
					break wait
				}
			}
		}
	}
	lap.finish()
	for i, admission := range admissions {
		if admission.receipt != nil {
			outcomes[i] = admission.receipt.outcome(true)
			if !outcomes[i].Pending {
				outcomes[i].Receipt = ""
			}
		}
	}
	return outcomes
}

// An operation may rewrite one file more than once (propagated callsites then
// a definition). Only its final physical version can own a publication ticket.
// Intermediate physical receipts remain historical, without invented graph
// success for bytes no publication actually read.
type committedSourceMutation struct {
	target sourceMutationTarget
	record *mutationCommitRecord
}

func captureCommittedSourceMutation(ctx context.Context, path string, record *mutationCommitRecord) committedSourceMutation {
	return committedSourceMutation{target: captureSourceMutationTarget(ctx, path), record: record}
}

func (s *Server) completeCommittedSourceMutations(ctx context.Context, writes []committedSourceMutation, releasePaths func()) ([]committedSourceMutation, []mutationReindexOutcome) {
	final := make([]committedSourceMutation, 0, len(writes))
	positions := make(map[string]int, len(writes))
	for _, write := range writes {
		if at, ok := positions[write.target.key]; ok {
			final[at] = write
		} else {
			positions[write.target.key] = len(final)
			final = append(final, write)
		}
	}
	targets := make([]sourceMutationTarget, len(final))
	for i, write := range final {
		targets[i] = write.target
	}
	admissions := s.admitSourceMutationFrontier(ctx, targets)
	releasePaths()
	outcomes := s.awaitSourceMutationFrontier(ctx, admissions)
	for i, outcome := range outcomes {
		final[i].record.recordGraph(outcome)
	}
	return final, outcomes
}
