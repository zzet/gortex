package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/semantic"
)

// The sparse generation builder.
//
// A sparse generation is a payload generation that carries only the part of a
// repository one change touched. It is produced by the ordinary index
// pipeline, not by a second one: BeginPayloadGeneration hands back a store
// handle pinned to the new generation, a private Indexer is constructed over
// that handle, and IndexCtx runs against a content source narrowed to the
// files the generation must carry. Every node, edge and sidecar row the pass
// writes is generation-scoped by the handle, so the build needs no special
// write path and cannot leak into the base corpus.
//
// # Why the file set is wider than the change
//
// Reads through a derived handle are strictly generation-scoped: they do not
// fall through to the layer below. A file the generation does not carry is
// therefore invisible to the pass — the resolver cannot bind a call to a
// definition it cannot see, and the incremental prior-view reads come back
// empty. Rather than teach the pipeline to read through two generations, the
// build widens the generation's file set to the affected closure of the change
// (see builder_closure.go) and re-derives every file in it. The pass then runs
// exactly as a whole index of that file set does, and the result composes over
// the base because the file masks the build writes claim exactly the paths the
// payload lands at.
//
// # What the generation claims, and what it deliberately does not
//
//   - A replace mask for every path the generation's payload lands at, and a
//     delete mask for every path the change removed. The mask set is derived
//     from the payload after the pass, not from the plan, so the "every
//     identity a generation carries is one its own masks speak for"
//     precondition graphview's composition relies on holds by construction.
//   - A node tombstone for every identity the payload carries that lives in NO
//     file — the resolver's repo-scoped stubs for builtins, stdlib symbols and
//     external modules. Their ids have no path component, so no file mask can
//     reach them, and without the tombstone the generation's copy would
//     surface BESIDE the one still showing through from below instead of
//     replacing it. Nothing else is tombstoned: an identity that DOES live in
//     a claimed file is already replaced by that file's mask, and an identity
//     in an unclaimed file cannot have changed — a file whose resolution the
//     change could alter is, by the definition of the closure, inside the
//     closure and therefore claimed.
//   - An edge-source marker for the same class of identity on the adjacency
//     side, for the same reason: an edge whose source has no path is invisible
//     to the bounded out-edge readers unless the layer claims that source's
//     edge set, while the whole-graph readers return it either way. No marker
//     is written for a source that lives in a claimed file — the file mask
//     already replaces its whole outgoing set, so a marker would restate it,
//     which is why re-deriving every closure file in full is what removes the
//     need for the thin "retarget without re-deriving" marker entirely.
//
// The one case that escapes the argument above is a truncated closure: a
// dependent past the cap keeps reading the layer below and is stale. That is
// reported as a completeness fact and narrows the generation's local-resolution
// producer state, rather than being papered over with a tombstone that would be
// exactly as incomplete as the closure it came from.

// LayerChangeKind is what a change did to one path between the two states a
// layer spans.
type LayerChangeKind string

const (
	// LayerPathModified is a path present on both sides with different content.
	LayerPathModified LayerChangeKind = "modified"
	// LayerPathAdded is a path the target state holds and the base does not.
	LayerPathAdded LayerChangeKind = "added"
	// LayerPathDeleted is a path the base state holds and the target does not.
	LayerPathDeleted LayerChangeKind = "deleted"
)

// Valid reports whether k is a defined change kind.
func (k LayerChangeKind) Valid() bool {
	switch k {
	case LayerPathModified, LayerPathAdded, LayerPathDeleted:
		return true
	default:
		return false
	}
}

// LayerPathChange is one path's difference between the base state and the
// target state a generation is built for. Path is repo-relative and
// slash-separated — the namespace a content source is addressed by. A rename
// is not a kind of its own: it decomposes into a delete of the source path and
// an add of the destination, because that is what the two states differ by.
type LayerPathChange struct {
	Path string
	Kind LayerChangeKind
}

// GenerationIdentity is the catalog identity of the generation to build. It is
// passed through to BeginPayloadGeneration unchanged, where the fingerprint
// fields decide whether a second build of the same inputs adopts the
// generation already in flight instead of minting a second one.
type GenerationIdentity struct {
	OwnerKind      string
	GraphID        string
	LayerID        string
	CheckoutID     string
	GenerationKind string

	// BaseGenerationID is the layer beneath this one, 0 for the base corpus.
	BaseGenerationID int64

	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	ConfigHash           string
	ExtractorVersions    string
	ResolverVersion      string

	CreatedAt int64 // unix seconds; 0 stamps the wall clock
}

// LayerBase is the reader a build computes its affected closure against: the
// layer the generation will sit on.
//
// It is an interface rather than the store itself because the layer beneath is
// not always a store. A commit layer sits on the base corpus, which is a store
// handle; a working-tree layer sits on that corpus with the checkout's commit
// generation composed over it, which is a reader. Both answer the identity
// reads the closure walks and the batched file read it seeds from, and that is
// the whole of what a base has to do.
type LayerBase interface {
	graph.Reader
	GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node
}

// BuildRequest is one sparse generation build.
type BuildRequest struct {
	// Identity names the generation in the catalog.
	Identity GenerationIdentity

	// Base is the reader the affected closure is computed against: the layer
	// the generation will sit on. It is read, never written.
	Base LayerBase

	// Target serves the content of the state being built. The builder narrows
	// it to the generation's file set and does not close it — the caller owns
	// its lifetime.
	Target source.ContentSource

	// Changes is the per-path difference between the base state and the target.
	Changes []LayerPathChange

	// RootPath is the repository root every source path is relative to. It is
	// the currency the parse pipeline works in; under a content source no read
	// reaches it, but paths are still spelled against it.
	RootPath string

	// RepoPrefix, WorkspaceID and ProjectID are stamped onto every node the
	// generation carries, exactly as the live indexer stamps them.
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string

	// Enrich, when set, asks the build to run the semantic enrichment stage
	// over its own payload before the masks are written. Only a routed
	// checkout's working-tree build sets it — that is the only generation
	// whose RootPath is a working copy a language server can be rooted at.
	Enrich *EnrichmentStage

	// PrePublish runs after the payload, the masks and the producer states are
	// written and before the generation is published. Returning an error aborts
	// the publish and supersedes the generation, so a build whose inputs moved
	// underneath it never becomes readable. nil skips the step.
	PrePublish func(ctx context.Context, generationID int64) error
}

// EnrichmentStage names the checkout a build's enrichment pass describes.
type EnrichmentStage struct {
	// CheckoutID is the checkout whose working copy the pass reads, and the
	// dimension its completion marker is scoped by so one worktree's
	// enrichment never speaks for the primary's.
	CheckoutID string
	// Fingerprint identifies the working-tree state the pass ran over. It is
	// what the marker records in place of a commit sha, because a tree with
	// uncommitted edits in it is a state no commit names.
	Fingerprint string
}

// EnrichmentOutcome is what a build's enrichment stage did. It is the evidence
// behind the generation's lsp.* capability states, so every field is something
// the generation has to be able to say about itself rather than a metric.
type EnrichmentOutcome struct {
	// Requested reports that the build asked for the stage at all. A commit
	// layer and a ref view never do.
	Requested bool
	// Ran lists the languages a provider enriched, sorted.
	Ran []string
	// Starved lists the languages the global workspace cap could not admit,
	// sorted. They are not failures: the next build over this checkout asks
	// again, and by then a slot may have come free.
	Starved []string
	// Partial reports that a provider that ran was cut short at its deadline.
	Partial bool
	// Disabled reports that enrichment is switched off rather than unable to
	// run — the difference between waiting being pointless and waiting being
	// the fix.
	Disabled bool
	// Reason says why the stage did not enrich everything it could have.
	Reason string
}

// BuildReport is what one build did — and, as importantly, what it could not
// do. Every field a caller has to act on is a completeness fact, not a metric:
// ClosureTruncated says the generation may be missing a dependent it should
// have re-derived, and UnmaskedPayloadNodes says the payload carries a node at
// a path no mask claims.
type BuildReport struct {
	GenerationID int64

	// Coalesced reports that this call reused another caller's physical build
	// or a generation that became ready before it joined. Only reports with
	// Coalesced false represent physical payload work for metrics accounting.
	Coalesced bool

	// ChangedFiles, AddedFiles and DeletedFiles partition the request's change
	// set after validation.
	ChangedFiles int
	AddedFiles   int
	DeletedFiles int

	// ClosureFiles is how many files the affected-closure walk added on top of
	// the change set, and ClosurePaths lists them in sorted order.
	ClosureFiles int
	ClosurePaths []string

	// ClosureTruncated reports that the closure hit ClosureCap and was cut. The
	// generation is then knowingly incomplete: a dependent that fell past the
	// cap still reads the base layer's stale payload.
	ClosureTruncated bool
	ClosureCap       int

	// IndexedPaths is the repo-relative file set the pass actually walked, and
	// SourceBytes their total size in the target snapshot.
	IndexedPaths []string
	SourceBytes  int64

	// PlannedNotCovered lists planned paths the generation carries no payload
	// for — a path the walk admission rejected (unknown language, excluded,
	// oversized). They are left unmasked, so the base layer keeps showing
	// through at those paths.
	PlannedNotCovered []string

	// NodeCount and EdgeCount are what the generation carries.
	NodeCount int
	EdgeCount int

	// ReplaceMasks and DeleteMasks are the file-level claims written;
	// NodeTombstones and EdgeSourceMarkers the identity- and adjacency-level
	// ones. The latter two cover exactly the payload that lives in no file.
	ReplaceMasks      int
	DeleteMasks       int
	NodeTombstones    int
	EdgeSourceMarkers int

	// UnmaskedPayloadNodes counts the nodes the generation carries at no path —
	// the resolver's repo-scoped stubs. Each one is tombstoned, so the count is
	// how much of the payload the file masks could not reach on their own.
	UnmaskedPayloadNodes int

	// ContestedEdgeSources counts the pathless edge sources whose adjacency the
	// generation replaced while the layer below still carried edges from them
	// in files the generation does not claim. Those base edges are hidden by
	// the replacement — the one place a sparse generation knowingly drops a row
	// the layer below holds.
	ContestedEdgeSources int

	// Producers is exactly what was declared for the generation.
	Producers []store_sqlite.ProducerCompleteness

	// Enrichment is what the semantic enrichment stage did, zero when the
	// build did not ask for it.
	Enrichment EnrichmentOutcome

	// PlanningDuration is the wall time spent selecting the sparse file set.
	PlanningDuration time.Duration
	// Duration is the wall time of the whole build.
	Duration time.Duration
}

// SparseGenerationBuilder builds sparse payload generations over one store.
// It holds no per-build state and is safe to reuse.
type SparseGenerationBuilder struct {
	// Store is any handle on the database. Generations are begun and published
	// through it and the pass writes through the handle it hands back, so which
	// generation this handle is pinned to does not matter.
	Store *store_sqlite.Store
	// Registry is the parser registry the pass extracts with.
	Registry *parser.Registry
	// Config is the index configuration the pass runs under. It must be the
	// configuration the base corpus was indexed with, or the generation's
	// payload will not compose with it.
	Config config.IndexConfig
	// Logger receives the pass's own logging. nil is refused rather than
	// silently swapped for a no-op: a build that logs nowhere is undiagnosable.
	Logger *zap.Logger
	// Admissions is the live Indexer for this repository, whose process-wide
	// parse-admission gates the build shares so a background build cannot
	// double the daemon's bytes-in-flight budget. nil leaves the build on the
	// process defaults.
	Admissions *Indexer
	// Embedder is the optional embedding provider. nil declares the vector
	// capability disabled for the generation rather than leaving it unstated.
	Embedder embedding.Provider
	// Semantic runs the language-server enrichment stage for the builds that
	// ask for one. nil declares the lsp.* capabilities disabled for the
	// generation rather than leaving them unstated.
	Semantic *semantic.Manager

	// beforePayloadFlightJoin is a deterministic test barrier for the narrow
	// race where a caller adopts a building catalog generation after its leader
	// publishes but before joining the process-local flight. Production builders
	// leave it nil.
	beforePayloadFlightJoin func(generationID int64, adopted bool)
	// beforePhysicalPass is a deterministic test seam at the physical-flight
	// leader boundary. Production builders leave it nil.
	beforePhysicalPass func(generationID int64) error
}

const (
	generationAbandonTimeout = 5 * time.Second

	sparsePhysicalBuildStartedMessage   = "sparse generation physical build started"
	sparsePhysicalBuildCompletedMessage = "sparse generation physical build completed"
	sparsePhysicalBuildFailedMessage    = "sparse generation physical build failed"
	sparseReadyReuseMessage             = "sparse generation reused ready payload"
	sparseFollowerReuseMessage          = "sparse generation joined physical build"
)

func (b *SparseGenerationBuilder) logSparseBuildReuse(
	message string,
	generationID int64,
	reuseKind string,
	duration time.Duration,
	includeSuccess bool,
	success bool,
) {
	checked := b.Logger.Check(zap.DebugLevel, message)
	if checked == nil {
		return
	}
	fields := []zap.Field{
		zap.Int64("generation_id", generationID),
		zap.Bool("coalesced", true),
		zap.String("reuse_kind", reuseKind),
		zap.Duration("duration", duration),
	}
	if includeSuccess {
		fields = append(fields, zap.Bool("success", success))
	}
	checked.Write(fields...)
}

func (b *SparseGenerationBuilder) logSparsePhysicalBuildStart(report BuildReport, recovery bool) {
	if checked := b.Logger.Check(zap.InfoLevel, sparsePhysicalBuildStartedMessage); checked != nil {
		checked.Write(
			zap.Int64("generation_id", report.GenerationID),
			zap.Bool("coalesced", false),
			zap.Bool("adopted", recovery),
			zap.Bool("recovery", recovery),
			zap.Int("changed_files", report.ChangedFiles),
			zap.Int("added_files", report.AddedFiles),
			zap.Int("deleted_files", report.DeletedFiles),
			zap.Int("closure_files", report.ClosureFiles),
			zap.Bool("closure_truncated", report.ClosureTruncated),
			zap.Int("indexed_files", len(report.IndexedPaths)),
			zap.Int64("source_bytes", report.SourceBytes),
			zap.Duration("planning_duration", report.PlanningDuration),
		)
	}
}

func (b *SparseGenerationBuilder) logSparsePhysicalBuildTerminal(report BuildReport, success bool) {
	level := zap.InfoLevel
	message := sparsePhysicalBuildCompletedMessage
	if !success {
		level = zap.ErrorLevel
		message = sparsePhysicalBuildFailedMessage
	}
	if checked := b.Logger.Check(level, message); checked != nil {
		checked.Write(
			zap.Int64("generation_id", report.GenerationID),
			zap.Bool("coalesced", false),
			zap.Bool("success", success),
			zap.Duration("duration", report.Duration),
			zap.Int("node_count", report.NodeCount),
			zap.Int("edge_count", report.EdgeCount),
			zap.Int("replace_masks", report.ReplaceMasks),
			zap.Int("delete_masks", report.DeleteMasks),
			zap.Int("node_tombstones", report.NodeTombstones),
			zap.Int("edge_source_markers", report.EdgeSourceMarkers),
		)
	}
}

// Build produces one sparse payload generation and publishes it.
//
// It does not route the generation: publishing says the payload is whole and
// immutable, while pointing a checkout at it is a decision about the checkout
// and belongs to the caller. A published-but-unrouted generation is a legal
// resting state.
//
// Two builds naming the same layer and the same input fingerprints share one
// physical writer. The catalog aligns them on one generation ID; the store
// core's process-local flight gives exactly one caller runPass and publish
// ownership while followers wait with independently cancelable contexts.
type sparseBuildPreparation func(
	ctx context.Context, identity GenerationIdentity,
) (BuildRequest, func(), error)

type sparseBuildPreflightError struct {
	err error
}

func (e *sparseBuildPreflightError) Error() string { return e.err.Error() }
func (e *sparseBuildPreflightError) Unwrap() error { return e.err }

func markSparseBuildPreflightError(err error) error {
	if err == nil {
		return nil
	}
	var marked *sparseBuildPreflightError
	if errors.As(err, &marked) {
		return err
	}
	return &sparseBuildPreflightError{err: err}
}

func isSparseBuildPreflightError(err error) bool {
	var marked *sparseBuildPreflightError
	return errors.As(err, &marked)
}

func (b *SparseGenerationBuilder) Build(ctx context.Context, req BuildRequest) (int64, BuildReport, error) {
	started := time.Now()
	if err := b.validate(ctx, &req); err != nil {
		return 0, BuildReport{}, err
	}
	return b.buildPrepared(ctx, started, req.Identity, func(
		_ context.Context, identity GenerationIdentity,
	) (BuildRequest, func(), error) {
		req.Identity = identity
		return req, nil, nil
	})
}

// buildPrepared adopts or allocates a payload generation before invoking
// prepare. Exactly one flight leader pays for source construction, diffing and
// sparse planning; ready reusers return immediately and followers only wait.
func (b *SparseGenerationBuilder) buildPrepared(
	ctx context.Context,
	started time.Time,
	identity GenerationIdentity,
	prepare sparseBuildPreparation,
) (int64, BuildReport, error) {
	if prepare == nil {
		return 0, BuildReport{}, errors.New("indexer: sparse generation build needs preparation")
	}
	if err := b.validateBuildPrelude(ctx, &identity); err != nil {
		return 0, BuildReport{}, err
	}

	report := BuildReport{}
	flightStart, err := b.Store.BeginPayloadBuildFlight(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:            identity.OwnerKind,
		GraphID:              identity.GraphID,
		LayerID:              identity.LayerID,
		CheckoutID:           identity.CheckoutID,
		GenerationKind:       identity.GenerationKind,
		BaseGenerationID:     identity.BaseGenerationID,
		LowerViewFingerprint: identity.LowerViewFingerprint,
		TreeOID:              identity.TreeOID,
		ProvenanceCommitOID:  identity.ProvenanceCommitOID,
		ConfigHash:           identity.ConfigHash,
		ExtractorVersions:    identity.ExtractorVersions,
		ResolverVersion:      identity.ResolverVersion,
		CreatedAt:            identity.CreatedAt,
	}, b.beforePayloadFlightJoin)
	generationID := flightStart.GenerationID
	if err != nil {
		if generationID == 0 {
			return 0, BuildReport{}, fmt.Errorf("indexer: begin payload generation: %w", err)
		}
		report.GenerationID = generationID
		report.Coalesced = flightStart.Adopted
		report.Duration = time.Since(started)
		return generationID, report, fmt.Errorf("indexer: join payload build flight %d: %w", generationID, err)
	}
	handle := flightStart.Handle
	adopted := flightStart.Adopted
	flight := flightStart.Flight
	leader := flightStart.Leader
	ready := flightStart.Ready
	report.GenerationID = generationID
	if ready {
		report.Coalesced = true
		report.Duration = time.Since(started)
		b.logSparseBuildReuse(sparseReadyReuseMessage, generationID, "ready", report.Duration, false, true)
		return generationID, report, nil
	}
	if !leader {
		report.Coalesced = true
		waitStarted := time.Now()
		err = flight.Wait(ctx)
		waitDuration := time.Since(waitStarted)
		report.Duration = time.Since(started)
		b.logSparseBuildReuse(sparseFollowerReuseMessage, generationID, "in_flight", waitDuration, true, err == nil)
		if isSparseBuildPreflightError(err) {
			return 0, report, err
		}
		return generationID, report, err
	}

	report.Coalesced = false
	var (
		buildErr            error
		physicalStartLogged bool
	)
	defer func() {
		terminalReport := report
		terminalReport.Duration = time.Since(started)
		if recovered := recover(); recovered != nil {
			if !physicalStartLogged {
				b.logSparsePhysicalBuildStart(terminalReport, adopted)
			}
			panicErr := fmt.Errorf("indexer: payload generation %d build panicked: %v", generationID, recovered)
			b.logSparsePhysicalBuildTerminal(terminalReport, false)
			flight.Complete(panicErr)
			panic(recovered)
		}
		if !physicalStartLogged {
			b.logSparsePhysicalBuildStart(terminalReport, adopted)
		}
		b.logSparsePhysicalBuildTerminal(terminalReport, buildErr == nil)
		flight.Complete(buildErr)
	}()
	buildErr = func() error {
		// A physical build that dies part way must not leave a generation in the
		// only mutable state forever. Cleanup completes before followers wake, so
		// a retry cannot re-adopt payload the failed writer left behind.
		published := false
		defer func() {
			if !published {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generationAbandonTimeout)
				defer cancel()
				b.abandon(cleanupCtx, generationID)
			}
		}()

		req, cleanup, err := prepare(ctx, identity)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return markSparseBuildPreflightError(err)
		}
		if err := b.validate(ctx, &req); err != nil {
			return markSparseBuildPreflightError(err)
		}
		if req.Identity != identity {
			return markSparseBuildPreflightError(errors.New("indexer: sparse generation preparation changed its identity"))
		}

		planningStarted := time.Now()
		plan, plannedReport, err := b.planFileSetContext(ctx, req)
		plannedReport.PlanningDuration = time.Since(planningStarted)
		plannedReport.GenerationID = generationID
		plannedReport.Coalesced = false
		report = plannedReport
		if err != nil {
			return markSparseBuildPreflightError(err)
		}
		b.logSparsePhysicalBuildStart(report, adopted)
		physicalStartLogged = true

		// A newly allocated generation cannot carry payload yet. When the plan
		// has no files to index, the masks below completely describe a no-op or
		// deletion-only layer. A recovered adopted generation may carry partial
		// payload from a vanished writer, so it remains on the established
		// recovery path and is re-derived in full.
		if adopted || len(plan.indexed) > 0 {
			if err := b.runPass(ctx, req, plan, handle, &report); err != nil {
				return err
			}
		}
		// Enrichment runs before the masks so anything it adds to the payload is
		// covered by the claims derived from it, and before the producer states
		// so what it did is what they describe.
		b.runEnrichment(req, handle, &report)
		if err := b.writeMasks(req, plan, handle, &report); err != nil {
			return err
		}
		if err := b.declareProducers(req, handle, &report); err != nil {
			return err
		}
		if req.PrePublish != nil {
			if err := req.PrePublish(ctx, generationID); err != nil {
				return err
			}
		}
		if err := b.Store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix()); err != nil {
			return fmt.Errorf("indexer: publish generation %d: %w", generationID, err)
		}
		published = true
		return nil
	}()
	report.Duration = time.Since(started)
	if isSparseBuildPreflightError(buildErr) {
		return 0, report, buildErr
	}
	return generationID, report, buildErr
}

// validateBuildPrelude refuses malformed build identities before they can
// allocate a catalog generation or occupy a payload flight. The leader still
// validates the complete prepared request before planning.
func (b *SparseGenerationBuilder) validateBuildPrelude(
	ctx context.Context, identity *GenerationIdentity,
) error {
	switch {
	case b == nil || b.Store == nil:
		return errors.New("indexer: sparse generation builder needs a store")
	case b.Registry == nil:
		return errors.New("indexer: sparse generation builder needs a parser registry")
	case b.Logger == nil:
		return errors.New("indexer: sparse generation builder needs a logger")
	case ctx == nil:
		return errors.New("indexer: sparse generation build needs a context")
	case ctx.Err() != nil:
		return ctx.Err()
	case identity.OwnerKind == "":
		return errors.New("indexer: sparse generation build needs an owner kind")
	case identity.GenerationKind == "":
		return errors.New("indexer: sparse generation build needs a generation kind")
	}
	if identity.CreatedAt == 0 {
		identity.CreatedAt = time.Now().Unix()
	}
	return nil
}

// validate refuses a request that cannot produce a composable generation, and
// fills the defaults a caller may leave unset.
func (b *SparseGenerationBuilder) validate(ctx context.Context, req *BuildRequest) error {
	switch {
	case b == nil || b.Store == nil:
		return errors.New("indexer: sparse generation builder needs a store")
	case b.Registry == nil:
		return errors.New("indexer: sparse generation builder needs a parser registry")
	case b.Logger == nil:
		return errors.New("indexer: sparse generation builder needs a logger")
	case ctx == nil:
		return errors.New("indexer: sparse generation build needs a context")
	case ctx.Err() != nil:
		return ctx.Err()
	case req.Base == nil:
		return errors.New("indexer: sparse generation build needs a base reader")
	case req.Target == nil:
		return errors.New("indexer: sparse generation build needs a target content source")
	case req.RootPath == "":
		return errors.New("indexer: sparse generation build needs a repository root")
	case req.Identity.OwnerKind == "":
		return errors.New("indexer: sparse generation build needs an owner kind")
	case req.Identity.GenerationKind == "":
		return errors.New("indexer: sparse generation build needs a generation kind")
	}
	for _, change := range req.Changes {
		if !change.Kind.Valid() {
			return fmt.Errorf("indexer: change on %q has unknown kind %q", change.Path, change.Kind)
		}
		if change.Path == "" || path.IsAbs(change.Path) {
			return fmt.Errorf("indexer: change path %q is not repo-relative", change.Path)
		}
	}
	if req.Identity.CreatedAt == 0 {
		req.Identity.CreatedAt = time.Now().Unix()
	}
	return nil
}

// buildPlan is the file set one build walks, plus the paths it claims deleted.
type buildPlan struct {
	// indexed is the repo-relative file set the pass walks, sorted.
	indexed []string
	// deleted is the repo-relative set the generation claims removed, sorted.
	deleted []string
}

// planFileSet turns the change set into the file set the pass walks: the
// changed and added paths the target still holds, plus the affected closure of
// the whole change set, minus anything the change deleted.
//
// A path the change calls modified or added but the target source does not
// hold is a contradiction between the caller's diff and the content it handed
// over, and is refused: silently dropping it would produce a generation that
// masks nothing at a path the caller believes it replaced.
func (b *SparseGenerationBuilder) planFileSet(req BuildRequest) (buildPlan, BuildReport, error) {
	return b.planFileSetContext(context.Background(), req)
}

func (b *SparseGenerationBuilder) planFileSetContext(
	ctx context.Context,
	req BuildRequest,
) (buildPlan, BuildReport, error) {
	var report BuildReport
	if err := ctx.Err(); err != nil {
		return buildPlan{}, report, err
	}
	deleted := make(map[string]struct{})
	present := make(map[string]struct{})
	for _, change := range req.Changes {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		clean := path.Clean(change.Path)
		switch change.Kind {
		case LayerPathDeleted:
			report.DeletedFiles++
			deleted[clean] = struct{}{}
		case LayerPathAdded:
			report.AddedFiles++
			present[clean] = struct{}{}
		case LayerPathModified:
			report.ChangedFiles++
			present[clean] = struct{}{}
		}
	}
	// A path that is both added and deleted in one change set is the target's
	// path: a rename destination whose source happened to share the name, or a
	// caller that reported both halves of a replace. The target holds it, so
	// the surviving claim is the add.
	for p := range present {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		delete(deleted, p)
	}
	for p := range present {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		if _, err := req.Target.Stat(p); err != nil {
			return buildPlan{}, report, fmt.Errorf(
				"indexer: change set names %q as present but the target source does not hold it: %w", p, err)
		}
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
	}

	closure, err := b.affectedClosureContext(ctx, req, present, deleted, &report)
	if err != nil {
		return buildPlan{}, report, err
	}
	indexedSet := make(map[string]struct{}, len(present)+len(closure))
	for p := range present {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		indexedSet[p] = struct{}{}
	}
	for _, p := range closure {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		if _, gone := deleted[p]; gone {
			continue
		}
		indexedSet[p] = struct{}{}
	}

	var plan buildPlan
	for p := range indexedSet {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		plan.indexed = append(plan.indexed, p)
	}
	for p := range deleted {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		plan.deleted = append(plan.deleted, p)
	}
	sort.Strings(plan.indexed)
	sort.Strings(plan.deleted)
	if err := ctx.Err(); err != nil {
		return buildPlan{}, report, err
	}
	// A path reported deleted that the target still holds was folded into the
	// present set above; the surviving list is what the generation claims.
	report.DeletedFiles = len(plan.deleted)

	for _, p := range plan.indexed {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		meta, statErr := req.Target.Stat(p)
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		if statErr == nil && meta.Size > 0 {
			report.SourceBytes += meta.Size
		}
	}
	report.IndexedPaths = plan.indexed
	return plan, report, nil
}

// runPass constructs the private Indexer over the generation handle and runs
// one whole-tree index against the narrowed source.
//
// The Indexer is deliberately built the ordinary way. New binds the resolver
// and the search backend to the handle it is given, so nothing has to be
// redirected afterwards — the shadow-swap's SetGraph hazard exists only for a
// handle swapped in after construction. What is shared with the live indexer
// is the process-wide admission budget and nothing else: the mutation-owner
// link is left unset so the build never registers with a MultiIndexer, and
// every per-instance cache (mtimes, contracts, trigram, prepared parses) is
// this build's alone and dies with it.
func (b *SparseGenerationBuilder) runPass(
	ctx context.Context,
	req BuildRequest,
	plan buildPlan,
	handle *store_sqlite.Store,
	report *BuildReport,
) error {
	if b.beforePhysicalPass != nil {
		if err := b.beforePhysicalPass(report.GenerationID); err != nil {
			return err
		}
	}
	idx := New(handle, b.Registry, b.Config, b.Logger)
	defer idx.Close()

	idx.SetRepoPrefix(req.RepoPrefix)
	idx.SetWorkspaceID(req.WorkspaceID)
	idx.SetProjectID(req.ProjectID)
	if b.Embedder != nil {
		idx.SetEmbedder(b.Embedder)
	}
	if b.Admissions != nil {
		idx.shadowAdmission = b.Admissions.shadowAdmission
		idx.indexMemoryAdmission = b.Admissions.indexMemoryAdmission
		idx.parseAdmission.Store(b.Admissions.parseAdmission.Load())
		idx.nativeParseAdmission.Store(b.Admissions.nativeParseAdmission.Load())
	}
	idx.SetContentSource(newFileSetSource(req.Target, plan.indexed))

	result, err := idx.IndexCtx(ctx, req.RootPath)
	if err != nil {
		return fmt.Errorf("indexer: index generation payload: %w", err)
	}
	if result != nil {
		report.NodeCount = result.NodeCount
		report.EdgeCount = result.EdgeCount
	}
	return nil
}

// runEnrichment runs the semantic enrichment stage over the generation's own
// payload, against the checkout root the build read its bytes from.
//
// It records what happened and never fails the build. The payload is whole
// with or without it: enrichment adds facts on top of a generation that is
// already complete as a description of the tree, so a language server that
// could not be started, a workspace the global cap refused and a pass that
// broke all cost the generation the same thing — a capability it must not
// claim. Losing the whole build over that would be a worse answer than a
// published generation that says its lsp.* facts are not there yet.
//
// The graph it enriches is the generation handle, not the corpus. Reads
// through it are generation-scoped, so the census that picks languages sees
// only what this build carries and the edges the providers land are written
// into the generation rather than into the tree everyone else reads.
func (b *SparseGenerationBuilder) runEnrichment(
	req BuildRequest,
	handle *store_sqlite.Store,
	report *BuildReport,
) {
	if req.Enrich == nil || !enrichesWorkingCopy(req.Identity) {
		return
	}
	out := &report.Enrichment
	out.Requested = true
	if b.Semantic == nil {
		out.Disabled = true
		out.Reason = "no semantic enrichment manager is installed"
		return
	}
	pass, err := b.Semantic.EnrichCheckout(handle, semantic.CheckoutEnrichRequest{
		RepoPrefix:       req.RepoPrefix,
		CheckoutID:       req.Enrich.CheckoutID,
		Root:             req.RootPath,
		Fingerprint:      req.Enrich.Fingerprint,
		MinLanguageNodes: semantic.EnrichmentAdmissionFloor(),
	})
	if err != nil {
		out.Reason = err.Error()
		b.Logger.Warn("indexer: the generation's enrichment stage failed",
			zap.String("checkout", req.Enrich.CheckoutID),
			zap.String("root", req.RootPath),
			zap.Error(err))
		return
	}
	out.Ran, out.Starved = pass.Ran, pass.Starved
	out.Partial, out.Disabled, out.Reason = pass.Partial, pass.Disabled, pass.Reason
}

// writeMasks derives the generation's ownership claims from the payload it
// actually carries, then writes them.
//
// Deriving from the payload rather than from the plan is what makes the
// composition's precondition — every identity a generation carries is one its
// own masks speak for — hold by construction instead of by agreement between
// two lists. Three claims come out of it, and the third and fourth exist for
// exactly one reason each.
//
//   - File masks: replace for every path the payload lands at, delete for
//     every path the change removed. This covers everything that lives in a
//     file, which is every symbol, parameter and file node.
//   - Node tombstones for the identities that live in NO file: the resolver's
//     repo-scoped stubs for builtins, stdlib symbols and external modules.
//     A file mask cannot reach them — their id has no path component to key on
//     — so without a tombstone the generation's copy would surface BESIDE the
//     copy still showing through from the layer below rather than replacing
//     it. The tombstone is what makes the layer speak for the identity; the
//     generation carries a node under it, so the composition answers with the
//     generation's copy instead of hiding it.
//   - Edge-source markers for the same class of identity on the adjacency
//     side: an edge whose source has no path is invisible to the bounded
//     out-edge readers unless the layer claims that source's edge set, even
//     though the whole-graph readers already return it. The marker is what
//     keeps those two answers the same.
//
// No claim is made for an identity that DOES live in a file the generation
// claims — its file mask already replaces both the node and its whole outgoing
// edge set, so a tombstone or a marker would only restate it. And no claim is
// made about a file outside the generation's set: such a file's payload is
// unchanged by construction, unless the closure was truncated, which is
// reported as a completeness fact rather than guessed at here.
func (b *SparseGenerationBuilder) writeMasks(
	req BuildRequest,
	plan buildPlan,
	handle *store_sqlite.Store,
	report *BuildReport,
) error {
	covered := make(map[string]struct{})
	rows, err := handle.FileMetasForRepo(req.RepoPrefix)
	if err != nil {
		return fmt.Errorf("indexer: read generation file inventory: %w", err)
	}
	for _, row := range rows {
		if row.FilePath != "" {
			covered[row.FilePath] = struct{}{}
		}
	}
	nodes := handle.AllNodes()
	for _, node := range nodes {
		if node != nil && node.FilePath != "" {
			covered[node.FilePath] = struct{}{}
		}
	}

	masks := make([]store_sqlite.FileMask, 0, len(covered)+len(plan.deleted))
	for graphPath := range covered {
		masks = append(masks, store_sqlite.FileMask{
			RepoPrefix: req.RepoPrefix, FilePath: graphPath, Mode: store_sqlite.OwnershipReplace,
		})
		report.ReplaceMasks++
	}
	for _, rel := range plan.deleted {
		graphPath := builderGraphPath(req.RepoPrefix, rel)
		if _, carried := covered[graphPath]; carried {
			return fmt.Errorf(
				"indexer: generation carries payload at %q while claiming it deleted", graphPath)
		}
		masks = append(masks, store_sqlite.FileMask{
			RepoPrefix: req.RepoPrefix, FilePath: graphPath, Mode: store_sqlite.OwnershipDelete,
		})
		report.DeleteMasks++
	}
	sort.Slice(masks, func(i, j int) bool { return masks[i].FilePath < masks[j].FilePath })
	if err := handle.SetFileMasks(masks); err != nil {
		return fmt.Errorf("indexer: write generation file masks: %w", err)
	}

	var tombstones []string
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		if _, claimed := covered[builderMaskKey(node.ID)]; claimed {
			continue
		}
		tombstones = append(tombstones, node.ID)
		report.UnmaskedPayloadNodes++
	}
	sort.Strings(tombstones)
	if err := handle.SetNodeTombstones(tombstones); err != nil {
		return fmt.Errorf("indexer: write generation node tombstones: %w", err)
	}
	report.NodeTombstones = len(tombstones)

	markers, contested := b.unclaimedEdgeSources(req, handle, covered)
	if err := handle.SetEdgeSourceMasks(markers); err != nil {
		return fmt.Errorf("indexer: write generation edge-source masks: %w", err)
	}
	report.EdgeSourceMarkers = len(markers)
	report.ContestedEdgeSources = contested

	for _, rel := range plan.indexed {
		if _, ok := covered[builderGraphPath(req.RepoPrefix, rel)]; !ok {
			report.PlannedNotCovered = append(report.PlannedNotCovered, rel)
		}
	}
	sort.Strings(report.PlannedNotCovered)
	return nil
}

// unclaimedEdgeSources returns the replacement markers for every edge source
// the generation carries that lives at no path its file masks claim, and the
// number of those the layer below also carries edges from in files the
// generation does not claim.
//
// That second number is a completeness fact and not a warning about nothing.
// A pathless source's adjacency is aggregated across every file that
// contributed to it, and the marker replaces the whole set — so a base edge
// from such a source that originated in an untouched file is hidden by the
// generation's claim. Marking anyway is the lesser wrong: without the marker,
// the bounded out-edge readers and the whole-graph readers give different
// answers for the same node, and an inconsistent view is worse to reason about
// than one that names what it dropped.
func (b *SparseGenerationBuilder) unclaimedEdgeSources(
	req BuildRequest,
	handle *store_sqlite.Store,
	covered map[string]struct{},
) ([]store_sqlite.EdgeSourceMask, int) {
	unclaimed := make(map[string]struct{})
	for _, edge := range handle.AllEdges() {
		if edge == nil || edge.From == "" {
			continue
		}
		if _, claimed := covered[builderMaskKey(edge.From)]; claimed {
			continue
		}
		unclaimed[edge.From] = struct{}{}
	}
	if len(unclaimed) == 0 {
		return nil, 0
	}
	sources := make([]string, 0, len(unclaimed))
	for id := range unclaimed {
		sources = append(sources, id)
	}
	sort.Strings(sources)

	contested := 0
	baseEdges := req.Base.GetOutEdgesByNodeIDs(sources)
	masks := make([]store_sqlite.EdgeSourceMask, 0, len(sources))
	for _, id := range sources {
		for _, edge := range baseEdges[id] {
			if edge == nil {
				continue
			}
			if _, claimed := covered[edge.FilePath]; !claimed {
				contested++
				break
			}
		}
		masks = append(masks, store_sqlite.EdgeSourceMask{
			SourceID: id, Mode: store_sqlite.OwnershipReplace,
		})
	}
	return masks, contested
}

// builderMaskKey is the path a file mask must claim for the composition to
// speak for an identity. It mirrors the layer's own rule: a symbol id carries
// its file before the "::" separator, a file node's id IS its path, and an id
// with a separator but no path component — a repo-scoped resolver stub —
// yields a key no file mask can hold, which is what the tombstone and marker
// claims exist for.
func builderMaskKey(id string) string {
	if file := graph.IDFile(id); file != "" {
		return file
	}
	return id
}

// declareProducers records how complete each capability is for this
// generation. A capability nothing is said about is inherited from the layer
// below, so silence is a claim too — every capability this build narrows is
// named here.
func (b *SparseGenerationBuilder) declareProducers(
	req BuildRequest,
	handle *store_sqlite.Store,
	report *BuildReport,
) error {
	// Two admission rules read the working tree rather than the state the pass
	// describes, so both are inert under a content source and both are named
	// here rather than left as a coverage the generation never had. The
	// hierarchical ignore matcher reads per-directory ignore files off disk,
	// and a source serves a revision whose ignore files may differ from the
	// checkout's — or not be on disk at all. The untracked-asset gate asks
	// `git ls-files` of the checkout, which describes a different tree.
	const configReason = "per-directory ignore files and the untracked-asset gate " +
		"are not applied under a content source"

	vector := store_sqlite.ProducerCompleteness{
		Producer: string(graphview.CapSearchVector),
		State:    store_sqlite.ProducerStateDisabledByConfig,
		Reason:   "no embedding provider is configured for the build",
	}
	if b.Embedder != nil {
		vector.State = store_sqlite.ProducerStateComplete
		vector.Reason = ""
	}

	// Near-duplicate detection is a whole-CORPUS statistic: the boilerplate
	// filter is derived from shingle frequencies across every body in the
	// corpus, the LSH bands are populated from that same population, and a
	// diffusion pass runs over the resulting clone graph. A sparse generation
	// carries part of a corpus by construction, so the pass sees a different
	// population and can emit a different relation for the very same bodies.
	// Two things follow, and the closure can only fix one of them: a pair the
	// BASE already records joins the generation whole, because the counterpart
	// file is reachable along the recorded edge — but a pair the change
	// CREATES, between a claimed file and an untouched one, has no base edge
	// to walk, so the generation replaces the claimed file's payload without
	// the counterpart's half of the pair ever being written.
	similarity := store_sqlite.ProducerCompleteness{
		Producer: string(graphview.CapSimilarity),
		State:    store_sqlite.ProducerStateDisabledByConfig,
		Reason:   "near-duplicate detection is switched off for the build",
	}
	if b.Config.Coverage.IsEnabled("clones") {
		similarity.State = store_sqlite.ProducerStateIncomplete
		similarity.Reason = "near-duplicate detection ranks bodies against a corpus; " +
			"a sparse generation ranks them against its file set"
	}

	// Literal and regex search is answered over a working copy on disk rather
	// than out of the generation: the checkout's coordinator builds a trigram
	// index over its checkout root and searches that. A generation describing
	// a checkout therefore serves the capability whole, and one describing a
	// committed tree nobody has checked out cannot serve it at all — there is
	// no root to index, and the canonical checkout holds a different tree.
	text := store_sqlite.ProducerCompleteness{
		Producer: string(graphview.CapSearchText),
		State:    store_sqlite.ProducerStateComplete,
	}
	if !servesTextSearch(req.Identity) {
		text.State = store_sqlite.ProducerStateUnavailable
		text.Reason = "a committed tree has no working copy to run a text search over"
	}

	rows := []store_sqlite.ProducerCompleteness{
		{Producer: string(graphview.CapSourceSnapshot), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapSourceConfig), State: store_sqlite.ProducerStateComplete, Reason: configReason},
		{Producer: string(graphview.CapSyntaxGraph), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapResolutionLocal), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapIncomingEdges), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapSearchSymbols), State: store_sqlite.ProducerStateComplete},
		{Producer: string(graphview.CapSearchContent), State: store_sqlite.ProducerStateComplete},
		vector,
		similarity,
		text,
		{
			Producer: string(graphview.CapResolutionCrossRepo),
			State:    store_sqlite.ProducerStateIncomplete,
			Reason:   "a sparse generation is resolved within one repository",
		},
	}
	lsp := lspProducerRow(req.Identity, report.Enrichment)
	for _, capability := range []graphview.CapabilityID{
		graphview.CapLSPReferences, graphview.CapLSPDiagnostics, graphview.CapLSPHover,
		graphview.CapLSPRename, graphview.CapLSPCodeActions,
	} {
		lsp.Producer = string(capability)
		rows = append(rows, lsp)
	}
	if report.ClosureTruncated {
		// The syntax and resolution the generation carries are whole for the
		// files it re-derived; what is not whole is the set of files it should
		// have re-derived. A file past the cap keeps reading a stale resolution
		// from the layer below — and it is also a file whose references into
		// this generation's symbols were never re-derived, so the reverse index
		// is missing exactly the edges that file would have contributed. Both
		// producers are narrowed; the generation is published either way, and
		// what a knowingly incomplete capability is worth is the reader's call.
		truncated := fmt.Sprintf(
			"the affected closure was truncated at %d files; files past the cap were not re-resolved",
			report.ClosureCap)
		for i := range rows {
			switch rows[i].Producer {
			case string(graphview.CapResolutionLocal), string(graphview.CapIncomingEdges):
				rows[i].State = store_sqlite.ProducerStateIncomplete
				rows[i].Reason = truncated
			}
		}
	}
	if err := handle.SetProducerStates(rows); err != nil {
		return fmt.Errorf("indexer: declare producers: %w", err)
	}
	report.Producers = rows
	return nil
}

// abandon moves a generation whose build did not reach publish out of the
// building state, so nothing adopts it and a sweep can collect it.
//
// A generation that has already left building is left alone: the pre-publish
// step supersedes the one it refuses, and re-stating that verdict as a failure
// would lose the distinction between "this build broke" and "this build's
// inputs moved".
func (b *SparseGenerationBuilder) abandon(ctx context.Context, generationID int64) {
	err := b.Store.Catalog().SetViewGenerationState(
		ctx, generationID, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding)
	if err != nil && !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		b.Logger.Warn("indexer: could not mark an abandoned generation failed",
			zap.Int64("generation", generationID), zap.Error(err))
	}
}

// supersede records that a generation must not be read, without publishing it.
//
// MarkPayloadGenerationSuperseded only accepts a generation that already
// reached ready, and a build that aborts before publishing never does — so the
// transition is made through the catalog's guarded setter instead, from the
// building state the abort leaves it in.
func (b *SparseGenerationBuilder) supersede(ctx context.Context, generationID int64) error {
	return b.Store.Catalog().SetViewGenerationState(
		ctx, generationID, store_sqlite.ViewGenerationSuperseded, store_sqlite.ViewGenerationBuilding)
}

// derivedGenerationTarget reports whether a store handle is pinned to a
// derived payload generation rather than to the base corpus.
//
// The bulk index path asks it because it has one step a derived generation
// must never take: the in-memory shadow's drain evicts the repository's
// persisted rows before its INSERT-only bulk load, and that eviction spans
// every generation. That is right for a re-track of the base corpus and a wipe
// of the corpus a sparse generation exists to leave alone.
//
// The question is put to the handle rather than to a flag the builder sets, so
// any store that grows the concept answers it without a second switch.
func derivedGenerationTarget(g graph.Store) bool {
	scoped, ok := g.(interface{ ViewGeneration() int64 })
	return ok && scoped.ViewGeneration() > 0
}

// lspProducerRow is what a generation says about the language-server
// capabilities, from what its enrichment stage actually did.
//
// The one state that is never reachable here is a false complete: every case
// that did not enrich the whole payload lands on incomplete or
// disabled_by_config, and the two are not interchangeable — disabled means
// waiting will not help, incomplete with a reason means the next working-tree
// build over this checkout tries again.
//
// A starved or evicted checkout takes the incomplete arm rather than building.
// A generation with a producer still building cannot be published at all (the
// catalog refuses it, and rightly: nothing would ever settle the row on a
// sealed payload), so building would cost the checkout its whole view instead
// of one capability. What incomplete says is also what is true of the composed
// view: the layers below carry the primary's enrichment, so an lsp query still
// answers for every file this build did not claim and answers thinly for the
// ones it did.
func lspProducerRow(
	identity GenerationIdentity, out EnrichmentOutcome,
) store_sqlite.ProducerCompleteness {
	disabled := func(reason string) store_sqlite.ProducerCompleteness {
		return store_sqlite.ProducerCompleteness{
			State: store_sqlite.ProducerStateDisabledByConfig, Reason: reason,
		}
	}
	incomplete := func(reason string) store_sqlite.ProducerCompleteness {
		return store_sqlite.ProducerCompleteness{
			State:  store_sqlite.ProducerStateIncomplete,
			Reason: reason + "; the next build over this checkout runs it again",
		}
	}
	switch {
	case !enrichesWorkingCopy(identity):
		return disabled("a committed tree has no working copy for a language server to be rooted at")
	case !out.Requested:
		return disabled("language-server enrichment does not run in a sparse generation build")
	case out.Disabled:
		return disabled(out.Reason)
	case len(out.Ran) == 0:
		return incomplete("no language server enriched this checkout: " + enrichmentReason(out))
	case out.Partial:
		return incomplete("a language server was cut short before it finished this checkout")
	case len(out.Starved) > 0:
		return incomplete("the language-server workspace cap refused " + strings.Join(out.Starved, ", "))
	default:
		return store_sqlite.ProducerCompleteness{State: store_sqlite.ProducerStateComplete}
	}
}

// enrichmentReason renders why a stage that ran nothing ran nothing, so the
// producer row names a cause rather than only an absence.
func enrichmentReason(out EnrichmentOutcome) string {
	if out.Reason != "" {
		return out.Reason
	}
	return "no provider covered its languages"
}

// enrichesWorkingCopy reports whether a generation describes a working copy a
// language server can be rooted at. A checkout's layers do — the coordinator
// builds them from a directory on disk; a ref view describes a committed tree
// nobody has checked out, so there is no root to enrich from.
func enrichesWorkingCopy(identity GenerationIdentity) bool {
	return identity.OwnerKind != refViewOwnerKind
}

// servesTextSearch reports whether a generation describes a state some working
// copy holds on disk. A checkout's layers describe a checkout, whose
// coordinator indexes its root; a ref view describes a committed tree nobody
// has checked out, and nothing on disk holds it.
func servesTextSearch(identity GenerationIdentity) bool {
	return identity.OwnerKind != refViewOwnerKind
}

// builderGraphPath prefixes a repo-relative slash path into the graph
// namespace masks and node file paths are keyed by.
func builderGraphPath(repoPrefix, rel string) string {
	if repoPrefix == "" {
		return rel
	}
	return repoPrefix + "/" + rel
}

// builderRelPath is builderGraphPath's inverse. owned is false when the graph
// path belongs to another repository, which is the only safe answer.
func builderRelPath(repoPrefix, graphPath string) (string, bool) {
	if repoPrefix == "" {
		return graphPath, graphPath != ""
	}
	rel, trimmed := strings.CutPrefix(graphPath, repoPrefix+"/")
	return rel, trimmed && rel != ""
}

// fileSetSource narrows a content source to a fixed set of repo-relative
// paths. It is what makes a whole-tree index pass produce a sparse generation:
// the pipeline walks and reads exactly the plan, through the ordinary walk and
// read seams, without knowing it is looking at part of a tree.
//
// A read outside the set is refused rather than served. The plan is what the
// generation's masks are derived against, so content that leaked in past it
// would land in the payload at a path nothing claims.
type fileSetSource struct {
	inner source.ContentSource
	keep  map[string]struct{}
	paths []string
}

var _ source.ContentSource = (*fileSetSource)(nil)

// newFileSetSource returns src narrowed to paths.
func newFileSetSource(src source.ContentSource, paths []string) *fileSetSource {
	keep := make(map[string]struct{}, len(paths))
	canonical := make([]string, 0, len(paths))
	for _, p := range paths {
		p = path.Clean(p)
		if _, exists := keep[p]; exists {
			continue
		}
		keep[p] = struct{}{}
		canonical = append(canonical, p)
	}
	sort.Strings(canonical)
	return &fileSetSource{inner: src, keep: keep, paths: canonical}
}

// Identity distinguishes the narrowed source from the whole one, so two builds
// over the same revision with different plans are not confused for each other.
func (s *fileSetSource) Identity() string {
	return fmt.Sprintf("%s#files:%d", s.inner.Identity(), len(s.keep))
}

// Close releases the underlying source. The builder does not call it — the
// caller owns the target source's lifetime — but the interface requires it and
// leaving it unimplemented would strand a caller that does hold the wrapper.
func (s *fileSetSource) Close() error { return s.inner.Close() }

func (s *fileSetSource) holds(p string) bool {
	_, ok := s.keep[path.Clean(p)]
	return ok
}

func (s *fileSetSource) Stat(p string) (source.FileMeta, error) {
	if !s.holds(p) {
		return source.FileMeta{}, fmt.Errorf("%s: outside the generation's file set: %w", p, source.ErrNotInSource)
	}
	return s.inner.Stat(p)
}

func (s *fileSetSource) Open(p string) (io.ReadCloser, source.FileMeta, error) {
	if !s.holds(p) {
		return nil, source.FileMeta{}, fmt.Errorf("%s: outside the generation's file set: %w", p, source.ErrNotInSource)
	}
	return s.inner.Open(p)
}

func (s *fileSetSource) Walk(ctx context.Context, fn func(source.FileMeta) error) error {
	if fn == nil {
		return errors.New("indexer: file set source needs a walk function")
	}
	for _, p := range s.paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := s.inner.Stat(p)
		if err != nil {
			if errors.Is(err, source.ErrNotInSource) {
				continue
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}
