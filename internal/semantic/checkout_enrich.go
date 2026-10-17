package semantic

import (
	"context"
	"errors"
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// CheckoutEnrichRequest is one enrichment pass over a routed checkout's
// working copy, run against the payload of the generation that describes it.
type CheckoutEnrichRequest struct {
	// RepoPrefix is the namespace the payload's nodes are keyed in. Every
	// checkout of a family shares it — the layers compose over the primary's
	// corpus — so it does not identify the working copy on its own.
	RepoPrefix string
	// CheckoutID is what does identify it, and what the pass's marker is
	// scoped by so a checkout's completion never speaks for the primary's.
	CheckoutID string
	// Root is the checkout's working copy: the directory the language servers
	// are rooted at and the bytes the pass reads.
	Root string
	// Fingerprint names the working-tree state the pass ran over. It is
	// recorded on the marker in place of a commit sha, because a working tree
	// with uncommitted edits in it is a state no commit names.
	Fingerprint string
	// MinLanguageNodes is the admission floor a language must clear to be
	// worth a server, mirroring the index-time pass.
	MinLanguageNodes int
	// Compiler is the compiler scope the pass asks of compiler-backed
	// providers. The pass always runs as a checkout pass (per-file compact
	// projections restricted to the files the generation carries); the zero
	// value asks for a whole-module load.
	Compiler CheckoutCompilerScope
}

// CheckoutEnrichReport is what one checkout-scoped pass did. Every field is
// something the caller has to declare about its generation: what ran decides
// which capabilities are whole, and what did not run decides what the
// generation must not claim.
type CheckoutEnrichReport struct {
	// Ran lists the languages a provider enriched, sorted.
	Ran []string
	// Starved lists the languages the workspace cap could not admit, sorted.
	// They are not failures — the next build over this checkout tries again.
	Starved []string
	// Partial reports that a provider that ran was cut short at its deadline.
	Partial bool
	// Disabled reports that checkout enrichment is switched off rather than
	// unable to run, which is the difference between waiting being pointless
	// and waiting being the fix.
	Disabled bool
	// Reason says why the pass did not enrich everything it could have. Empty
	// when it did.
	Reason string
	// Preempted reports that a committed tree's pass gave way to an edit's
	// compiler load before it finished. The caller's build goes on without
	// the pass's facts.
	Preempted bool
	// Compiler sums the compiler-context work the pass's providers reported.
	// nil when no provider that ran reports counts.
	Compiler *CompilerLoadStats
}

// EnrichCheckout runs the language-server enrichment stage over one routed
// checkout's payload.
//
// g is the generation being built, not the corpus: the census that decides
// which languages are worth a server reads the generation's own nodes, and the
// edges the providers land are written into the generation. The root is the
// checkout's, so the servers read the branch and the uncommitted edits the
// generation describes rather than the primary's tree.
//
// It never returns an error for work it merely could not do. A language the
// cap refused, a checkout with nothing enrichable in it and enrichment being
// switched off are all reported in the report and leave the caller's build
// intact; only a request that names no checkout is refused.
func (m *Manager) EnrichCheckout(g graph.Store, req CheckoutEnrichRequest) (CheckoutEnrichReport, error) {
	return m.EnrichCheckoutContext(context.Background(), g, req)
}

// EnrichCheckoutContext is EnrichCheckout under the caller's context: a
// cancelled ctx stops the pass before dispatch, reaches every provider (the
// go/types load and its stages observe it), and is returned as the error.
func (m *Manager) EnrichCheckoutContext(ctx context.Context, g graph.Store, req CheckoutEnrichRequest) (CheckoutEnrichReport, error) {
	var report CheckoutEnrichReport
	if ctx == nil {
		ctx = context.Background()
	}
	if req.RepoPrefix == "" || req.Root == "" {
		return report, errors.New("semantic: a checkout enrichment needs a repo prefix and a checkout root")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	switch {
	case m == nil || !m.config.Enabled:
		report.Disabled = true
		report.Reason = "semantic enrichment is switched off"
		return report, nil
	case !m.config.checkoutLSPEnabled():
		report.Disabled = true
		report.Reason = "per-checkout language-server enrichment is switched off"
		return report, nil
	}

	roots := map[string]string{req.RepoPrefix: req.Root}
	languages := m.enrichableLanguages(g, roots, req.MinLanguageNodes)
	if len(languages) == 0 {
		report.Reason = "the generation carries no symbols worth enriching"
		return report, nil
	}
	committed := req.Compiler.Committed
	if committed {
		if languages = m.committedTreeLanguages(languages); len(languages) == 0 {
			report.Reason = "no provider that reads a committed tree serves the generation's languages"
			return report, nil
		}
	}
	// A committed tree's pass is background work: an edit's admission takes
	// its workspace slot or its compiler admission, and either one ends it
	// through this context, with its own cause so the pass can say so.
	var stageCancel context.CancelCauseFunc
	if committed {
		ctx, stageCancel = context.WithCancelCause(ctx)
		defer stageCancel(nil)
		req.Compiler.Preempt = func() { stageCancel(ErrCommittedPassPreempted) }
	}

	admitted := make([]string, 0, len(languages))
	releases := make([]func(), 0, len(languages))
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, language := range languages {
		var (
			release func()
			ok      bool
		)
		if committed {
			release, ok = m.checkouts.AcquireCommitted(language, req.Root, req.Compiler.Preempt)
		} else {
			release, ok = m.checkouts.Acquire(language, req.Root)
		}
		if !ok {
			report.Starved = append(report.Starved, language)
			continue
		}
		admitted = append(admitted, language)
		releases = append(releases, release)
	}
	if len(admitted) == 0 {
		report.Reason = "every language server workspace this checkout needs is over the global cap"
		m.logger.Info("checkout enrichment starved by the workspace cap",
			zap.String("checkout", req.CheckoutID),
			zap.String("root", req.Root),
			zap.Strings("languages", report.Starved),
		)
		return report, nil
	}

	compiler := req.Compiler
	results, partial, err := m.EnrichAll(g, roots, EnrichOptions{
		RepoState: map[string]RepoEnrichState{req.RepoPrefix: {
			SHA:        req.Fingerprint,
			CheckoutID: req.CheckoutID,
		}},
		MinLanguageNodes:  req.MinLanguageNodes,
		Languages:         admitted,
		CheckoutScope:     &compiler,
		CommittedTreeOnly: committed,
		Context:           ctx,
	})
	for _, result := range results {
		if result == nil || result.Compiler == nil {
			continue
		}
		if report.Compiler == nil {
			report.Compiler = &CompilerLoadStats{}
		}
		report.Compiler.Add(result.Compiler)
	}
	if committed && errors.Is(context.Cause(ctx), ErrCommittedPassPreempted) {
		report.Preempted = true
		report.Partial = true
		report.Reason = ErrCommittedPassPreempted.Error()
		m.logger.Info("committed tree's enrichment pass preempted by an edit",
			zap.String("checkout", req.CheckoutID),
			zap.String("root", req.Root))
		return report, nil
	}
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.Partial = partial[req.RepoPrefix]
	report.Ran = enrichedLanguages(results)
	if len(report.Starved) > 0 {
		report.Reason = "the global language-server workspace cap refused some of this checkout's languages"
	}
	return report, nil
}

// CheckoutWorkspaces returns the registry the manager admits per-checkout
// language-server workspaces through. It is never nil for a manager built by
// NewManager, so a caller may read the cap or the live set without a guard.
func (m *Manager) CheckoutWorkspaces() *CheckoutWorkspaces {
	if m == nil {
		return nil
	}
	return m.checkouts
}

// enrichableLanguages is the language set a pass over roots would run for: the
// languages the graph holds symbol-bearing nodes in, minus the ones below the
// admission floor. It is the same census EnrichAll runs, lifted ahead of it so
// the workspace cap decides per language before any provider is asked for.
func (m *Manager) enrichableLanguages(g graph.Store, roots map[string]string, floor int) []string {
	present, _, langCounts := m.repoLanguages(g, roots)
	out := make([]string, 0, len(present))
	for language := range present {
		if floor > 0 && langCounts[language] < floor {
			continue
		}
		out = append(out, language)
	}
	sort.Strings(out)
	return out
}

// enrichedLanguages collapses a pass's results into the sorted language set it
// covered. A provider may serve several languages and several providers may
// serve one, so the results are a bag rather than a set.
func enrichedLanguages(results []*EnrichResult) []string {
	seen := make(map[string]struct{}, len(results))
	out := make([]string, 0, len(results))
	for _, result := range results {
		if result == nil || result.Language == "" {
			continue
		}
		if _, dup := seen[result.Language]; dup {
			continue
		}
		seen[result.Language] = struct{}{}
		out = append(out, result.Language)
	}
	sort.Strings(out)
	return out
}

// committedTreeLanguages narrows languages to those whose provider reads a
// committed tree through the pass's overlay; every other provider would read
// the checkout's working copy instead of the tree being built.
func (m *Manager) committedTreeLanguages(languages []string) []string {
	out := languages[:0:0]
	for _, language := range languages {
		if provider := m.ProviderForLanguage(language); provider != nil && readsCommittedTree(provider) {
			out = append(out, language)
		}
	}
	return out
}
