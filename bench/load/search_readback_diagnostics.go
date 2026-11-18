package main

import (
	"context"
	"time"
)

const searchAcceptanceDiagnosticBudget = time.Second

// Called only after every offered required-turn continuation has joined. This
// is one serial observer, not one background worker/session per offered edit.
// Its deadline is clipped by the unchanged original phase window; it never
// provides ACK/search, canonical, accounting or other-issue acceptance credit.
func (r *editProbeRuntime) collectSearchAcceptanceDiagnostics(ctx context.Context) {
	if len(r.states) == 0 {
		return
	}
	client := r.states[0].client
	if client == nil {
		return // No initialized optional observer; no synthetic observation.
	}
	diagnosticCtx, cancel := context.WithTimeout(ctx, searchAcceptanceDiagnosticBudget)
	defer cancel()
	for editor := range r.editors {
		var selected *editProbeState
		for _, state := range r.states {
			if state.step.Editor == editor && searchReadbackObjectACK(state.record.SearchACK) {
				selected = state // Last acknowledged offer in original plan order.
			}
		}
		if selected == nil {
			continue
		}
		repo, err := r.repoFor(selected.step)
		if err != nil {
			continue
		}
		file := repo.Corpus.File
		for _, item := range []struct {
			kind string
			req  request
		}{
			{"file", request{Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": file}}}},
			{"source", request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": file + "::" + selected.step.Marker}, "options": map[string]any{"context_lines": 0}}}},
			{"receipt", request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": selected.record.Receipt}}}},
		} {
			if diagnosticCtx.Err() != nil {
				_ = selected.emit(map[string]any{"event": "search_acceptance_diagnostic", "kind": item.kind, "state": "unavailable", "reason": diagnosticCtx.Err().Error(), "observation_only": true})
				continue
			}
			caller := &editProbeCaller{runtime: r, state: selected, client: client, purpose: "diagnostic:" + item.kind, request: item.req, role: "probe"}
			payload, callErr := caller.call(diagnosticCtx, "tools/call", map[string]any{"name": item.req.Tool, "arguments": dispatchArgs(item.req, repo)})
			if callErr == nil {
				callErr = checkToolResult(payload)
			}
			row := map[string]any{"event": "search_acceptance_diagnostic", "kind": item.kind, "state": "observed", "wire_id": caller.last.ID, "observation_only": true}
			if callErr != nil {
				row["state"], row["reason"] = "unavailable", callErr.Error()
			}
			_ = selected.emit(row) // Persistent sink errors still fail finish().
		}
	}
}
