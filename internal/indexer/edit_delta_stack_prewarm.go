package indexer

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Pre-warming a stack's per-delta caches.
//
// Every read a per-file delta repeats over an immutable stack is kept per
// stack (the contract registry, the Go file inventory, the provides rows, the
// dependency contracts, the stack's directory index, import adjacency, import
// target placements, package type index and callee parameter tables), but the
// first delta over a new stack paid them all: on a large store the provides
// scan alone was about 16 s, the import projection 4-5 s. A new stack comes
// with a base publication or a base advance (a new commit generation under
// the checkout), and a restart starts over with an empty process.
//
// So the coordinator warms them, once per commit generation it routes, in the
// background, before any edit asks. The warm runs the steps the first edit is
// likeliest to need first: the small whole-repository reads, then the
// packages of the checkout's dirty and recently committed files (their
// imports, import targets, type index and callees' parameters), then the
// provides scan, then the rest of the repository. It never competes with an
// edit: while a per-file delta runs, the warm waits between steps and chunks
// and resumes after it. A delta that arrives during a whole-repository step
// the warm is running waits for that step's answer instead of reading it a
// second time; every other read it needs and the warm has not reached it
// makes itself, and keeps. Names and reference facts are kept per name and per
// target and are not warmed: which ones an edit reads is the edit's.

// editDeltaActive counts the per-file deltas running in the process; the
// pre-warm yields while it is not zero.
var editDeltaActive atomic.Int32

// editDeltaBegin marks a per-file delta running until the returned func.
func editDeltaBegin() func() {
	editDeltaActive.Add(1)
	return func() { editDeltaActive.Add(-1) }
}

// prewarmStandDown reports that a per-file delta is running: a pre-warm scan
// in flight stops at its next row and is run again once the delta is done,
// so no speculative read competes with a delta for the processor or the
// store's read path. (The single-flighted loads a delta itself waits on —
// the contract registry, the Go inventory, the dependency contracts, the
// provides scan — keep running: a delta that needs one would otherwise
// start it over.)
func prewarmStandDown() bool { return editDeltaActive.Load() > 0 || editCycleHoldsLane() }

// editCycleSources are the edit-cycle predicates of the pre-warm runs in
// flight (SparseGenerationBuilder.EditCycleActive: the edit-cycle predicate, the build
// lane held at interactive priority). A pre-warm stands down while any of
// them holds, so it gives way to the whole edit cycle — its enrichment and
// publication included — not only to the delta. Entries are per run, not per
// builder: two runs on one builder (a re-warm starting while the first warm
// is still going) each keep their own until they end.
var editCycleSources sync.Map // *editCycleRun -> func() bool

type editCycleRun struct{ b *SparseGenerationBuilder }

// registerEditCycleSource registers b's edit-cycle predicate for one run and
// returns the run's unregistration.
func registerEditCycleSource(b *SparseGenerationBuilder) func() {
	if b == nil || b.EditCycleActive == nil {
		return func() {}
	}
	run := &editCycleRun{b: b}
	editCycleSources.Store(run, b.EditCycleActive)
	return func() { editCycleSources.Delete(run) }
}

func editCycleHoldsLane() bool {
	held := false
	editCycleSources.Range(func(_, fn any) bool {
		held = fn.(func() bool)()
		return !held
	})
	return held
}

// prewarmYield waits while a per-file delta runs, and reports how long.
func prewarmYield(ctx context.Context) time.Duration {
	started := time.Now()
	for prewarmStandDown() && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(started)
}

// editDeltaPrewarmed records the (checkout, commit generation) pairs already
// warmed or being warmed.
var editDeltaPrewarmed sync.Map

// editDeltaPrewarmDone, when set, is told each stack key a pre-warm finished
// (tests); editDeltaPrewarmStep is told each step as it starts.
var (
	editDeltaPrewarmDone func(key string)
	editDeltaPrewarmStep func(step string)
	// editDeltaPrewarmOpenHook, when set, opens the stack a coordinator's
	// pre-warm reads instead of the commit generation's view (tests: a
	// fixture whose view composes the base corpus has no keyed stack).
	editDeltaPrewarmOpenHook func(ctx context.Context, commitGeneration int64) (LayerBase, func(), error)
	// editDeltaPrewarmKeyOpen is told each time a pre-warm decision opens
	// the stack to compute its key (tests).
	editDeltaPrewarmKeyOpen func()
	// editDeltaPrewarmScanRow is told each row a stand-down scan reads,
	// before it looks whether to stand down (tests).
	editDeltaPrewarmScanRow func(scan string)
)

// prewarmScanRow runs the scan-row hook, when set, and reports whether the
// scan stands down.
func prewarmScanRow(scan string) bool {
	if hook := editDeltaPrewarmScanRow; hook != nil {
		hook(scan)
	}
	return prewarmStandDown()
}

// editDeltaInflight single-flights the whole-repository reads the pre-warm
// and a delta may both start: the second waits for the first's answer.
var editDeltaInflight sync.Map // key -> chan struct{}

// singleFlight runs load unless another caller is running it for key, in
// which case it waits for that one; ready reports whether the answer is
// already kept (then nothing runs).
func singleFlight(key string, ready func() bool, load func()) {
	for {
		if ready() {
			return
		}
		done := make(chan struct{})
		if other, running := editDeltaInflight.LoadOrStore(key, done); running {
			<-other.(chan struct{})
			continue
		}
		func() {
			defer func() {
				editDeltaInflight.Delete(key)
				close(done)
			}()
			if !ready() {
				load()
			}
		}()
		return
	}
}

// prewarmEditDeltaStackOnce starts, once per commit generation, the
// background pre-warm of the stack a working-tree delta over it will read.
// The warm holds the generation's view (a lease on its ancestry) only for one
// unit of work at a time, never across its steps or while it waits.
func (c *CheckoutCoordinator) prewarmEditDeltaStackOnce(commitGeneration int64) {
	if c == nil || c.builder == nil || c.builder.Store == nil || commitGeneration <= 0 {
		return
	}
	c.prewarmCommitGeneration.Store(commitGeneration)
	// It is called after every cycle. The synchronous check answers an
	// unchanged stack from memory — the commit generation and its stack's
	// correction epochs as last seen — so a repeated call starts no goroutine
	// and opens no view; only a new commit generation, a moved epoch, or a
	// deferral that has since ended goes on.
	if !c.prewarmState.changed(commitGeneration, c.builder) || !c.prewarmState.begin() {
		return
	}
	go func() {
		defer func() {
			// A call that arrived while this one ran (a re-warm after a
			// correction) looks again now.
			if c.prewarmState.end() {
				c.prewarmEditDeltaStackOnce(c.prewarmCommitGeneration.Load())
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		defer registerEditCycleSource(c.builder)()
		open := func(ctx context.Context) (LayerBase, func(), error) {
			if hook := editDeltaPrewarmOpenHook; hook != nil {
				return hook(ctx, commitGeneration)
			}
			return c.generationLayerReader(ctx, commitGeneration)
		}
		// The key is computed over an opened view of the stack: that open
		// stands down for an edit cycle like every unit of the warm.
		prewarmYield(ctx)
		if ctx.Err() != nil {
			return
		}
		if hook := editDeltaPrewarmKeyOpen; hook != nil {
			hook()
		}
		stackKey, stack, ok := c.prewarmStackKey(ctx, open)
		if !ok {
			return
		}
		// A stack the pending startup correction will change is left to the
		// re-warm that follows it (RewarmEditDeltaStacks): warmed now, its
		// caches would be orphaned when the correction moves its key.
		deferred := c.builder.PrewarmDeferred != nil && c.builder.PrewarmDeferred(stack)
		c.prewarmState.record(commitGeneration, stack, c.builder, deferred)
		if deferred {
			return
		}
		// Once per stack as its caches are keyed: the key carries every
		// generation's correction epoch, so a stack a correction changed is
		// a new stack and is warmed again, and one it did not change keeps
		// its warm caches.
		marker := c.checkoutID + "\x00" + strconv.FormatInt(commitGeneration, 10) + "\x00" + stackKey
		if _, already := editDeltaPrewarmed.LoadOrStore(marker, struct{}{}); already {
			return
		}
		head := prewarmHead{root: c.root, sha: gitHeadCommit(ctx, c.root)}
		if !c.builder.prewarmEditDeltaStack(ctx, open, c.repoPrefix, c.workspaceID, c.projectID, likelyEditedFiles(ctx, c.root), head) {
			editDeltaPrewarmed.Delete(marker)
			c.prewarmState.forget()
		}
	}()
}

// prewarmState is what a coordinator's last pre-warm decision saw: the
// commit generation, its stack, the stack's correction epochs, and whether
// the warm was deferred to the re-warm. One pre-warm runs at a time.
type prewarmState struct {
	mu       sync.Mutex
	commit   int64
	stack    []int64
	epochs   []uint64
	deferred bool
	inflight bool
	again    bool // a call arrived while one was in flight
}

// changed reports whether commit's stack must be looked at again: another
// commit generation, a stack generation whose correction epoch moved, or a
// deferral whose reason has ended. It reads memory only.
func (s *prewarmState) changed(commit int64, b *SparseGenerationBuilder) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commit != commit || len(s.stack) == 0 {
		return true
	}
	for i, gen := range s.stack {
		if b.Store.GenerationCorrectionEpoch(gen) != s.epochs[i] {
			return true
		}
	}
	return s.deferred && (b.PrewarmDeferred == nil || !b.PrewarmDeferred(s.stack))
}

func (s *prewarmState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight {
		s.again = true
		return false
	}
	s.inflight = true
	return true
}

// end reports whether a call arrived while this one was in flight.
func (s *prewarmState) end() (again bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight, again, s.again = false, s.again, false
	return again
}

func (s *prewarmState) record(commit int64, stack []int64, b *SparseGenerationBuilder, deferred bool) {
	epochs := make([]uint64, len(stack))
	for i, gen := range stack {
		epochs[i] = b.Store.GenerationCorrectionEpoch(gen)
	}
	s.mu.Lock()
	s.commit, s.stack, s.epochs, s.deferred = commit, append([]int64(nil), stack...), epochs, deferred
	s.mu.Unlock()
}

// forget drops the recorded decision, so the next call looks again (a warm
// that did not complete).
func (s *prewarmState) forget() {
	s.mu.Lock()
	s.commit, s.stack, s.epochs, s.deferred = 0, nil, nil, false
	s.mu.Unlock()
}

// likelyEditedFiles is the checkout's dirty files and the files its recent
// commits touched, repository-relative, most likely first.
func likelyEditedFiles(ctx context.Context, root string) []string {
	if root == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(rel string) {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			return
		}
		if _, dup := seen[rel]; !dup {
			seen[rel] = struct{}{}
			out = append(out, rel)
		}
	}
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		data, _ := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
		return string(data)
	}
	for _, line := range strings.Split(run("status", "--porcelain", "--untracked-files=no"), "\n") {
		if len(line) > 3 {
			path := line[3:]
			if i := strings.Index(path, " -> "); i >= 0 {
				path = path[i+4:]
			}
			add(path)
		}
	}
	for _, line := range strings.Split(run("log", "-n", "30", "--name-only", "--format="), "\n") {
		add(line)
	}
	return out
}

// prewarmView is one unit of pre-warm work's view of the stack: opened for
// the unit and released after it.
type prewarmView struct {
	base LayerBase
	dw   *graph.DeltaWriter
	idx  *Indexer
}

// PrewarmEditDeltaStack loads the per-stack caches a per-file delta over the
// stack open answers reads, for one repository, most likely first (likely are
// repository-relative paths the next edits are likeliest to touch). Each unit
// of work opens the stack's view and releases it before the next, so the warm
// holds at most one view at a time and none while it waits for an edit. It
// reads only; nothing is written. It reports whether it ran (false: the view
// could not be opened or has no immutable stack key).
func (b *SparseGenerationBuilder) PrewarmEditDeltaStack(ctx context.Context, open func(context.Context) (LayerBase, func(), error), repoPrefix, workspaceID, projectID string, likely []string) bool {
	return b.prewarmEditDeltaStack(ctx, open, repoPrefix, workspaceID, projectID, likely, prewarmHead{})
}

// prewarmHead is the checkout the stack is the committed state of: its root
// and HEAD commit (empty: unknown, and the HEAD-content reads are skipped).
type prewarmHead struct {
	root, sha string
}

// prewarmEditDeltaStack is PrewarmEditDeltaStack with the checkout's HEAD,
// whose content the likely files' prior fingerprints are read from.
func (b *SparseGenerationBuilder) prewarmEditDeltaStack(ctx context.Context, open func(context.Context) (LayerBase, func(), error), repoPrefix, workspaceID, projectID string, likely []string, head prewarmHead) bool {
	if b == nil || open == nil || repoPrefix == "" {
		return false
	}
	defer registerEditCycleSource(b)()
	var key string
	var cache *graph.BaseProjectionCache
	var yielded time.Duration
	// layerRows counts the rows the pre-warm's reads took from the stack's
	// generation layers, logged per step (rows per warm-up).
	var layerRows int
	// with runs fn over a freshly opened view of the stack and releases it.
	lowPriority := false
	with := func(fn func(v prewarmView)) bool {
		yielded += prewarmYield(ctx)
		if lowPriority && editDeltaPrewarmLowPriorityPause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(editDeltaPrewarmLowPriorityPause):
			}
		}
		if ctx.Err() != nil {
			return false
		}
		base, release, err := open(ctx)
		if err != nil {
			return false
		}
		defer release()
		k, ok := editDeltaBaseCacheKey(base, b.Store)
		if !ok || (key != "" && k != key) {
			return false
		}
		if key == "" {
			key, cache = k, editDeltaBaseCache(k)
		}
		dw := graph.NewDeltaWriter(base, nil)
		dw.SetBaseProjectionCache(cache)
		idx := New(dw, b.Registry, b.Config, b.Logger)
		defer idx.Close()
		idx.SetRepoPrefix(repoPrefix)
		idx.SetWorkspaceID(workspaceID)
		idx.SetProjectID(projectID)
		fn(prewarmView{base: base, dw: dw, idx: idx})
		layerRows += dw.DeltaStats().LayerRowsRead
		return true
	}
	started := time.Now()
	laps := make([]zap.Field, 0, 16)
	last := time.Now()
	lastRows := 0
	step := func(name string) {
		laps = append(laps, zap.Int64(name+"_ms", time.Since(last).Milliseconds()),
			zap.Int(name+"_layer_rows", layerRows-lastRows))
		lastRows = layerRows
		if hook := editDeltaPrewarmStep; hook != nil {
			hook(name)
		}
		last = time.Now()
	}
	if !with(func(prewarmView) {}) {
		return false
	}
	step("start")

	// 1. The small whole-repository reads every delta makes.
	var goFiles, files []string
	with(func(v prewarmView) {
		if contractKey, ok := editDeltaContractCacheKey(v.base, b.Store, repoPrefix, workspaceID, projectID); ok {
			seedEditDeltaContractRegistry(v.idx, contractKey)
		}
	})
	step("contract_registry")
	with(func(v prewarmView) {
		invKey, ok := editDeltaContractCacheKey(v.base, b.Store, repoPrefix, "", "")
		if !ok {
			return
		}
		singleFlight("inventory\x00"+invKey, func() bool { _, ok := cachedEditDeltaGoInventory(invKey); return ok }, func() {
			storeEditDeltaGoInventory(invKey, goInventoryOf(graph.NodesInScopeSeq(v.dw, []string{repoPrefix}, nil, graph.KindFile)))
		})
		goFiles, _ = cachedEditDeltaGoInventory(invKey)
	})
	step("go_inventory")
	for {
		stoodDown := false
		files = files[:0]
		with(func(v prewarmView) {
			for row := range v.dw.FileNodeIdentitiesSeq([]string{repoPrefix}) {
				if prewarmScanRow("directory_index") {
					stoodDown = true
					return
				}
				files = append(files, row.FilePath)
			}
		})
		if !stoodDown || ctx.Err() != nil {
			break
		}
	}
	sort.Strings(files)
	step("directory_index")
	depKey := key + "\x00" + repoPrefix
	with(func(v prewarmView) {
		singleFlight("deps\x00"+depKey, func() bool { _, ok := cachedEditDeltaDeps(depKey); return ok }, func() {
			storeEditDeltaDeps(depKey, depContractRows(v.dw, repoPrefix))
		})
	})
	step("dep_contracts")

	// 2. The likely-edited files' packages: their imports and targets, their
	// type index, their prior adjacency and their callees' parameters.
	fileSet := make(map[string]struct{}, len(files))
	for _, p := range files {
		fileSet[p] = struct{}{}
	}
	byDir := make(map[string][]string)
	for _, p := range files {
		byDir[filePathDirKey(p)] = append(byDir[filePathDirKey(p)], p)
	}
	prefixed := func(rel string) string {
		if repoPrefix == "" {
			return rel
		}
		return repoPrefix + "/" + rel
	}
	var likelyFiles, likelyDirs []string
	dirSeen := make(map[string]struct{})
	for _, rel := range likely {
		p := prefixed(rel)
		if _, ok := fileSet[p]; !ok {
			continue
		}
		likelyFiles = append(likelyFiles, p)
		if dir := filePathDirKey(p); dir != "" {
			if _, dup := dirSeen[dir]; !dup {
				dirSeen[dir] = struct{}{}
				likelyDirs = append(likelyDirs, dir)
			}
		}
	}
	const chunk = 256
	params := newEditDeltaParamIndex(key, nil)
	kinds := []graph.NodeKind{graph.KindType, graph.KindInterface}
	warmPackages := func(dirs []string) {
		var pkgFiles []string
		for _, dir := range dirs {
			pkgFiles = append(pkgFiles, byDir[dir]...)
		}
		prewarmImports(pkgFiles, chunk, with)
		for _, dir := range dirs {
			var goInDir []string
			for _, p := range byDir[dir] {
				if strings.HasSuffix(p, ".go") {
					goInDir = append(goInDir, p)
				}
			}
			if len(goInDir) > 0 && !with(func(v prewarmView) { v.dw.NodesInFilesByKind(goInDir, kinds) }) {
				return
			}
		}
	}
	warmPackages(likelyDirs)
	for _, file := range likelyFiles {
		if !with(func(v prewarmView) {
			var ids []string
			for _, n := range v.dw.GetFileNodes(file) {
				if n != nil && n.ID != "" {
					ids = append(ids, n.ID)
				}
			}
			if len(ids) > 0 {
				editDeltaPriorEdges(v.dw, key)(ids)
			}
			// The payload's rows below at the file (edit_delta_below_rows.go):
			// its rows recorded there include dataflow rows keyed from
			// unresolved placeholders, which no node of the file is an
			// endpoint of, so the prior view above cannot stand in for them.
			editDeltaBelowRowsSource{key: key, base: v.base}.BelowFileRows(file)
		}) {
			break
		}
	}
	step("likely_packages")
	warmCallees(likelyFiles, chunk, params, with)
	step("likely_callee_params")

	// 3. The provides scan, then the rest of the repository.
	with(func(v prewarmView) {
		singleFlight("provides\x00"+key, func() bool { _, ok := cachedEditDeltaProvides(key); return ok }, func() {
			storeEditDeltaProvides(key, scanProvidesRows(v.dw))
		})
	})
	step("provides")
	// The likely files' names, within a budget: an edit reads its own file's
	// names, most likely first; the budget keeps a long likely set from
	// holding the warm (and the store's read path) for minutes.
	namesDeadline := time.Now().Add(editDeltaPrewarmNamesBudget)
	for _, file := range likelyFiles {
		if time.Now().After(namesDeadline) {
			break
		}
		fileDeadline := time.Now().Add(editDeltaPrewarmNamesPerFile)
		if fileDeadline.After(namesDeadline) {
			fileDeadline = namesDeadline
		}
		if !with(func(v prewarmView) {
			warmLikelyNames(v.dw, repoPrefix, file, fileDeadline)
			warmLikelyReferrers(v.dw, repoPrefix, file, fileDeadline, params)
			warmLikelyPriorFingerprints(v.idx, v.base, b.Store, repoPrefix, file, v.dw.GetFileNodes(file), head)
		}) {
			break
		}
	}
	step("likely_names")
	var restDirs []string
	for dir := range byDir {
		if _, done := dirSeen[dir]; !done {
			restDirs = append(restDirs, dir)
		}
	}
	sort.Strings(restDirs)
	// The rest is not needed by an edit of a likely file: it runs at low
	// priority, pausing between packages so it never holds the store's read
	// path for long.
	if ctx.Err() == nil {
		lowPriority = true
		warmPackages(restDirs)
		step("repository_packages")
	}
	for ctx.Err() == nil {
		stoodDown := false
		with(func(v prewarmView) {
			for range v.dw.FileNodeIdentitiesSeq(nil) {
				if prewarmScanRow("directory_index_all") {
					stoodDown = true
					return
				}
			}
		})
		if !stoodDown {
			step("directory_index_all")
			break
		}
	}

	fields := append([]zap.Field{zap.String("repo", repoPrefix), zap.Int("files", len(files)), zap.Int("go_files", len(goFiles)),
		zap.Int("likely_files", len(likelyFiles)), zap.Bool("complete", ctx.Err() == nil),
		zap.Int64("ms", time.Since(started).Milliseconds()), zap.Int64("yielded_ms", yielded.Milliseconds())}, laps...)
	if b.Logger != nil {
		b.Logger.Info("edit delta: stack pre-warmed", fields...)
	}
	if hook := editDeltaPrewarmDone; hook != nil {
		hook(key)
	}
	return true
}

// prewarmImports warms files' import adjacency and their targets' placements,
// one chunk per view.
func prewarmImports(files []string, chunk int, with func(func(prewarmView)) bool) {
	targetSet := make(map[string]struct{})
	for start := 0; start < len(files); start += chunk {
		batch := files[start:min(start+chunk, len(files))]
		if !with(func(v prewarmView) {
			projected, _ := v.dw.ProjectImportAdjacency(batch)
			for _, targets := range projected {
				for _, to := range targets {
					if !graph.IsUnresolvedTarget(to) && !strings.HasPrefix(to, "external::") {
						targetSet[to] = struct{}{}
					}
				}
			}
		}) {
			return
		}
	}
	targets := make([]string, 0, len(targetSet))
	for to := range targetSet {
		targets = append(targets, to)
	}
	sort.Strings(targets)
	for start := 0; start < len(targets); start += chunk {
		batch := targets[start:min(start+chunk, len(targets))]
		if !with(func(v prewarmView) { v.dw.NodePlacementsByIDs(batch) }) {
			return
		}
	}
}

// warmCallees warms the parameter tables of the callees files pass
// arguments to, one chunk per view.
func warmCallees(files []string, chunk int, params *editDeltaParamIndex, with func(func(prewarmView)) bool) {
	for start := 0; start < len(files); start += chunk {
		batch := files[start:min(start+chunk, len(files))]
		if !with(func(v prewarmView) {
			var ids []string
			for _, nodes := range v.dw.GetFileNodesByPaths(batch) {
				for _, n := range nodes {
					if n != nil && n.ID != "" {
						ids = append(ids, n.ID)
					}
				}
			}
			callees := make(map[string]struct{})
			for _, edges := range v.dw.GetOutEdgesByNodeIDs(ids) {
				for _, e := range edges {
					if e != nil && e.Kind == graph.EdgeArgOf {
						if callee, _, ok := argOfRewriteTarget(e); ok {
							callees[callee] = struct{}{}
						}
					}
				}
			}
			if len(callees) > 0 {
				params.index(v.dw, callees)
			}
		}) {
			return
		}
	}
}

// filePathDirKey is a graph path's directory.
func filePathDirKey(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// editDeltaPrewarmNamesBudget bounds the likely-names step.
var editDeltaPrewarmNamesBudget = 60 * time.Second

// editDeltaPrewarmNamesPerFile bounds one likely file's name reads, so a file
// with many references cannot spend the step's whole budget.
var editDeltaPrewarmNamesPerFile = 10 * time.Second

// editDeltaPrewarmLowPriorityPause is the pause before each unit of the
// pre-warm's low-priority tail (the packages no likely file is in and the
// whole directory index).
var editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond

// warmLikelyNames makes the name reads a delta editing file would make: its
// catch-up re-resolves every reference of the file (a reparse makes them
// unresolved again), and its affected-by pass re-resolves the pending
// references of the files that reference the file's declarations. Past
// deadline (when set) it stops between reads.
func warmLikelyNames(dw *graph.DeltaWriter, repoPrefix, file string, deadline time.Time) {
	past := func() bool { return prewarmStandDown() || (!deadline.IsZero() && time.Now().After(deadline)) }
	nodes := dw.GetFileNodes(file)
	ids := make([]string, 0, len(nodes))
	declared := make(map[string]struct{})
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		ids = append(ids, n.ID)
		if n.Name != "" && n.Kind != graph.KindFile && n.Kind != graph.KindLocal && n.Kind != graph.KindParam {
			declared[n.Name] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return
	}
	out := dw.GetOutEdgesByNodeIDs(ids)
	if past() {
		return
	}
	var resolvedTargets []string
	for _, edges := range out {
		for _, e := range edges {
			if e != nil && !graph.IsUnresolvedTarget(e.To) && e.To != "" {
				resolvedTargets = append(resolvedTargets, e.To)
			}
		}
	}
	names := dw.NodeNamesByIDs(resolvedTargets)
	if past() {
		return
	}
	var pending []*graph.Edge
	for _, edges := range out {
		for _, e := range edges {
			if e == nil || e.To == "" {
				continue
			}
			to := e.To
			if !graph.IsUnresolvedTarget(to) {
				name := names[to]
				if name == "" {
					continue
				}
				to = graph.UnresolvedMarker + name
			}
			pending = append(pending, &graph.Edge{From: e.From, To: to, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line, Meta: e.Meta})
		}
	}
	// The referrers' pending references (affected-by), and the file's own
	// declared names as they would be restubbed there.
	if facts, err := dw.LoadRefFactsByTargets(repoPrefix, ids); err == nil {
		var referrers []string
		for referrer := range facts {
			if referrer != file {
				referrers = append(referrers, referrer)
			}
		}
		sort.Strings(referrers)
		// The referrers' restubbed references name the file's declarations;
		// their other pending references are theirs, not the edit's.
		var referrerID string
		if len(referrers) > 0 {
			for _, n := range dw.GetFileNodes(referrers[0]) {
				if n != nil && n.ID != "" {
					referrerID = n.ID
					break
				}
			}
		}
		if referrerID != "" {
			for name := range declared {
				pending = append(pending, &graph.Edge{From: referrerID, To: graph.UnresolvedMarker + name, Kind: graph.EdgeCalls})
			}
		}
	}
	_, _ = resolver.WarmPendingNamesUntil(dw, pending, deadline)
}

// editDeltaPrewarmReferrerChunk is how many referrer files one read of the
// likely file's referrers covers; the deadline is checked between chunks.
const editDeltaPrewarmReferrerChunk = 16

// warmLikelyReferrers makes the rest of the reads a delta editing file makes
// past its own names, most edit-bound first, stopping between reads past
// deadline (when set):
//   - the incoming identities of the file's declarations' stub keys (the
//     catch-up's incoming admission reads them for every declared name);
//   - the pending references of the files referencing the file (the
//     affected-by pass re-resolves them, reading their names);
//   - the parameter tables of those files' callees (the dataflow pass
//     re-derives the referrers' argument edges), when params is set.
func warmLikelyReferrers(dw *graph.DeltaWriter, repoPrefix, file string, deadline time.Time, params *editDeltaParamIndex) {
	past := func() bool { return prewarmStandDown() || (!deadline.IsZero() && time.Now().After(deadline)) }
	if past() {
		return
	}
	var ids, stubKeys []string
	seenKey := make(map[string]struct{})
	for _, n := range dw.GetFileNodes(file) {
		if n == nil || n.ID == "" {
			continue
		}
		ids = append(ids, n.ID)
		if n.Name == "" || !graph.IsReferenceableSymbol(n.Kind) {
			continue
		}
		for _, key := range graph.UnresolvedNameCandidateIDs(n) {
			if _, dup := seenKey[key]; !dup && key != "" {
				seenKey[key] = struct{}{}
				stubKeys = append(stubKeys, key)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	if len(stubKeys) > 0 {
		graph.InEdgeIdentitiesByNodeIDs(dw, stubKeys)
	}
	if past() {
		return
	}
	facts, err := dw.LoadRefFactsByTargets(repoPrefix, ids)
	if err != nil {
		return
	}
	var referrers []string
	for referrer := range facts {
		if referrer != file && referrer != "" {
			referrers = append(referrers, referrer)
		}
	}
	sort.Strings(referrers)
	for start := 0; start < len(referrers); start += editDeltaPrewarmReferrerChunk {
		if past() {
			return
		}
		chunk := referrers[start:min(start+editDeltaPrewarmReferrerChunk, len(referrers))]
		var nodeIDs []string
		for _, nodes := range dw.GetFileNodesByPaths(chunk) {
			for _, n := range nodes {
				if n != nil && n.ID != "" {
					nodeIDs = append(nodeIDs, n.ID)
				}
			}
		}
		var pending []*graph.Edge
		callees := make(map[string]struct{})
		for _, edges := range dw.GetOutEdgesByNodeIDs(nodeIDs) {
			for _, e := range edges {
				if e == nil {
					continue
				}
				if graph.IsUnresolvedTarget(e.To) {
					pending = append(pending, e)
				}
				if e.Kind == graph.EdgeArgOf {
					if callee, _, ok := argOfRewriteTarget(e); ok {
						callees[callee] = struct{}{}
					}
				}
			}
		}
		if complete, _ := resolver.WarmPendingNamesUntil(dw, pending, deadline); !complete {
			return
		}
		if params != nil && len(callees) > 0 {
			params.index(dw, callees)
		}
	}
}

// warmLikelyPriorFingerprints makes the prior-fingerprint read a delta editing
// file makes when the file's prior rows carry no (or no hierarchy) derived
// fingerprints: its HEAD content's, parsed and kept per stack
// (edit_delta_prior_fingerprints.go). Nothing is read when the rows carry
// them, or the checkout's HEAD is unknown.
func warmLikelyPriorFingerprints(idx *Indexer, base graph.Reader, store any, repoPrefix, file string, priorNodes []*graph.Node, head prewarmHead) {
	if idx == nil || head.root == "" || head.sha == "" || len(priorNodes) == 0 {
		return
	}
	stored := storedDerivedFingerprints(priorNodes)
	needed := (!stored.complete() && derivedFingerprintSideExists(priorNodes)) ||
		(stored.complete() && stored.hierarchy == "")
	if !needed {
		return
	}
	key, ok := editDeltaContractCacheKey(base, store, repoPrefix, "", "")
	if !ok {
		return
	}
	absRoot, err := filepath.Abs(head.root)
	if err != nil {
		return
	}
	rel := strings.TrimPrefix(file, repoPrefix+"/")
	absPath := filepath.Join(absRoot, filepath.FromSlash(rel))
	// The HEAD parse keys its nodes by the indexer's root, as the delta's
	// indexer does: a parse keyed any other way would not match the rows and
	// would be kept as a refusal for every later delta over the stack.
	if idx.rootPath == "" {
		idx.SetRootPath(absRoot)
	}
	if idx.relKey(absPath) != rel {
		return
	}
	if source := headPriorFingerprints(idx, absRoot, head.sha, key); source != nil {
		source(absPath, priorNodes)
	}
}

// gitHeadCommit is the checkout's HEAD commit, empty when unknown.
func gitHeadCommit(ctx context.Context, root string) string {
	if root == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// prewarmStackKey is the per-stack cache key of the stack open yields, and
// the stack's generations. One open: the commit generation's catalog
// ancestry and its layers' masks (the materializer's layer cache serves a
// stack it has opened before); the key itself reads only memory (the
// generation ids and their correction epochs).
func (c *CheckoutCoordinator) prewarmStackKey(ctx context.Context, open func(context.Context) (LayerBase, func(), error)) (string, []int64, bool) {
	base, release, err := open(ctx)
	if err != nil {
		return "", nil, false
	}
	defer release()
	layer, ok := base.(commitLayerBase)
	if !ok {
		return "", nil, false
	}
	key, ok := editDeltaBaseCacheKey(base, c.builder.Store)
	return key, append([]int64(nil), layer.stack...), ok
}

// RewarmEditDeltaStacks warms again, in the background, each checkout's
// last pre-warmed stack whose cache key moved since (a derived-row
// correction advanced one of its generations' epochs). A stack whose key did
// not move is left alone. The warm stands down for edits as the first one
// does.
func (l *CheckoutLifecycle) RewarmEditDeltaStacks() {
	if l == nil {
		return
	}
	l.prewarmDeferral.end()
	l.coordMu.Lock()
	coordinators := make([]*CheckoutCoordinator, 0, len(l.coordinators))
	for _, c := range l.coordinators {
		coordinators = append(coordinators, c)
	}
	l.coordMu.Unlock()
	for _, c := range coordinators {
		if g := c.prewarmCommitGeneration.Load(); g > 0 {
			c.prewarmEditDeltaStackOnce(g)
		}
	}
}

// prewarmDeferral holds back the first pre-warm of a stack the pending
// startup correction will change (a generation with a stale derivation
// stamp). The daemon starts it before the correction (DeferPrewarmsUntil-
// Corrected) and RewarmEditDeltaStacks ends it; nothing is deferred while it
// is not active, so a caller that never runs the correction never waits.
type prewarmDeferral struct {
	mu     sync.Mutex
	active bool
	stale  map[int64]struct{}
}

func (d *prewarmDeferral) defers(stack []int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return false
	}
	for _, gen := range stack {
		if _, ok := d.stale[gen]; ok {
			return true
		}
	}
	return false
}

func (d *prewarmDeferral) end() {
	d.mu.Lock()
	d.active, d.stale = false, nil
	d.mu.Unlock()
}

// DeferPrewarmsUntilCorrected holds back, until RewarmEditDeltaStacks, the
// first pre-warm of every stack holding a generation the startup correction
// will re-derive: those caches would be orphaned when the correction moves
// the stack's key.
func (l *CheckoutLifecycle) DeferPrewarmsUntilCorrected(ctx context.Context) {
	if l == nil || l.store == nil {
		return
	}
	stale := make(map[int64]struct{})
	for _, pass := range []struct {
		name    string
		version int
	}{
		{derivationPassFileFingerprints, fileFingerprintDerivationVersion},
		{derivationPassCapability, capabilityDerivationVersion},
	} {
		gens, err := l.store.GenerationsWithStaleDerivation(ctx, pass.name, pass.version)
		if err != nil {
			return
		}
		for _, g := range gens {
			stale[g.GenerationID] = struct{}{}
		}
	}
	if len(stale) == 0 {
		return
	}
	l.prewarmDeferral.mu.Lock()
	l.prewarmDeferral.active, l.prewarmDeferral.stale = true, stale
	l.prewarmDeferral.mu.Unlock()
}
