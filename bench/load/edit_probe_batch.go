package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// Batch source authority is the durable transaction. The acknowledgement RPC
// and this file's graph ticket have separate clocks/retention and are never
// represented as one synthetic mutation_receipt.
type editProbeBatchReferences struct {
	TransactionID        string `json:"transaction_id"`
	Observation          string `json:"file_observation_receipt"`
	Acknowledgement      string `json:"acknowledgement_receipt"`
	Path                 string `json:"path"`
	RelativePath         string `json:"relative_path"`
	AfterSHA256          string `json:"after_sha256"`
	GraphReceipt         string `json:"reindex_receipt"`
	GraphGeneration      uint64 `json:"reindex_generation"`
	ResultReceipt        string `json:"result_reindex_receipt"`
	ResultGeneration     uint64 `json:"result_reindex_generation"`
	AcknowledgementState string `json:"acknowledgement_state"`
	TransactionError     string `json:"transaction_error,omitempty"`
}

// This explicit fixture offers whole-file before/after bytes. It is not the
// generic anchor-splice batch shape: the same precomputed prefix as edit.write
// makes its durable after-image provable without reading later server state.
func editProbeBatchRequest(c Corpus, before, marker, transactionID string) (request, string) {
	_, after := editProbeWriteRequest(c, before, marker)
	return request{Tool: "edit", Operation: "batch", Arguments: map[string]any{
		"operation": "batch",
		"changes":   []any{map[string]any{"op": "edit_file", "path": c.File, "old_string": before, "new_string": after}},
		"options":   map[string]any{"transaction_id": transactionID},
	}}, after
}

// Only the pointed one-file edit_file shape is supported here. Missing timing
// references do not negate a genuine committed ACK: validateReferences below
// reports those independently, so unavailable accounting cannot become zero or
// remove the successful source acknowledgement from its denominator.
func decodeEditProbeBatchAcknowledgement(body map[string]any, transactionID, absPath, relativePath string, offered []byte) (editProbeBatchReferences, error) {
	out := editProbeBatchReferences{}
	if transactionID == "" || body["transaction_id"] != transactionID || body["status"] != "committed" || body["disk_status"] != "committed" {
		return out, errors.New("batch transaction is not the requested committed source acknowledgement")
	}
	files, ok := body["files"].([]any)
	if !ok || len(files) != 1 {
		return out, errors.New("one-file batch acknowledgement requires exactly one durable file")
	}
	results, ok := body["results"].([]any)
	if !ok || len(results) != 1 {
		return out, errors.New("one-file batch acknowledgement requires exactly one applied result")
	}
	file, ok := files[0].(map[string]any)
	if !ok {
		return out, errors.New("invalid durable file")
	}
	result, ok := results[0].(map[string]any)
	if !ok || result["op"] != "edit_file" || result["status"] != "applied" || result["path"] != relativePath {
		return out, errors.New("batch result does not identify the applied requested edit_file")
	}
	path, _ := file["path"].(string)
	rel, _ := file["relative_path"].(string)
	if !filepath.IsAbs(absPath) || filepath.Clean(absPath) != absPath || path != absPath || rel != relativePath || relativePath == "" || file["after_absent"] == true || file["before_absent"] == true {
		return out, errors.New("batch durable file identity does not match the existing requested file")
	}
	for _, name := range []string{"after_absent", "before_absent"} {
		if value, present := file[name]; present {
			if absent, ok := value.(bool); !ok || absent {
				return out, errors.New("batch file absence flag contradicts existing-file edit")
			}
		}
	}
	after, _ := file["after_sha256"].(string)
	bytes, err := hex.DecodeString(after)
	if err != nil || len(bytes) != 32 || after != strings.ToLower(after) {
		return out, errors.New("batch durable after-image SHA256 unavailable")
	}
	expected := sha256.Sum256(offered)
	if len(offered) == 0 || after != hex.EncodeToString(expected[:]) {
		return out, errors.New("batch durable after-image does not match exact offered whole-file bytes")
	}
	out.TransactionID, out.Path, out.RelativePath, out.AfterSHA256 = transactionID, path, rel, after
	if value, present := body["error"]; present {
		message, ok := value.(string)
		if !ok {
			return out, errors.New("invalid transaction error")
		}
		out.TransactionError = message
	}
	out.Acknowledgement, _ = body["acknowledgement_receipt"].(string)
	out.AcknowledgementState, _ = body["acknowledgement_state"].(string)
	out.GraphReceipt, _ = file["reindex_receipt"].(string)
	if value, present := file["reindex_generation"]; present {
		generation, err := strconvEditProbeUint(value)
		if err != nil {
			return out, errors.New("invalid batch file generation")
		}
		out.GraphGeneration = generation
	}
	out.ResultReceipt, _ = result["reindex_receipt"].(string)
	if value, present := result["reindex_generation"]; present {
		generation, err := strconvEditProbeUint(value)
		if err != nil {
			return out, errors.New("invalid batch result generation")
		}
		out.ResultGeneration = generation
	}
	if rows, ok := body["file_observations"].([]any); ok && len(rows) == 1 {
		if row, ok := rows[0].(map[string]any); ok && row["path"] == relativePath {
			out.Observation, _ = row["receipt"].(string)
		}
	}
	return out, nil
}

func (r editProbeBatchReferences) validateReferences() error {
	if !strings.HasPrefix(r.Observation, "batch-file-") || r.Observation == "batch-file-" {
		return errors.New("original batch file observation reference unavailable")
	}
	if r.Observation == r.TransactionID || r.Observation == r.Acknowledgement || r.Observation == r.GraphReceipt {
		return errors.New("batch file timing identity borrowed from another domain")
	}

	if !strings.HasPrefix(r.Acknowledgement, "batch-ack-") || r.Acknowledgement == "batch-ack-" || r.AcknowledgementState != "reference_only_query_mutation_status" {
		return errors.New("original shared batch acknowledgement reference unavailable")
	}
	if r.GraphReceipt == "" || r.GraphGeneration == 0 {
		return errors.New("original batch file graph reference unavailable")
	}
	if r.ResultReceipt != r.GraphReceipt || r.ResultGeneration != r.GraphGeneration {
		return errors.New("batch result and durable file graph identities disagree")
	}
	if r.TransactionID == r.Acknowledgement || r.TransactionID == r.GraphReceipt || r.Acknowledgement == r.GraphReceipt {
		return errors.New("batch transaction/acknowledgement/file identities are not distinct")
	}
	return nil
}

// Validate the response-only observation against the durable ACK before any
// phase accounting. This does not replace the transaction's source verdict or
// synthesize a single-file mutation receipt from the shared RPC clock.
func (r editProbeBatchReferences) validateFileObservation(status map[string]any) error {
	if err := r.validateReferences(); err != nil {
		return err
	}
	if status["found"] != true || status["receipt_kind"] != "batch_file_observation" ||
		status["receipt"] != r.Observation || status["transaction_id"] != r.TransactionID ||
		status["path"] != r.RelativePath || status["batch_disk_verdict"] != "committed" ||
		status["after_sha256"] != r.AfterSHA256 || status["publication_receipt"] != r.GraphReceipt ||
		status["writer_return_observed"] != true || status["acknowledgement_state"] != "not_applicable_shared_transaction_rpc" {
		return errors.New("batch file timing does not identify original durable file and publication")
	}
	generation, err := strconvEditProbeUint(status["publication_generation"])
	if err != nil || generation != r.GraphGeneration {
		return errors.New("batch file timing publication generation mismatch")
	}
	return nil
}

func (r editProbeBatchReferences) accounting(fileStatus, acknowledgementStatus map[string]any) editProbeAccounting {
	if err := r.validateFileObservation(fileStatus); err != nil {
		return editProbeAccounting{State: "unavailable", Reasons: []string{err.Error()}}
	}
	if acknowledgementStatus["found"] != true || acknowledgementStatus["receipt"] != r.Acknowledgement || acknowledgementStatus["receipt_kind"] != "batch_acknowledgement" {
		return editProbeAccounting{State: "unavailable", Reasons: []string{"original shared batch ACK owner unavailable"}}
	}
	shared, _ := acknowledgementStatus["acknowledgement"].(map[string]any)
	if shared == nil {
		return editProbeAccounting{State: "unavailable", Reasons: []string{"shared batch ACK intervals unavailable"}}
	}
	return checkEditProbeAccountingWithAcknowledgement(fileStatus, r.Observation, "publication_generation", shared)
}

func editProbeBatchIntent(req request) (string, []byte, error) {
	if req.Tool != "edit" || req.Operation != "batch" {
		return "", nil, errors.New("not the supported edit.batch request")
	}
	options, ok := req.Arguments["options"].(map[string]any)
	if !ok {
		return "", nil, errors.New("batch transaction option unavailable")
	}
	id, _ := options["transaction_id"].(string)
	changes, ok := req.Arguments["changes"].([]any)
	if id == "" || !ok || len(changes) != 1 {
		return "", nil, errors.New("one-file batch offered intent unavailable")
	}
	change, ok := changes[0].(map[string]any)
	if !ok || change["op"] != "edit_file" {
		return "", nil, errors.New("unsupported batch operation")
	}
	before, beforeOK := change["old_string"].(string)
	after, afterOK := change["new_string"].(string)
	if !beforeOK || !afterOK || before == "" || after == "" {
		return "", nil, errors.New("batch whole-file intent unavailable")
	}
	return id, []byte(after), nil
}

// Poll the durable transaction separately from the shared A and original file
// P/B/R owners. This begins only after own-ACK immediate readbacks; neither
// scheduling nor the readback continuation waits for another offered edit.
func (r *editProbeRuntime) collectBatchEvidence(ctx context.Context, s *editProbeState, refs editProbeBatchReferences, offered []byte) {
	perform := func(req request, purpose string, slot int) (map[string]any, error) {
		raw, _, err := r.perform(ctx, s, s.client, req, purpose, "probe", slot)
		if err != nil {
			return nil, err
		}
		return decodeEditProbeResult(raw)
	}
	for ctx.Err() == nil {
		tx, err := perform(request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"operation": "batch", "options": map[string]any{"transaction_id": refs.TransactionID, "status_only": true}}}, "batch_transaction_status", 200)
		if errors.Is(err, errEditProbeEvidenceIncomplete) {
			return
		}
		if err == nil {
			current, decodeErr := decodeEditProbeBatchAcknowledgement(tx, refs.TransactionID, refs.Path, refs.RelativePath, offered)
			if decodeErr != nil {
				s.mu.Lock()
				s.record.Errors = append(s.record.Errors, "batch status source identity: "+decodeErr.Error())
				s.mu.Unlock()
				return
			}
			// A durable recovery may legitimately replace its live publication. That
			// does not authorize borrowing the replacement for the original clocks.
			if current.GraphReceipt != refs.GraphReceipt || current.GraphGeneration != refs.GraphGeneration {
				s.mu.Lock()
				s.record.Errors = append(s.record.Errors, "batch publication was readmitted; original accounting unavailable")
				s.mu.Unlock()
				return
			}
			file, fileErr := perform(request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": refs.Observation}}}, "batch_file_status", 201)
			ack, ackErr := perform(request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": refs.Acknowledgement}}}, "batch_ack_status", 202)
			if errors.Is(fileErr, errEditProbeEvidenceIncomplete) || errors.Is(ackErr, errEditProbeEvidenceIncomplete) {
				return
			}
			if fileErr == nil && ackErr == nil {
				accounting := refs.accounting(file, ack)
				terminal := tx["graph_status"] == "fresh" || tx["graph_status"] == "failed"
				s.mu.Lock()
				s.record.Accounting = accounting
				s.record.GraphTerminal = terminal
				all := true
				for _, value := range s.record.FirstCorrect {
					if value.Value == nil {
						all = false
					}
				}
				if terminal && tx["graph_status"] != "fresh" {
					s.record.Errors = append(s.record.Errors, "batch graph publication failed")
				}
				s.mu.Unlock()
				if terminal && tx["graph_status"] != "fresh" {
					return
				}
				if editProbeFinished(terminal, all, r.requireAccounting, accounting) {
					return
				}
			}
		}
		if waitContext(ctx, editProbePeriod) != nil {
			return
		}
	}
}
