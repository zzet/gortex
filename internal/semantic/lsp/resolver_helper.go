package lsp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/lspuri"
)

// ResolverHelper adapts one or more *Provider instances for resolve-
// time use by the cross-file resolver. The resolver consults this
// helper as part of the hot path for every TS/JS/JSX/TSX edge (see
// internal/resolver/lsp_resolve.go). Compared to the enricher path
// (Provider.Enrich), the helper holds the language server warm
// across the whole resolve pass and applies a per-call timeout so a
// stalled server never gates the resolve.
//
// Two ownership contracts live behind this one type, and they differ
// in exactly one respect — who owns the *Provider:
//
//   - Router-backed (NewLazyResolverHelper). The helper owns NO
//     provider. Every Definition call re-runs the lookup closure
//     (Router.ForSpecWorkspace) to acquire the provider for that one
//     call. On a router cache hit that lookup is a map read which also
//     refreshes the router's lastUsed, so a provider under active
//     resolve traffic is never reaped mid-pass; when the router has
//     already reaped or LRU-evicted it, the lookup spawns a fresh,
//     router-TRACKED replacement. Caching the pointer across calls
//     (what this helper used to do) let the router drop the provider
//     from its map while the helper kept the only reference: the next
//     EnsureClient saw a dead client and re-spawned the LSP subprocess
//     behind the router's back, leaking a process tree that nothing
//     could ever reap for the life of the daemon.
//   - Helper-owned (NewPooledResolverHelper with poolSize > 1, and
//     NewResolverHelper with a concrete provider). The helper spawns
//     and owns its providers and borrows one per call from a pool
//     channel. These providers sit outside the router's bookkeeping by
//     construction, so an in-place respawn inside EnsureClient is the
//     helper's own business; Close shuts them down.
//
// Concurrency model: one Definition call uses exactly one provider at
// a time. Router-backed mode holds h.mu across the whole call — the
// router hands every caller the same *Provider and a provider's stdio
// is single-threaded, and pool size is 1 in that mode anyway, so this
// matches the throughput of the borrow channel it replaced.
// Owned-pool mode borrows from a channel of N independent providers
// (= N tsserver processes for the TS spec), giving mutual exclusion
// per provider and parallelism across them.
//
// Lifecycle:
//   - Constructed once per (workspace, language family) at index time.
//   - Owned-pool mode lazy-spawns N underlying LSP subprocesses on the
//     first Definition call. Router-backed mode spawns nothing of its
//     own — the router does, on demand, inside its own reap/evict
//     accounting.
//   - Caches no answers across passes — the resolver owns dedup via
//     its lspIndex.
//
// Memory note: each pooled provider opens files independently via
// EnsureFileOpen. For workspaces with thousands of hot source files
// the per-provider state can add up; the pool size knob trades
// throughput for tsserver memory.
type ResolverHelper struct {
	// routerBacked marks the NewLazyResolverHelper flavour, whose
	// provider belongs to the LSP router rather than to this helper.
	// It selects the per-call lookup path in acquire and makes Close a
	// no-op. See the type comment for why the two modes must differ.
	routerBacked bool

	// mu serialises Definition in router-backed mode, where every call
	// runs against the single router-cached provider for this (spec,
	// workspace) pair. Unused in owned-pool mode — the pool channel
	// already provides the same per-provider exclusion there.
	mu sync.Mutex

	// spawnOnce gates the lazy creation of the provider pool so the
	// underlying LSP processes aren't started until the first
	// Definition call lands. Owned-pool mode only; router-backed mode
	// never touches the pool.
	spawnOnce sync.Once
	spawnErr  error

	// poolSize is the number of providers to spawn. Zero is treated
	// as 1 (single-provider, mu-serialised) for back-compat with the
	// pre-pool ResolverHelper API.
	poolSize int

	// pool is a buffered channel of borrowable providers. Capacity =
	// poolSize. Definition takes a provider off the channel, uses it,
	// and puts it back. Allocated inside spawnPool under spawnOnce.
	pool chan *Provider

	// providers is the master slice of pooled providers, retained so
	// Close can shut them down without draining the pool channel
	// (which may have providers in flight).
	providers []*Provider

	// spawnFn produces the *Provider a call runs against. Owned-pool
	// mode requires a FRESH, fully-initialised provider per call and
	// invokes it poolSize times at pool construction. Router-backed
	// mode invokes it once per Definition call — a router cache hit is
	// a cheap map read, and going through it every time is precisely
	// what keeps the router's idle/LRU accounting honest.
	// At most one of spawnFn and provider is set at construction.
	spawnFn func() (*Provider, error)

	// provider is the pre-supplied provider in eager-construction
	// mode (NewResolverHelper). When set, the pool collapses to size
	// 1 and the channel holds this one entry. Mutually exclusive
	// with spawnFn.
	provider *Provider

	workspaceRoot string

	// extensions is the set of lowercase file extensions (with
	// leading dot) the helper claims. Populated from the spec at
	// construction time so SupportsPath can short-circuit without
	// touching the provider lock.
	extensions map[string]struct{}

	// timeout caps each textDocument/definition call. tsserver
	// usually answers in <100 ms on warm buffers, but a cold
	// project load can take seconds. 1500 ms is a conservative
	// per-call budget: long enough for typical warm answers,
	// short enough that the parallel resolver doesn't stall on a
	// genuinely-broken server.
	timeout time.Duration

	logger *zap.Logger
}

// NewResolverHelper wraps a Provider for resolve-time use. The
// helper claims every extension the underlying spec declares (when
// the provider was constructed from a ServerSpec); otherwise it
// claims the TS-family extensions by default, matching the N5
// initial scope.
//
// workspaceRoot is the absolute path the LSP server is initialised
// against. timeout caps each definition call; pass 0 to apply the
// default (1500 ms).
//
// The pool collapses to size 1 — call NewPooledResolverHelper to get
// the multi-provider parallel mode.
func NewResolverHelper(provider *Provider, workspaceRoot string, timeout time.Duration, logger *zap.Logger) *ResolverHelper {
	if logger == nil {
		logger = zap.NewNop()
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}

	exts := make(map[string]struct{})
	if provider != nil && provider.spec != nil {
		for _, e := range provider.spec.Extensions {
			exts[strings.ToLower(e)] = struct{}{}
		}
	}
	if len(exts) == 0 {
		// Default TS-family scope — matches N5 initial coverage.
		for _, e := range []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"} {
			exts[e] = struct{}{}
		}
	}

	h := &ResolverHelper{
		provider:      provider,
		poolSize:      1,
		workspaceRoot: workspaceRoot,
		extensions:    exts,
		timeout:       timeout,
		logger:        logger,
	}
	// Pre-fire spawnOnce since the provider is already concrete: seed
	// the pool channel with the single supplied provider so the first
	// Definition call doesn't try to spawn.
	h.spawnOnce.Do(func() {
		h.pool = make(chan *Provider, 1)
		if provider != nil {
			h.providers = []*Provider{provider}
			h.pool <- provider
		}
	})
	return h
}

// NewLazyResolverHelper builds the router-backed helper: pass a
// closure that calls Router.ForSpecWorkspace (or equivalent) and the
// helper will run it on EVERY Definition call rather than caching the
// resulting *Provider.
//
// Per-call lookup is the contract, not an inefficiency. The router
// owns provider lifetime — idle reaping and LRU eviction both Close
// providers and delete them from its map — so a helper holding a
// cached pointer would keep driving a provider the router no longer
// tracks, and Provider.EnsureClient would silently re-spawn the
// language server as an untracked orphan. Re-asking the router is a
// map read on the hot path (it also refreshes lastUsed, which is what
// stops the reaper from taking a provider mid-pass), and returns a
// fresh router-tracked provider when the previous one is gone.
//
// Lookup errors are NOT sticky: the router's own markSpawnFailed
// already fails a genuinely broken server fast for every subsequent
// call, so per-call lookup cannot turn into a respawn storm, and a
// helper poisoned by one transient failure would stay blind for the
// life of the daemon.
//
// extensions narrows the set of file extensions the helper claims
// without consulting the router at all. Pass nil to use the default
// TS-family set.
func NewLazyResolverHelper(lookup func() (*Provider, error), workspaceRoot string, extensions []string, timeout time.Duration, logger *zap.Logger) *ResolverHelper {
	h := NewPooledResolverHelper(lookup, workspaceRoot, extensions, timeout, 1, logger)
	h.routerBacked = true
	return h
}

// NewPooledResolverHelper builds a helper backed by `poolSize`
// independent providers. Each Definition call borrows one provider
// from the pool, so up to poolSize concurrent tsserver Definition
// requests run in parallel — eliminating the single-mutex throughput
// ceiling that dominated multi-repo resolve-time profiles (29 min
// `deferred_passes_all` on a 488-repo TS-heavy workspace).
//
// The providers are owned by the helper — they are NOT registered
// with the LSP router, so nothing idle-reaps them and Close is the
// only thing that shuts them down. NewLazyResolverHelper wraps this
// constructor with poolSize 1 and flips the helper to the
// router-backed contract instead (per-call lookup, no ownership).
//
// spawn must produce a fresh, fully-initialised provider each call.
// For the typical owned-pool wiring the closure is something like:
//
//	func() (*Provider, error) {
//	    p := lsp.NewProviderFromSpec(spec, logger)
//	    if err := p.EnsureClient(absRoot); err != nil { return nil, err }
//	    return p, nil
//	}
//
// poolSize ≤ 0 falls back to the default (4) — large enough to
// saturate the resolver's worker pool on commodity CPU counts, small
// enough that the per-workspace tsserver memory footprint stays
// bounded.
func NewPooledResolverHelper(
	spawn func() (*Provider, error),
	workspaceRoot string,
	extensions []string,
	timeout time.Duration,
	poolSize int,
	logger *zap.Logger,
) *ResolverHelper {
	if logger == nil {
		logger = zap.NewNop()
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	if poolSize <= 0 {
		poolSize = 4
	}
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}

	exts := make(map[string]struct{})
	for _, e := range extensions {
		exts[strings.ToLower(e)] = struct{}{}
	}
	if len(exts) == 0 {
		for _, e := range []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"} {
			exts[e] = struct{}{}
		}
	}

	return &ResolverHelper{
		spawnFn:       spawn,
		poolSize:      poolSize,
		workspaceRoot: workspaceRoot,
		extensions:    exts,
		timeout:       timeout,
		logger:        logger,
	}
}

// ensurePool spawns the pool's providers on first call, populating
// the borrow channel. Subsequent calls are a no-op. Returns the
// cached error when spawn failed — Definition then short-circuits.
//
// Owned-pool / eager mode only. Router-backed helpers never call it:
// they own no providers, so there is no pool to fill and no spawn
// error worth remembering (see NewLazyResolverHelper).
func (h *ResolverHelper) ensurePool() error {
	h.spawnOnce.Do(func() {
		// Eager-construction path (NewResolverHelper): the pool was
		// pre-seeded with the supplied provider when the helper was
		// constructed. Nothing more to do.
		if h.spawnFn == nil {
			return
		}
		size := h.poolSize
		if size <= 0 {
			size = 1
		}
		pool := make(chan *Provider, size)
		providers := make([]*Provider, 0, size)
		for i := 0; i < size; i++ {
			p, err := h.spawnFn()
			if err != nil {
				// Spawn failed mid-way — close anything we already
				// got and surface the error. The helper is poisoned
				// for the rest of its lifetime so we don't keep
				// retrying a broken tsserver in the resolver hot path.
				for _, prov := range providers {
					go func(prov *Provider) { _ = prov.Close() }(prov)
				}
				h.spawnErr = err
				return
			}
			providers = append(providers, p)
			pool <- p
		}
		h.providers = providers
		h.pool = pool
		if size > 1 {
			h.logger.Info("resolve-time LSP: provider pool spawned",
				zap.String("workspace", h.workspaceRoot),
				zap.Int("pool_size", size))
		}
	})
	return h.spawnErr
}

// borrow takes a provider out of the pool (blocking until one is
// available) and returns a release closure the caller defers. The
// pool guarantees mutual exclusion per provider — within tsserver
// each provider's stdio is single-threaded — while allowing up to
// poolSize Definition calls to run in parallel across distinct
// providers.
func (h *ResolverHelper) borrow() (*Provider, func()) {
	p := <-h.pool
	return p, func() { h.pool <- p }
}

// acquire returns the provider one Definition call should run
// against, plus the release closure the caller must defer. ok is
// false when no provider could be obtained — the caller then falls
// through to the resolver's heuristics, and release must not be
// called.
//
// This is the single point where the two ownership contracts diverge:
//
//   - Router-backed: take h.mu, then ask the router again. The lookup
//     is what keeps the router's lastUsed fresh (so an in-flight
//     resolve pass is never reaped) and what guarantees a provider the
//     router already reaped is replaced by a new router-tracked one
//     instead of being resurrected in place behind the router's back.
//     The lock is held until release so a provider shared with every
//     other caller of ForSpecWorkspace still sees serialised stdio.
//   - Owned pool: spawn the pool once, then borrow a provider from the
//     channel for the duration of the call.
//
// relPath is carried purely for the failure log — a lookup that fails
// is worth one Debug line naming the file that wanted it, never an
// error that poisons the helper.
func (h *ResolverHelper) acquire(relPath string) (*Provider, func(), bool) {
	if h.routerBacked {
		h.mu.Lock()
		p, err := h.spawnFn()
		if err != nil || p == nil {
			h.mu.Unlock()
			h.logger.Debug("resolve-time LSP: provider lookup failed",
				zap.String("path", relPath), zap.Error(err))
			return nil, nil, false
		}
		return p, func() { h.mu.Unlock() }, true
	}

	if err := h.ensurePool(); err != nil {
		h.logger.Debug("resolve-time LSP: pool spawn failed",
			zap.String("path", relPath), zap.Error(err))
		return nil, nil, false
	}
	if h.pool == nil {
		// Eager-construction path was given a nil provider — short-
		// circuit instead of deadlocking on a never-fed channel.
		return nil, nil, false
	}
	p, release := h.borrow()
	return p, release, true
}

// SupportsPath implements resolver.LSPHelper.
//
// SupportsPath does NOT trigger the lazy provider lookup — it's
// answered purely from the extension set. This keeps the
// short-circuit cheap (no LSP spawn) for the common case where the
// resolver asks "do you handle this file?" against many candidate
// edges, only a fraction of which will actually want a Definition
// call.
func (h *ResolverHelper) SupportsPath(relPath string) bool {
	if h == nil || relPath == "" {
		return false
	}
	if h.provider == nil && h.spawnFn == nil {
		return false
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	_, ok := h.extensions[ext]
	return ok
}

// Definition implements resolver.LSPHelper. Returns
// (definitionRelPath, 1-based line, ok).
//
// Implementation notes:
//   - The provider for the call comes from acquire: a per-call router
//     lookup in router-backed mode, a pool borrow in owned-pool mode.
//   - The file is opened with didOpen on first call (EnsureFileOpen)
//     so tsserver has the buffer in its workspace state.
//   - The identifier column on `oneBasedLine` is resolved from the
//     cached source so the LSP cursor sits on the identifier.
//   - The returned path is repo-relative when possible (matching
//     graph.Node.FilePath), else falls back to absolute.
func (h *ResolverHelper) Definition(relPath string, oneBasedLine int, name string) (string, int, bool) {
	if h == nil {
		return "", 0, false
	}
	if !h.SupportsPath(relPath) {
		return "", 0, false
	}
	if oneBasedLine <= 0 || name == "" {
		return "", 0, false
	}

	provider, release, ok := h.acquire(relPath)
	if !ok {
		return "", 0, false
	}
	defer release()

	if err := provider.EnsureClient(h.workspaceRoot); err != nil {
		h.logger.Debug("resolve-time LSP: ensure client failed",
			zap.String("path", relPath), zap.Error(err))
		return "", 0, false
	}
	if err := provider.EnsureFileOpen(h.workspaceRoot, relPath); err != nil {
		h.logger.Debug("resolve-time LSP: open document failed",
			zap.String("path", relPath), zap.Error(err))
		return "", 0, false
	}

	src := provider.Source(h.workspaceRoot, relPath)
	col := IdentifierColumn(src, oneBasedLine, name)

	locs, err := provider.FindDefinition(h.workspaceRoot, relPath, oneBasedLine-1, col, h.timeout)
	if err != nil {
		h.logger.Debug("resolve-time LSP: definition error",
			zap.String("path", relPath), zap.Int("line", oneBasedLine),
			zap.String("name", name), zap.Error(err))
		return "", 0, false
	}
	if len(locs) == 0 {
		return "", 0, false
	}

	// First location is the canonical definition. Tsserver may
	// return multiple (e.g. an interface declaration plus its
	// implementations); the resolver picks the first as the
	// "source of truth" and falls through to the heuristic when
	// the kind gate rejects it.
	loc := locs[0]
	abs := uriToAbsLocalPath(loc.URI)
	if abs == "" {
		return "", 0, false
	}
	rel := abs
	if r, err := filepath.Rel(h.workspaceRoot, abs); err == nil {
		// filepath.Rel can produce "../" paths when the
		// definition sits outside the workspace (node_modules
		// resolution, for example). Reject those — the
		// resolver's graph only has nodes for files under the
		// workspace.
		if !strings.HasPrefix(r, "..") {
			rel = filepath.ToSlash(r)
		} else {
			return "", 0, false
		}
	}
	return rel, loc.Range.Start.Line + 1, true
}

// uriToAbsLocalPath converts a file:// URI to an absolute local
// path. Returns "" for non-file URIs or malformed input. Mirrors
// the behaviour of uriToAbsPath but is exported intent-named here
// for clarity in resolver wiring.
func uriToAbsLocalPath(uri string) string {
	if uri == "" {
		return ""
	}
	if strings.HasPrefix(uri, "file://") {
		return lspuri.URIToAbsPath(uri)
	}
	// Some servers (rare) reply with a bare absolute path.
	if filepath.IsAbs(uri) {
		return uri
	}
	return ""
}

// SpawnProviderForResolver creates a fresh, fully-initialised
// *Provider against the given workspace root for use as one slot in a
// ResolverHelper pool. Unlike Router.ForSpecWorkspace this does NOT
// cache — every call spawns a new tsserver process. Use it as the
// spawnFn argument to NewPooledResolverHelper. The returned provider
// is owned by the helper; helper.Close shuts it down.
func SpawnProviderForResolver(spec *ServerSpec, workspaceRoot string, logger *zap.Logger) (*Provider, error) {
	if spec == nil {
		return nil, fmt.Errorf("nil spec")
	}
	p := NewProviderFromSpec(spec, logger)
	if err := p.EnsureClient(workspaceRoot); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// ResolverPoolSizeFromEnv returns the pool size for the resolve-time
// LSP hot path, honouring GORTEX_LSP_POOL_SIZE. Falls back to the
// caller's defaultSize when the env var is unset or unparseable.
// Clamped to [1, 32] to keep tsserver memory bounded.
//
// Default is intentionally 1: at one provider per workspace the
// caller's daemon-side wiring can route through Router.ForSpecWorkspace
// (which idle-reaps unused providers via the LSP router's existing
// reaper), keeping the multi-workspace memory footprint at "one
// long-lived tsserver per workspace" — matching the pre-pool design.
// Values >1 spawn N FRESH tsservers per workspace via
// SpawnProviderForResolver, which have NO idle reaping; opt in only
// when the tracked-workspace count is small (rough rule of thumb:
// total_workspaces * pool_size * 150 MB < available RAM).
func ResolverPoolSizeFromEnv(defaultSize int) int {
	if defaultSize <= 0 {
		defaultSize = 1
	}
	raw := os.Getenv("GORTEX_LSP_POOL_SIZE")
	if raw == "" {
		return defaultSize
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return defaultSize
	}
	if n > 32 {
		n = 32
	}
	return n
}

// Close shuts down every provider the helper owns. Called by the
// indexer at shutdown.
//
// Router-backed helpers (NewLazyResolverHelper) own nothing, so Close
// is deliberately a no-op for them: their provider is the router's,
// shared with enrichment and with every other helper for the same
// (spec, workspace), and closing it here would kill a subprocess the
// router still lists as alive — the mirror image of the orphan bug
// per-call lookup exists to prevent. Router.Close is what shuts those
// down.
//
// Safe to call when the lazy spawn has not yet fired — Close is a
// no-op in that case too.
func (h *ResolverHelper) Close() error {
	if h == nil {
		return nil
	}
	if h.routerBacked {
		return nil
	}
	// Resolution/pool construction has already joined at this ownership
	// boundary. Start every owned provider before waiting for any one child.
	for _, p := range h.providers {
		if p != nil {
			p.BeginClose()
		}
	}
	var errs []error
	for _, p := range h.providers {
		if p != nil {
			if err := p.Close(); err != nil {
				errs = append(errs, fmt.Errorf("resolver provider %s: %w", p.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}
