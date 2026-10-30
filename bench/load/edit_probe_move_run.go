package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

func (r *editProbeRuntime) movePaths() map[string]editProbeMovePath {
	out := make(map[string]editProbeMovePath, 2)
	for role, path := range map[string]string{"source": r.plan.File, "target": r.p.Repos[0].Corpus.EditDestination} {
		out[role] = editProbeMovePath{Relative: path, Qualified: r.p.Repos[0].Prefix + "/" + path, Absolute: filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(path))}
	}
	return out
}

func movePublicFactsComplete(record editProbeRecord) bool {
	if len(record.Readbacks) != 4 || len(record.FirstCorrect) != 8 {
		return false
	}
	readbacks := map[string]bool{"file": false, "source": false, "old_source_absent": false, "source_file_after_move": false}
	for _, readback := range record.Readbacks {
		seen, known := readbacks[readback.Kind]
		if !known || seen || !readback.Correct {
			return false
		}
		readbacks[readback.Kind] = true
	}
	return moveProbesComplete(record)
}

func moveProbesComplete(record editProbeRecord) bool {
	if len(record.FirstCorrect) != 8 { return false }
	for _, name := range []string{"source", "summary", "editing_context", "search_visible", "text", "callers", "call_chain", "old_source_absent"} {
		first, present := record.FirstCorrect[name]
		if !present || first.Value == nil { return false }
	}
	return true
}

// Only the original, exactly identified per-file owner can freeze a terminal
// refusal. Missing/foreign rows and late pending callbacks remain collecting.
func moveOwnedRefusal(row map[string]any, refs editProbeMoveReferences, file editProbeMoveFileReference) bool {
	state, _ := row["direct_reconcile_state"].(string)
	return row["found"] == true && row["receipt_kind"] == "move_file_observation" && row["receipt"] == file.Receipt && row["operation_id"] == refs.OperationID && row["source_id"] == refs.SourceID && row["target_id"] == refs.TargetID && row["path"] == file.Path && row["role"] == file.Role && strings.HasPrefix(state, "unavailable_")
}

// Own-ACK readbacks precede these separate observation queries. Shared A is
// validated once across the two original files; no physical P/B fallback exists.
func (r *editProbeRuntime) collectMoveEvidence(ctx context.Context, s *editProbeState, refs editProbeMoveReferences, wireID int64) {
	for ctx.Err() == nil {
		statuses := make(map[string]map[string]any, 2)
		refused := false
		if refs.validateReferences() == nil {
			for i, file := range refs.Files {
				req := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": file.Receipt}}}
				raw, _, err := r.perform(ctx, s, s.client, req, "move_file_status:"+file.Role, "probe", 200+i)
				if errors.Is(err, errEditProbeEvidenceIncomplete) {
					return
				}
				if err == nil {
					row, decodeErr := decodeEditProbeResult(raw)
					if decodeErr == nil {
						statuses[file.Role] = row
						refused = refused || moveOwnedRefusal(row, refs, file)
					}
				}
			}
		}
		accounting := checkEditProbeMoveAccounting(statuses, refs, wireID)
		s.mu.Lock()
		s.record.MoveAccounting = &accounting
		all := movePublicFactsComplete(s.record)
		if accounting.State == "observed_move_windows_checks_pass" {
			s.record.GraphTerminal = true
			s.record.GraphTerminalBasis = "original_move_files_actual_accepted_direct_reconcile"
		} else if !r.requireAccounting && all {
			// Old controls may have no instrumentation. Public facts establish only
			// functional completion, never an observed graph receipt or zero phases.
			s.record.GraphTerminalBasis = "baseline_complete_public_move_facts_no_receipt_proof"
		}
		done := moveProbesComplete(s.record) && (!r.requireAccounting || accounting.State == "observed_move_windows_checks_pass" || refused || refs.ReferenceError != "")
		s.mu.Unlock()
		if done {
			return
		}
		if waitContext(ctx, editProbePeriod) != nil {
			return
		}
	}
}
