package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// attributeGoExternalCalls materialises a KindFunction node for every
// unique `stdlib::<importPath>::<symbol>` / `dep::<importPath>::<symbol>`
// / `external::<importPath>::<symbol>` edge target, plus a KindModule
// parent for each owning import path. Without this pass the targets
// are stubs in storage backends that enforce rel-table FK (the on-disk backend)
// and invisible nodes in the in-memory backend, so a query like
// `find_usages(stdlib::encoding/json::Marshal)`
// can't surface "every function in this codebase that calls
// json.Marshal" — the destination doesn't exist as a graph node.
//
// Mirrors the Python / Dart attributeNonGoModuleImports pass for Go.
// Runs after resolveExtern (which classifies extern targets into the
// three prefix buckets) so we materialise the post-classification
// state rather than the pre-classification `unresolved::extern::*`
// shape.
//
// ID conventions:
//   - Module node:    `module::go:<importPath>` — shared across every
//     repo that imports the same path. Carries
//     Meta["ecosystem"]="go" and Meta["import_path"]=<path>.
//     Meta["role"]="stdlib" for stdlib paths.
//   - Symbol node:    the original `stdlib::*` / `dep::*` /
//     `external::*` ID stays the symbol's ID so existing edges land
//     on it without rewriting. Carries Meta["external"]=true and
//     Meta["module_path"]=<importPath>.
//   - EdgeMemberOf:   symbol → module so `get_callers` on the module
//     surfaces every symbol used from that package.
//
// All AddNode / AddEdge calls are idempotent on ID, so a second run
// of this pass (incremental ResolveFile re-invocation) is a no-op.
// extKey identifies a unique external target across the attribution
// passes; modKey identifies its owning module. Package-level so the
// whole-graph and single-file collectors feed one materialiser.
//
// repoPrefix is part of the key because stdlib stubs are per-repo (see
// internal/graph/stub.go) — two repos on different Go SDK versions emit
// semantically distinct `<repoA>::stdlib::fmt::Errorf` and
// `<repoB>::stdlib::fmt::Errorf` stubs that MUST round-trip through this
// attribution as distinct nodes, not collide into one.
type extKey struct {
	repoPrefix, prefix, importPath, symbol string
}

type modKey struct{ repoPrefix, importPath string }

// goExternalAttribKinds is every edge kind an extern-prefixed target can
// show up on — the same set attributeGoBuiltins scans.
var goExternalAttribKinds = []graph.EdgeKind{
	graph.EdgeCalls,
	graph.EdgeReferences,
	graph.EdgeReads,
	graph.EdgeArgOf,
	graph.EdgeValueFlow,
	graph.EdgeReturnsTo,
	graph.EdgeTypedAs,
	graph.EdgeReturns,
	graph.EdgeInstantiates,
	graph.EdgeCaptures,
	graph.EdgeThrows,
}

func (r *Resolver) attributeGoExternalCalls() {
	// Go-only pass: skip the external-prefix edge scan when the graph has
	// no Go nodes.
	if !r.graphHasLanguage("go") {
		return
	}
	seen := map[extKey]struct{}{}
	for _, target := range r.graph.DistinctExternalTargets(goExternalAttribKinds) {
		collectGoExternalTargetID(target, seen)
	}
	r.materializeGoExternalSeen(seen)
}

// attributeGoExternalCallsForFile is the single-file scope of
// attributeGoExternalCalls: an extern-prefixed target is referenced from
// inside the edited file, so only that file's outgoing edges can
// introduce a new one. Produces the same materialisation as the
// whole-graph sweep for a per-save resolve.
func (r *Resolver) attributeGoExternalCallsForFile(filePath string) {
	if !r.graphHasLanguage("go") {
		return
	}
	seen := map[extKey]struct{}{}
	for _, e := range r.fileOutEdges(filePath) {
		collectGoExternalTarget(e, seen)
	}
	r.materializeGoExternalSeen(seen)
}

// goExternalAttribKindSet is goExternalAttribKinds as a set.
var goExternalAttribKindSet = func() map[graph.EdgeKind]struct{} {
	out := make(map[graph.EdgeKind]struct{}, len(goExternalAttribKinds))
	for _, k := range goExternalAttribKinds {
		out[k] = struct{}{}
	}
	return out
}()

// collectGoExternalTarget records e's external target (if any) into seen,
// deduping by the per-repo (prefix, path, symbol) tuple. Only the kinds the
// whole-graph sweep reads count: a file's import edge to `external::os`
// would otherwise materialize a module node (unscoped, since the import
// target carries no repository) that a whole index never has.
func collectGoExternalTarget(e *graph.Edge, seen map[extKey]struct{}) {
	if e == nil || e.To == "" {
		return
	}
	if _, attributed := goExternalAttribKindSet[e.Kind]; !attributed {
		return
	}
	collectGoExternalTargetID(e.To, seen)
}

func collectGoExternalTargetID(target string, seen map[extKey]struct{}) {
	if target == "" {
		return
	}
	prefix, importPath, symbol := splitGoExternalTarget(target)
	if prefix == "" {
		return
	}
	seen[extKey{graph.StubRepoPrefix(target), prefix, importPath, symbol}] = struct{}{}
}

// materializeGoExternalSeen turns the collected external targets into
// KindModule + KindFunction nodes and their EdgeMemberOf links. Mutations are
// flushed in bounded batches so a large workspace does not issue one SQLite
// transaction per external target or retain the entire materialization in
// memory. AddBatch remains idempotent, so re-resolve is a no-op topologically.
func (r *Resolver) materializeGoExternalSeen(seen map[extKey]struct{}) {
	if len(seen) == 0 {
		return
	}

	// Build the desired node/edge identity sets first. ExistingNodeIDs projects
	// only primary keys and GetEdgeCandidates returns only exact membership
	// links, so warm runs do not decode or rewrite every external node again.
	modules := make(map[modKey]*graph.Node)
	symbols := make(map[string]*graph.Node)
	endpointSet := make(map[graph.EdgeEndpoint]struct{}, len(seen))
	for k := range seen {
		mk := modKey{repoPrefix: k.repoPrefix, importPath: k.importPath}
		module := modules[mk]
		role := goExternalModuleRole(k.prefix)
		if module != nil && goExternalModuleRoleRank(role) > goExternalModuleRoleRank(module.Meta["role"]) {
			// One module reached through several terminal shapes (a call
			// bound to `dep::`, an import landing on `external::`) carries
			// the strongest role whatever order the keys were visited in.
			module.Meta["role"] = role
		}
		if module == nil {
			module = &graph.Node{
				ID:         graph.StubID(k.repoPrefix, graph.StubKindModule, "go:"+k.importPath),
				Kind:       graph.KindModule,
				Name:       lastImportSegment(k.importPath),
				Language:   "go",
				RepoPrefix: k.repoPrefix,
				Meta: map[string]any{
					"ecosystem":   "go",
					"role":        role,
					"import_path": k.importPath,
				},
			}
			modules[mk] = module
		}
		// A package-only target creates its module but no malformed empty-name
		// function. This keeps partial file attribution identical to the global
		// pass, whose edge-kind scope normally excludes import-only targets.
		if k.symbol == "" {
			continue
		}

		var symbolID string
		symbolRepoPrefix := ""
		switch k.prefix {
		case "stdlib::":
			symbolID = graph.StubID(k.repoPrefix, graph.StubKindStdlib, k.importPath, k.symbol)
			symbolRepoPrefix = k.repoPrefix
		default:
			// dep:: / external:: keep their shared legacy ID. Their node remains
			// global while per-repo module links preserve workspace ownership.
			symbolID = k.prefix + k.importPath + "::" + k.symbol
		}
		if symbols[symbolID] == nil {
			symbols[symbolID] = &graph.Node{
				ID:         symbolID,
				Kind:       graph.KindFunction,
				Name:       k.symbol,
				Language:   "go",
				RepoPrefix: symbolRepoPrefix,
				Meta: map[string]any{
					"external":    true,
					"module_path": k.importPath,
					"module_role": map[string]string{
						"stdlib::":   "stdlib",
						"dep::":      "dep",
						"external::": "external",
					}[k.prefix],
				},
			}
		}
		endpointSet[graph.EdgeEndpoint{From: symbolID, To: module.ID}] = struct{}{}
	}

	ids := make([]string, 0, len(modules)+len(symbols))
	for _, module := range modules {
		ids = append(ids, module.ID)
	}
	for id := range symbols {
		ids = append(ids, id)
	}
	existingNodes := graph.LookupExistingNodeIDs(r.graph, ids)
	// A module node that already exists is rewritten only when this pass
	// reaches it with a stronger role, so the stored role is the strongest
	// one any pass saw — the same row a whole index writes, whichever pass
	// (an import leg, a call leg) minted the module first.
	upgrade := map[string]bool{}
	if len(modules) > 0 {
		moduleIDs := make([]string, 0, len(modules))
		for _, module := range modules {
			if _, exists := existingNodes[module.ID]; exists {
				moduleIDs = append(moduleIDs, module.ID)
			}
		}
		if len(moduleIDs) > 0 {
			stored := r.graph.GetNodesByIDs(moduleIDs)
			for _, module := range modules {
				if prior := stored[module.ID]; prior != nil &&
					goExternalModuleRoleRank(module.Meta["role"]) > goExternalModuleRoleRank(prior.Meta["role"]) {
					upgrade[module.ID] = true
				}
			}
		}
	}
	endpoints := make([]graph.EdgeEndpoint, 0, len(endpointSet))
	endpointsBySymbol := make(map[string][]graph.EdgeEndpoint, len(symbols))
	for endpoint := range endpointSet {
		endpoints = append(endpoints, endpoint)
		endpointsBySymbol[endpoint.From] = append(endpointsBySymbol[endpoint.From], endpoint)
	}
	existingEdges := graph.LookupEdgeCandidates(r.graph, endpoints, nil)

	const materializeBatchSize = 5000
	nodes := make([]*graph.Node, 0, materializeBatchSize)
	edges := make([]*graph.Edge, 0, materializeBatchSize)
	flush := func() {
		if len(nodes) == 0 && len(edges) == 0 {
			return
		}
		r.graph.AddBatch(nodes, edges)
		nodes = nodes[:0]
		edges = edges[:0]
	}
	for _, module := range modules {
		if _, exists := existingNodes[module.ID]; !exists || upgrade[module.ID] {
			nodes = append(nodes, module)
		}
		if len(nodes) >= materializeBatchSize {
			flush()
		}
	}
	for id, symbol := range symbols {
		if _, exists := existingNodes[id]; !exists {
			nodes = append(nodes, symbol)
		}
		for _, endpoint := range endpointsBySymbol[id] {
			if existingEdges.EndpointKind(endpoint.From, endpoint.To, graph.EdgeMemberOf) == nil {
				edges = append(edges, &graph.Edge{
					From: endpoint.From, To: endpoint.To,
					Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved,
				})
			}
		}
		if len(nodes) >= materializeBatchSize || len(edges) >= materializeBatchSize {
			flush()
		}
	}
	flush()
}

// goExternalModuleRole is the role a Go module node gets from one terminal
// prefix.
func goExternalModuleRole(prefix string) string {
	switch prefix {
	case "stdlib::":
		return "stdlib"
	case "dep::":
		return "dep"
	default:
		return "external"
	}
}

// goExternalModuleRoleRank orders module roles: a module reached as a
// declared dependency (or the standard library) is described by that, not by
// the weaker "external" an unmatched import gives it.
func goExternalModuleRoleRank(role any) int {
	switch role {
	case "stdlib":
		return 3
	case "dep":
		return 2
	case "external":
		return 1
	default:
		return 0
	}
}

// splitGoExternalTarget recognises the three external-target prefixes
// the resolver emits after resolveExtern. Returns the prefix
// (`stdlib::` / `dep::` / `external::`), the import path, and the
// symbol name. Returns ("", "", "") for any other shape so the pass
// can skip it cleanly.
//
// The stdlib case is matched via graph.IsStdlibStub so both the
// legacy `stdlib::fmt::Errorf` shape and the per-repo-prefixed
// `<repo>::stdlib::fmt::Errorf` shape (see internal/graph/stub.go)
// route the same way. The returned bucket label stays `stdlib::` for
// downstream `k.prefix == "stdlib::"` comparisons.
func splitGoExternalTarget(target string) (prefix, importPath, symbol string) {
	var body string
	switch {
	case graph.IsStdlibStub(target):
		prefix = "stdlib::"
		body = graph.StubRest(target)
	case strings.HasPrefix(target, "dep::"):
		prefix = "dep::"
		body = strings.TrimPrefix(target, prefix)
	case strings.HasPrefix(target, "external::"):
		prefix = "external::"
		body = strings.TrimPrefix(target, prefix)
	default:
		return "", "", ""
	}
	// The body shape produced by resolveExtern is
	// `<importPath>::<symbol>`. Split on the LAST `::` because import
	// paths can include slashes but not `::`, so the rightmost
	// separator is always between path and symbol.
	sep := strings.LastIndex(body, "::")
	if sep < 0 {
		// `external::os` style (just the package, no symbol —
		// the resolveImport path). Treat the whole body as the path
		// and leave symbol empty so we still materialise the module
		// node but skip the symbol.
		return prefix, body, ""
	}
	return prefix, body[:sep], body[sep+2:]
}

// lastImportSegment returns the rightmost path component, used as
// the human-readable Name on the KindModule node. For
// `github.com/stretchr/testify/assert` the segment is `assert`; for
// `encoding/json` it's `json`; for `fmt` it's `fmt`.
func lastImportSegment(importPath string) string {
	if importPath == "" {
		return ""
	}
	return path.Base(importPath)
}
