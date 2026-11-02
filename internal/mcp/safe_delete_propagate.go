package mcp

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
)

// SRN-7: transitive propagate-delete. computeCascadeClosure already cascades
// orphaned symbols; the missing half is the surviving callers that still
// reference the deleted symbol. buildPropagationPlan reads each call site and
// classifies it — a standalone statement call can be removed outright; an
// embedded reference is flagged for manual patching — and applyRemoveLinePatches
// deletes the removable lines (text-anchored, parse-gate validated) so the
// delete leaves no dangling reference behind.

type callerPatch struct {
	File                    string `json:"file"`
	CallerName              string `json:"caller_name"`
	Line                    int    `json:"line"`
	CurrentText             string `json:"current_text"`
	Action                  string `json:"action"` // remove_line | manual
	Reason                  string `json:"reason"`
	NeedsSourceRevalidation bool   `json:"source_revalidation_required,omitempty"`
	abs                     string // resolved absolute path (internal)
	from, target            string
	kind                    graph.EdgeKind
	sourceHash              [32]byte
}

// buildPropagationPlan walks the referencing call sites of a symbol and returns
// the per-caller patch each one needs.
func (s *Server) buildPropagationPlan(ctx context.Context, g graph.Reader, target *graph.Node) []callerPatch {
	return s.buildPropagationPlanForPhase(ctx, g, target, false)
}

func (s *Server) buildPropagationPreview(ctx context.Context, g graph.Reader, target *graph.Node) []callerPatch {
	return s.buildPropagationPlanForPhase(ctx, g, target, true)
}

func (s *Server) buildPropagationPlanForPhase(ctx context.Context, g graph.Reader, target *graph.Node, preview bool) []callerPatch {
	var plan []callerPatch
	seen := make(map[string]int)
	for _, edge := range g.GetInEdges(target.ID) {
		if !isReferencingEdgeKind(edge.Kind) {
			continue
		}
		patch := callerPatch{File: edge.FilePath, Line: edge.Line, Action: "manual", Reason: "reference has no verified isolated source occurrence", from: edge.From, target: edge.To, kind: edge.Kind}
		caller := g.GetNode(edge.From)
		if caller != nil {
			patch.CallerName = caller.Name
			if patch.File == "" {
				patch.File = caller.FilePath
			}
		}
		if caller == nil || edge.Line <= 0 || edge.Kind != graph.EdgeCalls || patch.File != caller.FilePath {
			plan = append(plan, patch)
			continue
		}
		abs, err := s.resolveNodePath(ctx, caller)
		if err != nil {
			plan = append(plan, patch)
			continue
		}
		patch.abs = abs
		raw, accepted := s.acceptedPropagationSource(ctx, g, caller, abs)
		lines := strings.Split(string(raw), "\n")
		if len(raw) == 0 || !accepted && !preview || edge.Line > len(lines) {
			patch.Reason = "caller source is not the selected graph's accepted content — publish it or patch by hand"
			plan = append(plan, patch)
			continue
		}
		patch.CurrentText = strings.TrimSpace(lines[edge.Line-1])
		patch.Action, patch.Reason = classifyCallSite(patch.CurrentText, target.Name)
		patch.sourceHash = blake3.Sum256(raw)
		if !accepted && patch.Action == "remove_line" {
			patch.NeedsSourceRevalidation = true
			patch.Reason = "isolated call in current source; this preview requires source and reference revalidation before applying"
		}
		key := fmt.Sprintf("%s:%d", patch.File, patch.Line)
		if previous, ok := seen[key]; ok {
			// More than one graph occurrence cannot be discharged by one
			// textual line deletion. Keep both references manually actionable.
			plan[previous].Action, plan[previous].Reason = "manual", "multiple references share this source line — patch by hand"
			patch.Action, patch.Reason = "manual", "multiple references share this source line — patch by hand"
		} else {
			seen[key] = len(plan)
		}
		plan = append(plan, patch)
	}
	sort.Slice(plan, func(a, b int) bool {
		if plan[a].File != plan[b].File {
			return plan[a].File < plan[b].File
		}
		if plan[a].Line != plan[b].Line {
			return plan[a].Line < plan[b].Line
		}
		if plan[a].from != plan[b].from {
			return plan[a].from < plan[b].from
		}
		if plan[a].kind != plan[b].kind {
			return plan[a].kind < plan[b].kind
		}
		return plan[a].target < plan[b].target
	})
	return plan
}

// Automatic source removal needs the same selected graph's accepted bytes,
// not merely a matching callee name on a line in a changed file. Unknown or
// transformed identities conservatively stay manual; there is no canonical
// fallback for a routed reader.
func (s *Server) acceptedPropagationSource(ctx context.Context, reader graph.Reader, caller *graph.Node, abs string) ([]byte, bool) {
	files, err := source.NewFilesystemSource(filepath.Dir(abs))
	if err != nil {
		return nil, false
	}
	defer files.Close()
	raw, _, err := files.ReadRegularFile(ctx, filepath.Base(abs), 1<<20)
	if err != nil {
		return nil, false
	}
	checked, ok := reader.(graph.ConstantValueProjectionReader)
	if !ok {
		return raw, false
	}
	prefix := caller.RepoPrefix
	if owner, _ := s.indexerForRel(caller.FilePath); owner != nil {
		prefix = owner.RepoPrefix()
	}
	key := graph.ConstantFileKey{RepoPrefix: prefix, FilePath: caller.FilePath}
	projection, err := checked.ReadConstantValueProjectionContext(ctx, nil, []graph.ConstantFileKey{key})
	if err != nil {
		return raw, false
	}
	row, ok := projection.Files[key]
	sum := blake3.Sum256(raw)
	return raw, ok && row.ContentHash == fmt.Sprintf("%x", sum[:])
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
	// Parse one expression, not a line prefix: Target(); Keep() is two
	// statements, and deleting its whole line would erase unrelated work.
	if expression, err := parser.ParseExpr(core); err == nil {
		if call, ok := expression.(*ast.CallExpr); ok {
			name := ""
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				name = callee.Name
			case *ast.SelectorExpr:
				name = callee.Sel.Name
			}
			if name == target {
				return "remove_line", "isolated standalone call — the line can be removed"
			}
		}
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
	applied, failed, writes, err := s.applyRemoveLinePatchesHeld(ctx, plan)
	s.completeCommittedSourceMutations(ctx, writes, release)
	return applied, failed, err
}

func (s *Server) applyRemoveLinePatchesHeld(ctx context.Context, plan []callerPatch) (applied, failed []callerPatch, writes []committedSourceMutation, err error) {
	byFile := make(map[string][]callerPatch)
	for _, patch := range plan {
		if patch.Action == "remove_line" {
			byFile[patch.abs] = append(byFile[patch.abs], patch)
		}
	}
	for _, abs := range slices.Sorted(maps.Keys(byFile)) {
		ok, refused, commit, ferr := s.removePlannedLines(ctx, abs, byFile[abs])
		applied = append(applied, ok...)
		failed = append(failed, refused...)
		if commit != nil && ferr == nil {
			writes = append(writes, captureCommittedSourceMutation(ctx, abs, commit))
		}
		if ferr != nil {
			return applied, failed, writes, ferr
		}
	}
	return applied, failed, writes, nil
}

// removePlannedLines applies one file's patches bottom-up, so earlier line
// numbers stay valid; the caller holds the file's mutation lock, so no
// concurrent writer lands between the read and the write. A patch is anchored
// on its planned text: a line that was edited or shifted since planning is
// refused as line_changed instead of being deleted by number. When the
// surviving removals would introduce new parse errors the file is left
// untouched and they are refused as parse_errors. Nothing is written when
// every patch is refused.
func (s *Server) removePlannedLines(ctx context.Context, abs string, patches []callerPatch) (applied, failed []callerPatch, committed *mutationCommitRecord, err error) {
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", abs, err)
	}
	lines := strings.Split(string(content), "\n")
	sort.Slice(patches, func(a, b int) bool { return patches[a].Line > patches[b].Line })
	for _, p := range patches {
		switch {
		case p.NeedsSourceRevalidation:
			p.Reason = "source_unverified"
			failed = append(failed, p)
		case p.Line < 1 || p.Line > len(lines):
			p.Reason = "line_out_of_range"
			failed = append(failed, p)
		case strings.TrimSpace(lines[p.Line-1]) != p.CurrentText:
			p.Reason = "line_changed"
			failed = append(failed, p)
		case p.sourceHash != ([32]byte{}) && p.sourceHash != blake3.Sum256(content):
			p.Reason = "file_changed"
			failed = append(failed, p)
		default:
			lines = append(lines[:p.Line-1], lines[p.Line:]...)
			applied = append(applied, p)
		}
	}
	if len(applied) == 0 {
		return nil, failed, nil, nil
	}
	newContent := []byte(strings.Join(lines, "\n"))
	// Parse gate: never let a propagated removal corrupt a file.
	prepared, gate := s.prepareAcknowledgedProjection(ctx, patches[0].File, abs, content, newContent)
	if gate.Blocked {
		for _, p := range applied {
			p.Reason = "parse_errors"
			failed = append(failed, p)
		}
		return nil, failed, nil, nil
	}
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(abs); statErr == nil {
		perm = info.Mode().Perm()
	}
	commit, writeErr := s.commitFileMutation(ctx, "safe_delete_symbol", "", "", patches[0].File, abs, newContent, perm, sourceMutationOperation{kind: sourceWrite, projection: prepared})
	if writeErr != nil {
		return nil, failed, nil, fmt.Errorf("write %s: %w", abs, writeErr)
	}
	return applied, failed, commit, nil
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
