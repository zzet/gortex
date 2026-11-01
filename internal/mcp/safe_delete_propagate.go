package mcp

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
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
// deletes the removable lines (text-anchored, parse-gate validated) so the
// delete leaves no dangling reference behind.

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

// applyRemoveLinePatches deletes the remove_line patches from their files.
// Manual patches are ignored here. Every caller file is locked before any is
// written, so a lock that cannot be taken (a cancelled request, a path the
// checkout guard refuses) leaves them all untouched. Each file is then handled
// by removePlannedLines; a refused patch lands in failed exactly once, with
// its Reason replaced by the refusal code.
func (s *Server) applyRemoveLinePatches(ctx context.Context, plan []callerPatch) (applied, failed []callerPatch, err error) {
	byFile := make(map[string][]callerPatch)
	for _, p := range plan {
		if p.Action == "remove_line" {
			byFile[p.abs] = append(byFile[p.abs], p)
		}
	}
	paths := slices.Sorted(maps.Keys(byFile))
	ctx, release, err := acquireMutationPaths(ctx, paths)
	if err != nil {
		return nil, nil, fmt.Errorf("lock caller files: %w", err)
	}
	defer release()
	for _, abs := range paths {
		ok, refused, ferr := s.removePlannedLines(abs, byFile[abs])
		applied = append(applied, ok...)
		failed = append(failed, refused...)
		if ferr != nil {
			return applied, failed, ferr
		}
	}
	return applied, failed, nil
}

// removePlannedLines applies one file's patches bottom-up, so earlier line
// numbers stay valid; the caller holds the file's mutation lock, so no
// concurrent writer lands between the read and the write. A patch is anchored
// on its planned text: a line that was edited or shifted since planning is
// refused as line_changed instead of being deleted by number. When the
// surviving removals would introduce new parse errors the file is left
// untouched and they are refused as parse_errors. Nothing is written when
// every patch is refused.
func (s *Server) removePlannedLines(abs string, patches []callerPatch) (applied, failed []callerPatch, err error) {
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", abs, err)
	}
	lines := strings.Split(string(content), "\n")
	sort.Slice(patches, func(a, b int) bool { return patches[a].Line > patches[b].Line })
	for _, p := range patches {
		switch {
		case p.Line < 1 || p.Line > len(lines):
			p.Reason = "line_out_of_range"
			failed = append(failed, p)
		case strings.TrimSpace(lines[p.Line-1]) != p.CurrentText:
			p.Reason = "line_changed"
			failed = append(failed, p)
		default:
			lines = append(lines[:p.Line-1], lines[p.Line:]...)
			applied = append(applied, p)
		}
	}
	if len(applied) == 0 {
		return nil, failed, nil
	}
	newContent := []byte(strings.Join(lines, "\n"))
	// Parse gate: never let a propagated removal corrupt a file.
	if gate := checkParseGate(patches[0].File, content, newContent); gate.Blocked {
		for _, p := range applied {
			p.Reason = "parse_errors"
			failed = append(failed, p)
		}
		return nil, failed, nil
	}
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(abs); statErr == nil {
		perm = info.Mode().Perm()
	}
	if writeErr := agents.AtomicWriteFile(abs, newContent, perm); writeErr != nil {
		return nil, failed, fmt.Errorf("write %s: %w", abs, writeErr)
	}
	s.reindexFile(abs)
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
