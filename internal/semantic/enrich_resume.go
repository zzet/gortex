package semantic

import (
	"context"
	"os"
)

// EnrichResumeScope tells a provider that applies in deterministic pages
// whether it may persist and honour resume points for the pass it is running.
// A pass cut by its deadline otherwise starts again from the first page on
// the next run and, given the same budget, is cut at the same place: it can
// never complete. Only the manager attaches a scope, and only for a pass
// whose completion marker could be written (see enrichResumeScope).
//
// SHA keys every point; a point recorded at another revision is never used.
// Record allows writing points. Resume allows starting from an existing one;
// when it is false an existing point must be discarded before the pass
// applies anything, so a point recorded before this run invalidated it can
// never be honoured later.
type EnrichResumeScope struct {
	SHA    string
	Record bool
	Resume bool
}

type enrichResumeScopeKey struct{}

// WithEnrichResume attaches a resume scope to a provider context.
func WithEnrichResume(ctx context.Context, scope EnrichResumeScope) context.Context {
	return context.WithValue(ctx, enrichResumeScopeKey{}, scope)
}

// EnrichResumeFrom returns the pass's resume scope. ok is false when the
// caller attached none: the provider then neither reads nor writes points.
func EnrichResumeFrom(ctx context.Context) (EnrichResumeScope, bool) {
	if ctx == nil {
		return EnrichResumeScope{}, false
	}
	scope, ok := ctx.Value(enrichResumeScopeKey{}).(EnrichResumeScope)
	return scope, ok
}

// enrichResumeScope derives a pass's resume scope from the same freshness the
// completion marker is gated on, so a pass resumes only where a marker would
// be honoured. No scope without a sha. A checkout-scoped pass gets none: each
// working-tree build mints a payload nothing has enriched, so progress an
// earlier build recorded says nothing about this one (the reason
// enrichMarkerCurrent never skips on a checkout marker). A dirty tree may
// neither record nor resume — the pass applies uncommitted content the sha
// does not name. Force and GORTEX_WARMUP_FORCE_ENRICH mean this run must not
// trust what is persisted (a re-parse evicted the repo's enrichment, or the
// operator asked for a full re-enrich), so they start from the first page;
// their own points stay valid for the next run. A Force raised only by
// scoped re-parses whose files are all on the per-file ledger
// (ReparsedFilesMarked) still resumes: the provider re-applies those files.
func enrichResumeScope(rs RepoEnrichState) (EnrichResumeScope, bool) {
	if rs.SHA == "" || rs.CheckoutID != "" {
		return EnrichResumeScope{}, false
	}
	scope := EnrichResumeScope{SHA: rs.SHA, Record: !rs.Dirty}
	trusted := !rs.Force || rs.ReparsedFilesMarked
	scope.Resume = scope.Record && trusted && os.Getenv("GORTEX_WARMUP_FORCE_ENRICH") != "1"
	return scope, true
}
