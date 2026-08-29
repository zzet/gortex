package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/excludes"
)

// ConfigManager merges GlobalConfig + per-repo WorkspaceConfig.
// It loads the GlobalConfig once at construction and caches workspace
// configs (per-repo .gortex.yaml) on demand with a sync.RWMutex.
type ConfigManager struct {
	global    *GlobalConfig
	workspace map[string]*Config // repoPrefix → workspace config
	// workspacePaths tracks the absolute filesystem root for each
	// repoPrefix. Needed by EffectiveExclude to locate the repo's own
	// `.gitignore` file. Populated by LoadWorkspaceConfig regardless of
	// whether `.gortex.yaml` exists, so a repo without workspace config
	// still gets gitignore-respecting behaviour.
	workspacePaths map[string]string
	mu             sync.RWMutex
	// revision is a monotonically increasing epoch for immutable views built
	// from global and per-repo config. Readers can cache those views and pay
	// only an atomic load until configuration state actually changes.
	revision atomic.Uint64
	logger   *zap.Logger
	// excludeCache memoizes the per-repo `.gitignore` parse and the layered
	// exclude list so EffectiveExclude — called on every indexer walk and
	// per-file reconcile — does not re-read and re-merge on every call.
	excludeCache *excludeCache
}

// NewConfigManager creates a ConfigManager by loading the GlobalConfig
// from the given path. If globalPath is empty, the default path is used.
// A missing GlobalConfig file is not an error (returns empty config).
func NewConfigManager(globalPath string) (*ConfigManager, error) {
	var gc *GlobalConfig
	var err error
	if globalPath != "" {
		gc, err = LoadGlobal(globalPath)
	} else {
		gc, err = LoadGlobal()
	}
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	return &ConfigManager{
		global:         gc,
		workspace:      make(map[string]*Config),
		workspacePaths: make(map[string]string),
		logger:         zap.NewNop(),
		excludeCache:   newExcludeCache(),
	}, nil
}

// SetLogger sets the logger for the ConfigManager.
func (cm *ConfigManager) SetLogger(logger *zap.Logger) {
	if logger != nil {
		cm.logger = logger
	}
}

// Global returns the underlying GlobalConfig.
func (cm *ConfigManager) Global() *GlobalConfig {
	cm.mu.RLock()
	global := cm.global
	cm.mu.RUnlock()
	return global
}

// RepoEntries returns a lock-protected snapshot of every durable explicit
// repository entry. Top-level repositories take precedence over duplicate
// project entries; projects are traversed by name so repeated snapshots are
// deterministic even though Projects is map-backed.
func (cm *ConfigManager) RepoEntries() []RepoEntry {
	if cm == nil {
		return nil
	}

	// Snapshot the active revision pointer without nesting cm.mu and
	// globalConfigMu. A concurrent reload may replace the pointer afterward,
	// but this selected revision remains internally consistent while copied.
	cm.mu.RLock()
	global := cm.global
	cm.mu.RUnlock()
	if global == nil {
		return nil
	}

	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()

	projectNames := make([]string, 0, len(global.Projects))
	for name := range global.Projects {
		projectNames = append(projectNames, name)
	}
	sort.Strings(projectNames)

	entries := make([]RepoEntry, 0, len(global.Repos))
	seenPaths := make(map[string]struct{}, len(global.Repos))
	appendEntry := func(entry RepoEntry) {
		identity := filepath.Clean(entry.Path)
		if absolute, err := filepath.Abs(identity); err == nil {
			identity = absolute
		}
		if _, exists := seenPaths[identity]; exists {
			return
		}
		seenPaths[identity] = struct{}{}
		entry.Exclude = append([]string(nil), entry.Exclude...)
		entries = append(entries, entry)
	}
	for _, entry := range global.Repos {
		appendEntry(entry)
	}
	for _, name := range projectNames {
		for _, entry := range global.Projects[name].Repos {
			appendEntry(entry)
		}
	}
	return entries
}

// Revision returns the current configuration epoch. It changes after every
// successful global reload and whenever LoadWorkspaceConfig changes cached
// per-repo state.
func (cm *ConfigManager) Revision() uint64 {
	if cm == nil {
		return 0
	}
	return cm.revision.Load()
}

// Reload re-reads the GlobalConfig from disk, keeping the same config
// path, AND re-reads every per-repo `.gortex.yaml` this manager has seen.
// Used by the daemon's `reload` control RPC to pick up manual edits to
// either file without a full process restart.
//
// The per-repo re-read is not optional bookkeeping. This used to drop the
// workspace caches and wait for a "lazy" re-read, but the only writer of
// those caches is LoadWorkspaceConfig, which runs at track / index time
// and never on this path — so the lazy re-read never happened. Worse,
// workspacePaths is what EffectiveExclude uses to locate a repo's own
// `.gitignore`, so dropping it left every tracked repo running with no
// workspace config AND no gitignore layer (builtin-only admission) until
// the process restarted: strictly worse than the stale state the drop was
// meant to avoid.
//
// Every file is read before anything is published, so a concurrent
// indexer walk never observes a config-less window.
func (cm *ConfigManager) Reload() error {
	cm.mu.Lock()
	path := cm.global.ConfigPath()
	known := make(map[string]string, len(cm.workspacePaths))
	for prefix, repoPath := range cm.workspacePaths {
		known[prefix] = repoPath
	}
	cm.mu.Unlock()

	var fresh *GlobalConfig
	var err error
	if path != "" {
		fresh, err = LoadGlobal(path)
	} else {
		fresh, err = LoadGlobal()
	}
	if err != nil {
		return fmt.Errorf("reload global config: %w", err)
	}

	reread := make(map[string]*Config, len(known))
	var dropped []string
	for prefix, repoPath := range known {
		cfg, authoritative := cm.readWorkspaceConfig(prefix, repoPath)
		switch {
		case !authoritative:
			// Present but unreadable / malformed — keep the last good
			// parse rather than silently downgrading the repo to global
			// defaults on a transient I/O error or a half-saved edit.
		case cfg == nil:
			// The `.gortex.yaml` is gone; so are its overrides.
			dropped = append(dropped, prefix)
		default:
			reread[prefix] = cfg
		}
	}

	// Apply per prefix rather than swapping the whole map: a repo tracked
	// concurrently with this reload must not be erased by a map snapshot
	// taken before it existed.
	cm.mu.Lock()
	cm.global = fresh
	for prefix, cfg := range reread {
		cm.workspace[prefix] = cfg
	}
	for _, prefix := range dropped {
		delete(cm.workspace, prefix)
	}
	cm.revision.Add(1)
	cm.mu.Unlock()
	return nil
}

// LoadWorkspaceConfig loads a .gortex.yaml from the given repo root
// and caches it under the given repoPrefix. If the file is missing, any
// entry cached under that prefix is dropped (global defaults will
// apply). If the file exists but is unreadable or malformed, a warning
// is logged and the last good parse is kept.
func (cm *ConfigManager) LoadWorkspaceConfig(repoPrefix, repoPath string) {
	cfg, authoritative := cm.readWorkspaceConfig(repoPrefix, repoPath)
	cm.publishWorkspaceConfig(repoPrefix, repoPath, cfg, authoritative)
}

// readWorkspaceConfig reads and parses a repo's `.gortex.yaml`. The second
// return reports whether the result reflects what is on disk: it is false
// only when the file exists but could not be read or parsed, in which case
// the caller keeps whatever it had cached. A (nil, true) result means the
// file genuinely does not exist.
func (cm *ConfigManager) readWorkspaceConfig(repoPrefix, repoPath string) (*Config, bool) {
	configPath := filepath.Join(repoPath, ".gortex.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No workspace config — global defaults will apply.
			return nil, true
		}
		cm.logger.Warn("failed to read workspace config",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return nil, false
	}

	// Seed with Default() and unmarshal the file OVER it — the same
	// overlay semantics config.Load() applies. A zero-value seed turned
	// the file's mere presence into a wholesale replacement: every field
	// a partial .gortex.yaml didn't mention lost its documented default
	// (unset index.workers → parse pool of 1, unset
	// max_parse_bytes_in_flight → admission semaphore disabled).
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		// Malformed workspace config — log warning, keep the last good parse.
		cm.logger.Warn("malformed workspace config, keeping the last good parse",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return nil, false
	}

	return cfg, true
}

// publishWorkspaceConfig publishes a per-repo state transition and advances
// revision only when the cached state actually changes. authoritative says
// whether cfg reflects what is on disk: a failed read/parse updates only the
// remembered path and leaves the cached config alone, while an authoritative
// nil (no `.gortex.yaml` on disk) drops it — deleting the file must stop its
// overrides applying, without waiting for a daemon restart.
//
// The remembered path is never cleared: EffectiveExclude needs it to locate
// the repo's own `.gitignore`, which is independent of `.gortex.yaml`.
func (cm *ConfigManager) publishWorkspaceConfig(repoPrefix, repoPath string, cfg *Config, authoritative bool) {
	// Publishing a path is the one moment we know a repo was just tracked,
	// reindexed, or refreshed — re-discover where its git root is rather
	// than trusting a shape memoized before the repo existed in this form.
	cm.excludeCache.forgetChain(repoPath)

	cm.mu.Lock()
	changed := false
	if repoPath != "" && cm.workspacePaths[repoPrefix] != repoPath {
		cm.workspacePaths[repoPrefix] = repoPath
		changed = true
	}
	switch {
	case !authoritative:
		// Keep the last good parse.
	case cfg == nil:
		if _, ok := cm.workspace[repoPrefix]; ok {
			delete(cm.workspace, repoPrefix)
			changed = true
		}
	case !reflect.DeepEqual(cm.workspace[repoPrefix], cfg):
		cm.workspace[repoPrefix] = cfg
		changed = true
	}
	if changed {
		cm.revision.Add(1)
	}
	cm.mu.Unlock()
}

// WorkspacePrefixes returns the repo prefixes this manager has loaded a
// workspace config for — i.e. every repo that has been tracked or indexed
// in this process, whether or not it has a `.gortex.yaml`. Callers use it
// to tell a real repo prefix from the leading path segment of an
// unprefixed node ID.
func (cm *ConfigManager) WorkspacePrefixes() []string {
	if cm == nil {
		return nil
	}
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	out := make([]string, 0, len(cm.workspacePaths))
	for prefix := range cm.workspacePaths {
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

// getWorkspaceConfig returns the cached workspace config for a repo, or nil.
func (cm *ConfigManager) getWorkspaceConfig(repoPrefix string) *Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.workspace[repoPrefix]
}

// GetRepoConfig returns the merged config for a repository. The returned
// Config has Index.Exclude and Watch.Exclude populated with the full
// layered exclude list from EffectiveExclude, so callers passing
// cfg.Index into indexer.New automatically receive the effective patterns.
func (cm *ConfigManager) GetRepoConfig(repoPrefix string) *Config {
	var out *Config
	if ws := cm.getWorkspaceConfig(repoPrefix); ws != nil {
		dup := *ws
		out = &dup
	} else {
		out = Default()
	}
	effective := cm.EffectiveExclude(repoPrefix)
	out.Exclude = effective
	out.Index.Exclude = effective
	out.Watch.Exclude = effective
	// Plumb semantic.skip_embed through to the indexer's config so the
	// embedder can filter without a new setter. Workspace > compiled
	// defaults.
	if len(out.Semantic.SkipEmbed) > 0 {
		out.Index.SkipEmbed = out.Semantic.SkipEmbed
	} else {
		out.Index.SkipEmbed = DefaultSkipEmbed()
	}
	// Same plumbing for semantic.skip_search — controls what goes into
	// the store-native text search index. Separate from SkipEmbed so
	// users can tune the two filters independently (e.g. a tiny-repo
	// user who doesn't care about text-index size can clear SkipSearch
	// while keeping SkipEmbed's embedding-cost savings).
	if len(out.Semantic.SkipSearch) > 0 {
		out.Index.SkipSearch = out.Semantic.SkipSearch
	} else {
		out.Index.SkipSearch = DefaultSkipSearch()
	}
	// Prose indexing toggle -- propagated from search.index_prose so
	// the indexer (which only sees IndexConfig) can honour it.
	// Defaults to enabled when the key is unset.
	out.Index.IndexProse = out.Search.IndexProseEnabled()
	return out
}

// EffectiveExclude returns the effective ignore patterns for a repo,
// layered in precedence order (later layers can re-include via !pattern):
//
//  1. Builtin baseline (excludes.Builtin)
//  2. Every `.gitignore` from the enclosing git root down to the tracked
//     root, outermost first, with ancestor patterns re-anchored relative to
//     the tracked root (read from disk; opt out with
//     `respect_gitignore: false` in `.gortex.yaml`)
//  3. Global Exclude from ~/.gortex/config.yaml
//  4. Matching RepoEntry.Exclude (first match in Repos, then Projects)
//  5. Workspace .gortex.yaml top-level Exclude
//  6. Legacy workspace Index.Exclude / Watch.Exclude (deprecated)
//
// This runs on a firehose of calls (every indexer walk and per-file
// reconcile), so the layered result is memoized per repo and returned
// shared: a steady-state call does one os.Stat per `.gitignore` in the
// chain and returns the cached slice without re-reading or re-merging. The
// cache invalidates on config changes (the global and workspace configs are
// swapped, never mutated in place, so pointer identity is the version) and
// on any chain member's mtime/size change.
//
// The returned slice is SHARED and IMMUTABLE: callers MUST NOT mutate its
// elements. It is clipped (len == cap), so appending to it is safe —
// append reallocates rather than writing through the shared backing array.
func (cm *ConfigManager) EffectiveExclude(repoPrefix string) []string {
	cm.mu.RLock()
	gc := cm.global
	ws := cm.workspace[repoPrefix]
	repoPath := cm.workspacePaths[repoPrefix]
	cm.mu.RUnlock()

	respect := shouldRespectGitignore(ws)
	var chain []gitignoreLayer
	if respect && repoPath != "" {
		chain = cm.excludeCache.chain(repoPath)
	}
	if m, ok := cm.excludeCache.lookupMerged(repoPrefix, gc, ws, repoPath, respect, chain); ok {
		return m
	}

	out := make([]string, 0, 32)
	out = append(out, excludes.Builtin...)

	// Layer 2: the repo's `.gitignore` chain, unless the workspace config
	// explicitly opts out. Layers arrive outermost first so a deeper file
	// overrides a shallower one, as git does. Each parse is cached per
	// directory and refreshed only when that file's mtime/size changes (see
	// excludeCache), so a mid-session edit is still picked up on the next
	// call.
	for _, layer := range chain {
		out = append(out, reanchorGitignore(cm.excludeCache.patterns(layer.dir, layer.stat), layer.sub)...)
	}

	if gc != nil {
		out = append(out, gc.Exclude...)
		if entry := gc.FindRepoByPrefix(repoPrefix); entry != nil {
			out = append(out, entry.Exclude...)
		}
	}
	if ws != nil {
		out = append(out, ws.Exclude...)
		// Legacy fallback: older configs put patterns under index.exclude
		// or watch.exclude. Fold them in so nothing silently breaks.
		if len(ws.Exclude) == 0 {
			out = append(out, ws.Index.Exclude...)
			out = append(out, ws.Watch.Exclude...)
		}
	}

	// Force-include last so it wins: each Include entry becomes a gitignore
	// `!pattern` re-include over every exclude layer above (builtin,
	// .gitignore, global, repo). This is the readable form of hand-writing
	// negations, for a vendored/generated tree you want indexed anyway.
	if ws != nil {
		for _, inc := range ws.Include {
			inc = strings.TrimSpace(inc)
			if inc == "" {
				continue
			}
			if !strings.HasPrefix(inc, "!") {
				inc = "!" + inc
			}
			out = append(out, inc)
		}
	}

	// Clip to len == cap so a caller that appends to the returned slice is
	// forced to reallocate and can never write through the shared backing
	// array the cache hands to every reader.
	out = out[:len(out):len(out)]
	cm.excludeCache.storeMerged(repoPrefix, gc, ws, repoPath, respect, chain, out)
	return out
}

// shouldRespectGitignore returns true when the repo's `.gitignore`
// should be folded into the effective exclude list. Absence of a
// workspace config or absence of an explicit `respect_gitignore` setting
// both default to true; only an explicit `respect_gitignore: false`
// disables the layer.
func shouldRespectGitignore(ws *Config) bool {
	if ws == nil || ws.RespectGitignore == nil {
		return true
	}
	return *ws.RespectGitignore
}

// EffectiveGuardRules returns the effective guard rules for a repo.
// Workspace config wins when present; otherwise global defaults apply.
func (cm *ConfigManager) EffectiveGuardRules(repoPrefix string) []GuardRule {
	ws := cm.getWorkspaceConfig(repoPrefix)
	if ws != nil && len(ws.Guards.Rules) > 0 {
		return ws.Guards.Rules
	}
	return Default().Guards.Rules
}

// ActiveRepos returns the repos for the active project, or the top-level
// repos if no active project is set.
func (cm *ConfigManager) ActiveRepos() []RepoEntry {
	if cm.global.ActiveProject != "" {
		repos, err := cm.global.ResolveRepos(cm.global.ActiveProject)
		if err == nil {
			return repos
		}
		// If the active project is invalid, fall through to top-level repos.
		cm.logger.Warn("active project not found, falling back to top-level repos",
			zap.String("project", cm.global.ActiveProject),
			zap.Error(err))
	}
	return cm.global.Repos
}
