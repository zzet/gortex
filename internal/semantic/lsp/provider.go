package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/lspuri"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider uses an LSP server for on-demand semantic queries.
type Provider struct {
	command string
	args    []string
	// env carries extra KEY=VALUE entries for the server subprocess,
	// from a .gortex.yaml override (e.g. JAVA_HOME for jdtls).
	env []string
	// workspaceFolders are additional roots advertised to the server's
	// initialize request alongside the primary workspace root.
	workspaceFolders []string
	languages        []string
	daemon           bool
	maxParallel      int
	logger           *zap.Logger
	// excludeGlobs are user-configured path globs to skip for enrichment, on
	// top of the built-in generated/vendored heuristic. Set by the router from
	// config when the provider is spawned.
	excludeGlobs []string
	// sweepMode selects how much of the per-file hover / call-hierarchy sweep
	// runs ("demand" default / "full" / "off"); see sweep.go. Set by the
	// router from config when the provider is spawned. An empty value means
	// the demand-gated default; the GORTEX_LSP_SWEEP env override wins over it.
	sweepMode string
	// opensDocs reports whether the enrichment pass sends the
	// textDocument/didOpen / didClose lifecycle before querying a file.
	// Resolved at construction from OpenDocsEnv and the spec's NoDidOpen;
	// true for every server that has not opted out. See ServerSpec.NoDidOpen
	// for why a barrier-scheduling server wants this off.
	opensDocs bool
	// noHeavyRequests disables the textDocument/references and
	// callHierarchy/incomingCalls legs of the enrichment pass. Set from
	// ServerSpec.NoHeavyRequests for servers whose FindReferences machinery
	// leaks per request (csharp-ls); ambiguous edges are confirmed through
	// the definition pass at their call sites instead.
	noHeavyRequests bool
	// spec is the ServerSpec this provider was built from (when the
	// caller used NewProviderFromSpec). nil for legacy NewProvider
	// invocations — those fall back to single-language routing.
	spec *ServerSpec

	// altInitOptions / altInitOptionsFunc carry the InitializationOptions
	// of the alternative command that won on PATH in NewProviderFromSpec.
	// effectiveInitializationOptions(spec) cannot see them because it keys
	// on the spec's primary command; a resolved-alternative server (e.g.
	// intelephense standing in for phpactor) sends these instead.
	altInitOptions     json.RawMessage
	altInitOptionsFunc func(repoRoot string) json.RawMessage

	client *Client

	// sourceCache holds file contents read by openDocument so the
	// per-symbol column-resolution lookups don't reread the file for
	// every hover / references / implementation query. Keyed by
	// absolute path. closeDocument drops a file's entry and
	// resetForReconnect clears the whole map, so a long-lived
	// interactive session — where hover / definition traffic keeps the
	// provider's lastUsed fresh and the router never reaps it — does
	// not retain every navigated file's bytes for the daemon's
	// lifetime. Accessed lock-free (see getSource, which tolerates a
	// miss by falling back to col=0): the interactive
	// open→lookup→close sequence is serialized per file, so it needs
	// no docMu.
	sourceCache map[string][]byte

	// docMu guards docVersions / openDocs / lastDiag so concurrent
	// callers (LSP push notifications + MCP request goroutines) can
	// share one client safely.
	docMu       sync.RWMutex
	docVersions map[string]int          // absPath → most-recent didOpen / didChange version
	openDocs    map[string]bool         // absPath → already opened
	lastDiag    map[string][]Diagnostic // absPath → most recent diagnostics from publishDiagnostics

	// diagWaitersMu guards diagWaiters which lets sync code wait for
	// the next publishDiagnostics for a given file (e.g. fix-all
	// loops re-collecting diagnostics after each apply).
	diagWaitersMu sync.Mutex
	diagWaiters   map[string][]chan []Diagnostic

	// diagHookMu guards diagHook — a single persistent subscriber the
	// router (or any caller) can install to be notified on every
	// publishDiagnostics. The hook MUST be non-blocking; it runs on
	// the LSP client's message-pump goroutine.
	diagHookMu sync.RWMutex
	diagHook   func(absPath string, diags []Diagnostic)

	// capsMu guards caps and dynamicCaps. Read on every Supports()
	// call (hot path), so use RWMutex — register/unregister bursts
	// are rare relative to capability checks.
	capsMu sync.RWMutex
	// caps is the snapshot returned by the server's initialize reply
	// — what the server statically supports. Set once per subprocess
	// lifetime; reset to a zero value on respawn.
	caps ServerCapabilities
	// dynamicCaps holds capabilities the server announced lazily via
	// client/registerCapability. Keyed by Registration.ID (the wire
	// handle the server uses for unregisterCapability). Reset on
	// every ensureClient — a fresh subprocess starts with an empty
	// dynamic table and re-registers what it needs.
	dynamicCaps map[string]Registration

	// connect, when non-nil, switches ensureClient into passive-
	// attach mode: the Provider dials the configured endpoint
	// instead of spawning a subprocess. Reconnect on EOF retries
	// the dial with exponential backoff; fallback to spawn happens
	// only when connect.FallbackSpawn is true.
	connect *ConnectSpec
	// dialBackoff is the current backoff window between failed dial
	// attempts. Doubles on each failure (capped at maxDialBackoff)
	// and resets to dialBackoffStart on the first success.
	dialBackoff time.Duration

	// dialBackoffStart / maxDialBackoff are the per-instance bounds for
	// the reconnect-with-backoff loop (Enrich hover recovery). They
	// default to the package consts of the same name; tests pin them to
	// tiny values to keep recovery fast. dialOrSpawn keeps using the
	// package consts directly — only the mid-flight reconnect path reads
	// these fields.
	dialBackoffStart time.Duration
	maxDialBackoff   time.Duration

	// reconnectMu serialises mid-flight reconnection attempts so that
	// when many enrichment goroutines observe "LSP server exited" at the
	// same instant, exactly one of them rebuilds the client while the
	// others wait and then retry their hover against the fresh session.
	reconnectMu sync.Mutex
	// reconnectAttempts counts how many reconnect *cycles* ran across the
	// whole Enrich pass. Surfaced in the final metrics log.
	reconnectAttempts atomic.Int64

	// connectOnce establishes a fresh client connection (one attempt, no
	// backoff). Defaults to ensureClient. Injected in tests to swap in an
	// in-memory piped client instead of spawning a subprocess.
	connectOnce func(absRoot string) error

	// reqStats counts the LSP request methods issued during the current
	// enrichment pass. Reset at pass start and surfaced in the end-of-pass
	// telemetry so the round-trip volume per method is observable.
	reqStats requestStats
}

// requestStats holds one atomic counter per LSP request method the
// enrichment pass issues, so the pass-complete log can report where the
// round trips went.
type requestStats struct {
	references           atomic.Int64
	implementations      atomic.Int64
	definitions          atomic.Int64
	hovers               atomic.Int64
	prepareCallHierarchy atomic.Int64
	outgoingCalls        atomic.Int64
	incomingCalls        atomic.Int64
	prepareTypeHierarchy atomic.Int64
	supertypes           atomic.Int64
	subtypes             atomic.Int64
	// incomingSkipped counts the callHierarchy/incomingCalls round trips the
	// sweep declined to make because the declaration's callers are already
	// recoverable from the outgoing side — a derived saving, not a request.
	incomingSkipped atomic.Int64
}

// reset zeroes every counter at the start of an enrichment pass.
func (r *requestStats) reset() {
	r.references.Store(0)
	r.implementations.Store(0)
	r.definitions.Store(0)
	r.hovers.Store(0)
	r.prepareCallHierarchy.Store(0)
	r.outgoingCalls.Store(0)
	r.incomingCalls.Store(0)
	r.prepareTypeHierarchy.Store(0)
	r.supertypes.Store(0)
	r.subtypes.Store(0)
	r.incomingSkipped.Store(0)
}

// Dial-retry constants for passive-attach reconnect. The window
// doubles on each failure and tops out at maxDialBackoff, then a
// successful dial resets it. Test fixtures can pin these via the
// helpers in the test file; production code uses the defaults.
const (
	dialBackoffStart = 100 * time.Millisecond
	maxDialBackoff   = 30 * time.Second
)

// NewProvider creates an LSP provider.
func NewProvider(command string, args []string, languages []string, daemon bool, maxParallel int, logger *zap.Logger) *Provider {
	if maxParallel <= 0 {
		maxParallel = 10
	}
	return &Provider{
		command:          command,
		args:             args,
		languages:        languages,
		daemon:           daemon,
		maxParallel:      maxParallel,
		opensDocs:        resolveOpensDocs("", nil),
		noHeavyRequests:  resolveNoHeavyRequests(nil),
		logger:           logger,
		docVersions:      map[string]int{},
		openDocs:         map[string]bool{},
		lastDiag:         map[string][]Diagnostic{},
		diagWaiters:      map[string][]chan []Diagnostic{},
		dynamicCaps:      map[string]Registration{},
		dialBackoffStart: dialBackoffStart,
		maxDialBackoff:   maxDialBackoff,
	}
}

// MaxParallelEnv overrides every spawned server's concurrent-request cap,
// winning over the spec default — the same operator-experiment shape as
// GORTEX_LSP_SWEEP. The probe-measured levers differ per machine (a server
// that multiplexes well takes a higher cap than its conservative spec
// default), so the knob lets an operator try a value for one run without
// editing the registry. Zero, negative, or unparseable values are ignored.
const MaxParallelEnv = "GORTEX_LSP_MAX_PARALLEL"

// resolveMaxParallel picks the effective concurrent-request cap by the
// sweep-mode precedence: the GORTEX_LSP_MAX_PARALLEL env override wins
// over the operator-configured value (`semantic.lsp_max_parallel`), which
// wins over the spec default; 10 is the package fallback. Non-positive or
// unparseable values at any level fall through to the next.
func resolveMaxParallel(configured, specDefault int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxParallelEnv))); err == nil && v > 0 {
		return v
	}
	if configured > 0 {
		return configured
	}
	if specDefault > 0 {
		return specDefault
	}
	return 10
}

// NewProviderFromSpec builds a Provider directly from a ServerSpec.
// Mostly equivalent to NewProvider but lets the runtime router resolve
// the right `languageId` per file extension and pick the first
// available command from the spec's alternatives.
func NewProviderFromSpec(spec *ServerSpec, logger *zap.Logger) *Provider {
	cmd := spec.Command
	args := spec.Args
	var altInitOptions json.RawMessage
	var altInitOptionsFunc func(repoRoot string) json.RawMessage
	if _, err := exec.LookPath(cmd); err != nil {
		for _, alt := range spec.AlternativeCommands {
			if _, err := exec.LookPath(alt.Command); err == nil {
				cmd = alt.Command
				args = alt.Args
				altInitOptions = alt.InitializationOptions
				altInitOptionsFunc = alt.InitOptionsFunc
				break
			}
		}
	}
	maxParallel := resolveMaxParallel(0, spec.MaxParallel)
	p := &Provider{
		command:            cmd,
		args:               args,
		env:                spec.Env,
		languages:          spec.Languages,
		daemon:             spec.Daemon,
		maxParallel:        maxParallel,
		opensDocs:          resolveOpensDocs("", spec),
		noHeavyRequests:    resolveNoHeavyRequests(spec),
		logger:             logger,
		spec:               spec,
		docVersions:        map[string]int{},
		openDocs:           map[string]bool{},
		lastDiag:           map[string][]Diagnostic{},
		diagWaiters:        map[string][]chan []Diagnostic{},
		dynamicCaps:        map[string]Registration{},
		connect:            spec.Connect,
		altInitOptions:     altInitOptions,
		altInitOptionsFunc: altInitOptionsFunc,
		dialBackoff:        dialBackoffStart,
		dialBackoffStart:   dialBackoffStart,
		maxDialBackoff:     maxDialBackoff,
	}
	return p
}

// effectiveInitOptions resolves the initializationOptions to send in this
// provider's initialize request. When the provider was built from a spec
// whose primary command was absent and an alternative won on PATH, that
// alternative's options take precedence over the spec-level blob (which is
// keyed to the primary command and cannot see the resolved alternative).
// InitOptionsFunc is consulted first so an alternative can root a per-repo
// cache path under Gortex's cache home using workspaceRoot.
func (p *Provider) effectiveInitOptions(workspaceRoot string) json.RawMessage {
	if p.altInitOptionsFunc != nil {
		if opts := p.altInitOptionsFunc(workspaceRoot); len(opts) > 0 {
			return opts
		}
	}
	if len(p.altInitOptions) > 0 {
		return p.altInitOptions
	}
	return effectiveInitializationOptions(p.spec)
}

func (p *Provider) Name() string        { return "lsp-" + p.command }
func (p *Provider) Languages() []string { return p.languages }

func (p *Provider) Available() bool {
	_, err := exec.LookPath(p.command)
	return err == nil
}

func (p *Provider) Close() error {
	if p.client != nil {
		return p.client.Shutdown()
	}
	return nil
}

// nodeRelPath strips a node's own RepoPrefix from its FilePath so the
// result joins cleanly under repoRoot, which carries no prefix. Without
// it a multi-repo node FilePath like "gortex/bench/x.rb" joined onto
// repoRoot ".../gortex" doubles the prefix (".../gortex/gortex/bench/x.rb")
// and every os.ReadFile / didOpen fails with ENOTDIR.
func nodeRelPath(n *graph.Node) string {
	if n.RepoPrefix != "" {
		return strings.TrimPrefix(n.FilePath, n.RepoPrefix+"/")
	}
	return n.FilePath
}

// enrichNodeHasUnresolvedDemand reports whether a callable declaration still
// has unresolved same-name call candidates in the graph — `unresolved::*.<name>`
// edges the resolver never bound. Such declarations are the ones an LSP
// references pass can actually connect, so the add-phase enriches them first.
// Unresolved call stubs are indexed by their target string, so this is a
// single reverse-edge lookup, not a scan.
func enrichNodeHasUnresolvedDemand(g graph.Store, n *graph.Node) bool {
	if n == nil || n.Name == "" || (n.Kind != graph.KindMethod && n.Kind != graph.KindFunction) {
		return false
	}
	id := graph.UnresolvedMarker + "*." + n.Name
	return len(g.GetInEdgesByNodeIDs([]string{id})[id]) > 0
}

func enrichNodeHasUnresolvedDemandFromView(view *lspGraphView, n *graph.Node) bool {
	return view.hasUnresolvedDemand(n)
}

// enrichCallableIsDispatchRelevant reports whether a function or method takes
// part in dynamic dispatch, so its incoming callers name concrete targets the
// outgoing side of the sweep cannot reach. Every intra-repo static call is
// recoverable from its caller's outgoing hop — collected for every caller by
// the file sweep — so a declaration's incoming callers only add signal when a
// call to it dispatches through another declaration: a call through an
// interface / abstract member lands the caller's outgoing edge on that member,
// leaving the concrete implementation's real call sites visible only from its
// incoming side. A declaration qualifies when it is itself an abstract /
// interface / virtual member, when it overrides (or is overridden by) another
// declaration, or when its declaring type implements or extends another type.
func enrichCallableIsDispatchRelevant(g graph.Store, n *graph.Node) bool {
	if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
		return false
	}
	view := newLSPGraphView([]*graph.Node{n}, nil)
	out := g.GetOutEdgesByNodeIDs([]string{n.ID})
	in := g.GetInEdgesByNodeIDs([]string{n.ID})
	view.addEdges(out[n.ID])
	view.addEdges(in[n.ID])
	var parentType string
	for _, edge := range out[n.ID] {
		if edge.Kind == graph.EdgeMemberOf {
			parentType = edge.To
			break
		}
	}
	if parentType != "" {
		view.addEdges(g.GetOutEdgesByNodeIDs([]string{parentType})[parentType])
		view.addEdges(g.GetInEdgesByNodeIDs([]string{parentType})[parentType])
	}
	return view.callableIsDispatchRelevant(n)
}

func enrichCallableIsDispatchRelevantFromView(view *lspGraphView, n *graph.Node) bool {
	return view.callableIsDispatchRelevant(n)
}

// enrichTypeIsDispatchRelevantFromView is the type half of the per-file sweep
// gate: an interface, or a class involved in a super/subtype hierarchy — see
// lspGraphView.typeIsDispatchRelevant. Such declarations never contribute
// unresolved-call demand (demand only counts callables), so a file whose only
// enrichable work is a type hierarchy would score zero demand and be skipped
// under the demand default; this signal keeps exactly those files in while a
// bare data type no longer admits its whole file. hierarchyEvidence is the
// per-language self-tuning switch computed by
// enrichLanguageHasHierarchyEvidence: without it the strict check would be
// circular and the gate stays permissive.
func enrichTypeIsDispatchRelevantFromView(view *lspGraphView, n *graph.Node, hierarchyEvidence bool) bool {
	return view.typeIsDispatchRelevant(n, hierarchyEvidence)
}

// enrichLanguageHasHierarchyEvidence reports whether the language's edge set
// carries at least one extends / implements edge some non-LSP lane produced —
// the AST extractor, the tstypes supplemental lane, or resolver inference.
// The strict type gate is meaningful only where such a lane exists: in
// languages with none (c, cpp, objc, swift — no base-list extraction, no
// tstypes lane) the sweep's typeHierarchy hop is the ONLY producer of
// hierarchy edges, so requiring an existing edge for admission would demand
// as input exactly what the sweep exists to produce, and a class-only file's
// hierarchy would never be recovered. Edges the sweep itself MINTED
// (lsp_resolved / lsp_dispatch, no provenance marker) are excluded: counting
// the sweep's own output would flip such a language onto the strict gate one
// run later and silently drop every class added after that. An LSP-origin
// edge carrying meta confirmed_from_origin still counts: ConfirmEdge flips
// the origin in place when the sweep agrees with a non-LSP lane's edge, and
// without the preserved provenance every confirm would decay exactly the
// evidence this probe depends on. One pass over the repo's projected edges,
// attributed to a language via the source node — no per-language table.
func enrichLanguageHasHierarchyEvidence(view *lspGraphView, repoEdges []*graph.Edge, languageMatches func(string) bool) bool {
	for _, e := range repoEdges {
		if e == nil || (e.Kind != graph.EdgeImplements && e.Kind != graph.EdgeExtends) {
			continue
		}
		if e.Origin == graph.OriginLSPResolved || e.Origin == graph.OriginLSPDispatch {
			confirmed := false
			if e.Meta != nil {
				_, confirmed = e.Meta["confirmed_from_origin"]
			}
			if !confirmed {
				continue
			}
		}
		if from := view.nodesByID[e.From]; from != nil && languageMatches(from.Language) {
			return true
		}
	}
	return false
}

// nodeHasSemanticType reports whether a node already carries a non-empty
// semantic_type stamp from an earlier enrichment. The per-file sweep skips
// re-hovering such a node — the hover would only re-derive the identical type
// string — which is what keeps a dispatch-relevant file (swept for its call /
// type-hierarchy edges) from re-paying the whole-file hover cost on a warm
// restart, where every node reloads with its prior stamp.
func nodeHasSemanticType(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	s, ok := n.Meta["semantic_type"].(string)
	return ok && s != ""
}

// nodeAlreadyStamped reports whether a prior enrichment pass already wrote a
// semantic type onto the node. semantic.EnrichNodeMeta stamps semantic_type
// (the load-bearing key) alongside semantic_source, and the stamp round-trips
// through the store, so it persists across daemon restarts. A stamped node is
// already covered — re-hovering it is redundant LSP work. An edited file
// re-parses into fresh node objects with no Meta merge, so its symbols lose
// the stamp and are re-selected naturally on the next pass.
func nodeAlreadyStamped(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, ok := n.Meta["semantic_type"]
	if !ok {
		return false
	}
	if s, isStr := v.(string); isStr {
		return s != ""
	}
	return v != nil
}

// scopedPath re-attaches repoPrefix to a repo-relative path the language
// server handed back: uriToPath returns repo-relative, but graph node
// FilePaths are prefixed, so node lookups must re-prefix to match in a
// multi-repo graph.
func scopedPath(repoPrefix, rel string) string {
	if repoPrefix == "" || rel == "" {
		return rel
	}
	return repoPrefix + "/" + rel
}

// edgeSiteRelPath returns the repo-relative path of the file holding
// the edge's recorded call site, falling back to the caller node's
// path when the edge carries none.
func edgeSiteRelPath(e *graph.Edge, repoPrefix, callerRel string) string {
	if e.FilePath == "" {
		return callerRel
	}
	if repoPrefix != "" {
		return strings.TrimPrefix(e.FilePath, repoPrefix+"/")
	}
	return e.FilePath
}

// rebindTargetAcceptable reports whether a node kind is a sensible
// rebind target for a reference-shaped edge. Files, imports and params
// are containers/positions, never the declaration a call site binds to.
func rebindTargetAcceptable(k graph.NodeKind) bool {
	switch k {
	case graph.KindFile, graph.KindImport, graph.KindParam:
		return false
	}
	return true
}

// edgeExistsAt reports whether an edge (from, to, kind) already exists
// at the given site line — used to avoid minting a duplicate when a
// rebind would land exactly on an edge another pass already recorded.
func edgeExistsAt(view *lspGraphView, from, to string, kind graph.EdgeKind, line int) bool {
	return view.edgeExistsAt(from, to, kind, line)
}

// definitionNodeAtSite asks the language server which declaration the
// identifier `name` at (siteRel, siteLine) resolves to, and maps the
// answer back to a graph node by declaration file + line + name.
//
// Returns (node, true) when the server answered and the definition
// landed on a known node; (nil, true) when the server answered but no
// graph node anchors there (external / builtin / unindexed target);
// (nil, false) when there is no verdict (identifier not on the line,
// open/transport failure, empty response) — callers must not draw
// conclusions from a no-verdict.
//
// cache memoises verdicts per (file, line, name) within one enrichment
// pass; multiple ambiguous edges can share a call site.
//
// content is the site file's bytes, already opened on the server and read
// from disk by the caller (via the shared document session). A nil content
// yields a no-verdict, matching the pre-session behaviour when the site
// file could not be opened.
//
// breaker (nilable) observes each definition round trip: under the heavy
// opt-out this pass carries the whole confirm load, so a server that
// errors every request must trip the failure streak instead of grinding
// through every sited target at one timeout each. Only transport results
// feed it — a cache hit or an identifier miss never reaches the server,
// and an answered-but-empty response counts as success.
func (p *Provider) definitionNodeAtSite(view *lspGraphView, repoPrefix, absRoot, siteRel string, siteLine int, name string, content []byte, breaker *phaseBreaker, cache map[string]*graph.Node) (*graph.Node, bool) {
	if siteRel == "" || siteLine <= 0 || name == "" {
		return nil, false
	}
	key := siteRel + "\x00" + strconv.Itoa(siteLine) + "\x00" + name
	if cached, ok := cache[key]; ok {
		return cached, true
	}
	col, found := identifierColumnStrict(content, siteLine, name)
	if !found {
		// The identifier is not on the recorded line — a definition
		// request would return junk for whatever token sits there.
		return nil, false
	}
	locs, err := p.FindDefinition(absRoot, siteRel, siteLine-1, col, lspCallTimeout())
	if breaker != nil {
		breaker.observe(err == nil)
	}
	if err != nil || len(locs) == 0 {
		return nil, false
	}
	defPath := uriToPath(locs[0].URI, absRoot)
	if defPath == "" {
		// Definition outside the workspace (stdlib, site-packages…).
		cache[key] = nil
		return nil, true
	}
	node := findDeclarationNode(view, scopedPath(repoPrefix, defPath), locs[0].Range.Start.Line+1, name)
	if node == nil {
		// A definition can land on a member declaration INSIDE the target
		// type — `new T(...)` answers the constructor's line, not the type's,
		// and the ctor node carries its own name. A type declaration named
		// exactly `name` whose span contains the answered line is the same
		// verdict: the site binds to that type.
		node = view.findEnclosingTypeNamed(scopedPath(repoPrefix, defPath), locs[0].Range.Start.Line+1, name)
	}
	cache[key] = node
	return node, true
}

// findDeclarationNode locates the graph node whose declaration matches
// (filePath, oneBasedLine, name). Exact StartLine match wins; a ±1
// slack covers servers that anchor the definition on the identifier
// line of a multi-line declaration header. The name must match — that
// is the identity check this lookup exists for.
func findDeclarationNode(view *lspGraphView, filePath string, oneBasedLine int, name string) *graph.Node {
	return view.findDeclarationNode(filePath, oneBasedLine, name)
}

// Enrich runs the full LSP enrichment pass for a single-repo (un-
// prefixed) graph. It delegates to EnrichRepoContext with an empty prefix.
func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, "", repoRoot, nil)
}

// EnrichRepo runs the full LSP enrichment pass with no cancellation
// bound. Kept so the provider still satisfies semantic.RepoScopedProvider
// for callers that don't thread a context.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, repoPrefix, repoRoot, nil)
}

// EnrichRepoContext runs the full LSP enrichment pass over the nodes that
// belong to repoPrefix (the multi-repo scope key; "" for a single-repo /
// in-memory graph). Scoping the node/edge selection to one repo stops a
// multi-repo graph from driving this repo's language server with another
// repo's files, and lets each node's on-disk path resolve by stripping
// its own RepoPrefix.
//
// The language server is spawned lazily: if the repo has no AMBIGUOUS
// (sub-1.0-confidence) edge of this provider's language there is nothing
// for the server to confirm or refute, so the pass returns before
// starting it. This is what keeps a warm restart — where the snapshot is
// already fully resolved — from paying a full server spin-up plus a whole
// hover / call-hierarchy sweep per language for zero enrichment.
//
// Work is ordered by accuracy value and lands INCREMENTALLY: the
// interface-implementation and ambiguous-edge-confirmation passes (the
// edges that decide graph tiers) run first and commit per item; the
// per-file sweep runs call/type hierarchy before hover within each file
// and flushes each file's stamps + hops into the graph as soon as the
// file completes. When ctx is cancelled (the Manager's per-repo
// deadline) the pass stops scheduling new work, keeps everything already
// flushed, marks the result Partial, and returns — completed work is
// never discarded and no writer goroutine outlives the pass.
//
// deadline (may be nil) sizes the pass's context bound LAZILY, from the
// count of hover candidates left after already-stamped nodes are skipped.
// A warm restart where most nodes are already stamped lands a small budget;
// a cold repo (nothing stamped) keeps the full size-scaled headroom. The
// incoming ctx already carries the Manager's generous outer ceiling; the
// derived bound only ever narrows it.
func (p *Provider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.languages[0],
	}

	// Gather this repo's nodes via the indexed repo-scoped scan, NOT a
	// whole-graph AllNodes / AllEdges walk: on a disk backend the latter is
	// O(graph) per provider per repo (a whole-graph AllEdges plus a point
	// GetNode per edge), which dominated fresh-index warmup. Split into two
	// views from one scan:
	//   - langNodes: SYMBOL nodes (file/import excluded) — drives the symbol
	//     count, the per-file fan-out, and the interface-implementation pass.
	//   - langAllByID: every repo+language node id INCLUDING file/import — the
	//     original ambiguous-edge target scan matched any repo+language source
	//     node (file/import edges like EdgeImports included), so the
	//     references-confirm pass must keep matching those too.
	//
	// repoNodes is fetched via repoScopedNodesLight: on a backend with the
	// optional LightNodeReader fast path (sqlite), the meta blob is never
	// decoded for the repo's already-stamped majority. langAllByID only
	// ever needs structural fields (file path, kind, position) for the
	// confirm pass below, so it's built directly from whatever repoNodes
	// holds. langNodes' candidates, in contrast, get round-tripped through
	// AddBatch once hovered (see flushFile), so on a light scan a candidate
	// is never added there directly — its ID is queued and the FULL node
	// (with any non-promoted meta intact) is re-fetched afterward.
	var (
		repoNodes             []*graph.Node
		langNodes             []*graph.Node
		repoEdges             []*graph.Edge
		targets               []enrichTarget
		skippedAlreadyStamped int
		lightScan             bool
	)
	projectedRepo, hasProjectedRepo := p.readLSPRepoProjection(g, repoPrefix)
	if hasProjectedRepo {
		repoNodes = projectedRepo.repoNodes
		langNodes = projectedRepo.langNodes
		repoEdges = projectedRepo.repoEdges
		targets = projectedRepo.targets
		skippedAlreadyStamped = projectedRepo.skippedAlreadyStamped
		if p.logger != nil {
			p.logger.Debug("LSP enrich: SQLite projection ready",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Int("frontier_pages", projectedRepo.frontierPages),
				zap.Int("frontier_peak_files", projectedRepo.frontierPeakFiles),
				zap.Int("frontier_peak_nodes", projectedRepo.frontierPeakNodes),
				zap.Int("frontier_peak_edges", projectedRepo.frontierPeakEdges),
				zap.Int("retained_location_nodes", projectedRepo.retainedLocationNodes),
				zap.Int("retained_lsp_edges", projectedRepo.retainedConfirmableEdges),
			)
		}
	} else {
		repoNodes, lightScan = p.repoScopedNodesLight(g, repoPrefix)
		langAllByID := make(map[string]*graph.Node, len(repoNodes))
		langNodes = make([]*graph.Node, 0, len(repoNodes))
		var candidateIDs []string
		// Count symbol nodes a prior pass already stamped with a semantic type.
		// Stamps persist in node Meta across restarts, so on a warm restart of an
		// unchanged repo — or a changed repo where only a few files moved — most
		// nodes are already covered and re-hovering them is redundant LSP work.
		skippedAlreadyStamped = 0
		for _, n := range repoNodes {
			if n.RepoPrefix != repoPrefix || !p.languageMatches(n.Language) {
				continue
			}
			// Skip machine-generated / vendored files (e.g. tree-sitter's generated
			// parser.c) so the language server never opens or indexes them — that
			// indexing is by far the slowest part of a fresh index for zero value.
			if semantic.IsLowValueForEnrichment(n.FilePath, p.excludeGlobs) {
				continue
			}
			langAllByID[n.ID] = n
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			// Skip symbols a prior enrichment pass already stamped — hover would
			// only re-derive the same type. An edited file re-parses into fresh
			// nodes with no Meta, so its symbols lose the stamp and re-enter here.
			if nodeAlreadyStamped(n) {
				skippedAlreadyStamped++
				continue
			}
			if lightScan {
				candidateIDs = append(candidateIDs, n.ID)
				continue
			}
			langNodes = append(langNodes, n)
		}
		if len(candidateIDs) > 0 {
			full := g.GetNodesByIDs(candidateIDs)
			for _, id := range candidateIDs {
				if n := full[id]; n != nil {
					langNodes = append(langNodes, n)
				}
			}
		}

		// Collect AMBIGUOUS edges (confidence < 1.0) whose source is one of this
		// repo's language nodes — the references pass below confirms / refutes
		// them. The indexed GetRepoEdges scan + the id-set replaces the AllEdges
		// walk with a per-edge GetNode. (enrichTarget is package-scoped so the
		// confirm-pass grouping / matching helpers can take it.)
		repoEdges = p.repoScopedEdges(g, repoPrefix)
		for _, e := range repoEdges {
			if e.Confidence >= 1.0 {
				continue
			}
			// Skip structural-containment edges (member_of, defines, contains,
			// param_of, imports, captures): they anchor no use site a reference
			// lookup can adjudicate, so confirming them wastes a round-trip and can
			// feed a correct edge into the definition-rebind fallback and mutate its
			// target. Use-site, type-position and dataflow edges stay confirmable.
			// See confirmableEdgeKind.
			if !confirmableEdgeKind(e.Kind) {
				continue
			}
			if from, ok := langAllByID[e.From]; ok {
				targets = append(targets, enrichTarget{node: from, edge: e})
			}
		}
	}

	// Lazy server spawn: spin up only when there is something to do — at least
	// one symbol node of the language, OR an ambiguous edge to confirm (the
	// same condition the original whole-graph gate applied). When the repo has
	// neither (a Swift / TS server scoped to a Go repo, or a language whose
	// nodes all live in another repo) return without starting the server — this
	// stops a per-language LSP spin-up for zero enrichment.
	if len(langNodes) == 0 && len(targets) == 0 {
		if p.logger != nil {
			p.logger.Debug("LSP enrich: skipped, repo has no nodes for language",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
			)
		}
		result.DurationMs = time.Since(start).Milliseconds()
		return result, nil
	}

	// Project-readiness preflight: a server whose workspace lacks the project
	// setup it needs (node_modules for tsserver) resolves nothing — every
	// import / cross-file request returns "no package metadata" — yet still
	// pays a full index + per-file hover / call-hierarchy sweep for minutes.
	// Skip the pass entirely; the tree-sitter floor still covers these files,
	// and the resolver's static edges are untouched. This is the general form
	// of the compile-database degrade below, for a missing dependency tree
	// rather than a missing compilation database.
	if p.spec != nil && p.spec.ProjectReady != nil {
		if ready, remediation := p.spec.ProjectReady(absRoot); !ready {
			if p.logger != nil {
				p.logger.Warn("LSP enrich: skipped, project not analyzable",
					zap.String("provider", p.Name()),
					zap.String("repo_prefix", repoPrefix),
					zap.String("remediation", remediation),
				)
			}
			result.DegradedReason = remediation
			result.DurationMs = time.Since(start).Milliseconds()
			return result, nil
		}
	}

	// Compile-database preflight: a server that needs a compilation database
	// (clangd) but has none rebuilds a full fallback AST on every didOpen, so
	// the hover / hierarchy sweep churns for little signal and a directly
	// opened header becomes a standalone TU. Degrade to reference confirmation
	// — the confirm / rebind passes work inside the fallback TU on fallback
	// flags — and skip the sweep, the interface pass, and header files.
	degraded := p.spec != nil && p.spec.NeedsCompileDB && !hasCompileDB(absRoot)
	var degradedSkipFile func(rel string) bool
	if degraded {
		degradedSkipFile = isCXXHeaderFile
		result.Degraded = true
		result.DegradedReason = "no compilation database found; enrichment limited to reference confirmation (hover, hierarchy, and header translation units skipped)"
		if p.logger != nil {
			p.logger.Warn("LSP enrich: degrading to reference confirmation, compilation database missing",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.String("remediation", "generate compile_commands.json (cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON, bear -- make, or meson), then re-index"),
			)
		}
	}

	// Start or reuse the client now that there is work to do.
	if err := p.ensureClient(absRoot); err != nil {
		return nil, fmt.Errorf("start LSP server: %w", err)
	}
	// Default the reconnect seam to the real ensureClient unless a test
	// injected its own (in-memory piped client). Set once per pass.
	if p.connectOnce == nil {
		p.connectOnce = p.ensureClient
	}
	// Reset the cross-pass reconnect counter so the metrics log reflects
	// only this Enrich invocation.
	p.reconnectAttempts.Store(0)
	// Reset the per-method request counters so the pass-complete log
	// reports only this invocation's round trips.
	p.reqStats.reset()

	// One document session shared by every phase below: a file didOpen'd
	// by the interface pass stays warm for the confirm pass and the sweep
	// instead of being reopened, and every open is paired with a didClose
	// (evicted under cap, or by closeAll here at pass end).
	session := newDocSession(p)
	defer session.closeAll()

	// Total symbols scoped to repo + language. langNodes now holds only the
	// symbols this pass will hover (file / import nodes and already-stamped
	// nodes excluded); HoverCandidates exposes that post-filter count for
	// deadline budgeting. SymbolsTotal keeps whole-repo meaning by re-adding
	// the already-stamped symbols, which are pre-seeded into SymbolsCovered
	// since their semantic type is already in the graph.
	result.HoverCandidates = len(langNodes)
	result.SymbolsTotal = len(langNodes) + skippedAlreadyStamped
	if hasProjectedRepo {
		result.SymbolsTotal = projectedRepo.symbolsTotal
	}
	result.SymbolsCovered = skippedAlreadyStamped

	// Lazy per-repo deadline: now that candidate selection is done, size the
	// window on the count of symbols this pass will actually hover — the
	// already-stamped nodes were skipped above, so a warm restart with few
	// unstamped nodes lands a small budget while a cold repo keeps full
	// headroom. Only ever narrows the Manager's outer ceiling already on ctx;
	// BudgetSeconds records the derived value for the status surface.
	if deadline != nil {
		if d := deadline(len(langNodes)); d > 0 {
			result.BudgetSeconds = d.Seconds()
			var cancelBudget context.CancelFunc
			ctx, cancelBudget = context.WithTimeout(ctx, d)
			defer cancelBudget()
		}
	}

	// Productivity checkpoint: the candidate-scaled budget above assumes the
	// server yields. A server that ANSWERS but resolves nothing for this
	// workspace (observed: a 10-minute typescript pass, thousands of answered
	// requests, zero confirmations and zero type stamps) would otherwise
	// consume its whole budget producing nothing. Once real request volume
	// has flowed with zero useful yield, cut the pass through the ordinary
	// deadline path — completed work is already flushed, and zero yield
	// means nothing is lost. A warming server (jdtls indexing, blocked
	// requests) never reaches the volume gate, and any repo that yields even
	// one confirmation, added edge, or type stamp is exempt for the pass.
	var usefulYield atomic.Int64
	passReqBase := p.reqStats.total()
	checkpointCtx, cancelCheckpoint := context.WithCancel(ctx)
	defer cancelCheckpoint()
	ctx = checkpointCtx
	if window := lspProductivityWindow(); window > 0 {
		go func() {
			t := time.NewTicker(window)
			defer t.Stop()
			tick := int64(0)
			for {
				select {
				case <-checkpointCtx.Done():
					return
				case <-t.C:
					tick++
					if p.reqStats.total()-passReqBase < lspProductivityMinRequests {
						// Requests are not flowing (server still warming /
						// indexing); volume, not time, gates the verdict.
						continue
					}
					// Sustained-rate verdict, not a one-shot zero check: the
					// observed pathology dribbles one type stamp per ~30s of
					// full-timeout requests — enough to defeat a zero-yield
					// test while still wasting the whole budget. Require the
					// cumulative yield to keep pace with a modest floor per
					// elapsed window; any genuinely productive pass clears it
					// by orders of magnitude, and short passes finish before
					// the first tick.
					if usefulYield.Load() >= tick*lspProductivityMinYieldPerWindow {
						continue
					}
					p.logger.Warn("LSP enrich: pass cut by productivity checkpoint — request volume flowed with near-zero useful yield",
						zap.String("provider", p.Name()),
						zap.String("repo_prefix", repoPrefix),
						zap.Duration("window", window),
						zap.Int64("windows_elapsed", tick),
						zap.Int64("useful_yield", usefulYield.Load()),
						zap.Int64("requests", p.reqStats.total()-passReqBase))
					cancelCheckpoint()
					return
				}
			}
		}()
	}

	// The graph-mutation blocks in this pass serialise on the backend
	// resolve mutex (the same lock every other edge-mutating pass holds)
	// so this pass can run concurrently with other repos' enrichment.
	// Only the in-memory mutations are locked — the LSP I/O stays outside
	// the lock so concurrent language servers still overlap.
	rmu := g.ResolveMutex()

	// Reuse the repo-scoped node/edge projections for every LSP result. The
	// extra inbound read is one bounded batch for cross-repo dispatch edges;
	// target endpoints absent from this repo projection are fetched together.
	view := newLSPGraphView(repoNodes, repoEdges)
	view.addNodes(langNodes)
	if hasProjectedRepo {
		view.setFanInCounts(projectedRepo.fanInByID)
		view.addEdges(projectedRepo.inboundDispatchEdges)
	} else {
		inboundIDSet := make(map[string]struct{}, len(langNodes))
		for _, n := range langNodes {
			inboundIDSet[n.ID] = struct{}{}
			for _, edge := range view.outByID[n.ID] {
				if edge.Kind == graph.EdgeMemberOf {
					inboundIDSet[edge.To] = struct{}{}
				}
			}
		}
		inboundIDs := make([]string, 0, len(inboundIDSet))
		for id := range inboundIDSet {
			inboundIDs = append(inboundIDs, id)
		}
		sort.Strings(inboundIDs)
		if len(inboundIDs) > 0 {
			for _, edges := range g.GetInEdgesByNodeIDs(inboundIDs) {
				view.addEdges(edges)
			}
		}
	}
	missingTargetIDs := make([]string, 0)
	seenMissingTarget := make(map[string]struct{})
	for _, target := range targets {
		id := target.edge.To
		if view.nodesByID[id] != nil {
			continue
		}
		if _, seen := seenMissingTarget[id]; seen {
			continue
		}
		seenMissingTarget[id] = struct{}{}
		missingTargetIDs = append(missingTargetIDs, id)
	}
	if len(missingTargetIDs) > 0 {
		fetched := g.GetNodesByIDs(missingTargetIDs)
		for _, id := range missingTargetIDs {
			if n := fetched[id]; n != nil {
				view.addNodes([]*graph.Node{n})
			}
		}
	}

	// Targeted edge work runs FIRST — before the whole-repo per-file
	// sweep — because confirmed / added edges (implements, calls) decide
	// graph tiers and find_usages accuracy, while hover only stamps type
	// strings. Each item commits to the graph as soon as it resolves, so
	// a deadline cut loses only the un-visited remainder.
	//
	// Deadline budgeting: the reference-confirm pass is round-trip bound and
	// can, on a medium repo, consume the entire per-repo deadline before the
	// hover / hierarchy add phase runs at all — leaving edges_added stuck at 0.
	// targetedCtx caps the targeted-edge passes (implementations + confirm) at
	// a fraction of the window, reserving the remainder for the sweep so both
	// phases make progress. With no deadline (tests) it is the parent context.
	// A degraded pass skips the sweep entirely, so there is nothing to reserve
	// for — give the confirm pass the whole window.
	targetedCtx := ctx
	if dl, ok := ctx.Deadline(); ok && !degraded {
		window := dl.Sub(start)
		reserve := time.Duration(float64(window) * enrichSweepReserveFraction)
		if reserve > 0 && reserve < window {
			var cancelTargeted context.CancelFunc
			targetedCtx, cancelTargeted = context.WithDeadline(ctx, dl.Add(-reserve))
			defer cancelTargeted()
		}
	}

	// Failure-streak breakers (see yield_breaker.go): a server that answers
	// nothing for this workspace must not grind through the whole target and
	// hover corpus at one timeout per request. Any successful answer disarms
	// the phase's breaker permanently.
	targetedBreaker := newPhaseBreaker(lspPhaseFailureStreakLimit(), p.logger, "targeted", repoPrefix)
	hoverBreaker := newPhaseBreaker(lspPhaseFailureStreakLimit(), p.logger, "hover", repoPrefix)

	// Phase boundary timestamps: the completion log breaks the pass wall time
	// out per phase (phase_*_ms) — an aggregate duration cannot say which pass
	// owns the minutes when a run needs speed forensics.
	setupDone := time.Now()

	// Query implementations for interface nodes. A degraded pass skips this:
	// the query opens each interface's file, and a database-less clangd cannot
	// resolve implementations across translation units regardless. Graph
	// matches are served from the repo projection and all writes land together.
	interfaceMutations := newLSPMutationBatch()
	interfacePromotions := make(map[*graph.Edge]struct{})
	for _, n := range langNodes {
		if degraded {
			break
		}
		if targetedCtx.Err() != nil {
			break
		}
		if n.Kind != graph.KindInterface {
			continue
		}
		line, ok := lspLine(n)
		if !ok {
			continue
		}

		rel := nodeRelPath(n)
		if !p.servesFile(rel) {
			continue // never open a file this server can't compile
		}
		content, release, err := session.acquire(p.client, filepath.Join(absRoot, rel))
		if err != nil {
			continue
		}
		col := identifierColumn(content, n.StartLine, n.Name)
		impls, err := p.findImplementations(absRoot, rel, line, col)
		release()
		targetedBreaker.observe(err == nil)
		if targetedBreaker.isTripped() {
			break
		}
		if err != nil || len(impls) == 0 {
			continue
		}

		for _, loc := range impls {
			implPath := uriToPath(loc.URI, absRoot)
			if implPath == "" {
				continue
			}
			implNode := view.matchNodeByFileLine(scopedPath(repoPrefix, implPath), loc.Range.Start.Line+1)
			if implNode == nil {
				continue
			}

			existing := view.findMatchingEdge(implNode.ID, n.ID, graph.EdgeImplements)
			if existing != nil {
				if existing.Confidence < 1.0 {
					interfacePromotions[existing] = struct{}{}
				}
				continue
			}
			edge := newLSPResolvedEdge(implNode.ID, n.ID, graph.EdgeImplements,
				implNode.FilePath, implNode.StartLine, p.Name(), graph.OriginLSPDispatch)
			if interfaceMutations.stageAdd(view, edge) {
				usefulYield.Add(1)
				result.EdgesAdded++
			}
		}
	}
	if len(interfacePromotions) > 0 || len(interfaceMutations.adds) > 0 {
		rmu.Lock()
		for existing := range interfacePromotions {
			semantic.ConfirmEdge(existing, p.Name())
			interfaceMutations.stagePersist(existing)
			result.EdgesConfirmed++
		}
		interfaceMutations.apply(g, nil)
		rmu.Unlock()
	}
	implsDone := time.Now()

	// Query references for AMBIGUOUS edges to confirm/refute. Promotion
	// to the lsp tier is identity-anchored: the server's evidence must
	// name the EDGE'S OWN call site, not merely fall somewhere inside
	// the caller's body. The old span-containment check promoted a
	// heuristically-misbound edge whenever the caller referenced BOTH
	// same-named declarations (test exercising sync Client.stream and
	// async AsyncClient.stream in one function): one genuine reference
	// of the wrong target inside the caller's span rubber-stamped the
	// edge bound at the OTHER member's line — a compiler-grade tier
	// serving the wrong declaration.
	//
	// The sweep fans out across maxParallel, grouped by the referent's file
	// so each file is opened once (per-goroutine, via enrichOpenDoc) and
	// serves every target sharing it — turning the ~7 edges/s sequential
	// round-trip loop into maxParallel-wide throughput. The definition-rebind
	// fallback that follows fans out the same way over the targets the sweep
	// left unconfirmed, grouped by call-site file — each site file is owned
	// by exactly one goroutine, so document open/close never overlaps on the
	// same document.
	var confirmMu sync.Mutex
	confirmPromotions := make(map[*graph.Edge]struct{})
	var fallback []enrichTarget
	if p.noHeavyRequests {
		// This server leaks per references request (ServerSpec.NoHeavyRequests)
		// — the reference sweep never runs. Every sited target goes straight
		// to the definition pass below, which reaches the same confirm /
		// rebind verdicts from the call site at position-request cost.
		// Built from the raw target list, not confirmGroups: the grouping
		// drops targets on referent-side conditions (no position, unserved
		// referent file) that exist only because findReferences opens the
		// REFERENT's file, and a misbound edge with an unusable stored
		// target is exactly what the rebind arm is for. The definition pass
		// applies its own call-site filters when it groups. Targets without
		// a recorded site line are left at their heuristic tier: there is
		// no site to ask definition at.
		for _, t := range targets {
			if t.edge.Line > 0 {
				fallback = append(fallback, t)
			}
		}
	} else {
		confirmGroups := p.groupConfirmTargets(view.nodesByID, targets, degradedSkipFile)
		sem := make(chan struct{}, p.maxParallel)
		var wg sync.WaitGroup
		for _, grp := range confirmGroups {
			if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(grp *confirmGroup) {
				defer func() {
					<-sem
					wg.Done()
				}()
				if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
					return
				}
				absPath := filepath.Join(absRoot, grp.rel)
				// The file is unique to this group, so no two goroutines open
				// it; the session pins it for the group and leaves it warm for
				// a later phase that shares the file.
				content, release, err := session.acquire(p.client, absPath)
				if err != nil {
					return
				}
				defer release()
				// findReferences is positioned on the TARGET declaration, not the
				// call site, so every ambiguous edge in this group that points at
				// the same referent asks the server for the identical reference
				// list — measured ~9x fan-in for calls, ~8x for references, since
				// an overloaded / hot declaration draws many candidate edges.
				// confirmRefMatchesSite still filters that shared list per edge,
				// so caching it by target node turns N identical round-trips into
				// one with byte-identical confirm/refute decisions. A target's
				// file is unique to one group, so every edge to it lands here —
				// the per-group cache captures all of its redundancy. ok=false
				// records a server error so repeat targets skip exactly as the
				// un-cached path did (error → skip, no fallback).
				type cachedRefs struct {
					refs []Location
					ok   bool
				}
				refsByTarget := make(map[string]cachedRefs, len(grp.targets))
				for _, t := range grp.targets {
					if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
						return
					}
					toNode := view.nodesByID[t.edge.To]
					if toNode == nil {
						continue
					}
					line, ok := lspLine(toNode)
					if !ok {
						continue
					}
					cr, seen := refsByTarget[t.edge.To]
					if !seen {
						col := identifierColumn(content, toNode.StartLine, toNode.Name)
						refs, err := p.findReferences(absRoot, grp.rel, line, col)
						targetedBreaker.observe(err == nil)
						cr = cachedRefs{refs: refs, ok: err == nil}
						refsByTarget[t.edge.To] = cr
					}
					if !cr.ok {
						continue
					}
					if p.confirmRefMatchesSite(cr.refs, absRoot, repoPrefix, t) {
						usefulYield.Add(1)
						confirmMu.Lock()
						confirmPromotions[t.edge] = struct{}{}
						confirmMu.Unlock()
						continue
					}
					// Unconfirmed with a recorded site line: defer to the
					// serial definition-rebind fallback below.
					if t.edge.Line > 0 {
						confirmMu.Lock()
						fallback = append(fallback, t)
						confirmMu.Unlock()
					}
				}
			}(grp)
		}
		wg.Wait()
	}
	if len(confirmPromotions) > 0 {
		mutations := newLSPMutationBatch()
		rmu.Lock()
		for edge := range confirmPromotions {
			semantic.ConfirmEdge(edge, p.Name())
			mutations.stagePersist(edge)
			result.EdgesConfirmed++
		}
		mutations.apply(g, nil)
		rmu.Unlock()
	}
	confirmDone := time.Now()

	// Definition-rebind pass: for a site the reference sweep did not tie to
	// the edge's target, ask the server what the site actually resolves to
	// (textDocument/definition at the site identifier). When the definition
	// lands back on the target we confirm anyway (reference lists can be
	// incomplete); when it names a DIFFERENT known declaration we rebind the
	// edge to the correct target instead of leaving a misattribution behind.
	//
	// Sites are grouped by their call-site file — one session acquire and
	// one definition cache serve every site in the file — and the groups fan
	// out across maxParallel. The serial loop this replaces existed to keep
	// call-site document opens from overlapping across goroutines; for a
	// heavy-opt-out server this pass carries the whole confirm load (tens of
	// thousands of definitions on a large solution), and a position request
	// needs no such serialization. Stable sort so sites keep their relative
	// order within a file.
	sort.SliceStable(fallback, func(i, j int) bool {
		return edgeSiteRelPath(fallback[i].edge, repoPrefix, nodeRelPath(fallback[i].node)) <
			edgeSiteRelPath(fallback[j].edge, repoPrefix, nodeRelPath(fallback[j].node))
	})
	type defSiteGroup struct {
		rel     string
		targets []enrichTarget
	}
	var defGroups []defSiteGroup
	for _, t := range fallback {
		siteRel := edgeSiteRelPath(t.edge, repoPrefix, nodeRelPath(t.node))
		if !p.servesFile(siteRel) {
			continue // never open a call-site file this server can't compile
		}
		if degradedSkipFile != nil && degradedSkipFile(siteRel) {
			continue // degraded mode: never open this call-site (e.g. a header)
		}
		if len(defGroups) == 0 || defGroups[len(defGroups)-1].rel != siteRel {
			defGroups = append(defGroups, defSiteGroup{rel: siteRel})
		}
		defGroups[len(defGroups)-1].targets = append(defGroups[len(defGroups)-1].targets, t)
	}
	fallbackMutations := newLSPMutationBatch()
	{
		sem := make(chan struct{}, p.maxParallel)
		var wg sync.WaitGroup
		for i := range defGroups {
			if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(grp *defSiteGroup) {
				defer func() {
					<-sem
					wg.Done()
				}()
				if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
					return
				}
				// The file is unique to this group, so no two goroutines
				// open it. A failed acquire yields nil content, which
				// definitionNodeAtSite treats as a no-verdict — the same
				// outcome the per-site openDocument failure produced before.
				var content []byte
				if c, release, err := session.acquire(p.client, filepath.Join(absRoot, grp.rel)); err == nil {
					content = c
					defer release()
				}
				// Cache keys carry the site file, so the cache is
				// group-local by construction — no sharing to guard.
				cache := map[string]*graph.Node{}
				for _, t := range grp.targets {
					if targetedCtx.Err() != nil || targetedBreaker.isTripped() {
						return
					}
					toNode := view.nodesByID[t.edge.To]
					if toNode == nil {
						continue
					}
					cand, ok := p.definitionNodeAtSite(view, repoPrefix, absRoot, grp.rel, t.edge.Line, toNode.Name, content, targetedBreaker, cache)
					// The verdict switch reads the view's edge indexes
					// (edgeExistsAt, the dispatch-member helpers) that the
					// staged mutations also update, so the whole switch —
					// counters included — runs under rmu. The expensive
					// part, the definition round trip above, stays outside.
					rmu.Lock()
					switch {
					case !ok || cand == nil:
						// No verdict — leave the edge at its heuristic tier
						// so min_tier filtering excludes it.
					case cand.ID == toNode.ID:
						// Yield feeds the productivity checkpoint: under the
						// heavy opt-out this pass is the only confirm source
						// until the hover phase, and without these the
						// checkpoint reads a pass that settles thousands of
						// edges here as zero-yield and cancels it.
						semantic.ConfirmEdge(t.edge, p.Name())
						fallbackMutations.stagePersist(t.edge)
						result.EdgesConfirmed++
						usefulYield.Add(1)
					case view.declaredDispatchMember(cand) && view.implementsDeclaredMember(toNode, cand):
						// The compiler answers the DECLARED member — the site's static
						// receiver is the interface / abstract base, so the stored edge
						// to one of its concrete impls is a devirtualization inference
						// definition cannot vouch for. Add the compiler-proven edge to
						// the declared member and leave the inference exactly as it was
						// (not retargeted, not promoted): impl reachability flows
						// through the declared member's dispatch fan-out, and the
						// heuristic tier keeps the guess honestly labeled. A target
						// UNRELATED to the declared member is not this case — that is a
						// plain misbind and falls through to the rebind below.
						if !edgeExistsAt(view, t.edge.From, cand.ID, t.edge.Kind, t.edge.Line) {
							declared := newLSPResolvedEdge(t.edge.From, cand.ID, t.edge.Kind,
								t.edge.FilePath, t.edge.Line, p.Name(), graph.OriginLSPResolved)
							if fallbackMutations.stageAdd(view, declared) {
								result.EdgesAdded++
								usefulYield.Add(1)
							}
						}
					case rebindTargetAcceptable(cand.Kind) && !edgeExistsAt(view, t.edge.From, cand.ID, t.edge.Kind, t.edge.Line):
						// Mutate the full edge state before staging the set-oriented
						// reindex so the backend persists the complete confirmed payload.
						oldTo := t.edge.To
						t.edge.To = cand.ID
						semantic.ConfirmEdge(t.edge, p.Name())
						t.edge.Meta["rebound_from"] = oldTo
						fallbackMutations.stageReindex(view, t.edge, oldTo)
						result.EdgesRebound++
						usefulYield.Add(1)
					}
					rmu.Unlock()
				}
			}(&defGroups[i])
		}
		wg.Wait()
	}
	if len(fallbackMutations.reindexes) > 0 || len(fallbackMutations.persists) > 0 ||
		len(fallbackMutations.adds) > 0 {
		rmu.Lock()
		fallbackMutations.apply(g, nil)
		rmu.Unlock()
	}
	defsDone := time.Now()

	// Degraded finalisation: the interface pass, the references-add pass, and
	// the per-file hover / hierarchy sweep are all skipped when a needed
	// compilation database is missing — only the reference-confirm and
	// definition-rebind passes ran, working inside the fallback translation
	// unit. Finalise the result here, before the sweep machinery below.
	if degraded {
		if result.SymbolsTotal > 0 {
			result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
		}
		result.DurationMs = time.Since(start).Milliseconds()
		if ctx.Err() != nil {
			result.Partial = true
			result.AbortReason = ctx.Err().Error()
		}
		if p.logger != nil {
			didOpens, reopenedFiles, docEvictions, peakOpenDocs := session.stats()
			p.logger.Info("LSP enrich: degraded pass complete (reference confirmation only)",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Bool("degraded", true),
				zap.Int("edges_confirmed", result.EdgesConfirmed),
				zap.Int("edges_rebound", result.EdgesRebound),
				zap.Int("did_opens", didOpens),
				zap.Int("reopened_files", reopenedFiles),
				zap.Int("doc_evictions", docEvictions),
				zap.Int("peak_open_docs", peakOpenDocs),
				zap.Int64("req_references", p.reqStats.references.Load()),
				zap.Int64("req_definitions", p.reqStats.definitions.Load()),
			)
		}
		return result, nil
	}

	// References-driven add pass: a server that enumerates references but has
	// no call hierarchy (e.g. intelephense) never runs the per-file sweep's
	// hierarchy hops, so the dispatch call sites it can see are confirmed but
	// never ADDED. Ask textDocument/references per declaration and mint those
	// call edges. Runs under the targeted budget (before the hover sweep) so
	// a deadline cut sheds hover work, not the recall-bearing add.
	if !p.noHeavyRequests &&
		p.Supports("textDocument/references") && !p.Supports("textDocument/prepareCallHierarchy") {
		p.referencesAddPass(targetedCtx, g, view, repoPrefix, absRoot, langNodes, rmu, session, result)
	}
	refsAddDone := time.Now()

	// Per-file document lifecycle + bounded concurrency. The original
	// implementation bulk-opened every target file up front and closed
	// them all in one deferred sweep after a fully sequential hover loop —
	// at peak that pinned tens of thousands of documents open in the
	// language server and OOM-killed it. The fix bounds the open set, but
	// must keep a file open for the whole span of its symbols' hovers: a
	// per-node open/close re-opened the file once per symbol whenever a
	// file's per-node goroutines did not overlap in time (common on a
	// loaded CI runner), so didOpen was no longer sent exactly once per
	// file (TestLSP_Provider_OpensEachFileOnce). Enrichment is therefore
	// grouped by file — one goroutine per file opens it exactly once, fans
	// its symbols out across a shared hover budget, then closes it exactly
	// once. fileSem caps the simultaneously-open documents at maxParallel
	// (the original OOM trigger); hoverSem caps concurrent hovers at
	// maxParallel independently, so a single many-symbol file still hovers
	// in parallel.
	enrichedNodes := make(map[string]bool)

	// Race-safe metric counters for the concurrent hover phase.
	var diagTotalNodes, diagHoverOK, diagHoverErr, diagHoverNil, diagTypeEmpty, diagEnriched, diagNoPosition, diagHoverSkipped atomic.Int64

	// mu guards the cross-goroutine aggregation: per-file stamp buffers,
	// enrichedNodes, the EnrichResult node counters, and the best-effort
	// first-sample diagnostics below.
	var mu sync.Mutex
	var diagFirstHoverValue, diagFirstHoverError, diagFirstNodeName, diagFirstNodeFile string

	// activeClient is the client the hover goroutines currently target.
	// reconnectWithBackoff swaps it (under reconnectMu) when the server
	// dies mid-flight; goroutines load it atomically so the swap never
	// races an in-flight hover.
	var activeClient atomic.Pointer[Client]
	activeClient.Store(p.client)

	// Abort coordination: if reconnection fails permanently, the first
	// goroutine to learn it records the error and flips aborted; the rest
	// stop early and Enrich returns that error.
	var aborted atomic.Bool
	var abortErr error
	var abortOnce sync.Once
	// reconnectCycles counts distinct reconnect cycles this pass — a server
	// that connects fine but keeps exiting mid-request (e.g. clangd crashing in
	// a lint matcher) would otherwise reconnect endlessly, repeating work and
	// pinning the process at high CPU. The crash-loop guard caps it.
	var reconnectCycles atomic.Int64

	// reconnect serialises mid-flight recovery so that when a burst of
	// goroutines observe the same dead client only the first rebuilds it;
	// the others wait on reconnectMu and then reuse the fresh client.
	reconnect := func(stale *Client) (*Client, error) {
		p.reconnectMu.Lock()
		defer p.reconnectMu.Unlock()
		if aborted.Load() {
			return nil, abortErr
		}
		if cur := activeClient.Load(); cur != stale {
			return cur, nil // someone else already reconnected
		}
		// Crash-loop guard: past the per-pass cap, abandon this provider's
		// enrichment for the repo rather than reconnect again. Whatever already
		// flushed stays in the graph; the caller sees the failure and the log
		// names the provider, repo, and reconnect count.
		if cycles := reconnectCycles.Add(1); cycles > maxEnrichReconnectCycles {
			err := fmt.Errorf("LSP provider %q exited %d times during enrichment of repo %q; abandoning pass",
				p.Name(), cycles, repoPrefix)
			abortOnce.Do(func() {
				abortErr = err
				aborted.Store(true)
			})
			p.logger.Warn("LSP enrich: crash-loop guard tripped, abandoning provider",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Int64("reconnect_cycles", cycles),
				zap.Int64("reconnect_attempts", p.reconnectAttempts.Load()),
			)
			return nil, err
		}
		newC, err := p.reconnectWithBackoff(absRoot)
		if err != nil {
			abortOnce.Do(func() {
				abortErr = err
				aborted.Store(true)
			})
			return nil, err
		}
		activeClient.Store(newC)
		return newC, nil
	}

	// Demand/dispatch decisions are immutable during the concurrent sweep.
	// Compute them once from the repo adjacency projection before any file
	// goroutine starts staging new hierarchy edges into that projection.
	langHierarchyEvidence := enrichLanguageHasHierarchyEvidence(view, repoEdges, p.languageMatches)
	nodeDemand := make(map[string]bool, len(langNodes))
	nodeDispatch := make(map[string]bool, len(langNodes))
	nodeTypeDispatch := make(map[string]bool, len(langNodes))
	for _, n := range langNodes {
		nodeDemand[n.ID] = enrichNodeHasUnresolvedDemandFromView(view, n)
		nodeDispatch[n.ID] = enrichCallableIsDispatchRelevantFromView(view, n)
		nodeTypeDispatch[n.ID] = enrichTypeIsDispatchRelevantFromView(view, n, langHierarchyEvidence)
	}

	// Group enrichment targets by file so each file's open/close lifecycle
	// spans all of its symbols. Files keep encounter order; symbols keep
	// their order within a file.
	type fileTargets struct {
		rel      string
		nodes    []*graph.Node
		demand   int  // declarations still carrying unresolved same-name candidates
		dispatch bool // carries a dispatch-relevant callable, or a type involved in a hierarchy
	}
	var fileList []*fileTargets
	fileIndex := map[string]*fileTargets{}
	for _, n := range langNodes {
		rel := nodeRelPath(n)
		if !p.servesFile(rel) {
			continue // never open a file this server can't compile
		}
		ft := fileIndex[n.FilePath]
		if ft == nil {
			ft = &fileTargets{rel: rel}
			fileIndex[n.FilePath] = ft
			fileList = append(fileList, ft)
		}
		ft.nodes = append(ft.nodes, n)
		if nodeDemand[n.ID] {
			ft.demand++
		}
		if nodeDispatch[n.ID] || nodeTypeDispatch[n.ID] {
			ft.dispatch = true
		}
	}
	// Demand-driven ordering: enrich the files whose declarations still carry
	// unresolved same-name call candidates first. Under a per-repo deadline the
	// budget is then spent where the LSP references pass will actually bind
	// dropped call sites, instead of on declarations static resolution already
	// covered. Stable so files of equal demand keep encounter order.
	sort.SliceStable(fileList, func(i, j int) bool {
		return fileList[i].demand > fileList[j].demand
	})

	// Sweep gate: the per-file hover / call-hierarchy sweep below is the
	// whole-repo churn a warm restart pays to confirm zero new edges. The
	// resolved mode (GORTEX_LSP_SWEEP env override > router-configured field
	// > demand-gated default) decides which files it visits — sweepFile
	// skips a file the mode excludes. "demand" (default) sweeps a file that
	// still carries unresolved same-name candidates OR declares a type /
	// interface whose super/subtype hierarchy only this sweep recovers,
	// "full" sweeps every file, "off" skips the sweep entirely. The
	// tier-deciding confirm / add / interface passes above are never gated.
	sweepMode := p.effectiveSweepMode()

	// Call- and type-hierarchy hops are collected per file (while the
	// file is open) and applied in that file's flush below, so each
	// file's graph mutations land as soon as the file completes while
	// the file is still opened exactly once per pass.
	type callHop struct {
		n          *graph.Node
		other      CallHierarchyItem
		asOutgoing bool
		// fromRanges carries the call-expression ranges reported by
		// the server — per the LSP spec these are always ranges
		// inside the CALLER's file, for both incoming and outgoing
		// hops. Kept so the landed edge can be stamped at the real
		// call-site line instead of the caller's declaration line.
		fromRanges []Range
	}
	type typeHop struct {
		n           *graph.Node
		other       TypeHierarchyItem
		asSupertype bool
	}

	// Only interrogate the server for call / type hierarchy when it
	// advertised the capability. Skipping otherwise avoids the
	// "non-added document" / method-not-found churn against servers (or
	// languages) that do not implement it.
	callHierOK := p.Supports("textDocument/prepareCallHierarchy")
	typeHierOK := p.Supports("textDocument/prepareTypeHierarchy")

	// flushFile lands one completed file's work into the graph: hover
	// stamps plus call/type-hierarchy hops. EnrichNodeMeta mutates
	// Node.Meta in place, but on disk backends the node is a per-call
	// reconstruction, so stamped nodes must be round-tripped through the
	// store (AddBatch) or the semantic_type stamp is discarded — see
	// semantic.EnrichNodeMeta. Committing per file — instead of buffering
	// the whole repo and applying after the sweep — is what makes a
	// deadline cut lose only unfinished files, never completed work.
	// Graph mutations and the result edge counters inside
	// recordHierarchyCall / linkTypeHierarchy serialise on the resolve
	// mutex; the counters are next read after wg.Wait, so they stay
	// race-free.
	flushFile := func(stamped []*graph.Node, cHops []callHop, tHops []typeHop) {
		if len(stamped) == 0 && len(cHops) == 0 && len(tHops) == 0 {
			return
		}
		rmu.Lock()
		defer rmu.Unlock()
		mutations := newLSPMutationBatch()
		for _, h := range cHops {
			p.recordHierarchyCall(view, mutations, repoPrefix, absRoot, h.n, h.other, h.asOutgoing, h.fromRanges, result)
		}
		for _, h := range tHops {
			p.linkTypeHierarchy(view, mutations, repoPrefix, absRoot, h.n, h.other, h.asSupertype, result)
		}
		mutations.apply(g, stamped)
	}

	// fileSem bounds the number of simultaneously-open documents; hoverSem
	// bounds concurrent hovers across all open files. Both at maxParallel:
	// holding a file open never consumes a hover slot, so one file with
	// many symbols still hovers maxParallel-wide, while many single-symbol
	// files keep at most maxParallel documents open at once.
	fileSem := make(chan struct{}, p.maxParallel)
	hoverSem := make(chan struct{}, p.maxParallel)
	var wg sync.WaitGroup

	for _, ft := range fileList {
		if aborted.Load() || ctx.Err() != nil {
			break
		}
		if !sweepFile(sweepMode, ft.demand, ft.dispatch) {
			continue // mode excludes this file from the per-file sweep
		}
		wg.Add(1)
		fileSem <- struct{}{} // acquire — bounds simultaneously-open docs
		go func(ft *fileTargets) {
			defer func() {
				<-fileSem
				wg.Done()
			}()
			if aborted.Load() || ctx.Err() != nil {
				return
			}

			absPath := filepath.Join(absRoot, ft.rel)

			// Pin the file open on the active client through the shared
			// session for the whole span of this file's hierarchy + hover
			// work — exactly one didOpen per file on the happy path, and the
			// content is read from disk once. The session dedupes per
			// (client, path), so a hover goroutine that reconnects re-opens
			// the file on the fresh client (once) without a second open on
			// the client this pin holds; closeAll pairs every open with a
			// didClose at pass end.
			content, release, err := session.acquire(activeClient.Load(), absPath)
			if err != nil {
				p.logger.Debug("LSP enrich: didOpen failed",
					zap.String("file", ft.rel), zap.Error(err))
				return
			}
			defer release()

			// Per-file result buffers, flushed into the graph when this
			// file finishes (or is cut mid-file by cancellation) —
			// deferred so even a partially-processed file keeps what it
			// completed.
			var fileStamped []*graph.Node // guarded by mu during the hover fan-out
			var cHops []callHop
			var tHops []typeHop
			defer func() { flushFile(fileStamped, cHops, tHops) }()

			// While the file is open on the server, interrogate it for
			// call- and type-hierarchy edges the AST extractor may have
			// missed. This runs BEFORE the hover fan-out: hierarchy hops
			// become lsp_resolved edges — far more accuracy-bearing than
			// hover type strings — so a deadline cut mid-file sheds hover
			// work, not edges. Running while the document is added also
			// keeps prepare* from failing with "non-added document" and
			// preserves exactly one didOpen per file.
			if callHierOK || typeHierOK {
				for _, n := range ft.nodes {
					if aborted.Load() || ctx.Err() != nil {
						break
					}
					line, ok := lspLine(n)
					if !ok {
						continue
					}
					col := identifierColumn(content, n.StartLine, n.Name)
					switch n.Kind {
					case graph.KindFunction, graph.KindMethod:
						if !callHierOK {
							continue
						}
						items, err := p.prepareCallHierarchy(absRoot, ft.rel, line, col)
						if err != nil {
							continue
						}
						// Outgoing always: the file sweep visits every caller,
						// so a declaration's outgoing hops alone reconstruct
						// every intra-repo static call edge. Incoming only adds
						// what the outgoing side is blind to — the concrete
						// callers of a dynamic-dispatch target, and names the
						// resolver still could not bind — so it is fetched only
						// for a dispatch-relevant or demand-bearing declaration
						// (or under a full sweep). Demand-first file ordering
						// sweeps the demand-bearing callers before any deadline
						// cut, so a skipped incoming costs no reachable edge.
						// incomingCalls rides the same FindReferences
						// machinery as references — a NoHeavyRequests
						// server never gets the round trip, whatever the
						// sweep mode says.
						wantIncoming := !p.noHeavyRequests &&
							(sweepMode == sweepModeFull ||
								nodeDispatch[n.ID] || nodeDemand[n.ID])
						for _, item := range items {
							if outs, oerr := p.outgoingCalls(item); oerr == nil {
								for _, oc := range outs {
									cHops = append(cHops, callHop{n: n, other: oc.To, asOutgoing: true, fromRanges: oc.FromRanges})
								}
							}
							if !wantIncoming {
								p.reqStats.incomingSkipped.Add(1)
								continue
							}
							if ins, ierr := p.incomingCalls(item); ierr == nil {
								for _, ic := range ins {
									cHops = append(cHops, callHop{n: n, other: ic.From, asOutgoing: false, fromRanges: ic.FromRanges})
								}
							}
						}
					case graph.KindType, graph.KindInterface:
						if !typeHierOK {
							continue
						}
						items, err := p.prepareTypeHierarchy(absRoot, ft.rel, line, col)
						if err != nil {
							continue
						}
						for _, item := range items {
							if sups, serr := p.supertypes(item); serr == nil {
								for _, s := range sups {
									tHops = append(tHops, typeHop{n: n, other: s, asSupertype: true})
								}
							}
							if subs, serr := p.subtypes(item); serr == nil {
								for _, s := range subs {
									tHops = append(tHops, typeHop{n: n, other: s, asSupertype: false})
								}
							}
						}
					}
				}
			}

			var nodeWg sync.WaitGroup
			for _, n := range ft.nodes {
				if aborted.Load() || ctx.Err() != nil {
					break
				}
				// Hover-skip: a node already carrying a semantic_type stamp
				// (e.g. reloaded on a warm restart) gains nothing from another
				// hover — the derived type string is unchanged. Skip it so a
				// file swept for its call / type-hierarchy edges does not
				// re-pay the whole-file hover cost; the hierarchy interrogation
				// above already ran for every node regardless.
				if nodeHasSemanticType(n) {
					diagHoverSkipped.Add(1)
					continue
				}
				line, ok := lspLine(n)
				if !ok {
					// No real source position — the request would carry
					// line -1 and be rejected by the server. Skip.
					diagNoPosition.Add(1)
					continue
				}
				diagTotalNodes.Add(1)
				nodeWg.Add(1)
				hoverSem <- struct{}{} // acquire — bounds concurrent hovers
				go func(n *graph.Node, line int) {
					defer func() {
						<-hoverSem
						nodeWg.Done()
					}()
					if aborted.Load() || ctx.Err() != nil || hoverBreaker.isTripped() {
						return
					}

					c := activeClient.Load()
					// Ensure the doc is open on this goroutine's client. On
					// the happy path this dedupes against the file goroutine's
					// pin (no didOpen); when the active client was swapped by a
					// concurrent reconnect it opens the file on the new client.
					_, releaseHover, err := session.acquire(c, absPath)
					if err != nil {
						p.logger.Debug("LSP enrich: didOpen failed",
							zap.String("file", n.FilePath), zap.Error(err))
						return
					}
					defer releaseHover()

					col := identifierColumn(content, n.StartLine, n.Name)
					hoverResult, err := p.hoverWith(c, absRoot, nodeRelPath(n), line, col)
					if err != nil && isServerExitError(err) {
						// Server died mid-flight — recover once and retry this
						// node's hover against the fresh session. The new client
						// has no record of our document, so re-open it there
						// (the session dedupes the re-open across this file's
						// goroutines) before retrying.
						newC, rerr := reconnect(c)
						if rerr != nil {
							return // aborted; wg.Wait + abort check below handles it
						}
						c = newC
						var releaseNew func()
						_, releaseNew, err = session.acquire(c, absPath)
						if err != nil {
							p.logger.Debug("LSP enrich: reopen after reconnect failed",
								zap.String("file", n.FilePath), zap.Error(err))
							return
						}
						defer releaseNew()
						hoverResult, err = p.hoverWith(c, absRoot, nodeRelPath(n), line, col)
					}
					if err != nil {
						hoverBreaker.observe(false)
						diagHoverErr.Add(1)
						mu.Lock()
						if diagFirstHoverError == "" {
							diagFirstHoverError = err.Error()
							diagFirstNodeName = n.Name
							diagFirstNodeFile = n.FilePath
						}
						mu.Unlock()
						return
					}
					if hoverResult == nil {
						hoverBreaker.observe(true)
						diagHoverNil.Add(1)
						return
					}
					hoverBreaker.observe(true)
					diagHoverOK.Add(1)

					typeInfo := extractTypeFromHover(hoverResult.Contents.Value)
					mu.Lock()
					if diagFirstHoverValue == "" {
						diagFirstHoverValue = hoverResult.Contents.Value
						if len(diagFirstHoverValue) > 200 {
							diagFirstHoverValue = diagFirstHoverValue[:200]
						}
					}
					mu.Unlock()
					if typeInfo == "" {
						diagTypeEmpty.Add(1)
						return
					}

					semantic.EnrichNodeMeta(n, "semantic_type", typeInfo, p.Name())
					usefulYield.Add(1)
					diagEnriched.Add(1)
					mu.Lock()
					fileStamped = append(fileStamped, n)
					if !enrichedNodes[n.ID] {
						result.NodesEnriched++
						result.SymbolsCovered++
						enrichedNodes[n.ID] = true
					}
					mu.Unlock()
				}(n, line)
			}
			nodeWg.Wait()
		}(ft)
	}
	wg.Wait()

	// Enrichment metrics (acceptance criterion 6): hover outcomes, the
	// shared document session's open/reopen/eviction accounting, and the
	// per-method request volume that drove the pass.
	didOpens, reopenedFiles, docEvictions, peakOpenDocs := session.stats()
	p.logger.Info("LSP enrich: hover phase complete",
		zap.String("sweep_mode", sweepMode),
		zap.Int64("total_nodes", diagTotalNodes.Load()),
		zap.Int64("hover_ok", diagHoverOK.Load()),
		zap.Int64("hover_err", diagHoverErr.Load()),
		zap.Int64("hover_nil", diagHoverNil.Load()),
		zap.Int64("type_empty", diagTypeEmpty.Load()),
		zap.Int64("enriched", diagEnriched.Load()),
		zap.Int("skipped_already_stamped", skippedAlreadyStamped),
		zap.Int("hover_candidates", result.HoverCandidates),
		zap.Int64("skipped_no_position", diagNoPosition.Load()),
		zap.Int64("hover_sweep_skipped", diagHoverSkipped.Load()),
		zap.Int64("reconnect_attempts", p.reconnectAttempts.Load()),
		zap.Int("did_opens", didOpens),
		zap.Int("reopened_files", reopenedFiles),
		zap.Int("doc_evictions", docEvictions),
		zap.Int("peak_open_docs", peakOpenDocs),
		zap.Int64("phase_setup_ms", setupDone.Sub(start).Milliseconds()),
		zap.Int64("phase_impls_ms", implsDone.Sub(setupDone).Milliseconds()),
		zap.Int64("phase_confirm_ms", confirmDone.Sub(implsDone).Milliseconds()),
		zap.Int64("phase_definitions_ms", defsDone.Sub(confirmDone).Milliseconds()),
		zap.Int64("phase_refs_add_ms", refsAddDone.Sub(defsDone).Milliseconds()),
		zap.Int64("phase_sweep_ms", time.Since(refsAddDone).Milliseconds()),
		zap.Int64("req_references", p.reqStats.references.Load()),
		zap.Int64("req_implementations", p.reqStats.implementations.Load()),
		zap.Int64("req_definitions", p.reqStats.definitions.Load()),
		zap.Int64("req_hovers", p.reqStats.hovers.Load()),
		zap.Int64("req_prepare_call_hierarchy", p.reqStats.prepareCallHierarchy.Load()),
		zap.Int64("req_outgoing_calls", p.reqStats.outgoingCalls.Load()),
		zap.Int64("req_incoming_calls", p.reqStats.incomingCalls.Load()),
		zap.Int64("incoming_calls_skipped", p.reqStats.incomingSkipped.Load()),
		zap.Int64("req_prepare_type_hierarchy", p.reqStats.prepareTypeHierarchy.Load()),
		zap.Int64("req_supertypes", p.reqStats.supertypes.Load()),
		zap.Int64("req_subtypes", p.reqStats.subtypes.Load()),
		zap.String("first_hover_value", diagFirstHoverValue),
		zap.String("first_hover_error", diagFirstHoverError),
		zap.String("first_node_name", diagFirstNodeName),
		zap.String("first_node_file", diagFirstNodeFile),
		zap.Bool("targeted_breaker_tripped", targetedBreaker.isTripped()),
		zap.Bool("hover_breaker_tripped", hoverBreaker.isTripped()),
	)

	if targetedBreaker.isTripped() && hoverBreaker.isTripped() &&
		result.EdgesConfirmed == 0 && result.EdgesRebound == 0 &&
		result.EdgesAdded == 0 && result.NodesEnriched == 0 {
		result.DegradedReason = "language server answered no request for this workspace; pass abandoned by zero-yield breaker"
	}

	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}
	result.DurationMs = time.Since(start).Milliseconds()

	if aborted.Load() {
		// Whatever was flushed before the abort stays in the graph; the
		// result rides along so the caller can record its counts.
		return result, fmt.Errorf("LSP enrichment aborted: %w", abortErr)
	}

	if ctx.Err() != nil {
		// Deadline / cancellation: everything flushed so far is in the
		// graph and counted; only the un-visited remainder was skipped.
		result.Partial = true
		result.AbortReason = ctx.Err().Error()
		p.logger.Warn("LSP enrich: pass cancelled at deadline; completed work already landed",
			zap.String("repo_prefix", repoPrefix),
			zap.Int("edges_confirmed", result.EdgesConfirmed),
			zap.Int("edges_rebound", result.EdgesRebound),
			zap.Int("edges_added", result.EdgesAdded),
			zap.Int("nodes_enriched", result.NodesEnriched),
			zap.Error(ctx.Err()),
		)
	}
	return result, nil
}

func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// LSP supports incremental updates, but for simplicity we skip it.
	// The full Enrich pass handles this.
	return nil, nil
}

// resetForReconnect clears the per-connection state that a dead client
// invalidated: open-document tracking, doc versions, and the dynamic
// capability table. The server (whether a freshly spawned subprocess
// or a freshly dialed IDE) has no knowledge of the documents we
// previously opened against the dead session — re-opening on first
// touch lets the next call (textDocument/hover etc.) succeed.
//
// Caps are reset separately at the top of ensureClient so the reset
// also covers the initial-connect path; here we only clear what the
// reconnect-specific recovery needs.
func (p *Provider) resetForReconnect() {
	// Drop the dead client so the next ensureClient branch builds a
	// fresh transport. Close it best-effort first to free any
	// pending pending-map entries; the dead read loop already closed
	// `done` so Shutdown() is a no-op past that point.
	if p.client != nil {
		_ = p.client.Shutdown()
		p.client = nil
	}
	p.docMu.Lock()
	p.docVersions = map[string]int{}
	p.openDocs = map[string]bool{}
	p.lastDiag = map[string][]Diagnostic{}
	p.docMu.Unlock()
	// sourceCache is the unsynchronised interactive-navigation cache
	// (written lock-free by openDocument); drop it on reconnect so a
	// long-lived provider doesn't carry the dead session's file bytes.
	// nil is the freed state — openDocument lazily re-creates it and
	// getSource nil-checks before reading.
	p.sourceCache = nil
}

// dialOrSpawn builds the LSP client according to the provider's spec.
// When p.connect is set, it dials the configured endpoint; on dial
// failure with FallbackSpawn=true it falls back to spawning the
// subprocess; with FallbackSpawn=false it returns the dial error so
// the language stays unavailable rather than racing the IDE.
//
// The dial path retries with exponential backoff capped at
// maxDialBackoff. Each successful dial resets the backoff window.
func (p *Provider) dialOrSpawn(workspaceRoot string) (*Client, error) {
	if p.connect != nil {
		if err := p.connect.Validate(); err != nil {
			return nil, fmt.Errorf("lsp passive attach: %w", err)
		}
		client, dialErr := NewClientWithTransport(&DialTransport{
			Network: p.connect.Network,
			Address: p.connect.Address,
		}, p.logger)
		if dialErr == nil {
			// Reset the backoff so a flap doesn't punish the next
			// reconnect with the last failure's window.
			p.dialBackoff = dialBackoffStart
			return client, nil
		}
		// Bump the backoff for the next attempt — callers that retry
		// immediately on failure can pace themselves via
		// SinceLastDialAttempt(), and the router's reaper / on-demand
		// callers naturally space attempts apart.
		nextBackoff := p.dialBackoff * 2
		if nextBackoff > maxDialBackoff {
			nextBackoff = maxDialBackoff
		}
		if nextBackoff <= 0 {
			nextBackoff = dialBackoffStart
		}
		p.dialBackoff = nextBackoff

		if !p.connect.FallbackSpawn {
			return nil, fmt.Errorf("lsp passive attach %s %s: %w (no spawn fallback configured)",
				p.connect.Network, p.connect.Address, dialErr)
		}
		// Fallback to spawn — log loudly so operators see the IDE
		// went away. The next ensureClient will retry dial first
		// (resetForReconnect clears p.client), so once the IDE
		// comes back we drift back to the passive path on next
		// reconnect.
		if p.logger != nil {
			p.logger.Warn("lsp: passive dial failed, falling back to spawn",
				zap.String("network", p.connect.Network),
				zap.String("address", p.connect.Address),
				zap.Error(dialErr),
			)
		}
	}
	if p.command == "" {
		return nil, fmt.Errorf("lsp: no command configured and no passive attach available")
	}
	args := p.args
	// jdtls with no -data lets the launcher default its Eclipse workspace to
	// ~/Library/Caches/jdtls/<hash>, outside Gortex's cache isolation. Pin it
	// under the resolved cache home per repo instead.
	if isJdtlsCommand(p.command) {
		args = jdtlsDataArgs(args, workspaceRoot)
	}
	// Pin the workspace's resolved solution (env or lone root .sln/.slnx)
	// so the loaded workspace is deterministic and matches the targeted
	// pre-restore — see csharpSolutionArgs. Log the choice and any dropped
	// env entries: a silently ignored mis-spelling otherwise presents as an
	// unpinned workspace with no explanation.
	if isCSharpLSCommand(p.command) {
		before := len(args)
		args = csharpSolutionArgs(args, workspaceRoot)
		if p.logger != nil {
			choice := csharpSolutionResolution(workspaceRoot)
			for _, entry := range choice.ignored {
				p.logger.Warn("lsp: csharp solution env entry ignored",
					zap.String("entry", entry),
					zap.String("workspace", workspaceRoot))
			}
			if len(args) > before {
				p.logger.Info("lsp: csharp solution pinned",
					zap.String("solution", choice.solution),
					zap.String("source", choice.source),
					zap.String("workspace", workspaceRoot))
			}
		}
	}
	return NewClient(p.command, args, p.env, workspaceRoot, p.logger)
}

// defaultLSPCallTimeout bounds a single post-initialize LSP request.
// A wedged server — e.g. csharp-ls stuck loading an MSBuild workspace,
// alive but never replying — would otherwise let an enrichment hover /
// findReferences Call block forever and stall the whole enrichment
// WaitGroup. The initialize handshake itself is left unbounded (a cold
// .NET / Java workspace load can legitimately run for minutes).
const defaultLSPCallTimeout = 30 * time.Second

// lspCallTimeout resolves the post-initialize Call bound, honouring the
// GORTEX_LSP_CALL_TIMEOUT env override (a Go duration such as "45s";
// "0" / "off" / "none" disables the bound). An unparseable value falls
// back to the default.
func lspCallTimeout() time.Duration {
	switch v := strings.TrimSpace(os.Getenv("GORTEX_LSP_CALL_TIMEOUT")); v {
	case "":
		return defaultLSPCallTimeout
	case "0", "off", "none":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		return defaultLSPCallTimeout
	}
}

// servesCSharp reports whether this provider routes C# (.cs) files. Used to
// scope the C#-specific pre-restore and diagnostic-filter behaviour so it can
// never touch another language's provider.
func (p *Provider) servesCSharp() bool {
	for _, l := range p.languages {
		if l == "csharp" {
			return true
		}
	}
	return false
}

// CSharpDiagFilterEnv toggles the C# advisory-diagnostic filter (see
// filterCSharpAdvisoryDiags). ON by default; set to a falsey value
// ("0" / "off" / "false" / "none") to pass every diagnostic through unchanged.
const CSharpDiagFilterEnv = "GORTEX_LSP_CSHARP_DIAG_FILTER"

// csharpDiagFilterEnabled reports whether the C# advisory-diagnostic filter is
// active. Default ON — the filter only drops NuGet advisory codes, which are
// never code-level problems an indexer acts on.
func csharpDiagFilterEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(CSharpDiagFilterEnv))) {
	case "0", "off", "false", "none":
		return false
	default:
		return true
	}
}

// CSharpRestoreEnv toggles the C# pre-spawn `dotnet restore` (see
// Provider.maybeCSharpPreRestore). ON by default; set to a falsey value
// ("0" / "off" / "false" / "none") to skip it — e.g. offline / air-gapped
// indexing, or to keep indexing off the network.
const CSharpRestoreEnv = "GORTEX_LSP_CSHARP_RESTORE"

// csharpRestoreEnabled reports whether the C# pre-spawn restore is active.
// Default ON: gortex only restores repositories the user has explicitly added
// (it never auto-discovers), and spawning the C# server already evaluates the
// project's MSBuild graph — so restore adds no execution surface beyond the
// workspace load it precedes, while letting the Roslyn workspace load every
// project instead of dropping audit-flagged ones and reporting false errors.
func csharpRestoreEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(CSharpRestoreEnv))) {
	case "0", "off", "false", "none":
		return false
	default:
		return true
	}
}

// diagCodeString renders a Diagnostic.Code as a string when it is a string
// code (NuGet / Roslyn codes such as "NU1902" / "CS0246"); numeric codes
// return "". Sufficient for the NuGet-advisory check, which only matches NU####.
func diagCodeString(code any) string {
	switch c := code.(type) {
	case string:
		return c
	case json.Number:
		return c.String()
	default:
		return ""
	}
}

// isNuGetAdvisoryCode reports whether code is a NuGet code — "NU" (any case)
// followed by one or more digits — the audit / restore advisory family.
func isNuGetAdvisoryCode(code string) bool {
	if len(code) < 3 {
		return false
	}
	if (code[0] != 'N' && code[0] != 'n') || (code[1] != 'U' && code[1] != 'u') {
		return false
	}
	for _, r := range code[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// filterCSharpAdvisoryDiags drops NuGet *advisory* diagnostics (code NU####)
// from a C# publishDiagnostics batch and returns the survivors.
//
// Why: csharp-ls / OmniSharp build a Roslyn MSBuildWorkspace that escalates a
// NuGet audit *warning* — e.g. NU1902 "package has a known vulnerability" — to
// a fatal project-load failure and then surfaces it as a diagnostic. The
// `dotnet build` / `dotnet test` CLIs keep the same NU19xx a non-fatal warning
// and succeed, so the diagnostic is noise from the indexer's point of view
// (gortex does not act on dependency-vulnerability advisories).
//
// The filter is deliberately narrow: it matches ONLY the NU#### NuGet code
// family. Real compiler diagnostics (CS####) and analyzer warnings always pass
// through — a dropped project's genuine "unresolved type" errors are fixed by
// loading the project (the pre-restore guard), never by hiding CS codes
// here. Returns the input slice unchanged (no allocation) when nothing matches.
func filterCSharpAdvisoryDiags(diags []Diagnostic) []Diagnostic {
	drop := false
	for _, d := range diags {
		if isNuGetAdvisoryCode(diagCodeString(d.Code)) {
			drop = true
			break
		}
	}
	if !drop {
		return diags
	}
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		if isNuGetAdvisoryCode(diagCodeString(d.Code)) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// csharpRestoreTimeout bounds the pre-spawn `dotnet restore`.
const csharpRestoreTimeout = 5 * time.Minute

// csharpPreRestoreEligible reports whether the C# pre-restore should run for
// this provider: it serves C#, restore is enabled (csharpRestoreEnabled, ON by
// default), and we are spawning the server (not passively attached to an
// IDE-owned LSP, which manages its own restore).
func (p *Provider) csharpPreRestoreEligible() bool {
	return p.connect == nil && p.servesCSharp() && csharpRestoreEnabled()
}

// maybeCSharpPreRestore runs `dotnet restore` with NuGet audit suppressed in
// workspaceRoot before the C# LSP starts, when csharpPreRestoreEligible.
//
// Why: csharp-ls / OmniSharp treat a NuGet audit warning (NU19xx) as a fatal
// project-load failure and drop the project; its files then have no compilation
// and the server reports false "unresolved type" errors, while `dotnet build`
// keeps NU19xx a non-fatal warning. Restoring with `-p:NuGetAudit=false` writes
// a clean project.assets.json so the workspace loads every project.
//
// On by default (CSharpRestoreEnv): gortex only indexes repositories the user
// explicitly added (never auto-discovered), and the C# server spawn already
// evaluates the project's MSBuild graph — so restore adds no execution surface
// beyond the workspace load it precedes. Best-effort: a restore failure logs
// and falls through to the normal spawn (status quo), never aborting
// enrichment; skipped on passive attach (the IDE owns restore) and when dotnet
// is not on PATH.
func (p *Provider) maybeCSharpPreRestore(workspaceRoot string) {
	if !p.csharpPreRestoreEligible() {
		return
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		if p.logger != nil {
			p.logger.Debug("lsp: csharp pre-restore skipped — dotnet not on PATH",
				zap.Error(err))
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), csharpRestoreTimeout)
	defer cancel()
	// Target the same solution the server will load (when one resolves) — a
	// bare restore fails with MSB1011 in a multi-solution root, leaving the
	// audit-suppressed assets unwritten. See csharpRestoreArgs.
	cmd := exec.CommandContext(ctx, "dotnet", csharpRestoreArgs(workspaceRoot)...)
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Dir = workspaceRoot
	cmd.Env = append(os.Environ(), p.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if p.logger != nil {
			p.logger.Warn("lsp: csharp pre-restore failed; spawning server anyway",
				zap.String("workspace", workspaceRoot),
				zap.Error(err),
				zap.ByteString("output", lastBytes(out, 600)),
			)
		}
		return
	}
	if p.logger != nil {
		p.logger.Info("lsp: csharp pre-restore complete (NuGetAudit suppressed)",
			zap.String("workspace", workspaceRoot),
			zap.String("solution", csharpSolutionFor(workspaceRoot)))
	}
}

// lastBytes returns up to the last n bytes of b — keeps a failed restore's
// tail (where the error sits) out of an unbounded log line.
func lastBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// ensureClient starts the LSP server if not already running, OR
// reconnects if the previous client's transport went away (e.g. the
// IDE that owned a passive-attach LSP restarted, closing the socket).
//
// For spawn-mode providers this matches the original behaviour: first
// call spawns, subsequent calls are no-ops while the subprocess lives.
// For passive (connect) mode, a dead client triggers a re-dial with
// exponential backoff, falling back to spawn only if the spec's
// Connect.FallbackSpawn is true.
func (p *Provider) ensureClient(workspaceRoot string) error {
	// Liveness probe — if we have a client but its read loop has
	// terminated, the server (or socket) is gone. Treat as
	// disconnected: drop the dead handle and reset per-connection
	// state so the next branch can build a fresh transport.
	if p.client != nil {
		select {
		case <-p.client.Done():
			p.resetForReconnect()
		default:
			return nil
		}
	}

	// Reset the dynamic capability table — a fresh subprocess (or a
	// fresh dialed connection) has no dynamic registrations until it
	// re-announces them. Reset under the lock so any racing
	// Supports() reader sees a coherent state.
	p.capsMu.Lock()
	p.caps = ServerCapabilities{}
	p.dynamicCaps = map[string]Registration{}
	p.capsMu.Unlock()

	// C#: optionally `dotnet restore` (NuGet audit suppressed) before the
	// server starts so its MSBuild workspace loads every project instead of
	// dropping audit-warning projects and reporting false errors. On by
	// default and best-effort — see maybeCSharpPreRestore / CSharpRestoreEnv.
	p.maybeCSharpPreRestore(workspaceRoot)

	client, err := p.dialOrSpawn(workspaceRoot)
	if err != nil {
		return err
	}

	// Wire diagnostic + reverse-RPC handlers before initialize so we
	// don't lose the first publishDiagnostics burst that some servers
	// emit during workspace warmup.
	client.OnNotification("textDocument/publishDiagnostics",
		func(_ string, params json.RawMessage) {
			var pd PublishDiagnosticsParams
			if err := json.Unmarshal(params, &pd); err != nil {
				return
			}
			abs := uriToAbsPath(pd.URI)
			if abs == "" {
				return
			}
			diags := pd.Diagnostics
			// C#: strip NuGet audit advisories (NU19xx) that csharp-ls /
			// OmniSharp surface as diagnostics (and escalate to fatal
			// project drops) — they are not code errors an indexer acts
			// on. Real CS#### compiler diagnostics pass through untouched.
			if p.servesCSharp() && csharpDiagFilterEnabled() {
				diags = filterCSharpAdvisoryDiags(diags)
			}
			p.docMu.Lock()
			p.lastDiag[abs] = diags
			p.docMu.Unlock()
			p.fanoutDiagnostics(abs, diags)
		})
	// Some servers (rust-analyzer, jdtls) emit progress / log messages
	// — silently swallow them; they're not actionable for the indexer.
	for _, m := range []string{
		"$/progress", "window/logMessage", "window/showMessage",
		"telemetry/event", "$/cancelRequest",
	} {
		client.OnNotification(m, func(_ string, _ json.RawMessage) {})
	}

	// Reply OK to common reverse-RPC requests so servers don't stall.
	// We never *need* to mutate workspace settings — saying "applied"
	// to applyEdit when we're an indexer is wrong, so we say no by
	// default and let the apply-code-action path opt in explicitly.
	client.OnRequest("workspace/configuration",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Reply with one nil per requested item — servers that ask
			// for configuration treat null as "use defaults".
			return []any{nil}, nil
		})
	// client/registerCapability and client/unregisterCapability —
	// dynamic capability announcements. Some servers send these as
	// requests (with id, expecting a null ack); others send them as
	// notifications. We wire both forms to the same handlers so the
	// caps table converges regardless of which framing the server
	// chose. See applyRegistrations / applyUnregistrations.
	client.OnRequest("client/registerCapability",
		func(_ string, params json.RawMessage) (any, *jsonRPCError) {
			p.applyRegistrations(params)
			// LSP spec: reply with null (an empty success result).
			return nil, nil
		})
	client.OnRequest("client/unregisterCapability",
		func(_ string, params json.RawMessage) (any, *jsonRPCError) {
			p.applyUnregistrations(params)
			return nil, nil
		})
	client.OnNotification("client/registerCapability",
		func(_ string, params json.RawMessage) {
			p.applyRegistrations(params)
		})
	client.OnNotification("client/unregisterCapability",
		func(_ string, params json.RawMessage) {
			p.applyUnregistrations(params)
		})
	client.OnRequest("workspace/applyEdit",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Default: refuse. The apply-code-action path swaps this
			// handler before issuing executeCommand so server-driven
			// applies land on disk via WriteWorkspaceEdit.
			return ApplyWorkspaceEditResponse{Applied: false, FailureReason: "applies are routed through gortex"}, nil
		})

	initParams := InitializeParams{
		ProcessID:        os.Getpid(),
		RootURI:          pathToURI(workspaceRoot),
		WorkspaceFolders: buildWorkspaceFolders(workspaceRoot, p.workspaceFolders),
		Capabilities: ClientCapabilities{
			Workspace: &WorkspaceClientCapabilities{
				ApplyEdit: true,
				WorkspaceEdit: &WorkspaceEditClientCapabilities{
					DocumentChanges:    true,
					ResourceOperations: []string{"create", "rename", "delete"},
				},
				ExecuteCommand:   &ExecuteCommandCapability{DynamicRegistration: true},
				WorkspaceFolders: true,
				Configuration:    true,
			},
			TextDocument: TextDocumentClientCapabilities{
				Synchronization: &SynchronizationCapability{DynamicRegistration: true},
				Implementation:  &ImplementationCapability{DynamicRegistration: true},
				References:      &ReferencesCapability{DynamicRegistration: true},
				Definition:      &DefinitionCapability{DynamicRegistration: true},
				Hover:           &HoverCapability{ContentFormat: []string{"plaintext"}},
				CallHierarchy:   &CallHierarchyCapability{DynamicRegistration: true},
				TypeHierarchy:   &TypeHierarchyCapability{DynamicRegistration: true},
				CodeAction: &CodeActionCapability{
					DynamicRegistration: true,
					CodeActionLiteralSupport: &CodeActionLiteralSupport{
						CodeActionKind: CodeActionKindCapability{
							ValueSet: []string{
								CodeActionKindEmpty,
								CodeActionKindQuickFix,
								CodeActionKindRefactor,
								CodeActionKindRefactorExtract,
								CodeActionKindRefactorInline,
								CodeActionKindRefactorRewrite,
								CodeActionKindSource,
								CodeActionKindSourceOrganizeImports,
								CodeActionKindSourceFixAll,
							},
						},
					},
					IsPreferredSupport: true,
				},
				PublishDiagnostics: &PublishDiagnosticsCapability{
					RelatedInformation: true,
					VersionSupport:     true,
				},
			},
		},
	}
	// Pass server-specific InitializationOptions (e.g. Maven/Gradle import
	// settings for jdtls, or a per-repo storagePath for a resolved-
	// alternative intelephense) when the provider was built from a ServerSpec.
	if opts := p.effectiveInitOptions(workspaceRoot); len(opts) > 0 {
		initParams.InitializationOptions = opts
	}

	var initResult InitializeResult
	if err := client.Call("initialize", initParams, &initResult); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialize: %w", err)
	}

	// Snapshot the server's static capabilities so Supports() can
	// answer "did the server advertise this at initialize time?".
	// Dynamic registrations may arrive any time after this point.
	p.capsMu.Lock()
	p.caps = initResult.Capabilities
	p.capsMu.Unlock()

	// Send initialized notification.
	if err := client.Notify("initialized", struct{}{}); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialized: %w", err)
	}

	// The (possibly slow) cold-workspace load is done — bound every
	// subsequent request so a server that wedges mid-session can no
	// longer block an enrichment Call forever. See lspCallTimeout.
	client.SetCallTimeout(lspCallTimeout())

	p.client = client
	return nil
}

// WaitReady implements semantic.ReadinessProber for the Roslyn / MSBuild-class
// C# servers (csharp-ls, OmniSharp). They answer `initialize` quickly but keep
// loading the solution in the background and serve empty results until it
// finishes — the pathology where the enrichment deadline elapses mid-load and
// the pass lands zero edges ("completed in 8s, 0 coverage"). WaitReady brings
// the client up (spawn, optional `dotnet restore`, `initialize`) and then blocks
// on a readiness probe until the solution's compilation is live, so the Manager
// starts the per-repo deadline only once real queries will actually resolve.
//
// The probe polls workspace/symbol until it returns a match; on the load-timeout
// it reports semantic.ErrWorkspaceNotReady so the Manager skips the sweep with an
// honest not-ready state instead of running it against a still-empty server.
// Every other server answers once initialized and does not implement
// ReadinessProber, so it never waits. Runs synchronously; ctx bounds the
// readiness budget and short-circuits an already-cancelled one.
func (p *Provider) WaitReady(ctx context.Context, repoRoot string) error {
	if !p.servesCSharp() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	if err := p.ensureClient(absRoot); err != nil {
		return err
	}
	return p.waitForCSharpSolutionLoad(ctx, absRoot)
}

// csharpSolutionLoadTimeout bounds how long WaitReady polls for the Roslyn
// solution load to finish after `initialize` returns. csharpReadinessProbeInterval
// spaces the polls. Both are var (not const) so tests can shrink them.
var (
	csharpSolutionLoadTimeout    = 120 * time.Second
	csharpReadinessProbeInterval = 750 * time.Millisecond
)

// waitForCSharpSolutionLoad blocks until the server's compilation can answer a
// workspace/symbol probe (the solution finished loading) or the load-timeout
// elapses. Bounded by the smaller of ctx and csharpSolutionLoadTimeout.
func (p *Provider) waitForCSharpSolutionLoad(ctx context.Context, repoRoot string) error {
	query, ok := csharpProbeQuery(repoRoot)
	if !ok {
		// No C# declaration to probe with — there is nothing to enrich either,
		// so treat the workspace as ready and let the (empty) pass complete.
		return nil
	}
	loadCtx, cancel := context.WithTimeout(ctx, csharpSolutionLoadTimeout)
	defer cancel()
	return p.pollWorkspaceReady(loadCtx, query)
}

// pollWorkspaceReady polls workspace/symbol until it returns at least one match
// — the signal the compilation is live — or ctx expires. On expiry it wraps
// semantic.ErrWorkspaceNotReady so the Manager can record an honest not-ready
// state and skip the sweep. A transport / RPC error counts as "still loading",
// not fatal, so a server mid-load is retried rather than abandoned.
func (p *Provider) pollWorkspaceReady(ctx context.Context, query string) error {
	ticker := time.NewTicker(csharpReadinessProbeInterval)
	defer ticker.Stop()
	for {
		if p.workspaceSymbolNonEmpty(query) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: workspace/symbol %q stayed empty", semantic.ErrWorkspaceNotReady, query)
		case <-ticker.C:
		}
	}
}

// workspaceSymbolNonEmpty issues one workspace/symbol request and reports
// whether the server returned any symbol. A nil client or any error reads as
// "not ready yet" rather than a hard failure.
func (p *Provider) workspaceSymbolNonEmpty(query string) bool {
	client := p.client
	if client == nil {
		return false
	}
	var syms []json.RawMessage
	params := struct {
		Query string `json:"query"`
	}{Query: query}
	if err := client.Call("workspace/symbol", params, &syms); err != nil {
		return false
	}
	return len(syms) > 0
}

var csharpTypeDeclRe = regexp.MustCompile(`\b(?:class|interface|struct|enum)\s+([A-Za-z_]\w*)|\brecord\s+(?:class\s+|struct\s+)?([A-Za-z_]\w*)`)
var csharpNamespaceRe = regexp.MustCompile(`\bnamespace\s+([A-Za-z_][\w.]*)`)

// csharpDeclName extracts a probeable identifier from C# source: a type name
// (class / interface / struct / record / enum) when present, else the leaf of a
// namespace declaration. Returns "" when the source has neither.
func csharpDeclName(src []byte) string {
	if m := csharpTypeDeclRe.FindSubmatch(src); m != nil {
		if len(m[1]) > 0 {
			return string(m[1])
		}
		if len(m[2]) > 0 {
			return string(m[2])
		}
	}
	if m := csharpNamespaceRe.FindSubmatch(src); m != nil {
		ns := string(m[1])
		if i := strings.LastIndexByte(ns, '.'); i >= 0 {
			ns = ns[i+1:]
		}
		return ns
	}
	return ""
}

// csharpProbeQuery walks repoRoot for the first C# type/namespace declaration
// and returns its name — a workspace/symbol query token guaranteed to match once
// the solution loads. ok=false when the tree holds no probeable declaration
// (in which case there is nothing to enrich, so readiness is moot). The walk is
// capped so a huge monorepo can't turn readiness into a full-tree scan.
func csharpProbeQuery(repoRoot string) (string, bool) {
	var found string
	scanned := 0
	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != repoRoot && (strings.HasPrefix(name, ".") || name == "bin" || name == "obj" || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".cs") {
			return nil
		}
		scanned++
		if scanned > 500 {
			return fs.SkipAll
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if name := csharpDeclName(src); name != "" {
			found = name
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// fanoutDiagnostics wakes everyone who called WaitForDiagnostics for
// this absPath AND invokes the persistent hook installed via
// SetDiagnosticsHook (if any). Runs with no provider lock held.
//
// The hook MUST NOT block — this method runs on the LSP client's
// message-pump goroutine. The MCP-level wiring uses
// `SendNotificationToAllClients` which is non-blocking by design (the
// SDK drops to an error hook when a session's notification channel is
// full).
func (p *Provider) fanoutDiagnostics(absPath string, diags []Diagnostic) {
	p.diagWaitersMu.Lock()
	waiters := p.diagWaiters[absPath]
	delete(p.diagWaiters, absPath)
	p.diagWaitersMu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- diags:
		default:
		}
	}
	p.diagHookMu.RLock()
	hook := p.diagHook
	p.diagHookMu.RUnlock()
	if hook != nil {
		hook(absPath, diags)
	}
}

// SetDiagnosticsHook installs a persistent callback invoked for every
// `textDocument/publishDiagnostics` the LSP server emits for this
// provider. Pass nil to detach. The Router uses this to forward LSP
// diagnostics to MCP clients via `notifications/diagnostics`.
//
// The hook MUST NOT block — see fanoutDiagnostics doc.
func (p *Provider) SetDiagnosticsHook(hook func(absPath string, diags []Diagnostic)) {
	p.diagHookMu.Lock()
	p.diagHook = hook
	p.diagHookMu.Unlock()
}

// DiagnosticsSnapshot returns a copy of the most recent
// publishDiagnostics payload per absolute path. Used to replay current
// state to a freshly-subscribed MCP client so it doesn't have to wait
// for the next edit to learn what's currently broken.
//
// The map is a defensive copy — callers may mutate freely.
func (p *Provider) DiagnosticsSnapshot() map[string][]Diagnostic {
	p.docMu.RLock()
	defer p.docMu.RUnlock()
	out := make(map[string][]Diagnostic, len(p.lastDiag))
	for path, diags := range p.lastDiag {
		cp := make([]Diagnostic, len(diags))
		copy(cp, diags)
		out[path] = cp
	}
	return out
}

// uriToAbsPath converts a file:// URI to an absolute filesystem path.
// Returns "" for non-file URIs or malformed input.
func uriToAbsPath(uri string) string {
	return lspuri.URIToAbsPath(uri)
}

// openDocument sends textDocument/didOpen for a file. Tracks version
// 1 in docVersions so a later didChange can monotonically bump it.
// Idempotent — a second call to openDocument with the same path is a
// no-op.
func (p *Provider) openDocument(repoRoot, relPath string) error {
	absPath := filepath.Join(repoRoot, relPath)
	p.docMu.Lock()
	if p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	p.docMu.Unlock()

	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = content

	langID := p.languageIDFor(absPath)

	if err := p.client.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: langID,
			Version:    1,
			Text:       string(content),
		},
	}); err != nil {
		return err
	}
	p.docMu.Lock()
	p.openDocs[absPath] = true
	p.docVersions[absPath] = 1
	p.docMu.Unlock()
	return nil
}

// languageIDFor picks the LSP `languageId` to send in didOpen. When
// the provider was built from a ServerSpec, the spec's per-extension
// table wins; otherwise we fall back to the first configured language.
// Final fallback is the file's extension stripped of its leading dot.
func (p *Provider) languageIDFor(absPath string) string {
	if p.spec != nil {
		ext := strings.ToLower(filepath.Ext(absPath))
		if id, ok := p.spec.LanguageIDs[ext]; ok && id != "" {
			return id
		}
	}
	if len(p.languages) > 0 {
		return p.languages[0]
	}
	if ext := strings.ToLower(filepath.Ext(absPath)); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return ""
}

// changeDocument sends textDocument/didChange with a full-text replace
// and bumps the document's version monotonically.
func (p *Provider) changeDocument(absPath, newText string) error {
	p.docMu.Lock()
	v := p.docVersions[absPath] + 1
	p.docVersions[absPath] = v
	p.docMu.Unlock()
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = []byte(newText)
	return p.client.Notify("textDocument/didChange", DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			URI:     pathToURI(absPath),
			Version: v,
		},
		ContentChanges: []TextDocumentContentChangeEvent{{Text: newText}},
	})
}

// closeDocument sends textDocument/didClose. Idempotent.
func (p *Provider) closeDocument(absPath string) error {
	p.docMu.Lock()
	if !p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	delete(p.openDocs, absPath)
	delete(p.docVersions, absPath)
	p.docMu.Unlock()
	// Drop this file's cached bytes too. getSource tolerates a miss
	// (falls back to col=0) and the next openDocument repopulates, so
	// without this an interactive session that navigates thousands of
	// files would pin every one's contents until the daemon exits.
	delete(p.sourceCache, absPath)
	return p.client.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
	})
}

// PushSimulatedContent sends a textDocument/didChange carrying
// `newText` as a full-text replacement, so the LSP server re-analyses
// the file as if it contained that buffer without anything ever
// touching disk. Used by the simulation engine (preview_edit /
// simulate_chain) to round-trip diagnostics for hypothetical edits.
// The caller is responsible for restoring the original on-disk
// content with a second PushSimulatedContent at simulation
// completion — otherwise other sessions that share this Provider
// will read diagnostics that reflect the simulated state instead of
// the saved file. EnsureFileOpen must be called first so the server
// has the document in its open-documents set; calling on an unopened
// path returns the underlying transport error.
func (p *Provider) PushSimulatedContent(absPath, newText string) error {
	return p.changeDocument(absPath, newText)
}

// LastDiagnostics returns the most recent diagnostics published for a
// file. Returns nil + false when the server has not (yet) emitted
// diagnostics for that path.
func (p *Provider) LastDiagnostics(absPath string) ([]Diagnostic, bool) {
	p.docMu.RLock()
	defer p.docMu.RUnlock()
	d, ok := p.lastDiag[absPath]
	if !ok {
		return nil, false
	}
	out := make([]Diagnostic, len(d))
	copy(out, d)
	return out, true
}

// WaitForDiagnostics blocks until the server publishes the next
// publishDiagnostics for absPath, or the timeout elapses (returning the
// last known diagnostics if any). Callers register their interest
// before triggering the change that will cause the publish, otherwise
// they may miss the event.
func (p *Provider) WaitForDiagnostics(absPath string, timeout time.Duration) []Diagnostic {
	ch := make(chan []Diagnostic, 1)
	p.diagWaitersMu.Lock()
	p.diagWaiters[absPath] = append(p.diagWaiters[absPath], ch)
	p.diagWaitersMu.Unlock()
	select {
	case d := <-ch:
		return d
	case <-time.After(timeout):
		// Drain & remove our waiter so we don't leak.
		p.diagWaitersMu.Lock()
		var kept []chan []Diagnostic
		for _, w := range p.diagWaiters[absPath] {
			if w != ch {
				kept = append(kept, w)
			}
		}
		p.diagWaiters[absPath] = kept
		p.diagWaitersMu.Unlock()
		if d, ok := p.LastDiagnostics(absPath); ok {
			return d
		}
		return nil
	}
}

// Client exposes the underlying LSP client for advanced callers (e.g.
// the daemon router). Returns nil before ensureClient succeeds.
func (p *Provider) Client() *Client { return p.client }

// EnsureClient is the exported form of ensureClient — it spawns the
// LSP server (idempotent) so callers that want diagnostics or code
// actions outside an Enrich pass can prime the connection on demand.
func (p *Provider) EnsureClient(workspaceRoot string) error {
	// filepath.Abs("") is the process's working directory. For a daemon
	// tracking many repos that is a directory belonging to none of them, so
	// an omitted root must be an error rather than a server rooted wherever
	// the daemon was launched.
	if workspaceRoot == "" {
		return fmt.Errorf("lsp %s: no workspace root supplied; pass the tracked repo's own path", p.command)
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return err
	}
	return p.ensureClient(abs)
}

// EnsureFileOpen makes sure the document is opened on the server (with
// version 1) so request methods that take a position can proceed.
func (p *Provider) EnsureFileOpen(repoRoot, relPath string) error {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	return p.openDocument(abs, relPath)
}

// EnrichNode lazily enriches a single symbol node with an LSP-grade
// semantic_type (and return_type for callables), on demand. It is the
// per-symbol counterpart to the whole-repo Enrich pass: when the synchronous
// LSP sweep is off (the default), a query tool that needs a precise type for
// one symbol calls this to fault in exactly that symbol's hover — one LSP
// round-trip, not a repo sweep — and the stamp persists in the graph so a
// second query is free.
//
// Returns (true, nil) when a type was stamped, (false, nil) when the server had
// no type at the node's position (nothing is written), and (false, err) on a
// transport failure. The file is opened on the server first (idempotent).
// ConfirmSymbolRefs confirms the incoming references to ONE symbol on demand.
// It prepares a call hierarchy at the symbol's definition and, for every
// server-verified incoming call, mints or promotes an lsp_resolved edge —
// the per-symbol unit of the whole-repo confirm + call-hierarchy sweep. A
// find_usages / get_callers answer for symbol S needs S's callers confirmed,
// not the whole repo's, so this lets lazy enrichment restore compiler-grade
// usages accuracy at query time without paying a cold-path sweep. It reuses
// recordHierarchyCall, so every hard-won correctness rule (callable-kind
// matching, per-site promotion, declaration-identity) applies unchanged.
//
// Scoped to callable nodes (the call-hierarchy unit); returns (0, nil) for
// other kinds or a server without call-hierarchy. LSP round-trips run
// unlocked; the graph mutations are batched under the resolve mutex.
func (p *Provider) ConfirmSymbolRefs(g graph.Store, repoRoot string, n *graph.Node) (int, error) {
	if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
		return 0, nil
	}
	if p.noHeavyRequests {
		// incomingCalls rides the same FindReferences machinery the sweep
		// skips for this server (ServerSpec.NoHeavyRequests) — a long-lived
		// daemon answering find_usages would accumulate the leak one query
		// at a time.
		return 0, nil
	}
	if !p.Supports("textDocument/prepareCallHierarchy") {
		return 0, nil
	}
	line, ok := lspLine(n)
	if !ok {
		return 0, nil
	}
	rel := nodeRelPath(n)
	if err := p.EnsureFileOpen(repoRoot, rel); err != nil {
		return 0, err
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return 0, err
	}
	col := 0
	if src := p.getSource(repoRoot, rel); src != nil {
		col = identifierColumn(src, n.StartLine, n.Name)
	}
	items, err := p.prepareCallHierarchy(repoRoot, rel, line, col)
	if err != nil {
		return 0, err
	}
	type hop struct {
		other  CallHierarchyItem
		ranges []Range
	}
	var hops []hop
	for _, item := range items {
		ins, ierr := p.incomingCalls(item)
		if ierr != nil {
			continue
		}
		for _, ic := range ins {
			hops = append(hops, hop{other: ic.From, ranges: ic.FromRanges})
		}
	}
	if len(hops) == 0 {
		return 0, nil
	}

	// Fetch every referenced file and every caller adjacency in two bounded
	// store calls. The per-hop loop below is then pure map work plus one batch
	// mutation, even when a hot symbol has thousands of incoming calls.
	paths := make([]string, 0, len(hops))
	seenPath := make(map[string]struct{}, len(hops))
	for _, h := range hops {
		otherPath := uriToPath(h.other.URI, absRoot)
		if otherPath == "" {
			continue
		}
		// Store rows may spell separators either way, and the file_path
		// lookup is an exact match — fetch every spelling a row may carry.
		for _, path := range storePathSpellings(n.RepoPrefix, otherPath) {
			if _, seen := seenPath[path]; seen {
				continue
			}
			seenPath[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return 0, nil
	}
	nodesByFile := g.GetFileNodesByPaths(paths)
	view := newLSPGraphView([]*graph.Node{n}, nil)
	for _, path := range paths {
		view.addNodes(nodesByFile[path])
	}
	callerIDs := make([]string, 0, len(hops))
	seenCaller := make(map[string]struct{}, len(hops))
	for _, h := range hops {
		otherPath := uriToPath(h.other.URI, absRoot)
		otherNode := view.matchCallableByFileLine(scopedPath(n.RepoPrefix, otherPath), h.other.SelectionRange.Start.Line+1)
		if otherNode == nil {
			continue
		}
		if _, seen := seenCaller[otherNode.ID]; seen {
			continue
		}
		seenCaller[otherNode.ID] = struct{}{}
		callerIDs = append(callerIDs, otherNode.ID)
	}
	for _, edges := range g.GetOutEdgesByNodeIDs(callerIDs) {
		view.addEdges(edges)
	}

	result := &semantic.EnrichResult{Provider: p.Name()}
	mutations := newLSPMutationBatch()
	rmu := g.ResolveMutex()
	rmu.Lock()
	for _, h := range hops {
		p.recordHierarchyCall(view, mutations, n.RepoPrefix, absRoot, n, h.other, false, h.ranges, result)
	}
	mutations.apply(g, nil)
	rmu.Unlock()
	return result.EdgesConfirmed + result.EdgesAdded, nil
}

func (p *Provider) EnrichNode(g graph.Store, repoRoot string, n *graph.Node) (bool, error) {
	if n == nil {
		return false, nil
	}
	line, ok := lspLine(n)
	if !ok {
		return false, nil
	}
	rel := nodeRelPath(n)
	if err := p.EnsureFileOpen(repoRoot, rel); err != nil {
		return false, err
	}
	col := 0
	if src := p.getSource(repoRoot, rel); src != nil {
		col = identifierColumn(src, n.StartLine, n.Name)
	}
	hr, err := p.hoverWith(p.client, repoRoot, rel, line, col)
	if err != nil {
		return false, err
	}
	if hr == nil {
		return false, nil
	}
	typeInfo := extractTypeFromHover(hr.Contents.Value)
	if typeInfo == "" {
		return false, nil
	}
	semantic.EnrichNodeMeta(n, "semantic_type", typeInfo, p.Name())
	g.AddBatch([]*graph.Node{n}, nil)
	return true, nil
}

// getSource returns cached file content from the most recent
// openDocument call. Returns nil when not cached — callers fall
// back to col=0 then.
func (p *Provider) getSource(repoRoot, relPath string) []byte {
	if p.sourceCache == nil {
		return nil
	}
	return p.sourceCache[filepath.Join(repoRoot, relPath)]
}

// hoverWith issues textDocument/hover against an explicit client.
// PURPOSE — race-free per-goroutine hover during concurrent enrichment.
// RATIONALE — enrichment goroutines pass the client they captured
// atomically, so a concurrent reconnect that swaps p.client never races
// an in-flight hover (the goroutine holds its own pointer value).
// KEYWORDS — lsp, hover, concurrency, reconnect
func (p *Provider) hoverWith(c *Client, repoRoot, relPath string, line, col int) (*HoverResult, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := HoverParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	p.reqStats.hovers.Add(1)
	var result HoverResult
	if err := c.Call("textDocument/hover", params, &result); err != nil {
		return nil, err
	}
	if result.Contents.Value == "" {
		return nil, nil
	}
	return &result, nil
}

// enrichOpenDoc sends a bare textDocument/didOpen against an explicit
// client without touching the shared openDocs / sourceCache tables.
// PURPOSE — per-goroutine document open for the concurrent hover phase.
// RATIONALE — each enrichment goroutine owns its document's lifecycle, so
// it must not contend on the provider-wide doc maps; the matching close
// is enrichCloseDoc, always deferred.
// KEYWORDS — lsp, didOpen, enrichment, per-goroutine
func (p *Provider) enrichOpenDoc(c *Client, absPath string, content []byte) error {
	return c.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: p.languageIDFor(absPath),
			Version:    1,
			Text:       string(content),
		},
	})
}

// enrichCloseDoc sends a bare textDocument/didClose against an explicit
// client — the counterpart to enrichOpenDoc.
// PURPOSE — release the server's per-document state immediately after a
// node's hover so simultaneously-open docs stay capped at maxParallel.
// RATIONALE — bulk-holding documents open was the enrichment OOM root
// cause; closing eagerly per goroutine bounds heap pressure.
// KEYWORDS — lsp, didClose, enrichment, lifecycle
func (p *Provider) enrichCloseDoc(c *Client, absPath string) error {
	return c.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
	})
}

// isServerExitError reports whether err signals that the language server
// process / transport is gone, so the enrichment loop should reconnect
// rather than keep hammering a dead session.
// PURPOSE — classify hover errors into "server died" vs "ordinary".
// RATIONALE — only transport/exit failures warrant a reconnect; protocol
// errors (e.g. an internal-error JSON-RPC reply) must not.
// KEYWORDS — lsp, reconnect, server-exit, error-classification
func isServerExitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"LSP server exited",
		"client is closed",
		"broken pipe",
		"connection reset",
		"use of closed network connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// reconnectWithBackoff rebuilds the LSP client after the server exits
// mid-enrichment. It retries connectOnce with exponential backoff
// (dialBackoffStart → maxDialBackoff) up to maxReconnectAttempts and
// returns the fresh client, or an error if every attempt failed.
// PURPOSE — automatic recovery so one mid-flight crash doesn't fail every
// remaining hover.
// RATIONALE — callers hold reconnectMu, so exactly one reconnection runs
// at a time; backoff prevents a tight retry loop against a persistently
// dead server.
// KEYWORDS — lsp, reconnect, backoff, recovery
// maxEnrichReconnectCycles caps how many times a single enrichment pass will
// reconnect to a provider that keeps exiting mid-request. reconnectWithBackoff
// (below) caps the connection retries within ONE cycle; this caps the number of
// cycles, so a server that reconnects cleanly but crashes again on the next
// request can't pin the pass in an endless crash -> reconnect -> crash loop.
const maxEnrichReconnectCycles = 5

func (p *Provider) reconnectWithBackoff(absRoot string) (*Client, error) {
	const maxReconnectAttempts = 5
	backoff := p.dialBackoffStart
	if backoff <= 0 {
		backoff = dialBackoffStart
	}
	maxBackoff := p.maxDialBackoff
	if maxBackoff <= 0 {
		maxBackoff = maxDialBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
		p.reconnectAttempts.Add(1)
		p.logger.Warn("LSP enrich: reconnecting after server exit",
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxReconnectAttempts),
		)
		if err := p.connectOnce(absRoot); err != nil {
			lastErr = err
			p.logger.Warn("LSP enrich: reconnect attempt failed",
				zap.Int("attempt", attempt), zap.Error(err))
			time.Sleep(backoff)
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		p.logger.Info("LSP enrich: reconnected", zap.Int("attempt", attempt))
		return p.client, nil
	}
	return nil, fmt.Errorf("LSP reconnect failed after %d attempts: %w", maxReconnectAttempts, lastErr)
}

// findImplementations queries textDocument/implementation.
func (p *Provider) findImplementations(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ImplementationParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	p.reqStats.implementations.Add(1)
	var locations []Location
	if err := p.client.Call("textDocument/implementation", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// CodeActionsRequest carries the params for a single
// textDocument/codeAction call.
type CodeActionsRequest struct {
	// AbsPath is the absolute path to the file the cursor is in.
	AbsPath string
	// Range narrows the request. Pass {} for the whole file.
	Range Range
	// Diagnostics is the set of diagnostics the actions should
	// address — typically a recent slice from LastDiagnostics.
	Diagnostics []Diagnostic
	// Only restricts the kind of actions returned (e.g.
	// CodeActionKindQuickFix, CodeActionKindSourceOrganizeImports).
	Only []string
}

// GetCodeActions issues textDocument/codeAction and returns a unified
// list of CodeActionOrCommand. The provider must already have opened
// the document via EnsureFileOpen before calling this.
func (p *Provider) GetCodeActions(req CodeActionsRequest) ([]CodeActionOrCommand, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := CodeActionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(req.AbsPath)},
		Range:        req.Range,
		Context: CodeActionContext{
			Diagnostics: req.Diagnostics,
			Only:        req.Only,
			TriggerKind: 1, // Invoked.
		},
	}
	var raw []json.RawMessage
	if err := p.client.Call("textDocument/codeAction", params, &raw); err != nil {
		return nil, err
	}
	out := make([]CodeActionOrCommand, 0, len(raw))
	for _, item := range raw {
		var u CodeActionOrCommand
		if err := json.Unmarshal(item, &u); err != nil {
			continue
		}
		// Legacy Command form has the shape {title, command, arguments}.
		// CodeAction literal has {title, kind?, edit?, command?, ...}.
		// json.Unmarshal handles both with the unified struct above.
		out = append(out, u)
	}
	return out, nil
}

// ResolveCodeAction calls codeAction/resolve. Some servers (rust-
// analyzer, jdtls) defer the heavy WorkspaceEdit computation until
// resolve to keep the initial codeAction call cheap.
func (p *Provider) ResolveCodeAction(action CodeActionOrCommand) (CodeActionOrCommand, error) {
	if p.client == nil {
		return action, fmt.Errorf("LSP client not initialized")
	}
	var resolved CodeActionOrCommand
	if err := p.client.Call("codeAction/resolve", action, &resolved); err != nil {
		return action, err
	}
	return resolved, nil
}

// ExecuteCommand issues workspace/executeCommand. Used by the
// apply-code-action path when a CodeAction has only a Command
// (legacy) form.
func (p *Provider) ExecuteCommand(cmd Command) (json.RawMessage, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := ExecuteCommandParams{Command: cmd.Command, Arguments: cmd.Arguments}
	var result json.RawMessage
	if err := p.client.Call("workspace/executeCommand", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// findReferences queries textDocument/references.
func (p *Provider) findReferences(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
		Context: ReferenceContext{IncludeDeclaration: false},
	}

	p.reqStats.references.Add(1)
	var locations []Location
	if err := p.client.Call("textDocument/references", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// FindDefinition queries textDocument/definition with a per-call
// timeout so a stalled LSP can't block the resolve-time hot path.
// Returns the locations reported by the server (typically one) or
// an error / empty slice on timeout, no-result, or transport failure.
//
// repoRoot is the absolute workspace root; relPath is repo-relative.
// (line, col) are 0-based, matching LSP convention.
func (p *Provider) FindDefinition(repoRoot, relPath string, line, col int, timeout time.Duration) ([]Location, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialised")
	}
	absPath := filepath.Join(repoRoot, relPath)
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
		Position:     Position{Line: line, Character: col},
	}
	p.reqStats.definitions.Add(1)

	type result struct {
		locations []Location
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		// Tsserver replies with either a single Location, an array of
		// Location, or null. The unified handling: try array first
		// (most common), fall back to single Location on unmarshal
		// error.
		var raw json.RawMessage
		if err := p.client.Call("textDocument/definition", params, &raw); err != nil {
			ch <- result{nil, err}
			return
		}
		if len(raw) == 0 || string(raw) == "null" {
			ch <- result{nil, nil}
			return
		}
		var locs []Location
		if err := json.Unmarshal(raw, &locs); err == nil {
			ch <- result{locs, nil}
			return
		}
		var single Location
		if err := json.Unmarshal(raw, &single); err == nil {
			ch <- result{[]Location{single}, nil}
			return
		}
		ch <- result{nil, fmt.Errorf("unexpected definition response shape")}
	}()

	if timeout <= 0 {
		r := <-ch
		return r.locations, r.err
	}
	select {
	case r := <-ch:
		return r.locations, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("textDocument/definition: timeout after %s", timeout)
	}
}

// IdentifierColumn is the exported form of the package-internal
// identifierColumn helper. Resolve-time callers (the resolver's LSP
// hot path) need the 0-based column for a given identifier on a
// given 1-based line to satisfy LSP servers that require the cursor
// to sit on the identifier.
func IdentifierColumn(src []byte, oneBasedLine int, name string) int {
	return identifierColumn(src, oneBasedLine, name)
}

// Source returns the cached source for relPath under repoRoot, or
// nil when the document has not been opened. Exported so the
// resolve-time helper can compute identifier columns without
// re-reading the file from disk.
func (p *Provider) Source(repoRoot, relPath string) []byte {
	return p.getSource(repoRoot, relPath)
}

// recordHierarchyCall lands one call-hierarchy hop into the graph.
// asOutgoing=true means "this node calls other"; false means "other
// calls this node" (incoming-calls direction). Existing edges get
// promoted to lsp_resolved; missing edges get added.
//
// fromRanges carries the call-expression ranges the server reported
// for this hop — per the LSP spec they always live in the CALLER's
// file, for both directions. New edges are stamped at those lines
// (one edge per distinct call-site line), so a synthesized
// interface-dispatch edge points at the `b.Name()` expression, not
// at the calling function's declaration line. When the server
// returned no ranges, the caller's declaration line remains the
// fallback anchor.
func (p *Provider) recordHierarchyCall(view *lspGraphView, mutations *lspMutationBatch, repoPrefix, absRoot string, n *graph.Node, other CallHierarchyItem, asOutgoing bool, fromRanges []Range, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := view.matchCallableByFileLine(scopedPath(repoPrefix, otherPath),
		other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := n, otherNode
	if !asOutgoing {
		from, to = otherNode, n
	}
	if from.ID == to.ID {
		return
	}

	var existing *graph.Edge
	pairEdges := 0
	byLine := map[int][]*graph.Edge{}
	for _, e := range view.outByID[from.ID] {
		if e.Kind != graph.EdgeCalls || e.To != to.ID {
			continue
		}
		byLine[e.Line] = append(byLine[e.Line], e)
		pairEdges++
		if existing == nil {
			existing = e
		}
	}
	promote := func(e *graph.Edge) {
		if graph.OriginRank(e.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(e, p.Name())
			e.Origin = graph.OriginLSPResolved
			mutations.stagePersist(e)
			result.EdgesConfirmed++
		}
	}

	lines := callSiteLines(fromRanges)
	if len(lines) == 0 {
		if existing != nil {
			if pairEdges == 1 {
				promote(existing)
			}
			return
		}
		edge := newLSPResolvedEdge(from.ID, to.ID, graph.EdgeCalls,
			from.FilePath, from.StartLine, p.Name(), graph.OriginLSPResolved)
		if mutations.stageAdd(view, edge) {
			result.EdgesAdded++
		}
		return
	}
	for _, line := range lines {
		if es, ok := byLine[line]; ok {
			for _, e := range es {
				promote(e)
			}
			continue
		}
		edge := newLSPResolvedEdge(from.ID, to.ID, graph.EdgeCalls,
			from.FilePath, line, p.Name(), graph.OriginLSPResolved)
		if mutations.stageAdd(view, edge) {
			result.EdgesAdded++
		}
	}
}

// callSiteLines lowers a hop's fromRanges to the distinct, sorted
// 1-based start lines of the call expressions. Zero-valued / negative
// lines are dropped.
func callSiteLines(ranges []Range) []int {
	if len(ranges) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(ranges))
	lines := make([]int, 0, len(ranges))
	for _, r := range ranges {
		line := r.Start.Line + 1
		if line <= 0 || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	sort.Ints(lines)
	return lines
}

// linkTypeHierarchy emits the right edge kind for one super/subtype
// hop. When asSupertype=true, the hop is `cur → other` (cur extends
// or implements other). When false, the hop is `other → cur`.
//
// Beyond the type-level edge, it also walks the methods of the child
// type (the `from` side) and emits EdgeOverrides for every method
// whose name matches a method on the parent — closing the
// method-level half of the type hierarchy (Joern calls these
// CONTAINS + OVERRIDES).
func (p *Provider) linkTypeHierarchy(view *lspGraphView, mutations *lspMutationBatch, repoPrefix, absRoot string, cur *graph.Node, other TypeHierarchyItem, asSupertype bool, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := view.matchNodeByFileLine(scopedPath(repoPrefix, otherPath), other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := cur, otherNode
	if !asSupertype {
		from, to = otherNode, cur
	}
	kind := graph.EdgeExtends
	origin := graph.OriginLSPResolved
	if to.Kind == graph.KindInterface {
		kind = graph.EdgeImplements
		origin = graph.OriginLSPDispatch
	}
	if from.ID == to.ID {
		return
	}
	existing := view.findMatchingEdge(from.ID, to.ID, kind)
	if existing != nil {
		if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(existing, p.Name())
			existing.Origin = graph.OriginLSPResolved
			mutations.stagePersist(existing)
			result.EdgesConfirmed++
		}
	} else {
		edge := newLSPResolvedEdge(from.ID, to.ID, kind, from.FilePath, from.StartLine, p.Name(), origin)
		if mutations.stageAdd(view, edge) {
			result.EdgesAdded++
		}
	}

	addOverrideEdgesBatch(view, mutations, from, to, p.Name(), graph.OriginLSPDispatch, result)
}

// addOverrideEdges emits EdgeOverrides from each method of child to
// the matching method of parent (matched by name). Parent methods are
// resolved via EdgeMemberOf (`m -member_of-> parent`) so the routine
// works regardless of language as long as the AST extractor recorded
// member_of for methods.
//
// origin lets the caller stamp the edges with lsp_dispatch (LSP-
// confirmed parent), ast_resolved (AST-confirmed parent in the same
// compilation unit), or ast_inferred (parent is a heuristic match).
func addOverrideEdges(g graph.Store, child, parent *graph.Node, provider, origin string, result *semantic.EnrichResult) {
	if child == nil || parent == nil || child.ID == parent.ID {
		return
	}
	ownerIDs := []string{child.ID, parent.ID}
	inByID := g.GetInEdgesByNodeIDs(ownerIDs)
	methodSet := make(map[string]struct{})
	edges := make([]*graph.Edge, 0)
	for _, id := range ownerIDs {
		for _, edge := range inByID[id] {
			edges = append(edges, edge)
			if edge.Kind == graph.EdgeMemberOf {
				methodSet[edge.From] = struct{}{}
			}
		}
	}
	methodIDs := make([]string, 0, len(methodSet))
	for id := range methodSet {
		methodIDs = append(methodIDs, id)
	}
	methodNodes := g.GetNodesByIDs(methodIDs)
	nodes := []*graph.Node{child, parent}
	for _, id := range methodIDs {
		if n := methodNodes[id]; n != nil {
			nodes = append(nodes, n)
		}
	}
	for _, out := range g.GetOutEdgesByNodeIDs(methodIDs) {
		edges = append(edges, out...)
	}
	view := newLSPGraphView(nodes, edges)
	mutations := newLSPMutationBatch()
	rmu := g.ResolveMutex()
	rmu.Lock()
	addOverrideEdgesBatch(view, mutations, child, parent, provider, origin, result)
	mutations.apply(g, nil)
	rmu.Unlock()
}

func addOverrideEdgesBatch(view *lspGraphView, mutations *lspMutationBatch, child, parent *graph.Node, provider, origin string, result *semantic.EnrichResult) {
	if child == nil || parent == nil || child.ID == parent.ID {
		return
	}
	parentMethods := map[string]*graph.Node{}
	for _, e := range view.inByID[parent.ID] {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := view.nodesByID[e.From]
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		parentMethods[m.Name] = m
	}
	if len(parentMethods) == 0 {
		return
	}
	for _, e := range view.inByID[child.ID] {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := view.nodesByID[e.From]
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		pm, ok := parentMethods[m.Name]
		if !ok || pm.ID == m.ID {
			continue
		}
		existing := view.findMatchingEdge(m.ID, pm.ID, graph.EdgeOverrides)
		if existing != nil {
			if graph.OriginRank(existing.Origin) < graph.OriginRank(origin) {
				semantic.ConfirmEdge(existing, provider)
				existing.Origin = origin
				mutations.stagePersist(existing)
				if result != nil {
					result.EdgesConfirmed++
				}
			}
			continue
		}
		edge := newLSPResolvedEdge(m.ID, pm.ID, graph.EdgeOverrides, m.FilePath, m.StartLine, provider, origin)
		if mutations.stageAdd(view, edge) && result != nil {
			result.EdgesAdded++
		}
	}
}

// languageMatches returns true when n.Language is one of the
// languages this provider serves.
func (p *Provider) languageMatches(lang string) bool {
	for _, l := range p.languages {
		if l == lang {
			return true
		}
	}
	return false
}

// servesFile reports whether relPath's extension is one this provider's server
// can actually compile — the guard that stops enrichment from ever sending a
// language server a file outside its coverage. Without it, an ambiguous edge
// or interface whose file is an asset (.png/.svg) or an unrelated script (.sh)
// would be opened on, say, clangd, which then tries to build an AST with an
// inferred C++ compile command and logs invalid-AST churn for zero signal.
// With no ServerSpec (unit-test providers) it defaults to true so fakes that
// drive arbitrary extensions are unaffected.
func (p *Provider) servesFile(relPath string) bool {
	if p.spec == nil || len(p.spec.Extensions) == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	if ext == "" {
		return false
	}
	for _, e := range p.spec.Extensions {
		if strings.ToLower(e) == ext {
			return true
		}
	}
	return false
}

// repoScopedNodes returns the exact repo projection. Empty prefix denotes the
// embedded single-repository namespace; Store implementations handle it as an
// exact predicate, never as a request for a graph-wide snapshot.
func (p *Provider) repoScopedNodes(g graph.Store, repoPrefix string) []*graph.Node {
	return g.GetRepoNodes(repoPrefix)
}

// repoScopedNodesLight fetches this repo's nodes via the store's optional
// LightNodeReader fast path when available — skipping the meta-blob decode
// for the majority of nodes that are already fully enriched — falling back
// to the full repoScopedNodes scan otherwise. The bool result reports
// whether the light path was taken: when true, the returned nodes must not
// be round-tripped back through AddBatch as-is (see graph.LightNodeReader);
// the caller re-fetches in full whatever subset it intends to stamp.
func (p *Provider) repoScopedNodesLight(g graph.Store, repoPrefix string) ([]*graph.Node, bool) {
	if lr, ok := g.(graph.LightNodeReader); ok {
		return lr.GetRepoNodesLight(repoPrefix), true
	}
	return p.repoScopedNodes(g, repoPrefix), false
}

// repoScopedEdges returns the edges whose source belongs to repoPrefix. The
// multi-repo path uses the compound store query. GetRepoEdges intentionally
// treats an empty prefix as no scope, so the embedded path resolves its exact
// empty-prefix nodes once and performs one batched adjacency lookup.
func (p *Provider) repoScopedEdges(g graph.Store, repoPrefix string) []*graph.Edge {
	if repoPrefix != "" {
		return g.GetRepoEdges(repoPrefix)
	}
	nodes := g.GetRepoNodes("")
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			ids = append(ids, node.ID)
		}
	}
	bySource := g.GetOutEdgesByNodeIDs(ids)
	var edges []*graph.Edge
	for _, id := range ids {
		edges = append(edges, bySource[id]...)
	}
	return edges
}

// prepareCallHierarchy queries textDocument/prepareCallHierarchy and
// returns the items the server resolved at the given position. Empty
// (and nil error) means the server doesn't recognise a function-like
// symbol at that location.
func (p *Provider) prepareCallHierarchy(repoRoot, relPath string, line, col int) ([]CallHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := CallHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	p.reqStats.prepareCallHierarchy.Add(1)
	var items []CallHierarchyItem
	if err := p.client.Call("textDocument/prepareCallHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// outgoingCalls queries callHierarchy/outgoingCalls for one item.
func (p *Provider) outgoingCalls(item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	p.reqStats.outgoingCalls.Add(1)
	var calls []CallHierarchyOutgoingCall
	if err := p.client.Call("callHierarchy/outgoingCalls",
		CallHierarchyOutgoingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// incomingCalls queries callHierarchy/incomingCalls for one item.
func (p *Provider) incomingCalls(item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	p.reqStats.incomingCalls.Add(1)
	var calls []CallHierarchyIncomingCall
	if err := p.client.Call("callHierarchy/incomingCalls",
		CallHierarchyIncomingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// prepareTypeHierarchy queries textDocument/prepareTypeHierarchy.
func (p *Provider) prepareTypeHierarchy(repoRoot, relPath string, line, col int) ([]TypeHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := TypeHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	p.reqStats.prepareTypeHierarchy.Add(1)
	var items []TypeHierarchyItem
	if err := p.client.Call("textDocument/prepareTypeHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// supertypes queries typeHierarchy/supertypes.
func (p *Provider) supertypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	p.reqStats.supertypes.Add(1)
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/supertypes",
		TypeHierarchySupertypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// subtypes queries typeHierarchy/subtypes.
func (p *Provider) subtypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	p.reqStats.subtypes.Add(1)
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/subtypes",
		TypeHierarchySubtypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// pathToURI converts a file path to a file:// URI (Windows-correct).
func pathToURI(path string) string {
	return lspuri.PathToURI(path)
}

// buildWorkspaceFolders returns the LSP workspaceFolders list — the
// primary root followed by any additional roots.
func buildWorkspaceFolders(primary string, additional []string) []WorkspaceFolder {
	folders := make([]WorkspaceFolder, 0, len(additional)+1)
	folders = append(folders, WorkspaceFolder{
		URI:  pathToURI(primary),
		Name: filepath.Base(primary),
	})
	for _, f := range additional {
		if f == "" {
			continue
		}
		if abs, err := filepath.Abs(f); err == nil {
			f = abs
		}
		folders = append(folders, WorkspaceFolder{
			URI:  pathToURI(f),
			Name: filepath.Base(f),
		})
	}
	return folders
}

// uriToPath converts a file:// URI to a repo-relative path (Windows-correct).
func uriToPath(uri, repoRoot string) string {
	return lspuri.URIToRepoRel(uri, repoRoot)
}

// lspLine converts a node's 1-based StartLine to the 0-based line LSP
// positions use, reporting ok=false for nodes without a real source
// position (StartLine < 1 — synthetic module/package nodes and extractor
// fallbacks). Sending such a node would put position.line == -1 on the
// wire, which servers reject per request (gopls: "cannot unmarshal
// number -1 into ... position.line of type uint32") — a guaranteed
// wasted round trip. Skipping beats clamping to line 0: the identifier
// is not there, so a clamped request would at best return junk for a
// different symbol.
func lspLine(n *graph.Node) (int, bool) {
	if n.StartLine < 1 {
		return 0, false
	}
	return n.StartLine - 1, true
}

// identifierColumn returns the 0-based column of the first
// occurrence of name on the given 1-based line of src. Returns 0
// when the source doesn't have the line, the name isn't found on
// it, or name is empty — col=0 was the previous unconditional
// default and remains a safe fallback for those edge cases.
//
// Why this matters: most LSP servers (gopls, jdtls, rust-analyzer,
// kotlin-ls, omnisharp, pyright) require the position cursor to be
// _on_ the identifier for textDocument/references and
// textDocument/implementation. Pinning to col=0 silently empty-resulted
// every method declaration in indented contexts (`func (f *Foo) Bar()`
// — col=0 is the `func` keyword, not `Bar`). Resolving to the actual
// identifier column unblocks the bulk of cross-file edge promotion.
func identifierColumn(src []byte, oneBasedLine int, name string) int {
	col, _ := identifierColumnStrict(src, oneBasedLine, name)
	return col
}

// identifierColumnStrict is identifierColumn with an explicit found
// flag: (0, false) when the source has no such line or the line does
// not contain the whole identifier. Callers that would otherwise fire
// an LSP request at a junk position (column 0 of an unrelated token)
// can skip the round trip instead.
func identifierColumnStrict(src []byte, oneBasedLine int, name string) (int, bool) {
	if name == "" || oneBasedLine <= 0 || len(src) == 0 {
		return 0, false
	}
	// Walk to the start of the requested line.
	target := oneBasedLine - 1
	lineStart := 0
	cur := 0
	for cur < len(src) && target > 0 {
		if src[cur] == '\n' {
			target--
			lineStart = cur + 1
		}
		cur++
	}
	if target > 0 {
		return 0, false
	}
	lineEnd := lineStart
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	line := string(src[lineStart:lineEnd])
	idx := identifierIndex(line, name)
	if idx < 0 {
		return 0, false
	}
	return idx, true
}

// identifierIndex returns the column of the first occurrence of name in
// line that is a WHOLE identifier — not a substring of a longer one. The
// naive scan silently targeted the wrong symbol whenever a method's name
// is contained in its receiver type: for `func (formBinding) Bind(...)`
// the first "Bind" sits inside "formBinding", so hover /
// prepareCallHierarchy / implementations were asked about the TYPE
// identifier — prepare returned no items and the entire incoming-call
// fan-out never ran for that method (every implementation of a
// same-named interface method lost all its dispatch callers).
func identifierIndex(line, name string) int {
	if name == "" {
		return -1
	}
	for from := 0; from+len(name) <= len(line); {
		idx := strings.Index(line[from:], name)
		if idx < 0 {
			return -1
		}
		idx += from
		beforeOK := idx == 0 || !isIdentByte(line[idx-1])
		afterOK := idx+len(name) == len(line) || !isIdentByte(line[idx+len(name)])
		if beforeOK && afterOK {
			return idx
		}
		from = idx + 1
	}
	return -1
}

// isIdentByte reports whether c can appear inside an ASCII identifier.
// Multi-byte runes are treated as boundaries — a heuristic that errs
// toward accepting a match, which is still strictly tighter than the
// unbounded substring scan this replaces.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// extractTypeFromHover extracts type information from hover text.
func extractTypeFromHover(hover string) string {
	// Remove markdown code fences.
	hover = strings.TrimPrefix(hover, "```go\n")
	hover = strings.TrimPrefix(hover, "```java\n")
	hover = strings.TrimPrefix(hover, "```\n")
	hover = strings.TrimSuffix(hover, "\n```")
	hover = strings.TrimSpace(hover)

	lines := strings.SplitN(hover, "\n", 2)
	if len(lines) > 0 {
		line := strings.TrimSpace(lines[0])
		// Go keywords
		if strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") ||
			strings.HasPrefix(line, "var ") ||
			strings.HasPrefix(line, "const ") ||
			strings.HasPrefix(line, "field ") ||
			strings.HasPrefix(line, "package ") {
			return line
		}
		// Java keywords / modifiers — jdtls hover format:
		//   "public class Foo", "void bar()", "private String baz",
		//   "abstract class X", "interface Y", "@Deprecated",
		//   "static final int N", "enum Color", "protected Object"
		if strings.HasPrefix(line, "public ") ||
			strings.HasPrefix(line, "private ") ||
			strings.HasPrefix(line, "protected ") ||
			strings.HasPrefix(line, "abstract ") ||
			strings.HasPrefix(line, "static ") ||
			strings.HasPrefix(line, "final ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "interface ") ||
			strings.HasPrefix(line, "enum ") ||
			strings.HasPrefix(line, "void ") ||
			strings.HasPrefix(line, "@") {
			return line
		}
		// Short type like "string", "*Foo", "[]byte", "int", "boolean".
		if !strings.Contains(line, " ") && len(line) > 0 && len(line) < 100 {
			return line
		}
	}
	return ""
}
