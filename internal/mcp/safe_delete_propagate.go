package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/graph"
)

// SRN-7: transitive propagate-delete. computeCascadeClosure already cascades
// orphaned symbols; the missing half is the surviving callers that still
// reference the deleted symbol. buildPropagationPlan reads each call site and
// classifies it — a standalone statement call can be removed outright; an
// embedded reference is flagged for manual patching — and applyRemoveLinePatches
// deletes the removable lines (parse-gate validated) so the delete leaves no
// dangling reference behind.

type callerPatch struct {
	File        string `json:"file"`
	CallerName  string `json:"caller_name"`
	Line        int    `json:"line"`
	CurrentText string `json:"current_text"`
	Action      string `json:"action"` // remove_line | manual
	Reason      string `json:"reason"`
	abs         string // resolved absolute path (internal)
}

// buildPropagationPlan walks the referencing call sites of a symbol and returns
// the per-caller patch each one needs.
func (s *Server) buildPropagationPlan(ctx context.Context, g graph.Reader, target *graph.Node) []callerPatch {
	var plan []callerPatch
	seen := make(map[string]bool)
	for _, e := range g.GetInEdges(target.ID) {
		if !isReferencingEdgeKind(e.Kind) || e.Line == 0 {
			continue
		}
		caller := g.GetNode(e.From)
		if caller == nil {
			continue
		}
		key := fmt.Sprintf("%s:%d", caller.FilePath, e.Line)
		if seen[key] {
			continue
		}
		seen[key] = true

		abs, err := s.resolveNodePath(ctx, caller)
		if err != nil {
			continue
		}
		text := readSingleLineAt(abs, e.Line)
		action, reason := classifyCallSite(text, target.Name)
		plan = append(plan, callerPatch{
			File:        caller.FilePath,
			CallerName:  caller.Name,
			Line:        e.Line,
			CurrentText: strings.TrimSpace(text),
			Action:      action,
			Reason:      reason,
			abs:         abs,
		})
	}
	sort.Slice(plan, func(a, b int) bool {
		if plan[a].File != plan[b].File {
			return plan[a].File < plan[b].File
		}
		return plan[a].Line < plan[b].Line
	})
	return plan
}

// classifyCallSite decides whether a reference line is a standalone statement
// call (safe to delete whole) or an embedded reference (manual). Conservative:
// anything ambiguous is manual.
func classifyCallSite(line, target string) (action, reason string) {
	t := strings.TrimSpace(line)
	if t == "" || !strings.Contains(t, target) {
		return "manual", "reference not found on the recorded line — patch by hand"
	}
	body := strings.TrimRight(t, ";,")
	// An assignment or comparison means the call's value is consumed.
	if strings.Contains(body, "=") && !strings.Contains(body, "==") {
		return "manual", "reference feeds an assignment — removing it would drop a value"
	}
	for _, frag := range []string{"return ", "return\t", "if ", "for ", "while ", "switch ", "&&", "||", " ? ", "+ ", " +", "<-"} {
		if strings.Contains(body, frag) {
			return "manual", "reference is embedded in a larger expression"
		}
	}
	core := strings.TrimPrefix(strings.TrimPrefix(body, "defer "), "go ")
	if strings.HasSuffix(core, ")") &&
		(strings.HasPrefix(core, target+"(") || strings.Contains(core, "."+target+"(")) {
		return "remove_line", "standalone statement call — the line can be removed"
	}
	return "manual", "could not confirm a standalone call — patch by hand"
}

// applyRemoveLinePatches deletes the remove_line patches from their files,
// grouped per file and applied bottom-up so earlier line numbers stay valid.
// A patch whose removal would introduce new parse errors is not applied and is
// returned in failed. Manual patches are ignored here.
func (s *Server) applyRemoveLinePatches(plan []callerPatch) (applied, failed []callerPatch, err error) {
	byFile := make(map[string][]callerPatch)
	for _, p := range plan {
		if p.Action == "remove_line" {
			byFile[p.abs] = append(byFile[p.abs], p)
		}
	}
	for abs, patches := range byFile {
		content, readErr := os.ReadFile(abs)
		if readErr != nil {
			return applied, failed, fmt.Errorf("read %s: %w", abs, readErr)
		}
		lines := strings.Split(string(content), "\n")
		// Apply bottom-up so deletions don't shift later indices.
		sort.Slice(patches, func(a, b int) bool { return patches[a].Line > patches[b].Line })
		kept := patches[:0]
		newLines := append([]string{}, lines...)
		for _, p := range patches {
			if p.Line < 1 || p.Line > len(newLines) {
				failed = append(failed, p)
				continue
			}
			newLines = append(newLines[:p.Line-1], newLines[p.Line:]...)
			kept = append(kept, p)
		}
		newContent := []byte(strings.Join(newLines, "\n"))
		// Parse gate: never let a propagated removal corrupt a file.
		relPath := patches[0].File
		if gate := checkParseGate(relPath, content, newContent); gate.Blocked {
			failed = append(failed, patches...)
			continue
		}
		perm := os.FileMode(0o644)
		if info, statErr := os.Stat(abs); statErr == nil {
			perm = info.Mode().Perm()
		}
		if writeErr := agents.AtomicWriteFile(abs, newContent, perm); writeErr != nil {
			return applied, failed, fmt.Errorf("write %s: %w", abs, writeErr)
		}
		s.reindexFile(abs)
		applied = append(applied, kept...)
	}
	return applied, failed, nil
}

func countManual(plan []callerPatch) int {
	n := 0
	for _, p := range plan {
		if p.Action == "manual" {
			n++
		}
	}
	return n
}
