package resolver

import (
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// rebindGoMethodReceivers fixes Go EdgeMemberOf edges whose target is
// a phantom `<methodfile>::TypeName` ID — the artefact of the Go
// extractor building the receiver-type endpoint from the method's own
// file rather than the file the type is actually declared in. Methods
// spread across multiple files in the same package each emit a
// different `<file>::Type` target even though they all logically
// belong to the single type node defined elsewhere.
//
// Without this pass:
//   - the on-disk backend materialises phantom Node rows to satisfy the
//     rel-table FK on every cross-file method-receiver edge;
//   - InferImplements builds a typeID → method-set map keyed on the
//     phantom IDs, so a type whose methods span N files appears as N
//     partial types each with a fraction of the real method set, and
//     interface satisfaction is under-detected;
//   - find_implementations / get_class_hierarchy / get_callers over
//     interface methods all return partial results for cross-file-
//     method types (which is most of any non-trivial Go codebase).
//
// Algorithm: index every Go KindType / KindInterface node by
// (filePathDir(file), name); walk EdgeMemberOf; for each Go method
// whose To doesn't resolve, look up (its file's dir, type name); if
// exactly one match, rewrite edge.To to the canonical type ID via
// ReindexEdges (one batched commit instead of per-edge round-trips).
//
// Scope: Go only — other languages (Java / TS / Python) group methods
// inside the class body in the same file, so the cross-file pattern
// doesn't arise. The method node's Language gates the rebind.
// pkgKey identifies a Go type by repository, package directory, and name — the
// key the receiver-rebind type index is built on. Package-level so the
// whole-graph and per-file builders share it.
type pkgKey struct{ repo, pkg, name string }

func (r *Resolver) rebindGoMethodReceivers() {
	if rebinder, ok := r.graph.(graph.GoMethodReceiverRebinder); ok {
		if _, err := rebinder.RebindGoMethodReceivers(""); err == nil {
			return
		} else {
			r.logger.Warn("resolver: backend Go receiver rebind failed; using graph fallback", zap.Error(err))
		}
	}

	typesIdx := make(map[pkgKey]string)
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
		// Server-side language scope: only Go type/interface nodes cross
		// the cgo boundary. On a graph with few/no Go types (e.g. a TS
		// repo) this avoids marshaling + meta-decoding every type node
		// just to discard the non-Go majority — the bulk of this pass's
		// cost on a large single-language graph.
		for n := range r.nodesByKindLang(kind, "go") {
			addGoTypeToIndex(typesIdx, n)
		}
	}
	if len(typesIdx) == 0 {
		return
	}
	var memberOf []*graph.Edge
	for e := range r.graph.EdgesByKind(graph.EdgeMemberOf) {
		memberOf = append(memberOf, e)
	}
	r.rebindMemberOf(typesIdx, memberOf)
}

// rebindGoMethodReceiversForFile is the single-file scope of
// rebindGoMethodReceivers. A Go method's receiver type lives in the same
// package (directory) as the method, and only the edited file's methods
// carry a freshly-extracted phantom `<file>::Type` receiver — so the type
// index is built from just that package's files and the MemberOf edges
// from just the edited file's methods, instead of indexing every Go type
// and scanning every MemberOf edge in the graph on each save (the
// dominant per-edit cost on a large graph). dirIndex (built by
// buildPassIndexes before this runs) supplies the package's files.
func (r *Resolver) rebindGoMethodReceiversForFile(filePath string) {
	if rebinder, ok := r.graph.(graph.GoMethodReceiverRebinder); ok {
		if _, err := rebinder.RebindGoMethodReceivers(filePath); err == nil {
			return
		} else {
			r.logger.Warn("resolver: backend scoped Go receiver rebind failed; using graph fallback",
				zap.String("file", filePath), zap.Error(err))
		}
	}

	fileNodes := r.incrementalFileNodes(filePath)
	// Only Go methods can be rebound below. The accepted full-file cache
	// also records empty files, so free-function edits and deletions need no
	// package type scan or adjacency read on the graph fallback.
	hasGoMethod := false
	for _, n := range fileNodes {
		if n != nil && n.Kind == graph.KindMethod && n.Language == "go" {
			hasGoMethod = true
			break
		}
	}
	if !hasGoMethod {
		return
	}

	// The package's type index is a pure function of the directory's files and
	// never mutated by the tail passes, so memoize it: a scoped tail visiting
	// every file of a D-file package builds it once (O(D)) instead of per file
	// (O(D^2)). The empty result is cached too so a package with no Go types
	// isn't re-scanned per file.
	repoPrefix := ""
	for _, n := range fileNodes {
		if n != nil && n.RepoPrefix != "" {
			repoPrefix = n.RepoPrefix
			break
		}
	}
	dir := filePathDir(filePath)
	cacheKey := repoPrefix + "\x00" + dir
	typesIdx, ok := r.receiverTypeIdxByDir[cacheKey]
	if !ok {
		typesIdx = make(map[pkgKey]string)
		// The normal SQLite path never reaches this fallback. For in-memory and
		// failed-capability recovery, fetch the package's files through one
		// set-oriented compound query instead of rescanning every Go type once
		// per package or issuing one file query per package member.
		var packageFiles []string
		for _, fileNode := range r.dirIndex[dir] {
			if fileNode.RepoPrefix == repoPrefix && fileNode.FilePath != "" {
				packageFiles = append(packageFiles, fileNode.FilePath)
			}
		}
		if finder, supported := r.graph.(graph.NodesInFilesByKindFinder); supported && len(packageFiles) > 0 {
			for _, n := range finder.NodesInFilesByKind(packageFiles, []graph.NodeKind{graph.KindType, graph.KindInterface}) {
				if n != nil && n.Language == "go" && n.RepoPrefix == repoPrefix {
					addGoTypeToIndex(typesIdx, n)
				}
			}
		} else {
			// Third-party stores without the compound capability retain a bounded
			// two-pass fallback; there is still no per-file query loop.
			for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
				for n := range r.nodesByKindLang(kind, "go") {
					if n != nil && n.RepoPrefix == repoPrefix && filePathDir(n.FilePath) == dir {
						addGoTypeToIndex(typesIdx, n)
					}
				}
			}
		}
		if r.receiverTypeIdxByDir == nil {
			r.receiverTypeIdxByDir = make(map[string]map[pkgKey]string)
		}
		r.receiverTypeIdxByDir[cacheKey] = typesIdx
	}
	if len(typesIdx) == 0 {
		return
	}
	ids := make([]string, 0, len(fileNodes))
	for _, n := range fileNodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	out := r.graph.GetOutEdgesByNodeIDs(ids)
	var memberOf []*graph.Edge
	for _, id := range ids {
		for _, e := range out[id] {
			if e.Kind == graph.EdgeMemberOf {
				memberOf = append(memberOf, e)
			}
		}
	}
	r.rebindMemberOf(typesIdx, memberOf)
}

// addGoTypeToIndex records a Go type/interface node under (dir, name).
// Two distinct nodes with the same name in the same package directory
// shouldn't happen in valid Go; if they do, the entry is poisoned to ""
// so the rebind leaves the edge alone rather than pick an arbitrary
// winner.
func addGoTypeToIndex(idx map[pkgKey]string, n *graph.Node) {
	if n == nil || n.Name == "" || n.FilePath == "" {
		return
	}
	k := pkgKey{n.RepoPrefix, filePathDir(n.FilePath), n.Name}
	if existing, ok := idx[k]; ok && existing != n.ID {
		idx[k] = ""
		return
	}
	idx[k] = n.ID
}

// rebindMemberOf lifts each Go method's MemberOf edge from a phantom
// `<methodfile>::TypeName` target onto the canonical type node from
// typesIdx. Endpoints are batch-loaded in one GetNodesByIDs (a per-edge
// GetNode here is two query round-trips per method on a disk backend).
func (r *Resolver) rebindMemberOf(typesIdx map[pkgKey]string, memberOf []*graph.Edge) {
	if len(memberOf) == 0 {
		return
	}
	ids := make(map[string]struct{}, len(memberOf)*2)
	for _, e := range memberOf {
		if e.From != "" {
			ids[e.From] = struct{}{}
		}
		if e.To != "" {
			ids[e.To] = struct{}{}
		}
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	nodes := r.graph.GetNodesByIDs(idList)

	var batch []graph.EdgeReindex
	for _, e := range memberOf {
		method := nodes[e.From]
		if method == nil || method.Language != "go" || method.Kind != graph.KindMethod {
			continue
		}
		// Already resolves to a real type node — same-file methods
		// land here. Nothing to do.
		if n := nodes[e.To]; n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface) {
			continue
		}
		// Parse `<methodfile>::<typename>`. The split is on the LAST
		// `::` so paths embedded in the ID (none in Go, but stay
		// defensive) can't trip us up.
		i := strings.LastIndex(e.To, "::")
		if i <= 0 {
			continue
		}
		file := e.To[:i]
		typeName := e.To[i+2:]
		if file == "" || typeName == "" {
			continue
		}
		canonicalID, ok := typesIdx[pkgKey{method.RepoPrefix, filePathDir(file), typeName}]
		if !ok || canonicalID == "" || canonicalID == e.To {
			continue
		}
		oldTo := e.To
		e.To = canonicalID
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	r.persistAttributionReindexes(batch)
}
