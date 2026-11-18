package lsp

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// lspSite is a server-reported source position the enrichment passes map
// back onto a graph node. Graph nodes carry line spans only, so every member
// written on one line — `public void Tally() { } public Box Fetch() { ... }` —
// contains that line equally; the column and the server's own name for the
// item are what tell them apart. Columns are 0-based characters, compared
// against byte offsets of the source line the same way identifierColumn
// positions requests (exact for ASCII lines).
type lspSite struct {
	line int    // 1-based line
	col  int    // 0-based character on line; < 0 when unknown
	name string // server's display name for the item at the position ("" when unknown)
	// src lazily yields the site file's content; nil (or a nil result)
	// when it is unavailable. Only consulted to break a same-line tie, so
	// the common single-candidate case never reads the file.
	src func() []byte
}

// lineSite is a position known only by its 1-based line.
func lineSite(line int) lspSite { return lspSite{line: line, col: -1} }

// disambiguateSameLine picks the one node among tied — candidates whose line
// spans contain the site equally — that the server's position names. A
// single candidate is returned as is. Otherwise the server's item name
// filters the set, and the column chooses the member whose declaration name
// is the last one starting at or before it on the line. Anything it cannot
// prove unique returns nil: the caller then mints nothing rather than a
// confident edge under the wrong member.
func disambiguateSameLine(tied []*graph.Node, site lspSite) *graph.Node {
	if len(tied) == 1 {
		return tied[0]
	}
	if len(tied) == 0 {
		return nil
	}
	if ident := hierarchyItemIdent(site.name); ident != "" {
		var named []*graph.Node
		for _, n := range tied {
			if n.Name == ident {
				named = append(named, n)
			}
		}
		switch len(named) {
		case 0:
			return nil
		case 1:
			return named[0]
		}
		tied = named
	}
	if site.col < 0 || site.src == nil {
		return nil
	}
	text, ok := sourceLine(site.src(), site.line)
	if !ok {
		return nil
	}
	var best *graph.Node
	bestCol, bestTies := -1, 0
	for _, n := range tied {
		cols := identifierIndices(text, n.Name)
		if len(cols) != 1 {
			// The name is absent (a synthesized closure name) or repeated
			// on the line: its declaration column cannot be pinned.
			return nil
		}
		c := cols[0]
		if c > site.col {
			continue
		}
		switch {
		case c > bestCol:
			best, bestCol, bestTies = n, c, 1
		case c == bestCol:
			bestTies++
		}
	}
	if bestTies != 1 {
		return nil
	}
	return best
}

// hierarchyItemIdent lowers a server's call / type hierarchy item name to the
// bare declaration identifier graph nodes carry: servers decorate it with a
// signature (`Fetch() : Box`, `Fetch(int)`), generics (`List<T>`) or a
// qualifier (`Box.serialize`, `Box::serialize`).
func hierarchyItemIdent(name string) string {
	if i := strings.IndexAny(name, "( <"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

// sourceLine returns the text of the 1-based line of src, without its
// newline. ok is false when src has no such line.
func sourceLine(src []byte, oneBasedLine int) (string, bool) {
	if oneBasedLine <= 0 || len(src) == 0 {
		return "", false
	}
	target := oneBasedLine - 1
	lineStart := 0
	for cur := 0; cur < len(src) && target > 0; cur++ {
		if src[cur] == '\n' {
			target--
			lineStart = cur + 1
		}
	}
	if target > 0 {
		return "", false
	}
	lineEnd := lineStart
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	return string(src[lineStart:lineEnd]), true
}

// identifierIndices returns the column of every occurrence of name in line
// that is a whole identifier, in order.
func identifierIndices(line, name string) []int {
	var out []int
	for from := 0; from <= len(line); {
		idx := identifierIndex(line[from:], name)
		if idx < 0 {
			break
		}
		out = append(out, from+idx)
		from += idx + len(name)
	}
	return out
}

// siteDefinitionAnswer is the server's textDocument/definition answer for
// one occurrence (col) of the site identifier.
type siteDefinitionAnswer struct {
	col  int
	locs []Location
}

// resolveSiteDefinition maps the definition answers for every occurrence of
// the site identifier on the edge's line to the graph node the edge's site
// binds to. wantID is the edge's current target.
//
// A line can carry the identifier more than once — a one-line method whose
// body calls a same-named member on another receiver,
// `serialize() { return this.leaf.serialize(); }`. Asking only at the first
// occurrence asks about the DECLARATION token, whose definition is itself,
// and confirms a false self-recursive edge. So an occurrence whose
// definition lands on itself is a declaration, not a use, and is dropped.
// Of the remaining uses: the edge's own target among them confirms it;
// otherwise a single distinct declaration across every use is the verdict.
// Uses that disagree, or a use the server could not anchor to a graph node,
// leave no verdict — never a guess between them.
func resolveSiteDefinition(view *lspGraphView, repoPrefix, absRoot, siteRel string, siteLine int, name, wantID string, answers []siteDefinitionAnswer) *graph.Node {
	var (
		found      []*graph.Node
		unanchored bool
	)
	for _, a := range answers {
		if len(a.locs) == 0 {
			unanchored = true
			continue
		}
		loc := a.locs[0]
		if definitionAtOccurrence(loc, absRoot, siteRel, siteLine, a.col) {
			continue
		}
		defPath := uriToPath(loc.URI, absRoot)
		if defPath == "" {
			unanchored = true // outside the workspace (stdlib, deps, …)
			continue
		}
		node := view.findDeclarationNode(scopedPath(repoPrefix, defPath), loc.Range.Start.Line+1, name)
		if node == nil {
			// A definition can land on a member declaration INSIDE the
			// target type — `new T(...)` answers the constructor's line,
			// not the type's, and the ctor node carries its own name. A
			// type declaration named exactly `name` whose span contains
			// the answered line is the same verdict: the site binds to
			// that type.
			node = view.findEnclosingTypeNamed(scopedPath(repoPrefix, defPath), loc.Range.Start.Line+1, name)
		}
		if node == nil {
			unanchored = true
			continue
		}
		if node.ID == wantID {
			return node
		}
		found = append(found, node)
	}
	if unanchored || len(found) == 0 {
		return nil
	}
	for _, n := range found[1:] {
		if n.ID != found[0].ID {
			return nil
		}
	}
	return found[0]
}

// definitionAtOccurrence reports whether a definition answer points back at
// the very identifier it was asked about (siteLine 1-based, col 0-based) —
// the shape of a declaration token, whose definition is itself.
func definitionAtOccurrence(loc Location, absRoot, siteRel string, siteLine, col int) bool {
	if viewPathKey(uriToPath(loc.URI, absRoot)) != viewPathKey(siteRel) {
		return false
	}
	r := loc.Range
	line := siteLine - 1
	if r.Start.Line != line || r.Start.Character > col {
		return false
	}
	return r.End.Line > line || r.End.Character > col
}

// hierarchyItemSite is the position of a call / type hierarchy item: its
// selection range (the declaration's name) plus the server's name for it.
func (p *Provider) hierarchyItemSite(absRoot, rel, name string, sel Range) lspSite {
	return lspSite{
		line: sel.Start.Line + 1,
		col:  sel.Start.Character,
		name: name,
		src:  p.fileSource(absRoot, rel),
	}
}

// fileSource returns a lazy loader for rel's content: the copy the provider
// holds for an opened document, else the file on disk.
func (p *Provider) fileSource(absRoot, rel string) func() []byte {
	return func() []byte {
		if src := p.getSource(absRoot, rel); src != nil {
			return src
		}
		src, err := os.ReadFile(filepath.Join(absRoot, rel))
		if err != nil {
			return nil
		}
		return src
	}
}

// sameLineNamesakes returns the IDs of the hierarchy-swept declarations
// (functions, methods, types, interfaces) that share both name and start line
// with another one — `class Leaf { serialize() {} } class Box { serialize() {} }`.
// A request positioned by identifierColumn cannot tell them apart.
func sameLineNamesakes(nodes []*graph.Node) map[string]bool {
	type key struct {
		name string
		line int
	}
	byKey := map[key][]string{}
	for _, n := range nodes {
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface:
			k := key{n.Name, n.StartLine}
			byKey[k] = append(byKey[k], n.ID)
		}
	}
	var out map[string]bool
	for _, ids := range byKey {
		if len(ids) < 2 {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}
