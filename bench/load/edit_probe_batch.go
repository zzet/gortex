package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
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
	return editProbeBatchContentRequest(c, before, after, transactionID), after
}

func editProbeBatchContentRequest(c Corpus, before, after, transactionID string) request {
	return request{Tool: "edit", Operation: "batch", Arguments: map[string]any{
		"operation": "batch",
		"changes":   []any{map[string]any{"op": "edit_file", "path": c.File, "old_string": before, "new_string": after}},
		"options":   map[string]any{"transaction_id": transactionID},
	}}
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
	var observation any
	if rows, ok := body["file_observations"].([]any); ok && len(rows) == 1 {
		observation = rows[0]
	}
	return decodeEditProbeBatchMember(body, files[0], results[0], observation, transactionID, absPath, relativePath, offered)
}

// Select actual rows from an already validated complete journal response; no
// status fields or clocks are synthesized to impersonate an ordinary mutation.
func decodeEditProbeBatchMember(body map[string]any, fileValue, resultValue, observation any, transactionID, absPath, relativePath string, offered []byte) (editProbeBatchReferences, error) {
	out := editProbeBatchReferences{}
	file, ok := fileValue.(map[string]any)
	if !ok {
		return out, errors.New("invalid durable file")
	}
	result, ok := resultValue.(map[string]any)
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
	if row, ok := observation.(map[string]any); ok && row["path"] == relativePath {
		out.Observation, _ = row["receipt"].(string)
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
	return checkEditProbeAccountingWithAcknowledgement(fileStatus, r.Observation, "publication_generation", shared, true)
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
				s.record.PhasesMS = editProbePhases(file, "batch file status exposes no phases_ms")
				s.record.Derived = editProbeDerived(file)
				s.record.GraphTerminal = terminal
				if terminal {
					r.settlePendingLocked(s)
				}
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

// A complete offered frontier is retained separately from the shared ACK.
// Each file references its original physical member, never a share of E/B/A.
type editProbeBatchIntentMember struct {
	Path, RelativePath string
	Offered            []byte
}

type editProbeBatchSetReferences struct {
	TransactionID   string                     `json:"transaction_id"`
	Fingerprint     string                     `json:"fingerprint"`
	Acknowledgement string                     `json:"acknowledgement_receipt"`
	RPCID           string                     `json:"bound_server_rpc_id"`
	WireID          string                     `json:"actual_wire_id"`
	ReferenceError  string                     `json:"reference_error,omitempty"`
	Files           []editProbeBatchReferences `json:"files"`
}

type editProbeBatchMemberAccounting struct {
	Receipt              string                    `json:"receipt"`
	Path                 string                    `json:"path"`
	AcknowledgementState string                    `json:"acknowledgement_state"`
	Shared               *editProbeSharedReference `json:"shared_owner,omitempty"`
	Reasons              []string                  `json:"reasons,omitempty"`
}

type editProbeBatchSetAccounting struct {
	State   string                           `json:"state"`
	Files   []editProbeBatchMemberAccounting `json:"files"`
	Reasons []string                         `json:"reasons,omitempty"`
}

func editProbeBatchSetIntent(req request, root, prefix string) (string, []editProbeBatchIntentMember, error) {
	if req.Tool != "edit" || req.Operation != "batch" || !filepath.IsAbs(root) || prefix == "" {
		return "", nil, errors.New("batch set has no owned primary intent")
	}
	options, _ := req.Arguments["options"].(map[string]any)
	id, _ := options["transaction_id"].(string)
	changes, ok := req.Arguments["changes"].([]any)
	if id == "" || !ok || len(changes) < 2 {
		return "", nil, errors.New("complete multi-file batch intent unavailable")
	}
	seen := map[string]bool{}
	var members []editProbeBatchIntentMember
	for _, value := range changes {
		change, ok := value.(map[string]any)
		if !ok || change["op"] != "edit_file" {
			return "", nil, errors.New("unsupported batch set operation")
		}
		path, _ := change["path"].(string)
		before, beforeOK := change["old_string"].(string)
		after, afterOK := change["new_string"].(string)
		if path == "" || !beforeOK || !afterOK || before == "" || after == "" {
			return "", nil, errors.New("batch whole-file member intent unavailable")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, strings.TrimPrefix(filepath.ToSlash(path), prefix+"/"))
		}
		path = filepath.Clean(path)
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || seen[path] {
			return "", nil, errors.New("foreign or repeated batch member intent")
		}
		seen[path] = true
		members = append(members, editProbeBatchIntentMember{Path: path, RelativePath: filepath.ToSlash(filepath.Join(prefix, rel)), Offered: []byte(after)})
	}
	return id, members, nil
}

func decodeEditProbeBatchSetAcknowledgement(body map[string]any, transaction string, intents []editProbeBatchIntentMember, rpcID string) (editProbeBatchSetReferences, error) {
	fingerprint, _ := body["fingerprint"].(string)
	out := editProbeBatchSetReferences{TransactionID: transaction, Fingerprint: fingerprint, WireID: rpcID}
	if err := editProbeSharedReplyError(body); err != nil {
		return out, err
	}
	files, fok := body["files"].([]any)
	results, rok := body["results"].([]any)
	observations, ook := body["file_observations"].([]any)
	if !ook || len(observations) != len(intents) {
		out.ReferenceError = "complete original batch observations unavailable"
	}
	if transaction == "" || !editProbeSharedHash(fingerprint) || body["transaction_id"] != transaction || body["status"] != "committed" || body["disk_status"] != "committed" || len(intents) < 2 || !fok || !rok || len(files) != len(intents) || len(results) != len(intents) {
		return out, errors.New("batch set lacks its complete committed journal frontier")
	}
	byPath, byResult, byObservation := map[string]any{}, map[string]any{}, map[string]any{}
	for _, value := range files {
		row, ok := value.(map[string]any)
		path, _ := row["path"].(string)
		if !ok || path == "" || byPath[path] != nil {
			return out, errors.New("ambiguous durable batch files")
		}
		byPath[path] = value
	}
	for _, value := range results {
		row, ok := value.(map[string]any)
		path, _ := row["path"].(string)
		if !ok || path == "" || byResult[path] != nil {
			return out, errors.New("ambiguous applied batch results")
		}
		byResult[path] = value
	}
	for _, value := range observations {
		row, ok := value.(map[string]any)
		path, _ := row["path"].(string)
		if !ok || path == "" || byObservation[path] != nil {
			out.ReferenceError = "ambiguous batch observation paths"
			continue
		}
		byObservation[path] = value
	}
	seenPath, seenReceipt, seenTicket := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, intent := range intents {
		if seenPath[intent.Path] {
			return out, errors.New("duplicate offered batch path")
		}
		seenPath[intent.Path] = true
		ref, err := decodeEditProbeBatchMember(body, byPath[intent.Path], byResult[intent.RelativePath], byObservation[intent.RelativePath], transaction, intent.Path, intent.RelativePath, intent.Offered)
		if err != nil {
			return out, err
		}
		if err = ref.validateReferences(); err != nil {
			out.ReferenceError = err.Error()
		}
		if seenReceipt[ref.Observation] || seenTicket[ref.GraphReceipt] {
			out.ReferenceError = "batch members share a physical receipt or publication"
		}
		seenReceipt[ref.Observation], seenTicket[ref.GraphReceipt] = true, true
		if out.Acknowledgement != "" && out.Acknowledgement != ref.Acknowledgement {
			out.ReferenceError = "batch shared ACK differs across members"
		}
		out.Acknowledgement = ref.Acknowledgement
		out.Files = append(out.Files, ref)
	}
	// The server's bound-call counter is a distinct domain from JSON-RPC
	// wire IDs. The original typed ACK receipt carries that server counter.
	counter := strings.TrimPrefix(out.Acknowledgement, "batch-ack-")
	n, counterErr := strconv.ParseUint(counter, 10, 64)
	if counterErr != nil || n == 0 || counter == out.Acknowledgement {
		out.ReferenceError = "original batch server RPC owner unavailable"
	} else {
		out.RPCID = "rpc-" + strconv.FormatUint(n, 10)
	}
	if rpcID == "" {
		out.ReferenceError = "batch actual wire identity unavailable"
	}
	return out, nil
}

func (r editProbeBatchSetReferences) validateReferences() error {
	if r.ReferenceError != "" {
		return errors.New(r.ReferenceError)
	}
	if len(r.Files) < 2 || r.RPCID == "" || r.WireID == "" || r.Acknowledgement == "" {
		return errors.New("original complete batch timing references unavailable")
	}
	for _, file := range r.Files {
		if err := file.validateReferences(); err != nil {
			return err
		}
	}
	return nil
}

// This validator is invoked ONLY by the batch-bound shared proof route.
// A journal ACK or a saved owner dictionary cannot authorize another member.
func (r editProbeBatchSetReferences) validateSharedMember(status map[string]any, member editProbeSharedMember, members []editProbeSharedMember) error {
	if err := r.validateReferences(); err != nil {
		return err
	}
	if len(r.Files) < 2 || len(r.Files) != len(members) {
		return errors.New("batch shared owner frontier count differs from original journal")
	}
	seen := map[string]bool{}
	var current *editProbeBatchReferences
	for i := range r.Files {
		ref := &r.Files[i]
		if ref.TransactionID != r.TransactionID || ref.Acknowledgement != r.Acknowledgement || seen[ref.Observation] {
			return errors.New("batch source frontier identity changed")
		}
		seen[ref.Observation] = true
		found := false
		for _, m := range members {
			if m.Receipt == ref.Observation && m.Path == ref.Path && m.ContentSHA256 == ref.AfterSHA256 && m.Ticket == ref.GraphGeneration {
				found = true
			}
		}
		if !found {
			return errors.New("shared member differs from original offered journal file")
		}
		if ref.Observation == member.Receipt {
			current = ref
		}
	}
	if current == nil {
		return errors.New("batch status outside original complete frontier")
	}
	if err := current.validateFileObservation(status); err != nil {
		return err
	}
	applied, err := strconvEditProbeUint(status["applied_generation"])
	publication, _ := status["publication_phases"].(map[string]any)
	ticket, ticketErr := strconvEditProbeUint(publication["ticket"])
	if err != nil || applied != member.Ticket || ticketErr != nil || ticket != member.Ticket || status["publication_fresh"] != true || status["publication_pending"] != false || publication["key"] != member.PublicationKey || publication["source"] != "mcp_edit" || publication["clock"] != "monotonic" || publication["terminal"] != true {
		return errors.New("batch original publication is not terminal/current")
	}
	if status["acknowledgement"] != nil || status["disk_status"] != nil || status["commit_seq"] != nil {
		return errors.New("batch observation claims an ordinary mutation or per-file ACK domain")
	}
	return nil
}

func (r *editProbeRuntime) accountingForBatchSetStatuses(ctx context.Context, statuses map[string]map[string]any, ack map[string]any, refs editProbeBatchSetReferences) (editProbeAccounting, *editProbeBatchSetAccounting) {
	out := editProbeAccounting{State: "unavailable_shared_owner"}
	batch := &editProbeBatchSetAccounting{State: "unavailable_batch_shared_owner"}
	fail := func(reason string) {
		out.Reasons = append(out.Reasons, reason)
		batch.Reasons = append(batch.Reasons, reason)
	}
	if len(refs.Files) < 2 {
		fail("complete original batch reference frontier unavailable")
		return out, batch
	}
	if err := editProbeSharedReplyError(ack); err != nil {
		fail(err.Error())
		return out, batch
	}
	if ack["found"] != true || ack["receipt_kind"] != "batch_acknowledgement" || ack["receipt"] != refs.Acknowledgement {
		fail("original shared transaction ACK unavailable")
		return out, batch
	}
	shared, _ := ack["acknowledgement"].(map[string]any)
	if shared == nil || shared["rpc_id"] != refs.RPCID || refs.RPCID == "" {
		fail("transaction ACK does not match actual offered RPC")
		return out, batch
	}
	// Validate the actual shared RPC once using the existing independent A
	// accounting rules; never put its intervals in a file or shared E/B owner.
	first := refs.Files[0]
	out = checkEditProbeAcknowledgement(statuses[first.Observation], first.Observation, "publication_generation", shared, true)
	out.State = "unavailable_shared_owner"
	// A's applicability failures remain in the overall result. They cannot
	// erase or manufacture independently observed shared execution evidence.
	if states, ok := shared["phase_states"].(map[string]any); ok && states["reindex_wait"] == "unobserved" {
		counts, _ := shared["counts"].(map[string]any)
		wait, _ := counts["reindex_wait"].(map[string]any)
		started, se := strconvEditProbeUint(wait["started"])
		completed, ce := strconvEditProbeUint(wait["completed"])
		na, ne := strconvEditProbeUint(wait["not_applicable"])
		if se == nil && ce == nil && ne == nil && started == 0 && completed == 0 && na == 0 {
			out.Reasons = append(out.Reasons, "batch shared A reindex_wait is unobserved with counts 0/0/0 and no applicability proof")
		}
	}
	if err := refs.validateReferences(); err != nil {
		fail(err.Error())
		return out, batch
	}
	if len(statuses) != len(refs.Files) {
		fail("complete original batch status frontier unavailable")
	}
	for _, file := range refs.Files {
		status, ok := statuses[file.Observation]
		entry := editProbeBatchMemberAccounting{Receipt: file.Observation, Path: file.RelativePath, AcknowledgementState: "unavailable"}
		if state, ok := status["acknowledgement_state"].(string); ok {
			entry.AcknowledgementState = state
		}
		if !ok {
			entry.Reasons = append(entry.Reasons, "original batch member status missing")
		} else if err := file.validateFileObservation(status); err != nil {
			entry.Reasons = append(entry.Reasons, err.Error())
		} else {
			ref, _, err := r.referenceForSharedBatchStatus(ctx, status, file.Observation, "", &refs)
			if err != nil {
				entry.Reasons = append(entry.Reasons, err.Error())
			} else {
				entry.Shared = ref
			}
		}
		for _, reason := range entry.Reasons {
			fail(file.RelativePath + ": " + reason)
		}
		batch.Files = append(batch.Files, entry)
	}
	if len(batch.Reasons) == 0 && len(batch.Files) == len(refs.Files) {
		batch.State = "observed_batch_shared_owner_windows_checks_pass"
		if out.A != nil && len(out.Reasons) == 0 {
			out.State = "observed_shared_owner_windows_checks_pass"
		} else {
			out.State = "terminal_unavailable_shared_owner"
		}
	}
	return out, batch
}

func (r *editProbeRuntime) collectBatchSetEvidence(ctx context.Context, s *editProbeState, refs editProbeBatchSetReferences, intents []editProbeBatchIntentMember) {
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
			current, decodeErr := decodeEditProbeBatchSetAcknowledgement(tx, refs.TransactionID, intents, refs.WireID)
			if decodeErr != nil {
				s.mu.Lock()
				s.record.Errors = append(s.record.Errors, "batch set source identity: "+decodeErr.Error())
				s.mu.Unlock()
				return
			}
			if current.Fingerprint != refs.Fingerprint || current.Acknowledgement != refs.Acknowledgement {
				s.mu.Lock()
				s.record.Errors = append(s.record.Errors, "batch journal identity changed")
				s.mu.Unlock()
				return
			}
			for i := range refs.Files {
				if current.Files[i] != refs.Files[i] {
					s.mu.Lock()
					s.record.Errors = append(s.record.Errors, "batch original frontier was replaced or readmitted")
					s.mu.Unlock()
					return
				}
			}
			statuses := map[string]map[string]any{}
			for i, file := range refs.Files {
				row, rowErr := perform(request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": file.Observation}}}, "batch_file_status", 201+i)
				if errors.Is(rowErr, errEditProbeEvidenceIncomplete) {
					return
				}
				if rowErr == nil {
					statuses[file.Observation] = row
				}
			}
			ack, ackErr := perform(request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": refs.Acknowledgement}}}, "batch_ack_status", 201+len(refs.Files))
			if errors.Is(ackErr, errEditProbeEvidenceIncomplete) {
				return
			}
			if ackErr == nil {
				accounting, batch := r.accountingForBatchSetStatuses(ctx, statuses, ack, refs)
				terminal := tx["graph_status"] == "fresh" || tx["graph_status"] == "failed"
				s.mu.Lock()
				s.record.Accounting, s.record.BatchAccounting = accounting, batch
				s.record.GraphTerminal = terminal
				if terminal {
					r.settlePendingLocked(s)
				}
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
				if terminal && tx["graph_status"] != "fresh" || editProbeFinished(terminal, all, r.requireAccounting, accounting) {
					return
				}
			}
		}
		if waitContext(ctx, editProbePeriod) != nil {
			return
		}
	}
}
