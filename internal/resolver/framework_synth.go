package resolver

import (
	"iter"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Framework-dispatch synthesizer engine.
//
// Direct AST/LSP resolution lands the calls a compiler can see. A large
// class of real call edges, though, is wired by a *framework* at runtime
// and is invisible to static resolution: a gRPC client stub dispatched
// to its server handler, a Temporal workflow proxy to its activity, an
// event published on one side of an in-process channel and handled on
// the other, a JS bridge method routed to its native implementation.
//
// FrameworkSynthesizer is the plugin contract for a pass that
// materialises one such family of edges. Every synthesizer is a
// full-recompute, idempotent pass: it derives each edge it owns from
// durable graph state (placeholder edges plus their Meta markers, shared
// topic nodes, registration call edges) so a reindex of any endpoint
// re-lands or un-lands the edge without leaving a stale one behind —
// graph.AddEdge dedupes by edge key and graph.EvictFile drops a node's
// edges in both directions. Every edge a synthesizer lands is stamped
// with provenance (StampSynthesized) so its origin is auditable and the
// `analyze kind=synthesizers` roll-up can attribute it.
//
// The engine is the single orchestration point: the indexers call
// RunFrameworkSynthesizers at every settle point (full index, watcher
// reindex, incremental reindex) in place of invoking each pass directly,
// so adding a synthesizer (a native-bridge resolver, an event-channel
// pass) is one line in defaultFrameworkSynthesizers rather than an edit
// at six call sites.
type FrameworkSynthesizer interface {
	// Name is the stable provenance tag stamped on every edge the
	// synthesizer lands (lower-kebab, e.g. "grpc-stub", "event-channel").
	Name() string
	// Synthesize runs the pass over g and returns the number of edges the
	// synthesizer owns (landed on a real target) after this run.
	Synthesize(g graph.Store) int
}

// Edge.Meta keys stamped by StampSynthesized.
const (
	// MetaSynthesizedBy names the synthesizer that produced an edge.
	MetaSynthesizedBy = "synthesized_by"
	// MetaProvenance records that an edge is a heuristic materialisation
	// rather than a compiler-verified fact.
	MetaProvenance = "provenance"
	// ProvenanceHeuristic is the MetaProvenance value the string- and
	// name-keyed framework synthesizers stamp — these edges are
	// framework-dispatch inferences correlated by a literal (an event
	// name, a dispatch string, a registry key) with no type evidence.
	ProvenanceHeuristic = "heuristic"
	// ProvenanceFramework is the MetaProvenance value the typed,
	// decorator-, base-list- or type-keyed synthesizers stamp — the
	// framework's own contract (a decorator, a generic base, a typed
	// listener parameter) names the target, so the edge carries more
	// confidence than a string-correlated guess. analyze kind=synthesizers
	// reports the two tiers separately from the same MetaProvenance read.
	ProvenanceFramework = "framework"
)

// Confidence tiers the framework synthesizers stamp on a landed edge.
// Typed/decorator/base-list/type-keyed passes (RTK Query, Celery, Spring,
// MediatR, Sidekiq, Laravel, GoFrame) use ConfidenceTyped; the string-
// and name-keyed passes (Vuex, Redux-thunk, object-registry, fn-pointer,
// Django) use ConfidenceHeuristic.
const (
	// ConfidenceTyped is the confidence for a type-/decorator-/base-list-
	// keyed dispatch edge — the framework contract names the target.
	ConfidenceTyped = 0.85
	// ConfidenceHeuristic is the confidence for a string-/name-keyed
	// dispatch edge — correlated by a literal, not by a type.
	ConfidenceHeuristic = 0.6
)

// Stable per-synthesizer provenance names. Used both as the registry
// label (for the report grouping) and as the value stamped on each
// landed edge, so the two never drift.
const (
	SynthGRPCStub            = "grpc-stub"
	SynthTemporalStub        = "temporal-stub"
	SynthEventChannel        = "event-channel"
	SynthSwiftObjC           = "swift-objc-bridge"
	SynthReactNative         = "react-native-bridge"
	SynthReactNativePair     = "react-native-native-pair"
	SynthObserverChannel     = "observer-channel"
	SynthClosureCollection   = "closure-collection"
	SynthReactSetState       = "react-setstate"
	SynthFlutterSetState     = "flutter-setstate"
	SynthKMPExpectActual     = "kmp-expect-actual"
	SynthExpoModules         = "expo-modules-bridge"
	SynthFabric              = "fabric-codegen"
	SynthMyBatis             = "mybatis"
	SynthRustScope           = "rust-scope"
	SynthFactoryChain        = "factory-chain"
	SynthSQLCallsite         = "sql-callsite"
	SynthStoreFactory        = "store-factory"
	SynthReduxThunk          = "redux-thunk"
	SynthNgRxEffect          = "ngrx-effect"
	SynthObjectRegistry      = "object-registry"
	SynthRTKQuery            = "rtk-query"
	SynthVuexDispatch        = "vuex-dispatch"
	SynthCelery              = "celery-dispatch"
	SynthSpringEvent         = "spring-event"
	SynthMediatR             = "mediatr-dispatch"
	SynthCSharpIfaceDispatch = "csharp-iface-dispatch"
	SynthCSharpDIIfaces      = "csharp-di-implemented-interfaces"
	SynthCSharpEFCoreModels  = "csharp-efcore-models"
	SynthSidekiq             = "sidekiq-dispatch"
	SynthLaravelEvent        = "laravel-event"
	SynthFnPointerDispatch   = "fn-pointer-dispatch"
	SynthMacroExpansion      = "macro-expansion"
	SynthGoFrameRoute        = "goframe-route"
	SynthDjangoDescriptor    = "django-descriptor"
	SynthExpressResolve      = "express-resolve"
	SynthReactResolve        = "react-resolve"
	SynthFastAPIResolve      = "fastapi-resolve"
	SynthRailsResolve        = "rails-resolve"
	SynthSwiftUIResolve      = "swiftui-resolve"
	SynthUIKitResolve        = "uikit-resolve"
	SynthVaporResolve        = "vapor-resolve"
	SynthGodotAutoload       = "godot-autoload"
	SynthGodotPreloadAlias   = "godot-preload-alias"
	SynthGodotConnection     = "godot-connection"
	SynthGinMiddleware       = "gin-middleware"
	SynthSvelteKitLoad       = "sveltekit-load"
	SynthSpeculative         = "speculative-dispatch"
	SynthFnValue             = SynthFnValueCallback
	SynthPascalFormName      = SynthPascalForm
	SynthValueRefName        = SynthValueRef
)

// StampSynthesized marks an edge as the product of a framework
// synthesizer: which synthesizer produced it (name) and that it is a
// heuristic materialisation. Safe on an edge with a nil Meta map.
func StampSynthesized(e *graph.Edge, name string) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[MetaSynthesizedBy] = name
	if _, ok := e.Meta[MetaProvenance]; !ok {
		e.Meta[MetaProvenance] = ProvenanceHeuristic
	}
}

// StampSynthesizedTyped marks an edge as the product of a typed-tier
// framework synthesizer: like StampSynthesized, but records
// ProvenanceFramework instead of ProvenanceHeuristic so the
// type-/decorator-/base-list-keyed passes (RTK Query, Celery, Spring,
// MediatR, Sidekiq, Laravel, GoFrame) separate from the string-keyed
// ones in analyze kind=synthesizers. Safe on an edge with a nil Meta map.
func StampSynthesizedTyped(e *graph.Edge, name string) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[MetaProvenance] = ProvenanceFramework
	StampSynthesized(e, name)
}

// UnstampSynthesized clears the provenance markers an edge picked up from
// a synthesizer. Called when a pass re-orphans an edge (its target
// disappeared) so the edge reads as a plain placeholder again.
func UnstampSynthesized(e *graph.Edge) {
	if e == nil || e.Meta == nil {
		return
	}
	delete(e.Meta, MetaSynthesizedBy)
	delete(e.Meta, MetaProvenance)
}

// synthFunc adapts a plain pass function into a FrameworkSynthesizer so
// the existing passes (ResolveGRPCStubCalls, …) register without a
// wrapper type each.
//
// scopedFn is optional: passes with bespoke cross-repository reconciliation may
// consume the changed-repo prefix set directly. Every other pass runs through
// frameworkScopedStore on a partial invocation; no synthFunc is permitted to
// fall back to the unfiltered workspace store.
type synthFunc struct {
	name     string
	fn       func(graph.Store) int
	scopedFn func(graph.Store, map[string]bool) int
	// candFn is the shared-stream form: the pass consumes the candidate
	// buffers the census dispatcher collected instead of re-decoding whole
	// edge kinds. Used only when a full-census run armed the pass's
	// collectors; every other invocation keeps fn / scopedFn.
	candFn func(graph.Store, *frameworkPassCandidates) int
}

func (s synthFunc) Name() string                 { return s.name }
func (s synthFunc) Synthesize(g graph.Store) int { return s.fn(g) }

// synthesizeScoped preserves the legacy entry point used by focused tests. A
// non-nil scope never reaches fn with the unfiltered store.
func (s synthFunc) synthesizeScoped(g graph.Store, scope map[string]bool) int {
	if scope == nil {
		return runLegacyFrameworkSynth(g, s.fn)
	}
	if s.scopedFn != nil {
		return s.scopedFn(g, scope)
	}
	return runLegacyFrameworkSynth(newFrameworkScopedStore(g, scope, nil), s.fn)
}

// frameworkSynthLanguageFamilies is deliberately conservative. An absent
// entry means that the pass is generic, spans too many runtimes to bound
// safely, or has not yet been audited, and therefore always runs. A mapped
// pass may be skipped only when its candidate domain contains none of the
// listed language families.
var frameworkSynthLanguageFamilies = map[string][]string{
	SynthSwiftObjC:           {"apple"},
	SynthReactNative:         {"web", "apple", "jvm"},
	SynthReactNativePair:     {"apple", "jvm"},
	SynthClosureCollection:   {"apple"},
	SynthKMPExpectActual:     {"jvm"},
	SynthExpoModules:         {"web", "apple", "jvm"},
	SynthFabric:              {"web", "apple", "jvm"},
	SynthMyBatis:             {"jvm"},
	SynthSQLCallsite:         {"sql"},
	SynthStoreFactory:        {"web"},
	SynthReduxThunk:          {"web"},
	SynthNgRxEffect:          {"web"},
	SynthObjectRegistry:      {"web"},
	SynthRTKQuery:            {"web"},
	SynthVuexDispatch:        {"web"},
	SynthCelery:              {"python"},
	SynthSpringEvent:         {"jvm"},
	SynthMediatR:             {"dotnet"},
	SynthCSharpIfaceDispatch: {"dotnet"},
	SynthCSharpDIIfaces:      {"dotnet"},
	SynthCSharpEFCoreModels:  {"dotnet"},
	SynthSidekiq:             {"ruby"},
	SynthLaravelEvent:        {"php"},
	SynthFnPointerDispatch:   {"c"},
	SynthMacroExpansion:      {"c"},
	SynthGinMiddleware:       {"go"},
	SynthExpressResolve:      {"web"},
	SynthReactResolve:        {"web"},
	SynthFastAPIResolve:      {"python"},
	SynthRailsResolve:        {"ruby"},
	SynthSwiftUIResolve:      {"apple"},
	SynthUIKitResolve:        {"apple"},
	SynthVaporResolve:        {"apple"},
	SynthGoFrameRoute:        {"go"},
	SynthSvelteKitLoad:       {"web"},
	SynthRustScope:           {"rust"},
	SynthPascalFormName:      {"pascal"},
}

type frameworkCandidateSummary struct {
	all           map[string]int
	scoped        map[string]int
	allMarkers    map[string]int
	scopedMarkers map[string]int
	// edges is the cold-run EdgeCalls admission census. Valid only when the
	// census walked the full stream (nil scope); scoped runs never consult it.
	edges frameworkEdgeCensus
	// csharpTypeNames counts distinct C# type/interface names, saturating at
	// two — the receiver-gate tail can demote only when the receiver and
	// target names differ and both are indexed C# types. Full-census runs only.
	csharpTypeNames frameworkDistinctNames
	// fullCensus records that this summary walked the ENTIRE store — a nil
	// scope, or a non-nil scope under the daemon's full-coverage attestation.
	// Absence-proof gates (edge census, family/receiver tails) may only
	// trust counts from a full walk; a partial summary cannot prove absence.
	fullCensus bool
	// streams carries the shared-stream candidate buffers the census edge
	// walks collected for the converted synthesizers. Full-census runs only;
	// nil on a partial run, where every pass keeps its own scoped scans.
	streams *frameworkStreamCandidates
}

// frameworkDistinctNames counts distinct non-empty names, saturating at two.
type frameworkDistinctNames struct {
	first string
	count int
}

func (d *frameworkDistinctNames) note(name string) {
	if name == "" || d.count >= 2 {
		return
	}
	if d.count == 0 {
		d.first = name
		d.count = 1
		return
	}
	if name != d.first {
		d.count = 2
	}
}

// frameworkEdgeCensus is the shared EdgeCalls admission census for the
// via-gated synthesizers: one Meta-decoding pass over the same edge stream
// those passes read, so each pass's own admission predicate is answered once
// instead of per pass. valid is set only on a nil-scope run — a scoped
// synthesizer view can admit incident placeholder edges a scoped stream walk
// would not see, so a partial census cannot prove absence.
type frameworkEdgeCensus struct {
	valid bool
	// via holds every Meta["via"] string present on an EdgeCalls edge.
	via map[string]bool
	// expressHandlerRef: the express_handler_ref Meta key rode on an
	// unresolved-target edge.
	expressHandlerRef bool
	// recvConst: a non-empty Meta["recv_const"] rode on an unresolved-target
	// edge.
	recvConst bool
	// setStateTarget: some edge's To satisfies isSetStateTarget.
	setStateTarget bool
	// objectRegistryValue: a non-empty Meta["registry_value"] rode on an
	// object-registry via edge.
	objectRegistryValue bool
	// grpcStub mirrors ResolveGRPCStubCalls' EXACT admission predicate: a
	// via=="grpc.stub" EdgeCalls edge carrying non-empty grpc_service AND
	// grpc_method metadata. Presence of the via alone is not enough — the
	// pass discards service/method-less stubs before building its index.
	grpcStub bool
	// temporalVia: some EdgeCalls via has the "temporal." prefix — the first
	// half of ResolveTemporalCalls' presence probe.
	temporalVia bool
	// temporalAnnotation: some EdgeAnnotated target satisfies the exact Java
	// temporal annotation-role predicate — the probe's second half. Filled by
	// a break-on-first-hit EdgeAnnotated walk that runs only when no
	// temporal via was seen, mirroring the pass's own short-circuit.
	temporalAnnotation bool
}

// collectFrameworkEdgeCensus makes the cold-run EdgeCalls pass feeding
// frameworkSynthEdgePreflights. Every flag is a necessary condition copied
// verbatim from the consuming pass's own loop filter, so a missed flag can
// only keep a pass enabled, never skip one that could land an edge.
//
// The same decoded walk doubles as the shared-stream candidate dispatcher:
// when streams is non-nil, every edge is also offered to the armed per-pass
// collectors, and the (smaller) EdgeAnnotated and EdgeReferences kinds are
// walked once here for their consumers instead of once per pass.
func collectFrameworkEdgeCensus(g graph.Store, streams *frameworkStreamCandidates) frameworkEdgeCensus {
	census := frameworkEdgeCensus{valid: true, via: map[string]bool{}}
	for e := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeCalls) {
		streams.collectCalls(e)
		if isSetStateTarget(e.To) {
			census.setStateTarget = true
		}
		if e.Via != "" {
			census.via[e.Via] = true
			if e.Via == objectRegistryVia && !census.objectRegistryValue && e.RegistryValue != "" {
				census.objectRegistryValue = true
			}
			if e.Via == "grpc.stub" && !census.grpcStub && e.GRPCService != "" && e.GRPCMethod != "" {
				census.grpcStub = true
			}
			if !census.temporalVia && strings.HasPrefix(e.Via, "temporal.") {
				census.temporalVia = true
			}
		}
		if graph.IsUnresolvedTarget(e.To) {
			if e.HasExpressHandlerRef {
				census.expressHandlerRef = true
			}
			if e.RecvConst != "" {
				census.recvConst = true
			}
		}
	}
	if streams.wantsAnnotated() {
		// The temporal pass consumes the matched annotation edges
		// themselves, so the presence probe's break-on-first-hit walk
		// becomes one full collection walk of this small kind — replacing
		// the pass's own EdgeAnnotated scan.
		for e := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeAnnotated) {
			if role, member := temporalRoleForJavaAnnotation(e.To); role != "" || member != "" {
				streams.addAnnotated(e)
			}
		}
		if !census.temporalVia {
			census.temporalAnnotation = streams.annotatedCount() > 0
		}
	} else if !census.temporalVia {
		for e := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeAnnotated) {
			if role, member := temporalRoleForJavaAnnotation(e.To); role != "" || member != "" {
				census.temporalAnnotation = true
				break
			}
		}
	}
	if streams.wantsRefs() {
		for e := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeReferences) {
			streams.collectRefs(e)
		}
	}
	return census
}

// summarizeFrameworkCandidates makes one metadata-free node pass for the
// synthesizer run. Full/cold runs retain the whole-graph projection; partial
// runs read only the changed repository buckets and use that same candidate
// census to gate both scoped and legacy synthesizers.
func summarizeFrameworkCandidates(g graph.Store, scope map[string]bool) frameworkCandidateSummary {
	return summarizeFrameworkCandidatesForFiles(g, scope, nil)
}

func summarizeFrameworkCandidatesForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
) frameworkCandidateSummary {
	return summarizeFrameworkCandidatesCensus(g, scope, filePaths, false)
}

// summarizeFrameworkCandidatesCensus is the census-aware form. censusEligible
// carries the daemon's attestation that a non-nil scope covers every tracked
// repository (a cold / full-reconciliation batch): the summary then reads the
// RAW node stream and builds the full edge census — census scope and
// execution scope deliberately diverge, execution stays on the scoped store.
func summarizeFrameworkCandidatesCensus(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
	censusEligible bool,
) frameworkCandidateSummary {
	summary := frameworkCandidateSummary{
		all:           map[string]int{},
		scoped:        map[string]int{},
		allMarkers:    map[string]int{},
		scopedMarkers: map[string]int{},
	}
	// fullCensus: the summary may treat the store as fully covered only when
	// there is no exact changed-file frontier. A nil repository scope can mean
	// the empty-prefix single-repository shape; filePaths must still win.
	fullCensus := len(filePaths) == 0 && (scope == nil || censusEligible)
	summary.fullCensus = fullCensus
	var observerRoles map[string]uint8
	observerRolesOverflow := false
	var nodes iter.Seq[*graph.Node]
	if !fullCensus {
		nodes = graph.NodesLightInScopeSeq(g, frameworkScopePrefixes(scope), filePaths)
	} else {
		nodes = graph.NodesLightSeq(g)
	}
	for n := range nodes {
		if n == nil {
			continue
		}
		family := frameworkLanguageFamily(n.Language)
		if fullCensus {
			summary.noteColdCSharpTypeName(n)
		}
		if role := recordFrameworkNodeCandidates(summary.allMarkers, n, family); role != 0 && n.ID != "" {
			if observerRoles == nil {
				observerRoles = map[string]uint8{}
			}
			if len(observerRoles) < frameworkScopeRetainedRowCap {
				observerRoles[n.ID] = role
			} else {
				observerRolesOverflow = true
			}
		}
		if !fullCensus {
			recordFrameworkNodeCandidates(summary.scopedMarkers, n, family)
		}
		if family == "" {
			continue
		}
		summary.all[family]++
		if !fullCensus {
			summary.scoped[family]++
		}
	}
	// Observer synthesis needs registrar and dispatcher methods accessing the
	// same field. When both name vocabularies exist, one metadata-free scan of
	// accesses_field edges proves whether such a channel can exist. This is
	// stricter than two unrelated name hits but retains every candidate the
	// synthesizer itself can consume.
	if observerRolesOverflow {
		// The proof cache is deliberately capped. Overflow cannot prove the
		// pass inert, so retain it; the scoped synthesizer view still bounds
		// its actual candidate rows.
		summary.allMarkers[SynthObserverChannel]++
	} else if summary.allMarkers[frameworkMarkerObserverRegistrar] > 0 &&
		summary.allMarkers[frameworkMarkerObserverDispatcher] > 0 {
		fieldRoles := map[string]uint8{}
		visit := func(e *graph.Edge) bool {
			if e == nil || e.From == "" || e.To == "" {
				return true
			}
			role := observerRoles[e.From]
			if role == 0 {
				return true
			}
			fieldRoles[e.To] |= role
			if fieldRoles[e.To] == (frameworkObserverRegistrarRole | frameworkObserverDispatcherRole) {
				summary.allMarkers[SynthObserverChannel]++
				return false
			}
			return true
		}
		if fullCensus {
			if _, streaming := g.(graph.LightEdgeSequencer); streaming {
				for edge := range graph.EdgesLightSeq(g, graph.EdgeAccessesField) {
					if !visit(edge) {
						break
					}
				}
			} else if light, ok := g.(graph.LightEdgeScanner); ok {
				for _, edge := range light.AllEdgesLight(graph.EdgeAccessesField) {
					if !visit(edge) {
						break
					}
				}
			} else {
				for edge := range g.EdgesByKind(graph.EdgeAccessesField) {
					if !visit(edge) {
						break
					}
				}
			}
		} else {
			if _, streaming := g.(graph.ScopedProjectionSequencer); streaming {
				for row := range graph.EdgesInScopeSeq(
					g, frameworkScopePrefixes(scope), filePaths, graph.EdgeAccessesField,
				) {
					if !visit(row.Edge) {
						break
					}
				}
			} else if light, ok := g.(graph.LightEdgeScanner); ok {
				for _, edge := range light.AllEdgesLight(graph.EdgeAccessesField) {
					if !visit(edge) {
						break
					}
				}
			} else {
				for row := range graph.EdgesInScopeSeq(
					g, frameworkScopePrefixes(scope), filePaths, graph.EdgeAccessesField,
				) {
					if !visit(row.Edge) {
						break
					}
				}
			}
		}
	}
	if fullCensus {
		// Arm the shared-stream collectors with the family / node-marker
		// verdicts the light walk just produced (the census-derived edge
		// preflights are decided by the walk below and can only narrow
		// admission further), then run the census edge walks as the single
		// candidate dispatcher for every armed pass.
		present, markers := summary.all, summary.allMarkers
		if !fullCensus {
			present, markers = summary.scoped, summary.scopedMarkers
		}
		summary.streams = newFrameworkStreamCandidates(g, present, markers)
		summary.edges = collectFrameworkEdgeCensus(g, summary.streams)
	}
	return summary
}

// noteColdCSharpTypeName feeds the receiver-gate tail census: only nodes the
// demote pass itself can index (C# type/interface names) are counted, and
// only up to the two distinct names a demotion minimally requires.
func (s *frameworkCandidateSummary) noteColdCSharpTypeName(n *graph.Node) {
	if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(n.Language), "csharp") {
		return
	}
	s.csharpTypeNames.note(n.Name)
}

const (
	frameworkMarkerObserverRegistrar  = "observer-channel:registrar"
	frameworkMarkerObserverDispatcher = "observer-channel:dispatcher"
	frameworkMarkerSwift              = "swift-objc-bridge:swift"
	frameworkMarkerObjC               = "swift-objc-bridge:objc"
	frameworkMarkerSvelteKitPage      = "sveltekit-load:page"
	frameworkMarkerSvelteKitServer    = "sveltekit-load:server"
	frameworkMarkerPascalSource       = "pascal-form:source"
	frameworkMarkerPascalForm         = "pascal-form:form"
	frameworkMarkerGDScript           = "gdscript:node"
	frameworkMarkerStrictFamilyPrefix = "family-strict:"

	frameworkObserverRegistrarRole  uint8 = 1
	frameworkObserverDispatcherRole uint8 = 2
)

// frameworkSynthNodePreflights names passes with necessary candidate shapes
// visible in the light node projection. Every listed marker is required: a
// missing marker proves the pass cannot produce an edge and avoids its full
// graph scans. These are deliberately one-way gates; a marker hit only keeps
// the pass enabled and does not claim that an edge will be produced.
var frameworkSynthNodePreflights = map[string][]string{
	SynthEventChannel:        {SynthEventChannel},
	SynthSwiftObjC:           {frameworkMarkerSwift, frameworkMarkerObjC},
	SynthObserverChannel:     {SynthObserverChannel},
	SynthReactSetState:       {SynthReactSetState},
	SynthFlutterSetState:     {SynthFlutterSetState},
	SynthMyBatis:             {SynthMyBatis},
	SynthSidekiq:             {SynthSidekiq},
	SynthLaravelEvent:        {SynthLaravelEvent},
	SynthSwiftUIResolve:      {SynthSwiftUIResolve},
	SynthUIKitResolve:        {SynthUIKitResolve},
	SynthVaporResolve:        {SynthVaporResolve},
	SynthCSharpIfaceDispatch: {SynthCSharpIfaceDispatch},
	SynthKMPExpectActual:     {SynthKMPExpectActual},
	SynthMacroExpansion:      {SynthMacroExpansion},
	SynthGoFrameRoute:        {SynthGoFrameRoute},
	SynthSvelteKitLoad:       {frameworkMarkerSvelteKitPage, frameworkMarkerSvelteKitServer},
	SynthPascalFormName:      {frameworkMarkerPascalSource, frameworkMarkerPascalForm},
}

// frameworkSynthFamilyConjunctions strengthens the OR family gate for
// cross-ecosystem bridge passes: every GROUP must have at least one present
// family (OR within a group, AND across groups). A bridge that links two
// sides provably cannot emit when either side's family census is zero.
var frameworkSynthFamilyConjunctions = map[string][][]string{
	SynthExpoModules:     {{"web"}, {"apple", "jvm"}},
	SynthFabric:          {{"web"}, {"apple", "jvm"}},
	SynthReactNativePair: {{"apple"}, {"jvm"}},
	SynthSQLCallsite:     {{"sql"}, {"web", "python"}},
}

// frameworkSynthEdgePreflights names passes whose necessary edge evidence is
// visible in the cold EdgeCalls census. Each predicate is a verbatim copy of
// the pass's own admission filter, evaluated on the same edge stream the
// pass reads: a false result proves the pass cannot land, rebind, or stamp
// an edge on this graph, while true only keeps it enabled. Conjunctive with
// the family and node gates; consulted only when the census saw the full
// stream (nil scope).
//
// rtk-query additionally leans on an extractor invariant: every
// rtk_generated_hook node is minted together with a via=rtk-query
// placeholder in the same extraction, so no via edge implies no generated
// hooks and the hook-name branch is inert too. Pinned by
// TestRTKQueryExtractorMintsHookWithViaPlaceholder.
var frameworkSynthEdgePreflights = map[string]func(frameworkEdgeCensus) bool{
	SynthObjectRegistry:  func(c frameworkEdgeCensus) bool { return c.objectRegistryValue },
	SynthNgRxEffect:      func(c frameworkEdgeCensus) bool { return c.via[ngrxEffectVia] },
	SynthExpressResolve:  func(c frameworkEdgeCensus) bool { return c.expressHandlerRef },
	SynthReduxThunk:      func(c frameworkEdgeCensus) bool { return c.via[reduxThunkVia] },
	SynthLaravelEvent:    func(c frameworkEdgeCensus) bool { return c.via[laravelEventVia] },
	SynthVuexDispatch:    func(c frameworkEdgeCensus) bool { return c.via[vuexDispatchVia] },
	SynthRTKQuery:        func(c frameworkEdgeCensus) bool { return c.via[rtkQueryVia] },
	SynthCelery:          func(c frameworkEdgeCensus) bool { return c.via[celeryVia] },
	SynthSpringEvent:     func(c frameworkEdgeCensus) bool { return c.via[springEventVia] },
	SynthMediatR:         func(c frameworkEdgeCensus) bool { return c.via[mediatrVia] },
	SynthReactSetState:   func(c frameworkEdgeCensus) bool { return c.setStateTarget },
	SynthFlutterSetState: func(c frameworkEdgeCensus) bool { return c.setStateTarget },
	SynthRailsResolve:    func(c frameworkEdgeCensus) bool { return c.recvConst },
	// grpc/temporal were historically ungated: their internal presence
	// probes short-circuit the yield but still pay a full EdgeCalls scan
	// each (measured 52s + 30s for zero edges on a stub-free workspace).
	// The census answers the same predicates in its single shared walk.
	SynthGRPCStub:     func(c frameworkEdgeCensus) bool { return c.grpcStub },
	SynthTemporalStub: func(c frameworkEdgeCensus) bool { return c.temporalVia || c.temporalAnnotation },
}

func recordFrameworkNodeCandidates(markers map[string]int, n *graph.Node, family string) uint8 {
	if isPubsubEventNode(n.ID) || isEmitterEventNode(n.ID) {
		markers[SynthEventChannel]++
	}

	language := strings.ToLower(strings.TrimSpace(n.Language))
	if strict := languageFamily(language); strict != "" {
		markers[frameworkMarkerStrictFamilyPrefix+strict]++
	}
	switch language {
	case "swift":
		markers[frameworkMarkerSwift]++
	case "objc", "objective-c", "objectivec":
		markers[frameworkMarkerObjC]++
	case "mybatis":
		if n.Kind == graph.KindMethod {
			markers[SynthMyBatis]++
		}
	case "ruby":
		if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) && n.Name == "perform" {
			markers[SynthSidekiq]++
		}
	case "php":
		if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) && n.Name == "handle" {
			markers[SynthLaravelEvent]++
		}
	case "gdscript":
		// The receiver gate reads only GDScript nodes, so a corpus with
		// none provably yields no demotion — and on a scoped run, a
		// resolve that touched no GDScript file cannot have changed any
		// verdict, since both the call edges and the class index it
		// judges against come from `.gd` files.
		markers[frameworkMarkerGDScript]++
	case "csharp":
		if n.Kind == graph.KindInterface {
			markers[SynthCSharpIfaceDispatch]++
		}
	case "kotlin":
		markers[SynthKMPExpectActual]++
	}
	if n.Kind == graph.KindMacro {
		markers[SynthMacroExpansion]++
	}
	// GoFrame route contracts are minted with a stable id namespace; any id
	// carrying it (bare or repo-prefixed) proves route candidates exist.
	if strings.Contains(n.ID, "route::goframe::") {
		markers[SynthGoFrameRoute]++
	}
	// SvelteKit load synthesis needs both a +page/+layout consumer and a
	// server-load producer file; Pascal form binding needs both a source
	// unit and a form file. Path/extension shapes are visible in the light
	// projection.
	if strings.Contains(n.FilePath, "+page.") || strings.Contains(n.FilePath, "+layout.") {
		markers[frameworkMarkerSvelteKitPage]++
		if strings.Contains(n.FilePath, "+page.server.") || strings.Contains(n.FilePath, "+layout.server.") {
			markers[frameworkMarkerSvelteKitServer]++
		}
	}
	if n.Kind == graph.KindFile {
		switch strings.ToLower(filepath.Ext(n.FilePath)) {
		case ".pas", ".pp", ".dpr", ".lpr":
			markers[frameworkMarkerPascalSource]++
		case ".dfm", ".lfm", ".fmx":
			markers[frameworkMarkerPascalForm]++
		}
	}

	observerRole := uint8(0)
	if (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) && n.Name != "" {
		if observerRegistrarRe.MatchString(n.Name) {
			markers[frameworkMarkerObserverRegistrar]++
			observerRole |= frameworkObserverRegistrarRole
		}
		if observerDispatcherRe.MatchString(n.Name) {
			markers[frameworkMarkerObserverDispatcher]++
			observerRole |= frameworkObserverDispatcherRole
		}
	}
	if n.Kind == graph.KindMethod {
		switch n.Name {
		case "render":
			markers[SynthReactSetState]++
		case "build":
			markers[SynthFlutterSetState]++
		}
	}
	if family != "apple" {
		return observerRole
	}
	name := n.Name
	path := strings.ToLower(strings.ReplaceAll(n.FilePath, "\\", "/"))
	if strings.HasSuffix(name, "ViewModel") || strings.HasSuffix(name, "View") ||
		strings.HasSuffix(name, "Store") || strings.HasSuffix(name, "Manager") ||
		strings.Contains(path, "/models/") || strings.Contains(path, "/model/") {
		markers[SynthSwiftUIResolve]++
	}
	if strings.HasSuffix(name, "ViewController") || strings.HasSuffix(name, "Cell") ||
		strings.HasSuffix(name, "Delegate") || strings.HasSuffix(name, "DataSource") {
		markers[SynthUIKitResolve]++
	}
	if (strings.HasSuffix(name, "Controller") && !strings.HasSuffix(name, "ViewController")) ||
		strings.HasSuffix(name, "Middleware") || strings.Contains(path, "/models/") ||
		strings.Contains(path, "/model/") {
		markers[SynthVaporResolve]++
	}
	return observerRole
}

func frameworkLanguageFamily(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if family := languageFamily(language); family != "" {
		return family
	}
	switch language {
	case "go", "golang":
		return "go"
	case "rust":
		return "rust"
	case "python", "py":
		return "python"
	case "ruby":
		return "ruby"
	case "php":
		return "php"
	case "dart":
		return "dart"
	case "sql":
		return "sql"
	case "pascal", "object-pascal", "object_pascal":
		return "pascal"
	case "vue", "svelte", "astro":
		return "web"
	case "objective-c++", "objc++", "objective-cpp":
		return "apple"
	default:
		return ""
	}
}

func frameworkSynthUsesScopedCandidates(s FrameworkSynthesizer, scope map[string]bool) bool {
	if scope == nil {
		return false
	}
	// Every synthFunc uses either its explicit scoped implementation or the
	// bounded frameworkScopedStore candidate view.
	if _, ok := s.(synthFunc); ok {
		return true
	}
	_, ok := s.(scopedSynthesizer)
	return ok
}

func shouldRunFrameworkSynthesizer(s FrameworkSynthesizer, scope map[string]bool, summary frameworkCandidateSummary) bool {
	present := summary.all
	markers := summary.allMarkers
	if frameworkSynthUsesScopedCandidates(s, scope) {
		present = summary.scoped
		markers = summary.scopedMarkers
	}
	if !frameworkSynthNodeGatesPass(s.Name(), present, markers) {
		return false
	}
	if summary.edges.valid {
		if preflight := frameworkSynthEdgePreflights[s.Name()]; preflight != nil && !preflight(summary.edges) {
			return false
		}
	}
	return true
}

// frameworkSynthNodeGatesPass evaluates the family / conjunction /
// node-marker gates for one pass against the chosen census maps — the
// sub-verdict of shouldRunFrameworkSynthesizer that is already known after
// the light node walk, before the edge census runs. The shared-stream
// dispatcher arms a pass's candidate collectors on exactly this verdict, so
// its armed set is a superset of the passes the full gate will admit.
func frameworkSynthNodeGatesPass(name string, present, markers map[string]int) bool {
	if families := frameworkSynthLanguageFamilies[name]; len(families) > 0 {
		found := false
		for _, family := range families {
			if present[family] > 0 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, groups := range frameworkSynthFamilyConjunctions[name] {
		found := false
		for _, family := range groups {
			if present[family] > 0 {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, marker := range frameworkSynthNodePreflights[name] {
		if markers[marker] == 0 {
			return false
		}
	}
	return true
}

// defaultFrameworkSynthesizers returns the registered framework
// synthesizers in run order. Order is load-bearing: every synthesizer
// here runs after InferImplements/InferOverrides (some depend on the
// EdgeImplements edges they produce) and before DetectCrossRepoEdges (so
// a cross-repo synthesized call gets its parallel cross_repo_calls edge).
// Native-bridge resolvers append to this slice.
func defaultFrameworkSynthesizers() []FrameworkSynthesizer {
	return []FrameworkSynthesizer{
		synthFunc{name: SynthGRPCStub, fn: ResolveGRPCStubCalls, candFn: resolveGRPCStubCalls},
		synthFunc{name: SynthTemporalStub, fn: ResolveTemporalCalls, candFn: resolveTemporalCalls},
		synthFunc{name: SynthEventChannel, fn: ResolveEventChannelCalls},
		synthFunc{name: SynthSwiftObjC, fn: ResolveSwiftObjCBridge},
		synthFunc{name: SynthReactNative, fn: ResolveReactNativeBridge},
		synthFunc{name: SynthReactNativePair, fn: ResolveReactNativeNativePairing},
		synthFunc{name: SynthObserverChannel, fn: ResolveObserverChannelCalls},
		synthFunc{name: SynthClosureCollection, fn: ResolveClosureCollectionCalls},
		synthFunc{name: SynthReactSetState, fn: ResolveReactSetStateCalls},
		synthFunc{name: SynthFlutterSetState, fn: ResolveFlutterSetStateCalls},
		synthFunc{name: SynthKMPExpectActual, fn: ResolveKMPExpectActual},
		synthFunc{name: SynthExpoModules, fn: ResolveExpoModuleBridge},
		synthFunc{name: SynthFabric, fn: ResolveFabricComponents},
		synthFunc{name: SynthMyBatis, fn: ResolveMyBatisCalls},
		synthFunc{name: SynthSQLCallsite, fn: ResolveSQLCallsites},
		// Store-factory (Zustand/Redux/Pinia/MobX) indirect action calls —
		// binds getState()-chain and destructured calls to the action node.
		synthFunc{name: SynthStoreFactory, fn: ResolveStoreFactoryCalls, candFn: resolveStoreFactoryCalls},
		// Redux Toolkit createAsyncThunk dispatch chains: a thunk →
		// each action/thunk it dispatches from its payload-creator body.
		// After store-factory so its action nodes are indexed for the
		// thunk → reducer cross-link.
		synthFunc{name: SynthReduxThunk, fn: ResolveReduxThunkCalls},
		// NgRx effects: a createEffect(() => actions$.pipe(ofType(X))) effect ->
		// the action X it reacts to. After the store/thunk passes so action
		// creator nodes are indexed.
		synthFunc{name: SynthNgRxEffect, fn: ResolveNgRxEffects},
		// Object-literal command/handler registry dispatch →
		// `new registry[key]().execute()`. Runs before the speculative
		// pass so a claimed dispatch site suppresses the hidden best-guess.
		synthFunc{name: SynthObjectRegistry, fn: ResolveObjectRegistryCalls},
		// RTK Query generated-hook → createApi endpoint, and component →
		// generated hook. Typed tier: the hook naming is RTK-contractual.
		synthFunc{name: SynthRTKQuery, fn: ResolveRTKQueryCalls},
		// Vuex string-keyed dispatch/commit → action/mutation, with
		// module-namespace disambiguation.
		synthFunc{name: SynthVuexDispatch, fn: ResolveVuexDispatchCalls},
		// Celery task dispatch: `task.delay()` / `send_task("name")` →
		// the decorator-gated task function. Typed tier.
		synthFunc{name: SynthCelery, fn: ResolveCeleryCalls},
		// Spring application events: publishEvent(new X()) → every
		// @EventListener / ApplicationListener<X>, type-keyed fan-out.
		synthFunc{name: SynthSpringEvent, fn: ResolveSpringEventCalls},
		// MediatR CQRS dispatch: Send(new X()) → the IRequestHandler<X>
		// Handle, Publish(new X()) → every INotificationHandler<X>.
		synthFunc{name: SynthMediatR, fn: ResolveMediatRCalls},
		// Autofac AsImplementedInterfaces(): expand each extractor-emitted
		// marker into one useClass provides binding per interface the impl
		// declares. After the implements-producing passes so the join reads
		// the settled hierarchy.
		synthFunc{name: SynthCSharpDIIfaces, fn: ResolveCSharpDIImplementedInterfaces, scopedFn: ResolveCSharpDIImplementedInterfacesScoped},
		// C# member-level interface dispatch: a call bound to an interface
		// member fans out to the same-named member on each in-repo
		// implementation, at the ast_inferred tier so it rides in the default
		// find_usages / get_callers result. After the implements-producing
		// passes so the impl fan-out is complete.
		synthFunc{name: SynthCSharpIfaceDispatch, fn: ResolveCSharpInterfaceDispatch, scopedFn: ResolveCSharpInterfaceDispatchScoped},
		// csharp-efcore-models joins the extractor's EF Core facts
		// (attribute models_table edges, ef_config_*/ef_fluent stamps)
		// into the models_table layer. Position-independent: it reads
		// only its own stamps and models_table itself, and no other
		// pass consumes models_table during synthesis.
		synthFunc{name: SynthCSharpEFCoreModels, fn: ResolveCSharpEFCoreModels, scopedFn: ResolveCSharpEFCoreModelsScoped},
		// Sidekiq job dispatch: Worker.perform_async(...) → the worker's
		// perform, namespace-aware. Include-gated, typed tier.
		synthFunc{name: SynthSidekiq, fn: ResolveSidekiqCalls},
		// Laravel events: event(new X()) / X::dispatch() → every listener
		// handle(X), from the Listeners convention and the $listen map.
		synthFunc{name: SynthLaravelEvent, fn: ResolveLaravelEventCalls},
		// C/C++ function-pointer dispatch: a fn registered into a struct's
		// fn-pointer field → the indirect recv->field() call, keyed by
		// (struct type, field) with a field-copy fixpoint.
		synthFunc{name: SynthFnPointerDispatch, fn: ResolveFnPointerDispatch, candFn: resolveFnPointerDispatch},
		// C/C++ function-like macro expansion: a macro invocation
		// `CALL_M(o)` → each call hidden in the macro's replacement list,
		// attributed to the use-site line so a forward call walk shows the
		// call where the macro is invoked, not at its `#define`.
		synthFunc{name: SynthMacroExpansion, fn: ResolveMacroExpansionCalls, candFn: resolveMacroExpansionCalls},
		// Gin middleware-chain dispatcher → registered handlers. Bridges the
		// `c.handlers[idx](c)` indirection so ServeHTTP→handler reachability
		// flows; repo-scoped, gated on a dispatcher existing.
		synthFunc{name: SynthGinMiddleware, fn: ResolveGinMiddlewareCalls},
		// Express named-handler resolution: middleware idents and
		// XController.method args bound by directory convention.
		synthFunc{name: SynthExpressResolve, fn: ResolveExpressHandlers},
		// React custom-hook / context resolution: a `useAuth()` call binds to
		// its /hooks/ definition; a `*Context`/`*Provider` reference binds to
		// /context/ or /providers/, with the suffix-strip fallback.
		synthFunc{name: SynthReactResolve, fn: ResolveReactHooksContext, candFn: resolveReactHooksContext},
		// FastAPI dependency / router fallback: a residual `Depends(get_db)`
		// binds to a /dependencies/ provider, an `include_router(api_router)`
		// to a /routers/ definition — only when reference resolution left the
		// target unresolved.
		synthFunc{name: SynthFastAPIResolve, fn: ResolveFastAPIDeps, candFn: resolveFastAPIDeps},
		// Rails receiver-constant resolution: a `UserService.perform` /
		// `User.find` / `ApplicationHelper.fmt` call binds to the directory-
		// located service / model / helper definition named by its receiver.
		synthFunc{name: SynthRailsResolve, fn: ResolveRailsRefs, candFn: resolveRailsRefs},
		// Godot autoload resolution: a `Game.set_speed()` call binds to
		// the script `project.godot` declares the singleton `Game` to
		// be — the only place that mapping exists.
		synthFunc{name: SynthGodotAutoload, fn: ResolveGDScriptAutoloads},
		// Godot preload aliases: a `const Persist = preload("res://…")`
		// names a script file-locally, and `Persist.snapshot()` is then
		// used exactly like a global — the standard way to reach a
		// static-utility script that declares no `class_name`.
		synthFunc{name: SynthGodotPreloadAlias, fn: ResolveGDScriptPreloadAliases},
		// Godot scene connections: a `.tscn` `[connection … method="…"]`
		// block wires a signal to a method of the script attached to the
		// target node. The method is named as a bare string, so nothing in
		// the scripts records the link.
		synthFunc{name: SynthGodotConnection, fn: ResolveGodotSceneConnections},
		// SwiftUI directory-convention fallback: a residual `*View` /
		// `*ViewModel` / `*Store` / `*Manager` / PascalCase-model reference
		// binds to its /Views/ /ViewModels/ /Stores/ /Models/ definition.
		synthFunc{name: SynthSwiftUIResolve, fn: ResolveSwiftUIRefs},
		// UIKit directory-convention fallback: a residual `*ViewController` /
		// `*Cell` / `*Delegate` / `*DataSource` reference binds to its
		// /ViewControllers/ /Cells/ /Delegates/ definition.
		synthFunc{name: SynthUIKitResolve, fn: ResolveUIKitRefs, candFn: resolveUIKitRefs},
		// Vapor directory-convention fallback: a residual `*Controller` /
		// `*Middleware` reference binds to its /Controllers/ /Middleware/
		// definition. After UIKit so `*ViewController` binds there first.
		synthFunc{name: SynthVaporResolve, fn: ResolveVaporRefs},
		// GoFrame reflective route → controller method, joined by the
		// method's request-struct type rather than its name.
		synthFunc{name: SynthGoFrameRoute, fn: ResolveGoFrameRoutes},
		// SvelteKit +page ↔ +page.server load pairing: a route's page component
		// reaches its server data loader so a trace flows page→load. Repo-scoped.
		synthFunc{name: SynthSvelteKitLoad, fn: ResolveSvelteKitLoad},
		// Rust impl-block / self-receiver / module-path resolution
		// completion. Runs in the same settle window so residual
		// unresolved Rust calls land before external-call synthesis
		// classifies the rest as external.
		synthFunc{name: SynthRustScope, fn: ResolveRustScopeCalls, candFn: resolveRustScopeCalls},
		// After rust-scope and the implements/extends-producing passes so the
		// cross-file factory-chain walk + conformance hop see settled edges.
		synthFunc{name: SynthFactoryChain, fn: ResolveFactoryChains, candFn: resolveFactoryChains},
		// Function-as-value callback registration — binds each captured
		// value-position function identifier to its same-file definition and
		// drops unbound candidates. The per-language capture feeds it via
		// placeholder edges; the pass is inert until those land.
		synthFunc{name: SynthFnValue, fn: ResolveFnValueCallbacks},
		// Pascal unit ↔ form (.pas/.dfm) pairing by same-dir basename.
		synthFunc{name: SynthPascalFormName, fn: ResolvePascalForms},
		// Same-file distinctive value references → EdgeReads to the constant,
		// so a config constant's blast radius reaches every reader.
		synthFunc{name: SynthValueRefName, fn: ResolveValueRefs},
	}
}

// SynthCount is the per-synthesizer result row in a FrameworkSynthReport.
type SynthCount struct {
	Name string `json:"name"`
	// Edges is the synthesizer's legacy attempted/landed count. Some passes
	// include already-persisted idempotent results, so it is not an inserted-row
	// count; the name remains for API compatibility.
	Edges int `json:"edges"`
	// Millis is how long this synthesizer's Synthesize call took. Named
	// passes that land 0 edges are not free — many scan a shared edge/node
	// kind across the whole graph before concluding there is nothing to
	// bind — so this rides on every row, not just the ones with edges.
	Millis int64 `json:"ms,omitempty"`
	// ScopeRows/ScopeBytes expose the bounded seed plus this pass's private
	// dependency expansion. They make accidental cross-pass widening visible
	// without retaining candidate objects after the pass completes.
	ScopeRows  int `json:"scope_rows,omitempty"`
	ScopeBytes int `json:"scope_bytes,omitempty"`
}

// FrameworkSynthReport is the aggregate result of one
// RunFrameworkSynthesizers invocation.
type FrameworkSynthReport struct {
	// Total preserves the legacy sum of per-pass attempted/landed counts. It is
	// not a durable inserted-edge delta; callers should label it accordingly.
	Total int          `json:"total"`
	Per   []SynthCount `json:"per_synthesizer"`
	// Gated counts synthesized reference/import edges dropped by the
	// cross-language-family gate (coincidental PascalCase collisions across
	// two known, different families; bridge synthesizers are exempt).
	Gated int `json:"gated_cross_family,omitempty"`
	// ReceiverGated counts C# member-call edges demoted to the speculative
	// tier because they attach to a same-named member of a type unrelated to
	// the edge's receiver_type.
	ReceiverGated int `json:"receiver_type_gated,omitempty"`
	// GDScriptReceiverGated counts the same demotion for GDScript, where the
	// receiver is stated at every call site and a same-named method in the
	// caller's own directory is the standing phantom.
	GDScriptReceiverGated int `json:"gdscript_receiver_type_gated,omitempty"`
	// GateMillis/ClaimMillis/DemoteMillis time the three tail passes that
	// run once (not per-synthesizer) after the main loop, so a slow one
	// doesn't hide behind the loop's aggregate elapsed.
	GateMillis   int64 `json:"gate_ms,omitempty"`
	ClaimMillis  int64 `json:"claim_ms,omitempty"`
	DemoteMillis int64 `json:"demote_ms,omitempty"`
	// CensusMillis times the admission census that runs before the loop. It
	// walks the node stream plus the cold EdgeCalls census; against a
	// checkpointed store it costs ~11s, but a measured cold run spent ~533s
	// here with every synthesizer gated to zero — the census, not the
	// synthesizers, owned the pass. It must never be silent again.
	CensusMillis int64 `json:"census_ms,omitempty"`
	// ScopeMillis times the scoped-store seed construction between the census
	// and the loop. ScopeRows/ScopeBytes describe that immutable seed; each
	// SynthCount then reports the seed plus only its private expansion.
	ScopeMillis int64 `json:"scope_ms,omitempty"`
	ScopeRows   int   `json:"scope_rows,omitempty"`
	ScopeBytes  int   `json:"scope_bytes,omitempty"`
	// FullReadCache reports the bounded immutable node/member projection cache
	// used only by cold/full executions. Scoped incremental passes keep their
	// exact frontier store and leave this zero-valued.
	FullReadCache FrameworkFullReadCacheStats `json:"full_read_cache,omitempty"`
}

// scopedSynthesizer is the optional capability a FrameworkSynthesizer exposes
// when it can restrict its candidate scan to a changed-repo prefix set. The
// driver consults it only when a scope is armed; a synthesizer that does not
// implement it runs whole-graph, which is always correct.
type scopedSynthesizer interface {
	synthesizeScoped(g graph.Store, scope map[string]bool) int
}

// RunFrameworkSynthesizers runs every registered framework synthesizer
// over g, in registration order, and returns the per-synthesizer and
// total landed-edge counts. A nil graph is a no-op.
func RunFrameworkSynthesizers(g graph.Store) FrameworkSynthReport {
	return RunFrameworkSynthesizersScoped(g, nil)
}

// RunFrameworkSynthesizersScoped is RunFrameworkSynthesizers with an armed
// changed-repo scope: each synthesizer that implements scopedSynthesizer
// narrows its candidate scan to those repos, the rest run whole-graph. A nil
// scope uses the whole-graph candidate census; language-specific passes proven
// irrelevant are skipped, while generic or unaudited passes always run. The
// claiming-resolver, family-gate, C# interface-dispatch, and receiver-gate tail
// passes use the changed repositories plus their exact reverse dependency
// frontier; nil scope retains full/cold whole-graph reconciliation.
func RunFrameworkSynthesizersScoped(g graph.Store, scope map[string]bool) FrameworkSynthReport {
	return runFrameworkSynthesizersScoped(g, scope, nil, false, true)
}

// RunFrameworkSynthesizersScopedWithCensus is the full-coverage batch form:
// the caller (the daemon's cold / full-reconciliation warmup) attests that
// the scope covers every tracked repository, so the admission census may be
// built from the RAW whole store even though synthesizer execution keeps the
// scoped view. The attestation must come from the repo registry's owner —
// it is never inferred here from the scope's size.
func RunFrameworkSynthesizersScopedWithCensus(
	g graph.Store,
	scope map[string]bool,
	censusEligible bool,
) FrameworkSynthReport {
	return runFrameworkSynthesizersScoped(g, scope, nil, censusEligible, true)
}

// RunFrameworkSynthesizersScopedForFiles is the exact incremental form. The
// changed-file frontier owns candidate scans; incident incoming edges and exact
// name dependencies are admitted by frameworkScopedStore so target-side edits
// reconcile without widening to the repository corpus.
func RunFrameworkSynthesizersScopedForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
	csharpHierarchyChanged bool,
) FrameworkSynthReport {
	return runFrameworkSynthesizersScoped(g, scope, filePaths, false, csharpHierarchyChanged)
}

func frameworkScopeForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
) map[string]bool {
	if scope != nil || g == nil || len(filePaths) == 0 {
		return scope
	}
	recovered := map[string]bool{}
	for _, nodes := range g.GetFileNodesByPaths(filePaths) {
		for _, node := range nodes {
			if node != nil {
				recovered[node.RepoPrefix] = true
			}
		}
	}
	return recovered
}

func runFrameworkSynthesizersScoped(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
	censusEligible bool,
	csharpHierarchyChanged bool,
) FrameworkSynthReport {
	rep := FrameworkSynthReport{}
	if g == nil {
		return rep
	}
	// A changed-file frontier is always partial, even when a legacy caller lost
	// its repository prefix (notably the empty-prefix single-repository shape).
	// Recover the owning prefixes from the exact file rows so tail claimers keep
	// their repository boundary without promoting the census to a global scan.
	effectiveScope := frameworkScopeForFiles(g, scope, filePaths)
	censusStart := time.Now()
	candidates := summarizeFrameworkCandidatesCensus(g, effectiveScope, filePaths, censusEligible)
	rep.CensusMillis = time.Since(censusStart).Milliseconds()

	// A full-census attestation means the supplied repository scope covers the
	// entire store. Execute it through the established nil-scope paths: bespoke
	// scoped synthesizers and tail passes otherwise turn the all-repository set
	// into repeated repo -> node -> edge probes. True partial runs retain their
	// repository and file frontier unchanged.
	executionScope := effectiveScope
	if candidates.fullCensus {
		executionScope = nil
	}

	scopeStart := time.Now()
	var genericSeed *frameworkScopedSeed
	var fullReadCache *frameworkFullReadCache
	if executionScope != nil {
		genericSeed = newFrameworkScopedSeed(g, executionScope, filePaths)
		rep.ScopeRows = genericSeed.retainedRows
		rep.ScopeBytes = genericSeed.retainedBytes
	} else {
		// Node declarations are immutable throughout the framework registry.
		// Share their decoded projections across passes under a hard run-local
		// budget; the per-pass edge facade invalidates on any future node/member
		// mutation so this optimization cannot return stale declaration state.
		fullReadCache = newFrameworkFullReadCache()
	}
	rep.ScopeMillis = time.Since(scopeStart).Milliseconds()
	for _, s := range defaultFrameworkSynthesizers() {
		start := time.Now()
		var n int
		var bundle *frameworkPassCandidates
		var passScope *frameworkScopedStore
		if shouldRunFrameworkSynthesizer(s, executionScope, candidates) {
			if sf, ok := s.(synthFunc); ok {
				bundle = candidates.streams.passStreams(g, sf.name)
				switch {
				case sf.candFn != nil && bundle != nil:
					// Shared-stream form. streams exist only on a full-census
					// run, where the execution store is the raw g for both
					// the nil-scope and the attested full-coverage shapes.
					n = runLegacyFrameworkSynthWithCache(g, fullReadCache, func(store graph.Store) int {
						return sf.candFn(store, bundle)
					})
				case executionScope == nil:
					n = runLegacyFrameworkSynthWithCache(g, fullReadCache, sf.fn)
				case sf.scopedFn != nil:
					n = sf.scopedFn(g, executionScope)
				default:
					passScope = genericSeed.newPassStore()
					n = runLegacyFrameworkSynth(passScope, sf.fn)
				}
			} else if ss, ok := s.(scopedSynthesizer); ok {
				n = runLegacyFrameworkSynthWithCache(g, fullReadCache, func(store graph.Store) int {
					return ss.synthesizeScoped(store, executionScope)
				})
			} else if executionScope == nil {
				n = runLegacyFrameworkSynthWithCache(g, fullReadCache, s.Synthesize)
			} else {
				panic("framework partial run has an unscoped synthesizer: " + s.Name())
			}
		}
		count := SynthCount{Name: s.Name(), Edges: n, Millis: time.Since(start).Milliseconds()}
		if passScope != nil {
			stats := passScope.stats()
			count.ScopeRows = stats.RetainedRows
			count.ScopeBytes = stats.RetainedBytes
		}
		rep.Per = append(rep.Per, count)
		rep.Total += n
		candidates.streams.releasePass(s.Name(), bundle)
	}
	// Capture observability before dropping the run-local cache. Tail gates and
	// claiming resolvers use different projections and must not prolong its
	// lifetime beyond the serial framework registry.
	rep.FullReadCache = fullReadCache.stats()
	fullReadCache = nil
	// The registry consumed or discarded every armed buffer. Release the
	// shared node snapshot before the independent tail gates and claimers run.
	candidates.streams = nil
	// Drop coincidental cross-language-family reference/import results before
	// the claiming resolvers run, so a gated edge cannot be mistaken for a
	// resolved placeholder downstream. Bridge synthesizers are exempt.
	gateStart := time.Now()
	if frameworkFamilyGateNeeded(executionScope, candidates) {
		rep.Gated = applyFrameworkFamilyGateScopedForFiles(g, executionScope, filePaths)
	}
	rep.GateMillis = time.Since(gateStart).Milliseconds()
	// Claiming resolvers run last — after every framework synthesizer has
	// had its chance to consume a pre-stamped placeholder, but before
	// external-call synthesis classifies the residual unresolved refs as
	// external. Reported in registration order for determinism.
	claimStart := time.Now()
	claimed := RunClaimingResolversScoped(g, executionScope)
	rep.ClaimMillis = time.Since(claimStart).Milliseconds()
	for _, r := range defaultClaimingResolvers() {
		n := claimed[r.Name()]
		rep.Per = append(rep.Per, SynthCount{Name: r.Name(), Edges: n})
		rep.Total += n
	}
	// Receiver-type gate runs last: it corrects (demotes) already-bound C#
	// member calls, so it must see the settled call graph.
	demoteStart := time.Now()
	if frameworkReceiverGateNeeded(executionScope, candidates) {
		rep.ReceiverGated = demoteCSharpMisattributedMemberCallsScopedForFiles(
			g, executionScope, filePaths, csharpHierarchyChanged,
		)
	}
	// The GDScript gate runs in the same slot and for the same reason: it
	// corrects already-bound member calls, so it must see the settled call
	// graph — after the Godot autoload / preload-alias binders above have
	// had their chance to claim an edge the gate would otherwise judge.
	if frameworkGDScriptGateNeeded(candidates) {
		rep.GDScriptReceiverGated = DemoteGDScriptReceiverMismatches(g)
	}
	rep.DemoteMillis = time.Since(demoteStart).Milliseconds()
	return rep
}

// frameworkStrictFamilies enumerates every non-empty languageFamily value.
// The census records one strict-family marker per node so the tail gates can
// count how many distinct families are present; keep in sync with
// languageFamily's switch arms.
var frameworkStrictFamilies = []string{"jvm", "apple", "web", "c", "dotnet"}

// frameworkFamilyGateNeeded reports whether the cross-family gate can drop
// an edge. A drop requires both endpoint nodes to map to two different
// non-empty strict families, and the cold census walks every node — so
// fewer than two distinct strict families proves the gate returns zero on
// any graph. Uses the strict languageFamily marker, not the framework
// family census: summary.all["web"] can be satisfied entirely by
// vue/svelte nodes whose strict family is empty and which can never
// trigger a drop. Scoped runs always run the gate — a scoped census does
// not walk off-scope endpoint nodes.
func frameworkFamilyGateNeeded(_ map[string]bool, summary frameworkCandidateSummary) bool {
	if !summary.fullCensus {
		return true
	}
	distinct := 0
	for _, family := range frameworkStrictFamilies {
		if summary.allMarkers[frameworkMarkerStrictFamilyPrefix+family] > 0 {
			distinct++
		}
	}
	return distinct >= 2
}

// frameworkReceiverGateNeeded reports whether the C# receiver gate can
// demote an edge. A demotion requires the edge's receiver_type and the
// target's receiver to be two different names, each resolving to an indexed
// C# type/interface node — so a cold census with fewer than two distinct C#
// type/interface names proves the gate returns zero on any graph. Scoped
// runs always run the gate — the gate's name index is whole-graph while a
// scoped census is not.
// frameworkGDScriptGateNeeded reports whether the GDScript receiver gate
// can change anything. Unlike the family/receiver tails, a scoped census
// is enough here: the gate reads only GDScript nodes and edges, so a
// resolve whose candidate set contains no GDScript node cannot have moved
// a verdict. The gate runs whole-graph when it runs, so keeping it off a
// resolve that touched no `.gd` file is what keeps a warm restart on a
// polyglot workspace from paying for it.
func frameworkGDScriptGateNeeded(summary frameworkCandidateSummary) bool {
	if summary.fullCensus {
		return summary.allMarkers[frameworkMarkerGDScript] > 0
	}
	return summary.scopedMarkers[frameworkMarkerGDScript] > 0
}

func frameworkReceiverGateNeeded(_ map[string]bool, summary frameworkCandidateSummary) bool {
	if !summary.fullCensus {
		return true
	}
	return summary.csharpTypeNames.count >= 2
}

// ClaimingResolver retroactively claims a residual unresolved reference —
// one naming no declared symbol — that the extractor could not pre-tag, and
// rewrites it to a framework-known target. This is the generic
// claimsReference hook: a resolver offers a cheap name-vocabulary pre-filter
// (Claims) and, when it wins, rebinds the edge (Resolve). It runs before
// external-call synthesis would otherwise discard the reference as external.
type ClaimingResolver interface {
	// Name is the stable provenance label stamped on the rebound edge.
	Name() string
	// Claims reports whether this resolver wants the unresolved edge — a
	// cheap pre-filter on the reference's vocabulary, no graph work.
	Claims(e *graph.Edge) bool
	// Resolve rebinds e.To to a concrete target, returning true on a hit.
	Resolve(g graph.Store, e *graph.Edge) bool
}

// batchClaimingResolver is the set-oriented form used by the framework tail.
// The returned map contains only edges actually rebound by this resolver.
type batchClaimingResolver interface {
	ResolveBatch(g graph.Store, edges []*graph.Edge) map[*graph.Edge]bool
}

// claimTargetVocabulary is the optional admission probe a ClaimingResolver
// exposes. RequiredTargetNames lists the node names the resolver binds
// claims to; AdmitsTarget reports whether one indexed node retrieved for
// such a name satisfies the resolver's bind-time shape. Before the tail
// pays the unresolved-edge collection scan, each resolver's vocabulary is
// probed against the live name index — the same index the resolver reads at
// bind time — so an absent vocabulary proves the resolver cannot claim on
// this graph. A resolver without the interface is always admissible.
type claimTargetVocabulary interface {
	RequiredTargetNames() []string
	AdmitsTarget(n *graph.Node) bool
}

// claimEdgeVocabulary exhaustively enumerates the unresolved names a resolver
// can claim. Scoped runs use these names for reverse-index lookups; full runs
// filter the bounded unresolved-identity scan before exact row refetch. Neither
// path decodes unrelated call/reference payloads. Resolvers with an unbounded
// vocabulary must omit this interface so discovery fails open to the complete
// unresolved-edge scan.
type claimEdgeVocabulary interface {
	ClaimedUnresolvedNames() []string
}

// RequiredTargetNames: a Django descriptor claim binds only to an indexed
// __iter__ method; without one, ResolveBatch resolves nothing.
func (DjangoDescriptorResolver) RequiredTargetNames() []string {
	return []string{"__iter__"}
}

func (DjangoDescriptorResolver) ClaimedUnresolvedNames() []string {
	return []string{"_iterable_class", "*._iterable_class"}
}

// AdmitsTarget reports whether an __iter__ candidate has the method shape
// ResolveBatch indexes into iterMethods.
func (DjangoDescriptorResolver) AdmitsTarget(n *graph.Node) bool {
	return n != nil && n.Kind == graph.KindMethod
}

// claimingResolverAdmissible evaluates a resolver's declared vocabulary with
// one indexed name lookup. An inadmissible resolver cannot claim any edge,
// so dropping it leaves every other resolver's candidate set unchanged.
func claimingResolverAdmissible(g graph.Store, r ClaimingResolver) bool {
	vocab, ok := r.(claimTargetVocabulary)
	if !ok {
		return true
	}
	names := vocab.RequiredTargetNames()
	if len(names) == 0 {
		return true
	}
	for _, nodes := range g.FindNodesByNames(names) {
		for _, n := range nodes {
			if n != nil && vocab.AdmitsTarget(n) {
				return true
			}
		}
	}
	return false
}

func scopedClaimingCandidates(g graph.Store, scope map[string]bool, resolvers []ClaimingResolver) []*graph.Edge {
	names := make([]string, 0, len(resolvers))
	seenNames := make(map[string]struct{}, len(resolvers))
	for _, resolver := range resolvers {
		vocabulary, ok := resolver.(claimEdgeVocabulary)
		if !ok {
			return unresolvedClaimingCandidates(g, scope)
		}
		for _, name := range vocabulary.ClaimedUnresolvedNames() {
			if name == "" {
				continue
			}
			if _, duplicate := seenNames[name]; duplicate {
				continue
			}
			seenNames[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}

	materialize := func(identities []graph.EdgeIdentity) []*graph.Edge {
		current := findFrameworkEdgesByIdentities(g, identities)
		pending := make([]*graph.Edge, 0, len(identities))
		for _, identity := range identities {
			edge := current[identity]
			if claimingCandidateInScope(edge, scope, resolvers) {
				pending = append(pending, edge)
			}
		}
		return pending
	}

	if scope == nil {
		scanner, ok := g.(graph.UnresolvedEdgeIdentityBatchScanner)
		if !ok {
			return unresolvedClaimingCandidates(g, scope)
		}
		identities := make([]graph.EdgeIdentity, 0)
		scanner.ScanUnresolvedEdgeIdentitiesBatched(
			[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, 512,
			func(batch []graph.EdgeIdentity) bool {
				for _, identity := range batch {
					if _, claimedName := seenNames[graph.UnresolvedName(identity.To)]; claimedName {
						identities = append(identities, identity)
					}
				}
				return true
			},
		)
		return materialize(identities)
	}

	prefixes := frameworkScopePrefixes(scope)
	targetIDs := make([]string, 0, len(names)*(len(prefixes)+1))
	for _, name := range names {
		targetIDs = append(targetIDs, graph.UnresolvedMarker+name)
		for _, prefix := range prefixes {
			targetIDs = append(targetIDs, prefix+"::"+graph.UnresolvedMarker+name)
		}
	}

	incoming := g.GetInEdgesByNodeIDs(targetIDs)
	identities := make([]graph.EdgeIdentity, 0)
	seen := make(map[graph.EdgeIdentity]struct{})
	for _, targetID := range targetIDs {
		for _, edge := range incoming[targetID] {
			if !claimingCandidateInScope(edge, scope, resolvers) {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			identities = append(identities, identity)
		}
	}

	return materialize(identities)
}

func unresolvedClaimingCandidates(g graph.Store, scope map[string]bool) []*graph.Edge {
	var pending []*graph.Edge
	for _, edge := range frameworkRepoEdges(g, scope, graph.EdgeCalls, graph.EdgeReferences) {
		if edge != nil && edge.To != "" && graph.IsUnresolvedTarget(edge.To) {
			pending = append(pending, edge)
		}
	}
	return pending
}

func claimingCandidateInScope(edge *graph.Edge, scope map[string]bool, resolvers []ClaimingResolver) bool {
	if edge == nil || edge.To == "" || !graph.IsUnresolvedTarget(edge.To) {
		return false
	}
	if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences {
		return false
	}
	if scope != nil {
		prefix := graph.RepoPrefixOfID(edge.From)
		if !scope[prefix] {
			prefix = graph.RepoPrefixOfID(edge.FilePath)
			if !scope[prefix] {
				return false
			}
		}
	}
	for _, resolver := range resolvers {
		if resolver.Claims(edge) {
			return true
		}
	}
	return false
}

// defaultClaimingResolvers returns the registered claiming resolvers, in
// offer order.
func defaultClaimingResolvers() []ClaimingResolver {
	return []ClaimingResolver{
		DjangoDescriptorResolver{},
	}
}

// RunClaimingResolvers offers every residual unresolved EdgeCalls /
// EdgeReferences to the claiming resolvers; the first whose Claims pre-filter
// passes and whose Resolve lands a target wins. Returns the per-resolver
// count of claimed edges. Unresolved edges are collected before resolving so
// a resolver's ReindexEdges does not mutate a live iteration.
func RunClaimingResolvers(g graph.Store) map[string]int {
	return RunClaimingResolversScoped(g, nil)
}

// RunClaimingResolversScoped limits partial-index work to unresolved calls and
// references sourced by changed repositories. Resolver precedence is retained:
// each registered resolver receives only edges not claimed by an earlier one.
func RunClaimingResolversScoped(g graph.Store, scope map[string]bool) map[string]int {
	out := map[string]int{}
	if g == nil {
		return out
	}
	resolvers := defaultClaimingResolvers()
	if len(resolvers) == 0 {
		return out
	}
	admissible := make([]ClaimingResolver, 0, len(resolvers))
	for _, r := range resolvers {
		if claimingResolverAdmissible(g, r) {
			admissible = append(admissible, r)
		}
	}
	if len(admissible) == 0 {
		return out
	}
	pending := scopedClaimingCandidates(g, scope, admissible)
	claimed := make(map[*graph.Edge]bool)
	for _, r := range admissible {
		candidates := make([]*graph.Edge, 0, len(pending))
		for _, e := range pending {
			if !claimed[e] && r.Claims(e) {
				candidates = append(candidates, e)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		if batcher, ok := r.(batchClaimingResolver); ok {
			for edge := range batcher.ResolveBatch(g, candidates) {
				if edge != nil && !claimed[edge] {
					claimed[edge] = true
					out[r.Name()]++
				}
			}
			continue
		}
		for _, e := range candidates {
			if r.Resolve(g, e) {
				claimed[e] = true
				out[r.Name()]++
			}
		}
	}
	return out
}
