package resolver

import (
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Per-synthesizer seed declarations.
//
// A scoped framework run sees the changed files through frameworkScopedStore:
// the files' own rows come from the scoped projections, and every other row a
// synthesizer enumerates must come from a seed. What a synthesizer needs from
// the change differs by synthesizer, and it is one of three things:
//
//   - the rows recorded elsewhere that name one of the changed identities
//     (incoming), for a pass that re-decides such rows when their target's
//     file changes — a gRPC stub bound into a changed handler;
//   - a named prefetch (names): the declarations elsewhere the changed rows
//     join to by name — a Gin registrar's handler names, a placeholder's
//     target name — so the pass's node enumeration sees them;
//   - every node the changed nodes share an edge with (endpoints).
//
// Each synthesizer declares which of them it needs, and the run's seed loads
// each part once, on the first pass that asks for it, with no row cap: a
// part is exactly the rows its declaration names, so a change touching a
// widely-referenced file pays for the incoming rows only when a pass that
// re-decides them runs, and for no name or endpoint read a pass does not use.
// A pass sees only its own declared parts plus the files' own nodes, so one
// pass's declaration never widens another pass's candidates.
//
// A synthesizer not audited for its needs is declared legacy: it sees the
// shared bounded seed the run built before declarations existed (every
// incident row and endpoint of the changed files and every token-named
// node, up to frameworkScopeRetainedRowCap rows), built only when such a
// pass runs.

// frameworkSeedDeclaration is what one synthesizer needs from the change.
type frameworkSeedDeclaration struct {
	// legacy selects the bounded shared seed (see above).
	legacy bool
	// incoming lists the edge kinds of rows recorded outside the changed
	// files that point at one of their nodes and that the pass enumerates as
	// candidates.
	incoming []graph.EdgeKind
	// endpoints admits every node the changed nodes share an edge with, of
	// any kind, as a node candidate.
	endpoints bool
	// nameKinds and nameEdge select the candidate edges — edges out of the
	// changed nodes, and incoming rows of a declared kind — whose join keys
	// (the placeholder target's name, the Meta values) name declarations
	// elsewhere that the pass enumerates by kind.
	nameKinds []graph.EdgeKind
	nameEdge  func(*graph.Edge) bool
	// nameKeys restricts a candidate edge's join keys to these Meta keys (and
	// its placeholder target's name); nil takes every Meta value.
	nameKeys []string
	// nameNode adds, for one changed node, the Meta values that name
	// declarations elsewhere.
	nameNode func(n *graph.Node, add func(value any))
	// languages, when set, limits every part to changes that touch a node of
	// one of these languages: the pass's candidates and targets are that
	// ecosystem's rows, so a change elsewhere cannot reach them through the
	// seed.
	languages []string
}

// frameworkSeedNone is a pass that binds within the changed rows alone or
// looks everything else up itself (a point or name read the scoped store
// passes through): it needs nothing from the seed.
var frameworkSeedNone = frameworkSeedDeclaration{}

var frameworkSeedLegacy = frameworkSeedDeclaration{legacy: true}

// frameworkSynthSeedDeclarations holds every synthesizer that runs through the
// generic scoped store. frameworkSynthSeedDeclarationFor refuses a name that
// is missing, so a new synthesizer has to state its needs.
var frameworkSynthSeedDeclarations = map[string]frameworkSeedDeclaration{
	// Re-decides every grpc.stub call, bound or not: a stub bound into a
	// changed handler is an incoming call. The handler index joins the
	// stub's service and method, and the registration's impl type, by name.
	SynthGRPCStub: {
		incoming:  []graph.EdgeKind{graph.EdgeCalls},
		endpoints: true,
		nameKinds: []graph.EdgeKind{graph.EdgeCalls},
		nameKeys:  []string{"grpc_service", "grpc_method", "grpc_register_service", "grpc_register_impl"},
		nameEdge: func(e *graph.Edge) bool {
			if frameworkEdgeVia(e) == "grpc.stub" {
				return true
			}
			svc, _ := e.Meta["grpc_register_service"].(string)
			return svc != ""
		},
	},
	// Re-decides temporal.* calls (stubs, starts, registrations and the
	// wrapper callers) and reads temporal annotations.
	SynthTemporalStub: {
		incoming:  []graph.EdgeKind{graph.EdgeCalls, graph.EdgeAnnotated},
		endpoints: true,
		nameKinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeAnnotated},
		nameEdge: func(e *graph.Edge) bool {
			if e.Kind == graph.EdgeAnnotated {
				role, method := temporalRoleForJavaAnnotation(e.To)
				return role != "" || method != ""
			}
			return strings.HasPrefix(frameworkEdgeVia(e), "temporal.")
		},
	},
	// Joins a changed registrar's handler names to their definitions.
	SynthGinMiddleware: {
		nameNode: func(n *graph.Node, add func(any)) {
			if v, ok := n.Meta["gin_handlers"]; ok {
				add(v)
			}
		},
	},
	// Binds an unresolved receiver-typed call to the autoload the receiver
	// names and that script's member. Its scripts are GDScript files and its
	// autoloads project.godot rows: a change touching neither cannot bring
	// one into view.
	SynthGodotAutoload: {
		nameKinds: []graph.EdgeKind{graph.EdgeCalls},
		nameEdge:  frameworkUnresolvedReceiverCall,
		nameKeys:  []string{"receiver_type"},
		languages: []string{"gdscript", "godot_project"},
	},
	// The same calls, through a preload alias declared in the caller's file.
	SynthGodotPreloadAlias: {
		nameKinds: []graph.EdgeKind{graph.EdgeCalls},
		nameEdge:  frameworkUnresolvedReceiverCall,
		nameKeys:  []string{"receiver_type"},
		nameNode: func(n *graph.Node, add func(any)) {
			if v, ok := n.Meta["gd_preload"]; ok {
				add(v)
			}
		},
		languages: []string{"gdscript"},
	},
	// Re-decides every scene connection, bound or not; the handler method
	// is named on the connection. Connections are scene rows into GDScript
	// members.
	SynthGodotConnection: {
		incoming:  []graph.EdgeKind{graph.EdgeReferences},
		nameKinds: []graph.EdgeKind{graph.EdgeReferences},
		nameEdge: func(e *graph.Edge) bool {
			conn, _ := e.Meta["godot_connection"].(bool)
			return conn
		},
		nameKeys:  []string{"godot_method"},
		languages: []string{"gdscript", "godot_resource"},
	},
	// Acts only on the changed files' own placeholders and looks every hop
	// up by name itself.
	SynthFactoryChain: frameworkSeedNone,
	// Binds each captured value to a definition in its own file, or looks
	// it up by name itself.
	SynthFnValue: frameworkSeedNone,
	// Binds each captured read to a constant declared in its own file.
	SynthValueRefName: frameworkSeedNone,
	// Re-decides every store-factory call, bound or not, against the
	// actions and getters its binding and action name.
	SynthStoreFactory: {
		incoming:  []graph.EdgeKind{graph.EdgeCalls},
		nameKinds: []graph.EdgeKind{graph.EdgeCalls},
		nameKeys:  []string{"store_binding", "store_action"},
		nameEdge:  func(e *graph.Edge) bool { return frameworkEdgeVia(e) == storeFactoryVia },
	},
	// Binds the changed files' own unresolved Depends / router placeholders,
	// looking each name up itself (ResolveByConvention).
	SynthFastAPIResolve: frameworkSeedNone,
	// Re-decides every MediatR dispatch, bound or not, against the handlers
	// its placeholder names.
	SynthMediatR: {
		incoming:  []graph.EdgeKind{graph.EdgeCalls},
		nameKinds: []graph.EdgeKind{graph.EdgeCalls},
		nameKeys:  []string{"mediatr_request_type"},
		nameEdge:  func(e *graph.Edge) bool { return frameworkEdgeVia(e) == mediatrVia },
	},
}

// frameworkSeedLegacyPasses are the generic synthesizers whose needs are not
// yet declared; they keep the bounded shared seed.
var frameworkSeedLegacyPasses = []string{
	SynthEventChannel, SynthSwiftObjC, SynthReactNative, SynthReactNativePair,
	SynthObserverChannel, SynthClosureCollection, SynthReactSetState,
	SynthFlutterSetState, SynthKMPExpectActual, SynthExpoModules, SynthFabric,
	SynthMyBatis, SynthSQLCallsite, SynthReduxThunk, SynthNgRxEffect,
	SynthObjectRegistry, SynthRTKQuery, SynthVuexDispatch, SynthCelery,
	SynthSpringEvent, SynthSidekiq, SynthLaravelEvent, SynthFnPointerDispatch,
	SynthMacroExpansion, SynthExpressResolve, SynthReactResolve,
	SynthRailsResolve, SynthSwiftUIResolve, SynthUIKitResolve,
	SynthVaporResolve, SynthGoFrameRoute, SynthSvelteKitLoad, SynthRustScope,
	SynthPascalFormName,
}

func init() {
	for _, name := range frameworkSeedLegacyPasses {
		if _, dup := frameworkSynthSeedDeclarations[name]; !dup {
			frameworkSynthSeedDeclarations[name] = frameworkSeedLegacy
		}
	}
}

// frameworkSynthSeedDeclarationFor returns a synthesizer's declaration. A
// synthesizer with none is a registration error: it panics, like the
// scoped run's other refusal to guess (an unscoped synthesizer).
func frameworkSynthSeedDeclarationFor(name string) frameworkSeedDeclaration {
	decl, ok := frameworkSynthSeedDeclarations[name]
	if !ok {
		panic("framework synthesizer has no seed declaration: " + name)
	}
	return decl
}

func frameworkEdgeVia(e *graph.Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	via, _ := e.Meta["via"].(string)
	return via
}

func frameworkUnresolvedReceiverCall(e *graph.Edge) bool {
	if !graph.IsUnresolvedTarget(e.To) {
		return false
	}
	recv, _ := e.Meta["receiver_type"].(string)
	return recv != ""
}

func (d frameworkSeedDeclaration) declaresIncoming(kind graph.EdgeKind) bool {
	for _, k := range d.incoming {
		if k == kind {
			return true
		}
	}
	return false
}

func (d frameworkSeedDeclaration) needsNodes() bool {
	return d.endpoints || d.nameEdge != nil || d.nameNode != nil
}

// frameworkDeclaredSeed is one scoped run's seed: the changed files' nodes,
// read at construction, and the declared parts, each read once on first use.
// Views handed to passes share its outputs overlay.
type frameworkDeclaredSeed struct {
	store   graph.Store
	scope   frameworkExecutionScope
	outputs *frameworkScopedOutputs

	fileNodes map[string]*graph.Node
	fileIDs   []string

	incomingRead bool
	incoming     map[graph.EdgeKind][]*graph.Edge

	fileEdges map[graph.EdgeKind][]*graph.Edge

	endpointsRead bool
	endpoints     map[string]*graph.Node

	names map[string][]*graph.Node

	legacy *frameworkScopedSeed

	rows    int
	elapsed time.Duration
}

func newFrameworkDeclaredSeed(store graph.Store, repos map[string]bool, filePaths []string) *frameworkDeclaredSeed {
	started := time.Now()
	d := &frameworkDeclaredSeed{
		store: store,
		scope: newFrameworkExecutionScope(repos, filePaths),
		outputs: &frameworkScopedOutputs{
			nodes:   make(map[string]*graph.Node),
			edges:   make(map[graph.EdgeIdentity]*graph.Edge),
			removed: make(map[graph.EdgeIdentity]struct{}),
		},
		fileNodes: make(map[string]*graph.Node),
		fileEdges: make(map[graph.EdgeKind][]*graph.Edge),
		names:     make(map[string][]*graph.Node),
	}
	if store != nil && len(d.scope.filePaths) > 0 {
		byFile := store.GetFileNodesByPaths(d.scope.filePaths)
		for _, filePath := range d.scope.filePaths {
			for _, node := range byFile[filePath] {
				if node == nil || node.ID == "" || !frameworkInExecutionScope(d.scope, node) {
					continue
				}
				if _, dup := d.fileNodes[node.ID]; dup {
					continue
				}
				d.fileNodes[node.ID] = node
				d.fileIDs = append(d.fileIDs, node.ID)
			}
		}
		d.rows += len(d.fileIDs)
	}
	d.elapsed += time.Since(started)
	return d
}

// frameworkInExecutionScope is frameworkScopedStore.inBaseScope for a scope.
func frameworkInExecutionScope(scope frameworkExecutionScope, node *graph.Node) bool {
	return (&frameworkScopedStore{scope: scope}).inBaseScope(node)
}

// passSeed is the seed one synthesizer's pass reads through.
func (d *frameworkDeclaredSeed) passSeed(name string) *frameworkScopedSeed {
	decl := frameworkSynthSeedDeclarationFor(name)
	if decl.legacy {
		return d.legacySeed()
	}
	if len(decl.languages) > 0 && !d.touchesLanguage(decl.languages) {
		decl = frameworkSeedNone
	}
	nodes := make(map[string]*graph.Node, len(d.fileNodes))
	for id, node := range d.fileNodes {
		nodes[id] = node
	}
	return &frameworkScopedSeed{
		store:          d.store,
		scope:          d.scope,
		nodes:          nodes,
		incidentByKind: make(map[graph.EdgeKind][]*graph.Edge),
		incidentSeen:   make(map[graph.EdgeIdentity]struct{}),
		outputs:        d.outputs,
		retainedRows:   len(nodes),
		declared:       d,
		decl:           decl,
		incidentReady:  make(map[graph.EdgeKind]bool),
	}
}

// touchesLanguage reports whether a changed node is of one of languages.
func (d *frameworkDeclaredSeed) touchesLanguage(languages []string) bool {
	for _, node := range d.fileNodes {
		for _, language := range languages {
			if node.Language == language {
				return true
			}
		}
	}
	return false
}

// passStore is the scoped store one synthesizer's pass runs against.
func (d *frameworkDeclaredSeed) passStore(name string) *frameworkScopedStore {
	return d.passSeed(name).newPassStore()
}

// legacySeed builds the bounded shared seed once, over the run's outputs.
func (d *frameworkDeclaredSeed) legacySeed() *frameworkScopedSeed {
	if d.legacy == nil {
		started := time.Now()
		d.legacy = newFrameworkScopedSeed(d.store, d.scope.repos, d.scope.filePaths)
		d.legacy.outputs = d.outputs
		d.rows += d.legacy.retainedRows
		d.elapsed += time.Since(started)
	}
	return d.legacy
}

// incomingOf returns the rows recorded outside the changed files that point
// at one of their nodes, of one kind. The incident read runs once per run.
func (d *frameworkDeclaredSeed) incomingOf(kind graph.EdgeKind) []*graph.Edge {
	d.readIncoming()
	return d.incoming[kind]
}

// readIncoming runs the incident read: every row into a changed node, by
// kind.
func (d *frameworkDeclaredSeed) readIncoming() {
	if !d.incomingRead {
		d.incomingRead = true
		started := time.Now()
		d.incoming = make(map[graph.EdgeKind][]*graph.Edge)
		if d.store != nil && len(d.fileIDs) > 0 {
			rows := d.store.GetInEdgesByNodeIDs(d.fileIDs)
			for _, id := range d.fileIDs {
				for _, edge := range rows[id] {
					if edge == nil {
						continue
					}
					d.incoming[edge.Kind] = append(d.incoming[edge.Kind], edge)
					d.rows++
				}
			}
		}
		d.elapsed += time.Since(started)
	}
}

// fileEdgesOf returns the edges out of the changed nodes of one kind, as the
// scoped projection the pass itself reads serves them.
func (d *frameworkDeclaredSeed) fileEdgesOf(kind graph.EdgeKind) []*graph.Edge {
	if edges, ok := d.fileEdges[kind]; ok {
		return edges
	}
	started := time.Now()
	var edges []*graph.Edge
	if d.store != nil && len(d.fileIDs) > 0 {
		for row := range graph.EdgesInScopeSeq(d.store, d.scope.repoPrefixes, d.scope.filePaths, kind) {
			if row.Edge != nil {
				edges = append(edges, row.Edge)
			}
		}
	}
	if edges == nil {
		edges = []*graph.Edge{}
	}
	d.fileEdges[kind] = edges
	d.rows += len(edges)
	d.elapsed += time.Since(started)
	return edges
}

// endpointNodes returns every node a changed node shares an edge with.
func (d *frameworkDeclaredSeed) endpointNodes() map[string]*graph.Node {
	if d.endpointsRead {
		return d.endpoints
	}
	d.endpointsRead = true
	d.readIncoming()
	started := time.Now()
	d.endpoints = make(map[string]*graph.Node)
	if d.store == nil || len(d.fileIDs) == 0 {
		d.elapsed += time.Since(started)
		return d.endpoints
	}
	seen := make(map[string]struct{})
	var ids []string
	note := func(id string) {
		if id == "" || graph.IsUnresolvedTarget(id) {
			return
		}
		if _, own := d.fileNodes[id]; own {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, edges := range d.incoming {
		for _, edge := range edges {
			note(edge.From)
		}
	}
	outgoing := d.store.GetOutEdgesByNodeIDs(d.fileIDs)
	for _, id := range d.fileIDs {
		for _, edge := range outgoing[id] {
			if edge != nil {
				note(edge.To)
			}
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		for id, node := range d.store.GetNodesByIDs(ids) {
			if node != nil {
				d.endpoints[id] = node
			}
		}
	}
	d.rows += len(d.endpoints)
	d.elapsed += time.Since(started)
	return d.endpoints
}

// declaredNames extracts the names one declaration prefetches, sorted.
func (d *frameworkDeclaredSeed) declaredNames(decl frameworkSeedDeclaration) []string {
	tokens := make(map[string]struct{})
	if decl.nameNode != nil {
		for _, id := range d.fileIDs {
			node := d.fileNodes[id]
			if node == nil || node.Meta == nil {
				continue
			}
			decl.nameNode(node, func(value any) { addDeclaredFrameworkToken(tokens, value) })
		}
	}
	if decl.nameEdge != nil {
		for _, kind := range decl.nameKinds {
			candidates := d.fileEdgesOf(kind)
			if decl.declaresIncoming(kind) {
				candidates = append(append([]*graph.Edge(nil), candidates...), d.incomingOf(kind)...)
			}
			for _, edge := range candidates {
				if edge == nil || edge.Meta == nil || !decl.nameEdge(edge) {
					continue
				}
				if graph.IsUnresolvedTarget(edge.To) {
					addDeclaredFrameworkToken(tokens, graph.UnresolvedName(edge.To))
				}
				if decl.nameKeys == nil {
					for _, value := range edge.Meta {
						addDeclaredFrameworkToken(tokens, value)
					}
					continue
				}
				for _, key := range decl.nameKeys {
					if value, ok := edge.Meta[key]; ok {
						addDeclaredFrameworkToken(tokens, value)
					}
				}
			}
		}
	}
	names := make([]string, 0, len(tokens))
	for token := range tokens {
		names = append(names, token)
	}
	sort.Strings(names)
	return names
}

// namedNodes returns every node carrying one of names, read once per name
// per run.
func (d *frameworkDeclaredSeed) namedNodes(names []string) []*graph.Node {
	var missing []string
	for _, name := range names {
		if _, ok := d.names[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 && d.store != nil {
		started := time.Now()
		matches := d.store.FindNodesByNames(missing)
		for _, name := range missing {
			nodes := append([]*graph.Node(nil), matches[name]...)
			sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
			d.names[name] = nodes
			d.rows += len(nodes)
		}
		d.elapsed += time.Since(started)
	}
	var out []*graph.Node
	for _, name := range names {
		out = append(out, d.names[name]...)
	}
	return out
}

// addDeclaredFrameworkToken is addFrameworkToken without the token cap: a
// declaration names only its own join keys.
func addDeclaredFrameworkToken(tokens map[string]struct{}, value any) {
	switch typed := value.(type) {
	case string:
		token := strings.TrimSpace(typed)
		if token == "" || len(token) > 256 {
			return
		}
		tokens[token] = struct{}{}
		if i := strings.LastIndexAny(token, ".:/#"); i >= 0 && i+1 < len(token) {
			tokens[token[i+1:]] = struct{}{}
		}
	case []string:
		for _, item := range typed {
			addDeclaredFrameworkToken(tokens, item)
		}
	case []any:
		for _, item := range typed {
			addDeclaredFrameworkToken(tokens, item)
		}
	}
}

// ensureNodes loads a declared view's node parts: the declared endpoints and
// named nodes. A legacy seed is complete at construction.
func (s *frameworkScopedSeed) ensureNodes() {
	if s == nil || s.declared == nil || s.nodesReady {
		return
	}
	s.nodesReady = true
	if !s.decl.needsNodes() {
		return
	}
	add := func(node *graph.Node) {
		if node == nil || node.ID == "" {
			return
		}
		if _, dup := s.nodes[node.ID]; dup {
			return
		}
		s.nodes[node.ID] = node
		s.retainedRows++
		s.retainedBytes += frameworkNodeBytes(node)
	}
	if s.decl.endpoints {
		ids := make([]string, 0)
		endpoints := s.declared.endpointNodes()
		for id := range endpoints {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			add(endpoints[id])
		}
	}
	for _, node := range s.declared.namedNodes(s.declared.declaredNames(s.decl)) {
		add(node)
	}
}

// ensureIncident loads a declared view's incoming rows of one kind, when
// its declaration names the kind.
func (s *frameworkScopedSeed) ensureIncident(kind graph.EdgeKind) {
	if s == nil || s.declared == nil || s.incidentReady[kind] {
		return
	}
	s.incidentReady[kind] = true
	if !s.decl.declaresIncoming(kind) {
		return
	}
	for _, edge := range s.declared.incomingOf(kind) {
		key := graph.EdgeIdentityFor(edge)
		if _, dup := s.incidentSeen[key]; dup {
			continue
		}
		s.incidentSeen[key] = struct{}{}
		s.incidentByKind[kind] = append(s.incidentByKind[kind], edge)
		s.retainedRows++
		s.retainedBytes += frameworkEdgeBytes(edge)
	}
}

// frameworkCandidateGatedPasses are the scoped passes whose candidates are
// exactly the rows their declaration's nameEdge selects, among the changed
// files' own rows and (for a declared incoming kind) the rows into them: a
// run whose change carries none has nothing to re-decide, and the pass does
// not run (passHasCandidates).
var frameworkCandidateGatedPasses = map[string]bool{
	SynthGRPCStub:        true,
	SynthTemporalStub:    true,
	SynthGodotConnection: true,
}

// passHasCandidates reports whether a candidate-gated pass has a row to
// re-decide in this run: a changed file's row, or a row into a changed node
// of a declared incoming kind, that the declaration's nameEdge selects.
// Passes that are not candidate-gated always report true.
func (d *frameworkDeclaredSeed) passHasCandidates(name string) bool {
	if !frameworkCandidateGatedPasses[name] {
		return true
	}
	decl := frameworkSynthSeedDeclarationFor(name)
	if decl.legacy || decl.nameEdge == nil {
		return true
	}
	if len(decl.languages) > 0 && !d.touchesLanguage(decl.languages) {
		return false
	}
	for _, kind := range decl.nameKinds {
		for _, edge := range d.fileEdgesOf(kind) {
			if edge != nil && edge.Meta != nil && decl.nameEdge(edge) {
				return true
			}
		}
		if decl.declaresIncoming(kind) {
			for _, edge := range d.incomingOf(kind) {
				if edge != nil && edge.Meta != nil && decl.nameEdge(edge) {
					return true
				}
			}
		}
	}
	return false
}
