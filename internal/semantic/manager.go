package semantic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// LSPRouter is the slice of lsp.Router that semantic.Manager needs to
// drive batch enrichment without importing the lsp package directly
// (which would create an import cycle, since lsp already imports
// semantic for the Provider interface).
type LSPRouter interface {
	// EnabledSpecNames returns the names of LSP specs the user has
	// enabled in config (no spawn implied — call ProviderForSpec to
	// trigger lazy spawn).
	EnabledSpecNames() []string

	// SpecAvailable reports whether the named spec is enabled AND
	// its command resolves on PATH. Pure read — no subprocess
	// spawn. Used by Manager.HasProviders.
	SpecAvailable(name string) bool

	// SpecLanguages returns the language codes the named spec
	// serves. Pure metadata read — no spawn. Used by EnrichAll
	// arbitration to compare candidate specs per language.
	SpecLanguages(name string) []string

	// SpecPriority returns the spec's default priority (lower = wins
	// over higher). Pure metadata read — no spawn.
	SpecPriority(name string) int

	// ProviderForSpec lazy-spawns and returns the LSP provider for
	// the given spec name as a semantic.Provider. Returns an error
	// if the spec is not enabled or its command is not on PATH.
	ProviderForSpec(name string) (Provider, error)

	// ProviderForSpecWorkspace is ProviderForSpec scoped to a workspace
	// root, so each repo gets its own provider instance (keyed by
	// (spec, workspace)). Per-repo instances are what let cross-repo
	// enrichment run concurrently without sharing one provider's LSP
	// connection or document caches. While the returned provider is held,
	// it is pinned in-use; the caller MUST pair it with ReleaseSpecWorkspace.
	ProviderForSpecWorkspace(name, workspace string) (Provider, error)

	// ReleaseSpecWorkspace marks a provider obtained via
	// ProviderForSpecWorkspace as no longer in active use, so the router's
	// LRU evictor / idle reaper may reclaim it. Pairs one-to-one with
	// ProviderForSpecWorkspace so a slow, in-use server is never evicted
	// mid-pass by another repo's concurrent spawn.
	ReleaseSpecWorkspace(name, workspace string)

	// MaxAlive returns the current live-provider cap (zero = unbounded),
	// and SetMaxAlive changes it at runtime. Batch enrichment raises the
	// cap for the duration of a multi-repo pass so concurrent passes do
	// not evict each other's warmed servers, then restores it.
	MaxAlive() int
	SetMaxAlive(n int)

	// EvictionCount returns the lifetime count of LRU evictions, sampled
	// before/after a batch to observe provider churn.
	EvictionCount() uint64

	// Close shuts down every active provider. Called by Manager.Close.
	Close() error
}

// SupplementalProvider is an optional interface a Provider MAY
// implement to opt out of the per-language arbitration: instead of
// competing for a language slot it always runs (when available and not
// config-disabled) in addition to whichever provider won the slot.
// The in-process tree-sitter type resolvers implement it so they
// coexist with LSP / SCIP providers — their AST-grade provenance never
// downgrades a compiler-grade edge, and a language with no external
// tooling still gets type-aware enrichment.
type SupplementalProvider interface {
	Supplemental() bool
}

func isSupplemental(p Provider) bool {
	sp, ok := p.(SupplementalProvider)
	return ok && sp.Supplemental()
}

// Manager orchestrates multiple semantic providers and coordinates enrichment.
type Manager struct {
	providers []Provider
	config    Config
	logger    *zap.Logger

	// lspRouter, when non-nil, owns subprocess lifecycle for LSP
	// providers (idle reaper + LRU eviction + PATH-availability
	// cache). EnrichAll asks it for providers via ProviderForSpec
	// instead of holding hard references that would defeat reaping.
	lspRouter LSPRouter

	// checkouts admits the per-(language, checkout root) language-server
	// workspaces EnrichCheckout runs through, against one global cap. It is
	// separate from the router's own live-provider cap because it bounds a
	// different population: the router caps what the tracked repositories keep
	// warm, and this caps the second copies routed checkouts add on top.
	checkouts *CheckoutWorkspaces

	mu          sync.RWMutex
	lastResults map[string]*EnrichResult // provider name → last result
	// enrichStatus tracks the lifecycle of every per-(repo, provider)
	// enrichment pass (running / completed / partial / abandoned /
	// failed) so index_health can surface an un-enriched graph instead
	// of reporting green. Keyed by repo + "\x00" + provider name.
	enrichStatus map[string]*EnrichmentStatus

	// lifecycleCtx is cancelled before providers are closed. activePasses
	// covers the whole manager-owned pass (including terminal marker writes),
	// not merely the provider call, so Close never returns while an enrichment
	// goroutine can still mutate the graph. lifecycleMu serialises Add with
	// Close's transition to closing; sync.WaitGroup alone cannot safely do so
	// when Add and Wait race at a zero count.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	lifecycleMu     sync.Mutex
	activePasses    sync.WaitGroup
	closing         bool
	closeOnce       sync.Once
	closeErr        error
}

// NewManager creates a Manager from configuration.
// It registers providers based on config, probes availability, and logs results.
func NewManager(cfg Config, logger *zap.Logger) *Manager {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	m := &Manager{
		config:          cfg,
		logger:          logger,
		checkouts:       NewCheckoutWorkspaces(cfg.checkoutWorkspaceCap(), logger),
		lastResults:     make(map[string]*EnrichResult),
		enrichStatus:    make(map[string]*EnrichmentStatus),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	return m
}

var errManagerClosed = errors.New("semantic manager is closed")

// beginPass registers a complete manager-owned enrichment pass. Close first
// flips closing under the same mutex and can then Wait without racing a new
// WaitGroup.Add. A pass that was admitted must call endPass on every return.
func (m *Manager) beginPass() bool {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.closing {
		return false
	}
	m.activePasses.Add(1)
	return true
}

func (m *Manager) endPass() {
	m.activePasses.Done()
}

// passContext joins a caller-owned context to the Manager lifecycle. The
// returned context is cancelled when either parent is done or Close begins.
func (m *Manager) passContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	m.lifecycleMu.Lock()
	lifecycleCtx := m.lifecycleCtx
	closing := m.closing
	m.lifecycleMu.Unlock()
	if closing {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() {}
	}
	if lifecycleCtx == nil { // Supports zero-value Managers used by small tests.
		return context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancel(parent)
	stopLifecycle := context.AfterFunc(lifecycleCtx, cancel)
	return ctx, func() {
		stopLifecycle()
		cancel()
	}
}

// RegisterProvider adds a provider to the manager.
func (m *Manager) RegisterProvider(p Provider) {
	m.providers = append(m.providers, p)
	m.logger.Info("semantic provider registered",
		zap.String("name", p.Name()),
		zap.Strings("languages", p.Languages()),
		zap.Bool("available", p.Available()),
	)
}

// SetLSPRouter installs the daemon-managed LSP router. Once set,
// EnrichAll will lazy-spawn LSP providers via the router (allowing
// idle reaping + LRU eviction) instead of expecting them to be
// pre-registered via RegisterProvider. Pass nil to detach.
//
// Boot order matters — call SetLSPRouter before EnrichAll runs the
// first time. The router does not need to be populated yet; specs are
// resolved lazily via ProviderForSpec.
func (m *Manager) SetLSPRouter(r LSPRouter) {
	m.lspRouter = r
	// The router is also what can stop a per-checkout workspace's servers, so
	// the eviction path is wired from the same call rather than from a second
	// one a caller could forget. A router that cannot stop one — and a nil
	// detach — leaves the registry evicting its bookkeeping alone.
	stopper, _ := r.(WorkspaceStopper)
	m.checkouts.SetStopper(stopper)
}

// LSPRouter returns the configured LSPRouter, or nil if none has been
// installed.
func (m *Manager) LSPRouter() LSPRouter {
	return m.lspRouter
}

// RepoEnrichState is the git freshness of one repo at enrichment time: the
// HEAD commit the graph reflects and whether the working tree carried
// uncommitted changes. The deferred-enrichment caller computes it once and
// threads it in so the per-provider skip gate and the completion-marker write
// agree on the identical (sha, dirty).
type RepoEnrichState struct {
	SHA   string
	Dirty bool
	// Force bypasses the completion-marker skip gate for this repo even when
	// the marker still records SHA on a clean tree. The deferred-enrichment
	// caller sets it when the repo was whole-repo re-parsed this run (a full
	// re-track / cold TrackRepo via IndexCtx): that pass evicts and re-creates
	// every node and edge, so it drops the LSP hover-enrichment edges the
	// marker claims are present. Without the bypass the marker would skip the
	// re-enrichment and the graph would be durably left missing that repo's
	// enrichment edges until HEAD moves or the tree goes dirty. The clean
	// non-partial completion still refreshes the marker afterwards.
	Force bool

	// CheckoutID scopes the pass to one routed automatic checkout's working
	// copy. Empty — every caller but EnrichCheckout — means the repository's
	// base corpus, and keeps the legacy marker key exactly as it was.
	//
	// It exists because every checkout of a family shares one repo prefix. A
	// checkout pass writing the unscoped key would overwrite the primary's
	// completion marker with a sha the primary's tree was never at, and the
	// next warm restart would skip the repository's enrichment on that
	// evidence. See enrichMarkerProvider.
	CheckoutID string
}

// EnrichOptions carries the optional per-repo freshness inputs to EnrichAll.
// A zero value disables both the skip gate and the marker write — every
// provider runs and nothing is persisted, matching the pre-feature behaviour
// the tests and the inline full-index path want. Only the deferred-enrichment
// path (a warm restart re-enriching persisted repos) supplies it.
type EnrichOptions struct {
	// RepoState maps a repo prefix (a roots key) to the git freshness the
	// caller observed for it. When an entry has a non-empty SHA, EnrichAll
	// (a) skips a provider whose persisted completion marker records the same
	// SHA on a clean tree, and (b) writes/refreshes that marker on the
	// provider's non-partial completion. A repo absent from the map — or with
	// an empty SHA — is never gated (always enriched) and never marked: the
	// same "no freshness evidence, don't skip" default the language-presence
	// gate uses.
	RepoState map[string]RepoEnrichState

	// MinLanguageNodes is the admission floor: a language with fewer than
	// this many enrichable nodes across the enriched roots is treated as NOT
	// present, so no provider (in-process engine or LSP spawn) runs for it.
	// 0 disables the floor — the default for every path except the
	// index-time enrichment calls, which set EnrichmentAdmissionFloor(). The
	// floor is what stops a Rust repo's go-binding stub or a grammar repo's
	// seven-node bindings/go tree from admitting a whole Go compiler pass.
	MinLanguageNodes int

	// Languages, when non-empty, narrows the pass to these language codes: a
	// provider whose languages the list does not name is skipped even though
	// the graph holds nodes for it. EnrichCheckout sets it to the languages
	// the workspace cap admitted, so a starved language is not enriched by
	// some other provider that happens to cover it as a side language.
	Languages []string

	// ApplyGate, when non-nil, parks every provider's graph-apply phase until
	// the channel closes (see WithApplyGate). The warmup sets it while the
	// enrichment pool overlaps the resolve phase so compute proceeds but no
	// apply can starve the resolver on the shared ResolveMutex.
	ApplyGate <-chan struct{}
}

// defaultEnrichmentAdmissionFloor balances two measured failure modes:
// seven-node tree-sitter go bindings admitted minutes-long go/packages loads
// (the floor must sit above ~10), while genuinely small-but-real language
// surfaces (a ~70-node Go tree inside a TS repo) should keep compiler-grade
// enrichment (the floor must stay well under that).
const defaultEnrichmentAdmissionFloor = 16

// EnrichmentAdmissionFloor resolves the index-time admission floor:
// GORTEX_ENRICH_MIN_NODES overrides (0 or a negative value disables),
// otherwise the default applies.
func EnrichmentAdmissionFloor() int {
	if v := strings.TrimSpace(os.Getenv("GORTEX_ENRICH_MIN_NODES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				return 0
			}
			return n
		}
	}
	return defaultEnrichmentAdmissionFloor
}

// EnrichAll runs all available providers against the graph.
// For each language, only the highest-priority available provider runs.
//
// The second return value reports, per repo prefix, whether that repo's
// enrichment was left incomplete — true when any provider it ran was cut
// short (Partial), abandoned at its deadline, or failed. A repo whose every
// provider finished cleanly (or that had no eligible provider) is absent from
// the map. Callers gating deferred-enrichment retries key off this: a partial
// repo keeps its pending marker so a later pass retries instead of trusting an
// incomplete graph.
//
// opts threads the per-repo git freshness so a provider whose persisted
// completion marker still matches HEAD on a clean tree is skipped instead of
// re-running its (minutes-long) hover pass; a zero value gates nothing.
func (m *Manager) EnrichAll(g graph.Store, roots map[string]string, opts EnrichOptions) ([]*EnrichResult, map[string]bool, error) {
	partial := make(map[string]bool)
	if !m.config.Enabled {
		return nil, partial, nil
	}

	// Build a map of language → sorted providers (by priority from config).
	// This covers SCIP / go-analysis / legacy LSP providers eagerly
	// registered via RegisterProvider.
	langProviders := m.selectProviders()

	// Languages actually present (in symbol-bearing nodes) across the repos
	// being enriched, from the indexed repo-scoped node scan. A provider — and
	// the LSP server spawn it would trigger — is skipped when none of its
	// languages are present, so a clangd / sourcekit / ruby-lsp never starts
	// for a repo that has no C / Swift / Ruby. This is the same condition the
	// per-provider EnrichRepo gate already applies, lifted ahead of the spawn.
	//
	// The gate fires only on POSITIVE evidence of absence: when the repo set
	// has indexed symbols (present is non-empty) but none in a provider's
	// languages. An empty / unindexed graph yields no evidence, so we don't
	// gate — providers fall through to their own per-pass gate as before.
	// nodeCounts (enrichable nodes per repo) feeds the size-scaled per-repo
	// deadline — see enrichRepoTimeout.
	present, nodeCounts, langCounts := m.repoLanguages(g, roots)
	if floor := opts.MinLanguageNodes; floor > 0 {
		below := make(map[string]int)
		for lang, count := range langCounts {
			if count < floor {
				below[lang] = count
				delete(present, lang)
			}
		}
		if len(below) > 0 {
			m.logger.Info("semantic enrichment: languages below admission floor",
				zap.Int("floor", floor),
				zap.Any("skipped", below))
		}
	}
	// A caller-supplied language set narrows what the census found. It is an
	// intersection rather than a replacement: the list says which languages
	// the caller is willing to pay for, not that the graph holds them.
	if len(opts.Languages) > 0 {
		allowed := make(map[string]bool, len(opts.Languages))
		for _, language := range opts.Languages {
			allowed[language] = true
		}
		for language := range present {
			if !allowed[language] {
				delete(present, language)
			}
		}
	}
	// Presence gating needs only EVIDENCE, not survivors: when the census saw
	// rows but the floor rejected every language, providers must still be
	// gated off rather than falling through to their own per-pass gates. A
	// caller-supplied language set is evidence of its own — it is a decision
	// about what may run, and an empty graph must not turn it back off.
	gateOnPresence := len(langCounts) > 0 || len(opts.Languages) > 0
	if len(present) > 0 {
		langs := make([]string, 0, len(present))
		for l := range present {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		m.logger.Info("semantic enrichment: repo languages present",
			zap.Strings("languages", langs),
		)
	}

	var results []*EnrichResult

	// Deterministic, primary-language-first order: the language with the most
	// enrichable nodes runs first, so on a multi-language repo the dominant
	// language's provider claims the bounded enrichment window before a minor
	// language's. Ties break by language name. This replaces a Go map-range
	// whose randomised order let whichever language the map happened to yield
	// first win the wall-clock — a Go-primary repo with a minor TS tree could
	// see its gopls pass never run because tsserver enriched first.
	for _, lang := range orderLangsByComposition(langProviders, langCounts) {
		provider := langProviders[lang]
		if !provider.Available() {
			m.logger.Debug("semantic provider unavailable, skipping",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
			)
			continue
		}
		if gateOnPresence && !anyLangPresent(provider.Languages(), present) {
			m.logger.Debug("semantic provider skipped, no nodes for its languages",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
			)
			continue
		}

		results = m.runEnrichForProvider(g, roots, lang, provider, nodeCounts, opts, results, partial)
	}

	// Router-backed LSP providers: arbitrate by priority per language
	// BEFORE spawning so unused candidates never start a subprocess.
	// Two router specs claiming the same language pick the lowest
	// priority number; ties break by spec-name lexicographic order.
	// Eager providers from selectProviders already won their language;
	// router specs may only fill gaps.
	//
	// Skipped unless EagerLSP is set: the subprocess LSP sweep is the slowest
	// part of a cold index and its net-new value over the in-process tiers
	// (go-types for Go, the tree-sitter floor for every language) is narrow.
	// Leaving it out of the synchronous pass is what keeps cold/warm start
	// fast; the router is still wired, so a query can lazy-spawn a server on
	// demand.
	if m.config.EagerLSP && m.lspRouter != nil {
		// Pre-pass: pure metadata, no spawn.
		bestSpec := make(map[string]string) // language → winning spec name
		bestPrio := make(map[string]int)
		for _, name := range m.lspRouter.EnabledSpecNames() {
			if !m.lspRouter.SpecAvailable(name) {
				continue
			}
			prio := m.lspRouter.SpecPriority(name)
			if cfgPrio, ok := m.configPriorityFor(name); ok {
				prio = cfgPrio
			}
			for _, lang := range m.lspRouter.SpecLanguages(name) {
				if _, eagerCovered := langProviders[lang]; eagerCovered {
					continue
				}
				// Only compete for a language the repo set actually contains —
				// a spec that wins no present language never enters runOrder and
				// so is never spawned via ProviderForSpec. Skipped when there is
				// no presence evidence at all (empty / unindexed graph).
				if gateOnPresence && !present[lang] {
					continue
				}
				cur, exists := bestSpec[lang]
				if !exists || prio < bestPrio[lang] || (prio == bestPrio[lang] && name < cur) {
					bestSpec[lang] = name
					bestPrio[lang] = prio
				}
			}
		}
		// One spec may win multiple languages — dedup so Enrich runs
		// once per spec, not once per (spec, language) pair.
		runOrder := make([]string, 0)
		seenSpec := make(map[string]bool)
		for _, name := range bestSpec {
			if seenSpec[name] {
				continue
			}
			seenSpec[name] = true
			runOrder = append(runOrder, name)
		}
		sort.Strings(runOrder)
		for _, name := range runOrder {
			// Fetch a provider per repo root (keyed by workspace) rather than
			// one shared default-workspace provider, so concurrent cross-repo
			// enrichment never shares a single LSP connection or document cache.
			for _, repoName := range sortedRootNames(roots, nodeCounts) {
				repoRoot := roots[repoName]
				provider, err := m.lspRouter.ProviderForSpecWorkspace(name, repoRoot)
				if err != nil {
					m.logger.Debug("router-backed LSP provider unavailable, skipping",
						zap.String("spec", name),
						zap.String("repo", repoName),
						zap.Error(err),
					)
					continue
				}
				// ProviderForSpecWorkspace pinned the provider in-use; release
				// it once this repo's pass returns so the evictor can reclaim it.
				func() {
					defer m.lspRouter.ReleaseSpecWorkspace(name, repoRoot)
					langs := provider.Languages()
					if len(langs) == 0 {
						return
					}
					results = m.runEnrichOne(g, repoName, repoRoot, langs[0], provider, nodeCounts[repoName], opts.RepoState[repoName], opts.ApplyGate, results, partial)
				}()
			}
		}
	}

	// Supplemental providers run last, outside arbitration: they only
	// hold AST-grade provenance, so running after a compiler-grade
	// winner can confirm-but-never-downgrade what it stamped.
	for _, p := range m.providers {
		if !isSupplemental(p) || !p.Available() || m.providerDisabled(p.Name()) {
			continue
		}
		langs := p.Languages()
		if len(langs) == 0 {
			continue
		}
		if gateOnPresence && !anyLangPresent(langs, present) {
			continue
		}
		results = m.runEnrichForProvider(g, roots, langs[0], p, nodeCounts, opts, results, partial)
	}

	return results, partial, nil
}

// repoLanguages returns the union of languages present (in symbol-bearing
// nodes) across the given repo roots, computed from the indexed repo-scoped
// node scan. Used to skip providers — and the LSP server spawns they would
// trigger — for languages a repo set does not contain. Mirrors the
// node-language condition the per-provider EnrichRepo gate applies, so a
// provider is gated here exactly when its own pass would have found no work.
//
// The second return value counts the enrichable nodes per repo (same
// filters), which sizes the per-repo enrichment deadline — see
// enrichRepoTimeout. The third counts them per language across all repos — the
// composition signal EnrichAll orders providers by (primary language first).
func (m *Manager) repoLanguages(g graph.Store, roots map[string]string) (map[string]bool, map[string]int, map[string]int) {
	present := make(map[string]bool)
	counts := make(map[string]int, len(roots))
	// langCounts is the enrichable-node count per language across all repos —
	// the composition signal EnrichAll ranks providers by so the dominant
	// language enriches first.
	langCounts := make(map[string]int)
	repoPrefixes := make([]string, 0, len(roots))
	for repoPrefix := range roots {
		repoPrefixes = append(repoPrefixes, repoPrefix)
	}
	sort.Strings(repoPrefixes)

	// One compact projection covers every repository. SQLite groups promoted
	// repo/file/language columns without decoding Meta, docs, or signatures;
	// content sections are filtered before rows cross the database boundary.
	for _, row := range graph.ReadRepoLanguageFileCounts(g, repoPrefixes) {
		if _, tracked := roots[row.RepoPrefix]; !tracked || row.Language == "" || row.Count <= 0 {
			continue
		}
		// Generated / vendored files don't make a language "present" — a
		// repo whose only C is tree-sitter's generated parser.c should not
		// spawn clangd just to index it.
		if IsLowValueForEnrichment(row.FilePath, m.config.ExcludeGlobs) {
			continue
		}
		// Fixture corpora don't either: a Go repo's TS/Java/Python parser
		// fixtures are indexed and searchable but are not evidence that the
		// repo needs those languages' type-inference passes.
		if IsFixtureCensusPath(row.FilePath) {
			continue
		}
		present[row.Language] = true
		counts[row.RepoPrefix] += row.Count
		langCounts[row.Language] += row.Count
	}
	return present, counts, langCounts
}

// anyLangPresent reports whether any of langs is in the present set.
func anyLangPresent(langs []string, present map[string]bool) bool {
	for _, l := range langs {
		if present[l] {
			return true
		}
	}
	return false
}

// orderLangsByComposition returns the languages of langProviders sorted by the
// repo set's composition: most enrichable nodes first (from langCounts), ties
// broken by language name. It gives EnrichAll a deterministic, primary-first
// provider run order in place of a randomised map-range.
func orderLangsByComposition(langProviders map[string]Provider, langCounts map[string]int) []string {
	langs := make([]string, 0, len(langProviders))
	for lang := range langProviders {
		langs = append(langs, lang)
	}
	sort.Slice(langs, func(i, j int) bool {
		if ci, cj := langCounts[langs[i]], langCounts[langs[j]]; ci != cj {
			return ci > cj
		}
		return langs[i] < langs[j]
	})
	return langs
}

// sortedRootNames returns the repo keys of roots in a deterministic order (most
// enrichable nodes first, ties by name), so a per-repo enrichment loop no
// longer visits repos in Go's randomised map-iteration order. counts may be nil
// (falls back to name order).
func sortedRootNames(roots map[string]string, counts map[string]int) []string {
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if ci, cj := counts[names[i]], counts[names[j]]; ci != cj {
			return ci > cj
		}
		return names[i] < names[j]
	})
	return names
}

// providerDisabled reports an explicit `enabled: false` config entry
// for the named provider. Used by the supplemental run loop, which
// never passes through selectProviders' config gate.
func (m *Manager) providerDisabled(name string) bool {
	for _, pc := range m.config.Providers {
		if pc.Name == name {
			return !pc.Enabled
		}
	}
	return false
}

// configPriorityFor returns the user's config-overridden priority for
// the named provider, if any. Used to let `.gortex.yaml` take
// precedence over the spec's built-in default.
func (m *Manager) configPriorityFor(name string) (int, bool) {
	for _, pc := range m.config.Providers {
		if pc.Name == name && pc.Enabled {
			return pc.Priority, true
		}
	}
	return 0, false
}

// runEnrichForProvider executes Enrich for one provider against every
// repo root and appends the results. Extracted so EnrichAll can share
// the logging + lastResults bookkeeping between eager and Router-backed
// providers.
func (m *Manager) runEnrichForProvider(g graph.Store, roots map[string]string, lang string, provider Provider, nodeCounts map[string]int, opts EnrichOptions, results []*EnrichResult, partial map[string]bool) []*EnrichResult {
	for _, repoName := range sortedRootNames(roots, nodeCounts) {
		results = m.runEnrichOne(g, repoName, roots[repoName], lang, provider, nodeCounts[repoName], opts.RepoState[repoName], opts.ApplyGate, results, partial)
	}
	return results
}

// Per-repo enrichment deadline sizing. The floor covers server spin-up
// plus a small repo's sweep; the per-node term tracks the real cost
// driver (one hover + up to three hierarchy calls per symbol node,
// maxParallel-wide); the ceiling stops a monorepo from pinning the
// enrichment WaitGroup for hours. The per-call LSP timeout (see
// lsp.Provider.ensureClient) already bounds a single wedged request —
// this bounds a provider that is merely slow across many symbols.
// GORTEX_LSP_ENRICH_TIMEOUT overrides the computed value verbatim;
// 0 / "off" disables the bound.
const (
	defaultEnrichRepoTimeout = 10 * time.Minute
	enrichTimeoutPerNode     = 40 * time.Millisecond
	maxEnrichRepoTimeout     = 90 * time.Minute
)

// enrichCancelGrace bounds how long the manager waits, after the
// deadline cancels a ContextEnricher's context, for the provider to
// flush its completed work and return the partial result. Generous —
// it only has to cover one in-flight LSP call (individually bounded)
// plus the final flush; a provider still silent after it is wedged in
// an uncancellable call and gets abandoned like a legacy provider.
// Var (not const) so tests can shrink it.
var enrichCancelGrace = 2 * time.Minute

// enrichReadinessBudget bounds how long the manager waits for a ReadinessProber
// provider's server to become ready (its Roslyn / MSBuild solution load to
// finish) BEFORE the per-repo enrichment deadline starts. Capping the wait
// keeps a server that never becomes ready from stalling the pipeline — the pass
// then proceeds best-effort. Var (not const) so tests can shrink it.
var enrichReadinessBudget = 3 * time.Minute

// scaleEnrichTimeout returns the size-scaled per-repo enrichment
// deadline for a repo with nodeCount enrichable nodes: floor + per-node
// cost, capped at the ceiling. A fixed 10-minute bound was tuned for
// small repos — a medium repo (tens of thousands of symbol nodes) pays
// 15-25 minutes of legitimate gopls work and was being cut mid-pass.
func scaleEnrichTimeout(nodeCount int) time.Duration {
	if nodeCount < 0 {
		nodeCount = 0
	}
	d := defaultEnrichRepoTimeout + time.Duration(nodeCount)*enrichTimeoutPerNode
	if d > maxEnrichRepoTimeout {
		return maxEnrichRepoTimeout
	}
	return d
}

// enrichRepoTimeout resolves the per-repo enrichment deadline for a repo
// with nodeCount enrichable nodes. The GORTEX_LSP_ENRICH_TIMEOUT env
// override (a Go duration such as "5m"; "0" / "off" / "none" disables
// it) wins verbatim; otherwise the deadline scales with repo size (see
// scaleEnrichTimeout). An unparseable value falls back to the scaled
// default.
func enrichRepoTimeout(nodeCount int) time.Duration {
	switch v := strings.TrimSpace(os.Getenv("GORTEX_LSP_ENRICH_TIMEOUT")); v {
	case "":
		return scaleEnrichTimeout(nodeCount)
	case "0", "off", "none":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		return scaleEnrichTimeout(nodeCount)
	}
}

// enrichOuterCeiling is the generous outer bound the Manager places on a
// ContextEnricher's context and its abandon-grace timer. The provider narrows
// it to a lazy, candidate-scaled deadline once selection is done (see
// EnrichDeadlinePolicy) — so the outer path is sized to the hard per-repo
// ceiling, NOT the whole-repo node estimate, which is exactly the headroom
// lazy budgeting reclaims. This only ever backstops a provider wedged in an
// uncancellable call. GORTEX_LSP_ENRICH_TIMEOUT pins it verbatim (matching the
// inner enrichRepoTimeout policy so the override still wins end to end);
// "0" / "off" / "none" disables the bound; garbage falls back to the ceiling.
func enrichOuterCeiling() time.Duration {
	switch v := strings.TrimSpace(os.Getenv("GORTEX_LSP_ENRICH_TIMEOUT")); v {
	case "":
		return maxEnrichRepoTimeout
	case "0", "off", "none":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		return maxEnrichRepoTimeout
	}
}

// setEnrichStatus records the lifecycle state of one (repo, provider)
// enrichment pass for the index_health surface. result may be nil.
func (m *Manager) setEnrichStatus(repo, provider, lang, state string, deadline time.Duration, result *EnrichResult, detail string) {
	st := &EnrichmentStatus{
		Repo:     repo,
		Provider: provider,
		Language: lang,
		State:    state,
		Detail:   detail,
	}
	if deadline > 0 {
		st.DeadlineSeconds = deadline.Seconds()
	}
	if result != nil {
		st.DurationMs = result.DurationMs
		st.EdgesConfirmed = result.EdgesConfirmed
		st.EdgesRebound = result.EdgesRebound
		st.EdgesAdded = result.EdgesAdded
		st.NodesEnriched = result.NodesEnriched
		st.SymbolsTotal = result.SymbolsTotal
		st.SymbolsCovered = result.SymbolsCovered
		st.CoveragePercent = result.CoveragePercent
		// Fill (and back-stamp) the bounding reason so a "completed" state
		// that covered < 100% of its targets is never read as full coverage.
		if result.BoundReason == "" {
			result.BoundReason = enrichBoundReason(state, result)
		}
		st.BoundReason = result.BoundReason
		st.ReferencesAddPass = result.ReferencesAddPass
		st.Degraded = result.Degraded
		st.DegradedReason = result.DegradedReason
	}
	key := repo + "\x00" + provider
	m.mu.Lock()
	if state == EnrichStateRunning {
		st.StartedAt = time.Now()
	} else if prev, ok := m.enrichStatus[key]; ok {
		// Carry the running pass's start time forward onto its terminal
		// status so a caller that only polls after completion can still
		// compute how long the pass actually ran.
		st.StartedAt = prev.StartedAt
	}
	m.enrichStatus[key] = st
	m.mu.Unlock()
}

// enrichBoundReason classifies why the add-phase stopped: a cut pass is
// budget-bound; a finished pass that skipped some targets is cap-bound; a
// finished pass that visited every target is completed-all.
func enrichBoundReason(state string, r *EnrichResult) string {
	if r.Partial || state == EnrichStatePartial || state == EnrichStateAbandoned {
		return EnrichBoundBudget
	}
	if r.SymbolsTotal > 0 && r.SymbolsCovered < r.SymbolsTotal {
		return EnrichBoundCap
	}
	return EnrichBoundCompletedAll
}

// EnrichmentStatuses returns a stable-ordered snapshot of every
// per-(repo, provider) enrichment pass the manager has run or is
// running. Consumed by index_health so an agent can see a graph whose
// enrichment was cut (partial) or discarded (abandoned) instead of
// trusting a green file count.
func (m *Manager) EnrichmentStatuses() []EnrichmentStatus {
	m.mu.RLock()
	out := make([]EnrichmentStatus, 0, len(m.enrichStatus))
	for _, st := range m.enrichStatus {
		out = append(out, *st)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// EnrichmentActive reports whether any per-(repo, provider) enrichment
// pass is currently in the running state. The LLM lifecycle gate uses it
// to defer an expensive local-model cold load while enrichment is in
// flight — the two must not contend for CPU/GPU/RAM on a small machine.
func (m *Manager) EnrichmentActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, st := range m.enrichStatus {
		if st.State == EnrichStateRunning || st.State == EnrichStateDraining {
			return true
		}
	}
	return false
}

// enrichMarkerCurrent reports whether the persisted completion marker for
// (repoPrefix, provider) already records rs.SHA on a clean tree, so a
// re-enrichment would confirm nothing. It returns false — never skip — when:
// GORTEX_WARMUP_FORCE_ENRICH=1 forces a full re-enrich, the caller flagged the
// repo rs.Force (it was whole-repo re-parsed this run, so its persisted
// enrichment edges were evicted), the caller supplied no sha (no reliable
// freshness signal), the working tree is dirty, the backend does not persist
// enrichment state, no marker row exists, or the recorded sha differs. The env
// override composes with the repo-level pending gate the deferred-enrichment
// caller applies before ever reaching here.
func (m *Manager) enrichMarkerCurrent(g graph.Store, repoPrefix, provider string, rs RepoEnrichState) bool {
	if os.Getenv("GORTEX_WARMUP_FORCE_ENRICH") == "1" {
		return false
	}
	// A checkout-scoped pass never skips on its own marker. Each working-tree
	// build mints a fresh payload whose nodes carry no enrichment edges, so a
	// marker an earlier generation of the same checkout left says nothing
	// about this one — honouring it would declare a capability complete over a
	// payload nothing enriched. The scoped marker is a record of what ran, not
	// a gate.
	if rs.CheckoutID != "" {
		return false
	}
	// A full re-track re-parsed every file and dropped this repo's hover
	// edges, so the marker's implicit invariant ("marker present + sha match
	// ⇒ the graph carries the enrichment edges") no longer holds — re-enrich
	// even though the sha still matches on a clean tree.
	if rs.Force {
		return false
	}
	if rs.SHA == "" || rs.Dirty {
		return false
	}
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return false
	}
	marker, found, err := store.GetEnrichmentState(repoPrefix, provider)
	if err != nil || !found {
		return false
	}
	return marker.IndexedSHA == rs.SHA
}

// recordEnrichMarker persists the completion marker for a provider that
// finished a non-partial pass, so a later restart can skip it while the repo
// sits at the same clean sha. No-op when the caller supplied no sha (nothing
// to key freshness on), the working tree is dirty (the pass enriched
// uncommitted content, so its edges do not describe the committed state the
// HEAD sha names — the read gate enrichMarkerCurrent likewise refuses to skip
// on a dirty tree, and recording here would be honored as authoritative once
// the tree becomes clean at the same sha), or the backend does not persist
// enrichment state.
func (m *Manager) recordEnrichMarker(g graph.Store, repoPrefix, provider string, rs RepoEnrichState, coverage float64) {
	if rs.SHA == "" {
		return
	}
	// The dirty veto exists because a commit sha cannot name a tree with
	// uncommitted edits in it. A checkout-scoped marker keys on the working
	// tree's own fingerprint, which names exactly that tree, so the veto does
	// not apply to it.
	if rs.Dirty && rs.CheckoutID == "" {
		return
	}
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return
	}
	if err := store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix:  repoPrefix,
		Provider:    enrichMarkerProvider(provider, rs.CheckoutID),
		IndexedSHA:  rs.SHA,
		CompletedAt: time.Now().Unix(),
		Coverage:    coverage,
	}); err != nil {
		m.logger.Warn("persist enrichment marker failed",
			zap.String("repo", repoPrefix),
			zap.String("provider", provider),
			zap.Error(err),
		)
	}
}

// checkoutMarkerSeparator joins a provider name to the checkout a marker is
// scoped to. No provider name and no reserved key contains it, so a scoped key
// can never be read as an unscoped one.
const checkoutMarkerSeparator = "@"

// enrichMarkerProvider spells the key an enrichment marker is stored under.
//
// The marker table is keyed by (repo prefix, provider), and every checkout of
// a family shares one repo prefix — the layers compose over the primary's
// corpus, so their nodes live in the primary's namespace. Base-corpus
// enrichment therefore keeps the bare provider name: markers written before
// checkouts existed stay valid, and every gate that reads them keeps reading
// exactly what it wrote. Only a checkout-scoped pass carries the extra
// dimension, which is what stops one worktree's completion from speaking for
// the primary's or for a sibling worktree's.
//
// The generation dimension rides in the row's IndexedSHA, which for a checkout
// pass is the working tree's fingerprint rather than a commit: one row per
// (checkout, provider) naming the state it was enriched at, instead of one row
// per generation growing without bound behind a checkout that is edited all
// day.
func enrichMarkerProvider(provider, checkoutID string) string {
	if checkoutID == "" {
		return provider
	}
	return provider + checkoutMarkerSeparator + checkoutID
}

// repoEnrichMarkerProvider is the reserved provider key under which the
// whole-repo enrichment completion marker is stored in the per-(repo, provider)
// enrichment_state table. No language provider is named this, so the row never
// collides with a real provider's marker. It records the git revision at which
// EVERY applicable provider finished a non-partial pass for the repo, letting a
// warm restart decide — with a single keyed lookup, without re-deriving which
// providers apply — whether the persisted graph's enrichment is complete or was
// cut short (partial / abandoned) and must be resumed.
const repoEnrichMarkerProvider = "__repo__"

// RecordRepoEnrichmentComplete persists the whole-repo enrichment completion
// marker at sha. The deferred-enrichment driver calls it once a repo's pass
// finished with no provider left partial / abandoned / failed. It shares
// recordEnrichMarker's discipline: a no-op on an empty sha or a dirty tree (the
// marker must describe the committed state the sha names) and on a backend that
// does not persist enrichment state.
func (m *Manager) RecordRepoEnrichmentComplete(g graph.Store, repoPrefix, sha string, dirty bool) {
	m.recordEnrichMarker(g, repoPrefix, repoEnrichMarkerProvider, RepoEnrichState{SHA: sha, Dirty: dirty}, 0)
}

// RepoEnrichmentMarkerState reports the whole-repo enrichment completion marker
// for repoPrefix against sha. persisted is false when the backend does not
// durably store enrichment state (the in-memory graph) — the caller then has no
// completeness signal from the marker and must not force a pass on marker
// evidence alone. When persisted is true, current reports whether a marker
// exists and records exactly sha, i.e. the repo's enrichment finished at this
// clean HEAD and need not be resumed on restart. A read error or an empty sha
// yields (false, true): no positive evidence of completeness, but the backend
// does persist state.
func (m *Manager) RepoEnrichmentMarkerState(g graph.Store, repoPrefix, sha string) (current, persisted bool) {
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return false, false
	}
	if sha == "" {
		return false, true
	}
	marker, found, err := store.GetEnrichmentState(repoPrefix, repoEnrichMarkerProvider)
	if err != nil || !found {
		return false, true
	}
	return marker.IndexedSHA == sha, true
}

// shortSHA truncates a git revision to its 7-char prefix for logging; a
// shorter or empty sha is returned as-is.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// runEnrichOne runs one provider against one repo root and appends the
// result. Split out of runEnrichForProvider so the Router-backed path can
// fetch a per-repo provider instance (keyed by the repo's workspace) before
// dispatching — distinct providers per repo are what makes concurrent
// cross-repo enrichment safe.
//
// The pass is bounded by a per-repo deadline scaled to the repo's
// enrichable node count (env-overridable — see enrichRepoTimeout).
// Providers that implement ContextEnricher are cancelled cooperatively:
// they land everything finished so far, mark the result Partial, and return.
// A provider that ignores cancellation is reported as abandoned after the
// grace window, but the Manager still drains the in-process call before it
// advances. Legacy providers are observed by the same deadline watchdog and
// likewise drained synchronously. At no point is a graph writer detached.
//
// partial records repos left incomplete: any provider that was abandoned,
// failed, or returned a Partial result flips partial[repoName] so the caller
// knows the repo's enrichment must be retried.
func (m *Manager) runEnrichOne(g graph.Store, repoName, repoRoot, lang string, provider Provider, nodeCount int, rs RepoEnrichState, applyGate <-chan struct{}, results []*EnrichResult, partial map[string]bool) []*EnrichResult {
	if !m.beginPass() {
		partial[repoName] = true
		m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateAbandoned, 0, nil,
			errManagerClosed.Error())
		return results
	}
	defer m.endPass()

	// Skip a provider whose persisted completion marker already records this
	// repo's current HEAD on a clean tree: re-running its hover pass would
	// confirm the edges the persisted graph already carries. Only the
	// deferred-enrichment caller supplies a marker sha; every other caller
	// passes a zero RepoEnrichState, so enrichMarkerCurrent returns false and
	// nothing is gated.
	if m.enrichMarkerCurrent(g, repoName, provider.Name(), rs) {
		m.logger.Info("semantic enrichment skipped: completion marker current",
			zap.String("provider", provider.Name()),
			zap.String("language", lang),
			zap.String("repo", repoName),
			zap.String("sha", shortSHA(rs.SHA)),
		)
		return results
	}

	start := time.Now()

	// Readiness gate: a server whose workspace load continues past `initialize`
	// (Roslyn / MSBuild, behind csharp-ls / OmniSharp) answers `initialize`
	// quickly but serves empty results until the solution finishes loading.
	// Bringing it to readiness BEFORE the enrichment deadline starts keeps that
	// cold-load latency out of the query budget — without it the deadline
	// elapses during the load and the pass lands zero edges. Only providers that
	// opt in (ReadinessProber) pay it; a server ready right after initialize does
	// not implement the interface, so gopls / rust-analyzer never wait. Bounded
	// and best-effort: a probe timeout or error just proceeds.
	if rp, ok := provider.(ReadinessProber); ok && enrichReadinessBudget > 0 {
		lifecycleCtx, stopLifecycle := m.passContext(context.Background())
		rctx, rcancel := context.WithTimeout(lifecycleCtx, enrichReadinessBudget)
		err := rp.WaitReady(rctx, repoRoot)
		rcancel()
		stopLifecycle()
		if errors.Is(err, ErrWorkspaceNotReady) {
			// The server never finished loading its workspace: a sweep now
			// would spend the whole budget answering empty and report a
			// misleading "completed, 0 coverage". Record the honest state and
			// skip; the repo stays un-enriched so a later cycle retries.
			m.logger.Info("semantic enrichment: workspace not ready; skipping sweep",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
				zap.String("repo", repoName),
			)
			m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateNotReady, 0, nil,
				"workspace did not finish loading within the readiness budget; sweep skipped, repo left for retry")
			return results
		}
		if err != nil {
			m.logger.Debug("semantic enrichment: readiness probe did not confirm; proceeding best-effort",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
				zap.String("repo", repoName),
				zap.Error(err),
			)
		}
	}

	_, isContextEnricher := provider.(ContextEnricher)
	// A ContextEnricher derives its real per-repo deadline lazily from the
	// post-filter candidate count (see EnrichDeadlinePolicy) — the Manager only
	// holds a generous outer ceiling so a wedged pass can't pin the WaitGroup.
	// Legacy providers, which never select candidates, keep the eager whole-repo
	// scaled deadline. d is updated to the provider's lazy value (once known)
	// before the terminal status is recorded.
	var d time.Duration
	if isContextEnricher {
		if _, eager := provider.(PreselectionDeadlineEnricher); eager {
			d = enrichRepoTimeout(nodeCount)
		} else {
			d = enrichOuterCeiling()
		}
	} else {
		d = enrichRepoTimeout(nodeCount)
	}
	m.logger.Info("semantic enrichment starting",
		zap.String("provider", provider.Name()),
		zap.String("language", lang),
		zap.String("repo", repoName),
		zap.Int("repo_nodes", nodeCount),
		zap.Duration("deadline", d),
	)
	m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateRunning, d, nil, "")

	// repoName is the roots-map key. In multi-repo mode it carries the
	// repo prefix (the MultiIndexer keys roots by prefix; the per-repo
	// indexer passes its own RepoPrefix()); a repo-scoped provider uses
	// it to scope file selection to the repo actually being enriched.
	var result *EnrichResult
	var err error
	type enrichOutcome struct {
		result *EnrichResult
		err    error
	}

	// The provider call runs in a Manager-owned goroutine so the deadline can
	// be observed without freezing daemon status/health requests. Crucially,
	// every path below receives from done before returning: a timed-out
	// in-process writer is never detached and the next provider/repo cannot
	// overlap it.
	baseCtx, stopLifecycle := m.passContext(context.Background())
	defer stopLifecycle()
	if ctxErr := baseCtx.Err(); ctxErr != nil {
		partial[repoName] = true
		m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateAbandoned, d, nil,
			"semantic manager closed before provider dispatch")
		return results
	}
	ctx := baseCtx
	cancel := func() {}
	if d > 0 {
		ctx, cancel = context.WithTimeout(baseCtx, d)
	}
	// Compiler-backed providers use the repository census only to choose an
	// admission class. Keeping the hint on this pass-owned context avoids
	// shared mutable scheduling state on Manager or Provider.
	ctx = WithEnrichmentAdmissionNodes(ctx, nodeCount)
	if applyGate != nil {
		ctx = WithApplyGate(ctx, applyGate)
	}
	defer cancel()

	ce, isContextEnricher := provider.(ContextEnricher)
	done := make(chan enrichOutcome, 1)
	go func() {
		var r *EnrichResult
		var e error
		if isContextEnricher {
			// enrichRepoTimeout is the lazy deadline policy: the provider calls
			// it with its post-filter candidate count to size its own context
			// bound inside the generous outer ceiling already on ctx.
			r, e = ce.EnrichRepoContext(ctx, g, repoName, repoRoot, enrichRepoTimeout)
		} else if rsp, ok := provider.(RepoScopedProvider); ok {
			r, e = rsp.EnrichRepo(g, repoName, repoRoot)
		} else {
			r, e = provider.Enrich(g, repoRoot)
		}
		done <- enrichOutcome{result: r, err: e}
	}()

	abandoned := false
	abandonDetail := ""
	if isContextEnricher {
		select {
		case oc := <-done:
			result, err = oc.result, oc.err
		case <-ctx.Done():
			// A cooperative provider gets a grace window to flush completed
			// work and report a counted Partial result. Past that window the
			// health state changes to draining, but we still wait for the call
			// to end before returning or releasing the active-pass gate.
			grace := time.NewTimer(enrichCancelGrace)
			select {
			case oc := <-done:
				if !grace.Stop() {
					select {
					case <-grace.C:
					default:
					}
				}
				result, err = oc.result, oc.err
			case <-grace.C:
				abandoned = true
				abandonDetail = "provider ignored cancellation past the grace window; waiting for its in-process graph writer to stop before advancing"
				m.logger.Warn("semantic enrichment ignored cancellation; draining provider before advancing",
					zap.String("provider", provider.Name()),
					zap.String("language", lang),
					zap.String("repo", repoName),
					zap.Duration("deadline", d),
					zap.Duration("grace", enrichCancelGrace),
				)
				m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateDraining, d, nil, abandonDetail)
				partial[repoName] = true
				<-done // mandatory drain: never abandon an in-process writer
			}
		}
	} else {
		select {
		case oc := <-done:
			result, err = oc.result, oc.err
		case <-ctx.Done():
			abandoned = true
			abandonDetail = "legacy provider exceeded its deadline; waiting for its in-process graph writer to stop before advancing"
			if errors.Is(ctx.Err(), context.Canceled) {
				abandonDetail = "semantic manager is closing; waiting for the legacy in-process graph writer to stop"
			}
			m.logger.Warn("semantic enrichment deadline reached; draining legacy provider before advancing",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
				zap.String("repo", repoName),
				zap.Duration("deadline", d),
			)
			m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateDraining, d, nil, abandonDetail)
			partial[repoName] = true
			<-done // mandatory drain: the legacy interface cannot be cancelled
		}
	}
	if abandoned {
		m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateAbandoned, d, nil,
			abandonDetail+"; provider stopped and its terminal result was discarded")
		return results
	}
	// Surface the deadline the ContextEnricher actually derived from its
	// candidate count (lazy budgeting) rather than the outer ceiling. 0 means
	// the pass ran unbounded or was a legacy provider; keep d as computed.
	if result != nil && result.BudgetSeconds > 0 {
		d = time.Duration(result.BudgetSeconds * float64(time.Second))
	}
	if err != nil {
		m.logger.Warn("semantic enrichment failed",
			zap.String("provider", provider.Name()),
			zap.String("language", lang),
			zap.Error(err),
		)
		m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateFailed, d, result, err.Error())
		partial[repoName] = true
		return results
	}

	if result != nil {
		result.DurationMs = time.Since(start).Milliseconds()
		results = append(results, result)

		m.mu.Lock()
		m.lastResults[provider.Name()] = result
		m.mu.Unlock()

		state := EnrichStateCompleted
		if result.Partial {
			state = EnrichStatePartial
			partial[repoName] = true
		}
		// A degraded (compile-db-missing) pass completes normally; surface its
		// reason as the status detail when there is no abort reason to report.
		detail := result.AbortReason
		if detail == "" {
			detail = result.DegradedReason
		}
		m.setEnrichStatus(repoName, provider.Name(), lang, state, d, result, detail)

		// Persist the completion marker only for a clean, non-partial pass so a
		// later restart can skip it while the repo sits at the same sha. A
		// partial / cut pass writes NOTHING — its marker must not claim the
		// repo is fully enriched. recordEnrichMarker is a no-op when the caller
		// supplied no sha or the backend does not persist enrichment state.
		if !result.Partial {
			m.recordEnrichMarker(g, repoName, provider.Name(), rs, result.CoveragePercent)
		}

		m.logger.Info("semantic enrichment complete",
			zap.String("provider", provider.Name()),
			zap.String("language", lang),
			zap.Bool("partial", result.Partial),
			zap.Int("confirmed", result.EdgesConfirmed),
			zap.Int("added", result.EdgesAdded),
			zap.Int("refuted", result.EdgesRefuted),
			zap.Int("rebound", result.EdgesRebound),
			zap.Int("nodes_enriched", result.NodesEnriched),
			zap.Float64("coverage", result.CoveragePercent),
			zap.Int64("duration_ms", result.DurationMs),
			zap.Int64("lock_wait_ms", result.LockWaitMs),
		)
	} else {
		m.setEnrichStatus(repoName, provider.Name(), lang, EnrichStateCompleted, d, nil, "")
	}
	return results
}

// EnrichFile runs one explicitly-requested, file-bounded enrichment pass.
// Watcher callers enforce EnrichOnWatch before dispatch; deferred incremental
// work calls the batched sibling so a scoped reindex never falls back to N
// heavyweight provider loads.
func (m *Manager) EnrichFile(g graph.Store, repoRoot, filePath string) (*EnrichResult, error) {
	if !m.config.Enabled {
		return nil, nil
	}

	// One exact file lookup determines both language and repository scope. The
	// batched entry point receives these explicitly and performs no per-file
	// graph discovery.
	nodes := g.GetFileNodes(filePath)
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, nil
	}
	return m.EnrichFiles(g, nodes[0].RepoPrefix, repoRoot, nodes[0].Language, []string{filePath})
}

// EnrichFiles runs one provider call for a deduplicated, single-language file
// frontier. Batch-capable providers receive every path together. A provider
// without that capability gets one repository call for multi-file work, never
// an N+1 loop of EnrichFile calls.
func (m *Manager) EnrichFiles(g graph.Store, repoPrefix, repoRoot, language string, filePaths []string) (*EnrichResult, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if m.config.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(m.config.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	return m.EnrichFilesContext(ctx, g, repoPrefix, repoRoot, language, filePaths)
}

// EnrichFilesContext is EnrichFiles with an explicit parent context. The
// manager-level semantic timeout remains the default through EnrichFiles;
// callers that already own a tighter deadline can pass it here without it
// being replaced by a Background root.
func (m *Manager) EnrichFilesContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot, language string, filePaths []string) (*EnrichResult, error) {
	if !m.config.Enabled || language == "" || len(filePaths) == 0 {
		return nil, nil
	}
	if !m.beginPass() {
		return nil, errManagerClosed
	}
	defer m.endPass()
	ctx, stopLifecycle := m.passContext(ctx)
	defer stopLifecycle()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(filePaths))
	files := make([]string, 0, len(filePaths))
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if _, ok := seen[filePath]; ok {
			continue
		}
		seen[filePath] = struct{}{}
		files = append(files, filePath)
	}
	if len(files) == 0 {
		return nil, nil
	}
	sort.Strings(files)

	run := func(provider Provider) (*EnrichResult, error) {
		if batch, ok := provider.(ContextFileBatchEnricher); ok {
			return batch.EnrichFilesContext(ctx, g, repoPrefix, repoRoot, files)
		}
		if batch, ok := provider.(FileBatchEnricher); ok {
			return batch.EnrichFiles(g, repoPrefix, repoRoot, files)
		}
		if len(files) == 1 {
			return provider.EnrichFile(g, repoRoot, files[0])
		}
		if contextual, ok := provider.(ContextEnricher); ok {
			return contextual.EnrichRepoContext(ctx, g, repoPrefix, repoRoot, nil)
		}
		if scoped, ok := provider.(RepoScopedProvider); ok {
			return scoped.EnrichRepo(g, repoPrefix, repoRoot)
		}
		return provider.Enrich(g, repoRoot)
	}

	langProviders := m.selectProviders()
	var primary *EnrichResult
	var primaryErr error
	if provider, ok := langProviders[language]; ok && provider.Available() {
		primary, primaryErr = run(provider)
	}

	// Supplemental providers preserve EnrichAll parity. Each receives one
	// batched call when supported, otherwise one repo call for a multi-file
	// frontier; no provider is invoked once per file.
	for _, provider := range m.providers {
		if !isSupplemental(provider) || !provider.Available() || m.providerDisabled(provider.Name()) {
			continue
		}
		handlesLanguage := false
		for _, candidate := range provider.Languages() {
			if candidate == language {
				handlesLanguage = true
				break
			}
		}
		if !handlesLanguage {
			continue
		}
		result, err := run(provider)
		if err != nil {
			m.logger.Debug("supplemental incremental enrichment failed",
				zap.String("provider", provider.Name()),
				zap.Int("files", len(files)),
				zap.Error(err),
			)
			continue
		}
		if primary == nil {
			primary = result
		}
	}

	return primary, primaryErr
}

// selectProviders returns the highest-priority available provider per language.
func (m *Manager) selectProviders() map[string]Provider {
	// Build priority map from config.
	type configEntry struct {
		name     string
		priority int
		enabled  bool
	}
	configMap := make(map[string]configEntry)
	for _, pc := range m.config.Providers {
		configMap[pc.Name] = configEntry{
			name:     pc.Name,
			priority: pc.Priority,
			enabled:  pc.Enabled,
		}
	}

	// Group providers by language with priority.
	type langCandidate struct {
		provider Provider
		priority int
	}
	langCandidates := make(map[string][]langCandidate)

	for _, p := range m.providers {
		// Supplemental providers never occupy a language slot — they
		// run unconditionally after arbitration (see EnrichAll), so a
		// router-backed LSP spec can still win the language.
		if isSupplemental(p) {
			continue
		}
		ce, ok := configMap[p.Name()]
		if ok && !ce.enabled {
			continue
		}
		priority := 99
		if ok {
			priority = ce.priority
		}
		for _, lang := range p.Languages() {
			langCandidates[lang] = append(langCandidates[lang], langCandidate{
				provider: p,
				priority: priority,
			})
		}
	}

	// Select highest-priority (lowest number) per language.
	result := make(map[string]Provider)
	for lang, candidates := range langCandidates {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].priority < candidates[j].priority
		})
		result[lang] = candidates[0].provider
	}

	return result
}

// Stats returns the current status of all providers — eager
// (RegisterProvider) plus router-enabled LSP specs. Router specs are
// reported as "lsp-<spec-name>" for discoverability; status is "ready"
// when SpecAvailable is true and "unavailable" otherwise. No
// subprocess is spawned by Stats — pure read.
func (m *Manager) Stats() []ProviderStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	var statuses []ProviderStatus
	for _, p := range m.providers {
		seen[p.Name()] = true
		for _, lang := range p.Languages() {
			status := "unavailable"
			if p.Available() {
				status = "ready"
			}

			ps := ProviderStatus{
				Name:     p.Name(),
				Language: lang,
				Status:   status,
			}

			if lr, ok := m.lastResults[p.Name()]; ok {
				ps.CoveragePercent = lr.CoveragePercent
				ps.LastResult = lr
			}

			statuses = append(statuses, ps)
		}
	}
	if m.lspRouter != nil {
		for _, name := range m.lspRouter.EnabledSpecNames() {
			provName := "lsp-" + name
			// Skip when the eager path already registered an
			// identically-named provider (avoids double-counting
			// in legacy boot configurations).
			if seen[provName] {
				continue
			}
			status := "unavailable"
			if m.lspRouter.SpecAvailable(name) {
				status = "ready"
			}
			for _, lang := range m.lspRouter.SpecLanguages(name) {
				ps := ProviderStatus{
					Name:     provName,
					Language: lang,
					Status:   status,
				}
				if lr, ok := m.lastResults[provName]; ok {
					ps.CoveragePercent = lr.CoveragePercent
					ps.LastResult = lr
				}
				statuses = append(statuses, ps)
			}
		}
	}
	return statuses
}

// Close shuts down all providers, including any LSP subprocesses
// owned by the installed LSPRouter. It first cancels every context-aware pass,
// then closes provider resources to interrupt subprocess-backed legacy calls,
// and finally waits for every admitted pass to stop mutating before returning.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.lifecycleMu.Lock()
		m.closing = true
		if m.lifecycleCancel != nil {
			m.lifecycleCancel()
		}
		providers := append([]Provider(nil), m.providers...)
		router := m.lspRouter
		m.lifecycleMu.Unlock()

		var errs []error
		for _, p := range providers {
			if err := p.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing %s: %w", p.Name(), err))
			}
		}
		if router != nil {
			if err := router.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing lsp router: %w", err))
			}
		}

		// No pass can be admitted after closing was set under lifecycleMu.
		// Every admitted run drains its provider goroutine before endPass.
		m.activePasses.Wait()
		if len(errs) > 0 {
			m.closeErr = fmt.Errorf("semantic manager close errors: %v", errs)
		}
	})
	return m.closeErr
}

// Enabled returns whether semantic enrichment is enabled.
func (m *Manager) Enabled() bool {
	return m.config.Enabled
}

// HasProviders returns whether any providers are registered and available.
// Includes Router-enabled LSP specs — Router providers are spawned
// lazily but their availability is decided by exec.LookPath, so
// Router.EnabledSpecNames() seen-and-resolvable counts as "have one".
func (m *Manager) HasProviders() bool {
	for _, p := range m.providers {
		if p.Available() {
			return true
		}
	}
	if m.lspRouter != nil {
		for _, name := range m.lspRouter.EnabledSpecNames() {
			if m.lspRouter.SpecAvailable(name) {
				return true
			}
		}
	}
	return false
}

// EnrichesOnWatch reports whether a single-file watch save re-runs semantic
// enrichment for the saved file (EnrichFile is a no-op otherwise). When this
// is false but providers exist, a live re-parse leaves the file's edges at
// their pre-enrichment tier until the next full enrichment — the window the
// indexer records so find_usages can flag suppressed usages as re-verification
// pending.
func (m *Manager) EnrichesOnWatch() bool {
	return m.config.Enabled && m.config.EnrichOnWatch
}

// AllProviders returns the unfiltered list of registered providers.
// Used by the daemon's LSP-action surface to find the right LSP
// provider for a file (call sites need the *lsp.Provider concrete
// type, so this stays untyped here and the caller does the type
// assertion against the lsp package).
func (m *Manager) AllProviders() []Provider {
	out := make([]Provider, len(m.providers))
	copy(out, m.providers)
	return out
}

// ProviderForLanguage returns the highest-priority registered provider
// for the given language code, or nil. The returned provider is the
// same one selectProviders would dispatch Enrich to.
func (m *Manager) ProviderForLanguage(lang string) Provider {
	if !m.config.Enabled {
		return nil
	}
	candidates := m.selectProviders()
	return candidates[lang]
}
