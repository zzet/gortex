package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// coChangeSetupPoll is the wait between reads while the startup mine runs.
	coChangeSetupPoll = 5 * time.Second
	// coChangeSetupReplyLimit bounds the retained setup receipt.
	coChangeSetupReplyLimit = 16 << 10
)

// warmEditCoChange waits for the daemon's process-wide startup co-change mine
// without starting a new one: the public facade fixes refresh=false. The mine
// commits graph writes after enrichment, so settling it first keeps those
// writes out of the analysis gates and the offered edits. The last bounded
// reply is returned as the setup receipt.
func warmEditCoChange(ctx context.Context, client toolCaller, repo privateRepo, deadline time.Duration, wait func(context.Context, time.Duration) error) (json.RawMessage, []Attempt, error) {
	req := request{Tool: "analyze", Operation: "co_change", Arguments: map[string]any{
		"kind":    "co_change",
		"target":  map[string]any{"file": repo.Corpus.File},
		"options": map[string]any{"limit": 1},
	}}
	args := dispatchArgs(req, repo)
	expectedFile := repo.Prefix + "/" + repo.Corpus.File
	var last json.RawMessage
	var attempts []Attempt
	for {
		result, rows, err := callTool(ctx, client, req, args, "probe", 0, 0, 0, deadline, waitContext)
		attempts = append(attempts, rows...)
		if err != nil {
			return last, attempts, fmt.Errorf("co-change setup probe: %w", err)
		}
		payload, err := textPayload(result, "content")
		if err != nil {
			return last, attempts, fmt.Errorf("co-change setup response: %w", err)
		}
		if len(payload) > coChangeSetupReplyLimit {
			return last, attempts, errors.New("co-change setup response exceeded evidence bound")
		}
		last = payload
		var state struct {
			TargetFile string            `json:"target_file"`
			Rows       []json.RawMessage `json:"co_changing"`
			Pending    json.RawMessage   `json:"mining_in_progress"`
			Failed     json.RawMessage   `json:"mining_failed"`
			MineError  json.RawMessage   `json:"mining_error"`
		}
		if err := json.Unmarshal(last, &state); err != nil {
			return last, attempts, fmt.Errorf("co-change setup response: %w", err)
		}
		if state.TargetFile != expectedFile {
			return last, attempts, errors.New("co-change setup response has wrong target")
		}
		if state.Rows == nil || len(state.Rows) > 1 {
			return last, attempts, errors.New("co-change setup response has invalid bounded collection")
		}
		switch {
		case state.Pending == nil && state.Failed == nil && state.MineError == nil:
			return last, attempts, nil
		case string(state.Pending) == "false" && string(state.Failed) == "true" && len(state.Rows) == 0:
			return last, attempts, errors.New("co-change setup mining failed")
		case string(state.Pending) == "true" && state.Failed == nil && state.MineError == nil && len(state.Rows) == 0:
			if err := wait(ctx, coChangeSetupPoll); err != nil {
				return last, attempts, fmt.Errorf("co-change setup stayed pending: %w", err)
			}
		default:
			return last, attempts, errors.New("co-change setup response has invalid mining status")
		}
	}
}
