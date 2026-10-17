package goanalysis

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/semantic"
)

// LoadMode controls how deeply the go/types provider analyzes the code.
type LoadMode int

const (
	// ModeTypeCheck loads types only (~5-10s). Resolves all type information
	// and interface implementations but does not build a call graph.
	ModeTypeCheck LoadMode = iota

	// ModeCallGraph loads SSA and builds a VTA call graph (~15-30s).
	// Most precise but requires more time and memory.
	ModeCallGraph
)

// Provider uses Go's native toolchain (go/packages, go/types) for
// compiler-level precision on Go codebases.
type Provider struct {
	mode         LoadMode
	includeTest  bool
	logger       *zap.Logger
	packagesLoad func(*packages.Config, ...string) ([]*packages.Package, error)
	// compilerGC is injectable for ordering tests; production uses a full GC
	// plus synchronous scavenging at pressure-sized compiler boundaries.
	compilerGC func()

	// The compiler program is intentionally never retained after EnrichRepo.
	// Contract extraction needs only a compact (file,line,name) -> bare type
	// projection, kept here for the in-memory backend and persisted by SQLite.
	// bindingOwners lets deterministic repo release remove only its own keys.
	stateMu           sync.RWMutex
	bindingTypes      map[bindingLookupKey]string
	bindingOwners     map[bindingLookupKey]string
	bindingKeysByRoot map[string][]bindingLookupKey
	retained          map[string]int

	// heavyGate bounds the total number of live go/packages programs. A second
	// one can still overlap useful compute on high-memory hosts.
	heavyGate chan struct{}
	// largeGate admits at most one large full-repository compiler program. The
	// lease ends after string/ID projection plus one large-program collection,
	// before detached graph apply. Small repos and file-bounded incremental loads do
	// not take this gate and may use the remaining heavyGate lane.
	largeGate chan struct{}
	// committed is the committed-tree passes holding compiler admission, and
	// interactive counts the loads waiting for it that may overtake them
	// (see acquireHeavy).
	committed committedHolders

	// scopeMu guards scopeCache: per module directory, the dependency
	// metadata index and line-directive set that handle-rooted checkout loads
	// reuse between passes (see checkoutScopeCache).
	scopeMu    sync.Mutex
	scopeCache map[string]*checkoutScopeCache

	// tcMu guards tcStates: per checkout directory, the retained dependency
	// closure and type-check state of handle-rooted checkout passes (see
	// checkoutTypecheckState). Each state has its own lock.
	tcMu     sync.Mutex
	tcStates map[string]*checkoutTypecheckState

	// warm runs the background whole-module listings that warm checkouts'
	// retained closures, and preempts them for compiler loads (see
	// typecheck_warmup.go).
	warm warmupRegistry
}

type bindingLookupKey struct {
	repoPrefix string
	filePath   string
	line       int
	name       string
}

const defaultGoTypesConcurrency = 1

// goTypesLargeNodeThreshold is the repository census point at which a full
// compiler program takes exclusive large-program admission. It matches the
// heavy-Go scheduling class used by the multi-repository enrichment queue.
const goTypesLargeNodeThreshold = 8192

// Large admission is broader than forced reclamation: repositories just over
// the census threshold still serialize compiler programs, but a stop-the-world
// GC is paid only when the detached program grew the heap materially or its Use
// projection proves a large closure despite noisy process-wide heap samples.
const (
	goTypesCompilerReclaimMinGrowth = 256 << 20
	goTypesCompilerReclaimMinUses   = 131072
)

func shouldReclaimLargeCompiler(large bool, heapGrowth uint64, projectedUses int) bool {
	return large && (heapGrowth >= goTypesCompilerReclaimMinGrowth || projectedUses >= goTypesCompilerReclaimMinUses)
}

// goTypesConcurrencyRAMFloor is the host RAM at or above which the gate
// defaults to two concurrent programs: a multi-repo workspace's enrichment
// chain is otherwise serialized behind its largest module (measured: the
// productive chain halves, ~800s → ~410s, on a 28-repo workspace), and two
// co-resident type-checked closures cost low single-digit GiB — safe with
// this much RAM, not below it.
const goTypesConcurrencyRAMFloor = 16 << 30

// goTypesConcurrency resolves the heavy-gate capacity:
// GORTEX_GOTYPES_CONCURRENCY always wins; otherwise hosts with at least
// goTypesConcurrencyRAMFloor of physical RAM admit two programs, everything
// else stays serial.
func goTypesConcurrency(hostRAM uint64) int {
	fallback := defaultGoTypesConcurrency
	if hostRAM >= goTypesConcurrencyRAMFloor {
		fallback = 2
	}
	return envPositiveInt("GORTEX_GOTYPES_CONCURRENCY", fallback)
}

// largeGoTypesAdmission classifies only full-repository work. An absent census
// is conservative because direct Provider calls can otherwise admit two
// unmeasured compiler closures. File-bounded incremental loads stay shareable.
func largeGoTypesAdmission(ctx context.Context, fullRepo bool) bool {
	if !fullRepo {
		return false
	}
	nodes, ok := semantic.EnrichmentAdmissionNodes(ctx)
	return !ok || nodes >= goTypesLargeNodeThreshold
}

// NewProvider creates a go/types provider.
func NewProvider(mode LoadMode, includeTest bool, logger *zap.Logger) *Provider {
	return &Provider{
		mode:              mode,
		includeTest:       includeTest,
		logger:            logger,
		packagesLoad:      packages.Load,
		compilerGC:        debug.FreeOSMemory,
		bindingTypes:      make(map[bindingLookupKey]string),
		bindingOwners:     make(map[bindingLookupKey]string),
		bindingKeysByRoot: make(map[string][]bindingLookupKey),
		retained:          make(map[string]int),
		heavyGate:         make(chan struct{}, goTypesConcurrency(platform.HostPhysicalMemoryBytes())),
		largeGate:         make(chan struct{}, 1),
	}
}

func (p *Provider) Name() string        { return "go-types" }
func (p *Provider) Languages() []string { return []string{"go"} }

// Close drops the compact in-memory binding index. Full compiler programs are
// local to an enrichment call and therefore require no provider-level cleanup.
func (p *Provider) Close() error {
	p.stopWarmups()
	p.stateMu.Lock()
	p.bindingTypes = nil
	p.bindingOwners = nil
	p.bindingKeysByRoot = nil
	p.retained = nil
	p.stateMu.Unlock()
	return nil
}

// RetainRepoState leases repoRoot's compact binding index across the deferred
// enrichment-to-contract boundary. The lease may be acquired before enrichment
// publishes the index.
func (p *Provider) RetainRepoState(repoRoot string) bool {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return false
	}
	p.stateMu.Lock()
	if p.retained == nil {
		p.retained = make(map[string]int)
	}
	p.retained[absRoot]++
	p.stateMu.Unlock()
	return true
}

// ReleaseRepoState drops only repoRoot's compact binding rows after its
// contract pass. It never owns a packages.Package/types.Info/AST graph.
func (p *Provider) ReleaseRepoState(repoRoot string) bool {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return false
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if leases := p.retained[absRoot]; leases > 1 {
		p.retained[absRoot] = leases - 1
		return false
	}
	delete(p.retained, absRoot)
	keys, removed := p.bindingKeysByRoot[absRoot]
	for _, key := range keys {
		if p.bindingOwners[key] != absRoot {
			continue
		}
		delete(p.bindingTypes, key)
		delete(p.bindingOwners, key)
	}
	delete(p.bindingKeysByRoot, absRoot)
	return removed
}

// committedHolders is the compiler admission's view of committed-tree passes.
type committedHolders struct {
	// interactive counts the loads that are waiting for admission and may
	// preempt a committed pass: every load that is not one.
	interactive atomic.Int64
	mu          sync.Mutex
	seq         uint64
	preempts    map[uint64]func()
}

func (c *committedHolders) register(preempt func()) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.preempts == nil {
		c.preempts = map[uint64]func(){}
	}
	c.seq++
	c.preempts[c.seq] = preempt
	return c.seq
}

func (c *committedHolders) unregister(id uint64) {
	c.mu.Lock()
	delete(c.preempts, id)
	c.mu.Unlock()
}

// preemptAll ends every committed pass holding admission. Each pass releases
// its admission on its way out.
func (c *committedHolders) preemptAll() int {
	c.mu.Lock()
	preempts := make([]func(), 0, len(c.preempts))
	for _, preempt := range c.preempts {
		preempts = append(preempts, preempt)
	}
	c.mu.Unlock()
	for _, preempt := range preempts {
		preempt()
	}
	return len(preempts)
}

// committedPreempt returns the preemption of a committed-tree pass, nil for
// every other load.
func committedPreempt(ctx context.Context) func() {
	scope, ok := semantic.CheckoutCompilerScopeFrom(ctx)
	if !ok || !scope.Committed || scope.Preempt == nil {
		return nil
	}
	return scope.Preempt
}

// committedOverlay is the overlay a committed-tree pass reads its tree
// through; nil for every other load, which reads the working copy.
func committedOverlay(ctx context.Context) map[string][]byte {
	scope, ok := semantic.CheckoutCompilerScopeFrom(ctx)
	if !ok || !scope.Committed || len(scope.Overlay) == 0 {
		return nil
	}
	return scope.Overlay
}

// committedEnv is the environment of a committed-tree pass's go command, nil
// (the process's) for every other load.
func committedEnv(ctx context.Context) []string {
	scope, ok := semantic.CheckoutCompilerScopeFrom(ctx)
	if !ok || !scope.Committed || !scope.GoWorkOff {
		return nil
	}
	return append(os.Environ(), "GOWORK=off")
}

// ReadsCommittedTree reports that the provider reads a committed tree through
// a committed pass's overlay (semantic.CommittedTreeReader): every load of the
// pass hands the overlay to the go command.
func (p *Provider) ReadsCommittedTree() bool { return true }

// acquireHeavy admits one compiler program.
//
// A committed tree's pass (a dedicated base, its advance, a commit layer)
// yields to every other load: it gives an admission back while another load
// waits, and a load that finds the admission taken ends the committed passes
// holding it before it waits. A committed pass that is preempted publishes its generation without
// the type checker's facts and says so; an edit never waits minutes behind it.
func (p *Provider) acquireHeavy(ctx context.Context, large bool) (func(), error) {
	preempt := committedPreempt(ctx)
	if preempt == nil {
		p.committed.interactive.Add(1)
		defer p.committed.interactive.Add(-1)
		if release, ok := p.tryAcquireHeavy(large); ok {
			return release, nil
		}
		if n := p.committed.preemptAll(); n > 0 && p.logger != nil {
			p.logger.Info("go-types: a load overtook committed-tree passes",
				zap.Int("preempted", n), zap.Bool("large", large))
		}
		return p.acquireHeavyBlocking(ctx, large)
	}
	for {
		release, err := p.acquireHeavyBlocking(ctx, large)
		if err != nil {
			return nil, err
		}
		id := p.committed.register(preempt)
		// A load that is waiting now found nothing to preempt when it began
		// (this pass was queued, not registered): give the admission back
		// and queue again behind it. Waiters are admitted in arrival order,
		// so a pass that arrives while a load waits is admitted after it.
		if p.committed.interactive.Load() > 0 {
			p.committed.unregister(id)
			release()
			continue
		}
		return func() {
			p.committed.unregister(id)
			release()
		}, nil
	}
}

// tryAcquireHeavy takes the admission only when it is free now.
func (p *Provider) tryAcquireHeavy(large bool) (func(), bool) {
	gate, largeGate := p.admissionGates()
	if large {
		select {
		case largeGate <- struct{}{}:
		default:
			return nil, false
		}
	}
	select {
	case gate <- struct{}{}:
		return func() {
			<-gate
			if large {
				<-largeGate
			}
		}, true
	default:
		if large {
			<-largeGate
		}
		return nil, false
	}
}

func (p *Provider) admissionGates() (chan struct{}, chan struct{}) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.heavyGate == nil {
		p.heavyGate = make(chan struct{}, defaultGoTypesConcurrency)
	}
	if p.largeGate == nil {
		p.largeGate = make(chan struct{}, 1)
	}
	return p.heavyGate, p.largeGate
}

func (p *Provider) acquireHeavyBlocking(ctx context.Context, large bool) (func(), error) {
	gate, largeGate := p.admissionGates()

	largeHeld := false
	if large {
		select {
		case largeGate <- struct{}{}:
			largeHeld = true
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	select {
	case gate <- struct{}{}:
		return func() {
			<-gate
			if largeHeld {
				<-largeGate
			}
		}, nil
	case <-ctx.Done():
		if largeHeld {
			<-largeGate
		}
		return nil, ctx.Err()
	}
}

func lockResolveContext(ctx context.Context, mu *sync.Mutex) error {
	for !mu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// resolveLockSlices records the actual graph-lock cost of a provider pass.
// It deliberately owns no compiler state: callers build detached projections
// and mutation plans outside the lock, then use with for the bounded store
// read/write slice only.
type resolveLockSlices struct {
	mu      *sync.Mutex
	waited  time.Duration
	held    time.Duration
	maxHeld time.Duration
	count   int
}

func (s *resolveLockSlices) with(ctx context.Context, fn func() error) error {
	waitStart := time.Now()
	if err := lockResolveContext(ctx, s.mu); err != nil {
		s.waited += time.Since(waitStart)
		return err
	}
	acquired := time.Now()
	s.waited += acquired.Sub(waitStart)
	s.count++
	defer func() {
		held := time.Since(acquired)
		s.held += held
		if held > s.maxHeld {
			s.maxHeld = held
		}
		s.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// detachGoNodeProjection protects the in-memory backend as well as SQLite.
// SQLite projections are already reconstructed values, but Graph returns its
// resident pointers. The apply phase only needs structural fields, so shallow
// copies with Meta detached remain stable while another lock slice mutates the
// live graph and avoid retaining opaque metadata with the compiler program.
func detachGoNodeProjection(nodes []*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		clone := *node
		clone.Meta = nil
		out = append(out, &clone)
	}
	return out
}

func (p *Provider) replaceBindingIndex(absRoot string, files []string, rows []graph.SemanticBindingType) {
	fileSet := make(map[string]struct{}, len(files))
	for _, filePath := range files {
		fileSet[normalizeRelPath(filePath)] = struct{}{}
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.bindingTypes == nil {
		p.bindingTypes = make(map[bindingLookupKey]string)
		p.bindingOwners = make(map[bindingLookupKey]string)
		p.bindingKeysByRoot = make(map[string][]bindingLookupKey)
	}

	oldKeys := p.bindingKeysByRoot[absRoot]
	kept := oldKeys[:0]
	for _, key := range oldKeys {
		_, replaceFile := fileSet[key.filePath]
		if len(files) > 0 && !replaceFile {
			kept = append(kept, key)
			continue
		}
		if p.bindingOwners[key] == absRoot {
			delete(p.bindingTypes, key)
			delete(p.bindingOwners, key)
		}
	}
	p.bindingKeysByRoot[absRoot] = kept

	for _, row := range rows {
		key := bindingLookupKey{repoPrefix: row.Site.RepoPrefix, filePath: normalizeRelPath(row.Site.FilePath), line: row.Site.Line, name: row.Site.Name}
		p.bindingTypes[key] = row.TypeName
		p.bindingOwners[key] = absRoot
		p.bindingKeysByRoot[absRoot] = append(p.bindingKeysByRoot[absRoot], key)
	}
}

// SemanticBindingTypes resolves a batch from the compact in-memory index. The
// SQLite backend implements the same capability persistently; this provider
// implementation keeps the in-memory backend and direct unit tests query-free.
func (p *Provider) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	out := make(map[graph.SemanticBindingSite]string, len(sites))
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	for _, site := range sites {
		key := bindingLookupKey{repoPrefix: site.RepoPrefix, filePath: normalizeRelPath(site.FilePath), line: site.Line, name: site.Name}
		if typeName := p.bindingTypes[key]; typeName != "" {
			out[site] = typeName
		}
	}
	return out, nil
}

var _ graph.SemanticBindingTypeReader = (*Provider)(nil)

// goToolchainOnce caches the one-time probe for the `go` command. The
// provider shells out through go/packages (`go list`), so without the
// toolchain on PATH it can only fail — and a shipped gortex binary often
// runs on a machine with no Go installed. Probing once lets the manager's
// Available() gate skip the provider instead of attempting a package load
// that fails on every index.
var (
	goToolchainOnce sync.Once
	goToolchainOK   bool
)

func goToolchainAvailable() bool {
	goToolchainOnce.Do(func() {
		_, err := exec.LookPath("go")
		goToolchainOK = err == nil
	})
	return goToolchainOK
}

func (p *Provider) Available() bool {
	// go/packages requires the Go toolchain; gate on a cached PATH probe
	// so a binary running without `go` skips this provider cleanly and
	// the go-ast-types supplemental provider serves Go instead.
	return goToolchainAvailable()
}

func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepo(g, "", repoRoot)
}

// EnrichRepo runs the go/types enrichment pass with its graph scans scoped
// to repoPrefix (the multi-repo scope key; "" for a single-repo / in-memory
// graph). The go/packages load is already scoped to repoRoot; scoping the
// graph-side symbol count and implements-edge scan to one repo stops a
// multi-repo warmup from paying a whole-graph AllNodes / AllEdges walk per
// repo. Implementing this makes the provider a semantic.RepoScopedProvider,
// so the manager dispatches it per repo with the repo's prefix.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	return p.enrichRepoContext(context.Background(), g, repoPrefix, repoRoot)
}

// EnrichRepoContext is the cooperative manager path. Cancellation applies to
// admission, go/packages, and resolve-lock acquisition, preventing timed-out
// repos from continuing as detached multi-gigabyte background passes.
func (p *Provider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, _ semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	return p.enrichRepoContext(ctx, g, repoPrefix, repoRoot)
}

func (p *Provider) enrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("absolute path: %w", err)
	}
	// Module admission BEFORE the heavy gate: without a go.mod/go.work,
	// go/packages "./..." degrades to a GOPATH-mode directory scan of the
	// whole (possibly non-Go) repository — measured minutes on a Rust tree —
	// and the pass would additionally hold the serialized gate for that whole
	// time, stalling every genuine Go repo queued behind it.
	loadDir, moduleRoots := goLoadDir(absRoot)
	if moduleRoots == 0 {
		return &semantic.EnrichResult{
			Provider:       p.Name(),
			Language:       "go",
			Degraded:       true,
			DegradedReason: "no go.mod/go.work within two directory levels; go/packages not attempted",
		}, nil
	}
	// Several modules below the root and none at it: there is no single
	// directory in which "./..." names all of them, so this pass has nothing
	// correct to load. Report the shape rather than letting the loadability
	// probe below call the modules unloadable, which they are not.
	if loadDir == absRoot && !hasGoManifest(absRoot) {
		return &semantic.EnrichResult{
			Provider: p.Name(),
			Language: "go",
			Degraded: true,
			DegradedReason: fmt.Sprintf(
				"%d Go modules below the repository root and none at it; go/packages needs a single module directory",
				moduleRoots),
		}, nil
	}

	// Loadability probe BEFORE the heavy gate: goModulePresent only proves a
	// go.mod exists, not that the module resolves. A module that fails the
	// heavy NeedDeps load still burns minutes of build-list construction and
	// GOPROXY attempts while holding the single serialized gate slot, yielding
	// zero edges — measured as the head-of-line block of the whole enrichment
	// chain. The probe is a cheap `go list` off the gate; it fails open on any
	// ambiguity, so a productive repo is never skipped. It runs only when the
	// manifest is NOT at the repo root: that is exactly the doomed shape
	// (`./...` from the root is out-of-module), while a root manifest loads
	// normally and paying a second metadata enumeration on every healthy repo
	// is a measured per-repo tax with no yield.
	if !hasGoManifest(absRoot) {
		if loadable, realPkgs, erroredPkgs := p.probeGoPackagesLoadable(ctx, loadDir); !loadable {
			if p.logger != nil {
				p.logger.Info("go-types: skipping unloadable module before heavy load",
					zap.String("repo_prefix", repoPrefix),
					zap.String("root", absRoot),
					zap.String("module_dir", loadDir),
					zap.Int("real_packages", realPkgs),
					zap.Int("errored_packages", erroredPkgs))
			}
			return &semantic.EnrichResult{
				Provider:       p.Name(),
				Language:       "go",
				Degraded:       true,
				DegradedReason: "go/packages loadability probe found no cleanly-loading packages; full typecheck skipped",
			}, nil
		}
	}
	// A checkout pass enriches a generation handle whose facts are keyed to
	// the files it carries. Its Go projection is read once, here, before the
	// admission decision, and reused as the apply-side projection below. With
	// the handle-rooted load requested, the roots are the packages of those
	// files; every decision to load the whole module instead is taken now,
	// before anything is loaded or written.
	checkoutScope, checkoutPass := semantic.CheckoutCompilerScopeFrom(ctx)
	if checkoutScope.Committed {
		// A handle-rooted load keeps per-checkout state keyed by the working
		// copy's manifests and line directives, which a committed tree does
		// not share: a committed pass loads the whole module.
		checkoutScope.HandleRoots = false
	}
	compiler := &semantic.CompilerLoadStats{Scope: semantic.CompilerScopeFull}
	var (
		handleFiles    map[string]struct{}
		handleFileList []string
		earlyNodes     []*graph.Node
		roots          handleRootPlan
		manifestDigest string
	)
	if checkoutPass {
		if err := lockResolveContext(ctx, g.ResolveMutex()); err != nil {
			return nil, err
		}
		earlyNodes = detachGoNodeProjection(repoGoNodes(g, repoPrefix))
		g.ResolveMutex().Unlock()
		handleFiles = handleGoFiles(earlyNodes)
		handleFileList = make([]string, 0, len(handleFiles))
		for file := range handleFiles {
			handleFileList = append(handleFileList, file)
		}
		sort.Strings(handleFileList)
		if checkoutScope.HandleRoots {
			roots = planHandleRoots(absRoot, loadDir, repoPrefix, handleFiles, checkoutScope)
			compiler.ScopeReason = roots.reason
		} else {
			compiler.ScopeReason = semantic.CompilerScopeReasonDisabled
		}
	}
	useHandleRoots := checkoutPass && checkoutScope.HandleRoots && !roots.full
	if useHandleRoots {
		compiler.Scope = semantic.CompilerScopeHandleRoots
		manifestDigest = goManifestDigest(loadDir)
		// A package outside the handle's packages whose source carries a
		// hand-written line directive may attribute positions to a handle
		// file; the whole-module load scans it, so it joins the roots.
		for _, dir := range p.lineDirectiveDirs(loadDir, manifestDigest, roots.handleAbs) {
			before := len(roots.rootDirs)
			roots.addDir(loadDir, dir)
			if len(roots.rootDirs) != before {
				compiler.ScopeReason = semantic.CompilerScopeReasonLineDirective
			}
		}
		sort.Strings(roots.patterns)
		if roots.empty() {
			return p.finishEmptyCheckoutScope(g, absRoot, repoPrefix, handleFileList, compiler, start)
		}
	}

	// Metadata-only dependency index for the externals classification, loaded
	// OFF the heavy gate (it is a `go list` walk, not a typecheck). Only the
	// export-data mode needs it — the closure mode's Imports walk already
	// carries every dep. A failed index falls back to nil: resolveSymbol then
	// skips classification per object (counted as missingPkgInfo) instead of
	// mislabeling anything. A handle-rooted load obtains its index after the
	// load instead (scopedDepIndex), from the paths the roots actually use.
	var depIndex map[string]*packages.Package
	if !goTypesNeedDepsClosure() && !useHandleRoots {
		depIndexStart := time.Now()
		var depErr error
		depIndex, depErr = p.loadDepModuleIndex(ctx, loadDir)
		if depErr != nil && p.logger != nil {
			p.logger.Warn("go-types: dependency metadata index failed; external classification degraded for this pass",
				zap.String("repo_prefix", repoPrefix),
				zap.Error(depErr))
		}
		if depErr == nil && p.logger != nil {
			p.logger.Info("go-types: dependency metadata index loaded",
				zap.String("repo_prefix", repoPrefix),
				zap.Int("packages", len(depIndex)),
				zap.Duration("elapsed", time.Since(depIndexStart)))
		}
		compiler.IndexMs = time.Since(depIndexStart).Milliseconds()
	}

	gateWaitStart := time.Now()
	// A handle-rooted load is bounded by the edit's packages, not the
	// repository, so it is not "large" and shares the heavy gate.
	largeAdmission := largeGoTypesAdmission(ctx, !useHandleRoots)
	releaseAdmission, err := p.acquireHeavy(ctx, largeAdmission)
	if err != nil {
		return nil, err
	}
	var (
		pkgs                       []*packages.Package
		fset                       *token.FileSet
		externals                  *externalsAttribution
		objToNode                  map[types.Object]string
		repoNodes                  []*graph.Node
		nodesByFile                map[string][]*graph.Node
		funcIndexByFile            map[string]*fileFuncIndex
		loadAttempted              bool
		compilerProjectionComplete bool
		projectedUseCount          int
		usePackagesScanned         int
		usePackagesSkipped         int
		useIdentsScanned           int
		useIdentsSkipped           int
		// releaseCheckoutState unlocks the checkout's retained compiler
		// state a cached handle-rooted load read from; nil otherwise.
		releaseCheckoutState func()
	)
	var compilerHeapBaseline runtime.MemStats
	if largeAdmission {
		runtime.ReadMemStats(&compilerHeapBaseline)
	}

	// This is the only path that returns the heavy/large admission token. Every
	// success, error, and cancellation first severs compiler roots. Abnormal
	// post-load exits collect unconditionally so a canceled multi-GiB program
	// cannot overlap the next admitted compiler even when heap-growth sampling
	// was obscured by a concurrent process GC.
	releaseCompiler := sync.OnceFunc(func() {
		releaseStart := time.Now()
		heapBeforeRelease := compilerHeapBaseline
		if largeAdmission {
			runtime.ReadMemStats(&heapBeforeRelease)
		}
		heapGrowth := uint64(0)
		if heapBeforeRelease.HeapAlloc > compilerHeapBaseline.HeapAlloc {
			heapGrowth = heapBeforeRelease.HeapAlloc - compilerHeapBaseline.HeapAlloc
		}
		forceGC := loadAttempted && !compilerProjectionComplete
		if !forceGC {
			forceGC = shouldReclaimLargeCompiler(largeAdmission, heapGrowth, projectedUseCount)
		}
		heapAfterRelease := heapBeforeRelease

		collectCompiler := p.compilerGC
		if collectCompiler == nil {
			collectCompiler = debug.FreeOSMemory
		}
		reclaimAndReleaseGoCompiler(func() {
			clearGoPackageCompilerRoots(pkgs)
			pkgs = nil
			fset = nil
			if externals != nil {
				externals.releaseCompilerReferences()
				externals.moduleByPath = nil
				externals.knownNodeIDs = nil
				externals.useByNodeID = nil
			}
			clear(objToNode)
			objToNode = nil
			depIndex = nil
			repoNodes = nil
			nodesByFile = nil
			funcIndexByFile = nil
			if releaseCheckoutState != nil {
				releaseCheckoutState()
				releaseCheckoutState = nil
			}
		}, forceGC, collectCompiler, func() {
			if forceGC {
				runtime.ReadMemStats(&heapAfterRelease)
			}
		}, releaseAdmission)
		if p.logger != nil {
			p.logger.Info("go-types: compiler state released before graph apply",
				zap.String("repo_prefix", repoPrefix),
				zap.Bool("large_exclusive", largeAdmission),
				zap.Bool("projection_complete", compilerProjectionComplete),
				zap.Bool("forced_gc", forceGC),
				zap.Int("projected_uses", projectedUseCount),
				zap.Int("use_packages_scanned", usePackagesScanned),
				zap.Int("use_packages_skipped", usePackagesSkipped),
				zap.Int("use_idents_scanned", useIdentsScanned),
				zap.Int("use_idents_skipped", useIdentsSkipped),
				zap.Uint64("heap_alloc_before_load", compilerHeapBaseline.HeapAlloc),
				zap.Uint64("heap_alloc_before_release", heapBeforeRelease.HeapAlloc),
				zap.Uint64("heap_growth", heapGrowth),
				zap.Uint64("heap_alloc_after_release", heapAfterRelease.HeapAlloc),
				zap.Duration("elapsed", time.Since(releaseStart)))
		}
	})
	defer releaseCompiler()
	if wait := time.Since(gateWaitStart); wait > 5*time.Second && p.logger != nil {
		p.logger.Info("go-types: admission gate acquired after queueing",
			zap.String("repo_prefix", repoPrefix),
			zap.Bool("large_exclusive", largeAdmission),
			zap.Duration("queued", wait))
	}

	// Load one compiler program under the heavyweight admission gate. The
	// program remains local to this call and becomes unreachable on return.
	// The load is minutes-long on a big module; bracket it so the log never
	// goes silent between the manager's "starting" line and the result.
	loadStart := time.Now()
	loadUsage := readProcessUsage()
	patterns := []string{"./..."}
	var parseFile func(*token.FileSet, string, []byte) (*ast.File, error)
	if useHandleRoots {
		patterns = roots.patterns
		if checkoutScope.StripSiblingBodies {
			parseFile = stripSiblingBodiesParser(roots.handleAbs)
		}
	}
	if p.logger != nil {
		p.logger.Info("go-types: package load starting",
			zap.String("repo_prefix", repoPrefix),
			zap.Bool("large_exclusive", largeAdmission),
			zap.String("root", absRoot),
			zap.String("module_dir", loadDir),
			zap.String("scope", compiler.Scope),
			zap.String("scope_reason", compiler.ScopeReason),
			zap.Int("root_patterns", len(patterns)))
	}
	// absRoot stays the relativization base for every graph path below; only
	// the directory go/packages runs in follows the module.
	loadAttempted = true
	var (
		program compilerProgram
		closure map[string]*packages.Package
	)
	if useHandleRoots && checkoutScope.TypecheckCache {
		// The checkout's retained closure and dependency types. The state
		// stays locked until the compiler program is released (every exit
		// path goes through releaseCompiler).
		cacheStats := &semantic.CompilerCacheStats{}
		compiler.Cache = cacheStats
		var cached cachedProgram
		cached, err = p.loadCheckoutProgramCached(ctx, loadDir, manifestDigest, roots, checkoutScope, cacheStats)
		releaseCheckoutState = cached.release
		if err == nil && cached.bypass != "" {
			releaseCheckoutState()
			releaseCheckoutState = nil
			program, err = p.loadCompilerProgram(ctx, loadDir, parseFile, patterns...)
		} else if err == nil {
			program, closure = cached.program, cached.closure
		}
	} else {
		program, err = p.loadCompilerProgram(ctx, loadDir, parseFile, patterns...)
	}
	compiler.Loads++
	if useHandleRoots && ctx.Err() == nil {
		var missing []string
		if err == nil {
			missing = missingRootDirs(roots.rootDirs, program.raw)
		}
		if err != nil || len(missing) > 0 {
			// One whole-module retry, before anything is written. Never after
			// cancellation: a canceled pass returns its context error below.
			if p.logger != nil {
				p.logger.Warn("go-types: handle-rooted load incomplete; retrying the whole module",
					zap.String("repo_prefix", repoPrefix),
					zap.Strings("missing_roots", missing),
					zap.Error(err))
			}
			clearGoPackageCompilerRoots(program.raw)
			program = compilerProgram{}
			closure = nil
			if releaseCheckoutState != nil {
				releaseCheckoutState()
				releaseCheckoutState = nil
			}
			useHandleRoots = false
			compiler.Scope = semantic.CompilerScopeFull
			compiler.ScopeReason = semantic.CompilerScopeReasonScopedThenFull
			program, err = p.loadCompilerProgram(ctx, loadDir, nil, "./...")
			compiler.Loads++
			if err == nil && !goTypesNeedDepsClosure() {
				depIndexStart := time.Now()
				var depErr error
				depIndex, depErr = p.loadDepModuleIndex(ctx, loadDir)
				if depErr != nil && p.logger != nil {
					p.logger.Warn("go-types: dependency metadata index failed; external classification degraded for this pass",
						zap.String("repo_prefix", repoPrefix),
						zap.Error(depErr))
				}
				compiler.IndexMs += time.Since(depIndexStart).Milliseconds()
			}
		}
	}
	compiler.LoadMs = time.Since(loadStart).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	pkgs, fset = program.pkgs, program.fset
	program.raw = nil
	compiler.Packages, compiler.Files = program.packages, program.files
	if useHandleRoots && !goTypesNeedDepsClosure() {
		var (
			indexElapsed time.Duration
			depErr       error
		)
		depIndex, compiler.IndexCached, indexElapsed, depErr = p.scopedDepIndex(ctx, loadDir, manifestDigest, pkgs, closure)
		compiler.IndexMs += indexElapsed.Milliseconds()
		if depErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if p.logger != nil {
				p.logger.Warn("go-types: dependency metadata index failed; external classification degraded for this pass",
					zap.String("repo_prefix", repoPrefix),
					zap.Error(depErr))
			}
		}
	}
	loadErrs := classifyLoadErrors(pkgs)
	if p.logger != nil {
		fields := []zap.Field{
			zap.String("repo_prefix", repoPrefix),
			zap.Int("packages", len(pkgs)),
			zap.Int("compiled_files", compiler.Files),
			zap.Int("loads", compiler.Loads),
			zap.String("scope", compiler.Scope),
			zap.String("scope_reason", compiler.ScopeReason),
			zap.Bool("index_cached", compiler.IndexCached),
			zap.Int64("index_ms", compiler.IndexMs),
			zap.Int("soft_type_errors", loadErrs.soft),
			zap.Duration("elapsed", time.Since(loadStart)),
		}
		if c := compiler.Cache; c != nil {
			// The checkout's retained compiler state: whether this load
			// listed the closure (and why) or served it from memory.
			fields = append(fields,
				zap.Int("closure_hits", c.ClosureHits),
				zap.Int("closure_misses", c.ClosureMisses),
				zap.String("miss_reason", c.MissReason),
				zap.String("miss_package", c.MissPackage),
				zap.String("bypass", c.Bypass),
				zap.Int64("go_list_ms", c.GoListMs),
				zap.Int64("validate_ms", c.ValidateMs),
				zap.Int64("parse_ms", c.ParseMs),
				zap.Int64("check_ms", c.CheckMs),
				zap.Int("files_parsed", c.FilesParsed),
				zap.Int("files_reused", c.FilesReused),
				zap.Int("export_reads", c.ExportReads),
				zap.Int("changed_dependencies", c.ChangedDependencies),
				zap.Int("source_dependencies", c.SourceDependencies),
				zap.Int("source_dependency_files", c.SourceDependencyFiles),
				zap.Int64("source_dependency_parse_ms", c.SourceDependencyParseMs),
				zap.String("source_dependency_fallback", c.SourceDependencyFallback),
				zap.Int("retained_kept", c.RetainedKept),
				zap.Bool("warm_served", c.WarmServed),
				zap.Int("working_set_packages", c.WorkingSetPackages),
				zap.Int("state_packages", c.StatePackages),
				zap.Int64("state_bytes", c.StateBytes),
				zap.Bool("state_evicted", c.StateEvicted),
				zap.Int("evicted_packages", c.EvictedPackages),
				zap.Int("evicted_files", c.EvictedFiles),
				zap.Int("relist_scheduled", c.RelistScheduled),
				zap.Int64("targeted_wait_ms", c.TargetedWaitMs),
				zap.String("targeted_wait", c.TargetedWait),
				zap.String("warmup_state", c.WarmupState))
		}
		used := readProcessUsage().since(loadUsage)
		fields = append(fields, zap.Int64("major_faults", used.majorFaults), zap.Int64("minor_faults", used.minorFaults),
			zap.Float64("cpu_ms", float64(used.cpu.Microseconds())/1000))
		p.logger.Info("go-types: package load done", fields...)
	}
	// A pass that loads zero (or only broken) packages completes "cleanly"
	// with zero yield, which is indistinguishable in the result log from a
	// healthy no-op — surface it so a real repo silently enriching nothing
	// reads as the load failure it is. Soft type errors (an import or a
	// variable declared and not used) do not degrade a load: stripping
	// sibling bodies produces them by design, and they change no type.
	if p.logger != nil {
		if len(pkgs) == 0 || loadErrs.hard > 0 {
			p.logger.Warn("go-types: package load degraded",
				zap.String("repo_prefix", repoPrefix),
				zap.String("root", absRoot),
				zap.Int("packages", len(pkgs)),
				zap.Int("load_errors", loadErrs.hard),
				zap.Int("soft_type_errors", loadErrs.soft),
				zap.Any("load_error_kinds", loadErrs.kinds),
				zap.Strings("load_error_sample", loadErrs.sample))
		} else if loadErrs.soft > 0 {
			p.logger.Debug("go-types: package load soft type errors",
				zap.String("repo_prefix", repoPrefix),
				zap.Int("soft_type_errors", loadErrs.soft),
				zap.Strings("load_error_sample", loadErrs.sample))
		}
	}

	// Project compiler state to compact strings before any graph work. SQLite
	// replaces the repo atomically; the provider mirrors it in a small indexed
	// map for the in-memory backend and compatibility lookup path.
	bindings := buildSemanticBindingTypes(pkgs, fset, absRoot, repoPrefix)
	if checkoutPass {
		// A generation handle's binding rows describe the files it carries.
		// Every other file's rows compose from the layers beneath, keyed by
		// file; writing them here too would duplicate them in the view.
		bindings = filterBindingsToFiles(bindings, handleFiles)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Keep the heavyweight admission lease for the complete lifetime of pkgs
	// and fset, including any warmup apply-barrier park. Releasing it here let
	// every enrichment-pool lane accumulate a completed compiler program while
	// the resolver held the apply gate; four such programs retained several
	// GiB for more than twelve minutes in a measured cold workspace. The
	// deferred release above remains cancellation-safe on every exit path.

	// Warmup apply barrier: everything below reads or writes graph state, and
	// a giant apply landing mid-resolve starves the resolver on the shared
	// ResolveMutex. While the enrichment pool overlaps the resolve phase this
	// parks until the resolver finishes.
	applyGateStart := time.Now()
	if err := semantic.ApplyGateWait(ctx); err != nil {
		return nil, err
	}
	applyGateParked := time.Since(applyGateStart)
	if applyGateParked > 2*time.Second && p.logger != nil {
		p.logger.Info("go-types: apply gate opened after park",
			zap.String("repo_prefix", repoPrefix),
			zap.Duration("parked", applyGateParked))
	}
	_, persistentBindings := g.(graph.SemanticBindingTypeStore)

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: "go",
		Compiler: compiler,
	}

	// Keep compiler/heavy admission for the complete lifetime of pkgs, but hold
	// the graph resolve mutex only for bounded store-dependent slices. CPU-only
	// compiler walks use detached projections between slices, allowing other
	// repositories and resolvers to make forward progress.
	resolveSlices := &resolveLockSlices{mu: g.ResolveMutex()}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Apply-subphase instrumentation: one summary line per repo at apply
	// completion separates total apply wall time from actual graph-lock wait,
	// aggregate hold time, longest hold, and slice count. refs_walk INCLUDES
	// its inner write times (add_batch / reindex / confirm).
	applyStarted := time.Now()
	applyUsage, applyStore := readProcessUsage(), storeIOMarkOf(g)
	var applyProjectionDur, applyDefsDur, applyRefsDur time.Duration
	var applyAddBatchDur, applyReindexDur, applyConfirmDur time.Duration
	var applyImplementsDur, applyStampsDur time.Duration

	// Materialize this repository's Go nodes once and reuse them across every
	// compiler definition/use. The read is serialized with graph mutation, then
	// shallow detached so the CPU-only matching walks never retain or observe
	// mutable backend-owned node objects after the lock slice ends.
	projectionStart := time.Now()
	if checkoutPass {
		// Read once before admission; the handle is this pass's own
		// generation, so nothing else writes it in between.
		repoNodes = earlyNodes
	} else if err := resolveSlices.with(ctx, func() error {
		repoNodes = detachGoNodeProjection(repoGoNodes(g, repoPrefix))
		return nil
	}); err != nil {
		return nil, err
	}
	nodesByFile = make(map[string][]*graph.Node)
	nodesByID := make(map[string]*graph.Node, len(repoNodes))
	for _, node := range repoNodes {
		if node == nil {
			continue
		}
		nodesByID[node.ID] = node
		if node.FilePath != "" {
			nodesByFile[node.FilePath] = append(nodesByFile[node.FilePath], node)
		}
	}

	// Per-file containing-function indexes for the two use walks below: the
	// per-use linear scan over a file's node slice was a flat 28.8s per
	// profiling window on a large module.
	funcIndexByFile = buildFileFuncIndexes(nodesByFile)
	applyProjectionDur = time.Since(projectionStart)

	// Build the compiler-object → graph-node map used by later phases.
	objToNode = make(map[types.Object]string)

	// Phase 1: Map definitions.
	defsStart := time.Now()
	passPaths := newGraphPaths(absRoot, repoPrefix)
	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pkg.TypesInfo == nil {
			continue
		}

		// Only a file the projection holds nodes for can map a definition;
		// a checkout pass's root package parses hundreds of sibling files
		// for their declarations alone, and their syntax contexts and
		// definitions were walked for nothing.
		definitionContexts := buildGoDefinitionContexts(graphVisibleSyntax(pkg.Syntax, fset, absRoot, repoPrefix, nodesByFile))
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Pos() == token.NoPos {
				continue
			}

			pos := fset.Position(ident.Pos())
			graphPath := passPaths.of(pos.Filename)
			if graphPath == "" {
				continue
			}

			fileNodes := nodesByFile[graphPath]
			if len(fileNodes) == 0 {
				continue
			}
			node := matchRepoDefinitionNode(fileNodes, pos, ident.Name, obj, definitionContexts[ident])
			if node != nil {
				objToNode[obj] = node.ID
				result.SymbolsCovered++
			}
		}
	}

	// Count total Go symbols in this repo via the indexed repo-scoped scan
	// rather than a whole-graph AllNodes walk (which, in a multi-repo graph,
	// also wrongly counted every other repo's Go nodes against this repo's
	// coverage).
	for _, n := range repoNodes {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			result.SymbolsTotal++
		}
	}
	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	// Externals attribution: every Use of an external symbol becomes
	// an EdgeCalls / EdgeReferences targeting a freshly materialised
	// `ext::go:<importPath>::<name>` node, which itself carries an
	// EdgeDependsOnModule to the owning KindModule. Previously the
	// resolver left these calls pointing at stub strings
	// (`stdlib::fmt::Println`, `dep::github.com/.../foo::Bar`) that no
	// node holds; goanalysis upgrades them to real graph nodes with
	// LSP-grade origin.
	applyDefsDur = time.Since(defsStart)

	// The remaining compiler-only consumers are projected before graph writes.
	// Once Uses are detached below, packages.Package can release every AST/type
	// field instead of pinning the compiler heap for the full SQLite apply.
	stampPlanStart := time.Now()
	stamps, err := buildGoNodeStamps(ctx, pkgs, fset, absRoot, repoPrefix, nodesByFile)
	if err != nil {
		return nil, err
	}
	stampPlanDur := time.Since(stampPlanStart)
	implementsPlanStart := time.Now()
	interfaceIDs := implementationInterfaceIDs(objToNode, nodesByID)
	missingPairs := missingImplementationPairs(objToNode, nodesByID)
	implementsPlanDur := time.Since(implementsPlanStart)

	// A checkout pass's handle carries only the files its generation
	// re-derived. A use in one of them that binds to a declaration in an
	// unchanged file of the module finds no node on the handle; map it onto
	// the node the layer below serves there. This runs after the implements
	// plan on purpose: those declarations only receive the handle's uses,
	// they are not re-derived here.
	// The reader is the layer below, not the handle, so no handle lock slice
	// is taken for it; coverage stays the handle's own.
	if checkoutPass && checkoutScope.Declarations != nil {
		mapCheckoutContextDeclarations(pkgs, fset, passPaths, handleFiles, objToNode, checkoutScope.Declarations)
	}

	refsStart := time.Now()
	externals = newExternalsAttribution(g, pkgs, p.Name(), repoPrefix, depIndex)
	// Prefetch only the externals a projected use can name: resolveGoUse
	// drops a use outside a function of a file the projection indexes
	// before it asks the externals attribution for anything.
	externalNodeIDs := externals.existingNodeIDs(pkgs, objToNode, func(ident *ast.Ident) bool {
		graphPath := passPaths.of(fset.Position(ident.Pos()).Filename)
		return graphPath != "" && funcIndexByFile[graphPath] != nil
	})
	if err := resolveSlices.with(ctx, func() error {
		externals.prefetchExistingNodeIDs(externalNodeIDs)
		return nil
	}); err != nil {
		return nil, err
	}

	// Detach every compiler Use package by package before the first graph write.
	// The compact plans retain graph IDs, locations, inferred kinds, and interned
	// external string identities only. Clearing each packages.Package afterwards
	// breaks AST/types/import closures while preserving Name/PkgPath/Module for
	// the externals metadata map.
	usePlan, useStats, err := projectGoUsesAndReleaseCompilerStateWithStats(
		ctx, pkgs, fset, absRoot, repoPrefix, funcIndexByFile, objToNode, externals,
	)
	if err != nil {
		return nil, err
	}
	defer usePlan.release()
	projectedUseCount = usePlan.len()
	usePackagesScanned = useStats.packagesScanned
	usePackagesSkipped = useStats.packagesSkipped
	useIdentsScanned = useStats.identsScanned
	useIdentsSkipped = useStats.identsSkipped
	compilerProjectionComplete = true
	externalNodeIDs = nil

	// Every compiler-only consumer has returned. The shared cleanup severs all
	// aliases, conditionally reclaims the normal success path, and only then
	// returns the heavy/large admission token. Its deferred invocation covers
	// every earlier error and cancellation with unconditional post-load
	// reclamation. Detached graph work can overlap only after that release;
	// individual store mutations remain serialized by ResolveMutex/writeMu.
	releaseCompiler()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Binding rows are a compact compiler projection. Their repository-scoped
	// replacement is atomic and serialized by the store's own write gate.
	if writer, ok := g.(graph.SemanticBindingTypeWriter); ok {
		if err := writer.ReplaceSemanticBindingTypes(repoPrefix, bindings); err != nil {
			return nil, fmt.Errorf("persist semantic binding types: %w", err)
		}
	}
	// A cancellation after the atomic SQLite replace may leave these compact,
	// compiler-valid rows available, but it must not publish transient provider
	// state or a completion marker. The retry replaces the repo atomically.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !persistentBindings {
		if checkoutPass {
			p.replaceBindingIndex(absRoot, handleFileList, bindings)
		} else {
			p.replaceBindingIndex(absRoot, nil, bindings)
		}
	}

	// Projection may discover every external symbol before the first Use page.
	// Fixed pages commit each genuinely new symbol together with its module link;
	// remaining exact-identity pages repair links whose node survived an earlier
	// canceled pass without creating an unbounded first mutex hold.
	const projectedExternalApplyChunkSize = 512
	for len(externals.pendingNodes) > 0 {
		if err := resolveSlices.with(ctx, func() error {
			nodes := externals.drainPendingNodes(projectedExternalApplyChunkSize)
			edges := externals.drainPendingEdgesForNodes(nodes)
			if len(nodes) > 0 || len(edges) > 0 {
				addBatchStart := time.Now()
				g.AddBatch(nodes, edges)
				applyAddBatchDur += time.Since(addBatchStart)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for len(externals.pendingEdges) > 0 {
		if err := resolveSlices.with(ctx, func() error {
			edges := externals.drainPendingEdges(projectedExternalApplyChunkSize)
			if len(edges) > 0 {
				addBatchStart := time.Now()
				g.AddBatch(nil, edges)
				applyAddBatchDur += time.Since(addBatchStart)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	// Phase 2: Apply the detached package plans. Candidate lookup and mutation
	// remain bounded; unlike the former interleaved walk, no page can retain a
	// types.Object, AST, or packages.Package.
	applyStart := time.Now()
	lastApplyLog := applyStart
	for pkgIndex, resolvedUses := range usePlan.packages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.logger != nil && time.Since(lastApplyLog) > 15*time.Second {
			lastApplyLog = time.Now()
			p.logger.Info("go-types: apply progress",
				zap.String("repo_prefix", repoPrefix),
				zap.Int("packages_done", pkgIndex),
				zap.Int("packages_total", len(usePlan.packages)),
				zap.Int("confirmed", result.EdgesConfirmed),
				zap.Int("added", result.EdgesAdded),
				zap.Duration("elapsed", time.Since(applyStart)))
		}

		const goUseApplyChunkSize = 512
		for chunkStart := 0; chunkStart < len(resolvedUses); chunkStart += goUseApplyChunkSize {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			chunkEnd := min(chunkStart+goUseApplyChunkSize, len(resolvedUses))
			chunk := resolvedUses[chunkStart:chunkEnd]
			endpointSet := make(map[graph.EdgeEndpoint]struct{}, len(chunk))
			siteSet := make(map[graph.EdgeSite]struct{})
			for _, use := range chunk {
				endpointSet[graph.EdgeEndpoint{From: use.callerID, To: use.targetNodeID}] = struct{}{}
				if use.external == nil {
					continue
				}
				for _, target := range use.external.stubTargets {
					endpointSet[graph.EdgeEndpoint{From: use.callerID, To: target}] = struct{}{}
				}
				siteSet[graph.EdgeSite{
					From: use.callerID,
					Line: use.line,
					Kind: use.kind,
				}] = struct{}{}
			}

			endpoints := make([]graph.EdgeEndpoint, 0, len(endpointSet))
			for key := range endpointSet {
				endpoints = append(endpoints, key)
			}
			sites := make([]graph.EdgeSite, 0, len(siteSet))
			for key := range siteSet {
				sites = append(sites, key)
			}

			if err := resolveSlices.with(ctx, func() error {
				lookupStarted := time.Now()
				candidates := graph.LookupEdgeCandidates(g, endpoints, sites)
				lookupElapsed := time.Since(lookupStarted)
				// Slow-page records expose candidate reads that were previously
				// included only in refs_walk and mutex_held. They deliberately
				// exclude lock wait and do not claim to measure all lookup time.
				if p.logger != nil && lookupElapsed >= time.Second {
					p.logger.Info("go-types: slow candidate lookup",
						zap.String("repo_prefix", repoPrefix),
						zap.String("backend", fmt.Sprintf("%T", g)),
						zap.Int("package_index", pkgIndex),
						zap.Int("use_offset", chunkStart),
						zap.Int("uses", len(chunk)),
						zap.Int("endpoint_requests", len(endpoints)),
						zap.Int("site_requests", len(sites)),
						zap.Duration("elapsed", lookupElapsed))
				}
				externals.edgeCandidates = &candidates
				defer func() { externals.edgeCandidates = nil }()

				var confirmedEdges []*graph.Edge
				var newEdges []*graph.Edge
				for _, use := range chunk {
					// External: claim a resolver-stub edge if one exists, else add
					// a fresh edge. Internal: confirm or add as before.
					if use.external != nil {
						if upgraded := externals.claimAndUpgradeProjectedStub(use.callerID, use.external, use.targetNodeID, use.line); upgraded != nil {
							result.EdgesConfirmed++
							continue
						}
						existing := candidates.EndpointKind(use.callerID, use.targetNodeID, use.kind)
						if existing != nil {
							if existing.Confidence < 1.0 {
								semantic.ConfirmEdge(existing, p.Name())
								confirmedEdges = append(confirmedEdges, existing)
								result.EdgesConfirmed++
							}
							continue
						}
						if use.kind != "" {
							edge := semantic.NewSemanticEdge(use.callerID, use.targetNodeID, use.kind,
								use.graphPath, use.line, p.Name())
							candidates.Add(edge)
							newEdges = append(newEdges, edge)
							result.EdgesAdded++
						}
						continue
					}

					existing := candidates.EndpointKind(use.callerID, use.targetNodeID, use.kind)
					if existing != nil {
						if existing.Confidence < 1.0 {
							semantic.ConfirmEdge(existing, p.Name())
							confirmedEdges = append(confirmedEdges, existing)
							result.EdgesConfirmed++
						}
					} else {
						if use.kind != "" {
							edge := semantic.NewSemanticEdge(use.callerID, use.targetNodeID, use.kind,
								use.graphPath, use.line, p.Name())
							candidates.Add(edge)
							newEdges = append(newEdges, edge)
							result.EdgesAdded++
						}
					}
				}

				externalNodes, externalEdges := externals.drainPendingAdds()
				allNewEdges := make([]*graph.Edge, 0, len(externalEdges)+len(newEdges))
				allNewEdges = append(allNewEdges, externalEdges...)
				allNewEdges = append(allNewEdges, newEdges...)
				if len(externalNodes) > 0 || len(allNewEdges) > 0 {
					addBatchStart := time.Now()
					g.AddBatch(externalNodes, allNewEdges)
					applyAddBatchDur += time.Since(addBatchStart)
				}
				if reindexes := externals.drainPendingReindexes(); len(reindexes) > 0 {
					reindexStart := time.Now()
					g.ReindexEdges(reindexes)
					applyReindexDur += time.Since(reindexStart)
				}

				confirmStart := time.Now()
				persistConfirmedEdges(g, confirmedEdges)
				applyConfirmDur += time.Since(confirmStart)
				return nil
			}); err != nil {
				return nil, err
			}
		}
		usePlan.packages[pkgIndex] = nil
	}
	usePlan.release()

	// Stitch the externals counters into the standard result. NodesEnriched
	// previously only incremented for in-repo type-meta enrichment; here
	// we surface the synthetic external + module nodes the externals
	// pass added so callers can see the full graph delta in one number.
	result.EdgesAdded += externals.edgesAdded + externals.edgesUpgraded
	result.NodesEnriched += externals.nodesAdded

	applyRefsDur = time.Since(refsStart)

	// Phase 3: Interface implementations via go/types. Keep the existing and
	// missing-edge operations in separate lock slices so unrelated graph work
	// can interleave between their bounded reads and writes.
	implementsStart := time.Now()
	const goImplementsApplyChunkSize = 512
	for start := 0; start < len(interfaceIDs); start += goImplementsApplyChunkSize {
		end := min(start+goImplementsApplyChunkSize, len(interfaceIDs))
		chunk := interfaceIDs[start:end]
		var implementsConfirmed int
		if err := resolveSlices.with(ctx, func() error {
			implementsConfirmed = p.enrichImplementsByIDs(g, chunk)
			return nil
		}); err != nil {
			return nil, err
		}
		result.EdgesConfirmed += implementsConfirmed
	}
	for start := 0; start < len(missingPairs); start += goImplementsApplyChunkSize {
		end := min(start+goImplementsApplyChunkSize, len(missingPairs))
		chunk := missingPairs[start:end]
		var implementsAdded int
		if err := resolveSlices.with(ctx, func() error {
			implementsAdded = p.addMissingImplementationPairs(g, chunk, nodesByID)
			return nil
		}); err != nil {
			return nil, err
		}
		result.EdgesAdded += implementsAdded
	}
	applyImplementsDur = implementsPlanDur + time.Since(implementsStart)
	stampsPersistStart := time.Now()

	// Phase 4 persists the string-only stamp plan projected before compiler
	// release. SQLite still refetches full nodes in bounded batches, preserving
	// opaque metadata exactly as before.
	persistedStamps, err := persistGoNodeStampsSliced(ctx, g, stamps, p.Name(), resolveSlices)
	if err != nil {
		return nil, err
	}
	result.NodesEnriched += persistedStamps
	applyStampsDur = stampPlanDur + time.Since(stampsPersistStart)

	result.LockWaitMs = resolveSlices.waited.Milliseconds()
	if p.logger != nil {
		used := readProcessUsage().since(applyUsage)
		storeIO := storeIOSince(g, applyStore)
		p.logger.Info("go-types: apply subphases",
			zap.String("repo_prefix", repoPrefix),
			zap.Int64("major_faults", used.majorFaults), zap.Int64("minor_faults", used.minorFaults),
			zap.Float64("cpu_ms", float64(used.cpu.Microseconds())/1000),
			zap.Any("store_io", storeIO),
			zap.Duration("gate_parked", applyGateParked),
			zap.Duration("apply_wall", time.Since(applyStarted)),
			zap.Duration("mutex_waited", resolveSlices.waited),
			zap.Duration("mutex_held", resolveSlices.held),
			zap.Duration("mutex_max_held", resolveSlices.maxHeld),
			zap.Int("mutex_slices", resolveSlices.count),
			zap.Duration("graph_projection", applyProjectionDur),
			zap.Duration("defs_walk", applyDefsDur),
			zap.Duration("refs_walk", applyRefsDur),
			zap.Duration("add_batch", applyAddBatchDur),
			zap.Duration("reindex", applyReindexDur),
			zap.Duration("confirm_persist", applyConfirmDur),
			zap.Duration("implements", applyImplementsDur),
			zap.Duration("type_stamps", applyStampsDur))
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	repoPrefix := ""
	if nodes := g.GetFileNodes(filePath); len(nodes) > 0 && nodes[0] != nil {
		repoPrefix = nodes[0].RepoPrefix
	}
	return p.EnrichFiles(g, repoPrefix, repoRoot, []string{filePath})
}

// EnrichFiles loads every changed Go file pattern in one compiler invocation.
// go/packages coalesces files from the same package while still accepting
// patterns from different packages, so the batch avoids both N loads and a
// repository-wide ./... fallback.
func (p *Provider) EnrichFiles(g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*semantic.EnrichResult, error) {
	return p.EnrichFilesContext(context.Background(), g, repoPrefix, repoRoot, filePaths)
}

// EnrichFilesContext performs the partial compiler load under the manager's
// deadline. packages.Load and the heavyweight admission gate both observe the
// same cancellation, so an expired partial pass cannot remain stuck behind a
// full-repository program or continue consuming memory after abandonment.
func (p *Provider) EnrichFilesContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*semantic.EnrichResult, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := p.acquireHeavy(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("absolute path: %w", err)
	}

	prefix := normalizeRelPath(repoPrefix)
	seen := make(map[string]struct{}, len(filePaths))
	requestedFiles := make([]string, 0, len(filePaths))
	for _, filePath := range filePaths {
		graphPath := normalizeRelPath(filePath)
		if graphPath == "" {
			continue
		}
		if _, ok := seen[graphPath]; ok {
			continue
		}
		seen[graphPath] = struct{}{}
		requestedFiles = append(requestedFiles, graphPath)
	}
	sort.Strings(requestedFiles)
	if len(requestedFiles) == 0 {
		return &semantic.EnrichResult{Provider: p.Name(), Language: "go"}, nil
	}

	patterns := make([]string, 0, len(requestedFiles))
	for _, graphPath := range requestedFiles {
		relPath := graphPath
		if prefix != "" {
			relPath = strings.TrimPrefix(relPath, prefix+"/")
		}
		patterns = append(patterns, "file="+filepath.Join(absRoot, filepath.FromSlash(relPath)))
	}
	// The file= patterns are absolute, but Dir still selects the module whose
	// build list resolves them: a repository whose module lives in a
	// subdirectory resolves nothing from the root. absRoot remains the
	// relativization base for graph paths.
	fileLoadDir, _ := goLoadDir(absRoot)
	pkgs, fset, err := p.loadPackagesContext(ctx, fileLoadDir, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages for %d files: %w", len(requestedFiles), err)
	}

	rows := buildSemanticBindingTypes(pkgs, fset, absRoot, repoPrefix)
	loadedSet := make(map[string]struct{}, len(requestedFiles))
	for _, filePath := range requestedFiles {
		loadedSet[filePath] = struct{}{}
	}
	// Replace every main-module file returned in the loaded packages, not only
	// the triggering paths. A package sibling's inferred bindings can change
	// when one declaration changes, and this projection stays compact.
	for _, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		for _, syntax := range pkg.Syntax {
			if syntax == nil {
				continue
			}
			relPath := relativePath(fset.Position(syntax.Pos()).Filename, absRoot)
			if relPath == "" {
				continue
			}
			loadedSet[normalizeRelPath(scopedGraphPath(repoPrefix, relPath))] = struct{}{}
		}
	}
	loadedFiles := make([]string, 0, len(loadedSet))
	for filePath := range loadedSet {
		loadedFiles = append(loadedFiles, filePath)
	}
	sort.Strings(loadedFiles)

	filtered := rows[:0]
	for _, row := range rows {
		if _, ok := loadedSet[normalizeRelPath(row.Site.FilePath)]; ok {
			filtered = append(filtered, row)
		}
	}
	rows = filtered
	_, persistentBindings := g.(graph.SemanticBindingTypeStore)
	if writer, ok := g.(graph.SemanticBindingTypeWriter); ok {
		if err := writer.ReplaceSemanticBindingTypesForFiles(repoPrefix, loadedFiles, rows); err != nil {
			return nil, fmt.Errorf("persist semantic binding types for %d files: %w", len(loadedFiles), err)
		}
	}
	if !persistentBindings {
		p.replaceBindingIndex(absRoot, loadedFiles, rows)
	}
	return &semantic.EnrichResult{
		Provider:   p.Name(),
		Language:   "go",
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// LookupTypeAtLine returns the resolved type name of the first
// short_var_declaration / var_spec / typed declaration whose start
// line matches `line` in the file at `filePath`. Returns ("", false)
// when:
//   - Enrich hasn't been called (no cached state)
//   - filePath isn't in any loaded package
//   - no typed declaration is found at `line`
//   - the type can't be resolved via go/types
//
// This is the lsp_resolved upgrade tier referenced in
// spec-contract-extraction.md §4.5: when the goanalysis provider
// has run, the contract pipeline can ask for compiler-grade type
// resolution at any line in the indexed source.
func (p *Provider) LookupTypeAtLine(filePath string, line int) (string, bool) {
	normalized := normalizeRelPath(filePath)
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if typeName := p.bindingTypes[bindingLookupKey{filePath: normalized, line: line}]; typeName != "" {
		return typeName, true
	}

	// Compatibility only: the legacy interface has no repository argument.
	// Production contract extraction uses SemanticBindingTypes, whose exact key
	// includes RepoPrefix. Return a scoped match only when it is unambiguous.
	var found string
	for key, typeName := range p.bindingTypes {
		if key.filePath != normalized || key.line != line || key.name != "" || typeName == "" {
			continue
		}
		if found != "" && found != typeName {
			return "", false
		}
		found = typeName
	}
	return found, found != ""
}

// envPositiveInt reads a positive operator override.
func envPositiveInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func buildSemanticBindingTypes(pkgs []*packages.Package, fset *token.FileSet, absRoot, repoPrefix string) []graph.SemanticBindingType {
	bySite := make(map[graph.SemanticBindingSite]string)
	add := func(site graph.SemanticBindingSite, typeName string) {
		if typeName == "" {
			return
		}
		if _, exists := bySite[site]; !exists {
			bySite[site] = typeName
		}
	}

	for _, pkg := range pkgs {
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			if file == nil {
				continue
			}
			pos := fset.Position(file.Pos())
			relPath := relativePath(pos.Filename, absRoot)
			if relPath == "" {
				continue
			}
			graphPath := scopedGraphPath(repoPrefix, relPath)
			ast.Inspect(file, func(node ast.Node) bool {
				switch decl := node.(type) {
				case *ast.AssignStmt:
					line := fset.Position(decl.Pos()).Line
					add(graph.SemanticBindingSite{RepoPrefix: repoPrefix, FilePath: graphPath, Line: line}, typeNameFromAssign(decl, pkg.TypesInfo))
					for i, lhs := range decl.Lhs {
						ident, ok := lhs.(*ast.Ident)
						if !ok || ident.Name == "_" {
							continue
						}
						add(graph.SemanticBindingSite{
							RepoPrefix: repoPrefix,
							FilePath:   graphPath,
							Line:       fset.Position(ident.Pos()).Line,
							Name:       ident.Name,
						}, typeNameFromAssignIndex(decl, pkg.TypesInfo, i))
					}
				case *ast.GenDecl:
					line := fset.Position(decl.Pos()).Line
					add(graph.SemanticBindingSite{RepoPrefix: repoPrefix, FilePath: graphPath, Line: line}, typeNameFromGenDecl(decl, pkg.TypesInfo))
					for _, spec := range decl.Specs {
						valueSpec, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, ident := range valueSpec.Names {
							if ident == nil || ident.Name == "_" {
								continue
							}
							add(graph.SemanticBindingSite{
								RepoPrefix: repoPrefix,
								FilePath:   graphPath,
								Line:       fset.Position(ident.Pos()).Line,
								Name:       ident.Name,
							}, typeNameFromValueSpecIndex(valueSpec, pkg.TypesInfo, i))
						}
					}
				}
				return true
			})
		}
	}

	rows := make([]graph.SemanticBindingType, 0, len(bySite))
	for site, typeName := range bySite {
		rows = append(rows, graph.SemanticBindingType{Site: site, TypeName: typeName})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Site, rows[j].Site
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
	return rows
}

func typeNameFromAssignIndex(stmt *ast.AssignStmt, info *types.Info, i int) string {
	if i < 0 || i >= len(stmt.Lhs) || len(stmt.Rhs) == 0 {
		return ""
	}
	ident, ok := stmt.Lhs[i].(*ast.Ident)
	if !ok || ident.Name == "_" {
		return ""
	}
	obj := info.Defs[ident]
	if obj == nil {
		obj = info.Uses[ident]
	}
	if obj != nil {
		if name := unwrapTypeName(obj.Type()); name != "" {
			return name
		}
	}
	var rhs ast.Expr
	if i < len(stmt.Rhs) {
		rhs = stmt.Rhs[i]
	} else if len(stmt.Rhs) == 1 {
		rhs = stmt.Rhs[0]
	}
	if rhs != nil {
		if tv, ok := info.Types[rhs]; ok && tv.Type != nil {
			return unwrapTypeName(tv.Type)
		}
	}
	return ""
}

func typeNameFromValueSpecIndex(spec *ast.ValueSpec, info *types.Info, i int) string {
	if i < 0 || i >= len(spec.Names) {
		return ""
	}
	ident := spec.Names[i]
	if ident == nil || ident.Name == "_" {
		return ""
	}
	if obj := info.Defs[ident]; obj != nil {
		if name := unwrapTypeName(obj.Type()); name != "" {
			return name
		}
	}
	if spec.Type != nil {
		if tv, ok := info.Types[spec.Type]; ok && tv.Type != nil {
			if name := unwrapTypeName(tv.Type); name != "" {
				return name
			}
		}
	}
	if i < len(spec.Values) {
		if tv, ok := info.Types[spec.Values[i]]; ok && tv.Type != nil {
			return unwrapTypeName(tv.Type)
		}
	}
	return ""
}

// lookupTypeAtLineInFile walks the file's AST and returns the type
// name of the first declaration at `line` whose LHS the type info
// table has a type for.
func lookupTypeAtLineInFile(file *ast.File, info *types.Info, fset *token.FileSet, line int) (string, bool) {
	var found string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || found != "" {
			return false
		}
		startLine := fset.Position(n.Pos()).Line
		if startLine != line {
			// Keep descending if this node spans the target.
			endLine := fset.Position(n.End()).Line
			return startLine <= line && endLine >= line
		}
		// We're at the target line. Try to extract a type from the
		// most common declaration shapes.
		switch d := n.(type) {
		case *ast.AssignStmt:
			if name := typeNameFromAssign(d, info); name != "" {
				found = name
			}
		case *ast.GenDecl:
			if name := typeNameFromGenDecl(d, info); name != "" {
				found = name
			}
		case *ast.DeclStmt:
			if gd, ok := d.Decl.(*ast.GenDecl); ok {
				if name := typeNameFromGenDecl(gd, info); name != "" {
					found = name
				}
			}
		}
		return found == ""
	})
	return found, found != ""
}

// typeNameFromAssign reads the LHS type from a short var declaration
// (`x := f()` or `x := Foo{...}`). Returns the underlying named
// type's name.
func typeNameFromAssign(stmt *ast.AssignStmt, info *types.Info) string {
	if len(stmt.Lhs) == 0 || len(stmt.Rhs) == 0 {
		return ""
	}
	for i, lhs := range stmt.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name == "_" {
			continue
		}
		obj := info.Defs[ident]
		if obj == nil {
			obj = info.Uses[ident]
		}
		if obj != nil {
			if name := unwrapTypeName(obj.Type()); name != "" {
				return name
			}
		}
		// Fall back to the RHS expression's type.
		var rhs ast.Expr
		if i < len(stmt.Rhs) {
			rhs = stmt.Rhs[i]
		} else if len(stmt.Rhs) == 1 {
			rhs = stmt.Rhs[0]
		}
		if rhs != nil {
			if t, ok := info.Types[rhs]; ok && t.Type != nil {
				if name := unwrapTypeName(t.Type); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// typeNameFromGenDecl handles `var x Foo` / `var x = Foo{...}`.
func typeNameFromGenDecl(decl *ast.GenDecl, info *types.Info) string {
	for _, spec := range decl.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if name.Name == "_" {
				continue
			}
			obj := info.Defs[name]
			if obj != nil {
				if t := unwrapTypeName(obj.Type()); t != "" {
					return t
				}
			}
			if vs.Type != nil {
				if t, ok := info.Types[vs.Type]; ok && t.Type != nil {
					if u := unwrapTypeName(t.Type); u != "" {
						return u
					}
				}
			}
			if i < len(vs.Values) {
				if t, ok := info.Types[vs.Values[i]]; ok && t.Type != nil {
					if u := unwrapTypeName(t.Type); u != "" {
						return u
					}
				}
			}
		}
	}
	return ""
}

// unwrapTypeName strips slice/pointer/array wrappers and returns the
// underlying named type's bare name. Returns "" for primitives,
// interfaces, and untyped expressions.
func unwrapTypeName(t types.Type) string {
	if t == nil {
		return ""
	}
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Array:
			t = x.Elem()
		default:
			named, ok := t.(*types.Named)
			if !ok {
				return ""
			}
			return named.Obj().Name()
		}
	}
}

// normalizeRelPath collapses a/./b → a/b and uses forward slashes,
// so OS-dependent path separators don't trip the comparison.
func normalizeRelPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// loadPackages loads all Go packages in the given directory with type information.
func (p *Provider) loadPackages(dir string) ([]*packages.Package, *token.FileSet, error) {
	return p.loadPackagesContext(context.Background(), dir, "./...")
}

// loadDepModuleIndex enumerates the repository's full import closure in
// metadata-only mode (one `go list -deps`-class invocation — no compile, no
// typecheck, no syntax) and returns every transitively imported package
// indexed by import path, each carrying its Module classification. Under the
// default export-data load the main pass sees only ROOT packages, so this
// index is what lets the externals pass classify a dependency object as
// stdlib / module-cache / main exactly as the old full-closure load did.
// Runs OFF the heavy admission gate: it is memory-light and its cost is the
// `go list` walk the toolchain performs anyway.
func (p *Provider) loadDepModuleIndex(ctx context.Context, dir string) (map[string]*packages.Package, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedModule,
		Dir:     dir,
		Tests:   p.includeTest,
		Overlay: committedOverlay(ctx),
		Env:     committedEnv(ctx),
	}
	load := p.packagesLoad
	if load == nil {
		load = packages.Load
	}
	roots, err := load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	index := make(map[string]*packages.Package)
	var visit func(pkg *packages.Package)
	visit = func(pkg *packages.Package) {
		if pkg == nil || pkg.PkgPath == "" {
			return
		}
		if _, seen := index[pkg.PkgPath]; seen {
			return
		}
		index[pkg.PkgPath] = pkg
		for _, imp := range pkg.Imports {
			visit(imp)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return index, nil
}

// goTypesNeedDepsClosure reports whether the full-closure source typecheck is
// forced (GORTEX_GOTYPES_NEEDDEPS=1). Default OFF: root packages are
// type-checked from syntax while every dependency is imported from compiled
// export data (`go list -export`), which cuts both the load's CPU (no source
// typecheck of the closure) and its live heap (no dep ASTs/types graphs) by
// most of their former cost, and makes dep type information cacheable across
// repos through the shared GOCACHE. The env restores the old closure mode for
// A/B comparison or as an escape hatch.
func goTypesNeedDepsClosure() bool {
	v := os.Getenv("GORTEX_GOTYPES_NEEDDEPS")
	return v == "1" || strings.EqualFold(v, "true")
}

func (p *Provider) loadPackagesContext(ctx context.Context, dir string, patterns ...string) ([]*packages.Package, *token.FileSet, error) {
	program, err := p.loadCompilerProgram(ctx, dir, nil, patterns...)
	if err != nil {
		return nil, nil, err
	}
	return program.pkgs, program.fset, nil
}

// compilerProgram is one type-checking load: the packages the pass uses
// (those with type information), every package the load returned, and the
// load's compiler-context size.
type compilerProgram struct {
	pkgs []*packages.Package
	raw  []*packages.Package
	fset *token.FileSet
	// packages counts pkgs; files sums their compiled Go files.
	packages int
	files    int
}

// loadCompilerProgram is loadPackagesContext with the load's size and the
// unfiltered package list. parseFile, when non-nil, replaces go/packages'
// parser for the source-checked packages.
func (p *Provider) loadCompilerProgram(ctx context.Context, dir string, parseFile func(*token.FileSet, string, []byte) (*ast.File, error), patterns ...string) (compilerProgram, error) {
	// A compiler load is interactive work: a background warm-up listing
	// yields to it. A committed tree's load is background work itself: it
	// neither preempts nor holds back the working copy's warm-up.
	if committedPreempt(ctx) == nil {
		defer p.beginCompilerLoad(dir)()
	}
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedCompiledGoFiles |
		packages.NeedImports |
		packages.NeedTypes |
		packages.NeedTypesInfo |
		packages.NeedSyntax |
		// NeedModule populates pkg.Module so the externals pass can
		// classify imports as stdlib (Module == nil), module_cache
		// (Module != nil && !Main), or main (Module.Main). Without
		// it the loader returns nil for every Module field and we
		// can't tell stdlib calls from internal-package calls. Under
		// the default export-data mode it reaches only the ROOT
		// packages; dependency classification comes from the separate
		// metadata index (loadDepModuleIndex).
		packages.NeedModule
	if goTypesNeedDepsClosure() {
		mode |= packages.NeedDeps
	}

	cfg := &packages.Config{
		Context:   ctx,
		Mode:      mode,
		Dir:       dir,
		Tests:     p.includeTest,
		Fset:      token.NewFileSet(),
		ParseFile: parseFile,
		Overlay:   committedOverlay(ctx),
		Env:       committedEnv(ctx),
	}

	load := p.packagesLoad
	if load == nil {
		load = packages.Load
	}
	pkgs, err := load(cfg, patterns...)
	if err != nil {
		return compilerProgram{}, err
	}

	// Filter out packages with errors (they may have partial type info).
	var valid []*packages.Package
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			p.logger.Debug("package has errors, using partial info",
				zap.String("pkg", pkg.PkgPath),
				zap.Int("errors", len(pkg.Errors)),
			)
		}
		if pkg.TypesInfo != nil {
			valid = append(valid, pkg)
		}
	}

	program := compilerProgram{pkgs: valid, raw: pkgs, fset: cfg.Fset, packages: len(valid)}
	for _, pkg := range valid {
		program.files += len(pkg.CompiledGoFiles)
	}
	return program, nil
}

// repoGoNodes prefers the backend's repo+language summary projection so SQLite
// scans only identity/location columns: opaque Meta plus promoted docs and
// signatures never cross the driver boundary. Empty-prefix repositories remain
// explicitly scoped; backends without a summary fall back to light/full reads.
func repoGoNodes(g graph.Store, repoPrefix string) []*graph.Node {
	if reader, ok := g.(graph.RepoLanguageNodeSummaryReader); ok {
		return reader.GetRepoNodeSummariesByLanguage(repoPrefix, "go")
	}
	if reader, ok := g.(graph.LightNodeReader); ok {
		all := reader.GetRepoNodesLight(repoPrefix)
		out := make([]*graph.Node, 0, len(all))
		for _, node := range all {
			if node != nil && node.Language == "go" {
				out = append(out, node)
			}
		}
		return out
	}
	return g.GetRepoNodesByLanguage(repoPrefix, "go")
}

const goNodeStampChunkSize = 512

type goNodeStamp struct {
	semanticType string
	returnType   string
}

// buildGoNodeStamps projects the final compiler consumer to strings before the
// graph apply. Keeping this node-driven matching policy byte-for-byte equivalent
// to the former phase-4 loop preserves local/parameter precision while letting
// the package AST/type graphs die before SQLite mutation begins.
func buildGoNodeStamps(
	ctx context.Context,
	pkgs []*packages.Package,
	fset *token.FileSet,
	absRoot, repoPrefix string,
	nodesByFile map[string][]*graph.Node,
) (map[string]goNodeStamp, error) {
	type defEntry struct {
		line int
		obj  types.Object
	}
	defsByName := make(map[string][]defEntry) // key: rel \x00 name
	relSet := make(map[string]struct{})
	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pkg == nil || pkg.TypesInfo == nil {
			continue
		}
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Pos() == token.NoPos || ident.Name == "" || ident.Name == "_" {
				continue
			}
			pos := fset.Position(ident.Pos())
			rel := relativePath(pos.Filename, absRoot)
			if rel == "" {
				continue
			}
			// Stamps land on the projection's nodes only.
			if len(nodesByFile[scopedGraphPath(repoPrefix, rel)]) == 0 {
				continue
			}
			defsByName[rel+"\x00"+ident.Name] = append(defsByName[rel+"\x00"+ident.Name], defEntry{pos.Line, obj})
			relSet[rel] = struct{}{}
		}
	}

	stamps := make(map[string]goNodeStamp)
	for rel := range relSet {
		for _, node := range nodesByFile[scopedGraphPath(repoPrefix, rel)] {
			if node.Kind == graph.KindFile || node.Kind == graph.KindImport || node.Name == "" {
				continue
			}
			best := types.Object(nil)
			bestDist := 1 << 30
			for _, entry := range defsByName[rel+"\x00"+node.Name] {
				if entry.line < node.StartLine-1 || entry.line > node.EndLine+1 {
					continue
				}
				distance := entry.line - node.StartLine
				if distance < 0 {
					distance = -distance
				}
				if distance < bestDist {
					bestDist = distance
					best = entry.obj
				}
			}
			if best == nil {
				continue
			}

			stamp := goNodeStamp{}
			if typeStr := types.TypeString(best.Type(), nil); typeStr != "" && typeStr != "invalid type" {
				stamp.semanticType = typeStr
			}
			if fn, ok := best.(*types.Func); ok {
				if sig, ok := fn.Type().(*types.Signature); ok && sig.Results().Len() > 0 {
					stamp.returnType = types.TypeString(sig.Results(), nil)
				}
			}
			if stamp.semanticType != "" || stamp.returnType != "" {
				stamps[node.ID] = stamp
			}
		}
	}
	return stamps, nil
}

// releaseGoPackageCompilerState retains only package/module metadata used by
// externals attribution. Called after every Use has been detached to strings;
// clearing both directions of packages.Package breaks the AST/types/import
// closure before the long graph-write phase without a forced GC pause.
func releaseGoPackageCompilerState(pkg *packages.Package) {
	if pkg == nil {
		return
	}
	pkg.GoFiles = nil
	pkg.CompiledGoFiles = nil
	pkg.OtherFiles = nil
	pkg.EmbedFiles = nil
	pkg.EmbedPatterns = nil
	pkg.IgnoredFiles = nil
	pkg.Imports = nil
	pkg.Types = nil
	pkg.Fset = nil
	pkg.Syntax = nil
	pkg.TypesInfo = nil
	pkg.TypesSizes = nil
}

// persistGoNodeStamps uses the SQLite backend's set-oriented promoted-column
// writer when available. Other stores fetch only the full rows that will be
// mutated, in bounded ID batches. Summary/light projection rows are never
// written back, so opaque Meta keys cannot be discarded.
func persistGoNodeStamps(
	ctx context.Context,
	g graph.Store,
	stamps map[string]goNodeStamp,
	providerName string,
) (int, error) {
	return persistGoNodeStampsSliced(ctx, g, stamps, providerName, nil)
}

func persistGoNodeStampsSliced(
	ctx context.Context,
	g graph.Store,
	stamps map[string]goNodeStamp,
	providerName string,
	resolveSlices *resolveLockSlices,
) (int, error) {
	ids := make([]string, 0, len(stamps))
	for id := range stamps {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	withStore := func(fn func() error) error {
		if resolveSlices != nil {
			return resolveSlices.with(ctx, fn)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	}

	if writer, ok := g.(graph.SemanticNodeStampWriter); ok {
		changed := 0
		for start := 0; start < len(ids); start += goNodeStampChunkSize {
			end := min(start+goNodeStampChunkSize, len(ids))
			updates := make([]graph.SemanticNodeStamp, 0, end-start)
			for _, id := range ids[start:end] {
				stamp := stamps[id]
				updates = append(updates, graph.SemanticNodeStamp{
					NodeID:         id,
					SemanticType:   stamp.semanticType,
					ReturnType:     stamp.returnType,
					SemanticSource: providerName,
				})
			}
			if err := withStore(func() error {
				changed += writer.PersistSemanticNodeStamps(updates)
				return nil
			}); err != nil {
				return changed, err
			}
		}
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		return changed, nil
	}

	enriched := 0
	for start := 0; start < len(ids); start += goNodeStampChunkSize {
		end := min(start+goNodeStampChunkSize, len(ids))
		chunk := ids[start:end]
		if err := withStore(func() error {
			fullNodes := g.GetNodesByIDs(chunk)
			updated := make([]*graph.Node, 0, len(fullNodes))
			for _, id := range chunk {
				node := fullNodes[id]
				if node == nil {
					continue
				}
				stamp := stamps[id]
				if stamp.semanticType != "" {
					semantic.EnrichNodeMeta(node, "semantic_type", stamp.semanticType, providerName)
					enriched++
				}
				if stamp.returnType != "" {
					semantic.EnrichNodeMeta(node, "return_type", stamp.returnType, providerName)
				}
				updated = append(updated, node)
			}
			if len(updated) > 0 {
				g.AddBatch(updated, nil)
			}
			return nil
		}); err != nil {
			return enriched, err
		}
	}
	return enriched, nil
}

// resolvedGoUse is deliberately detached from go/types and backend-owned graph
// nodes. A full-repository pass can project compiler Uses to these compact facts,
// release AST/type graphs, and then spend its long SQLite apply on bounded pages.
type resolvedGoUse struct {
	callerID     string
	targetNodeID string
	graphPath    string
	line         int
	kind         graph.EdgeKind
	external     *externalUseIdentity
}

// projectGoUsesAndReleaseCompilerState creates a stack boundary around the last
func clearGoPackageCompilerRoots(pkgs []*packages.Package) {
	for i, pkg := range pkgs {
		releaseGoPackageCompilerState(pkg)
		pkgs[i] = nil
	}
}

// reclaimAndReleaseGoCompiler centralizes the ordering invariant: roots are
// severed first, optional reclamation completes second, and admission is
// returned last. Tests inject callbacks to prove cancellation cannot invert it.
func reclaimAndReleaseGoCompiler(sever func(), forceGC bool, collect, afterCollect, release func()) {
	sever()
	if forceGC {
		collect()
	}
	if afterCollect != nil {
		afterCollect()
	}
	release()
}

// ast/types walk. Returning from this helper, in addition to clearing every
// packages.Package, guarantees range temporaries cannot remain GC roots when
// the caller performs its one large-program reclamation cycle.
type goUseProjectionStats struct {
	packagesScanned int
	packagesSkipped int
	identsScanned   int
	identsSkipped   int
}

func projectGoUsesAndReleaseCompilerState(
	ctx context.Context,
	pkgs []*packages.Package,
	fset *token.FileSet,
	absRoot, repoPrefix string,
	funcIndex map[string]*fileFuncIndex,
	objToNode map[types.Object]string,
	externals *externalsAttribution,
) (*goUsePlan, error) {
	plan, _, err := projectGoUsesAndReleaseCompilerStateWithStats(
		ctx, pkgs, fset, absRoot, repoPrefix, funcIndex, objToNode, externals,
	)
	return plan, err
}

func projectGoUsesAndReleaseCompilerStateWithStats(
	ctx context.Context,
	pkgs []*packages.Package,
	fset *token.FileSet,
	absRoot, repoPrefix string,
	funcIndex map[string]*fileFuncIndex,
	objToNode map[types.Object]string,
	externals *externalsAttribution,
) (*goUsePlan, goUseProjectionStats, error) {
	plan := newGoUsePlan(len(pkgs))
	var stats goUseProjectionStats
	paths := newGraphPaths(absRoot, repoPrefix)
	for pkgIndex, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		if pkg == nil || pkg.TypesInfo == nil {
			releaseGoPackageCompilerState(pkg)
			pkgs[pkgIndex] = nil
			continue
		}
		uses := len(pkg.TypesInfo.Uses)
		if !packageMayContainGraphVisibleGoCaller(pkg, fset, absRoot, repoPrefix, funcIndex) {
			stats.packagesSkipped++
			stats.identsSkipped += uses
			plan.setPackage(pkgIndex, nil)
			releaseGoPackageCompilerState(pkg)
			pkgs[pkgIndex] = nil
			continue
		}
		stats.packagesScanned++
		stats.identsScanned += uses
		packageUses := make([]resolvedGoUse, 0, uses)
		for ident, obj := range pkg.TypesInfo.Uses {
			use, ok := resolveGoUseWithPaths(ident, obj, fset, paths, funcIndex, objToNode, externals)
			if ok {
				packageUses = append(packageUses, use)
			}
		}
		sortResolvedGoUses(packageUses)
		plan.setPackage(pkgIndex, packageUses)
		releaseGoPackageCompilerState(pkg)
		pkgs[pkgIndex] = nil
	}
	return plan, stats, nil
}

// graphVisibleSyntax is the subset of files whose definitions can match a
// node of the projection: a file the projection holds nodes for, and
// conservatively any file carrying a line directive (its identifiers may
// report positions in another file).
func graphVisibleSyntax(files []*ast.File, fset *token.FileSet, absRoot, repoPrefix string, nodesByFile map[string][]*graph.Node) []*ast.File {
	out := make([]*ast.File, 0, len(files))
	for _, file := range files {
		if file == nil {
			continue
		}
		if fileHasLineDirective(file) {
			out = append(out, file)
			continue
		}
		relPath := relativePath(fset.Position(file.Pos()).Filename, absRoot)
		if relPath != "" && len(nodesByFile[scopedGraphPath(repoPrefix, relPath)]) > 0 {
			out = append(out, file)
		}
	}
	return out
}

// fileHasLineDirective reports whether file carries a //line or /*line
// comment.
func fileHasLineDirective(file *ast.File) bool {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//line") || strings.HasPrefix(comment.Text, "/*line") {
				return true
			}
		}
	}
	return false
}

// sortResolvedGoUses puts a package's uses in source order. TypesInfo.Uses
// is a map, and the apply keeps the first use of a caller/target/kind as the
// edge (later uses of the same pair confirm it): without an order, which
// line a new edge carries, and which resolver stub an external use claims,
// changed from one pass to the next over identical source. The earliest use
// wins now, in every pass and in a whole index alike.
func sortResolvedGoUses(uses []resolvedGoUse) {
	sort.Slice(uses, func(i, j int) bool {
		a, b := uses[i], uses[j]
		if a.graphPath != b.graphPath {
			return a.graphPath < b.graphPath
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.callerID != b.callerID {
			return a.callerID < b.callerID
		}
		if a.targetNodeID != b.targetNodeID {
			return a.targetNodeID < b.targetNodeID
		}
		return a.kind < b.kind
	})
}

// packageMayContainGraphVisibleGoCaller retains packages with a graph-visible
// caller and conservatively retains //line or /*line-directed syntax.
// resolveGoUse maps each identifier's final position, which can differ from
// file.Pos after a directive, so those packages must keep the exact
// per-identifier walk.
func packageMayContainGraphVisibleGoCaller(
	pkg *packages.Package,
	fset *token.FileSet,
	absRoot, repoPrefix string,
	funcIndex map[string]*fileFuncIndex,
) bool {
	if len(pkg.Syntax) == 0 {
		return len(pkg.TypesInfo.Uses) > 0
	}
	for _, file := range pkg.Syntax {
		if file == nil {
			continue
		}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if strings.HasPrefix(comment.Text, "//line") || strings.HasPrefix(comment.Text, "/*line") {
					return true
				}
			}
		}
		pos := fset.Position(file.Pos())
		relPath := relativePath(pos.Filename, absRoot)
		if relPath == "" {
			continue
		}
		if funcIndex[scopedGraphPath(repoPrefix, relPath)] != nil {
			return true
		}
	}
	return false
}

// resolveGoUse is the query-free normalization shared by both package walks
// in EnrichRepo. External symbol creation is idempotent and cached by
// externalsAttribution, so the second walk does not repeat store work.
func resolveGoUse(
	ident *ast.Ident,
	obj types.Object,
	fset *token.FileSet,
	absRoot, repoPrefix string,
	funcIndex map[string]*fileFuncIndex,
	objToNode map[types.Object]string,
	externals *externalsAttribution,
) (resolvedGoUse, bool) {
	return resolveGoUseWithPaths(ident, obj, fset, newGraphPaths(absRoot, repoPrefix), funcIndex, objToNode, externals)
}

// resolveGoUseWithPaths is resolveGoUse with the pass's file-name to graph
// path memo.
func resolveGoUseWithPaths(
	ident *ast.Ident,
	obj types.Object,
	fset *token.FileSet,
	paths *graphPaths,
	funcIndex map[string]*fileFuncIndex,
	objToNode map[types.Object]string,
	externals *externalsAttribution,
) (resolvedGoUse, bool) {
	if ident == nil || obj == nil || ident.Pos() == token.NoPos {
		return resolvedGoUse{}, false
	}
	pos := fset.Position(ident.Pos())
	graphPath := paths.of(pos.Filename)
	if graphPath == "" {
		return resolvedGoUse{}, false
	}
	caller := funcIndex[graphPath].containing(pos.Line)
	if caller == nil {
		return resolvedGoUse{}, false
	}

	targetNodeID, ok := objToNode[obj]
	external := false
	if !ok {
		targetNodeID = externals.resolveSymbol(obj)
		if targetNodeID == "" {
			return resolvedGoUse{}, false
		}
		external = true
	}
	if caller.ID == targetNodeID {
		return resolvedGoUse{}, false
	}
	use := resolvedGoUse{
		callerID:     caller.ID,
		targetNodeID: targetNodeID,
		graphPath:    graphPath,
		line:         pos.Line,
		kind:         inferEdgeKindFromObj(obj),
	}
	if external {
		use.external = externals.projectedUse(obj, targetNodeID)
		if use.external == nil {
			return resolvedGoUse{}, false
		}
	}
	return use, true
}

// enrichImplements confirms existing EdgeImplements edges using go/types.
// implementsInterfaceNode reports whether nodeID can legitimately stand as
// the interface side of an implements edge. Phase 1 now rejects incompatible
// identities before mapping; this kind check remains defense in depth for
// persisted or legacy projections. The historical line-first matcher once
// mapped an interface to a same-line parameter and fanned it into 130,250
// implements edges targeting one `#param:ctx` node. Only an interface-kind
// node may host an interface.
func implementsInterfaceNode(nodesByID map[string]*graph.Node, nodeID string) bool {
	node := nodesByID[nodeID]
	return node != nil && node.Kind == graph.KindInterface
}

// implementsConcreteNode is the symmetric guard for the concrete side: a
// mis-mapped concrete TypeName must not source implements edges from a
// param/local/function node.
func implementsConcreteNode(nodesByID map[string]*graph.Node, nodeID string) bool {
	node := nodesByID[nodeID]
	return node != nil && node.Kind == graph.KindType
}

func implementationInterfaceIDs(objToNode map[types.Object]string, nodesByID map[string]*graph.Node) []string {
	interfaceSet := make(map[string]struct{})
	for obj, nodeID := range objToNode {
		if tn, ok := obj.(*types.TypeName); ok {
			if _, ok := tn.Type().Underlying().(*types.Interface); ok && implementsInterfaceNode(nodesByID, nodeID) {
				interfaceSet[nodeID] = struct{}{}
			}
		}
	}
	interfaceIDs := make([]string, 0, len(interfaceSet))
	for id := range interfaceSet {
		interfaceIDs = append(interfaceIDs, id)
	}
	sort.Strings(interfaceIDs)
	return interfaceIDs
}

func (p *Provider) enrichImplementsByIDs(g graph.Store, interfaceIDs []string) int {
	if len(interfaceIDs) == 0 {
		return 0
	}
	// Fetch inbound edges for every loaded interface in one predicate-shaped
	// batch. This preserves cross-repo concrete sources without scanning the
	// graph-wide EdgeImplements set once per repository.
	inbound := g.GetInEdgesByNodeIDs(interfaceIDs)

	var pending []*graph.Edge
	fromIDs := make(map[string]struct{})
	for _, interfaceID := range interfaceIDs {
		for _, edge := range inbound[interfaceID] {
			if edge == nil || edge.Kind != graph.EdgeImplements || edge.Confidence >= 1.0 {
				continue
			}
			pending = append(pending, edge)
			fromIDs[edge.From] = struct{}{}
		}
	}
	ids := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fromNodes := g.GetNodesByIDs(ids)
	confirmedEdges := make([]*graph.Edge, 0, len(pending))
	for _, edge := range pending {
		if fromNode := fromNodes[edge.From]; fromNode != nil && fromNode.Language == "go" {
			semantic.ConfirmEdge(edge, p.Name())
			confirmedEdges = append(confirmedEdges, edge)
		}
	}
	persistConfirmedEdges(g, confirmedEdges)

	return len(confirmedEdges)
}

func (p *Provider) enrichImplements(g graph.Store, objToNode map[types.Object]string, nodesByID map[string]*graph.Node) int {
	return p.enrichImplementsByIDs(g, implementationInterfaceIDs(objToNode, nodesByID))
}

func implementationMethodNames(typ, pointer types.Type) map[string]struct{} {
	methodNames := make(map[string]struct{})
	for _, candidateType := range []types.Type{typ, pointer} {
		methodSet := types.NewMethodSet(candidateType)
		for methodIndex := 0; methodIndex < methodSet.Len(); methodIndex++ {
			methodNames[methodSet.At(methodIndex).Obj().Name()] = struct{}{}
		}
	}
	return methodNames
}

// addMissingImplements discovers interface implementations that tree-sitter missed.
type implementationPair struct {
	from string
	to   string
}

func missingImplementationPairs(objToNode map[types.Object]string, nodesByID map[string]*graph.Node) []implementationPair {
	// Collect interfaces and concrete types. Concrete method names form a cheap,
	// lossless prefilter: exact go/types checks still decide every edge.
	type ifaceEntry struct {
		nodeID string
		iface  *types.Interface
	}
	type concreteEntry struct {
		nodeID      string
		typ         types.Type
		pointer     types.Type
		methodNames map[string]struct{}
	}

	var ifaces []ifaceEntry
	var concretes []concreteEntry

	for obj, nodeID := range objToNode {
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
			// Kind gate — see implementsInterfaceNode: an interface object
			// mis-mapped onto a non-interface node (a signature-line param)
			// must never enter this list, or an empty interface fans out an
			// edge from EVERY concrete type to that junk node.
			if !implementsInterfaceNode(nodesByID, nodeID) {
				continue
			}
			ifaces = append(ifaces, ifaceEntry{nodeID: nodeID, iface: iface.Complete()})
		} else {
			if !implementsConcreteNode(nodesByID, nodeID) {
				continue
			}
			typ := tn.Type()
			pointer := types.NewPointer(typ)
			concretes = append(concretes, concreteEntry{
				nodeID:      nodeID,
				typ:         typ,
				pointer:     pointer,
				methodNames: implementationMethodNames(typ, pointer),
			})
		}
	}

	// Index each method name to the concrete types that expose it on T or *T.
	// For every interface, seed exact checks from its rarest required method.
	// Empty interfaces still require checking every concrete type.
	byMethod := make(map[string][]int)
	allConcreteIndexes := make([]int, len(concretes))
	for concreteIndex, concrete := range concretes {
		allConcreteIndexes[concreteIndex] = concreteIndex
		for methodName := range concrete.methodNames {
			byMethod[methodName] = append(byMethod[methodName], concreteIndex)
		}
	}

	var pairs []implementationPair
	for _, ifaceEntry := range ifaces {
		requiredNames := make([]string, 0, ifaceEntry.iface.NumMethods())
		candidateIndexes := allConcreteIndexes
		for methodIndex := 0; methodIndex < ifaceEntry.iface.NumMethods(); methodIndex++ {
			methodName := ifaceEntry.iface.Method(methodIndex).Name()
			requiredNames = append(requiredNames, methodName)
			methodCandidates := byMethod[methodName]
			if len(methodCandidates) == 0 {
				candidateIndexes = nil
				break
			}
			if len(candidateIndexes) == len(allConcreteIndexes) || len(methodCandidates) < len(candidateIndexes) {
				candidateIndexes = methodCandidates
			}
		}

		for _, concreteIndex := range candidateIndexes {
			concrete := concretes[concreteIndex]
			if concrete.nodeID == ifaceEntry.nodeID {
				continue
			}
			containsAllMethodNames := true
			for _, methodName := range requiredNames {
				if _, ok := concrete.methodNames[methodName]; !ok {
					containsAllMethodNames = false
					break
				}
			}
			if !containsAllMethodNames {
				continue
			}
			if types.Implements(concrete.typ, ifaceEntry.iface) || types.Implements(concrete.pointer, ifaceEntry.iface) {
				pairs = append(pairs, implementationPair{from: concrete.nodeID, to: ifaceEntry.nodeID})
			}
		}
	}
	return pairs
}

func (p *Provider) addMissingImplementationPairs(g graph.Store, pairs []implementationPair, nodesByID map[string]*graph.Node) int {
	if len(pairs) == 0 {
		return 0
	}
	endpoints := make([]graph.EdgeEndpoint, 0, len(pairs))
	for _, pair := range pairs {
		endpoints = append(endpoints, graph.EdgeEndpoint{From: pair.from, To: pair.to})
	}
	candidates := graph.LookupEdgeCandidates(g, endpoints, nil)
	newEdges := make([]*graph.Edge, 0, len(pairs))
	for _, pair := range pairs {
		if candidates.EndpointKind(pair.from, pair.to, graph.EdgeImplements) != nil {
			continue
		}
		fromNode := nodesByID[pair.from]
		if fromNode == nil {
			continue
		}
		edge := semantic.NewSemanticEdge(pair.from, pair.to, graph.EdgeImplements,
			fromNode.FilePath, fromNode.StartLine, p.Name())
		newEdges = append(newEdges, edge)
		candidates.Add(edge)
	}
	if len(newEdges) > 0 {
		g.AddBatch(nil, newEdges)
	}
	return len(newEdges)
}

func (p *Provider) addMissingImplements(g graph.Store, objToNode map[types.Object]string, nodesByID map[string]*graph.Node) int {
	return p.addMissingImplementationPairs(g, missingImplementationPairs(objToNode, nodesByID), nodesByID)
}

func persistConfirmedEdges(g graph.Store, edges []*graph.Edge) {
	if len(edges) == 0 {
		return
	}
	if batch, ok := g.(graph.EdgeMetaBatchPersister); ok {
		batch.PersistEdgeAttributesBatch(edges)
	}
}

// findContainingFuncInNodes is the query-free core used by the full-repo
// provider after it has materialized a repo-scoped node snapshot.
func findContainingFuncInNodes(nodes []*graph.Node, line int) *graph.Node {
	var best *graph.Node
	bestSize := int(^uint(0) >> 1)
	for _, n := range nodes {
		if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			continue
		}
		if n.StartLine <= line && line <= n.EndLine {
			size := n.EndLine - n.StartLine
			if size < bestSize {
				best = n
				bestSize = size
			}
		}
	}
	return best
}

// inferEdgeKindFromObj determines the edge kind from a go/types object.
func inferEdgeKindFromObj(obj types.Object) graph.EdgeKind {
	switch obj.(type) {
	case *types.Func:
		return graph.EdgeCalls
	case *types.TypeName:
		return graph.EdgeReferences
	case *types.Var:
		return graph.EdgeReferences
	case *types.Const:
		return graph.EdgeReferences
	default:
		return ""
	}
}

// relativePath converts an absolute file path to a repo-relative path.
func relativePath(absPath, repoRoot string) string {
	if repoRoot == "" || absPath == "" {
		return ""
	}
	// Compiler export positions use forward slashes on Windows, whereas
	// checkout roots use native separators. Rel also respects path boundaries
	// and platform case rules, unlike a raw string prefix check.
	rel, err := filepath.Rel(filepath.FromSlash(repoRoot), filepath.FromSlash(absPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// graphPaths memoizes the graph path of a compiler file name for one pass.
// Every definition and use of a package names one of a few hundred files;
// deriving the path per identifier (filepath.Rel plus the prefix join)
// allocated for each of them.
type graphPaths struct {
	absRoot, repoPrefix string
	byFile              map[string]string
}

func newGraphPaths(absRoot, repoPrefix string) *graphPaths {
	return &graphPaths{absRoot: absRoot, repoPrefix: repoPrefix, byFile: make(map[string]string)}
}

// of returns scopedGraphPath(repoPrefix, relativePath(filename, absRoot)),
// "" for a file outside the repository.
func (p *graphPaths) of(filename string) string {
	if graphPath, ok := p.byFile[filename]; ok {
		return graphPath
	}
	graphPath := ""
	if relPath := relativePath(filename, p.absRoot); relPath != "" {
		graphPath = scopedGraphPath(p.repoPrefix, relPath)
	}
	p.byFile[filename] = graphPath
	return graphPath
}

// scopedGraphPath converts a repository-relative source path into the path
// stored by a multi-repo graph. Single-repo graphs intentionally retain the
// unprefixed form used by Provider.Enrich.
func scopedGraphPath(repoPrefix, relPath string) string {
	if repoPrefix == "" || relPath == "" {
		return relPath
	}
	return repoPrefix + "/" + relPath
}

// Ensure ast is used.
var _ = (*ast.File)(nil)

// loadErrorSummary splits a load's package errors into soft type errors (an
// import or a variable declared and not used: go/types reports them without
// changing any type, and a stripped sibling's unused imports are expected)
// and hard ones, counted by kind, with a short sample.
type loadErrorSummary struct {
	soft   int
	hard   int
	kinds  map[string]int
	sample []string
}

const loadErrorSampleSize = 5

func classifyLoadErrors(pkgs []*packages.Package) loadErrorSummary {
	out := loadErrorSummary{kinds: map[string]int{}}
	var softSample []string
	for _, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		soft := make(map[string]int, len(pkg.TypeErrors))
		for _, terr := range pkg.TypeErrors {
			if terr.Soft && terr.Fset != nil {
				soft[terr.Fset.Position(terr.Pos).String()+"\x00"+terr.Msg]++
			}
		}
		for _, e := range pkg.Errors {
			if key := e.Pos + "\x00" + e.Msg; e.Kind == packages.TypeError && soft[key] > 0 {
				soft[key]--
				out.soft++
				if len(softSample) < loadErrorSampleSize {
					softSample = append(softSample, e.Pos+": "+e.Msg)
				}
				continue
			}
			out.hard++
			out.kinds[loadErrorKind(e.Kind)]++
			if len(out.sample) < loadErrorSampleSize {
				out.sample = append(out.sample, e.Pos+": "+e.Msg)
			}
		}
	}
	if out.hard == 0 {
		out.sample = softSample
	}
	return out
}

func loadErrorKind(kind packages.ErrorKind) string {
	switch kind {
	case packages.ListError:
		return "list"
	case packages.ParseError:
		return "parse"
	case packages.TypeError:
		return "type"
	default:
		return "unknown"
	}
}

// ConcurrentCheckoutPreparation reports whether this file-bounded working-copy
// pass uses shareable compiler admission with a second slot for other roots.
// It follows the same module and handle-root planning as enrichRepoContext;
// full/exclusive initial loads and one-slot configurations are declined. A
// later scoped-load retry retains that same shareable token.
func (p *Provider) ConcurrentCheckoutPreparation(ctx context.Context, root, repoPrefix string, scope semantic.CheckoutCompilerScope, files []string) bool {
	if ctx.Err() != nil || root == "" || !scope.HandleRoots || scope.Committed || scope.ManifestChanged || p.includeTest {
		return false
	}
	gate, _ := p.admissionGates()
	if cap(gate) < 2 {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	loadDir, modules := goLoadDir(absRoot)
	if modules == 0 {
		return true
	} // The provider reports no-module without loading a compiler.
	handle := make(map[string]struct{}, len(files))
	for _, file := range files {
		handle[file] = struct{}{}
	}
	return !planHandleRoots(absRoot, loadDir, repoPrefix, handle, scope).full && ctx.Err() == nil
}
