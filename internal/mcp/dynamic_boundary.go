package mcp

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// DynamicBoundary is graph.DynamicBoundary — the {site, form, key, candidate}
// descriptor of a runtime-dispatch site. Aliased here so the detector reads
// naturally; the type lives in graph so query.SubGraph can carry it.
type DynamicBoundary = graph.DynamicBoundary

const (
	dispatchFormReflection     = "reflection"
	dispatchFormComputedMember = "computed_member"
	dispatchFormEventBus       = "event_bus"
)

var (
	// reflection / getattr / MethodByName / getMethod — name-driven invocation.
	dynReflectionRe = regexp.MustCompile(`(?:getattr\s*\(\s*\w+\s*,\s*|\.MethodByName\s*\(\s*|\.[gG]etMethod\s*\(\s*)['"]?([\w.$]+)`)
	// computed-member call: handlers[key](...) / obj[expr](...).
	dynComputedMemberRe = regexp.MustCompile(`\w+\s*\[\s*['"]?([\w.$]+)['"]?\s*\]\s*\(`)
	// typed/event bus: .emit('x') / .dispatch("x") / .publish(Topic).
	dynEventBusRe = regexp.MustCompile(`\.\s*(?:emit|dispatch|publish|sendEvent)\s*\(\s*['"]?([\w.$\-/]+)`)
)

// detectDynamicBoundaries scans a symbol body for runtime-dispatch forms and
// returns one boundary per site. The body is line-comment-stripped first so a
// dispatch-shaped token in a comment doesn't register. resolveCandidates
// supplies the candidate shortlist for a (form, key) — graph-backed in
// production, a stub in tests; nil yields empty shortlists.
func detectDynamicBoundaries(body, filePath string, startLine int, resolveCandidates func(form, key string) []string) []DynamicBoundary {
	lines := strings.Split(body, "\n")
	var out []DynamicBoundary
	seen := map[string]bool{}

	emit := func(form, key string, lineIdx int) {
		key = strings.TrimSpace(strings.Trim(key, `'"`))
		if key == "" {
			return
		}
		site := fmt.Sprintf("%s:%d", filePath, startLine+lineIdx)
		dedupe := form + "\x00" + key + "\x00" + site
		if seen[dedupe] {
			return
		}
		seen[dedupe] = true
		var cands []string
		if resolveCandidates != nil {
			cands = resolveCandidates(form, key)
		}
		out = append(out, DynamicBoundary{
			Site: site, Form: form, Key: key,
			Candidates: cands, AgentNamed: anyAgentNamed(cands),
		})
	}

	for i, raw := range lines {
		line := stripLineComment(raw)
		for _, m := range dynReflectionRe.FindAllStringSubmatch(line, -1) {
			emit(dispatchFormReflection, m[1], i)
		}
		for _, m := range dynComputedMemberRe.FindAllStringSubmatch(line, -1) {
			emit(dispatchFormComputedMember, m[1], i)
		}
		for _, m := range dynEventBusRe.FindAllStringSubmatch(line, -1) {
			emit(dispatchFormEventBus, m[1], i)
		}
	}
	return out
}

// stripLineComment blanks a `//` or `#` line comment (best-effort: a `#` or
// `//` inside a string is rare in dispatch lines and harmless to blank).
func stripLineComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		line = line[:i]
	}
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	return line
}

// agentNameRe flags candidate symbols whose name denotes an agent / handler /
// worker — the dispatch targets an agent-style component worth surfacing.
var agentNameRe = regexp.MustCompile(`(?i)(agent|handler|worker|processor|consumer|listener|subscriber)s?$`)

func anyAgentNamed(candidates []string) bool {
	for _, c := range candidates {
		name := c
		if i := strings.LastIndexAny(name, ":./"); i >= 0 {
			name = name[i+1:]
		}
		if agentNameRe.MatchString(name) {
			return true
		}
	}
	return false
}

// dynamicBoundariesForSymbol reads a symbol's source and detects the runtime
// dispatch boundaries inside it, resolving candidate targets through the graph.
// Returns nil when the source can't be read or no dispatch is found.
// The session's overlay buffer substitutes for the on-disk file when the
// request carries one: an overlay-served node's line range is a BUFFER
// coordinate, and slicing the stale disk content by it reads a different
// body and fabricates boundaries.
func (s *Server) dynamicBoundariesForSymbol(ctx context.Context, node *graph.Node) []DynamicBoundary {
	if node == nil || node.StartLine <= 0 {
		return nil
	}
	absPath, err := s.resolveNodePath(node)
	if err != nil {
		return nil
	}
	text, ok := s.overlayContentFor(ctx, absPath)
	if !ok {
		raw, err := os.ReadFile(absPath) //nolint:gosec // path resolved from the indexed graph
		if err != nil {
			return nil
		}
		text = string(raw)
	}
	lines := strings.Split(text, "\n")
	start := node.StartLine - 1
	end := node.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start < 0 || start >= end {
		return nil
	}
	body := strings.Join(lines[start:end], "\n")
	return detectDynamicBoundaries(body, node.FilePath, node.StartLine, s.dispatchCandidates)
}

// dispatchCandidates is the graph-backed candidate resolver: when the dispatch
// key looks like a symbol name, the shortlist is the functions/methods that
// bear that name (capped). A non-identifier key (a runtime variable) yields no
// shortlist — honest about what's statically knowable.
func (s *Server) dispatchCandidates(_, key string) []string {
	if !isIdentifierKey(key) {
		return nil
	}
	var out []string
	for _, n := range s.graph.FindNodesByName(key) {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			out = append(out, n.ID)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

func isIdentifierKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (i > 0 && b >= '0' && b <= '9') {
			continue
		}
		return false
	}
	return true
}
