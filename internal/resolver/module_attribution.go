package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// moduleAttributionWriteBatchSize bounds transient node and edge slices while
// allowing SQLite to amortize JSONB payload and transaction setup. It matches
// the node ingest chunk, the smaller of the two persisted row shapes.
const moduleAttributionWriteBatchSize = 4 * 1024

// attributeNonGoModuleImports runs as a serial post-pass at the
// end of ResolveAll. It walks every EdgeImports edge that ended up
// pointing at an `external::*` stub (i.e. resolveImport found no
// matching package in the graph and no go.mod-derived dep contract
// — typical for Python and Dart imports today), and for those whose
// caller file lives in an ecosystem we know how to attribute,
// rewrites the edge target onto a materialised KindModule node and
// emits a matching EdgeDependsOnModule.
//
// This mirrors the goanalysis externals pass for Go without
// requiring CGO type-checker output: text-based attribution maps
// each import URI to a top-level module ID per ecosystem
// (`module::pypi:<top>` for Python, `module::pub:<pkg>` for Dart;
// stdlib variants for the curated builtins). False negatives
// (modules left as `external::*`) are tolerated; false positives
// would corrupt the module graph and are guarded against by the
// per-ecosystem mapping refusing relative paths and ambiguous
// shapes.
//
// Idempotent: every (file, module) pair is short-circuited via a
// per-pass set so a second invocation in the same ResolveAll burst
// emits no duplicate EdgeDependsOnModule edges.
func (r *Resolver) attributeNonGoModuleImports() {
	// Python/Dart-only attribution (nonGoImportToModuleID handles exactly
	// those two ecosystems). Skip the EdgeImports scan when the graph has
	// neither language.
	if !r.graphHasLanguage("python") && !r.graphHasLanguage("dart") {
		return
	}
	fileLang := r.collectFileLanguages()
	type pendingEdge struct {
		edge     *graph.Edge
		oldTo    string
		moduleID string
	}
	var rewrites []pendingEdge
	moduleSeeds := map[string]moduleSeed{}
	dependsSeen := map[string]map[string]struct{}{} // fileID → set of moduleIDs

	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		if !strings.HasPrefix(e.To, "external::") {
			continue
		}
		// Scoped warm pass: an unchanged repo's external imports were already
		// attributed by a prior full pass, so only reconsider the changed repos.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		lang, ok := fileLang[e.From]
		if !ok {
			continue
		}
		importPath := strings.TrimPrefix(e.To, "external::")
		moduleID, mlang, ok := nonGoImportToModuleID(lang, importPath)
		if !ok {
			continue
		}
		rewrites = append(rewrites, pendingEdge{
			edge:     e,
			oldTo:    e.To,
			moduleID: moduleID,
		})
		moduleSeeds[moduleID] = moduleSeed{
			id:       moduleID,
			language: mlang,
			path:     importPath,
		}
	}

	if len(rewrites) == 0 {
		return
	}

	// Materialise module nodes first; later loops assume the
	// node exists when we add EdgeDependsOnModule. Batch the
	// presence check via GetNodesByIDs so disk backends do one
	// indexed SELECT IN (...) instead of one per-seed GetNode.
	seedIDs := make([]string, 0, len(moduleSeeds))
	for id := range moduleSeeds {
		seedIDs = append(seedIDs, id)
	}
	existing := r.graph.GetNodesByIDs(seedIDs)
	moduleBatchCapacity := len(moduleSeeds)
	if moduleBatchCapacity > moduleAttributionWriteBatchSize {
		moduleBatchCapacity = moduleAttributionWriteBatchSize
	}
	moduleBatch := make([]*graph.Node, 0, moduleBatchCapacity)
	for _, seed := range moduleSeeds {
		if _, ok := existing[seed.id]; ok {
			continue
		}
		moduleBatch = append(moduleBatch, buildNonGoModuleNode(seed))
		if len(moduleBatch) >= moduleAttributionWriteBatchSize {
			r.graph.AddBatch(moduleBatch, nil)
			moduleBatch = moduleBatch[:0]
		}
	}
	if len(moduleBatch) > 0 {
		r.graph.AddBatch(moduleBatch, nil)
	}

	// Pre-build a set of every (fileID, moduleID) pair the graph
	// already has an EdgeDependsOnModule edge for. The old code
	// called hasDependsOnModule per rewrite, which on a disk backend
	// fans out to N per-file GetOutEdges queries (50k+ on a
	// gortex-scale pass). One EdgesByKind scan is an indexed range
	// read on every backend, plus a Go-side map build that turns
	// the per-rewrite check into a constant-time lookup.
	existingDepends := make(map[string]map[string]struct{})
	for e := range graph.EdgesLightSeq(r.graph, graph.EdgeDependsOnModule) {
		// Only the rewrites' own callers (in scope) are queried below, so a
		// scoped pass needs the dedupe index for just those repos.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		set := existingDepends[e.From]
		if set == nil {
			set = make(map[string]struct{})
			existingDepends[e.From] = set
		}
		set[e.To] = struct{}{}
	}

	// Rewrite each EdgeImports target and collect the re-bucket
	// jobs into one batch so disk backends commit in chunks rather
	// than once per import rewrite.
	reindexBatch := make([]graph.EdgeReindex, 0, len(rewrites))
	dependsBatchCapacity := len(rewrites)
	if dependsBatchCapacity > moduleAttributionWriteBatchSize {
		dependsBatchCapacity = moduleAttributionWriteBatchSize
	}
	dependsBatch := make([]*graph.Edge, 0, dependsBatchCapacity)
	for _, p := range rewrites {
		p.edge.To = p.moduleID
		p.edge.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: p.edge, OldTo: p.oldTo})

		set, ok := dependsSeen[p.edge.From]
		if !ok {
			set = map[string]struct{}{}
			dependsSeen[p.edge.From] = set
		}
		if _, dup := set[p.moduleID]; dup {
			continue
		}
		set[p.moduleID] = struct{}{}
		// Avoid emitting a duplicate EdgeDependsOnModule when an
		// earlier pass already wired one (e.g. cold + warm
		// indexing of the same file). Constant-time map lookup
		// against the pre-built existingDepends index.
		if existing, ok := existingDepends[p.edge.From]; ok {
			if _, dup := existing[p.moduleID]; dup {
				continue
			}
		}
		dependsBatch = append(dependsBatch, &graph.Edge{
			From:            p.edge.From,
			To:              p.moduleID,
			Kind:            graph.EdgeDependsOnModule,
			FilePath:        p.edge.FilePath,
			Line:            p.edge.Line,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          graph.OriginASTResolved,
		})
		if len(dependsBatch) >= moduleAttributionWriteBatchSize {
			r.graph.AddBatch(nil, dependsBatch)
			dependsBatch = dependsBatch[:0]
		}
	}
	if len(dependsBatch) > 0 {
		r.graph.AddBatch(nil, dependsBatch)
	}
	if len(reindexBatch) > 0 {
		r.noteImportEdgeReindexes(reindexBatch)
		r.applyReindexEdges(reindexBatch)
	}
}

// collectFileLanguages walks KindFile nodes once and returns
// (file ID → language) for the per-edge dispatch above.
func (r *Resolver) collectFileLanguages() map[string]string {
	out := map[string]string{}
	for file := range graph.FileLanguageNodesSeq(r.graph) {
		out[file.ID] = file.Language
	}
	return out
}

// nonGoImportToModuleID maps a (language, importPath) pair to its
// canonical KindModule ID. The second return value is the module's
// own language tag (used at materialisation time so a stdlib module
// claims the same language as the consuming file).
//
// Returns ok=false for ecosystems we don't yet attribute (everything
// outside Python/Dart) and for shapes we can't disambiguate (Dart
// relative imports, Python relative imports). These flow through
// unchanged as `external::*` — a future pass can attribute them
// once we wire pyproject/pubspec parsing.
func nonGoImportToModuleID(language, importPath string) (string, string, bool) {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return "", "", false
	}
	switch language {
	case "python":
		return pythonImportToModuleID(importPath)
	case "dart":
		return dartImportToModuleID(importPath)
	}
	return "", "", false
}

func pythonImportToModuleID(path string) (string, string, bool) {
	if strings.HasPrefix(path, ".") {
		// Relative import — attributable only with knowledge of
		// the package layout, which we don't have here.
		return "", "", false
	}
	if strings.Contains(path, "/") {
		// File-path stem from a project-rooted relative import
		// (emitted by emitImportFromRelative). The relative-import
		// resolver pass owns these; landing them on a pypi module
		// would invent a phantom package named after the repo's
		// directory layout.
		return "", "", false
	}
	top := path
	if i := strings.Index(path, "."); i > 0 {
		top = path[:i]
	}
	if top == "" {
		return "", "", false
	}
	if isPythonStdlibTop(top) {
		return "module::python:stdlib::" + top, "python", true
	}
	return "module::pypi:" + top, "python", true
}

func dartImportToModuleID(path string) (string, string, bool) {
	switch {
	case strings.HasPrefix(path, "dart:"):
		name := strings.TrimPrefix(path, "dart:")
		if name == "" {
			return "", "", false
		}
		return "module::dart:stdlib::" + name, "dart", true
	case strings.HasPrefix(path, "package:"):
		rest := strings.TrimPrefix(path, "package:")
		pkg := rest
		if i := strings.Index(rest, "/"); i > 0 {
			pkg = rest[:i]
		}
		if pkg == "" {
			return "", "", false
		}
		return "module::pub:" + pkg, "dart", true
	}
	return "", "", false
}

// buildNonGoModuleNode shapes the KindModule for the attribution
// pass. The Meta block carries the original import path so
// downstream consumers can recover it without re-parsing the ID.
func buildNonGoModuleNode(seed moduleSeed) *graph.Node {
	ecosystem := "pypi"
	role := "external"
	switch {
	case strings.HasPrefix(seed.id, "module::python:stdlib"):
		ecosystem = "python:stdlib"
		role = "stdlib"
	case strings.HasPrefix(seed.id, "module::pypi:"):
		ecosystem = "pypi"
	case strings.HasPrefix(seed.id, "module::dart:stdlib"):
		ecosystem = "dart:stdlib"
		role = "stdlib"
	case strings.HasPrefix(seed.id, "module::pub:"):
		ecosystem = "pub"
	}
	return &graph.Node{
		ID:       seed.id,
		Kind:     graph.KindModule,
		Name:     seed.path,
		Language: seed.language,
		Meta: map[string]any{
			"ecosystem":   ecosystem,
			"role":        role,
			"import_path": seed.path,
		},
	}
}

// moduleSeed is exported only at the package scope for the
// pendingEdge / moduleSeeds maps above. Unique per moduleID; later
// edges with the same module reuse the seed.
type moduleSeed struct {
	id       string
	language string
	path     string
}

// pythonStdlibTops is a curated set of the most-imported Python
// stdlib top-level modules. This is the same trade-off
// `isExternalPyImport` makes in the parser: false negatives push a
// stdlib module into pypi:<name> (where it'll show up as a phantom
// external dependency in audits), false positives make a real pypi
// dependency look like stdlib. We pick the conservative direction —
// the list covers everything the typical app reaches into, and
// false negatives at most degrade the audit's separation of concerns.
var pythonStdlibTops = map[string]struct{}{
	"abc":             {},
	"argparse":        {},
	"array":           {},
	"ast":             {},
	"asyncio":         {},
	"base64":          {},
	"binascii":        {},
	"bisect":          {},
	"builtins":        {},
	"calendar":        {},
	"cmath":           {},
	"collections":     {},
	"concurrent":      {},
	"configparser":    {},
	"contextlib":      {},
	"contextvars":     {},
	"copy":            {},
	"csv":             {},
	"ctypes":          {},
	"dataclasses":     {},
	"datetime":        {},
	"decimal":         {},
	"difflib":         {},
	"dis":             {},
	"email":           {},
	"enum":            {},
	"errno":           {},
	"fnmatch":         {},
	"fractions":       {},
	"functools":       {},
	"gc":              {},
	"getopt":          {},
	"gettext":         {},
	"glob":            {},
	"gzip":            {},
	"hashlib":         {},
	"heapq":           {},
	"hmac":            {},
	"html":            {},
	"http":            {},
	"imaplib":         {},
	"importlib":       {},
	"inspect":         {},
	"io":              {},
	"ipaddress":       {},
	"itertools":       {},
	"json":            {},
	"keyword":         {},
	"linecache":       {},
	"locale":          {},
	"logging":         {},
	"math":            {},
	"mimetypes":       {},
	"multiprocessing": {},
	"numbers":         {},
	"operator":        {},
	"os":              {},
	"pathlib":         {},
	"pickle":          {},
	"platform":        {},
	"posixpath":       {},
	"pprint":          {},
	"queue":           {},
	"random":          {},
	"re":              {},
	"secrets":         {},
	"shutil":          {},
	"signal":          {},
	"smtplib":         {},
	"socket":          {},
	"sqlite3":         {},
	"ssl":             {},
	"stat":            {},
	"statistics":      {},
	"string":          {},
	"struct":          {},
	"subprocess":      {},
	"sys":             {},
	"sysconfig":       {},
	"tarfile":         {},
	"tempfile":        {},
	"textwrap":        {},
	"threading":       {},
	"time":            {},
	"timeit":          {},
	"tokenize":        {},
	"traceback":       {},
	"types":           {},
	"typing":          {},
	"unicodedata":     {},
	"unittest":        {},
	"urllib":          {},
	"uuid":            {},
	"warnings":        {},
	"weakref":         {},
	"xml":             {},
	"xmlrpc":          {},
	"zipfile":         {},
	"zlib":            {},
	"zoneinfo":        {},
}

func isPythonStdlibTop(name string) bool {
	_, ok := pythonStdlibTops[name]
	return ok
}
