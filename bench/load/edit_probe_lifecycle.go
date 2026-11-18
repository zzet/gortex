package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func editProbeMarker(seed int64, ordinal int) string {
	return fmt.Sprintf("GortexEditProbe%016xN%03d", uint64(seed), ordinal)
}

// Each even ordinal natively creates its own Go file beside the fixture, in
// the fixture's package, whose marker calls the fixture's callee; the next
// ordinal deletes that file again.
func editProbeLifecycleStep(c Corpus, pkg string, seed int64, ordinal int) (r request, file, beforeSymbol string) {
	marker := editProbeMarker(seed, ordinal-ordinal%2)
	file = "gortex_edit_probe_" + marker + ".go"
	if ordinal%2 == 1 {
		return request{Tool: "native", Operation: "delete_file", Arguments: map[string]any{"path": file}}, file, file + "::" + marker
	}
	content := "package " + pkg + "\n" + strings.ReplaceAll(c.EditTemplate, "{{marker}}", marker)
	return request{Tool: "native", Operation: "create_file", Arguments: map[string]any{"path": file, "content": content}}, file, ""
}

// Hot-file pairs share one due time on both fixed editors. The pair stream
// is the single-editor stream, so each editor still offers that mean.
func editProbeHotFileArrivals(seed int64, count int, mean time.Duration) ([]editProbeArrival, error) {
	pairs, err := editProbeArrivals(seed, count/2, 1, mean)
	if err != nil {
		return nil, err
	}
	arrivals := make([]editProbeArrival, 0, count)
	for _, pair := range pairs {
		arrivals = append(arrivals, editProbeArrival{DueNS: pair.DueNS}, editProbeArrival{DueNS: pair.DueNS, Editor: 1})
	}
	return arrivals, nil
}

// A created or deleted file is the step's own fixture file: its marker calls
// the original fixture's callee from another file of the same package.
func editProbeStepCorpus(c Corpus, step editProbeStep) Corpus {
	c.File, c.EditKind, c.EditCallee = step.File, "add_crossfile_call", c.Symbol
	return c
}

// A created file is checked like a crossfile call addition. A deleted file
// keeps only oracles whose answer for a missing file has a pinned contract:
// the exact scoped refusal and complete search, text and caller collections.
func editProbeLifecycleOracles(c Corpus, step editProbeStep) []editProbeOracle {
	created, inner := editProbeStepCorpus(c, step), step
	inner.File = ""
	if !step.Deleted {
		return editProbeOracles(created, inner)
	}
	var oracles []editProbeOracle
	for _, oracle := range editProbeDeleteOracles(created, inner) {
		switch oracle.name {
		case "source", "search_visible", "text", "callers":
			oracles = append(oracles, oracle)
		}
	}
	return editProbeOutputOracles(c, oracles)
}

func editProbeLifecycleCorrect(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	created, inner := editProbeStepCorpus(c, step), step
	inner.File = ""
	if !step.Deleted {
		return editProbeCorrect(oracle, body, created, inner, callee)
	}
	if oracle != "callers" {
		return editProbeDeleteCorrect(oracle, body, created, inner, callee)
	}
	// The stable callee keeps its own node in the fixture file, and no node
	// or edge may still name the deleted file's marker.
	nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
	edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
	truncated, tok := body["truncated"].(bool)
	if !nok || !eok || !tok || truncated {
		return false
	}
	old := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	survivor := false
	for _, raw := range nodes {
		row, ok := raw.(map[string]any)
		id, _ := row["id"].(string)
		if !ok || editProbeSourceID(id, step.File, old) {
			return false
		}
		survivor = survivor || editProbeSourceID(id, c.File, callee)
	}
	for _, raw := range edges {
		row, ok := raw.(map[string]any)
		from, fok := row["from"].(string)
		to, tok := row["to"].(string)
		if !ok || !fok || !tok || editProbeSourceID(from, step.File, old) || editProbeSourceID(to, step.File, old) {
			return false
		}
	}
	return survivor
}

// A create/delete cell never writes the fixture. Created files still on disk
// are removed by one timed public batch; none left means no batch is sent.
func (r *editProbeRuntime) restoreCreatedFiles(ctx context.Context) error {
	var created []string
	var changes []any
	for _, step := range r.plan.Steps {
		if step.Sender != "create_file" {
			continue
		}
		created = append(created, step.File)
		if _, err := os.Lstat(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(step.File))); err == nil {
			changes = append(changes, map[string]any{"op": "delete_file", "path": r.p.Repos[0].Prefix + "/" + step.File})
		}
	}
	if len(changes) == 0 {
		r.phase.EditProbe.RestoreMS = unavailable("ms", "every created file was deleted by its own pair")
	} else if err := r.restoreBatch(ctx, changes); err != nil {
		return err
	}
	for _, file := range created {
		if _, err := os.Lstat(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(file))); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("created file %s remains after restore", file)
		}
	}
	current, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.plan.File)))
	if err != nil {
		return err
	}
	if string(current) != string(r.original) {
		return errors.New("create/delete cell changed the fixture bytes")
	}
	return nil
}

func (r *editProbeRuntime) restoreBatch(ctx context.Context, changes []any) error {
	req := request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": fmt.Sprintf("load-restore-%016x", uint64(r.options.Seed))}, "output": map[string]any{"format": "json", "max_bytes": 0}}}
	start := time.Now()
	data, _, err := callEditProbeTool(ctx, r.editors[0], req, req.Arguments, "cleanup", 0, len(r.states), 0, r.options.Deadline, editProbeRetrySleep(r.options.Seed, len(r.states), 0))
	r.phase.EditProbe.RestoreMS = measured(float64(time.Since(start))/float64(time.Millisecond), "ms")
	r.phase.EditProbe.Restore = data
	return err
}
