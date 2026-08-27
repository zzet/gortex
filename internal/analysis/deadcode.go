package analysis

import (
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// DeadCodeEntry represents a symbol with zero incoming references that is not excluded.
type DeadCodeEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"start_line"`
}

// HotspotEntry represents a symbol with disproportionately high complexity metrics.
type HotspotEntry struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	FilePath           string `json:"file_path"`
	Line               int    `json:"start_line"`
	FanIn              int    `json:"fan_in"`
	FanOut             int    `json:"fan_out"`
	CommunityCrossings int    `json:"community_crossings"`
	// Betweenness is the node's betweenness-centrality score
	// normalized to 0-100 — how often it sits on a shortest path
	// between other symbols. A bottleneck the call graph routes
	// through scores high here even when its fan-in/out look modest.
	Betweenness     float64 `json:"betweenness"`
	ComplexityScore float64 `json:"complexity_score"`
}

// FindDeadCodeOptions controls filtering behavior for dead code analysis.
//
// Default behaviour ships only the high-signal kinds: function, method,
// type, interface. The opt-in flags below let callers pull in the
// lower-signal kinds (fields, variables, constants) that the graph can
// represent but can't reliably evaluate due to the absence of intra-
// function data-flow edges.
//
// Kinds that the dead-code analyzer never reports (regardless of flags):
// param, closure, generic_param, string, enum_member, module, column,
// table, config_key, flag, event, migration, fixture, todo, team,
// release, license, resource, kustomization, image, contract,
// file, package, import. These are either structural (file/package/
// import), extracted metadata (todo/team/release/license/fixture),
// infra (resource/kustomization/image/table/column/config_key/flag/
// event/migration), or function-shape (param/closure/generic_param)
// — none of them have a meaningful "is this code dead?" answer, and
// surfacing them drowns the real dead-function signal in noise.
type FindDeadCodeOptions struct {
	// IncludeVariables includes variable nodes in the results. Default false.
	// Variables are excluded by default because the graph does not track
	// intra-function data flow — local variables always appear "dead" even
	// though Go's compiler enforces their usage. Package-level variables
	// cannot be reliably distinguished from locals in the current graph model.
	IncludeVariables bool

	// IncludeFields includes struct/class field nodes in the results.
	// Default false. Same graph limitation as variables: a field read
	// inside a function body is captured as EdgeReads on the field node,
	// but the analyzer can't tell a real "field never read" from "graph
	// doesn't see the read because the resolver couldn't pick a
	// candidate." Fields are opt-in for callers that have manually
	// audited their resolver coverage.
	IncludeFields bool

	// IncludeConstants includes constant nodes (Go const, language
	// constants). Default false — same rationale as variables; the
	// graph can't distinguish "unused constant" from "constant read
	// inside a function body the resolver couldn't trace."
	IncludeConstants bool

	// IncludeCgoExports includes functions annotated with //export pragma.
	// Default false — CGo-exported functions are called from C, not Go,
	// so they have no incoming Go-level edges.
	// Requires the Go extractor to populate Node.Meta["cgo_export"] = true.
	IncludeCgoExports bool

	// IncludeLinknameTargets includes functions annotated with //go:linkname.
	// Default false — linkname targets are linked by name from another package
	// and have no visible call edges in the graph.
	// Requires the Go extractor to populate Node.Meta["go_linkname"] = true.
	IncludeLinknameTargets bool

	// SkipCrossRepoNodes excludes nodes whose RepoPrefix is non-empty.
	// Useful when cross-repo linking is incomplete — functions in secondary
	// repos may lack incoming edges from the primary repo.
	SkipCrossRepoNodes bool
}

// neverDeadCodeKinds enumerates node kinds the dead-code analyzer must
// never report — regardless of opt-in flags — because the question
// "is this code dead?" has no meaningful answer for them. Includes
// structural nodes (file/package/import), function-shape nodes
// (param/closure/generic_param), extracted metadata (todo/team/
// release/license), infra surface (table/column/migration/config_key/
// flag/event/fixture/resource/kustomization/image), package metadata
// (module), and value-extraction nodes (string/enum_member).
// Surfacing any of these drowns real dead-function signal in noise.
var neverDeadCodeKinds = map[graph.NodeKind]bool{
	graph.KindFile:          true,
	graph.KindPackage:       true,
	graph.KindImport:        true,
	graph.KindParam:         true,
	graph.KindClosure:       true,
	graph.KindGenericParam:  true,
	graph.KindString:        true,
	graph.KindEnumMember:    true,
	graph.KindModule:        true,
	graph.KindColumn:        true,
	graph.KindTable:         true,
	graph.KindConfigKey:     true,
	graph.KindFlag:          true,
	graph.KindEvent:         true,
	graph.KindMigration:     true,
	graph.KindFixture:       true,
	graph.KindTodo:          true,
	graph.KindTeam:          true,
	graph.KindRelease:       true,
	graph.KindLicense:       true,
	graph.KindResource:      true,
	graph.KindKustomization: true,
	graph.KindImage:         true,
	graph.KindContract:      true,
}

// incomingUsageKinds returns the set of incoming edge kinds that count
// as "this symbol is used" for the given node kind. The per-kind list
// matters because different shapes are exercised by different edges:
// a function is used via Calls or References, a type via References /
// Instantiates / MemberOf, a field via Reads or Writes.
//
// Before this split, the analyzer used a single global allowlist
// {Calls, References, MemberOf, Implements, Instantiates} — which
// meant struct fields and variables always appeared dead because
// the resolver records their use as EdgeReads, which wasn't in the
// allowlist. The result was 5,390 fields flagged across the gortex
// workspace, drowning out the ~300 real function-level signals.
func incomingUsageKinds(k graph.NodeKind) []graph.EdgeKind {
	switch k {
	case graph.KindFunction:
		// Calls: invoked as `foo()`. References: passed as a value
		// (`RunE: runClean`). MemberOf: appears in a method-table /
		// receiver mapping. Instantiates: NewFoo() pattern when the
		// receiver type is the function type itself.
		return []graph.EdgeKind{
			graph.EdgeCalls, graph.EdgeReferences,
			graph.EdgeMemberOf, graph.EdgeInstantiates,
		}
	case graph.KindMethod:
		// Same as functions plus: Implements (the method satisfies
		// an interface contract — required by the interface).
		return []graph.EdgeKind{
			graph.EdgeCalls, graph.EdgeReferences,
			graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeInstantiates,
		}
	case graph.KindType, graph.KindInterface:
		// Types are exercised by References (generic value-position
		// use), Instantiates (struct literal), MemberOf (methods/
		// fields hanging off the type), Implements (a type satisfies
		// this interface), Extends (subclass), Composes (embeds),
		// TypedAs (variable / param / field declared as this type),
		// Returns (function returns this type — the canonical pattern
		// for cross-package type re-export via `type X = pkg.X`).
		return []graph.EdgeKind{
			graph.EdgeReferences, graph.EdgeInstantiates,
			graph.EdgeMemberOf, graph.EdgeImplements,
			graph.EdgeExtends, graph.EdgeComposes,
			graph.EdgeTypedAs, graph.EdgeReturns,
		}
	case graph.KindField:
		// Fields are accessed via Reads/Writes (the dominant pattern)
		// and References (when a struct literal positionally fills the
		// field). MemberOf isn't a "use" — it just attaches the field
		// to its owner type.
		return []graph.EdgeKind{
			graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences,
		}
	case graph.KindVariable, graph.KindConstant:
		// Same as fields: Reads/Writes dominate; References covers
		// the value-as-arg case.
		return []graph.EdgeKind{
			graph.EdgeReads, graph.EdgeWrites, graph.EdgeReferences,
		}
	}
	// Fallback for any kind not specifically modelled: use the legacy
	// global allowlist so a future KindWidget doesn't silently
	// collapse to "always dead."
	return []graph.EdgeKind{
		graph.EdgeCalls, graph.EdgeReferences,
		graph.EdgeMemberOf, graph.EdgeImplements, graph.EdgeInstantiates,
	}
}

// isEntryPointNode reports whether n was stamped as a framework entry
// point (Alembic / Next.js / ASP.NET) by the entrypoints detector.
func isEntryPointNode(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["entry_point"].(bool)
	return v
}

// candidateNodeKinds enumerates the node kinds FindDeadCode is willing
// to flag (modulo the opt-in switches for fields / variables /
// constants). Used both for the per-kind allowlist handed to the
// DeadCodeCandidator capability and as the source of truth for the
// Go-fallback loop. Kept in lockstep with neverDeadCodeKinds: a kind
// MUST appear in exactly one of the two lists.
var candidateNodeKinds = []graph.NodeKind{
	graph.KindFunction,
	graph.KindMethod,
	graph.KindType,
	graph.KindInterface,
	graph.KindField,
	graph.KindVariable,
	graph.KindConstant,
}

// FindDeadCode returns all symbols with zero incoming calls or references,
// excluding entry points, test functions, exported symbols, and user-excluded patterns.
// By default, variables are excluded (see FindDeadCodeOptions for rationale).
func FindDeadCode(g graph.Store, processes *ProcessResult, excludePatterns []string, opts ...FindDeadCodeOptions) []DeadCodeEntry {
	var opt FindDeadCodeOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Build set of interface-required method names per type.
	// If a type implements an interface, all methods that the interface
	// requires are alive even if never called directly (they satisfy the
	// contract).  We index: typeID → set of required method names.
	// Backends that implement graph.IfaceImplementsScanner serve this
	// from one join; the fallback walks NodesByKind + EdgesByKind
	// just like before.
	ifaceRequiredMethods := buildIfaceRequiredMethods(g)

	// Pick the candidate-set source. When the backend implements
	// DeadCodeCandidator, the "no incoming usage edge" filter runs
	// inside the store and only the surviving ~hundreds of true
	// candidates are materialized — see graph.DeadCodeCandidator's
	// doc-comment for the 1.3M-row-vs-hundreds rationale. Otherwise
	// the legacy AllNodes + GetInEdgesByNodeIDs fallback runs,
	// identical to the pre-capability path.
	candidates, incomingByID := collectDeadCodeCandidates(g, opt)

	// Build set of entry point node IDs from processes
	entryPoints := make(map[string]bool)
	if processes != nil {
		for _, proc := range processes.Processes {
			entryPoints[proc.EntryPoint] = true
			// Also consider all nodes that participate in any process
			for _, step := range proc.Steps {
				entryPoints[step.ID] = true
			}
		}
	}

	// Files holding a framework entry point (Alembic migrations,
	// Next.js pages, ASP.NET host files) — every symbol inside is
	// reachable from a runtime, not application-dead. Computed via
	// NodesByKind(KindFile) so on disk backends we don't have to
	// materialise AllNodes() just to find the entry-point files.
	entryPointFiles := make(map[string]bool)
	for n := range g.NodesByKind(graph.KindFile) {
		if n != nil && isEntryPointNode(n) {
			entryPointFiles[n.FilePath] = true
		}
	}

	var result []DeadCodeEntry
	var resultNodes []*graph.Node
	for _, n := range candidates {
		// Skip kinds the analyzer never reports — structural,
		// extracted metadata, infra, function-shape, and value-only
		// nodes. See neverDeadCodeKinds for the full list and why.
		// (The server-side candidator only ships nodes whose kind is
		// in candidateNodeKinds, but the Go fallback path scans
		// AllNodes so we keep the explicit gate.)
		if neverDeadCodeKinds[n.Kind] {
			continue
		}

		// Synthetic external-symbol / stub nodes are NOT first-party
		// code. The external-call attribution pass materialises imported
		// stdlib / dependency / external symbols as KindFunction /
		// KindMethod nodes (IDs like "stdlib::fmt::Sprintf",
		// "dep::<mod>::Sym", "external::<path>::Sym") stamped with
		// Meta["external"]=true; the stub layer mints "<kind>::*" IDs for
		// stdlib/external_call/builtin/module targets. By construction
		// these carry only inbound import / member_of links — never a
		// call/reference usage edge — so they ALWAYS look dead. Reporting
		// them buried the real first-party signal under thousands of
		// stdlib/dep entries. Drop them unconditionally.
		if graph.IsStub(n.ID) {
			continue
		}
		if ext, _ := n.Meta["external"].(bool); ext {
			continue
		}
		// Code-generated symbols (RTK Query hooks, protobuf accessors, …) have
		// no hand-written call site by design, so they always look dead — never
		// report them.
		if gen, _ := n.Meta["generated"].(bool); gen {
			continue
		}

		// Framework entry points, and everything in an entry-point
		// file, are invoked by a runtime — never dead.
		if isEntryPointNode(n) || entryPointFiles[n.FilePath] {
			continue
		}

		// Skip variables/fields/constants unless explicitly opted in.
		// All three are subject to the same graph limitation: the
		// resolver can't always pick a candidate for intra-function
		// reads, so they look dead even when the code reads them
		// every line. We err toward false-negative (miss a real dead
		// variable) over false-positive (flag every struct field
		// in the repo) — the latter destroys the signal of the
		// function/method results we DO trust.
		if n.Kind == graph.KindVariable && !opt.IncludeVariables {
			continue
		}
		if n.Kind == graph.KindField && !opt.IncludeFields {
			continue
		}
		if n.Kind == graph.KindConstant && !opt.IncludeConstants {
			continue
		}

		// Skip implicitly-called constructors/initializers.
		// Go: init() is called by the runtime.
		// Python: every __dunder__ is invoked by the runtime or a builtin
		// rather than through a written call site -- __init__ on
		// instantiation, __repr__ by repr()/str(), __enter__/__exit__ by
		// `with`, __len__ by len(), __iter__ by `for`. They therefore always
		// look dead, the same way generated symbols and framework entry
		// points do above.
		if n.Name == "init" && n.Language == "go" {
			continue
		}
		if n.Language == "python" && isPythonDunder(n.Name) {
			continue
		}

		// Skip Go main() — it's the binary entry point, called by the runtime.
		// Constrained to KindFunction so (*Foo).main() methods are still checked.
		if n.Name == "main" && n.Language == "go" && n.Kind == graph.KindFunction {
			continue
		}

		// Skip vendored/generated C header functions — they're used via C
		// macros and linker symbols, invisible to the graph.
		if isVendoredOrGenerated(n.FilePath) {
			continue
		}

		// Skip functions in Go files with build constraints — only one
		// variant is active per build, so the others always look "dead".
		if n.Language == "go" && hasBuildConstraint(n.FilePath) {
			continue
		}

		// Re-check the per-kind incoming-edge allowlist when we still
		// have the in-edge map from the Go fallback path. The
		// server-side DeadCodeCandidator has already applied the
		// equivalent filter, so incomingByID is nil for that path and
		// the count check short-circuits to 0 (matching the
		// candidator's contract).
		incomingCount := 0
		if incomingByID != nil {
			allowed := incomingUsageKinds(n.Kind)
			inEdges := incomingByID[n.ID]
			for _, e := range inEdges {
				if slices.Contains(allowed, e.Kind) {
					incomingCount++
				}
			}
		}

		if incomingCount > 0 {
			continue
		}

		// For methods with zero incoming edges, check if they exist to satisfy
		// an interface contract.  Look up the receiver type via member_of edges
		// and check if any implemented interface requires this method name.
		if n.Kind == graph.KindMethod {
			outEdges := g.GetOutEdges(n.ID)
			for _, e := range outEdges {
				if e.Kind == graph.EdgeMemberOf {
					if required, ok := ifaceRequiredMethods[e.To]; ok {
						if required[n.Name] {
							incomingCount++ // treat as alive
							break
						}
					}
				}
			}
			if incomingCount > 0 {
				continue
			}

			// Java: a method tagged @Override implements or overrides a
			// supertype member and is reachable through that contract even
			// with no direct caller in the indexed graph. Public overrides
			// are already excluded by visibility; this rescues protected /
			// package-private overrides from a false positive.
			if n.Language == "java" {
				overridden := false
				for _, e := range outEdges {
					if e.Kind == graph.EdgeAnnotated && e.To == javaOverrideAnnoID {
						overridden = true
						break
					}
				}
				if overridden {
					continue
				}
			}

			// Fallback: well-known standard-library interface methods.
			// If the implements edge wasn't inferred, methods like ServeHTTP,
			// MarshalJSON, String, etc. are still almost certainly alive.
			if isWellKnownInterfaceMethod(n.Name, n.Language) {
				continue
			}
		}

		// Skip CGo-exported functions (called from C, no Go-level callers).
		if n.Language == "go" && !opt.IncludeCgoExports {
			if cgoExport, ok := n.Meta["cgo_export"].(bool); ok && cgoExport {
				continue
			}
		}

		// Skip go:linkname targets (linked by name from another package).
		if n.Language == "go" && !opt.IncludeLinknameTargets {
			if linkname, ok := n.Meta["go_linkname"].(bool); ok && linkname {
				continue
			}
		}

		// Skip nodes from secondary repos when cross-repo linking is incomplete.
		if opt.SkipCrossRepoNodes && n.RepoPrefix != "" {
			continue
		}

		// Check exclusions
		if entryPoints[n.ID] {
			continue
		}
		if isTestFilePath(n.FilePath) {
			continue
		}
		if isExportedNode(n) && !isPackagePrivateByConvention(n.FilePath, n.Language) {
			continue
		}
		if matchesExcludePattern(n.FilePath, n.ID, excludePatterns) {
			continue
		}

		result = append(result, DeadCodeEntry{
			ID:       n.ID,
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
		})
		resultNodes = append(resultNodes, n)
	}

	// Resolve unresolved same-name evidence in one batched store query. These
	// edges mean resolution coverage is incomplete, so zero concrete incoming
	// edges is not proof that a same-named callable is dead.
	placeholderSeen := make(map[string]struct{})
	placeholderIDs := make([]string, 0, len(resultNodes)*4)
	for _, n := range resultNodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		for _, id := range graph.UnresolvedNameCandidateIDs(n) {
			if id == "" {
				continue
			}
			if _, seen := placeholderSeen[id]; seen {
				continue
			}
			placeholderSeen[id] = struct{}{}
			placeholderIDs = append(placeholderIDs, id)
		}
	}

	unresolvedInDegree := make(map[string]int)
	if counter, ok := g.(graph.InDegreeForNodes); ok {
		unresolvedInDegree = counter.InDegreeForNodes(placeholderIDs)
	} else {
		for _, id := range placeholderIDs {
			if count := len(g.GetInEdges(id)); count > 0 {
				unresolvedInDegree[id] = count
			}
		}
	}

	filtered := result[:0]
	for i, entry := range result {
		n := resultNodes[i]
		incomplete := false
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			for _, id := range graph.UnresolvedNameCandidateIDs(n) {
				if unresolvedInDegree[id] > 0 {
					incomplete = true
					break
				}
			}
		}
		if !incomplete {
			filtered = append(filtered, entry)
		}
	}
	result = filtered

	// Sort by file path then line for deterministic output
	sort.Slice(result, func(i, j int) bool {
		if result[i].FilePath != result[j].FilePath {
			return result[i].FilePath < result[j].FilePath
		}
		return result[i].Line < result[j].Line
	})

	return result
}

// collectDeadCodeCandidates is the candidate-set splitter for
// FindDeadCode. When the backend implements DeadCodeCandidator the
// WHERE-NOT-EXISTS filter runs server-side and we never materialise
// the in-edge map (returned nil). Otherwise we fall back to today's
// AllNodes + batched-GetInEdgesByNodeIDs path, identical pre-Part-2
// behaviour. The post-filter loop in FindDeadCode handles both shapes
// uniformly — incomingByID==nil means "filter already applied".
func collectDeadCodeCandidates(g graph.Store, opt FindDeadCodeOptions) (candidates []*graph.Node, incomingByID map[string][]*graph.Edge) {
	if dc, ok := g.(graph.DeadCodeCandidator); ok {
		kinds := candidateNodeKinds[:0:0]
		for _, k := range candidateNodeKinds {
			// Honour the IncludeFields / IncludeVariables / IncludeConstants
			// opt-in switches at the candidate-source: kinds the caller
			// explicitly excluded never need to cross cgo. The post-
			// filter loop still re-checks these for the fallback path
			// (which sees every kind) so the contract holds either way.
			switch k {
			case graph.KindField:
				if !opt.IncludeFields {
					continue
				}
			case graph.KindVariable:
				if !opt.IncludeVariables {
					continue
				}
			case graph.KindConstant:
				if !opt.IncludeConstants {
					continue
				}
			}
			kinds = append(kinds, k)
		}
		allowed := make(map[graph.NodeKind][]graph.EdgeKind, len(kinds))
		for _, k := range kinds {
			allowed[k] = incomingUsageKinds(k)
		}
		return dc.DeadCodeCandidates(kinds, allowed), nil
	}

	// Fallback: pull every node and the batched in-edge map up front.
	// Same shape as before the DeadCodeCandidator capability landed.
	nodes := g.AllNodes()
	nodeIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	return nodes, g.GetInEdgesByNodeIDs(nodeIDs)
}

// buildIfaceRequiredMethods returns a map from type ID → set of method names
// that the type must implement to satisfy its interfaces.  This is computed by:
//  1. Collecting all interfaces with their required method names (from Meta["methods"]).
//  2. Collecting all EdgeImplements edges (type → interface).
//  3. For each type that implements an interface, merging all required method names.
//
// On backends that implement graph.IfaceImplementsScanner this is a
// single join; otherwise the fallback iterates
// NodesByKind(KindInterface) + EdgesByKind(EdgeImplements). Both paths
// produce the same map.
func buildIfaceRequiredMethods(g graph.Store) map[string]map[string]bool {
	if scanner, ok := g.(graph.IfaceImplementsScanner); ok {
		return buildIfaceRequiredMethodsFromRows(scanner.IfaceImplementsRows())
	}

	// Fallback: walk interfaces + EdgeImplements edges Go-side. Uses
	// NodesByKind(KindInterface) so disk backends still issue one
	// scan per kind instead of pulling AllNodes.
	ifaceMethods := make(map[string]map[string]bool)
	for n := range g.NodesByKind(graph.KindInterface) {
		if n == nil || n.Meta == nil {
			continue
		}
		raw, ok := n.Meta["methods"]
		if !ok {
			continue
		}
		methods := decodeMethodNames(raw)
		if len(methods) > 0 {
			ifaceMethods[n.ID] = methods
		}
	}

	if len(ifaceMethods) == 0 {
		return nil
	}

	result := make(map[string]map[string]bool)
	for e := range g.EdgesByKind(graph.EdgeImplements) {
		// EdgeImplements: From=type, To=interface
		iface, ok := ifaceMethods[e.To]
		if !ok {
			continue
		}
		if result[e.From] == nil {
			result[e.From] = make(map[string]bool)
		}
		for m := range iface {
			result[e.From][m] = true
		}
	}

	return result
}

// buildIfaceRequiredMethodsFromRows reduces the server-side
// IfaceImplementsScanner row set to the typeID → method-name-set
// shape the rest of FindDeadCode consumes. Same join logic as the
// fallback path, just folded over rows that already carry the
// interface Meta.
func buildIfaceRequiredMethodsFromRows(rows []graph.IfaceImplementsRow) map[string]map[string]bool {
	if len(rows) == 0 {
		return nil
	}
	// Cache decoded method-name sets per interface so repeated rows
	// (one per implementing type) don't re-decode the same Meta.
	ifaceMethods := make(map[string]map[string]bool)
	result := make(map[string]map[string]bool)
	for _, r := range rows {
		methods, ok := ifaceMethods[r.IfaceID]
		if !ok {
			raw, hasRaw := r.IfaceMeta["methods"]
			if !hasRaw {
				ifaceMethods[r.IfaceID] = nil
				continue
			}
			methods = decodeMethodNames(raw)
			ifaceMethods[r.IfaceID] = methods
		}
		if len(methods) == 0 {
			continue
		}
		if result[r.TypeID] == nil {
			result[r.TypeID] = make(map[string]bool)
		}
		for m := range methods {
			result[r.TypeID][m] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// decodeMethodNames normalises a Node.Meta["methods"] value into a
// set of method names. Accepts []string (in-memory backend) and
// []any (decoded payload from the disk backend); anything else is
// treated as "no methods declared".
func decodeMethodNames(raw any) map[string]bool {
	methods := make(map[string]bool)
	switch v := raw.(type) {
	case []string:
		for _, m := range v {
			methods[m] = true
		}
	case []any:
		for _, m := range v {
			if s, ok := m.(string); ok {
				methods[s] = true
			}
		}
	}
	return methods
}

// hotspotBetweennessWeight scales the betweenness component of a
// hotspot's raw score. Betweenness arrives normalized to 0-100 (same
// range as the fan-in/out/crossing terms after their own
// normalization is implicit), so a weight of 0.4 lets a pure
// bottleneck — a symbol every call path routes through — register as
// a hotspot without overpowering the fan-in/out signals that still
// dominate the ranking.
const hotspotBetweennessWeight = 0.4

// FindHotspots returns symbols whose ComplexityScore exceeds the given threshold.
// ComplexityScore = (fan_in * 2) + (fan_out * 1.5) + (community_crossings * 3) +
// (betweenness * hotspotBetweennessWeight), normalized to 0-100. Betweenness is a
// centrality component — how often the symbol lies on a shortest path between
// other symbols — that augments the fan-in/out signals rather than replacing them.
// If threshold <= 0, the default threshold is mean + 2*stddev.
func FindHotspots(g graph.Store, communities *CommunityResult, threshold float64) []HotspotEntry {
	// Pull only function/method node IDs — the hotspots ranking is
	// callable-only, and the scoring math doesn't touch any column
	// beyond the id. NodeIDsByKinds returns the projection from a
	// single query (one id per row instead of the ~10
	// columns NodesByKinds would ship). The full *Node rows are
	// fetched in one batched GetNodesByIDs call AFTER the threshold
	// filter, so a typical run materialises ~100 survivors rather
	// than the whole ~4k function/method bucket.
	hotspotKinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}
	var candidateIDs []string
	if scan, ok := g.(graph.NodeIDsByKinds); ok {
		candidateIDs = scan.NodeIDsByKinds(hotspotKinds)
	} else if scan, ok := g.(graph.NodesByKindsScanner); ok {
		ns := scan.NodesByKinds(hotspotKinds)
		candidateIDs = make([]string, 0, len(ns))
		for _, n := range ns {
			candidateIDs = append(candidateIDs, n.ID)
		}
	} else {
		all := g.AllNodes()
		candidateIDs = make([]string, 0, len(all))
		for _, n := range all {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				candidateIDs = append(candidateIDs, n.ID)
			}
		}
	}

	// Build lookup maps for community membership
	nodeToComm := make(map[string]string)
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}

	// Restrict the fan-count pass to the kinds hotspots cares about
	// (function + method). NodeFanAggregator expects the candidate id
	// list -- it never returns rows for ids the caller didn't ask
	// for, so the cgo payload stays bounded by the candidate count
	// rather than the whole graph.
	fanIn, fanOut := CollectFanCounts(g, candidateIDs,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		[]graph.EdgeKind{graph.EdgeCalls},
	)

	// Community crossings per node: outgoing edges (Calls or
	// References) whose target sits in a different community than
	// the source. CommunityCrossingsByKind ships only the (from, to)
	// projection from a single IN-list join — the disk path stops
	// re-materialising the full edge row per kind. Backends that
	// don't implement the capability fall back to the per-kind
	// EdgesByKind walk that mirrors the in-memory reference.
	crossingKinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	var crossings map[string]int
	if cc, ok := g.(graph.CommunityCrossingsByKind); ok {
		crossings = cc.CommunityCrossingsByKind(crossingKinds, nodeToComm)
		if crossings == nil {
			// A nil map is the capability's valid zero-row result. Capability
			// presence, not map allocation, determines whether the backend can
			// aggregate crossings without hydrating full edges.
			crossings = make(map[string]int)
		}
	} else {
		crossings = make(map[string]int)
		countCrossings := func(kind graph.EdgeKind) {
			for e := range g.EdgesByKind(kind) {
				if e == nil {
					continue
				}
				fromComm := nodeToComm[e.From]
				toComm := nodeToComm[e.To]
				if fromComm != "" && toComm != "" && fromComm != toComm {
					crossings[e.From]++
				}
			}
		}
		for _, k := range crossingKinds {
			countCrossings(k)
		}
	}

	// Betweenness centrality — exact on small graphs, sampled on
	// large ones. Normalized to 0-100 against the graph's own max so
	// it sits on the same scale as the other score terms.
	bc := ComputeBetweenness(g)
	betweenness := make(map[string]float64, len(bc.Scores))
	if bc.Max > 0 {
		for id, v := range bc.Scores {
			betweenness[id] = (v / bc.Max) * 100.0
		}
	}

	// Compute raw scores for function/method nodes only. Keyed by id
	// so the full *Node fetch is deferred until after the threshold
	// filter — on a ~4k candidate set the surviving share is the top
	// few percent, so this materialises ~100 nodes instead of the
	// whole bucket.
	type rawEntry struct {
		id          string
		fanIn       int
		fanOut      int
		crossing    int
		betweenness float64
		rawScore    float64
	}

	entries := make([]rawEntry, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		fi := fanIn[id]
		fo := fanOut[id]
		cc := crossings[id]
		bw := betweenness[id]
		raw := float64(fi)*2.0 + float64(fo)*1.5 + float64(cc)*3.0 + bw*hotspotBetweennessWeight

		entries = append(entries, rawEntry{
			id:          id,
			fanIn:       fi,
			fanOut:      fo,
			crossing:    cc,
			betweenness: bw,
			rawScore:    raw,
		})
	}

	if len(entries) == 0 {
		return nil
	}

	// Find max raw score for normalization
	maxRaw := 0.0
	for _, e := range entries {
		if e.rawScore > maxRaw {
			maxRaw = e.rawScore
		}
	}

	// Normalize to 0-100
	normalized := make([]float64, len(entries))
	for i, e := range entries {
		if maxRaw > 0 {
			normalized[i] = (e.rawScore / maxRaw) * 100.0
		}
	}

	// Compute default threshold if not specified: mean + 2*stddev
	if threshold <= 0 {
		var sum float64
		for _, s := range normalized {
			sum += s
		}
		mean := sum / float64(len(normalized))

		var variance float64
		for _, s := range normalized {
			diff := s - mean
			variance += diff * diff
		}
		variance /= float64(len(normalized))
		stddev := math.Sqrt(variance)

		threshold = mean + 2.0*stddev
	}

	// Filter by threshold first to identify the surviving id set, so
	// the full *Node materialisation is bounded by the result size,
	// not the candidate count.
	type survivor struct {
		entryIdx int
		score    float64
	}
	survivors := make([]survivor, 0, len(entries))
	for i := range entries {
		score := math.Round(normalized[i]*100) / 100 // round to 2 decimal places
		if score < threshold {
			continue
		}
		survivors = append(survivors, survivor{entryIdx: i, score: score})
	}
	if len(survivors) == 0 {
		return nil
	}

	survivorIDs := make([]string, 0, len(survivors))
	for _, s := range survivors {
		survivorIDs = append(survivorIDs, entries[s.entryIdx].id)
	}
	nodesByID := g.GetNodesByIDs(survivorIDs)

	result := make([]HotspotEntry, 0, len(survivors))
	for _, s := range survivors {
		e := entries[s.entryIdx]
		n := nodesByID[e.id]
		if n == nil {
			continue
		}
		result = append(result, HotspotEntry{
			ID:                 n.ID,
			Name:               n.Name,
			Kind:               string(n.Kind),
			FilePath:           n.FilePath,
			Line:               n.StartLine,
			FanIn:              e.fanIn,
			FanOut:             e.fanOut,
			CommunityCrossings: e.crossing,
			Betweenness:        math.Round(e.betweenness*100) / 100,
			ComplexityScore:    s.score,
		})
	}

	// Sort by ComplexityScore descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].ComplexityScore > result[j].ComplexityScore
	})

	return result
}

// isTestFilePath checks if a file path indicates a test file.
func isTestFilePath(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(base, "_test.") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(path, "__tests__/")
}

// isPackagePrivateByConvention reports whether a file lives inside a
// directory the language's tooling treats as package-private regardless of
// individual symbol capitalisation. The dead-code analyzer uses this to
// override the "skip all exported symbols" rule: a function inside
// `gortex/internal/parser/tsitter/` named `Test` is *visible only to other
// gortex packages*, so if no caller exists in the indexed graph it really
// is dead — there's nowhere else it could be called from.
//
// Currently handles Go's `/internal/` convention (compiler-enforced since
// Go 1.4). Add other languages as their tooling acquires similar
// hard-bounded visibility rules.
func isPackagePrivateByConvention(filePath, lang string) bool {
	if lang != "go" {
		return false
	}
	// Match the path component "internal" anywhere in the path — Go's rule
	// is that anything inside an `internal/` directory is only importable
	// from its enclosing tree.
	return strings.Contains(filePath, "/internal/") ||
		strings.HasPrefix(filePath, "internal/")
}

// keywordVisibilityLangs are languages whose access level is an explicit
// modifier keyword (public/private/protected/...) that the extractor
// records in Meta["visibility"], rather than a naming convention. For
// these the name-based isExportedSymbol gives the wrong answer — every
// Java identifier is non-underscore, so the underscore heuristic marks
// them ALL exported and dead-code analysis skips the whole language.
// Extend as other keyword-visibility extractors are verified to stamp
// Meta["visibility"] with the same value set.
var keywordVisibilityLangs = map[string]bool{
	"java": true,
}

// javaOverrideAnnoID is the synthetic node ID the Java extractor emits
// for @Override (mirrors languages.AnnotationNodeID("java", "Override"),
// duplicated here to avoid an analysis→parser import).
const javaOverrideAnnoID = "annotation::java::Override"

// isExportedNode reports whether n is part of the public API surface and
// therefore not a dead-code candidate even with zero callers. For
// keyword-visibility languages it trusts the recorded modifier; for
// everything else it falls back to the name-based isExportedSymbol.
func isExportedNode(n *graph.Node) bool {
	if keywordVisibilityLangs[n.Language] {
		if v, ok := n.Meta["visibility"].(string); ok && v != "" {
			// public / protected are reachable from outside the indexed
			// graph (other packages, subclasses); private / package-private
			// are fully visible to the indexed call graph, so an unused one
			// is genuinely dead and worth reporting.
			return v == "public" || v == "protected"
		}
	}
	return isExportedSymbol(n.Name, n.Language)
}

// isExportedSymbol checks if a symbol name is exported (public API).
func isExportedSymbol(name, lang string) bool {
	if lang == "go" {
		if len(name) == 0 {
			return false
		}
		return unicode.IsUpper(rune(name[0]))
	}
	// For other languages, assume exported if not starting with underscore
	return len(name) > 0 && !strings.HasPrefix(name, "_")
}

// goWellKnownMethods contains method names that satisfy standard-library or
// widely-used Go interfaces.  When an implements edge wasn't inferred, a method
// with one of these names is almost certainly alive via implicit interface
// satisfaction rather than truly dead.
var goWellKnownMethods = map[string]bool{
	// io interfaces
	"Read": true, "Write": true, "Close": true, "Flush": true,
	"Seek": true, "ReadAt": true, "WriteAt": true, "ReadFrom": true,
	"WriteTo": true, "ReadByte": true, "UnreadByte": true,
	"ReadRune": true, "UnreadRune": true, "WriteByte": true,
	"WriteString": true,
	// net/http
	"ServeHTTP": true, "RoundTrip": true,
	// encoding
	"MarshalJSON": true, "UnmarshalJSON": true,
	"MarshalXML": true, "UnmarshalXML": true,
	"MarshalText": true, "UnmarshalText": true,
	"MarshalBinary": true, "UnmarshalBinary": true,
	"MarshalYAML": true, "UnmarshalYAML": true,
	// fmt
	"String": true, "Error": true, "Format": true, "GoString": true,
	// sort
	"Len": true, "Less": true, "Swap": true,
	// sql
	"Scan": true, "Value": true,
	// hash
	"Sum": true, "Reset": true, "BlockSize": true,
	// driver
	"Open": true, "Exec": true, "Query": true, "Begin": true,
	"Prepare": true,
	// proto/gRPC
	"mustEmbedUnimplemented": true, "ProtoMessage": true,
	"ProtoReflect": true,
}

// isWellKnownInterfaceMethod returns true if the method name matches a
// standard-library or widely-used interface method in the given language.
func isWellKnownInterfaceMethod(name, lang string) bool {
	if lang != "go" {
		return false
	}
	return goWellKnownMethods[name]
}

// isPythonDunder reports whether name is a __dunder__ special method. Python
// invokes these implicitly -- via an operator, a builtin, or a statement -- so
// they carry no hand-written call site and always look dead to the graph.
func isPythonDunder(name string) bool {
	return len(name) > 4 &&
		strings.HasPrefix(name, "__") &&
		strings.HasSuffix(name, "__")
}

// isVendoredOrGenerated checks if a file is vendored or generated code that
// should be excluded from dead code analysis.
func isVendoredOrGenerated(path string) bool {
	if strings.Contains(path, "tree_sitter/") ||
		strings.Contains(path, "vendor/") ||
		strings.HasSuffix(path, ".h") ||
		strings.HasSuffix(path, ".c") {
		return true
	}
	base := filepath.Base(path)
	// Protobuf / gRPC generated Go files
	if strings.HasSuffix(base, ".pb.go") {
		return true
	}
	// Code-generation convention suffixes
	if strings.HasSuffix(base, "_gen.go") ||
		strings.HasSuffix(base, "_generated.go") ||
		strings.HasSuffix(base, ".gen.go") {
		return true
	}
	// controller-gen / kubebuilder: zz_generated.*.go
	if strings.HasPrefix(base, "zz_generated") {
		return true
	}
	// Mock files (mockery, gomock)
	if strings.HasPrefix(base, "mock_") && strings.HasSuffix(base, ".go") {
		return true
	}
	if strings.HasSuffix(base, "_mock.go") {
		return true
	}
	return false
}

// buildConstraintSuffixes covers OS, architecture, and special build-tag
// suffixes used by the Go toolchain for conditional compilation.
var buildConstraintSuffixes = []string{
	// OS
	"_linux.go", "_darwin.go", "_windows.go", "_freebsd.go",
	"_openbsd.go", "_netbsd.go", "_dragonfly.go", "_plan9.go",
	"_solaris.go", "_illumos.go", "_aix.go", "_android.go",
	"_ios.go", "_js.go", "_wasip1.go",
	// Architecture
	"_amd64.go", "_arm64.go", "_arm.go", "_386.go",
	"_mips.go", "_mipsle.go", "_mips64.go", "_mips64le.go",
	"_ppc64.go", "_ppc64le.go", "_s390x.go", "_riscv64.go",
	"_loong64.go", "_wasm.go",
	// Special
	"_stub.go", "_cgo.go", "_nocgo.go", "_purego.go", "_appengine.go",
}

// hasBuildConstraint checks if a Go file has build constraints (build tags).
// Files with build constraints are conditionally compiled — only one variant
// is active per build, so inactive variants always look "dead".
func hasBuildConstraint(path string) bool {
	base := filepath.Base(path)
	for _, s := range buildConstraintSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// matchesExcludePattern checks if a node matches any user-configured exclusion pattern.
// Patterns are matched against both the file path and the node ID.
func matchesExcludePattern(filePath, nodeID string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		// Try glob match against file path
		if matched, _ := filepath.Match(pattern, filePath); matched {
			return true
		}
		// Try prefix match against file path
		if graphpath.HasPrefix(filePath, pattern) {
			return true
		}
		// Try prefix match against node ID. A node ID embeds the file path,
		// so it carries the same native separators.
		if graphpath.HasPrefix(nodeID, pattern) {
			return true
		}
	}
	return false
}

// CollectFanCounts returns per-id fan-in / fan-out counts filtered by
// edge kind. Backends that implement graph.NodeFanAggregator serve
// both counts from one bulk pass per direction (~candidateCount
// rows instead of the full edge set); the fallback path
// streams the requested kinds via EdgesByKind, accumulating into the
// fan maps Go-side -- still no AllEdges materialisation, just an
// in-memory walk of the per-kind edge buckets.
//
// Used by FindHotspots and the health_score analyzer. Both pass the
// same fanInKinds / fanOutKinds pair today; the function signature
// keeps them per-call so a future analyzer with a different kind
// split can share the same plumbing.
func CollectFanCounts(g graph.Store, ids []string, fanInKinds []graph.EdgeKind, fanOutKinds []graph.EdgeKind) (fanIn, fanOut map[string]int) {
	fanIn = make(map[string]int, len(ids))
	fanOut = make(map[string]int, len(ids))
	if len(ids) == 0 {
		return fanIn, fanOut
	}
	if agg, ok := g.(graph.NodeFanAggregator); ok {
		for _, r := range agg.NodeFanCounts(ids, fanInKinds, fanOutKinds) {
			if r.FanIn != 0 {
				fanIn[r.NodeID] = r.FanIn
			}
			if r.FanOut != 0 {
				fanOut[r.NodeID] = r.FanOut
			}
		}
		return fanIn, fanOut
	}

	// Fallback path: stream the requested kinds via EdgesByKind and
	// tally Go-side. ID-set membership keeps the maps bounded to
	// candidate ids, matching the capability contract.
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			idSet[id] = struct{}{}
		}
	}
	streamed := make(map[graph.EdgeKind]struct{}, len(fanInKinds)+len(fanOutKinds))
	stream := func(kind graph.EdgeKind, toIn, toOut bool) {
		if _, ok := streamed[kind]; ok {
			return
		}
		streamed[kind] = struct{}{}
		for e := range g.EdgesByKind(kind) {
			if e == nil {
				continue
			}
			if toIn {
				if _, ok := idSet[e.To]; ok {
					fanIn[e.To]++
				}
			}
			if toOut {
				if _, ok := idSet[e.From]; ok {
					fanOut[e.From]++
				}
			}
		}
	}
	inKinds := make(map[graph.EdgeKind]struct{}, len(fanInKinds))
	for _, k := range fanInKinds {
		inKinds[k] = struct{}{}
	}
	outKinds := make(map[graph.EdgeKind]struct{}, len(fanOutKinds))
	for _, k := range fanOutKinds {
		outKinds[k] = struct{}{}
	}
	allKinds := make([]graph.EdgeKind, 0, len(inKinds)+len(outKinds))
	for k := range inKinds {
		allKinds = append(allKinds, k)
	}
	for k := range outKinds {
		if _, dup := inKinds[k]; dup {
			continue
		}
		allKinds = append(allKinds, k)
	}
	for _, k := range allKinds {
		_, toIn := inKinds[k]
		_, toOut := outKinds[k]
		stream(k, toIn, toOut)
	}
	return fanIn, fanOut
}
