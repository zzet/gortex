package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// ClaimedDedicatedBaseRequest is an initial full build reserved by the catalog.
// The effective configuration identity must include output-affecting workspace
// and project context. A caller must still adopt through the catalog guard after
// this method returns, including for ready reuse and flight followers.
type ClaimedDedicatedBaseRequest struct {
	Claim       store_sqlite.DedicatedBaseBuildClaim
	RootPath    string
	WorkspaceID string
	ProjectID   string
	PrePublish  func(context.Context, int64) error

	// BulkLoad brackets the reserved generation's payload write in the
	// bulk-load shape. nil resolves from the builder's own store, which is
	// what every production caller wants; a caller supplies one only to
	// observe or substitute the bracket.
	BulkLoad GenerationBulkLoader

	// CopySource is the row-level generation copy the clean-tree route writes
	// through. nil resolves from the builder's own store. When neither the
	// request nor the store offers one, every build re-parses the committed
	// tree exactly as it did before this route existed.
	CopySource GenerationPayloadCopier
}

// generationZero names the mutable working-copy view every store carries.
//
// It is the only legal SOURCE for a first committed base's row copy, and it is
// never the base itself. The copy writes into the positive generation the
// catalog already reserved: that generation keeps its own view_generations row,
// its own identity columns (tree_oid, config_hash, extractor_versions,
// resolver_version, dependency_revision), its own replacement masks and its own
// adoption. Generation zero stays mutable, stays where it was routed, and is
// not relabelled committed — which is the one thing acceptance gate 9 forbids.
const generationZero int64 = 0

// GenerationBulkLoader is the generation-scoped bulk-load bracket.
//
// The cold-load fast path engages only when the WHOLE store is empty, so
// generation zero consumes it and every later generation — the committed base
// included — pays per-row B-tree maintenance and page-cache spill at the
// pooled cache size instead. The generation-scoped window's precondition is
// "THIS generation holds no rows", which is true of every reserved candidate
// this builder is handed. Where the window is opened and closed, and why it
// now covers both build routes, is documented on generationBulkWindow.
//
// What the window takes and what it deliberately leaves is the store's own
// decision, documented beside BeginGenerationBulkLoad: it takes the page cache
// and wal_autocheckpoint, and it does NOT drop the generation-blind secondary
// indexes or lower synchronous, because generation zero's published rows are
// underneath and its readers are live. This builder only brackets; it makes no
// claim about the shape.
//
// It is declared as an interface here rather than called on *store_sqlite.Store
// directly so the bracket is substitutable in a test. generationBulkLoader
// resolves it with a plain optional assertion against the builder's own store,
// which is what binds the production path; a test asserts the store still
// satisfies this interface so a signature drift cannot silently unbind it.
type GenerationBulkLoader interface {
	// BeginGenerationBulkLoad reports whether a window was opened. False with
	// a nil error is a refusal, not a failure — an in-memory store, or another
	// bulk window already owning the pinned writer — and the caller must not
	// close a window it does not hold.
	BeginGenerationBulkLoad(generationID int64) (bool, error)
	// EndGenerationBulkLoad restores the connection-local pragmas, releases
	// the pinned writer and hands the accumulated log to the residue gate. It
	// is idempotent and inert when no window is open, which is what makes it
	// safe on the failure and panic paths as well as the success path.
	EndGenerationBulkLoad() error
}

// GenerationPayloadCopier materialises one payload generation's rows as
// another generation's rows, without reading the tree they describe.
//
// The copy is row-level across every generation-keyed table — nodes, edges,
// the file/FTS/sidecar projections and the masks the registries declare — so
// the destination generation ends up carrying the same payload a re-parse of
// the same bytes would have produced, plus whatever post-ready enrichment
// generation zero has since received and a re-parse would not carry at all.
//
// Declared as an interface for the same reason as GenerationBulkLoader: the
// store-side primitive lands separately, and this file consumes it through an
// optional assertion so no edit here is needed to activate it.
type GenerationPayloadCopier interface {
	CopyPayloadGeneration(ctx context.Context, from, to int64, repoPrefix string) (GenerationCopyCounts, error)
}

// GenerationCopyCounts is what one row-level generation copy moved. The counts
// are for the build report and the log line; the authoritative description of
// what landed is the destination generation's own payload, which the caller
// reads back rather than trusting these numbers.
//
// It is an ALIAS of the store's own type rather than a second declaration of
// the same three fields, and that is what lets *store_sqlite.Store satisfy
// GenerationPayloadCopier: an interface method's result type must be identical,
// not merely identically shaped, so a separate struct here would leave the
// production copier permanently unbindable and the copy route permanently dead.
type GenerationCopyCounts = store_sqlite.GenerationCopyCounts

// BuildClaimedDedicatedBase consumes the positive reserved generation rather
// than allocating a second one. It is an initial-full-snapshot primitive, not
// the later incremental committed-base advancement path.
func (b *SparseGenerationBuilder) BuildClaimedDedicatedBase(ctx context.Context, request ClaimedDedicatedBaseRequest) (builtGeneration int64, builtReport BuildReport, buildErr error) {
	started := time.Now()
	claim := request.Claim
	if ctx == nil || b == nil || b.Store == nil || b.Registry == nil || b.Logger == nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: claimed base requires context, store, registry and logger")
	}
	if err := ctx.Err(); err != nil {
		return 0, BuildReport{}, err
	}
	if claim.GenerationID <= 0 || claim.AttemptToken == "" || claim.BaseGenerationID != 0 || claim.LayerID != "" || claim.LowerViewFingerprint != "" {
		return 0, BuildReport{}, fmt.Errorf("indexer: initial base requires a positive full-snapshot reservation")
	}
	p, found, err := b.Store.Catalog().DedicatedBasePublication(ctx, claim.Desire.Authority.GraphID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !found || p.Desire != claim.Desire || p.Claim.AttemptToken != claim.AttemptToken ||
		p.Claim.GenerationID != claim.GenerationID || p.Claim.BaseGenerationID != claim.BaseGenerationID ||
		p.Claim.LayerID != claim.LayerID || p.Claim.LowerViewFingerprint != claim.LowerViewFingerprint ||
		p.Claim.ExpectedActiveGenerationID != claim.ExpectedActiveGenerationID ||
		(p.AttemptState != "building" && p.AttemptState != "ready" && p.AttemptState != "adopted") {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation changed", store_sqlite.ErrCatalogStaleGuard)
	}
	row, found, err := b.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	identity := claim.Desire.Identity
	if !found || row.GenerationID <= claim.Desire.Authority.GenerationFloor ||
		row.OwnerKind != "dedicated_graph" || row.GenerationKind != "dedicated" ||
		row.GraphID != claim.Desire.Authority.GraphID || row.CheckoutID != claim.Desire.Authority.Owner.CheckoutID ||
		row.BaseGenerationID != 0 || row.LayerID != "" || row.LowerViewFingerprint != "" ||
		row.TreeOID != identity.TreeOID || row.ConfigHash != identity.ConfigHash ||
		row.ExtractorVersions != identity.ExtractorVersions || row.ResolverVersion != identity.ResolverVersion || row.DependencyRevision != identity.DependencyRevision {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation has incompatible payload identity", store_sqlite.ErrDedicatedBaseCandidate)
	}
	if row.State == store_sqlite.ViewGenerationReady || row.State == store_sqlite.ViewGenerationSuperseded {
		// Never call BeginPayloadGeneration or reopen a payload seal on this path.
		// A ready snapshot must not need a Git subprocess, parse, or payload write.
		// As with the existing sparse ready/follower path, this reports reuse,
		// not freshly measured physical work. Do not scan payload to fill counts.
		return row.GenerationID, BuildReport{GenerationID: row.GenerationID, Coalesced: true,
			Duration: time.Since(started)}, nil
	}
	if row.State != store_sqlite.ViewGenerationBuilding || request.RootPath == "" {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base is not a buildable reservation", store_sqlite.ErrDedicatedBaseCandidate)
	}
	// Revalidate current availability and namespace before joining expensive
	// work. This reuses the existing claim without allocating or rewriting it.
	validated, err := b.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: claim.GenerationID,
		Desire:               claim.Desire, ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		AttemptToken: claim.AttemptToken, BaseGenerationID: claim.BaseGenerationID,
		LayerID: claim.LayerID, LowerViewFingerprint: claim.LowerViewFingerprint,
	})
	if err != nil {
		return 0, BuildReport{}, err
	}
	if validated.GenerationID != claim.GenerationID || validated.AttemptToken != claim.AttemptToken {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation was replaced", store_sqlite.ErrCatalogStaleGuard)
	}
	req := BuildRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GenerationKind: "dedicated",
			GraphID: row.GraphID, CheckoutID: row.CheckoutID, TreeOID: row.TreeOID,
			ProvenanceCommitOID: row.ProvenanceCommitOID, ConfigHash: row.ConfigHash,
			ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion, DependencyRevision: row.DependencyRevision, CreatedAt: row.CreatedAt},
		Base: graph.New(), RootPath: request.RootPath,
		RepoPrefix: claim.Desire.Authority.RepoPrefix, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, PrePublish: request.PrePublish,
	}
	handle, err := b.Store.AtManagedGeneration(claim.GenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	adopted := validated.Status != "allocated"
	reparse := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		target, err := source.NewGitTreeSource(ctx, request.RootPath, identity.TreeOID)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, fmt.Errorf("indexer: open claimed base snapshot: %w", err)
		}
		validation := req
		validation.Target = target
		return prepareOwnedDedicatedSnapshot(ctx, target, func() error { return b.validate(ctx, &validation) })
	}
	route := reparse
	copier, hasCopier := b.generationCopier(request)
	takeCopy, zeroState, why := b.claimedBaseCopyPlan(ctx, handle, request.RootPath, req.RepoPrefix,
		identity.TreeOID, identity.ExtractorVersions, hasCopier)
	if takeCopy {
		route = b.prepareCopiedDedicatedBase(copier, req.RepoPrefix, claim.GenerationID, handle, zeroState)
		// The copy route re-derives the whole generation from generation zero
		// and has just proved the reservation carries no rows of its own, which
		// is exactly the property the runner's recovery gate protects: an
		// "adopted" generation runs an index pass because it may hold partial
		// payload from a writer that vanished. Here there is none to complete,
		// and a pass over an empty file set would purge the rows the copy just
		// landed. The flag has no other behaviour at this call site — the build
		// flight reads it only to label an error message.
		adopted = false
	}
	b.Logger.Info("claimed dedicated base source plan",
		zap.Int64("generation", claim.GenerationID),
		zap.String("graph", claim.Desire.Authority.GraphID),
		zap.String("route", claimedBaseRouteName(takeCopy)),
		zap.String("reason", why))

	// One bracket, both routes. See generationBulkWindow for why it is opened
	// from inside the payload preparation and closed from out here.
	loader, _ := b.generationBulkLoader(request)
	window := &generationBulkWindow{
		loader: loader, generationID: claim.GenerationID, logger: b.Logger,
		wholeGeneration: takeCopy,
	}
	defer func() {
		closeErr := window.close()
		if closeErr == nil {
			return
		}
		if buildErr != nil {
			// The build already failed; the close failure is recorded beside
			// it rather than substituted for it.
			b.Logger.Warn("close generation bulk load after a failed claimed base build",
				zap.Int64("generation", claim.GenerationID), zap.Error(closeErr))
			return
		}
		buildErr = closeErr
	}()
	prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		if err := window.open(); err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		return route(ctx)
	}
	callerPrePublish := request.PrePublish
	req.PrePublish = func(ctx context.Context, generationID int64) error {
		// The publication is the heaviest catalog write this build makes and
		// the one whose latency a waiting reader feels, so the pinned writer
		// goes back to the pool before it rather than after. close is
		// idempotent: the deferred close above still covers every path that
		// never reaches a publication, including a caller's hook that refuses
		// one.
		//
		// The caller's own hook runs FIRST, so it still observes the state the
		// build had while it was writing — a hook is the last place a caller
		// can look at the generation it reserved, and moving the close in front
		// of it would change what that hook sees for no gain.
		if callerPrePublish != nil {
			if err := callerPrePublish(ctx, generationID); err != nil {
				return err
			}
		}
		return window.close()
	}
	failed := func(ctx context.Context, cause error) error {
		return b.Store.Catalog().FailDedicatedBaseBuild(ctx, store_sqlite.FailDedicatedBaseBuildRequest{
			Claim: claim, Error: cause.Error(),
		})
	}
	return b.buildReservedGenerationWithCallbacks(ctx, req, buildPlan{}, BuildReport{}, started, claim.GenerationID,
		handle, adopted, prepare, failed)
}

// claimedBaseRouteName is the stable log token for the plan that was taken.
func claimedBaseRouteName(copied bool) string {
	if copied {
		return "copy_generation_zero"
	}
	return "reparse_git_tree"
}

// Until this function returns the source, the physical runner cannot own it.
// Close locally on panic; transfer both successful and error returns to the
// runner for deferred closure.
func prepareOwnedDedicatedSnapshot(ctx context.Context, target source.ContentSource, validate func() error) (source.ContentSource, buildPlan, BuildReport, error) {
	transferred := false
	defer func() {
		if !transferred {
			_ = target.Close()
		}
	}()
	if err := validate(); err != nil {
		transferred = true
		return target, buildPlan{}, BuildReport{}, err
	}
	started := time.Now()
	plan, report, err := planDedicatedSnapshot(ctx, target)
	report.PlanningDuration = time.Since(started)
	transferred = true
	return target, plan, report, err
}

// planDedicatedSnapshot visits the source inventory once. It does not open
// blobs or query an existing graph. Ordinary index admission remains in the
// index pass, so this cannot accidentally omit a supported source language.
func planDedicatedSnapshot(ctx context.Context, target source.ContentSource) (buildPlan, BuildReport, error) {
	var plan buildPlan
	var report BuildReport
	err := target.Walk(ctx, func(meta source.FileMeta) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		plan.indexed = append(plan.indexed, meta.Path)
		if meta.Size > 0 {
			report.SourceBytes += meta.Size
		}
		return nil
	})
	if err != nil {
		return buildPlan{}, BuildReport{}, fmt.Errorf("indexer: enumerate dedicated base snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return buildPlan{}, BuildReport{}, err
	}
	report.AddedFiles = len(plan.indexed)
	report.IndexedPaths = plan.indexed
	return plan, report, nil
}

// generationBulkLoader resolves the bulk-load bracket for this build: the
// caller's, else the builder's own store when it carries the entry point.
//
// This is the single wiring point for the store-side primitive. Nothing else
// in this package needs to change when it lands.
func (b *SparseGenerationBuilder) generationBulkLoader(request ClaimedDedicatedBaseRequest) (GenerationBulkLoader, bool) {
	if request.BulkLoad != nil {
		return request.BulkLoad, true
	}
	loader, ok := any(b.Store).(GenerationBulkLoader)
	if !ok {
		return nil, false
	}
	return loader, true
}

// generationCopier resolves the row-level copy primitive for this build.
func (b *SparseGenerationBuilder) generationCopier(request ClaimedDedicatedBaseRequest) (GenerationPayloadCopier, bool) {
	if request.CopySource != nil {
		return request.CopySource, true
	}
	copier, ok := any(b.Store).(GenerationPayloadCopier)
	if !ok {
		return nil, false
	}
	return copier, true
}

// generationBulkWindow is one claimed-base build's generation-scoped bulk-load
// bracket. It exists as a small object rather than a `with...(run func())`
// wrapper because its two halves belong at two different depths of the build,
// and no single call frame spans exactly the payload write.
//
// # Where it opens: inside the payload preparation, leader-only
//
// The window is a store-level singleton: opening it pins one writer connection
// (store_sqlite/bulk_load.go, BeginGenerationBulkLoad — `s.bulkConn` plus
// `s.generationBulkLoad`), and the store refuses a second one while it is held.
// A build joins a payload flight, and only the physical LEADER writes rows; the
// coalescing followers run the same function and then wait. Opening from the
// call's own frame would let a follower win the window and hold the pinned
// writer across its wait while the leader — the only goroutine that writes —
// is refused and writes unbracketed. The payload preparation is the first thing
// the flight runs for the leader and for nobody else, so that is where open()
// is called from.
//
// # Where it closes: a deferred close on the build, plus the pre-publication one
//
// The close is DEFERRED on BuildClaimedDedicatedBase, so it runs on every exit
// the build has: the ordinary return, an error return, a panic, and a
// runtime.Goexit. Only a deferred close covers the last two, and any missed
// close leaves the store holding a pinned writer connection with automatic
// checkpoints disabled — an unbounded WAL nobody owns, which the next foreign
// FlushBulk would then adopt and pay for.
//
// A successful build closes earlier than that, from the pre-publication hook,
// so the publication's catalog transaction — the heaviest write the build makes
// and the one a waiting reader feels — runs on the ordinary pool rather than
// behind the pinned writer. close is idempotent, so the deferred close is a
// no-op after it and the "closed on every exit" property is unchanged.
//
// Either way it closes through EndGenerationBulkLoad, which is the store's
// DEFERRED drain door: one bounded PASSIVE (which never waits for a reader) and
// the follow-up TRUNCATE handed to the maintenance lane
// (store_sqlite/bulk_load.go, EndGenerationBulkLoad → scheduleWALDrainAboveLine).
// The other door — FlushBulk — runs the TRUNCATE inline, waiting out the live
// readers of the generations underneath, which is exactly the wait a committed
// base's publication must not take.
//
// # Why it now covers the re-parse route too
//
// It used to cover the copy route only, because the re-parse route's payload
// write is not this builder's: it happens inside the index pass, which brackets
// its own shadow drain with the store's cold window (indexer.go,
// `bl.BeginBulkLoad()` … `bl.FlushBulk()`). BeginBulkLoad was already a no-op
// inside a held window, but FlushBulk closed whatever bulk connection it found
// — taking the window from its owner mid-payload, leaving EndGenerationBulkLoad
// inert so the residue gate never ran, and charging the pass a synchronous
// TRUNCATE. FlushBulk now leaves a generation-scoped window to its owner
// (store_sqlite/bulk_load.go, the `s.generationBulkLoad != 0` arm), so the
// bracket spans the pass as well and both routes get the shape.
//
// # A populated destination is a refusal only when the build writes the whole
// generation
//
// ErrGenerationBulkLoadPopulated says the destination already holds rows. On
// the COPY route that contradicts the predicate that chose the route — the
// store's check covers nodes AND edges, which is strictly wider than the
// builder's node probe, so an edges-only residue from a writer that vanished
// reaches here — and a whole-generation copy on top of that residue would
// publish a base that is neither generation zero's payload nor a re-parse of
// the tree. So the build fails and the reservation is retried from a clean
// generation. On the RE-PARSE route a populated destination is the ordinary
// recovery case (an adopted generation carrying partial payload from a vanished
// writer, which the pass re-derives in full), so it costs the window and
// nothing else.
//
// Every other refused begin is not an error either: an in-memory store has no
// log to spare and another bulk window may already own the pinned writer. In
// both cases the ordinary write path is correct, and losing a build because the
// store could not take a cheaper shape would be a worse answer than a slower
// build.
//
// It is not safe for concurrent use and does not need to be: open runs in the
// leader's payload preparation and close runs in the leader's own pre-publish
// hook or in its deferred build exit, all on one goroutine.
type generationBulkWindow struct {
	loader       GenerationBulkLoader
	generationID int64
	logger       *zap.Logger

	// wholeGeneration says this build materialises the destination generation
	// in one piece, which is what makes a populated destination a contradiction
	// rather than a recovery.
	wholeGeneration bool

	opened bool
}

// open takes the window for this build's generation, or reports why the build
// must stop. A refusal that is not a contradiction leaves opened false and the
// ordinary unbracketed write path in place.
func (w *generationBulkWindow) open() error {
	if w == nil || w.loader == nil || w.opened {
		return nil
	}
	opened, err := w.loader.BeginGenerationBulkLoad(w.generationID)
	if err != nil {
		if w.wholeGeneration && errors.Is(err, store_sqlite.ErrGenerationBulkLoadPopulated) {
			return fmt.Errorf("indexer: refuse to copy into generation %d over existing payload: %w",
				w.generationID, err)
		}
		if w.logger != nil {
			w.logger.Warn("generation bulk load refused",
				zap.Int64("generation", w.generationID), zap.Error(err))
		}
	}
	w.opened = opened
	return nil
}

// close releases a window this build holds. It is idempotent and inert for a
// window that was never opened, which is what makes it safe both as the
// pre-publication release and as the deferred catch-all.
func (w *generationBulkWindow) close() error {
	if w == nil || !w.opened {
		return nil
	}
	w.opened = false
	if err := w.loader.EndGenerationBulkLoad(); err != nil {
		return fmt.Errorf("indexer: close generation %d bulk load: %w", w.generationID, err)
	}
	return nil
}

// claimedBaseCopyPlan decides whether this initial committed base can be
// materialised by copying generation zero's rows instead of re-parsing the
// committed tree, and says why in one bounded, path-free sentence.
//
// Every clause is a refusal, and the order is cheapest-first: the whole
// predicate costs nothing at all on a store with no copy primitive, and at most
// two git shell-outs plus one inventory read otherwise. A false negative costs
// the re-parse this route exists to avoid; a false positive would publish a
// committed base that does not describe the committed tree, so each clause is
// written to refuse whenever it cannot prove its half.
//
// The five things that together make the copy sound:
//
//  1. a copy primitive exists at all, and the reserved generation is still
//     empty — a writer that vanished part way may have left rows behind, and
//     the copy materialises a whole generation rather than completing a
//     partial one. This clause reads nodes only, because it is a cheap
//     predicate and not the authority: the store re-proves emptiness over
//     nodes AND edges when the bulk window opens, and generationBulkWindow
//     turns that refusal into a failed build rather than an unbracketed write
//     onto the residue;
//  2. generation zero recorded a CLEAN index at a known commit — the indexer
//     stamps repo_index_state.dirty at the end of every pass, so this is the
//     index's own statement about the bytes it read;
//
//  2a. generation zero's rows were produced by the EXTRACTOR VERSIONS this
//     reservation's identity claims. repo_index_state.extractor_versions and
//     the reservation identity are the same string from the same function
//     (index_state.go persistRepoIndexState marshals extractorVersionsSnapshot;
//     checkout_coordinator.go extractorVersionsFingerprint marshals the same
//     map), and they disagree exactly when generation zero is behind a version
//     bump — the window extractorVersionStaleLangSet exists to detect, in
//     which generation zero is still clean at HEAD and every other clause
//     passes. Copying then would publish old-extractor rows into a generation
//     whose catalog row claims the new versions, and the reuse guards compare
//     that identity, so the mislabelled base would be reused as current. The
//     re-parse route cannot have the problem: it extracts with the running
//     process's extractors. Neither ResolverVersion, ConfigHash nor
//     DependencyRevision is recorded in repo_index_state, so only this one
//     component of the identity can be proved from generation zero's row;
//  3. the working tree still sits on exactly that commit and is PROVABLY
//     unmodified — git status --porcelain lists untracked files too, so an
//     untracked source file that generation zero indexed shows up here. The
//     probe reports "clean", "dirty" and "could not tell" as three distinct
//     answers, and only the first admits the copy: repoHeadAndDirty, which the
//     indexer's own freshness stamp uses, reports a failed status as NOT dirty
//     because a freshness stamp must never block indexing. That default is
//     wrong here — a status that timed out on a contended index lock would
//     admit a copy over a tracked-file edit made after generation zero's clean
//     pass — so this predicate uses its own prove-or-refuse probe;
//  4. the commit's tree is the tree this base reserved, and generation zero
//     carries payload at no path outside it.
//
// Clauses 2 and 3 are not redundant. The first says what generation zero read;
// the second says what is on disk now. Only both together say that the rows in
// generation zero describe the tree named by identity.TreeOID.
//
// There is deliberately NO "freshly allocated reservation" clause. Every claimed
// initial base arrives on a reservation an earlier call allocated — the
// revalidation at the head of this build returns the publication's own attempt
// state, never "allocated" — so such a clause would refuse the route in every
// case. Partial payload from a writer that vanished is handled where it is
// already handled: the runner abandons a generation that did not publish, and
// the copy itself re-derives the whole source generation rather than a delta of
// it.
// The returned index state is generation zero's own freshness provenance, and
// it is meaningful only when the copy is taken: it is what the copied
// generation records as its own, in place of the row an index pass would have
// written at the end of the pass the copy route does not run.
func (b *SparseGenerationBuilder) claimedBaseCopyPlan(
	ctx context.Context,
	handle *store_sqlite.Store,
	rootPath, repoPrefix, treeOID, extractorVersions string,
	copierPresent bool,
) (bool, graph.RepoIndexState, string) {
	var none graph.RepoIndexState
	switch {
	case !copierPresent:
		return false, none, "no generation copy primitive on this store"
	case rootPath == "" || treeOID == "":
		return false, none, "the reservation names no root or no tree"
	}
	if err := ctx.Err(); err != nil {
		return false, none, "the build was cancelled before the source plan"
	}
	if len(handle.GetRepoNodesLight(repoPrefix)) > 0 {
		return false, none, "the reserved generation already carries payload"
	}
	state, found, err := b.Store.AtGeneration(generationZero).GetRepoIndexState(repoPrefix)
	switch {
	case err != nil:
		return false, none, "generation zero index provenance is unreadable"
	case !found || state.IndexedSHA == "":
		return false, none, "generation zero recorded no index provenance"
	case state.Dirty:
		return false, none, "generation zero indexed a dirty working tree"
	case state.ExtractorVersions == "" || extractorVersions == "":
		return false, none, "generation zero recorded no extractor versions"
	case state.ExtractorVersions != extractorVersions:
		return false, none, "generation zero was indexed by different extractor versions"
	}
	head, clean, proved := claimedBaseWorkingTreeState(ctx, rootPath)
	switch {
	case head == "":
		return false, none, "the checkout has no readable HEAD"
	case !proved:
		return false, none, "the working tree's state could not be read"
	case !clean:
		return false, none, "the working tree has uncommitted changes"
	case head != state.IndexedSHA:
		return false, none, "the working tree moved since generation zero was indexed"
	}
	headTree, err := gitcmd.Output(ctx, rootPath, "rev-parse", "--verify", head+"^{tree}")
	switch {
	case err != nil || headTree == "":
		return false, none, "the committed tree of HEAD is unreadable"
	case headTree != treeOID:
		return false, none, "generation zero covers a different tree"
	}
	outside, err := b.generationZeroPathsOutsideTree(ctx, rootPath, repoPrefix, treeOID)
	switch {
	case err != nil:
		return false, none, "generation zero inventory is unreadable"
	case outside > 0:
		return false, none, "generation zero carries payload outside the committed tree"
	}
	return true, state, "the working tree is clean at the reserved tree"
}

// claimedBaseWorkingTreeState answers "what commit is this checkout on, and is
// it modified" with THREE outcomes rather than two: clean, modified, and could
// not tell.
//
// repoHeadAndDirty, the indexer's own probe, collapses the third into "clean"
// on purpose — it feeds a freshness stamp, and a stamp that refused to be
// written because git status timed out would block indexing for no gain. A
// route that publishes a committed base out of working-copy rows cannot take
// that default: `git --no-optional-locks status --porcelain` fails on a
// contended index lock, a permission error or its own timeout, and every one of
// those leaves a tracked-file edit invisible while HEAD, the tree and the
// containment check all still agree. So an unreadable status is reported as
// unproved and the caller refuses.
//
// proved is false whenever either shell-out failed. head is empty only when
// HEAD itself is unreadable, which the caller reports separately so the two
// refusals stay distinguishable in the log.
func claimedBaseWorkingTreeState(ctx context.Context, rootPath string) (head string, clean, proved bool) {
	probe, cancel := context.WithTimeout(ctx, claimedBaseGitProbeTimeout)
	defer cancel()
	head, err := gitcmd.Output(probe, rootPath, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return "", false, false
	}
	// --no-optional-locks for the same reason the indexer uses it: status would
	// otherwise refresh Git's cached stat data under the worktree index lock,
	// and indexing is a read-only observer that must not compete with a user's
	// own git add.
	status, err := gitcmd.Output(probe, rootPath, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return head, false, false
	}
	return head, status == "", true
}

// claimedBaseGitProbeTimeout bounds the predicate's own shell-outs. It matches
// the indexer's freshness probe: long enough for a large repository's status,
// short enough that a wedged git sends the build back to the re-parse instead
// of stalling it.
const claimedBaseGitProbeTimeout = 5 * time.Second

// generationZeroPathsOutsideTree counts the files generation zero carries
// payload for that the committed tree does not contain.
//
// A clean status is necessary but not sufficient: git says nothing about a
// path it ignores, and the working-copy index admits files by its own rules.
// A single such file would make the copied base claim a path the committed
// tree has no bytes for, so its presence sends the build back to the re-parse.
// The check reads names only — no blob is opened on either side.
func (b *SparseGenerationBuilder) generationZeroPathsOutsideTree(
	ctx context.Context,
	rootPath, repoPrefix, treeOID string,
) (int, error) {
	out, err := gitcmd.Output(ctx, rootPath, "ls-tree", "-r", "--name-only", "--full-tree", "-z", treeOID)
	if err != nil {
		return 0, err
	}
	tree := make(map[string]struct{})
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			tree[name] = struct{}{}
		}
	}
	rows, err := b.Store.AtGeneration(generationZero).FileMetasForRepo(repoPrefix)
	if err != nil {
		return 0, err
	}
	outside := 0
	for _, row := range rows {
		rel, owned := builderRelPath(repoPrefix, row.FilePath)
		if !owned {
			outside++
			continue
		}
		if _, carried := tree[rel]; !carried {
			outside++
		}
	}
	return outside, nil
}

// prepareCopiedDedicatedBase is the copy route's payload production.
//
// It occupies exactly the slot the git-tree preparation occupies, which is why
// the rest of the reserved-generation lifecycle is untouched: the flight, the
// abandonment of a half-written generation, the failure notification, the mask
// derivation, the producer declaration and the publication all run as they do
// on the re-parse route. Only the bytes' origin changes.
//
// The returned plan is deliberately EMPTY. buildPlan.indexed is the file set a
// PASS walks, and this route runs no pass — the runner's own contract is that
// a freshly allocated generation with nothing to index skips runPass. The
// masks are not derived from the plan either: writeMasks reads the generation's
// own file inventory and node set back out of the handle, so the copied payload
// produces exactly the replacement claims its own rows justify.
// The whole write happens inside the generation-scoped bulk-load bracket that
// the build opened just before calling this preparation, and the bracket is
// opened there rather than here because the RE-PARSE route needs the same one
// and has no preparation of its own to hang it on. Everything the bracket has
// to be true of is argued on generationBulkWindow; what matters here is only
// that a copy is one write and it is inside it.
//
// Everything after the copy — enrichment, context separation, masks and
// producer states — runs inside the same window, and the publication runs
// outside it, on the shared lifecycle the re-parse route uses too, so the two
// routes finish identically.
func (b *SparseGenerationBuilder) prepareCopiedDedicatedBase(
	copier GenerationPayloadCopier,
	repoPrefix string,
	generationID int64,
	handle *store_sqlite.Store,
	zeroState graph.RepoIndexState,
) generationPayloadPreparation {
	return func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		counts, err := copier.CopyPayloadGeneration(ctx, generationZero, generationID, repoPrefix)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, fmt.Errorf("indexer: copy generation %d into claimed base %d: %w",
				generationZero, generationID, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		if err := writeCopiedGenerationIndexState(handle, repoPrefix, zeroState, counts); err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		report, err := copiedDedicatedBaseReport(handle, repoPrefix)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		report.NodeCount, report.EdgeCount = int(counts.Nodes), int(counts.Edges)
		return copiedGenerationSource{identity: fmt.Sprintf("generation-copy:%d->%d", generationZero, generationID)},
			buildPlan{}, report, nil
	}
}

// writeCopiedGenerationIndexState gives the copied generation the freshness
// provenance every re-parsed generation gets.
//
// repo_index_state is a per-generation sidecar (`view_gen` is part of its key),
// and the only writer of a generation's row is the tail of an index pass. The
// copy route runs no pass, so without this the copied base would be the one
// committed base in the store that cannot say which commit it describes, how
// many rows it carries, or which extractor versions produced them — and that
// row is exactly what this route's own predicate reads out of generation zero
// to decide whether a later base may be copied again.
//
// The values are generation zero's, which is sound precisely because the copy
// was admitted: the predicate proved generation zero indexed a CLEAN tree at
// this checkout's current HEAD, and that HEAD's tree is the tree this
// reservation names. IndexedAt is re-stamped to now because it describes when
// these rows landed in THIS generation. The counts are the copier's report of
// what it moved rather than a re-count of the destination: a full re-count of a
// freshly written generation is the one read this route exists to avoid, and
// the file inventory the build report carries is read back from the destination
// either way.
//
// A store that cannot record index state (the in-memory graph does not
// implement the writer) is not an error here, exactly as it is not an error at
// the end of a pass.
func writeCopiedGenerationIndexState(
	handle *store_sqlite.Store,
	repoPrefix string,
	zeroState graph.RepoIndexState,
	counts GenerationCopyCounts,
) error {
	writer, ok := graph.Store(handle).(graph.RepoIndexStateWriter)
	if !ok {
		return nil
	}
	state := zeroState
	state.RepoPrefix = repoPrefix
	state.IndexedAt = time.Now().Unix()
	state.NodeCount, state.EdgeCount = int(counts.Nodes), int(counts.Edges)
	if err := writer.SetRepoIndexState(state); err != nil {
		return fmt.Errorf("indexer: record copied generation %d index state: %w", handle.ViewGeneration(), err)
	}
	return nil
}

// copiedDedicatedBaseReport describes what the copy actually landed, read back
// from the destination generation rather than taken from the copier's own
// counts. A report that says what the generation carries is the only kind a
// later reader can check.
func copiedDedicatedBaseReport(handle *store_sqlite.Store, repoPrefix string) (BuildReport, error) {
	rows, err := handle.FileMetasForRepo(repoPrefix)
	if err != nil {
		return BuildReport{}, fmt.Errorf("indexer: read copied generation inventory: %w", err)
	}
	report := BuildReport{IndexedPaths: make([]string, 0, len(rows))}
	for _, row := range rows {
		rel, owned := builderRelPath(repoPrefix, row.FilePath)
		if !owned {
			continue
		}
		report.IndexedPaths = append(report.IndexedPaths, rel)
		if row.Size > 0 {
			report.SourceBytes += int64(row.Size)
		}
	}
	report.AddedFiles = len(report.IndexedPaths)
	return report, nil
}

// copiedGenerationSource stands in for the content source a parse would have
// read. The copy route walks no files: the payload it publishes came out of
// generation zero's rows, not out of a tree. It exists because the runner
// requires a non-nil source to own and close, and it answers every content
// request with source.ErrNotInSource rather than pretending to hold bytes.
type copiedGenerationSource struct{ identity string }

func (s copiedGenerationSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	return nil, source.FileMeta{}, fmt.Errorf("%s: a copied generation holds no bytes: %w", path, source.ErrNotInSource)
}

func (s copiedGenerationSource) Stat(path string) (source.FileMeta, error) {
	return source.FileMeta{}, fmt.Errorf("%s: a copied generation holds no bytes: %w", path, source.ErrNotInSource)
}

func (s copiedGenerationSource) Walk(context.Context, func(source.FileMeta) error) error { return nil }

func (s copiedGenerationSource) Identity() string { return s.identity }

func (s copiedGenerationSource) Close() error { return nil }
