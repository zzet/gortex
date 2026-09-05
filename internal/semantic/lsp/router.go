package lsp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/semantic"
)

// fileExists reports whether path names an existing regular file.
// spawnRubyLSPWithoutGemfile reports whether ruby-lsp should still be
// launched for a workspace that has no Gemfile (and would therefore trigger
// a network `bundle install` for its composed bundle on spawn). Default
// false — skip it and rely on the in-process ruby resolver. Set
// GORTEX_RUBY_LSP_NO_GEMFILE=1 to restore the spawn.
func spawnRubyLSPWithoutGemfile() bool {
	v := os.Getenv("GORTEX_RUBY_LSP_NO_GEMFILE")
	return v == "1" || strings.EqualFold(v, "true")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// absWorkspace absolutises a workspace root, preserving the empty string.
// filepath.Abs("") returns the process's own working directory — for a
// daemon tracking many repos that is a directory belonging to none of
// them, and it must never silently become a language server's cwd.
func absWorkspace(workspace string) string {
	if workspace == "" {
		return ""
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		return abs
	}
	return workspace
}

// validateWorkspaceRoot rejects a workspace that cannot serve as an LSP
// subprocess's working directory. The router hands this value to
// exec.Cmd.Dir, so an unusable one surfaces as an opaque
// `chdir <path>: no such file or directory` from the OS long after the
// mistake was made. Checking here names the real problem — and lets the
// caller fail without tripping markSpawnFailed, which would disable the
// spec for every other repo the daemon tracks.
//
// requested is what the caller asked for (before absolutisation);
// resolved is what would actually be handed to exec. Both matter: a
// requested root that is merely relative is already wrong even when it
// happens to resolve, because it resolved against the daemon's launch
// directory rather than against the tracked repo.
func validateWorkspaceRoot(specName, requested, resolved string) error {
	if requested == "" {
		return fmt.Errorf("LSP server %q: no workspace root supplied; the workspace must be the tracked repo's own path", specName)
	}
	if !filepath.IsAbs(requested) {
		return fmt.Errorf("LSP server %q: workspace %q is not absolute; a relative root resolves against the daemon's launch directory, not the tracked repo", specName, requested)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("LSP server %q: workspace %s is not usable as a working directory: %w", specName, resolved, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("LSP server %q: workspace %s is not a directory", specName, resolved)
	}
	return nil
}

// Router is a daemon-managed pool of LSP providers keyed by ServerSpec.
// It routes requests to the right provider by file extension, spawns
// providers lazily on first touch, and reaps idle ones to bound the
// number of subprocesses kept alive.
//
// Usage shape:
//
//	r := NewRouter(workspaceRoot, logger).WithIdleTimeout(10*time.Minute)
//	p, err := r.For("path/to/file.rs") // provider for rust-analyzer
//	if err != nil { ... }
//	d, _ := p.LastDiagnostics(absPath)
//
// Lifecycle:
//   - First For() call per spec: ServerSpec.Command must be on PATH
//     or one of AlternativeCommands must resolve. Failure returns the
//     unresolvable spec name in the error.
//   - Subsequent For() calls reuse the cached provider.
//   - Close() shuts every provider down deterministically.
//   - Reap() (best-effort, called from a tick goroutine when
//     WithReaperInterval is set) closes providers idle longer than
//     IdleTimeout.
type Router struct {
	// defaultWorkspace is the workspace root used by For / ForSpec
	// when the caller doesn't supply one explicitly. Multi-repo
	// daemons override per request via ForWorkspace / ForSpecWorkspace.
	defaultWorkspace string
	logger           *zap.Logger

	// additionalWorkspaceFolders are extra directory roots advertised
	// to every LSP server's initialize request (alongside the primary
	// root) so a server can resolve cross-package imports.
	additionalWorkspaceFolders []string

	// enrichExcludeGlobs are user-configured path globs to skip for
	// enrichment, propagated to every spawned provider.
	enrichExcludeGlobs []string

	// enrichSweepMode is the configured per-file enrichment sweep mode
	// ("demand" default / "full" / "off"), propagated to every spawned
	// provider. Empty means the demand-gated default; the GORTEX_LSP_SWEEP
	// env override wins over it at enrichment time.
	enrichSweepMode string

	// enrichOpenDocs is the configured didOpen-lifecycle override ("on" /
	// "off"), propagated to every spawned provider. Empty lets each spec's
	// NoDidOpen decide; the GORTEX_LSP_OPEN_DOCS env override wins over it
	// at enrichment time.
	enrichOpenDocs string
	// enrichMaxParallel is the operator-configured concurrent-request cap
	// (`semantic.lsp_max_parallel`), propagated to every spawned provider.
	// Zero keeps each spec's own default; the GORTEX_LSP_MAX_PARALLEL env
	// override wins over both.
	enrichMaxParallel int

	mu        sync.Mutex
	providers map[providerKey]*routedProvider // (spec.Name, workspace) → cached provider
	enabled   map[string]*ServerSpec          // spec.Name → spec marked enabled by config (no spawn until For/ForSpec)

	// limits — zero means "no limit / no reaping". maxAlive is guarded by
	// r.mu; SetMaxAlive mutates it at runtime (batch enrichment raises the
	// cap so concurrent passes don't evict each other's warmed servers).
	idleTimeout    time.Duration
	reaperInterval time.Duration
	maxAlive       int

	// evictions counts LRU evictions (maybeEvictLRULocked) over the life
	// of the router. Read via EvictionCount to observe provider churn
	// during batch enrichment. Atomic so the accessor needn't take r.mu.
	evictions atomic.Uint64
	timing    routerTimingCounters

	stopReaper chan struct{}

	// availability cache — checking exec.LookPath has measurable
	// overhead on Windows / WSL filesystems, and the answer is
	// stable for the life of the process.
	availMu sync.RWMutex
	avail   map[string]bool // spec.Name → resolved on PATH

	// diagHookMu / diagHook installs a single persistent
	// publishDiagnostics subscriber across every spawned provider —
	// current and future. The MCP server registers itself here at
	// boot to forward LSP diagnostics as `notifications/diagnostics`.
	diagHookMu sync.RWMutex
	diagHook   func(specName, absPath string, diags []Diagnostic)
}

type routedProvider struct {
	spec      *ServerSpec
	workspace string
	provider  *Provider
	lastUsed  time.Time
	// inUse counts callers currently holding this provider for a long
	// operation (an enrichment pass). The LRU evictor skips any provider
	// with inUse > 0 so a slow in-flight pass is never Close()d mid-use by
	// another repo's concurrent spawn. Guarded by r.mu.
	inUse int
}

// providerKey identifies a (spec, workspace) pair in the cache. Each
// (spec, workspace) combination gets its own LSP subprocess so a
// multi-repo daemon doesn't conflate which workspace a server was
// initialised against (Provider.EnsureClient is idempotent — only the
// first call's workspace root sticks).
type providerKey struct {
	specName  string
	workspace string
}

// NewRouter constructs an empty Router. defaultWorkspace is the
// directory passed to LSP servers as `rootUri` when the caller uses
// For / ForSpec without specifying a workspace. Multi-repo daemons
// override on a per-request basis via ForWorkspace / ForSpecWorkspace.
//
// Pass "" when there is no single meaningful default (the multi-repo
// daemon case): an empty default stays empty, so a caller that omits the
// per-repo workspace gets a clear error instead of a server spawned in
// whatever directory the daemon happened to be launched from.
func NewRouter(defaultWorkspace string, logger *zap.Logger) *Router {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Router{
		defaultWorkspace: absWorkspace(defaultWorkspace),
		logger:           logger,
		providers:        make(map[providerKey]*routedProvider),
		enabled:          make(map[string]*ServerSpec),
		avail:            make(map[string]bool),
	}
}

// RegisterSpec marks spec as enabled — the Router will return it from
// EnabledSpecs and accept it as a target for ForSpec, but no LSP
// subprocess is spawned until the first For/ForSpec call. Call this at
// boot for every server the user has opted into via config.
//
// Idempotent — re-registering the same spec is a no-op.
func (r *Router) RegisterSpec(spec *ServerSpec) {
	if spec == nil {
		return
	}
	r.mu.Lock()
	r.enabled[spec.Name] = spec
	r.mu.Unlock()
}

// RegisterAvailable iterates every spec in the global registry and
// registers each one whose command (or any AlternativeCommands entry)
// resolves on the daemon's PATH. The `disabled` set is checked first
// — listed names are skipped unconditionally so users keep precise
// per-spec opt-out without needing to know the registry contents.
//
// Returns the list of names that were actually registered, in
// registration order. Idempotent against RegisterSpec — re-running
// over already-registered specs is a no-op.
//
// Why this is safe-by-default: RegisterSpec only marks the spec as
// eligible — no subprocess is spawned until something calls
// ForSpec / ForSpecWorkspace. Routers that no caller queries cost
// the daemon nothing beyond a cached PATH lookup.
func (r *Router) RegisterAvailable(disabled map[string]bool) []string {
	specs := AllSpecs()
	var registered []string
	for _, spec := range specs {
		if spec == nil {
			continue
		}
		if disabled[spec.Name] {
			continue
		}
		// An explicit .gortex.yaml entry may have already registered a
		// SpecWithOverrides copy of this spec (a different pointer)
		// carrying custom command / args / env. The auto-pass must
		// defer to that override rather than clobber it with the
		// pristine built-in spec. A spec already registered as the
		// built-in itself is harmless to re-register.
		r.mu.Lock()
		existing, already := r.enabled[spec.Name]
		r.mu.Unlock()
		if already && existing != spec {
			continue
		}
		if !r.specAvailable(spec) {
			continue
		}
		r.RegisterSpec(spec)
		registered = append(registered, spec.Name)
	}
	sort.Strings(registered)
	return registered
}

// EnabledSpecs returns every spec previously registered via
// RegisterSpec, sorted by name. The slice may include specs whose
// command is not on PATH — call Available(spec) to filter.
func (r *Router) EnabledSpecs() []*ServerSpec {
	r.mu.Lock()
	out := make([]*ServerSpec, 0, len(r.enabled))
	for _, s := range r.enabled {
		out = append(out, s)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// EnabledSpecNames returns just the names of enabled specs. Used by the
// semantic.Manager interface bridge so the package boundary stays clean
// (semantic.Manager can't import lsp without a cycle).
func (r *Router) EnabledSpecNames() []string {
	specs := r.EnabledSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// ProviderForSpec returns the lazy-spawned LSP provider as a
// semantic.Provider interface. Used by semantic.Manager.EnrichAll to
// drive batch enrichment without taking a hard dependency on the lsp
// package. Returns an error if the spec is not enabled or not on PATH.
func (r *Router) ProviderForSpec(name string) (semantic.Provider, error) {
	r.mu.Lock()
	spec, ok := r.enabled[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("LSP spec %q not registered", name)
	}
	return r.ForSpec(spec)
}

// ProviderForSpecWorkspace returns the lazy-spawned LSP provider for the
// named spec scoped to a specific workspace root, so each repo gets its own
// provider instance keyed by (spec, workspace) instead of sharing the
// default-workspace one. Used by per-repo enrichment so concurrent passes
// across repos do not share a single Provider's connection / document caches.
func (r *Router) ProviderForSpecWorkspace(name, workspace string) (semantic.Provider, error) {
	r.mu.Lock()
	spec, ok := r.enabled[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("LSP spec %q not registered", name)
	}
	// forSpecWorkspace with pin=true increments inUse inside the same locked
	// section it publishes/looks up the provider, closing the spawn→pin race.
	// The caller MUST pair this with ReleaseSpecWorkspace.
	return r.forSpecWorkspace(spec, workspace, true)
}

// SpecAvailable reports whether the named spec is registered AND its
// command resolves on PATH. Pure read — no subprocess spawn. Caches
// the PATH-lookup result like specAvailable does for ForSpec.
func (r *Router) SpecAvailable(name string) bool {
	r.mu.Lock()
	spec, ok := r.enabled[name]
	r.mu.Unlock()
	if !ok {
		return false
	}
	return r.specAvailable(spec)
}

// SpecLanguages returns the language codes the named spec serves.
// Pure metadata read — never spawns a subprocess. Returns nil for
// unregistered specs.
func (r *Router) SpecLanguages(name string) []string {
	r.mu.Lock()
	spec, ok := r.enabled[name]
	r.mu.Unlock()
	if !ok || spec == nil {
		return nil
	}
	out := make([]string, len(spec.Languages))
	copy(out, spec.Languages)
	return out
}

// SpecPriority returns the spec's default priority (lower number wins
// when multiple specs serve the same language). Returns 99 (a
// fallback "any provider beats unknown") for unregistered specs.
func (r *Router) SpecPriority(name string) int {
	r.mu.Lock()
	spec, ok := r.enabled[name]
	r.mu.Unlock()
	if !ok || spec == nil {
		return 99
	}
	return spec.Priority
}

// WithIdleTimeout sets how long a provider can be idle before Reap()
// will shut it down.
func (r *Router) WithIdleTimeout(d time.Duration) *Router {
	r.idleTimeout = d
	return r
}

// IdleTTLEnv overrides the router's provider idle timeout — a Go duration
// ("45m", "2h"); zero or negative disables reaping entirely. Exists because
// one timeout cannot fit every server class: a Roslyn or jdtls workspace can
// take longer to load than the default idle window, and a server reaped
// mid-load never becomes useful — every later pass pays the load again.
const IdleTTLEnv = "GORTEX_LSP_IDLE_TTL"

// IdleTimeoutFromEnv resolves the router idle timeout: the IdleTTLEnv
// duration when set and parseable (negative clamps to 0 = never reap),
// else fallback.
func IdleTimeoutFromEnv(fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(IdleTTLEnv))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	if d < 0 {
		return 0
	}
	return d
}

// WithAdditionalWorkspaceFolders sets extra directory roots advertised
// to every LSP server's initialize request alongside the primary
// workspace root, enabling cross-package resolution. Builder-style.
func (r *Router) WithAdditionalWorkspaceFolders(folders []string) *Router {
	r.additionalWorkspaceFolders = folders
	return r
}

// WithEnrichExcludeGlobs sets user-configured path globs that every spawned
// provider skips for enrichment (on top of the built-in generated/vendored
// heuristic). Builder-style.
func (r *Router) WithEnrichExcludeGlobs(globs []string) *Router {
	r.enrichExcludeGlobs = globs
	return r
}

// WithEnrichSweepMode sets the per-file enrichment sweep mode ("demand"
// default / "full" / "off") propagated to every spawned provider. An empty
// value keeps the demand-gated default; the GORTEX_LSP_SWEEP env override
// still wins over whatever is set here. Builder-style.
func (r *Router) WithEnrichSweepMode(mode string) *Router {
	r.enrichSweepMode = mode
	return r
}

// WithEnrichOpenDocs sets the configured didOpen-lifecycle override ("on" /
// "off") propagated to every spawned provider. An empty value lets each
// spec's NoDidOpen decide; the GORTEX_LSP_OPEN_DOCS env override still wins
// over whatever is set here. Builder-style.
func (r *Router) WithEnrichOpenDocs(v string) *Router {
	r.enrichOpenDocs = v
	return r
}

// WithEnrichMaxParallel sets the operator-configured concurrent-request
// cap propagated to every spawned provider. Zero keeps each spec's own
// default; the GORTEX_LSP_MAX_PARALLEL env override still wins over
// whatever is set here. Builder-style.
func (r *Router) WithEnrichMaxParallel(n int) *Router {
	r.enrichMaxParallel = n
	return r
}

// WithReaperInterval starts a background reaper that calls Reap() at
// the given cadence. Idempotent — calling twice replaces the previous
// reaper. A zero duration disables reaping.
func (r *Router) WithReaperInterval(d time.Duration) *Router {
	r.mu.Lock()
	if r.stopReaper != nil {
		close(r.stopReaper)
		r.stopReaper = nil
	}
	if d > 0 {
		stop := make(chan struct{})
		r.stopReaper = stop
		go r.reaperLoop(d, stop)
	}
	r.reaperInterval = d
	r.mu.Unlock()
	return r
}

// WithMaxAlive caps the number of concurrent live providers. When
// exceeded, the least-recently-used provider is evicted. Construction-
// only and unlocked — use SetMaxAlive to change the cap at runtime.
func (r *Router) WithMaxAlive(n int) *Router {
	r.maxAlive = n
	return r
}

// SetMaxAlive changes the live-provider cap at runtime under r.mu.
// Raising the cap lets a batch of concurrent enrichment passes keep more
// warmed language servers alive at once instead of churning through the
// LRU evictor; lowering it evicts any now-excess (unpinned) providers so
// the invariant len(providers) <= maxAlive is restored. A provider held
// in-use by an in-flight pass is never evicted, matching the LRU rule.
func (r *Router) SetMaxAlive(n int) {
	r.mu.Lock()
	r.maxAlive = n
	r.maybeEvictLRULocked()
	r.mu.Unlock()
}

// MaxAlive returns the current live-provider cap. Zero means unbounded.
func (r *Router) MaxAlive() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxAlive
}

// EvictionCount returns the number of LRU evictions the router has
// performed over its lifetime. Used to observe provider churn across a
// batch enrichment window (sample before / after).
func (r *Router) EvictionCount() uint64 {
	return r.evictions.Load()
}

// workspaceKey resolves a workspace string to the cache key form
// ForSpecWorkspace uses (default-substituted, absolutised).
func (r *Router) workspaceKey(specName, workspace string) providerKey {
	if workspace == "" {
		workspace = r.defaultWorkspace
	}
	return providerKey{specName: specName, workspace: absWorkspace(workspace)}
}

// ReleaseSpecWorkspace marks a provider previously obtained via
// ProviderForSpecWorkspace as no longer in active use, so the LRU evictor /
// reaper may reclaim it again. Pairs one-to-one with ProviderForSpecWorkspace.
func (r *Router) ReleaseSpecWorkspace(name, workspace string) {
	key := r.workspaceKey(name, workspace)
	r.mu.Lock()
	if rp := r.providers[key]; rp != nil && rp.inUse > 0 {
		rp.inUse--
	}
	r.mu.Unlock()
}

// For returns the provider responsible for the given file path under
// the router's defaultWorkspace. Convenience wrapper for single-
// workspace callers; multi-repo daemons should use ForWorkspace and
// pass the per-file workspace root.
func (r *Router) For(relPath string) (*Provider, error) {
	return r.ForWorkspace(relPath, r.defaultWorkspace)
}

// ForWorkspace returns the provider responsible for the given file
// path under the given workspace root. Cache key is (spec, workspace)
// so the same LSP spec gets a separate subprocess per workspace —
// preventing the multi-repo bug where Provider.EnsureClient (which is
// idempotent) would otherwise leave every workspace pinned to the
// rootURI of whichever request happened to spawn the server first.
//
// When several specs cover the extension the router walks them in
// priority order and takes the first one its own availability cache
// reports live — so a preferred server that is missing from PATH or
// was downgraded by markSpawnFailed falls back to the next candidate
// (tsgo → typescript-language-server) instead of erroring.
func (r *Router) ForWorkspace(relPath, workspace string) (*Provider, error) {
	specs := SpecsForExtension(filepath.Ext(relPath))
	if len(specs) == 0 {
		return nil, fmt.Errorf("no LSP server registered for %s", filepath.Ext(relPath))
	}
	for _, spec := range specs {
		if r.specAvailable(spec) {
			return r.ForSpecWorkspace(spec, workspace)
		}
	}
	// None available: route to the priority winner so the caller gets
	// the same "not available on PATH" error shape as before.
	return r.ForSpecWorkspace(specs[0], workspace)
}

// ForSpec returns the provider for a named spec under the router's
// defaultWorkspace. Convenience wrapper.
func (r *Router) ForSpec(spec *ServerSpec) (*Provider, error) {
	return r.ForSpecWorkspace(spec, r.defaultWorkspace)
}

// ForSpecWorkspace returns the provider for a named spec under the
// given workspace root, spawning it on first call. The (spec,
// workspace) tuple uniquely identifies the cached Provider.
func (r *Router) ForSpecWorkspace(spec *ServerSpec, workspace string) (*Provider, error) {
	return r.forSpecWorkspace(spec, workspace, false)
}

// forSpecWorkspace is ForSpecWorkspace with an optional in-use pin. When pin
// is true the returned provider's inUse count is incremented in the SAME
// locked section that looks it up or publishes it — so a concurrent spawn's
// LRU eviction can never Close a freshly-returned-but-not-yet-pinned provider
// (the spawn→pin TOCTOU). A pinned fetch MUST be paired with ReleaseSpecWorkspace.
func (r *Router) forSpecWorkspace(spec *ServerSpec, workspace string, pin bool) (*Provider, error) {
	acquireStarted := time.Now()
	success, reused := false, false
	r.timing.acquires.Add(1)
	defer func() {
		elapsed := time.Since(acquireStarted)
		r.timing.acquireNanos.Add(int64(elapsed))
		if !success {
			r.timing.acquireFailures.Add(1)
		}
		if reused {
			r.timing.reuses.Add(1)
			r.timing.reuseNanos.Add(int64(elapsed))
		}
	}()
	if !r.specAvailable(spec) {
		return nil, fmt.Errorf("LSP server %q not available on PATH", spec.Name)
	}
	if workspace == "" {
		workspace = r.defaultWorkspace
	}
	// Keep what was asked for: validateWorkspaceRoot needs it to tell a
	// genuine absolute repo root from a relative fragment that merely
	// resolved against the daemon's launch directory.
	requested := workspace
	workspace = absWorkspace(workspace)
	key := providerKey{specName: spec.Name, workspace: workspace}

	lockStarted := time.Now()
	r.mu.Lock()
	r.timing.waitNanos.Add(int64(time.Since(lockStarted)))
	rp, ok := r.providers[key]
	if ok {
		rp.lastUsed = time.Now()
		if pin {
			rp.inUse++
		}
		r.mu.Unlock()
		success, reused = true, true
		return rp.provider, nil
	}
	r.mu.Unlock()

	// The workspace becomes the subprocess's working directory, so validate
	// it before spawning. A caller that handed us a path anchored on the
	// daemon's launch cwd instead of on the tracked repo's own root would
	// otherwise fail deep inside exec with a bare chdir error — and would
	// take the spec down for every repo via markSpawnFailed. Failing here
	// keeps the fault local to the one bad workspace.
	if err := validateWorkspaceRoot(spec.Name, requested, workspace); err != nil {
		return nil, err
	}

	// Spawn outside the lock — initialize() blocks on stdio I/O.
	constructStarted := time.Now()
	p := NewProviderFromSpec(spec, r.logger)
	p.workspaceFolders = r.additionalWorkspaceFolders
	p.excludeGlobs = r.enrichExcludeGlobs
	p.sweepMode = r.enrichSweepMode
	p.opensDocs = resolveOpensDocs(r.enrichOpenDocs, spec)
	p.maxParallel = resolveMaxParallel(r.enrichMaxParallel, spec.MaxParallel)
	// ruby-lsp (and any spec opting in) runs a `bundle install` for a composed
	// bundle on spawn unless BUNDLE_GEMFILE is set; point it at the workspace's
	// own Gemfile when present so enrichment skips that install.
	if spec.UseWorkspaceBundleGemfile {
		if gemfile := filepath.Join(workspace, "Gemfile"); fileExists(gemfile) {
			p.env = append(append([]string(nil), p.env...), "BUNDLE_GEMFILE="+gemfile)
		} else if !spawnRubyLSPWithoutGemfile() {
			// No Gemfile: ruby-lsp runs a `bundle install` for its composed
			// bundle on spawn — a multi-second network round-trip to
			// rubygems.org — for a workspace where Ruby is typically only test
			// fixtures. Skip the LSP; the in-process ruby-types resolver still
			// covers Ruby. This is a per-workspace skip (NOT markSpawnFailed),
			// so a sibling repo that does carry a Gemfile still spawns ruby-lsp.
			// GORTEX_RUBY_LSP_NO_GEMFILE=1 forces the spawn.
			return nil, fmt.Errorf("ruby-lsp: no Gemfile in %s; skipping composed-bundle install", workspace)
		}
	}
	if err := r.ensureTimedProvider(p, spec.Name, workspace, time.Since(constructStarted)); err != nil {
		// A binary that resolves on PATH but cannot launch (e.g. a rustup
		// `rust-analyzer` shim whose toolchain lacks the component) would
		// otherwise be re-attempted on every repo. Mark it unavailable so the
		// router stops retrying it for this session.
		r.markSpawnFailed(spec.Name, err)
		return nil, fmt.Errorf("spawn %s: %w", spec.Name, err)
	}
	// Attach the diagnostics hook (if any) before publishing to the
	// providers map so we don't drop the first publishDiagnostics
	// burst some servers emit during workspace warmup.
	r.attachDiagnosticsHook(spec.Name, p)

	lockStarted = time.Now()
	r.mu.Lock()
	r.timing.waitNanos.Add(int64(time.Since(lockStarted)))
	defer r.mu.Unlock()
	// Race: another goroutine may have spawned it while we were
	// initializing. Prefer the existing one and shut down our duplicate.
	if existing, ok := r.providers[key]; ok {
		existing.lastUsed = time.Now()
		if pin {
			existing.inUse++
		}
		go func() { _ = r.closeTimedProvider(p, spec.Name, workspace, "duplicate") }()
		r.timing.raceReuses.Add(1)
		success = true
		return existing.provider, nil
	}
	newRP := &routedProvider{
		spec:      spec,
		workspace: workspace,
		provider:  p,
		lastUsed:  time.Now(),
	}
	// Pin BEFORE maybeEvictLRULocked runs so the just-published provider is
	// never the eviction victim, and is protected the instant it is reachable.
	if pin {
		newRP.inUse = 1
	}
	r.providers[key] = newRP
	r.maybeEvictLRULocked()
	success = true
	return p, nil
}

// SetDiagnosticsHook installs a persistent subscriber called for every
// `textDocument/publishDiagnostics` any router-managed provider emits.
// Pass nil to detach.
//
// Calling SetDiagnosticsHook on a router that already owns providers
// re-attaches the new hook to every existing provider in addition to
// installing it for future spawns. Passing nil clears the per-provider
// hook on every existing provider.
//
// The hook MUST NOT block — it runs on the LSP client message-pump
// goroutine.
func (r *Router) SetDiagnosticsHook(hook func(specName, absPath string, diags []Diagnostic)) {
	r.diagHookMu.Lock()
	r.diagHook = hook
	r.diagHookMu.Unlock()

	// Re-attach to every live provider so the change takes effect
	// without requiring a restart.
	r.mu.Lock()
	live := make([]*routedProvider, 0, len(r.providers))
	for _, rp := range r.providers {
		live = append(live, rp)
	}
	r.mu.Unlock()
	for _, rp := range live {
		r.attachDiagnosticsHook(rp.spec.Name, rp.provider)
	}
}

// attachDiagnosticsHook installs the router-level hook on a single
// provider, capturing the spec name in the closure so subscribers can
// distinguish the source LSP. No-ops when the router has no hook set.
func (r *Router) attachDiagnosticsHook(specName string, p *Provider) {
	r.diagHookMu.RLock()
	hook := r.diagHook
	r.diagHookMu.RUnlock()
	if hook == nil {
		p.SetDiagnosticsHook(nil)
		return
	}
	p.SetDiagnosticsHook(func(absPath string, diags []Diagnostic) {
		hook(specName, absPath, diags)
	})
}

// DiagnosticsEntry is one (spec, file, diagnostics) row in a snapshot.
type DiagnosticsEntry struct {
	SpecName    string
	AbsPath     string
	Diagnostics []Diagnostic
}

// DiagnosticsSnapshot returns the most recent publishDiagnostics
// payload across every alive provider, flattened into a single slice.
// Used to replay current state to a freshly-subscribed MCP client.
func (r *Router) DiagnosticsSnapshot() []DiagnosticsEntry {
	r.mu.Lock()
	live := make([]*routedProvider, 0, len(r.providers))
	for _, rp := range r.providers {
		live = append(live, rp)
	}
	r.mu.Unlock()

	var out []DiagnosticsEntry
	for _, rp := range live {
		snap := rp.provider.DiagnosticsSnapshot()
		for path, diags := range snap {
			out = append(out, DiagnosticsEntry{
				SpecName:    rp.spec.Name,
				AbsPath:     path,
				Diagnostics: diags,
			})
		}
	}
	return out
}

// Available reports whether at least one of the spec's commands is on
// PATH. Negative results are cached, but a future PATH change between
// calls is the caller's problem.
func (r *Router) Available(spec *ServerSpec) bool {
	return r.specAvailable(spec)
}

// AvailableSpecs lists every spec resolvable on the current PATH. Use
// at startup to log which servers will spin up later.
func (r *Router) AvailableSpecs() []*ServerSpec {
	out := make([]*ServerSpec, 0)
	for _, s := range AllSpecs() {
		if r.specAvailable(s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// specAvailable returns true when one of spec.Command +
// spec.AlternativeCommands resolves on PATH, or — for a passive spec
// — when the connect block validates (Validate() == nil). A passive
// spec has no binary to look up; its "availability" is whether the
// configured endpoint is well-formed (the actual dial happens on
// first ensureClient).
func (r *Router) specAvailable(spec *ServerSpec) bool {
	if spec == nil {
		return false
	}
	r.availMu.RLock()
	v, cached := r.avail[spec.Name]
	r.availMu.RUnlock()
	if cached {
		return v
	}
	avail := false
	switch {
	case spec.Connect != nil:
		// Passive attach — no binary to find. Treat as available
		// when the connect block validates; the dial is exercised
		// lazily on first ensureClient.
		avail = spec.Connect.Validate() == nil
	default:
		if _, err := exec.LookPath(spec.Command); err == nil {
			avail = true
		} else {
			for _, alt := range spec.AlternativeCommands {
				if _, err := exec.LookPath(alt.Command); err == nil {
					avail = true
					break
				}
			}
		}
	}
	r.availMu.Lock()
	r.avail[spec.Name] = avail
	r.availMu.Unlock()
	return avail
}

// markSpawnFailed records that a spec's server process failed to start, so
// the availability cache reports it unavailable and the router stops
// retrying it for the life of the daemon (until restart). Enrichment is
// best-effort: a binary that resolves on PATH but cannot launch should be
// dropped once with a clear warning rather than re-attempted on every repo.
// The warning is emitted only on the transition to unavailable so a single
// failure is reported once, not once per repo.
func (r *Router) markSpawnFailed(specName string, err error) {
	r.availMu.Lock()
	prev, known := r.avail[specName]
	r.avail[specName] = false
	r.availMu.Unlock()
	if known && !prev {
		return // already marked unavailable — don't re-log
	}
	r.logger.Warn("LSP server failed to start; skipping its language enrichment this session",
		zap.String("spec", specName),
		zap.Error(err),
	)
}

// LanguageIDForPath proxies to the package-level helper for callers
// that hold a router but not a Provider.
func (r *Router) LanguageIDForPath(path string) string { return LanguageIDForPath(path) }

// Reap closes any provider idle for longer than IdleTimeout. Returns
// "spec@workspace" identifiers for reaped entries.
func (r *Router) Reap() []string {
	if r.idleTimeout <= 0 {
		return nil
	}
	cut := time.Now().Add(-r.idleTimeout)
	r.mu.Lock()
	var victims []*routedProvider
	for key, rp := range r.providers {
		if rp.inUse > 0 {
			continue // a provider held by an in-flight pass is not idle
		}
		if rp.lastUsed.Before(cut) {
			victims = append(victims, rp)
			delete(r.providers, key)
		}
	}
	r.mu.Unlock()
	names := make([]string, 0, len(victims))
	for _, v := range victims {
		names = append(names, formatProviderKey(v.spec.Name, v.workspace))
		_ = r.closeTimedProvider(v.provider, v.spec.Name, v.workspace, "idle")
	}
	if len(names) > 0 {
		r.logger.Info("LSP router reaped idle providers", zap.Strings("names", names))
	}
	return names
}

// The router is what the per-checkout workspace registry stops an evicted
// workspace through. SetLSPRouter type-asserts for it, so the assertion here
// is what keeps that wiring from going quiet if the method's shape drifts.
var _ semantic.WorkspaceStopper = (*Router)(nil)

// CloseCheckoutWorkspace stops every provider serving language at workspace
// and reports how many it stopped. It is what the semantic layer's
// per-checkout workspace registry calls when its own cap evicts a (language,
// root) pair, so an evicted workspace really gives its subprocess back rather
// than merely losing its slot in a ledger.
//
// A provider pinned in-use is left running: the pass holding it is reading the
// very server the eviction would stop, and the registry never evicts a pair a
// pass still holds either.
func (r *Router) CloseCheckoutWorkspace(language, workspace string) int {
	if language == "" || workspace == "" {
		return 0
	}
	root := absWorkspace(workspace)
	r.mu.Lock()
	var victims []*routedProvider
	for key, rp := range r.providers {
		if key.workspace != root || rp.inUse > 0 {
			continue
		}
		if !specServesLanguage(rp.spec, language) {
			continue
		}
		victims = append(victims, rp)
		delete(r.providers, key)
	}
	r.mu.Unlock()
	for _, victim := range victims {
		_ = r.closeTimedProvider(victim.provider, victim.spec.Name, victim.workspace, "checkout")
	}
	if len(victims) > 0 {
		names := make([]string, 0, len(victims))
		for _, victim := range victims {
			names = append(names, formatProviderKey(victim.spec.Name, victim.workspace))
		}
		r.logger.Info("LSP router stopped a checkout workspace's providers",
			zap.String("language", language),
			zap.Strings("names", names))
	}
	return len(victims)
}

// specServesLanguage reports whether spec covers one internal language code.
func specServesLanguage(spec *ServerSpec, language string) bool {
	if spec == nil {
		return false
	}
	for _, candidate := range spec.Languages {
		if candidate == language {
			return true
		}
	}
	return false
}

// maybeEvictLRULocked evicts least-recently-used providers until the
// live count is within maxAlive (or no unpinned provider remains to
// evict). Caller must hold r.mu. It loops so a SetMaxAlive that lowers
// the cap by more than one settles in a single call; the fast path
// (already within cap) returns before scanning.
func (r *Router) maybeEvictLRULocked() {
	for r.maxAlive > 0 && len(r.providers) > r.maxAlive {
		var oldest *routedProvider
		var oldestKey providerKey
		for key, rp := range r.providers {
			if rp.inUse > 0 {
				continue // never evict a provider held by an in-flight pass
			}
			if oldest == nil || rp.lastUsed.Before(oldest.lastUsed) {
				oldest = rp
				oldestKey = key
			}
		}
		if oldest == nil {
			return // every over-cap provider is pinned in-use; nothing to evict
		}
		delete(r.providers, oldestKey)
		r.evictions.Add(1)
		go func() { _ = r.closeTimedProvider(oldest.provider, oldestKey.specName, oldestKey.workspace, "lru") }()
		r.logger.Info("LSP router evicted LRU provider",
			zap.String("name", formatProviderKey(oldestKey.specName, oldestKey.workspace)))
	}
}

// formatProviderKey renders a (spec, workspace) pair into a stable
// human-readable identifier used in logs, Stats, and Names.
func formatProviderKey(specName, workspace string) string {
	if workspace == "" {
		return specName
	}
	return specName + "@" + workspace
}

func (r *Router) reaperLoop(d time.Duration, stop chan struct{}) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.Reap()
		case <-stop:
			return
		}
	}
}

// Close shuts down every active provider. Safe to call multiple times.
func (r *Router) Close() error {
	r.mu.Lock()
	if r.stopReaper != nil {
		close(r.stopReaper)
		r.stopReaper = nil
	}
	provs := r.providers
	r.providers = make(map[providerKey]*routedProvider)
	r.mu.Unlock()

	var firstErr error
	for _, rp := range provs {
		if err := r.closeTimedProvider(rp.provider, rp.spec.Name, rp.workspace, "shutdown"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RouterTimingStats is a process-lifetime diagnostic snapshot. Durations sum
// individual operations and can overlap across goroutines; they are not an
// additive breakdown of wall-clock startup time.
type RouterTimingStats struct {
	Acquires         uint64        `json:"acquires"`
	AcquireFailures  uint64        `json:"acquire_failures"`
	Reuses           uint64        `json:"reuses"`
	Starts           uint64        `json:"starts"`
	StartFailures    uint64        `json:"start_failures"`
	Evictions        uint64        `json:"evictions"`
	EvictionDuration time.Duration `json:"eviction_duration"`
	RaceReuses       uint64        `json:"race_reuses"`
	Closes           uint64        `json:"closes"`
	CloseFailures    uint64        `json:"close_failures"`
	AcquireDuration  time.Duration `json:"acquire_duration"`
	ReuseDuration    time.Duration `json:"reuse_duration"`
	StartDuration    time.Duration `json:"start_duration"`
	CloseDuration    time.Duration `json:"close_duration"`
	WaitDuration     time.Duration `json:"wait_duration"`
}

// MarshalLogObject uses the logger's configured duration encoding, matching
// ordinary zap.Duration fields rather than reflecting raw nanoseconds.
func (s RouterTimingStats) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddUint64("acquires", s.Acquires)
	enc.AddUint64("acquire_failures", s.AcquireFailures)
	enc.AddUint64("reuses", s.Reuses)
	enc.AddUint64("starts", s.Starts)
	enc.AddUint64("start_failures", s.StartFailures)
	enc.AddUint64("race_reuses", s.RaceReuses)
	enc.AddUint64("evictions", s.Evictions)
	enc.AddUint64("closes", s.Closes)
	enc.AddUint64("close_failures", s.CloseFailures)
	enc.AddDuration("acquire_duration", s.AcquireDuration)
	enc.AddDuration("reuse_duration", s.ReuseDuration)
	enc.AddDuration("start_duration", s.StartDuration)
	enc.AddDuration("close_duration", s.CloseDuration)
	enc.AddDuration("eviction_duration", s.EvictionDuration)
	enc.AddDuration("wait_duration", s.WaitDuration)
	return nil
}

// Sub reports the work completed since an earlier snapshot of the same router.
func (s RouterTimingStats) Sub(before RouterTimingStats) RouterTimingStats {
	return RouterTimingStats{
		Acquires: s.Acquires - before.Acquires, AcquireFailures: s.AcquireFailures - before.AcquireFailures,
		Reuses: s.Reuses - before.Reuses, Starts: s.Starts - before.Starts, StartFailures: s.StartFailures - before.StartFailures,
		Evictions: s.Evictions - before.Evictions, EvictionDuration: s.EvictionDuration - before.EvictionDuration,
		RaceReuses: s.RaceReuses - before.RaceReuses, Closes: s.Closes - before.Closes, CloseFailures: s.CloseFailures - before.CloseFailures,
		AcquireDuration: s.AcquireDuration - before.AcquireDuration, ReuseDuration: s.ReuseDuration - before.ReuseDuration,
		StartDuration: s.StartDuration - before.StartDuration, CloseDuration: s.CloseDuration - before.CloseDuration,
		WaitDuration: s.WaitDuration - before.WaitDuration,
	}
}

type routerTimingCounters struct {
	acquires, acquireFailures, reuses, starts, startFailures, raceReuses, closes, closeFailures atomic.Uint64
	acquireNanos, reuseNanos, startNanos, closeNanos, waitNanos, evictionNanos                  atomic.Int64
}

// TimingStats is lock-free so sampling a slow acquisition does not wait on the
// provider-map mutex being measured. Concurrent operations may finish between
// counter loads; snapshots are observational, not transactional.
func (r *Router) TimingStats() RouterTimingStats {
	return RouterTimingStats{
		Acquires: r.timing.acquires.Load(), AcquireFailures: r.timing.acquireFailures.Load(),
		Reuses: r.timing.reuses.Load(), Starts: r.timing.starts.Load(), StartFailures: r.timing.startFailures.Load(),
		Evictions: r.evictions.Load(), EvictionDuration: time.Duration(r.timing.evictionNanos.Load()),
		RaceReuses: r.timing.raceReuses.Load(), Closes: r.timing.closes.Load(), CloseFailures: r.timing.closeFailures.Load(),
		AcquireDuration: time.Duration(r.timing.acquireNanos.Load()), ReuseDuration: time.Duration(r.timing.reuseNanos.Load()),
		StartDuration: time.Duration(r.timing.startNanos.Load()), CloseDuration: time.Duration(r.timing.closeNanos.Load()),
		WaitDuration: time.Duration(r.timing.waitNanos.Load()),
	}
}

// closeTimedProvider preserves the caller's synchronous/asynchronous close
// policy while exposing shutdown latency and errors for every lifecycle reason.
func (r *Router) closeTimedProvider(p *Provider, spec, workspace, reason string) error {
	started := time.Now()
	fields := []zap.Field{zap.String("spec", spec), zap.String("workspace", workspace), zap.String("reason", reason)}
	r.logger.Info("LSP router provider close starting", fields...)
	err := p.Close()
	elapsed := time.Since(started)
	r.timing.closes.Add(1)
	r.timing.closeNanos.Add(int64(elapsed))
	if reason == "lru" {
		r.timing.evictionNanos.Add(int64(elapsed))
	}
	fields = append(fields, zap.Duration("elapsed", elapsed))
	if err != nil {
		r.timing.closeFailures.Add(1)
		r.logger.Warn("LSP router provider close failed", append(fields, zap.Error(err))...)
	} else {
		r.logger.Info("LSP router provider close complete", fields...)
	}
	return err
}

// ensureTimedProvider measures construction-independent initialize latency.
func (r *Router) ensureTimedProvider(p *Provider, spec, workspace string, constructionDuration time.Duration) error {
	started := time.Now()
	r.timing.starts.Add(1)
	fields := []zap.Field{zap.String("spec", spec), zap.String("workspace", workspace), zap.Duration("construct_elapsed", constructionDuration)}
	r.logger.Info("LSP router provider startup starting", fields...)
	err := p.EnsureClient(workspace)
	elapsed := time.Since(started)
	r.timing.startNanos.Add(int64(elapsed))
	fields = append(fields, zap.Duration("elapsed", elapsed))
	if err != nil {
		r.timing.startFailures.Add(1)
		r.logger.Warn("LSP router provider startup failed", append(fields, zap.Error(err))...)
	} else {
		r.logger.Info("LSP router provider startup complete", fields...)
	}
	return err
}

// Stats reports the live provider names and their last-used times.
// Intended for debug / status endpoints. Spec is the LSP server name;
// Workspace is the rootURI the server is initialised against.
type RouterStat struct {
	Spec      string    `json:"spec"`
	Workspace string    `json:"workspace"`
	LastUsed  time.Time `json:"last_used"`
	// InUse is the provider's pin count — how many callers currently
	// hold it for a long operation via ProviderForSpecWorkspace. A
	// pinned provider is skipped by both the reaper and the LRU
	// evictor, so a count that never returns to zero is the signature
	// of a pass that ended without its ReleaseSpecWorkspace, and it
	// pins a subprocess alive for the life of the daemon. Surfacing it
	// is what makes that leak observable from `gortex daemon status`.
	InUse int `json:"in_use"`
}

// Stats returns one entry per live (spec, workspace) provider.
func (r *Router) Stats() []RouterStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RouterStat, 0, len(r.providers))
	for key, rp := range r.providers {
		out = append(out, RouterStat{
			Spec:      key.specName,
			Workspace: key.workspace,
			LastUsed:  rp.lastUsed,
			InUse:     rp.inUse,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spec != out[j].Spec {
			return out[i].Spec < out[j].Spec
		}
		return out[i].Workspace < out[j].Workspace
	})
	return out
}

// Names returns "spec@workspace" identifiers for live providers
// (helper for tests + status output). Sorted for stable output.
func (r *Router) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.providers))
	for k := range r.providers {
		names = append(names, formatProviderKey(k.specName, k.workspace))
	}
	sort.Strings(names)
	return names
}

// SupportedLanguages returns the set of languages the router can serve
// (any spec with at least one alt command on PATH). Used to advertise
// capability to MCP clients on startup.
func (r *Router) SupportedLanguages() []string {
	seen := make(map[string]bool)
	for _, s := range r.AvailableSpecs() {
		for _, l := range s.Languages {
			seen[l] = true
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// MarshalDescription returns a human-readable status for one router,
// used by the daemon's `gortex daemon status` command.
func (r *Router) MarshalDescription() string {
	stats := r.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "lsp-router default=%s alive=%d\n", r.defaultWorkspace, len(stats))
	for _, s := range stats {
		fmt.Fprintf(&b, "  %s@%s last_used=%s\n", s.Spec, s.Workspace, s.LastUsed.Format(time.RFC3339))
	}
	return b.String()
}

// DefaultWorkspace returns the workspace root used by For / ForSpec
// when the caller doesn't supply one explicitly. Exposed for status /
// debug surfaces.
func (r *Router) DefaultWorkspace() string { return r.defaultWorkspace }
