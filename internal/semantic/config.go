package semantic

import "sync"

// Config holds configuration for the semantic enrichment layer.
type Config struct {
	Enabled           bool             `mapstructure:"enabled" yaml:"enabled"`
	TimeoutSeconds    int              `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
	EnrichOnWatch     bool             `mapstructure:"enrich_on_watch" yaml:"enrich_on_watch,omitempty"`
	WatchDebounceMs   int              `mapstructure:"watch_debounce_ms" yaml:"watch_debounce_ms,omitempty"`
	RefuteUnconfirmed bool             `mapstructure:"refute_unconfirmed" yaml:"refute_unconfirmed,omitempty"`
	Providers         []ProviderConfig `mapstructure:"providers" yaml:"providers,omitempty"`
	// ExcludeGlobs lists path globs to skip for semantic enrichment, in
	// addition to the built-in generated/vendored heuristic. A file matching
	// any glob is not enriched and does not count toward a repo's present
	// languages (so a language server is not spawned for a repo whose only
	// files of that language are excluded).
	ExcludeGlobs []string `mapstructure:"exclude_globs" yaml:"exclude_globs,omitempty"`
	// LSPSweep mirrors config.SemanticConfig.LSPSweep — the per-file LSP
	// enrichment sweep mode ("demand" default / "full" / "off"). Threaded to
	// each spawned LSP provider via the router's WithEnrichSweepMode. The
	// GORTEX_LSP_SWEEP env override wins over it at enrichment time.
	LSPSweep string `mapstructure:"lsp_sweep" yaml:"lsp_sweep,omitempty"`
	// LSPOpenDocs mirrors config.SemanticConfig.LSPOpenDocs — the
	// didOpen-lifecycle override ("" spec-decides / "on" / "off"). Threaded
	// to each spawned LSP provider via the router's WithEnrichOpenDocs. The
	// GORTEX_LSP_OPEN_DOCS env override wins over it at enrichment time.
	LSPOpenDocs string `mapstructure:"lsp_open_docs" yaml:"lsp_open_docs,omitempty"`
	// LSPMaxParallel mirrors config.SemanticConfig.LSPMaxParallel — the
	// concurrent-request cap for spawned LSP servers. Zero keeps each
	// spec's own default; GORTEX_LSP_MAX_PARALLEL wins over both.
	LSPMaxParallel int `mapstructure:"lsp_max_parallel" yaml:"lsp_max_parallel,omitempty"`
	// EagerLSP runs the subprocess LSP servers during the synchronous
	// enrichment pass. Default false: LSP is the slowest part of a cold index
	// (a full gopls/tsserver/rust-analyzer/pyright sweep can run for minutes to
	// hours) and its net-new value over the in-process tiers is narrow — a Go
	// module is served by go-types, and every language has the tree-sitter
	// floor. With this off, cold/warm start pays only the fast in-process
	// providers; the LSP router stays available so a query can still lazy-spawn
	// a server on demand. Set true (or GORTEX_LSP_EAGER=1) to restore the
	// pre-change eager behaviour.
	EagerLSP bool `mapstructure:"eager_lsp" yaml:"eager_lsp,omitempty"`
}

// ProviderConfig holds configuration for a single semantic provider.
type ProviderConfig struct {
	Name        string   `mapstructure:"name" yaml:"name"`
	Command     string   `mapstructure:"command" yaml:"command,omitempty"`
	Args        []string `mapstructure:"args" yaml:"args,omitempty"`
	Languages   []string `mapstructure:"languages" yaml:"languages"`
	Priority    int      `mapstructure:"priority" yaml:"priority,omitempty"`
	Enabled     bool     `mapstructure:"enabled" yaml:"enabled"`
	Mode        string   `mapstructure:"mode" yaml:"mode,omitempty"` // "typecheck" or "callgraph" for go-types
	Daemon      bool     `mapstructure:"daemon" yaml:"daemon,omitempty"`
	MaxParallel int      `mapstructure:"max_parallel" yaml:"max_parallel,omitempty"`
	// Env adds KEY=VALUE environment entries to the provider's LSP
	// subprocess (e.g. JAVA_HOME for jdtls).
	Env []string `mapstructure:"env" yaml:"env,omitempty"`
	// Connect, when non-nil, switches this provider from spawning a
	// subprocess to dialing an already-running LSP endpoint (e.g.
	// the user's IDE-managed gopls). Carrier is tcp or unix.
	Connect *ConnectConfig `mapstructure:"connect" yaml:"connect,omitempty"`
}

// ConnectConfig is the per-provider passive-attach block used by the
// semantic-layer Config struct. The cmd/gortex/* boot code copies it
// onto the matching lsp.ConnectSpec at router-registration time. Kept
// here rather than in lsp/* to avoid a circular import (lsp depends
// on semantic for EnrichResult).
type ConnectConfig struct {
	Network       string `mapstructure:"network" yaml:"network"`
	Address       string `mapstructure:"address" yaml:"address"`
	FallbackSpawn bool   `mapstructure:"fallback_spawn" yaml:"fallback_spawn,omitempty"`
}

// DefaultConfig returns a default semantic config with auto-detection enabled.
//
// The order matters: per-language priority sorts ascending, so go-types
// (priority 1) wins for Go even when scip-go and gopls are also
// available. Every known LSP server is enumerated via
// RegisterDefaultProviders so that, when its binary is on PATH, the
// daemon spins it up automatically without users editing
// `.gortex.yaml`.
func DefaultConfig() Config {
	cfg := Config{
		Enabled:           true,
		TimeoutSeconds:    120,
		EnrichOnWatch:     false,
		WatchDebounceMs:   500,
		RefuteUnconfirmed: false,
		Providers: []ProviderConfig{
			{
				Name:      "go-types",
				Languages: []string{"go"},
				Priority:  1,
				Enabled:   true,
				Mode:      "typecheck",
			},
			{
				Name:      "scip-go",
				Command:   "scip-go",
				Languages: []string{"go"},
				Priority:  2,
				Enabled:   true,
			},
			{
				Name:      "scip-typescript",
				Command:   "scip-typescript",
				Args:      []string{"index", "--infer-tsconfig"},
				Languages: []string{"typescript", "javascript"},
				Priority:  1,
				Enabled:   true,
			},
			{
				Name:      "scip-python",
				Command:   "scip-python",
				Languages: []string{"python"},
				Priority:  1,
				Enabled:   true,
			},
		},
	}
	cfg.Providers = append(cfg.Providers, defaultLSPProviders()...)
	return cfg
}

// defaultLSPProviders returns LSP-flavored ProviderConfig entries
// contributed by sub-packages via RegisterDefaultProviders. The
// `internal/semantic/lsp` package registers its `Servers` list at
// init time. The indirection avoids a circular import — lsp depends
// on semantic for EnrichResult etc.
func defaultLSPProviders() []ProviderConfig {
	defaultRegMu.RLock()
	defer defaultRegMu.RUnlock()
	out := make([]ProviderConfig, 0)
	for _, fn := range defaultRegistrations {
		out = append(out, fn()...)
	}
	return out
}

// RegisterDefaultProviders lets sub-packages contribute provider entries
// to DefaultConfig. Each registered function is called when DefaultConfig
// is invoked. Registration order is preserved.
func RegisterDefaultProviders(fn func() []ProviderConfig) {
	if fn == nil {
		return
	}
	defaultRegMu.Lock()
	defaultRegistrations = append(defaultRegistrations, fn)
	defaultRegMu.Unlock()
}

var (
	defaultRegMu         sync.RWMutex
	defaultRegistrations []func() []ProviderConfig
)
