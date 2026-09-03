package mcp

import (
	"path"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/resolver"
)

// Barrel re-export resolve-through for find_usages.
//
// A JS/TS barrel forwards a binding it does not declare —
// `export { persist } from './middleware/persist'` — and the extractor records
// that only as an EdgeReExports edge from the barrel FILE; it mints no node for
// the forwarded binding. So a consumer's public import path
// (`src/middleware.ts::persist`) has nothing to resolve and find_usages returns
// not_found even though the canonical declaration has usages.
//
// reExportBindingCanonical maps such a barrel-binding id to the canonical
// declaration id by walking the re-export chain (named, aliased, and chained
// barrels, plus `export * from`). It is consulted ONLY when the queried id is
// not itself a node, so it can never change the answer for a real symbol — the
// canonical strata stay byte-identical. Bare / path-aliased specifiers (needing
// tsconfig `paths` or npm-alias resolution the query layer does not carry) are
// out of scope; relative barrels — the dominant form — are covered.

// jsBarrelExts are the module file extensions probed when resolving a relative
// re-export specifier to a file node, in precedence order.
var jsBarrelExts = []string{".ts", ".tsx", ".mts", ".cts", ".d.ts", ".js", ".jsx", ".mjs", ".cjs"}

const maxReExportChainDepth = 8

// reExportBindingCanonical returns the canonical declaration id a barrel-binding
// id forwards to, or "" when id is not a resolvable barrel binding.
func reExportBindingCanonical(g graph.Reader, id string, depth int) string {
	if g == nil || depth > maxReExportChainDepth {
		return ""
	}
	barrelFile, name, ok := splitBarrelID(id)
	if !ok || !hasJSTSExt(barrelFile) {
		return ""
	}
	if g.GetNode(barrelFile) == nil {
		return ""
	}
	for _, e := range g.GetOutEdges(barrelFile) {
		if e == nil || e.Kind != graph.EdgeReExports {
			continue
		}
		spec, orig := parseReExportTarget(e.To)
		if spec == "" || !strings.HasPrefix(spec, ".") {
			continue // bare / aliased specifier — out of scope
		}
		switch {
		case orig != "":
			// Named (optionally aliased) re-export. The public binding name is
			// the alias when renamed, else the original.
			binding := e.Alias
			if binding == "" {
				binding = orig
			}
			if binding != name {
				continue
			}
			if canon := probeBindingInFile(g, barrelFile, spec, orig, depth); canon != "" {
				return canon
			}
		case e.Alias == "":
			// `export * from './x'` — any of x's exports is forwarded under its
			// own name, so probe the target for the queried name.
			if canon := probeBindingInFile(g, barrelFile, spec, name, depth); canon != "" {
				return canon
			}
		}
	}
	return ""
}

// probeBindingInFile resolves the relative spec to a target file and returns
// the canonical id for `orig` there — either a real symbol node, or, when the
// target is itself a barrel, the result of recursing through it.
func probeBindingInFile(g graph.Reader, fromFile, spec, orig string, depth int) string {
	tf := probeRelativeModuleFile(g, fromFile, spec)
	if tf == "" {
		return ""
	}
	if canon := tf + "::" + orig; g.GetNode(canon) != nil {
		return canon
	}
	return reExportBindingCanonical(g, tf+"::"+orig, depth+1)
}

// splitBarrelID splits `<file>::<name>` on the last `::`.
func splitBarrelID(id string) (file, name string, ok bool) {
	i := strings.LastIndex(id, "::")
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+2:], true
}

// parseReExportTarget decodes an EdgeReExports edge's target
// (`unresolved::import::<spec>[::<orig>]`) into its import specifier and, for a
// named re-export, the original binding name ("" for a wildcard / namespace).
func parseReExportTarget(to string) (spec, orig string) {
	payload, ok := strings.CutPrefix(graph.UnresolvedName(to), "import::")
	if !ok || payload == "" {
		return "", ""
	}
	if i := strings.LastIndex(payload, "::"); i >= 0 {
		return payload[:i], payload[i+2:]
	}
	return payload, ""
}

// probeRelativeModuleFile joins a relative specifier against the importing
// file's directory and returns the matching file node's id, trying the module
// extensions and an index file, or "" when no such file node exists.
//
// A specifier carrying an emitted-JavaScript extension — `./impl.js` for a
// module whose source is `impl.ts`, mandatory under `moduleResolution:
// node16` / `nodenext` — is probed verbatim and then against the sources
// that compile to it; appending extensions alone would only ever look for
// `impl.js.ts` (issue #304).
func probeRelativeModuleFile(g graph.Reader, fromFile, spec string) string {
	stem := path.Clean(path.Join(path.Dir(fromFile), spec))
	isFile := func(id string) bool {
		n := g.GetNode(id)
		return n != nil && n.Kind == graph.KindFile
	}
	if ext := path.Ext(stem); ext != "" {
		if isFile(stem) {
			return stem
		}
		base := strings.TrimSuffix(stem, ext)
		for _, srcExt := range resolver.EmittedJSSourceExts(ext) {
			if isFile(base + srcExt) {
				return base + srcExt
			}
		}
	}
	for _, ext := range jsBarrelExts {
		if g.GetNode(stem+ext) != nil {
			return stem + ext
		}
	}
	for _, ext := range jsBarrelExts {
		if cand := path.Join(stem, "index"+ext); g.GetNode(cand) != nil {
			return cand
		}
	}
	return ""
}

// hasJSTSExt reports whether a file path is a JS/TS module the barrel walk
// applies to.
func hasJSTSExt(file string) bool {
	for _, ext := range jsBarrelExts {
		if strings.HasSuffix(file, ext) {
			return true
		}
	}
	return false
}

// isReExportNode reports whether a node is a barrel re-export binding minted
// at an `export { X } from './mod'` site. Thin alias over graph.IsReExportNode
// so the find_usages delegation reads at the call site.
func isReExportNode(n *graph.Node) bool { return graph.IsReExportNode(n) }

// reExportNodeCanonical follows a barrel re-export node's outgoing
// EdgeReExports edge(s) to the terminal canonical declaration it forwards.
// It walks the resolved graph — the node→canonical edge the extractor mints is
// rewritten from `unresolved::import::…` onto the declaration id by the
// resolver — so a chain of barrels (index → middleware → persist) resolves hop
// by hop until a non-re-export node is reached. Returns "" when the node has no
// resolved forward edge (e.g. a bare-package re-export the resolver left
// unresolved). Distinct from reExportBindingCanonical, which walks a FILE's
// out-edges on the pre-resolution unresolved targets.
func reExportNodeCanonical(g graph.Reader, id string, depth int) string {
	if g == nil || depth > maxReExportChainDepth {
		return ""
	}
	for _, e := range g.GetOutEdges(id) {
		if e == nil || e.Kind != graph.EdgeReExports {
			continue
		}
		to := e.To
		if to == "" || to == id || graph.IsUnresolvedTarget(to) {
			continue // unresolved / self — nothing to delegate to
		}
		tn := g.GetNode(to)
		if tn == nil {
			continue
		}
		if isReExportNode(tn) {
			if canon := reExportNodeCanonical(g, to, depth+1); canon != "" {
				return canon
			}
			// An intermediate barrel whose own forward edge didn't resolve
			// still stands in as the best delegation target.
			return to
		}
		return to
	}
	return ""
}

// mergeUsageSubGraph folds src's usage rows into dst, deduping nodes by id and
// edges by (from,to,kind,line), and refreshes the totals. Used by find_usages
// to union a barrel binding's own refs with its canonical declaration's usages.
func mergeUsageSubGraph(dst, src *query.SubGraph) {
	if dst == nil || src == nil {
		return
	}
	haveNode := make(map[string]struct{}, len(dst.Nodes))
	for _, n := range dst.Nodes {
		if n != nil {
			haveNode[n.ID] = struct{}{}
		}
	}
	for _, n := range src.Nodes {
		if n == nil {
			continue
		}
		if _, dup := haveNode[n.ID]; dup {
			continue
		}
		haveNode[n.ID] = struct{}{}
		dst.Nodes = append(dst.Nodes, n)
	}
	edgeKey := func(e *graph.Edge) string {
		return e.From + "\x00" + e.To + "\x00" + string(e.Kind) + "\x00" + strconv.Itoa(e.Line)
	}
	haveEdge := make(map[string]struct{}, len(dst.Edges))
	for _, e := range dst.Edges {
		if e != nil {
			haveEdge[edgeKey(e)] = struct{}{}
		}
	}
	for _, e := range src.Edges {
		if e == nil {
			continue
		}
		k := edgeKey(e)
		if _, dup := haveEdge[k]; dup {
			continue
		}
		haveEdge[k] = struct{}{}
		dst.Edges = append(dst.Edges, e)
	}
	dst.TotalNodes = len(dst.Nodes)
	dst.TotalEdges = len(dst.Edges)
}
