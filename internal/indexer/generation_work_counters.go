package indexer

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/semantic"
)

// GenerationWorkCounters is the physical work one sparse generation build did,
// kept in three separate accounts that must never be charged to one another:
//
//   - extraction: the source files the index pass opened to parse
//     (ParserInputs / ParserInputPaths / ParserInputBytes);
//   - admission: stats and non-extraction opens against the target content
//     source — planning's presence checks, the pass's own walk stats, and
//     manifest reads (AdmissionStats / AdmissionOpens / AdmissionOpenBytes);
//   - compiler context: Go package loads by the semantic enrichment stage,
//     reported separately by package and file when a provider measured them
//     (CompilerContext).
//
// The physical store work comes from the writer itself (Store), never from a
// composed reader, and the payload a later build could have reused is stated
// explicitly (ReusedPriorPayloadFiles) rather than inferred from elapsed time.
//
// It is diagnostic evidence: nothing may branch on it to decide what to build.
type GenerationWorkCounters struct {
	GenerationID         int64
	BaseGenerationID     int64
	GenerationKind       string
	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	// Coalesced mirrors BuildReport.Coalesced: a coalesced report did no
	// physical payload work and carries plan sizes only.
	Coalesced bool

	// Plan sizes, as planFileSetContext decided them.
	PlanChanged  int
	PlanAdded    int
	PlanDeleted  int
	PlanIndexed  int
	PlanContext  int
	PlanClosure  int
	PlanOutput   int // indexed minus context: the files the generation speaks for
	PlanSrcBytes int64

	// Extraction parser inputs: distinct source paths the pass opened through
	// its narrowed file set, their total size, and the raw open count (a path
	// opened twice counts once in ParserInputs and twice in ParserInputOpens).
	ParserInputs     int
	ParserInputOpens int
	ParserInputBytes int64
	ParserInputPaths []string

	// Admission / fingerprint reads against the target source that were not
	// extraction: every Stat (planning presence checks, the pass's walk), and
	// every Open outside the narrowed file set (manifests and other context
	// the pipeline reads directly from the target).
	AdmissionStats     int
	AdmissionOpens     int
	AdmissionOpenBytes int64

	// CompilerContext is the Go compiler / package-context work, kept apart
	// from extraction.
	CompilerContext CompilerContextCounters

	// ReusedPriorPayloadFiles is the number of files whose payload this build
	// took physically from a prior generation instead of re-deriving it.
	// RebuiltFiles is the number it re-derived (parsed) instead.
	ReusedPriorPayloadFiles int
	RebuiltFiles            int

	// Context separation outcome: closure files parsed and withheld because
	// their re-derivation matched the layer below, and those retained because
	// it did not.
	ContextWithheldFiles  int
	ContextRetainedFiles  int
	ContextWithdrawnNodes int
	ContextWithdrawnEdges int
	ContextHeldInMemory   bool

	ReplaceMasks   int
	DeleteMasks    int
	NodeTombstones int
	EdgeSources    int

	// Store is the writer-side physical accounting for this generation's
	// bulk window; StoreMeasured is false when no window was recorded.
	Store         store_sqlite.GenerationWriteCounters
	StoreMeasured bool

	// Phases are the wall times of the build's stages in execution order.
	Phases []GenerationPhase

	// Working-tree chain accounting: the parent a delta build stood on (0 =
	// direct), the published chain depth, how many manifest rows were stored,
	// and the reason a chained attempt for the same state was refused (empty
	// when none was). ChainFallbackReasons counts fallback reasons by code.
	ParentGenerationID     int64
	ChainDepth             int
	ManifestEntriesWritten int
	ChainFallbackReason    string
	ChainFallbackReasons   map[string]int

	mu       sync.Mutex
	parsed   map[string]int64
	lastMark time.Time
}

// CompilerContextCounters is Go compiler / package-context work. Measured is
// false when no provider reported package and file counts; the zero counts
// then say "not measured", not "none".
type CompilerContextCounters struct {
	Requested bool
	Ran       []string
	Measured  bool
	Packages  int
	Files     int
	// Loads counts the type-checking package loads (2 after a handle-rooted
	// load fell back to the whole module).
	Loads int
	// Scope is "full" (every package of the module) or "handle_roots" (the
	// packages of the generation's Go files); ScopeReason is the fallback or
	// adjustment code, empty when none applied.
	Scope       string
	ScopeReason string
	// LoadMs is the type-checking loads' wall time and IndexMs the dependency
	// metadata index's; IndexCached reports that the index needed no `go list`.
	LoadMs      int64
	IndexMs     int64
	IndexCached bool
	// Cache is the per-checkout closure and type-check state's work (closure
	// hits and misses, `go list` time spent and saved, retained state size);
	// nil when the build did not use it.
	Cache  *semantic.CompilerCacheStats
	Reason string
}

// GenerationPhase is one named stage's wall time.
type GenerationPhase struct {
	Name     string
	Duration time.Duration
}

func newGenerationWorkCounters(req BuildRequest) *GenerationWorkCounters {
	return &GenerationWorkCounters{
		BaseGenerationID:     req.Identity.BaseGenerationID,
		GenerationKind:       req.Identity.GenerationKind,
		LowerViewFingerprint: req.Identity.LowerViewFingerprint,
		TreeOID:              req.Identity.TreeOID,
		ProvenanceCommitOID:  req.Identity.ProvenanceCommitOID,
		parsed:               map[string]int64{},
	}
}

// recordPlan copies the plan sizes the planner decided.
func (w *GenerationWorkCounters) recordPlan(plan buildPlan, report BuildReport, planning time.Duration) {
	if w == nil {
		return
	}
	w.PlanChanged, w.PlanAdded, w.PlanDeleted = report.ChangedFiles, report.AddedFiles, report.DeletedFiles
	w.PlanIndexed, w.PlanContext, w.PlanClosure = len(plan.indexed), len(plan.context), report.ClosureFiles
	w.PlanOutput = len(plan.indexed) - len(plan.context)
	w.PlanSrcBytes = report.SourceBytes
	w.Phases = append(w.Phases, GenerationPhase{Name: "plan", Duration: planning})
}

// startPhases begins the physical-build phase clock.
func (w *GenerationWorkCounters) startPhases() {
	if w == nil {
		return
	}
	w.lastMark = time.Now()
}

// mark closes the phase that ran since the previous mark.
func (w *GenerationWorkCounters) mark(name string) {
	if w == nil || w.lastMark.IsZero() {
		return
	}
	now := time.Now()
	w.Phases = append(w.Phases, GenerationPhase{Name: name, Duration: now.Sub(w.lastMark)})
	w.lastMark = now
}

// finish fills what is only known once the physical build has ended: the
// report's outcome and the store's writer-side counters for the generation.
func (w *GenerationWorkCounters) finish(store *store_sqlite.Store, generationID int64, report *BuildReport) {
	if w == nil || report == nil {
		return
	}
	w.GenerationID = generationID
	w.Coalesced = report.Coalesced
	w.mu.Lock()
	w.ParserInputs = len(w.parsed)
	w.ParserInputPaths = w.ParserInputPaths[:0]
	w.ParserInputBytes = 0
	for p, size := range w.parsed {
		w.ParserInputPaths = append(w.ParserInputPaths, p)
		w.ParserInputBytes += size
	}
	w.mu.Unlock()
	sort.Strings(w.ParserInputPaths)
	w.RebuiltFiles = w.ParserInputs
	w.ContextWithheldFiles = len(report.ContextPaths)
	w.ContextRetainedFiles = len(report.ContextRetainedPaths)
	w.ContextWithdrawnNodes, w.ContextWithdrawnEdges = report.ContextWithdrawnNodes, report.ContextWithdrawnEdges
	w.ContextHeldInMemory = report.ContextHeldInMemory
	w.ReplaceMasks, w.DeleteMasks = report.ReplaceMasks, report.DeleteMasks
	w.NodeTombstones, w.EdgeSources = report.NodeTombstones, report.EdgeSourceMarkers
	w.CompilerContext.Requested = report.Enrichment.Requested
	w.CompilerContext.Ran = append([]string(nil), report.Enrichment.Ran...)
	if compiler := report.Enrichment.Compiler; compiler != nil {
		w.CompilerContext.Measured = true
		w.CompilerContext.Packages, w.CompilerContext.Files = compiler.Packages, compiler.Files
		w.CompilerContext.Loads = compiler.Loads
		w.CompilerContext.Scope, w.CompilerContext.ScopeReason = compiler.Scope, compiler.ScopeReason
		w.CompilerContext.LoadMs, w.CompilerContext.IndexMs = compiler.LoadMs, compiler.IndexMs
		w.CompilerContext.IndexCached = compiler.IndexCached
		if compiler.Cache != nil {
			cache := *compiler.Cache
			w.CompilerContext.Cache = &cache
		}
		w.CompilerContext.Reason = ""
	}
	if !w.CompilerContext.Measured {
		switch {
		case !report.Enrichment.Requested:
			w.CompilerContext.Reason = "the build did not request semantic enrichment; no compiler context was loaded"
		case report.Enrichment.Disabled || len(report.Enrichment.Ran) == 0:
			w.CompilerContext.Reason = "semantic enrichment did not run: " + report.Enrichment.Reason
		default:
			w.CompilerContext.Reason = "semantic enrichment ran but its provider reports no package/file counts"
		}
	}
	if store != nil && !report.Coalesced {
		w.Store, w.StoreMeasured = store.TakeGenerationWriteCounters(generationID)
	}
}

// noteChainFallback records the reason a chained attempt was refused.
func (w *GenerationWorkCounters) noteChainFallback(reason string) {
	if w == nil || reason == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ChainFallbackReason = reason
	if w.ChainFallbackReasons == nil {
		w.ChainFallbackReasons = map[string]int{}
	}
	w.ChainFallbackReasons[reason]++
}

func (w *GenerationWorkCounters) noteParserInput(p string, size int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ParserInputOpens++
	w.parsed[path.Clean(p)] = size
}

func (w *GenerationWorkCounters) noteAdmissionStat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.AdmissionStats++
}

func (w *GenerationWorkCounters) noteAdmissionOpen(size int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.AdmissionOpens++
	w.AdmissionOpenBytes += size
}

// admissionSource wraps the build's target source so its stats and its
// non-extraction opens are counted. nil counters return src unchanged.
func (w *GenerationWorkCounters) admissionSource(src source.ContentSource) source.ContentSource {
	if w == nil || src == nil {
		return src
	}
	return &workCountingSource{inner: src, work: w}
}

// extractionSource wraps the pass's narrowed file set so every open through it
// is counted as an extraction parser input. It must wrap a source whose inner
// target is the admissionSource wrapper: an extraction open is then recorded
// here and NOT again as an admission open.
func (w *GenerationWorkCounters) extractionSource(src source.ContentSource) source.ContentSource {
	if w == nil || src == nil {
		return src
	}
	return &workCountingSource{inner: src, work: w, extraction: true}
}

// workCountingSource is a pass-through content source that counts reads. An
// extraction wrapper opens through openUncountedThrough so the admission
// wrapper beneath its narrowed file set does not count the same open again.
type workCountingSource struct {
	inner      source.ContentSource
	work       *GenerationWorkCounters
	extraction bool
}

var (
	_ source.ContentSource     = (*workCountingSource)(nil)
	_ source.RegularFileReader = (*workCountingSource)(nil)
)

func (s *workCountingSource) Identity() string { return s.inner.Identity() }
func (s *workCountingSource) Close() error     { return s.inner.Close() }

func (s *workCountingSource) Stat(p string) (source.FileMeta, error) {
	if !s.extraction {
		s.work.noteAdmissionStat()
		return s.inner.Stat(p)
	}
	// The narrowed set's stat reaches the admission wrapper beneath it, which
	// counts it; counting here too would double it.
	return s.inner.Stat(p)
}

func (s *workCountingSource) Open(p string) (io.ReadCloser, source.FileMeta, error) {
	if s.extraction {
		rc, meta, err := openUncountedThrough(s.inner, p)
		if err == nil {
			s.work.noteParserInput(p, meta.Size)
		}
		return rc, meta, err
	}
	rc, meta, err := s.inner.Open(p)
	if err == nil {
		s.work.noteAdmissionOpen(meta.Size)
	}
	return rc, meta, err
}

func (s *workCountingSource) Walk(ctx context.Context, fn func(source.FileMeta) error) error {
	return s.inner.Walk(ctx, fn)
}

// ReadRegularFile forwards the bounded regular-file read the wrapped source
// offers. The wrapper must not change what a reader can ask of the source:
// Go package ownership reads go.mod and go.work through the build's target
// with source.ReadRegularFile, and a wrapper without this method turns every
// such read into ErrRegularReadUnsupported — which ownership treats as an
// unreadable go.work, certifies no package, and leaves the resolver to pick
// an import or call target among same-named packages by name alone. A source
// that never offered the read still refuses it, exactly as it did unwrapped.
// A successful read through the admission wrapper is an admission open.
func (s *workCountingSource) ReadRegularFile(ctx context.Context, name string, maxBytes int64) ([]byte, source.FileMeta, error) {
	reader, ok := s.inner.(source.RegularFileReader)
	if !ok {
		return nil, source.FileMeta{}, fmt.Errorf("%s: %w", name, source.ErrRegularReadUnsupported)
	}
	data, meta, err := reader.ReadRegularFile(ctx, name, maxBytes)
	if err == nil && !s.extraction {
		s.work.noteAdmissionOpen(int64(len(data)))
	}
	return data, meta, err
}

// openUncountedThrough opens p through src, bypassing the admission count of
// any workCountingSource found at the bottom of a fileSetSource.
func openUncountedThrough(src source.ContentSource, p string) (io.ReadCloser, source.FileMeta, error) {
	if narrowed, ok := src.(*fileSetSource); ok {
		if !narrowed.holds(p) {
			return narrowed.Open(p)
		}
		if counted, ok := narrowed.inner.(*workCountingSource); ok && !counted.extraction {
			return counted.inner.Open(p)
		}
	}
	return src.Open(p)
}
