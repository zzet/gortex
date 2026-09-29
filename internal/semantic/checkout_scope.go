package semantic

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Compiler load scopes a compiler-backed provider reports on EnrichResult.
const (
	// CompilerScopeFull is a whole-module load ("./..." from the module
	// directory): every package of the checkout is type-checked from source.
	CompilerScopeFull = "full"
	// CompilerScopeHandleRoots is a load whose roots are exactly the packages
	// of the Go files the generation being built carries.
	CompilerScopeHandleRoots = "handle_roots"
)

// Reasons a checkout pass that asked for a handle-rooted load loaded the whole
// module instead (or loaded nothing). Each is decided before any graph or
// binding mutation.
const (
	// CompilerScopeReasonDisabled: the handle-rooted load is switched off.
	CompilerScopeReasonDisabled = "scope_disabled"
	// CompilerScopeReasonManifestChanged: the build's changes touch go.mod,
	// go.sum, go.work, go.work.sum or vendor/modules.txt.
	CompilerScopeReasonManifestChanged = "go_manifest_changed"
	// CompilerScopeReasonMultiModule: the module directory is not the
	// checkout root, or a go.work is present.
	CompilerScopeReasonMultiModule = "scope_multi_module"
	// CompilerScopeReasonPathUnmappable: a handle path cannot be made relative
	// to the module directory.
	CompilerScopeReasonPathUnmappable = "scope_path_unmappable"
	// CompilerScopeReasonEmpty: the handle carries no Go file a whole-module
	// load would type-check, so nothing is loaded.
	CompilerScopeReasonEmpty = "scope_empty"
	// CompilerScopeReasonLineDirective: a package outside the handle's
	// packages carries a hand-written //line or /*line directive; that
	// package is added to the roots (not a whole-module load).
	CompilerScopeReasonLineDirective = "line_directive_outside_scope"
	// CompilerScopeReasonScopedThenFull: the handle-rooted load failed or
	// missed a root while its context was still live; the pass retried once
	// with a whole-module load.
	CompilerScopeReasonScopedThenFull = "scoped_then_full"
)

// CompilerLoadStats is the compiler-context work of one provider pass: how
// many go/packages programs it loaded, how many type-checked packages and
// compiled files they held, and which scope they had. A nil *CompilerLoadStats
// means the provider reports no counts, not that it loaded nothing.
type CompilerLoadStats struct {
	// Loads counts type-checking package loads (2 for scoped_then_full).
	Loads int `json:"loads"`
	// Packages and Files are the type-checked packages of the load whose
	// result the pass used, and their compiled Go files.
	Packages int `json:"packages"`
	Files    int `json:"files"`
	// Scope is CompilerScopeFull or CompilerScopeHandleRoots.
	Scope string `json:"scope"`
	// ScopeReason is empty for an ordinary pass and for a handle-rooted load
	// that needed no adjustment; otherwise one of the CompilerScopeReason*
	// codes.
	ScopeReason string `json:"scope_reason,omitempty"`
	// LoadMs is the wall time of the type-checking loads.
	LoadMs int64 `json:"load_ms"`
	// IndexMs is the wall time spent obtaining the dependency metadata index,
	// and IndexCached reports that no `go list` ran for it.
	IndexMs     int64 `json:"index_ms"`
	IndexCached bool  `json:"index_cached,omitempty"`
	// Cache is the per-checkout closure and type-check state's work for this
	// pass; nil when the pass did not use it.
	Cache *CompilerCacheStats `json:"cache,omitempty"`
}

// CompilerCacheStats is what the per-checkout compiler state did for one
// handle-rooted pass: whether the dependency closure (`go list -export
// -deps`) was served from memory, how much of the type-check reused retained
// dependency types and parsed files, and how large the retained state is.
type CompilerCacheStats struct {
	// ClosureHits counts passes whose closure was served from memory, so no
	// `go list` ran; ClosureMisses counts passes that listed it.
	ClosureHits   int `json:"closure_hits"`
	ClosureMisses int `json:"closure_misses"`
	// MissReason is the first miss's reason: cold, root_unlisted,
	// root_files_changed, cgo_changed, cgo_unlisted (the root's metadata
	// came from a listing that could not run cgo because a dependency did
	// not compile), dependency_changed, import_added, export_unavailable.
	MissReason string `json:"miss_reason,omitempty"`
	// MissPackage names the dependency whose state caused a dependency miss
	// (a changed file set or build header, missing metadata).
	MissPackage string `json:"miss_package,omitempty"`
	// Bypass is set when the pass fell back to the plain package load
	// because the cached path cannot reproduce it (vendor, import "C" in a
	// file compiled as-is, a dependency without export data, a dependency
	// between two roots, tests, an unstable listing).
	Bypass string `json:"bypass,omitempty"`
	// GoListMs is the wall time of the listings this pass ran;
	// GoListSavedMs is, on a hit, the last listing's wall time for the
	// checkout (what the hit did not spend).
	GoListMs      int64 `json:"go_list_ms"`
	GoListSavedMs int64 `json:"go_list_saved_ms"`
	// ValidateMs checks the closure against the working tree; ParseMs
	// parses the roots' changed files; CheckMs type-checks the roots.
	ValidateMs int64 `json:"validate_ms"`
	// ValidateStats and ValidateReads count the files validation stat'ed
	// and the ones it had to read (a changed stamp).
	ValidateStats int   `json:"validate_stats"`
	ValidateReads int   `json:"validate_reads"`
	ParseMs       int64 `json:"parse_ms"`
	CheckMs       int64 `json:"check_ms"`
	// FilesParsed and FilesReused split the roots' compiled files between
	// parsed now and served from the retained syntax.
	FilesParsed int `json:"files_parsed"`
	FilesReused int `json:"files_reused"`
	// ExportReads counts dependency export files read now; TypesReused the
	// direct dependencies served from retained types.
	ExportReads int `json:"export_reads"`
	TypesReused int `json:"types_reused"`
	// StatePackages and StateBytes size the checkout's retained type state
	// after the pass (StateBytes estimates its heap from the export data read
	// and the retained source); StateEvicted reports that the pass dropped it
	// for exceeding the cap.
	StatePackages int   `json:"state_packages"`
	StateBytes    int64 `json:"state_bytes"`
	StateEvicted  bool  `json:"state_evicted,omitempty"`
	// DeclChangedFiles counts handle files whose package-level declarations
	// (everything outside function bodies, comments ignored) differ from the
	// version the state last parsed. Sibling files that use a changed
	// declaration are not in the handle, so their facts are not rewritten by
	// this pass (nor by a whole-module load of it); the count lets the
	// caller widen the handle.
	DeclChangedFiles int `json:"decl_changed_files,omitempty"`
	// WarmServed reports a closure hit whose root metadata came from the
	// checkout's background whole-module listing (no pass had listed the
	// root before).
	WarmServed bool `json:"warm_served,omitempty"`
	// WarmupState is the checkout's background whole-module listing as the
	// pass found it: empty (never started), running, preempted (an
	// interactive load cancelled it; it retries once the provider is quiet),
	// warm, failed, superseded (the module manifests changed under it) or
	// cancelled (the provider closed).
	WarmupState string `json:"warmup_state,omitempty"`
	// WarmupMs is the wall time of the last completed warm-up listing,
	// WarmupPackages the packages it listed, WarmupAttempts the listings
	// started for the current manifests and WarmupPreemptions how many of
	// them an interactive load cancelled.
	WarmupMs          int64 `json:"warmup_ms,omitempty"`
	WarmupPackages    int   `json:"warmup_packages,omitempty"`
	WarmupAttempts    int   `json:"warmup_attempts,omitempty"`
	WarmupPreemptions int   `json:"warmup_preemptions,omitempty"`
	// ChangedDependencies counts the closure's mutable dependencies whose
	// files changed (content only) since they were listed. Instead of
	// listing the closure again (which rebuilds the export data of every
	// package between the change and the roots), the pass type-checks them
	// and every closure package that imports one of them from source:
	// SourceDependencies packages with SourceDependencyFiles files (bodies
	// stripped), parsed in SourceDependencyParseMs. SourceDependencyFallback
	// names why a pass with changed dependencies listed the closure instead.
	ChangedDependencies      int    `json:"changed_dependencies,omitempty"`
	SourceDependencies       int    `json:"source_dependencies,omitempty"`
	SourceDependencyFiles    int    `json:"source_dependency_files,omitempty"`
	SourceDependencyParseMs  int64  `json:"source_dependency_parse_ms,omitempty"`
	SourceDependencyFallback string `json:"source_dependency_fallback,omitempty"`
	// RetainedKept counts the packages a listing this pass ran brought
	// back degraded (no export data: a dependency did not compile while it
	// ran) whose retained, still valid metadata the state kept instead.
	RetainedKept int `json:"retained_kept,omitempty"`
	// WorkingSetPackages is the pass's working set (its roots, their
	// closure and the dependencies checked from source), which the memory
	// cap never evicts. EvictedPackages and EvictedFiles count the retained
	// dependency types and parsed files the cap dropped after the pass,
	// least recently touched first (see the checkout typecheck state).
	WorkingSetPackages int `json:"working_set_packages,omitempty"`
	EvictedPackages    int `json:"evicted_packages,omitempty"`
	EvictedFiles       int `json:"evicted_files,omitempty"`
	// RelistScheduled counts the background export relists this pass
	// scheduled: it compiled, and its working set held packages retained
	// without export data (listed while they did not compile).
	RelistScheduled int `json:"relist_scheduled,omitempty"`
	// TargetedWaitMs is how long the pass waited for the checkout's
	// warm-up to merge the listing of its own (dirty or recently touched)
	// package instead of listing it; TargetedWait why the wait ended:
	// adopted (it joined the running listing), merged, stopped (the
	// warm-up was preempted or ended), timeout (still queued), small (the
	// package's closure is small: the pass listed itself).
	TargetedWaitMs int64  `json:"targeted_wait_ms,omitempty"`
	TargetedWait   string `json:"targeted_wait,omitempty"`
}

// Add accumulates another pass's cache work.
func (s *CompilerCacheStats) Add(o *CompilerCacheStats) {
	if s == nil || o == nil {
		return
	}
	s.ClosureHits += o.ClosureHits
	s.ClosureMisses += o.ClosureMisses
	if s.MissReason == "" {
		s.MissReason = o.MissReason
		s.MissPackage = o.MissPackage
	}
	if s.Bypass == "" {
		s.Bypass = o.Bypass
	}
	s.GoListMs += o.GoListMs
	s.GoListSavedMs += o.GoListSavedMs
	s.ValidateMs += o.ValidateMs
	s.ValidateStats += o.ValidateStats
	s.ValidateReads += o.ValidateReads
	s.ParseMs += o.ParseMs
	s.CheckMs += o.CheckMs
	s.FilesParsed += o.FilesParsed
	s.FilesReused += o.FilesReused
	s.ExportReads += o.ExportReads
	s.TypesReused += o.TypesReused
	s.StatePackages += o.StatePackages
	s.StateBytes += o.StateBytes
	s.StateEvicted = s.StateEvicted || o.StateEvicted
	s.DeclChangedFiles += o.DeclChangedFiles
	s.WarmServed = s.WarmServed || o.WarmServed
	s.ChangedDependencies += o.ChangedDependencies
	s.SourceDependencies += o.SourceDependencies
	s.SourceDependencyFiles += o.SourceDependencyFiles
	s.SourceDependencyParseMs += o.SourceDependencyParseMs
	if s.SourceDependencyFallback == "" {
		s.SourceDependencyFallback = o.SourceDependencyFallback
	}
	s.RetainedKept += o.RetainedKept
	s.WorkingSetPackages += o.WorkingSetPackages
	s.EvictedPackages += o.EvictedPackages
	s.EvictedFiles += o.EvictedFiles
	s.RelistScheduled += o.RelistScheduled
	s.TargetedWaitMs += o.TargetedWaitMs
	if s.TargetedWait == "" {
		s.TargetedWait = o.TargetedWait
	}
	if s.WarmupState == "" {
		s.WarmupState = o.WarmupState
		s.WarmupMs = o.WarmupMs
		s.WarmupPackages = o.WarmupPackages
		s.WarmupAttempts = o.WarmupAttempts
		s.WarmupPreemptions = o.WarmupPreemptions
	}
}

// Add accumulates another pass's counts into s. Scope and reason keep the
// first non-empty value, except that a whole-module scope wins over a
// handle-rooted one.
func (s *CompilerLoadStats) Add(o *CompilerLoadStats) {
	if s == nil || o == nil {
		return
	}
	s.Loads += o.Loads
	s.Packages += o.Packages
	s.Files += o.Files
	s.LoadMs += o.LoadMs
	s.IndexMs += o.IndexMs
	s.IndexCached = s.IndexCached || o.IndexCached
	if o.Cache != nil {
		if s.Cache == nil {
			s.Cache = &CompilerCacheStats{}
		}
		s.Cache.Add(o.Cache)
	}
	if s.Scope == "" || o.Scope == CompilerScopeFull {
		s.Scope = o.Scope
	}
	if s.ScopeReason == "" {
		s.ScopeReason = o.ScopeReason
	}
}

// CheckoutCompilerScope is what a checkout pass asks of a compiler-backed
// provider. Its presence on the pass context says the graph is a generation
// handle whose facts are keyed to the files it carries: a provider then
// writes per-file compact projections (semantic binding types) only for those
// files, never for the whole repository. Ordinary indexing never sets it.
type CheckoutCompilerScope struct {
	// HandleRoots asks for a load whose roots are the packages of the handle's
	// Go files instead of the whole module. The output is identical; only the
	// packages type-checked and then discarded differ.
	HandleRoots bool
	// ManifestChanged reports that the build's changes touch a Go module
	// manifest, which forces a whole-module load.
	ManifestChanged bool
	// StripSiblingBodies additionally parses the root packages' files that
	// the handle does not carry without function bodies. It applies only with
	// HandleRoots.
	StripSiblingBodies bool
	// TypecheckCache keeps, per checkout, the dependency closure the go
	// command listed (export files and metadata) and the dependency types
	// read from it, plus the roots' parsed files, so a pass whose closure is
	// unchanged runs no `go list` and re-type-checks only the roots. The
	// output is identical. It applies only with HandleRoots.
	TypecheckCache bool
	// TypecheckCacheBytes caps the retained state across checkouts (an
	// estimate; see CompilerCacheStats.StateBytes). Zero means the default.
	TypecheckCacheBytes int64
	// Declarations serves the declaration nodes of repository files the
	// pass's handle does not carry: the layer the generation sits on. A
	// narrowed working-tree generation carries only the files it re-derived,
	// so a use in one of them that binds to a declaration in an unchanged
	// file finds no node on the handle; the pass maps such a use onto the
	// node this reader serves at the declaration's position, exactly as a
	// whole-module load maps it onto its own copy. Nil keeps the handle-only
	// behaviour. It decides which node a use binds to, never what is loaded.
	Declarations CheckoutDeclarationReader
}

// CheckoutDeclarationReader is the read a checkout pass needs of the layer
// below its handle: every node recorded at each of the given graph paths.
type CheckoutDeclarationReader interface {
	GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node
}

type checkoutCompilerScopeKey struct{}

// WithCheckoutCompilerScope attaches a checkout pass's compiler scope.
func WithCheckoutCompilerScope(ctx context.Context, scope CheckoutCompilerScope) context.Context {
	return context.WithValue(ctx, checkoutCompilerScopeKey{}, scope)
}

// CheckoutCompilerScopeFrom returns the checkout pass's compiler scope; ok is
// false outside a checkout pass.
func CheckoutCompilerScopeFrom(ctx context.Context) (scope CheckoutCompilerScope, ok bool) {
	if ctx == nil {
		return CheckoutCompilerScope{}, false
	}
	scope, ok = ctx.Value(checkoutCompilerScopeKey{}).(CheckoutCompilerScope)
	return scope, ok
}
