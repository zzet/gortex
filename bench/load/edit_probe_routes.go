package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Multi-checkout cleanup never combines common-prefix paths into editor 0's
// view. Each admitted original has its own editor session and transaction.
func (r *editProbeRuntime) restoreRoutes(ctx context.Context) (resultErr error) {
	if r.plan.Kind != "add_intrafile_call" || r.plan.Tool != "edit.file" || r.plan.Mode != "json" || r.plan.Sender != "" {
		return errors.New("routed restore supports ordinary add_intrafile_call/edit.file JSON only")
	}
	type restoredRoute struct {
		Route    string          `json:"route"`
		Editor   int             `json:"editor"`
		File     string          `json:"file"`
		Response json.RawMessage `json:"response"`
		Error    string          `json:"error,omitempty"`
	}
	var rows []restoredRoute
	started := time.Now()
	defer func() {
		r.phase.EditProbe.RestoreMS = measured(float64(time.Since(started))/float64(time.Millisecond), "ms")
		data, err := json.Marshal(rows)
		r.phase.EditProbe.Restore = data
		resultErr = errors.Join(resultErr, err)
	}()
	for ordinal, repo := range r.p.Repos {
		original, found := r.originals[repo.Path]
		if !found {
			return errors.New("routed restore has no admitted original for checkout")
		}
		editor := -1
		for i, client := range r.editors {
			if client != nil && filepath.Clean(client.cwd) == filepath.Clean(repo.Path) {
				editor = i
				break
			}
		}
		if editor < 0 {
			return errors.New("routed restore has no editor for checkout")
		}
		file := filepath.Join(repo.Path, filepath.FromSlash(repo.Corpus.File))
		current, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		changes := []any{map[string]any{"op": "edit_file", "path": repo.Prefix + "/" + repo.Corpus.File, "old_string": string(current), "new_string": string(original)}}
		req := request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": fmt.Sprintf("load-restore-%016x-route-%d", uint64(r.options.Seed), ordinal)}, "output": map[string]any{"format": "json", "max_bytes": 0}}}
		data, _, err := callEditProbeTool(ctx, r.editors[editor], req, req.Arguments, "cleanup", ordinal, len(r.states), 0, r.options.Deadline, editProbeRetrySleep(r.options.Seed, len(r.states), ordinal))
		row := restoredRoute{Route: repo.Corpus.Label, Editor: editor, File: repo.Corpus.File, Response: data}
		if err == nil {
			var restored []byte
			restored, err = os.ReadFile(file)
			if err == nil && string(restored) != string(original) {
				err = errors.New("public routed restore did not restore its checkout's original bytes")
			}
		}
		if err != nil {
			row.Error = err.Error()
		}
		rows = append(rows, row)
		if err != nil {
			return err
		}
	}
	return nil
}
