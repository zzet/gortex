package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func (s *Server) registerPrompts() {
	s.addPrompt(
		mcp.NewPrompt("pre_commit",
			mcp.WithPromptDescription("Review uncommitted changes before committing. Shows changed symbols, blast radius, risk level, affected tests, and run commands."),
			mcp.WithArgument("scope",
				mcp.ArgumentDescription("Git diff scope: unstaged (default), staged, all, or compare"),
			),
			mcp.WithArgument("repo",
				mcp.ArgumentDescription("Repository prefix or path (multi-repo mode); defaults to the lone tracked repo or the session's cwd-bound repo"),
			),
		),
		s.handlePromptPreCommit,
	)

	s.addPrompt(
		mcp.NewPrompt("orientation",
			mcp.WithPromptDescription("Orient in an unfamiliar codebase. Shows graph stats, functional communities, execution flows, and key symbols."),
		),
		s.handlePromptOrientation,
	)

	s.addPrompt(
		mcp.NewPrompt("safe_to_change",
			mcp.WithPromptDescription("Analyze whether it's safe to change specific symbols. Shows blast radius, edit plan, affected tests, and risk level."),
			mcp.WithArgument("ids",
				mcp.ArgumentDescription("Comma-separated symbol IDs to analyze"),
				mcp.RequiredArgument(),
			),
		),
		s.handlePromptSafeToChange,
	)
}

// promptArg reads a prompts/get argument defensively. mcp-go leaves
// Params.Arguments nil when the client sends a request with no
// `arguments` object; reading a nil map is safe in Go, but routing
// every access through this helper makes the empty-args contract
// explicit — and trims surrounding whitespace — rather than relying
// on library behaviour that a future mcp-go bump could change.
func promptArg(req mcp.GetPromptRequest, key string) string {
	if req.Params.Arguments == nil {
		return ""
	}
	return strings.TrimSpace(req.Params.Arguments[key])
}

// validPreCommitScopes are the git-diff scopes handlePromptPreCommit
// accepts. An empty or unrecognised scope falls back to "all" rather
// than reaching analysis.MapGitDiff with junk.
var validPreCommitScopes = map[string]bool{
	"unstaged": true,
	"staged":   true,
	"all":      true,
	"compare":  true,
}

func (s *Server) handlePromptPreCommit(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	scope := promptArg(req, "scope")
	if !validPreCommitScopes[scope] {
		scope = "all"
	}

	// Resolve the working tree: explicit repo argument, lone tracked repo,
	// the session's cwd-bound repo, then its sole contained repo. "." is
	// reserved for the standalone (indexer-less) server, which is started
	// in the tree it serves.
	repoRoot, repoPrefix, rootErr := s.resolveDiffRoot(ctx, promptArg(req, "repo"))
	if rootErr != nil {
		return promptError(rootErr.Error()), nil
	}

	diff, err := analysis.MapGitDiff(s.readerFor(ctx), repoRoot, repoPrefix, scope, "main")
	if err != nil {
		return promptError("Could not analyze git changes: " + err.Error()), nil
	}

	var b strings.Builder
	b.WriteString("## Pre-Commit Review\n\n")

	if len(diff.ChangedSymbols) == 0 {
		fmt.Fprintf(&b, "No indexed symbols affected by current changes.\n\n")
		fmt.Fprintf(&b, "Changed files: %d\n", len(diff.ChangedFiles))
		return promptResult("Pre-commit review: no symbols affected", b.String()), nil
	}

	symbolIDs := make([]string, len(diff.ChangedSymbols))
	for i, cs := range diff.ChangedSymbols {
		symbolIDs[i] = cs.ID
	}
	impact := s.analyzeImpactLazy(ctx, symbolIDs)

	fmt.Fprintf(&b, "**Risk: %s**\n\n", impact.Risk)
	fmt.Fprintf(&b, "Changed: %d symbols in %d files\n\n", len(diff.ChangedSymbols), len(diff.ChangedFiles))

	b.WriteString("### Changed Symbols\n")
	for _, cs := range diff.ChangedSymbols {
		fmt.Fprintf(&b, "- `%s` (%s)\n", cs.ID, cs.Kind)
	}

	if len(impact.ByDepth) > 0 {
		b.WriteString("\n### Blast Radius\n")
		for depth, entries := range impact.ByDepth {
			var label string
			switch depth {
			case 1:
				label = "WILL BREAK"
			case 2:
				label = "LIKELY AFFECTED"
			default:
				label = "MAY NEED TESTING"
			}
			fmt.Fprintf(&b, "\n**d=%d (%s):**\n", depth, label)
			for _, entry := range entries {
				fmt.Fprintf(&b, "- `%s`\n", entry.ID)
			}
		}
	}

	testFiles := s.findTestFiles(ctx, symbolIDs)
	if len(testFiles) > 0 {
		b.WriteString("\n### Tests to Run\n")
		for file, names := range testFiles {
			fmt.Fprintf(&b, "- `%s`", file)
			if len(names) > 0 && strings.HasSuffix(file, "_test.go") {
				dir := file[:strings.LastIndex(file, "/")]
				fmt.Fprintf(&b, "\n  ```\n  go test -run %s ./%s/\n  ```", strings.Join(names, "|"), dir)
			}
			b.WriteString("\n")
		}
	}

	if len(impact.AffectedProcesses) > 0 {
		b.WriteString("\n### Affected Processes\n")
		for _, p := range impact.AffectedProcesses {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}

	return promptResult("Pre-commit review: "+string(impact.Risk), b.String()), nil
}

func (s *Server) handlePromptOrientation(ctx context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	// Confine the orientation to the session's workspace. scopedNodes
	// returns every node for an unbound session, so the prompt is
	// byte-identical to the legacy global view there. inScope is the
	// node-ID set used to bound the analyzer-driven sections; nil for
	// an unbound session.
	scoped := s.scopedNodesLight(ctx)
	_, _, bound := s.sessionScope(ctx)
	var inScope map[string]bool
	if bound {
		inScope = make(map[string]bool, len(scoped))
		for _, n := range scoped {
			inScope[n.ID] = true
		}
	}

	byLang := make(map[string]int)
	byKind := make(map[graph.NodeKind]int)
	for _, n := range scoped {
		if n.Language != "" {
			byLang[n.Language]++
		}
		byKind[n.Kind]++
	}
	totalEdges := 0
	for _, e := range s.readerFor(ctx).AllEdges() {
		if inScope != nil && (!inScope[e.From] || !inScope[e.To]) {
			continue
		}
		totalEdges++
	}

	var b strings.Builder
	b.WriteString("## Codebase Orientation\n\n")
	fmt.Fprintf(&b, "**%d nodes, %d edges**\n\n", len(scoped), totalEdges)

	if len(byLang) > 0 {
		b.WriteString("### Languages\n")
		for lang, count := range byLang {
			fmt.Fprintf(&b, "- %s: %d nodes\n", lang, count)
		}
		b.WriteString("\n")
	}

	if len(byKind) > 0 {
		b.WriteString("### Symbol Breakdown\n")
		for kind, count := range byKind {
			fmt.Fprintf(&b, "- %s: %d\n", kind, count)
		}
		b.WriteString("\n")
	}

	scopedIDs := make([]string, 0, len(scoped))
	for _, node := range scoped {
		if node != nil {
			scopedIDs = append(scopedIDs, node.ID)
		}
	}
	communities := s.communitySummariesForNodes(scopedIDs, bound, 10)
	if len(communities) > 0 {
		b.WriteString("### Functional Areas (Communities)\n")
		for _, community := range communities {
			fmt.Fprintf(&b, "- **%s** — %d symbols, %d files (cohesion: %.2f)\n",
				community.Label, community.Size, len(community.Files), community.Cohesion)
		}
		b.WriteString("\n")
	}

	processes := s.processSummariesForEntries(inScope, bound, 10)
	if len(processes) > 0 {
		b.WriteString("### Execution Flows (Processes)\n")
		for _, process := range processes {
			fmt.Fprintf(&b, "- **%s** — %d steps, entry: `%s`\n",
				process.Name, process.StepCount, process.EntryPoint)
		}
		b.WriteString("\n")
	}

	topRefs := s.findTopReferenced(ctx, 10)
	if len(topRefs) > 0 {
		b.WriteString("### Most-Referenced Symbols\n")
		for _, r := range topRefs {
			fmt.Fprintf(&b, "- `%s` — %d references\n", r.id, r.count)
		}
		b.WriteString("\n")
	}

	b.WriteString("Use `/gortex-explore` for guided architecture exploration.\n")

	return promptResult("Codebase orientation", b.String()), nil
}

func (s *Server) handlePromptSafeToChange(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	idsStr := promptArg(req, "ids")
	if idsStr == "" {
		return promptError("ids argument is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	sessWS, _, _ := s.sessionScope(ctx)

	// A symbol outside the session's workspace is treated as not found
	// — the prompt must not reveal cross-workspace symbols.
	var validIDs, notFound []string
	for _, id := range ids {
		if n := s.engineFor(ctx).GetSymbol(id); n != nil && s.nodeInSessionScope(ctx, n) {
			validIDs = append(validIDs, id)
		} else {
			notFound = append(notFound, id)
		}
	}

	var b strings.Builder
	b.WriteString("## Safety Analysis\n\n")

	if len(notFound) > 0 {
		fmt.Fprintf(&b, "**Warning:** symbols not found: %s\n\n", strings.Join(notFound, ", "))
	}
	if len(validIDs) == 0 {
		return promptError("No valid symbols found"), nil
	}

	impact := s.analyzeImpactLazy(ctx, validIDs)

	fmt.Fprintf(&b, "**Risk: %s**\n\n", impact.Risk)
	fmt.Fprintf(&b, "Symbols to change: %s\n\n", strings.Join(validIDs, ", "))

	if len(impact.ByDepth) > 0 {
		b.WriteString("### Blast Radius\n")
		for depth, entries := range impact.ByDepth {
			var label string
			switch depth {
			case 1:
				label = "WILL BREAK"
			case 2:
				label = "LIKELY AFFECTED"
			default:
				label = "MAY NEED TESTING"
			}
			fmt.Fprintf(&b, "\n**d=%d (%s):**\n", depth, label)
			for _, entry := range entries {
				fmt.Fprintf(&b, "- `%s`\n", entry.ID)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("### Edit Order\n")
	for _, id := range validIDs {
		node := s.engineFor(ctx).GetSymbol(id)
		if node == nil {
			continue
		}
		fmt.Fprintf(&b, "1. `%s` — definition\n", node.FilePath)

		if node.Kind == graph.KindInterface {
			impls := s.scopedNodeSlice(ctx, s.engineFor(ctx).FindImplementations(id))
			for _, impl := range impls {
				fmt.Fprintf(&b, "2. `%s` — implements %s\n", impl.FilePath, node.Name)
			}
		}

		dependents := s.engineFor(ctx).GetDependents(id, query.QueryOptions{Depth: 2, Limit: 20, Detail: "brief", WorkspaceID: sessWS})
		depFiles := make(map[string]bool)
		for _, dn := range dependents.Nodes {
			if dn.Kind != graph.KindFile && dn.FilePath != node.FilePath && !isTestFile(dn.FilePath) && !depFiles[dn.FilePath] {
				depFiles[dn.FilePath] = true
				fmt.Fprintf(&b, "3. `%s` — dependent\n", dn.FilePath)
			}
		}
	}

	testFiles := s.findTestFiles(ctx, validIDs)
	if len(testFiles) > 0 {
		b.WriteString("\n### Tests to Verify\n")
		for file := range testFiles {
			fmt.Fprintf(&b, "- `%s`\n", file)
		}
	} else {
		b.WriteString("\n**Warning:** No test coverage found for these symbols.\n")
	}

	if len(impact.AffectedProcesses) > 0 {
		b.WriteString("\n### Affected Processes\n")
		for _, p := range impact.AffectedProcesses {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}

	return promptResult("Safety analysis: "+string(impact.Risk), b.String()), nil
}

// findTestFiles traces callers of the given symbols and returns test files with function names.
// ctx scopes graph reads to the caller's overlay view (when present)
// so an editor's unsaved test edits surface in the impact summary.
func (s *Server) findTestFiles(ctx context.Context, symbolIDs []string) map[string][]string {
	eng := s.engineFor(ctx)
	testFiles := make(map[string]map[string]bool)
	for _, id := range symbolIDs {
		callers := eng.GetCallers(id, query.QueryOptions{Depth: 3, Limit: 50, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if isTestFile(cn.FilePath) {
				if testFiles[cn.FilePath] == nil {
					testFiles[cn.FilePath] = make(map[string]bool)
				}
				if cn.Kind == graph.KindFunction || cn.Kind == graph.KindMethod {
					testFiles[cn.FilePath][cn.Name] = true
				}
			}
		}
	}
	result := make(map[string][]string)
	for file, funcs := range testFiles {
		var names []string
		for n := range funcs {
			names = append(names, n)
		}
		result[file] = names
	}
	return result
}

type refEntry struct {
	id    string
	count int
}

// findTopReferenced returns the most-referenced symbols, confined to
// the current session's workspace.
func (s *Server) findTopReferenced(ctx context.Context, limit int) []refEntry {
	allNodes := s.scopedNodesLight(ctx)
	var refs []refEntry
	for _, n := range allNodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport || n.Kind == graph.KindVariable {
			continue
		}
		count := 0
		for _, e := range s.engineFor(ctx).GetInEdges(n.ID) {
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
				count++
			}
		}
		if count >= 3 {
			refs = append(refs, refEntry{n.ID, count})
		}
	}
	for i := 0; i < len(refs); i++ {
		for j := i + 1; j < len(refs); j++ {
			if refs[j].count > refs[i].count {
				refs[i], refs[j] = refs[j], refs[i]
			}
		}
	}
	if len(refs) > limit {
		refs = refs[:limit]
	}
	return refs
}

func promptResult(description, text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: description,
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: text,
				},
			},
		},
	}
}

func promptError(msg string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: "Error",
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: "Error: " + msg,
				},
			},
		},
	}
}
