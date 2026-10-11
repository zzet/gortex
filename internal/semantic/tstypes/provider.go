package tstypes

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider is the semantic.Provider over one LangSpec. Pure in-process
// — Available is unconditionally true, no subprocess is ever spawned,
// Close is a no-op. It is supplemental: it augments whichever provider
// wins the per-language arbitration (LSP / SCIP) instead of competing
// with it, and only ever stamps AST-grade provenance, so a
// compiler-grade pass running before or after never gets downgraded.
type Provider struct {
	spec         *LangSpec
	logger       *zap.Logger
	observePage  func(factPageStats) // synchronous test/diagnostic hook
	observeSpool func(string)        // synchronous cleanup test hook
}

var _ semantic.ContextEnricher = (*Provider)(nil)

// NewProvider wraps a LangSpec as a semantic provider.
func NewProvider(spec *LangSpec, logger *zap.Logger) *Provider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Provider{spec: spec, logger: logger}
}

// DefaultProviders returns the in-process type resolvers for every
// supported language. Registered unconditionally at daemon boot —
// disable one via a `semantic.providers` config entry with
// `enabled: false` under its name.
func DefaultProviders(logger *zap.Logger) []*Provider {
	return []*Provider{
		NewProvider(JavaSpec(), logger),
		NewProvider(PythonSpec(), logger),
		NewProvider(RubySpec(), logger),
		NewProvider(RustSpec(), logger),
		NewProvider(TypeScriptSpec(), logger),
		NewProvider(CSharpSpec(), logger),
		NewProvider(PHPSpec(), logger),
		NewProvider(KotlinSpec(), logger),
		NewProvider(GoSpec(), logger),
	}
}

func (p *Provider) Name() string        { return p.spec.ProviderName }
func (p *Provider) Languages() []string { return p.spec.Languages }
func (p *Provider) Available() bool     { return true }
func (p *Provider) Close() error        { return nil }

// Supplemental marks this provider as augmenting (see
// semantic.SupplementalProvider): the manager runs it for its
// languages in addition to the arbitration winner.
func (p *Provider) Supplemental() bool { return true }

// A suppressed fallback does no parsing or graph work, so it needs no
// shared admission. Active AST passes keep ordinary build-lane admission.
func (p *Provider) ConcurrentCheckoutPreparation(ctx context.Context, _, _ string, _ semantic.CheckoutCompilerScope, _ []string) bool {
	return ctx.Err() == nil && p.spec.Suppressed != nil && p.spec.Suppressed()
}

// Enrich runs the full-repo pass for a single-repo (un-prefixed) graph.
// It delegates to EnrichRepo with an empty prefix — the in-memory single
// repo case where every real node carries RepoPrefix "".
func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, "", repoRoot, nil)
}

// EnrichRepo runs the full-repo pass: parse every file of the provider's
// languages that belong to repoPrefix under repoRoot in a bounded worker
// pool, then apply the per-file facts to the graph from a single
// goroutine. repoPrefix scopes file selection so a multi-repo graph with
// a colliding relative path never reads the wrong repo's bytes.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, repoPrefix, repoRoot, nil)
}

func (p *Provider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Toolchain-fallback gate: a spec that suppresses itself on this host
	// (its authoritative compiler-grade provider is available) contributes
	// nothing — no parse, no graph mutation — so it can never alter, duplicate
	// or downgrade that provider's resolutions.
	if p.spec.Suppressed != nil && p.spec.Suppressed() {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}

	files := languageFiles(g, p.spec, repoPrefix, repoRoot)
	// The budget is charged phase by phase against a movable deadline instead
	// of arming one context up front: the wait for the graph-wide resolve
	// mutex below is ANOTHER pass's work, and letting it burn this pass's
	// budget turned a queued small repo into a partial with zero coverage
	// (observed: a 641s "pass" that was ~630s of queueing behind a Go
	// compiler apply). A parent context's own deadline still caps every
	// phase — WithDeadline never extends the parent.
	var deadlineAt time.Time
	if deadline != nil {
		if d := deadline(len(files)); d > 0 {
			deadlineAt = start.Add(d)
			res.BudgetSeconds = d.Seconds()
		}
	}
	phaseCtx := func() (context.Context, context.CancelFunc) {
		if deadlineAt.IsZero() {
			return ctx, func() {}
		}
		return context.WithDeadline(ctx, deadlineAt)
	}
	if err := ctx.Err(); err != nil {
		markContextPartial(res, err)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	if len(files) > 0 {
		spool, err := newFactSpool()
		if err != nil {
			return nil, err
		}
		defer spool.close()
		if p.observeSpool != nil {
			p.observeSpool(spool.path)
		}
		parseCtx, cancelParse := phaseCtx()
		if err := p.stageRepoFacts(parseCtx, files, spool); err != nil {
			parseErr := parseCtx.Err()
			cancelParse()
			if parseErr != nil {
				markContextPartial(res, parseErr)
				res.DurationMs = time.Since(start).Milliseconds()
				return res, nil
			}
			return nil, err
		}
		cancelParse()
		// Parsing above is pure and fans out across workers; the apply
		// phase mutates the shared graph (retargets edges, reindexes,
		// stamps provenance) and MUST run under the graph-wide resolve
		// mutex so it serialises against concurrent resolver / cross-repo
		// passes — the same lock every other edge-mutating pass holds.
		mu := g.ResolveMutex()
		lockStart := time.Now()
		// Warmup apply barrier first (parks while the pool overlaps the
		// resolve phase), then the mutex. Both waits are other passes' work,
		// so both are excluded from this pass's budget below.
		if err := semantic.ApplyGateWait(ctx); err != nil {
			markContextPartial(res, err)
			res.DurationMs = time.Since(start).Milliseconds()
			return res, nil
		}
		mu.Lock()
		lockWait := time.Since(lockStart)
		res.LockWaitMs = lockWait.Milliseconds()
		if lockWait > 5*time.Second {
			// The completion log's lock_wait_ms explains a long pass only
			// after the fact; announce the wait as it resolves so a watcher
			// can attribute the quiet stretch in real time.
			p.logger.Info("tstypes: apply began after queueing",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Duration("queued", lockWait))
		}
		if !deadlineAt.IsZero() {
			deadlineAt = deadlineAt.Add(lockWait)
		}
		applyCtx, cancelApply := phaseCtx()
		if err := applyCtx.Err(); err != nil {
			mu.Unlock()
			cancelApply()
			markContextPartial(res, err)
			res.DurationMs = time.Since(start).Milliseconds()
			return res, nil
		}
		applyErr := p.applyStagedFacts(applyCtx, g, repoPrefix, spool, res)
		mu.Unlock()
		applyCtxErr := applyCtx.Err()
		cancelApply()
		if applyErr != nil {
			if applyCtxErr != nil {
				markContextPartial(res, applyCtxErr)
				res.DurationMs = time.Since(start).Milliseconds()
				return res, nil
			}
			return nil, applyErr
		}
	}
	if err := ctx.Err(); err != nil {
		markContextPartial(res, err)
	}

	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

func markContextPartial(res *semantic.EnrichResult, err error) {
	res.Partial = true
	res.AbortReason = err.Error()
	res.BoundReason = semantic.EnrichBoundBudget
}

// EnrichFile runs the single-file incremental pass. filePath is the graph
// file key (prefixed in multi-repo mode), which is globally unique — the
// file's own node names the repo, so this is inherently scoped to the
// right repo without a separate prefix argument.
func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}
	// Toolchain-fallback gate (see EnrichRepo): a suppressed spec is a no-op.
	if p.spec.Suppressed != nil && p.spec.Suppressed() {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	// Find the file's own node by its exact graph key. It carries the
	// RepoPrefix that maps the prefixed path back to the on-disk file.
	var fileNode *graph.Node
	for _, n := range g.GetFileNodes(filePath) {
		if n.Kind == graph.KindFile {
			fileNode = n
			break
		}
	}
	if fileNode == nil || !p.spec.handles(fileNode.Language) {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	ref, ok := fileRefFor(fileNode, repoRoot)
	if !ok {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	facts, err := analyzeFile(p.spec, ref)
	if err != nil {
		return nil, err
	}
	if facts != nil {
		// Same contract as the full pass: the apply phase mutates the
		// shared graph and runs under the resolve mutex so it does not
		// race a concurrent watcher / resolver pass on another file.
		mu := g.ResolveMutex()
		mu.Lock()
		ap := newApplier(g, p.spec, p.Name())
		ap.applyAll([]*fileFacts{facts}, res)
		ap.flush()
		p.countCoverage(g, fileNode.RepoPrefix, map[string]bool{facts.file: true}, res)
		mu.Unlock()
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// countCoverage fills the symbols-covered counters: total is every
// symbol of the provider's languages, covered is the subset living in
// files the pass analyzed.
func (p *Provider) countCoverage(g graph.Store, repoPrefix string, analyzed map[string]bool, res *semantic.EnrichResult) {
	langs := make(map[string]bool, len(p.spec.Languages))
	for _, l := range p.spec.Languages {
		langs[l] = true
	}
	// Indexed exact-prefix scan; empty prefix is the embedded single-repository
	// namespace rather than a graph-wide wildcard.
	nodes := g.GetRepoNodes(repoPrefix)
	for _, n := range nodes {
		if n.RepoPrefix != repoPrefix || !langs[n.Language] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		res.SymbolsTotal++
		if analyzed[n.FilePath] {
			res.SymbolsCovered++
		}
	}
	if res.SymbolsTotal > 0 {
		res.CoveragePercent = float64(res.SymbolsCovered) / float64(res.SymbolsTotal) * 100
	}
}
