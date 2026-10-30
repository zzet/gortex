package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

// This first measured shape is one existing-file after-image. Other batch
// operations retain their ordinary semantics without acquiring invented file
// clocks. The durable transaction, never this bounded observation, is source
// authority; duplicate/recovery arms do not call this helper.
func (s *Server) beginBatchFileObservation(ctx context.Context, receipt batchTransactionReceipt, buffers map[string]*batchFileBuffer, paths []string) *mutationCommitRecord {
	ack := editAcknowledgementFrom(ctx, s)
	if ack == nil || len(paths) != 1 || len(receipt.Files) != 1 || len(receipt.Results) != 1 || receipt.Results[0].Op != "edit_file" {
		return nil
	}
	buffer := buffers[paths[0]]
	if buffer == nil || !buffer.existsBefore || !buffer.existsAfter || receipt.Files[0].Path != buffer.absPath {
		return nil
	}
	record := &mutationCommitRecord{
		kind: batchFileObservationCommit, owner: s,
		id:   "batch-file-" + strconv.FormatUint(mutationCommitSequence.Add(1), 10),
		tool: "batch_edit", key: receipt.TransactionID, fingerprint: receipt.Fingerprint,
		absPath: buffer.absPath, relPath: buffer.relPath,
		startedAt: ack.entered, acknowledgement: ack,
		disk: mutationDiskInFlight, graph: mutationGraphPending,
		batchOwner: sha256.Sum256([]byte(receipt.TransactionID)),
		checkoutID: receipt.CheckoutID, checkoutIncarnation: receipt.Incarnation,
	}
	s.mutationCommits.put(record)
	return record
}

// Capture directly around the selected real forward writer. Journal and
// rollback writers keep their original separate calls. No bytes or writer
// closure are retained in the ledger, and an unobserved batch keeps its writer.
func (s *Server) observedBatchFileWriter(record *mutationCommitRecord, writer func(string, []byte, os.FileMode) error) func(string, []byte, os.FileMode) error {
	if record == nil {
		return writer
	}
	return func(path string, content []byte, mode os.FileMode) error {
		started := time.Now()
		err := writer(path, content, mode)
		returned := time.Now()
		blob, after := "", [32]byte{}
		if err == nil {
			blob, after = gitBlobSHA(content), sha256.Sum256(content)
		}
		ledger := &s.mutationCommits
		ledger.mu.Lock()
		record.mu.Lock()
		if ledger.byID[record.id] == record && record.owner == s && record.kind == batchFileObservationCommit {
			if !record.diskWriteStartedAt.IsZero() || !pathkey.EqualPaths(record.absPath, path) {
				record.disk, record.errText = mutationDiskFailed, "unavailable_repeated_or_foreign_writer"
			} else {
				record.diskWriteStartedAt, record.committedAt = started, returned
				if err != nil {
					record.disk, record.errText = mutationDiskFailed, err.Error()
				} else {
					record.newSHA, record.batchAfterSHA256, record.bytesWritten = blob, after, len(content)
				}
			}
		}
		record.mu.Unlock()
		ledger.mu.Unlock()
		return err // observation never changes writer/durability behavior
	}
}

// A successful writer return alone is insufficient: directory durability and
// the actual transaction verdict must also succeed. Keep that verdict separate
// from committedAt's real per-file writer-return clock. Failure/rollback cannot
// become publication ownership, and late finalization cannot resurrect eviction.
func (s *Server) finishBatchFileObservation(record *mutationCommitRecord, receipt batchTransactionReceipt) {
	if record == nil {
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if ledger.byID[record.id] != record || record.owner != s || record.kind != batchFileObservationCommit ||
		record.batchOwner != sha256.Sum256([]byte(receipt.TransactionID)) || record.fingerprint != receipt.Fingerprint {
		return
	}
	if record.disk == mutationDiskCommitted {
		return
	}
	if receipt.Status != "committed" || receipt.DiskStatus != "committed" {
		record.disk, record.graph, record.graphRecorded = mutationDiskFailed, mutationGraphFailed, true
		record.errText = "batch_not_committed"
		return
	}
	if len(receipt.Files) != 1 || receipt.Files[0].Path != record.absPath || receipt.Files[0].AfterAbsent || receipt.Files[0].BeforeAbsent ||
		receipt.Files[0].AfterSHA256 != hex.EncodeToString(record.batchAfterSHA256[:]) || record.disk != mutationDiskInFlight ||
		record.diskWriteStartedAt.IsZero() || record.committedAt.IsZero() || record.committedAt.Before(record.diskWriteStartedAt) || record.newSHA == "" {
		record.disk, record.graph, record.graphRecorded = mutationDiskFailed, mutationGraphFailed, true
		record.errText = "unavailable_writer_or_final_file_identity"
		return
	}
	record.disk = mutationDiskCommitted
	record.commitSeq = mutationDiskCommitSequence.Add(1)
}

// batchPublicationObservation selects only the original measured transaction's
// retained file record. Recovery and duplicate admissions cannot manufacture a
// new file clock or replace a ticket already witnessed by that record.
func (s *Server) batchPublicationObservation(receipt batchTransactionReceipt, path string) *mutationCommitRecord {
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		record := ledger.ordered[i]
		record.mu.RLock()
		match := record.kind == batchFileObservationCommit && record.owner == s &&
			ledger.byID[record.id] == record && record.key == receipt.TransactionID &&
			record.fingerprint == receipt.Fingerprint && pathkey.EqualPaths(record.absPath, path) &&
			record.disk == mutationDiskCommitted && record.commitSeq != 0 && record.reindexReceipt == ""
		record.mu.RUnlock()
		if match {
			return record
		}
	}
	return nil
}

// Keep the observational ticket local to this record; ordinary physical
// mutation lookup/recovery indexes must not acquire batch authority.
func (s *Server) bindBatchFilePublication(record *mutationCommitRecord, outcome mutationReindexOutcome) {
	if record == nil || outcome.Receipt == "" || outcome.Generation == 0 {
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if ledger.byID[record.id] != record || record.owner != s || record.kind != batchFileObservationCommit ||
		record.disk != mutationDiskCommitted || record.reindexReceipt != "" {
		return
	}
	record.reindexReceipt, record.reindexGeneration = outcome.Receipt, outcome.Generation
	record.graph, record.graphRecorded = graphStatusFor(outcome), true
}

// This explicit timing-only variant never substitutes for the durable batch
// status. In particular a successful writer return is not a transaction ACK.
func (s *Server) batchFileObservationPayload(ctx context.Context, id string) (map[string]any, bool) {
	if !strings.HasPrefix(id, "batch-file-") {
		return nil, false
	}
	payload := map[string]any{"found": false, "receipt": id, "receipt_kind": "batch_file_observation",
		"guidance": "timing only; query transaction_id for durable batch and per-file publication outcomes"}
	checkout, incarnation := mutationCheckoutScope(ctx)
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	record := ledger.byID[id]
	var publication *indexer.PublicationPhaseRecord
	var committed time.Time
	var ticket string
	var repair *mutationReadRepairObservation
	if record != nil {
		record.mu.RLock()
		if record.kind == batchFileObservationCommit && record.owner == s &&
			mutationCheckoutScopeMatches(checkout, incarnation, record.checkoutID, record.checkoutIncarnation) {
			payload["found"], payload["transaction_id"], payload["path"] = true, record.key, record.relPath
			payload["batch_disk_verdict"] = record.disk
			if record.newSHA != "" {
				payload["after_sha256"] = hex.EncodeToString(record.batchAfterSHA256[:])
				payload["new_sha"] = record.newSHA
			}
			payload["publication_receipt"], payload["publication_generation"] = record.reindexReceipt, record.reindexGeneration
			payload["writer_return_observed"] = !record.committedAt.IsZero()
			publication, committed, ticket = record.publication, record.committedAt, record.reindexReceipt
			if record.readRepair != nil {
				copy := *record.readRepair
				repair = &copy
			}
		}
		record.mu.RUnlock()
	}
	ledger.mu.Unlock()
	attachMutationPhaseTimings(payload, ticket, committed, indexer.PublicationSourceMCPEdit, publication)
	if repair != nil {
		payload["read_assisted_publication"] = repair.payload()
	}
	payload["acknowledgement_state"] = "not_applicable_shared_transaction_rpc"
	return payload, true
}

type batchFileObservationReference struct {
	Path    string `json:"path"`
	Receipt string `json:"receipt"`
}

func (s *Server) batchFileObservationReferences(transaction batchTransactionReceipt) []batchFileObservationReference {
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		record := ledger.ordered[i]
		record.mu.RLock()
		match := record.kind == batchFileObservationCommit && record.owner == s && ledger.byID[record.id] == record &&
			record.key == transaction.TransactionID && record.fingerprint == transaction.Fingerprint
		reference := batchFileObservationReference{Path: record.relPath, Receipt: record.id}
		record.mu.RUnlock()
		if match {
			return []batchFileObservationReference{reference}
		}
	}
	return nil
}
