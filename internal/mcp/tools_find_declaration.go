package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

// registerFindDeclarationTool wires find_declaration — the use-site →
// declaration join. Given a string (or regex) that appears at a use
// site, it locates those sites and resolves each one to the symbol it
// uses, then groups the result by declaration.
func (s *Server) registerFindDeclarationTool() {
	s.addTool(
		mcp.NewTool("find_declaration",
			mcp.WithDescription("Resolves a use site to the symbol it uses — the inverse of find_usages. Give it text that appears where a symbol is called or referenced (e.g. \"fooBar(\"); it locates those sites and, for each, walks the enclosing function's edges to find the declaration whose use lands on that line. Results are grouped: one declaration with the list of use sites that reach it."),
			mcp.WithString("use_site", mcp.Required(), mcp.Description("Text matching a use site — a literal substring, or a regular expression when regex=true.")),
			mcp.WithBoolean("regex", mcp.Description("Treat use_site as an RE2 regular expression rather than a literal (default false).")),
			mcp.WithString("path_prefix", mcp.Description("Restrict the search to files under this forward-slash repo-relative path prefix.")),
			mcp.WithString("kind", mcp.Description("Comma-separated node-kind filter applied to the resolved declarations (e.g. function,method,type).")),
			mcp.WithNumber("limit", mcp.Description("Max use sites to scan (default 20, hard cap 1000).")),
			mcp.WithString("format", mcp.Description("Output format: json (default) or toon.")),
		),
		s.handleFindDeclaration,
	)
}

// declUseSite is one location where a declaration is used.
type declUseSite struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// declGroup is one resolved declaration with the use sites that reach it.
type declGroup struct {
	Declaration *graph.Node   `json:"declaration"`
	UseSites    []declUseSite `json:"use_sites"`
}

// declResolveKinds is the set of edge kinds whose target counts as the
// declaration a use site resolves to.
var declResolveKinds = map[graph.EdgeKind]bool{
	graph.EdgeCalls:      true,
	graph.EdgeReferences: true,
	graph.EdgeReads:      true,
}

// handleFindDeclaration runs the two-stage use-site → declaration join.
func (s *Server) handleFindDeclaration(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	useSite, err := req.RequireString("use_site")
	if err != nil || strings.TrimSpace(useSite) == "" {
		return mcp.NewToolResultError("find_declaration: use_site is required"), nil
	}
	if s.indexer == nil {
		return mcp.NewToolResultError("find_declaration: no indexer available"), nil
	}

	isRegex := req.GetBool("regex", false)
	pathPrefix := req.GetString("path_prefix", "")
	limit := req.GetInt("limit", 20)
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	kindFilter := parseDeclKindCSV(req.GetString("kind", ""))

	// Stage 1 — locate the use sites.
	matches, stage1Err := s.findUseSiteMatches(useSite, isRegex, pathPrefix, limit)
	if stage1Err != nil {
		return mcp.NewToolResultError("find_declaration: " + stage1Err.Error()), nil
	}
	if len(matches) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"use_site":     useSite,
			"declarations": []declGroup{},
			"count":        0,
			"note":         "no use sites matched",
		})
	}

	// Stage 2 — resolve each use site to a declaration.
	eng := s.engineFor(ctx)
	// Pass the NodesInFilesByKindFinder capability when the request
	// reader implements it; buildDeclFileIndex falls back to AllNodes()
	// when finder is nil (e.g. behind an overlay view that doesn't
	// expose the capability), and that walk reads the overlay too.
	finder, _ := s.readerFor(ctx).(graph.NodesInFilesByKindFinder)
	fileIdx := buildDeclFileIndex(eng, finder, matches)

	groups := make(map[string]*declGroup)
	var declOrder []string

	for _, m := range matches {
		declID := resolveUseSiteDecl(eng, fileIdx, m)
		if declID == "" {
			continue
		}
		decl := eng.GetSymbol(declID)
		if decl == nil {
			continue
		}
		if len(kindFilter) > 0 && !kindFilter[strings.ToLower(string(decl.Kind))] {
			continue
		}
		g := groups[declID]
		if g == nil {
			g = &declGroup{Declaration: decl}
			groups[declID] = g
			declOrder = append(declOrder, declID)
		}
		g.UseSites = append(g.UseSites, declUseSite{File: m.Path, Line: m.Line, Text: m.Text})
	}

	ordered := make([]*declGroup, 0, len(declOrder))
	for _, id := range declOrder {
		ordered = append(ordered, groups[id])
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"use_site":          useSite,
		"declarations":      ordered,
		"count":             len(ordered),
		"use_sites_scanned": len(matches),
	})
}

// findUseSiteMatches runs Stage 1 — the trigram-accelerated search that
// locates candidate use sites. For a literal it uses GrepText and
// post-filters by path_prefix; for a regex it uses GrepRegexp, which
// applies the prefix natively. Multi-repo mode fans out across every
// tracked per-repo Indexer — the singleton s.indexer has no rootPath
// in that mode, so its trigram index is empty and a direct call
// returns no matches.
func (s *Server) findUseSiteMatches(useSite string, isRegex bool, pathPrefix string, limit int) ([]trigram.Match, error) {
	if isRegex {
		var (
			matches []trigram.Match
			err     error
		)
		if s.multiIndexer != nil {
			matches, err = s.multiIndexer.GrepRegexp(useSite, pathPrefix, limit)
		} else if s.indexer != nil {
			matches, err = s.indexer.GrepRegexp(useSite, pathPrefix, limit)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %v", err)
		}
		return matches, nil
	}
	var raw []trigram.Match
	if s.multiIndexer != nil {
		raw = s.multiIndexer.GrepText(useSite, 0)
	} else if s.indexer != nil {
		raw = s.indexer.GrepText(useSite, 0)
	}
	var matches []trigram.Match
	for _, m := range raw {
		if !graphpath.HasPrefix(m.Path, pathPrefix) {
			continue
		}
		matches = append(matches, m)
		if len(matches) >= limit {
			break
		}
	}
	return matches, nil
}

// buildDeclFileIndex builds one fileSymbolIndex per file touched by the
// matches, so the enclosing symbol of any match line can be found
// quickly. It mirrors buildFileSymbolIndex but is keyed off the match
// set directly rather than astquery targets.
//
// finder may be nil when no NodesInFilesByKindFinder-capable backend
// is available (e.g. when running through an editor-buffer overlay
// whose underlying view doesn't expose the capability); the function
// then falls back to walking eng.AllNodes() Go-side, identical to
// the pre-capability shape. Backends that ship the capability
// (the disk backend) collapse the per-call node fetch into one query
// scoped to the trigram-match file set — on the gortex workspace
// that was ~70k AllNodes() rows over the storage boundary just to keep the few
// hundred whose FilePath sat in the small match-file set.
func buildDeclFileIndex(eng *query.Engine, finder graph.NodesInFilesByKindFinder, matches []trigram.Match) map[string]*fileSymbolIndex {
	wanted := make(map[string]struct{}, len(matches))
	files := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, ok := wanted[m.Path]; ok {
			continue
		}
		wanted[m.Path] = struct{}{}
		files = append(files, m.Path)
	}
	out := make(map[string]*fileSymbolIndex, len(wanted))

	add := func(n *graph.Node) {
		if n == nil {
			return
		}
		idx := out[n.FilePath]
		if idx == nil {
			idx = &fileSymbolIndex{}
			out[n.FilePath] = idx
		}
		idx.add(n)
	}

	if finder != nil {
		kinds := []graph.NodeKind{
			graph.KindFunction,
			graph.KindMethod,
			graph.KindClosure,
			graph.KindType,
			graph.KindInterface,
		}
		for _, n := range finder.NodesInFilesByKind(files, kinds) {
			if _, ok := wanted[n.FilePath]; !ok {
				continue
			}
			add(n)
		}
	} else {
		for _, n := range eng.AllNodes() {
			if _, ok := wanted[n.FilePath]; !ok {
				continue
			}
			switch n.Kind {
			case graph.KindFunction, graph.KindMethod, graph.KindClosure, graph.KindType, graph.KindInterface:
				add(n)
			}
		}
	}
	for _, idx := range out {
		idx.finalise()
	}
	return out
}

// resolveUseSiteDecl resolves a single match to a declaration node ID.
// Primary path: find the enclosing symbol, walk its outgoing edges, and
// pick the resolve-kind edge whose Line equals the match line — its To
// is the declaration. Fallback: extract the called identifier from the
// match text and resolve it by name.
func resolveUseSiteDecl(eng *query.Engine, fileIdx map[string]*fileSymbolIndex, m trigram.Match) string {
	if idx := fileIdx[m.Path]; idx != nil {
		if enclosingID, _ := idx.find(m.Line); enclosingID != "" {
			best := ""
			for _, e := range eng.GetOutEdges(enclosingID) {
				if e.Line != m.Line || !declResolveKinds[e.Kind] {
					continue
				}
				if graph.IsUnresolvedTarget(e.To) || strings.HasPrefix(e.To, "external::") {
					continue
				}
				// Prefer a call edge over a plain reference when the
				// same line carries both (a call line also references
				// the receiver type).
				if best == "" || e.Kind == graph.EdgeCalls {
					best = e.To
				}
			}
			if best != "" {
				return best
			}
		}
	}

	// Fallback — name-based resolution off the called identifier.
	ident := identifierBeforeParen(m.Text)
	if ident == "" {
		return ""
	}
	for _, n := range eng.FindSymbols(ident) {
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface, graph.KindVariable:
			return n.ID
		}
	}
	return ""
}

// identifierBeforeParen extracts a call-target identifier from a line —
// the identifier immediately preceding the first '('. Returns "" when
// the line has no call-shaped expression or the run before '(' is not
// a valid identifier.
func identifierBeforeParen(line string) string {
	idx := strings.Index(line, "(")
	if idx <= 0 {
		return ""
	}
	end := idx
	start := end
	for start > 0 && isIdentByte(line[start-1]) {
		start--
	}
	if start == end {
		return ""
	}
	ident := line[start:end]
	if ident[0] >= '0' && ident[0] <= '9' {
		return ""
	}
	return ident
}

// isIdentByte reports whether c can appear in an identifier.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// parseDeclKindCSV parses a comma-separated node-kind filter into a
// lowercased set. Empty input yields an empty set ("no filter").
func parseDeclKindCSV(csv string) map[string]bool {
	out := make(map[string]bool)
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			out[strings.ToLower(tok)] = true
		}
	}
	return out
}
