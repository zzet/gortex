package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/resolver"
)

// Shared lookup caches describe the checkout, not what any one graph has applied.
// reads includes malformed configs and absent extends parents: repairing either
// can change resolution without changing a caller or an already effective scope.
type resolutionInputs struct {
	aliases *tsalias.Collection
	nuxt    *nuxtimports.Collection
	reads   map[string]string
	digest  string
	// stats lets a save skip rereading an input whose size and mtime have not
	// moved since it was read. Each stat is taken before its read, so a
	// rewrite after the stat only costs a reread. Shared snapshots are
	// rechecked concurrently, hence statsMu.
	statsMu sync.Mutex
	stats   map[string]resolutionInputStat
}

// resolutionInputStat is an input's identity when it was read; missing marks
// an input that did not exist.
type resolutionInputStat struct {
	size    int64
	mtime   int64
	missing bool
}

func statResolutionInput(dir *os.Root, rel string) resolutionInputStat {
	info, err := dir.Stat(filepath.FromSlash(rel))
	if err != nil {
		return resolutionInputStat{missing: true}
	}
	return resolutionInputStat{size: info.Size(), mtime: info.ModTime().UnixNano()}
}

// statUnchanged reports whether rel still has the stat recorded with its read.
func (in *resolutionInputs) statUnchanged(dir *os.Root, rel string) bool {
	in.statsMu.Lock()
	defer in.statsMu.Unlock()
	recorded, ok := in.stats[rel]
	return ok && recorded == statResolutionInput(dir, rel)
}

func (in *resolutionInputs) noteStat(rel string, stat resolutionInputStat) {
	in.statsMu.Lock()
	defer in.statsMu.Unlock()
	if in.stats == nil {
		in.stats = make(map[string]resolutionInputStat)
	}
	in.stats[rel] = stat
}

// Both loaders consume one discovery walk. Only Exists escapes the load (for
// multi-target alias resolution); it always probes the working tree itself.
type resolutionTree struct {
	tsalias.Tree
	root  string
	dir   *os.Root
	paths []string
	reads map[string]string
	stats map[string]resolutionInputStat
	proof *buildContentProof
}

func (t *resolutionTree) Files(visit func(string)) {
	for _, rel := range t.paths {
		visit(rel)
	}
}

func (t *resolutionTree) Read(rel string) ([]byte, bool) {
	if t.dir != nil {
		t.stats[rel] = statResolutionInput(t.dir, rel)
	}
	data, sum := readResolutionInput(t.dir, t.root, rel, t.proof)
	t.reads[rel] = sum
	return data, sum != "" && sum != absentContent
}

func readResolutionInput(dir *os.Root, root, rel string, proof *buildContentProof) ([]byte, string) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if dir == nil {
		proof.record(abs, "")
		return nil, ""
	}
	data, err := dir.ReadFile(filepath.FromSlash(rel))
	var sum string
	switch {
	case err == nil:
		sum = gitstate.BlobSHA256(data)
	case errors.Is(err, os.ErrNotExist):
		sum = absentContent
	}
	proof.record(abs, sum)
	return data, sum
}

// A save of explicit files is not a discovery pass. Recheck every known input,
// and check each saved file's ancestors for a newly prepared project or alias
// scope. A directory event or explicit reconcile still discovers all roots.
// An input whose size and mtime are unchanged is not reread unless a content
// proof needs its bytes. A deleted or renamed point needs no walk: it changes
// resolution only if it was itself a known input, which the recheck catches,
// and its directory's candidates are checked like any other point's.
func (in *resolutionInputs) unchangedForPoints(root string, points []string, proof *buildContentProof) bool {
	if in == nil || len(points) == 0 {
		return false
	}
	rels := make([]string, 0, len(points))
	for _, point := range points {
		if point == "" {
			return false
		}
		if !filepath.IsAbs(point) {
			point = filepath.Join(root, point)
		}
		rel, err := filepath.Rel(root, point)
		if err != nil || !filepath.IsLocal(rel) {
			return false
		}
		info, err := os.Stat(point)
		if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
			return false
		}
		rels = append(rels, rel)
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer confined.Close()
	byContent := proof.active()
	for input, prior := range in.reads {
		if !byContent && prior != "" && in.statUnchanged(confined, input) {
			continue
		}
		stat := statResolutionInput(confined, input)
		if _, sum := readResolutionInput(confined, root, input, proof); sum == "" || prior != sum {
			return false
		}
		in.noteStat(input, stat) // touched, not changed
	}
	tsNames, nuxtNames := tsalias.ConfigFileNames(), nuxtimports.ConfigFileNames()
	names := append(append(tsNames[:], nuxtNames[:]...), nuxtimports.GeneratedList)
	checked := map[string]bool{}
	for _, rel := range rels {
		for dir := path.Dir(filepath.ToSlash(rel)); !checked[dir]; dir = path.Dir(dir) {
			checked[dir] = true
			for _, name := range names {
				candidate := path.Join(dir, name)
				if _, known := in.reads[candidate]; known {
					continue
				}
				abs := filepath.Join(root, filepath.FromSlash(candidate))
				_, err := confined.Stat(filepath.FromSlash(candidate))
				if !errors.Is(err, os.ErrNotExist) {
					return false
				}
				proof.record(abs, absentContent)
			}
			if dir == "." {
				break
			}
		}
	}
	return true
}

// The durable digest lives in ts_alias_state. It covers every resolution
// input, Nuxt generated lists included, not only tsconfig aliases.
type aliasStateStore interface {
	TSAliasDigest(repo string) (string, error)
	SetTSAliasDigest(repo, digest string) error
}

// resolutionInputStore persists the inputs behind a certified digest so a
// warm restart can recheck them instead of walking the checkout.
type resolutionInputStore interface {
	TSAliasInputs(repo string) (map[string]string, error)
	SetTSAliasInputs(repo string, inputs map[string]string) error
}

// resolutionCallerInventory answers whether a repository may hold JS/TS callers.
type resolutionCallerInventory interface {
	MayHaveTSAliasCallers(ctx context.Context, repo string) (bool, error)
}

// resolutionDiscoveryWalks counts working-copy discovery walks per root; tests
// use it to prove a pass stayed off the walk.
var resolutionDiscoveryWalks sync.Map // root -> *atomic.Int64

func resolutionDiscoveryWalkCount(root string) int64 {
	if counter, ok := resolutionDiscoveryWalks.Load(root); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

func discoverResolutionInputs(root string, proof *buildContentProof) (*resolutionInputs, error) {
	proof.noteUnboundedReader()
	counter, _ := resolutionDiscoveryWalks.LoadOrStore(root, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	t := newResolutionTree(root, dir, proof)
	t.Tree.Files(func(rel string) { t.paths = append(t.paths, rel) })
	return t.load(), nil
}

func newResolutionTree(root string, dir *os.Root, proof *buildContentProof) *resolutionTree {
	return &resolutionTree{Tree: nuxtimports.WorkingTree(root), root: root, dir: dir,
		reads: make(map[string]string), stats: make(map[string]resolutionInputStat), proof: proof}
}

// load runs both loaders over t.paths and digests every read they made.
func (t *resolutionTree) load() *resolutionInputs {
	current := &resolutionInputs{aliases: tsalias.LoadTree(t), nuxt: nuxtimports.LoadTree(t), reads: t.reads, stats: t.stats}
	h := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(current.reads)) {
		writeFingerprintString(h, rel)
		writeFingerprintString(h, current.reads[rel])
	}
	current.digest = hex.EncodeToString(h.Sum(nil))
	return current
}

// restoreResolutionInputs rebuilds the inputs recorded beside digest without
// walking the checkout. It replays the walk over the recorded paths the walk
// would visit, and returns nil unless every recorded input still has its
// recorded content and the replay reproduces digest exactly.
func restoreResolutionInputs(root, digest string, recorded map[string]string) *resolutionInputs {
	if digest == "" || len(recorded) == 0 {
		return nil
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer dir.Close()
	var visits []string
	for rel, want := range recorded {
		if want == "" {
			return nil // an unreadable input is never proven unchanged
		}
		if _, got := readResolutionInput(dir, root, rel, nil); got != want {
			return nil
		}
		if nuxtimports.WorkingTreeVisits(rel) {
			visits = append(visits, rel)
		}
	}
	// A clean census cannot see an ignored generator output. The walk visits
	// the generated import list beside every Nuxt config once its directory
	// exists, so one that appeared since the record forces the walk.
	nuxtNames := nuxtimports.ConfigFileNames()
	for rel := range recorded {
		if !slices.Contains(nuxtNames[:], path.Base(rel)) {
			continue
		}
		candidate := path.Join(path.Dir(rel), nuxtimports.GeneratedList)
		if _, known := recorded[candidate]; known {
			continue
		}
		if _, err := dir.Stat(filepath.FromSlash(candidate)); !errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	slices.SortFunc(visits, func(a, b string) int {
		switch {
		case tsalias.WalkOrderLess(a, b):
			return -1
		case tsalias.WalkOrderLess(b, a):
			return 1
		}
		return 0
	})
	t := newResolutionTree(root, dir, nil)
	t.paths = visits
	current := t.load()
	if current.digest != digest {
		return nil
	}
	return current
}

type resolutionCompletion struct {
	inputs            *resolutionInputs
	success           bool
	failed            []string // absolute paths; empty on a failure of unknown extent
	dropped           []string // graph paths from partially applied invalidation reindexes requiring re-extraction
	retryResolveFiles []string // graph paths from refused binding writes retried by resolution without re-extraction
	retryScoped       bool     // true when catchupErr is a scoped refusal captured in pendingResolutionRetry
	catchupErr        error    // interrupted whole-pass or refused-write catch-up error
	warm              bool
	frontier          []string // nil means a deferred full build
}

type resolutionRetry struct {
	inputs *resolutionInputs
	files  map[string]bool // relative paths; nil means the pass did not complete discovery
}

func (idx *Indexer) resolutionStore() aliasStateStore {
	s, _ := idx.graph.(aliasStateStore)
	return s
}

// A clean source census does not cover ignored generator outputs. Recheck the
// inputs recorded beside the digest first, and walk only when that fails.
func (idx *Indexer) censusResolutionCurrent(root string) bool {
	if idx.manifestTree().sourced() {
		return true
	}
	if restored := idx.restoreCertifiedResolution(root); restored != nil {
		idx.appliedResolution.Store(restored)
		cacheResolutionInputs(root, restored)
		return true
	}
	certified := ""
	if store := idx.resolutionStore(); store != nil {
		stored, err := store.TSAliasDigest(idx.repoPrefix)
		if err != nil {
			return false
		}
		certified = stored
	} else if prior := idx.appliedResolution.Load(); prior != nil {
		certified = prior.digest
	}
	if certified == "" {
		// Nothing was certified. Unless something consumes resolution
		// inputs, nothing can be stale either: a checkout without JS/TS
		// callers never certifies, and its census must not walk for configs.
		if needed, err := idx.resolutionPassNeeded(root, false, nil); err == nil && !needed {
			return true
		}
		return false
	}
	current, err := discoverResolutionInputs(root, nil)
	if err != nil || current.digest != certified {
		return false
	}
	idx.appliedResolution.Store(current)
	cacheResolutionInputs(root, current)
	return true
}

// restoreCertifiedResolution rebuilds the certified inputs from the record
// beside the digest, or returns nil when the store has none or they moved.
func (idx *Indexer) restoreCertifiedResolution(root string) *resolutionInputs {
	store := idx.resolutionStore()
	inputStore, ok := store.(resolutionInputStore)
	if !ok {
		return nil
	}
	stored, err := store.TSAliasDigest(idx.repoPrefix)
	if err != nil || stored == "" {
		return nil
	}
	recorded, err := inputStore.TSAliasInputs(idx.repoPrefix)
	if err != nil {
		return nil
	}
	return restoreResolutionInputs(root, stored, recorded)
}

func (idx *Indexer) refreshResolutionInputs(root string, points ...string) (*resolutionInputs, error) {
	return idx.refreshResolutionInputsFor(root, false, points...)
}

// resolutionPassNeeded reports whether a working-copy mutation can consume or
// change a resolution input. Without a JS/TS caller, a JS/TS or config path in
// scope, a known input, or unfinished resolution work, discovery is skipped; a
// lookup a later parse makes still loads the inputs lazily.
func (idx *Indexer) resolutionPassNeeded(root string, scopeHasJSTS bool, points []string) (bool, error) {
	if scopeHasJSTS || idx.appliedResolution.Load() != nil {
		return true, nil
	}
	idx.resolutionMu.Lock()
	pending := idx.warmResolution != nil
	idx.resolutionMu.Unlock()
	if pending || idx.resolutionRetry != nil || idx.resolutionInputsChanged {
		return true, nil
	}
	cached := cachedResolutionInputs(root)
	for _, p := range points {
		if p == "" {
			continue // the full index decides through scopeHasJSTS
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if closureJSTSPath(p) || resolutionInputName(filepath.Base(p)) {
			return true, nil
		}
		if rel, err := filepath.Rel(root, p); err == nil && cached != nil {
			if _, known := cached.reads[filepath.ToSlash(rel)]; known {
				return true, nil
			}
		}
	}
	if inv, ok := idx.graph.(resolutionCallerInventory); ok {
		return inv.MayHaveTSAliasCallers(context.Background(), idx.repoPrefix)
	}
	return idx.hasResolutionCallers(), nil
}

// resolutionInputName reports whether a file name can be a resolution input.
func resolutionInputName(name string) bool {
	if tsalias.IsConfigFileName(name) || name == path.Base(nuxtimports.GeneratedList) {
		return true
	}
	names := nuxtimports.ConfigFileNames()
	return slices.Contains(names[:], name)
}

// refreshResolutionInputsFor is refreshResolutionInputs for a pass that
// already knows whether its scope admitted a JS/TS file (the full index).
func (idx *Indexer) refreshResolutionInputsFor(root string, scopeHasJSTS bool, points ...string) (*resolutionInputs, error) {
	if idx.manifestTree().sourced() {
		return nil, nil
	}
	if _, delta := idx.graph.(*graph.DeltaWriter); !delta {
		needed, err := idx.resolutionPassNeeded(root, scopeHasJSTS, points)
		if err != nil {
			return nil, err
		}
		if !needed {
			return nil, nil
		}
	}
	if _, delta := idx.graph.(*graph.DeltaWriter); delta {
		// The delta planner pinned the inputs and refused a changed policy.
		// It does not certify a rebind for the checkout.
		if idx.deltaResolution != nil {
			idx.activeResolution.Store(idx.deltaResolution)
		}
		return idx.deltaResolution, nil
	}
	idx.resolutionMu.Lock()
	if pending := idx.warmResolution; pending != nil {
		// A new mutation overtook deferred warmup. Its incomplete pass cannot
		// certify this graph; recover exactly as after a restart.
		idx.resolutionRetry = &resolutionRetry{inputs: pending.inputs}
		idx.warmResolution = nil
	}
	idx.resolutionMu.Unlock()
	prior := idx.appliedResolution.Load()
	retry := idx.resolutionRetry
	if retry != nil {
		prior = retry.inputs
	}
	idx.resolutionInputsChanged = retry != nil && retry.files == nil
	current := prior
	if current == nil {
		// A new indexer for a checkout another build already scanned. Recheck
		// those inputs by content rather than scanning the whole tree again.
		current = cachedResolutionInputs(root)
	}
	if !current.unchangedForPoints(root, points, idx.contentProof) {
		var err error
		current, err = discoverResolutionInputs(root, idx.contentProof)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			idx.resolutionInputsChanged = idx.resolutionInputsChanged || prior.aliases.Digest() != current.aliases.Digest() || !prior.nuxt.Equal(current.nuxt)
		}
	}
	if store := idx.resolutionStore(); store != nil && prior == nil {
		stored, err := store.TSAliasDigest(idx.repoPrefix)
		if err != nil {
			return nil, err
		}
		if stored != current.digest {
			// A brand new scoped index has no callers to repair. Only
			// widen the request when this graph already owns JS/TS files.
			idx.resolutionInputsChanged = stored != "" || idx.hasResolutionCallers()
		}
	}
	cacheResolutionInputs(root, current)
	idx.activeResolution.Store(current)
	return current, nil
}

// Invalidate the certificate before binding rows can be replaced. A deferred
// resolver can leave a gap even when the input bytes themselves did not change.
func (idx *Indexer) fenceResolutionWrites(inputs *resolutionInputs, bindingWrites bool) error {
	if inputs == nil || (!idx.resolutionInputsChanged && !bindingWrites) {
		return nil
	}
	return idx.invalidateResolutionDigest()
}

func (idx *Indexer) invalidateResolutionDigest() error {
	if store := idx.resolutionStore(); store != nil {
		if err := store.SetTSAliasDigest(idx.repoPrefix, ""); err != nil {
			return err
		}
	}
	idx.appliedResolution.Store(nil)
	return nil
}

func (idx *Indexer) hasResolutionCallers() bool {
	var repos []string
	if idx.repoPrefix != "" {
		repos = []string{idx.repoPrefix}
	}
	for file := range graph.FileNodeIdentitiesSeq(idx.graph, repos) {
		if file.RepoPrefix == idx.repoPrefix && closureJSTSPath(file.FilePath) {
			return true
		}
	}
	return false
}

var resolutionCache = struct {
	sync.Mutex
	byRoot  map[string]*resolutionInputs
	loading map[string]*resolutionLoad
}{byRoot: make(map[string]*resolutionInputs), loading: make(map[string]*resolutionLoad)}

// resolutionLoad is one in-flight discovery walk; later callers for the same
// root wait for it instead of walking again.
type resolutionLoad struct {
	done   chan struct{}
	inputs *resolutionInputs
}

// workingResolutionInputs walks outside the cache lock, so a cold walk of one
// checkout never stalls lookups or saves in another.
func workingResolutionInputs(root string, proof *buildContentProof) *resolutionInputs {
	resolutionCache.Lock()
	inputs := resolutionCache.byRoot[root]
	if inputs == nil {
		load := resolutionCache.loading[root]
		if load == nil {
			load = &resolutionLoad{done: make(chan struct{})}
			resolutionCache.loading[root] = load
			resolutionCache.Unlock()
			discovered, err := discoverResolutionInputs(root, proof)
			resolutionCache.Lock()
			if err == nil {
				load.inputs = discovered
				if resolutionCache.byRoot[root] == nil {
					resolutionCache.byRoot[root] = discovered
				}
			}
			delete(resolutionCache.loading, root)
			resolutionCache.Unlock()
			close(load.done)
			if err != nil {
				return &resolutionInputs{}
			}
			return discovered
		}
		resolutionCache.Unlock()
		<-load.done
		if inputs = load.inputs; inputs == nil {
			return &resolutionInputs{}
		}
	} else {
		resolutionCache.Unlock()
	}
	if proof.active() {
		for rel, sum := range inputs.reads {
			proof.record(filepath.Join(root, filepath.FromSlash(rel)), sum)
		}
	}
	return inputs
}

func resolutionInputsForTree(tree manifestTree) *resolutionInputs {
	if tree.sourced() {
		src, _ := tree.(tsalias.Tree)
		return &resolutionInputs{aliases: tsalias.LoadTree(src), nuxt: nuxtimports.LoadTree(src)}
	}
	proven, _ := tree.(provenManifestTree)
	return workingResolutionInputs(tree.root(), proven.proof)
}

// checkoutResolutionInputs rechecks the cached inputs against the changed
// paths, and discovers them again only when that cannot prove them unchanged.
func checkoutResolutionInputs(root string, proof *buildContentProof, points []string) (*resolutionInputs, error) {
	if cached := cachedResolutionInputs(root); cached.unchangedForPoints(root, points, proof) {
		return cached, nil
	}
	current, err := discoverResolutionInputs(root, proof)
	if err != nil {
		return nil, err
	}
	cacheResolutionInputs(root, current)
	return current, nil
}

func cachedResolutionInputs(root string) *resolutionInputs {
	resolutionCache.Lock()
	defer resolutionCache.Unlock()
	return resolutionCache.byRoot[root]
}

func invalidateResolutionInputs(root string) {
	resolutionCache.Lock()
	delete(resolutionCache.byRoot, root)
	resolutionCache.Unlock()
}

func cacheResolutionInputs(root string, current *resolutionInputs) {
	resolutionCache.Lock()
	resolutionCache.byRoot[root] = current
	resolutionCache.Unlock()
}

// resolutionCertifiedFor reports whether inputs are the policy the graph was
// last certified against, in memory or durably. Call it before
// fenceResolutionWrites clears the in-memory certificate.
func (idx *Indexer) resolutionCertifiedFor(inputs *resolutionInputs) bool {
	if inputs == nil || idx.resolutionInputsChanged || idx.resolutionRetry != nil {
		return false
	}
	if applied := idx.appliedResolution.Load(); applied != nil {
		return applied.digest == inputs.digest
	}
	store := idx.resolutionStore()
	if store == nil {
		return false
	}
	stored, err := store.TSAliasDigest(idx.repoPrefix)
	return err == nil && stored != "" && stored == inputs.digest
}

func (idx *Indexer) hasUncertifiedResolutionPass() bool {
	active := idx.activeResolution.Load()
	if active == nil {
		return idx.resolutionRetry != nil || idx.resolutionInputsChanged
	}
	applied := idx.appliedResolution.Load()
	return applied == nil || applied.digest != active.digest || idx.resolutionRetry != nil || idx.resolutionInputsChanged
}

func (idx *Indexer) finishResolutionPass(work resolutionCompletion) (passErr error) {
	if dropped := work.dropped; len(dropped) > 0 {
		if work.inputs == nil {
			// A forced re-resolve or non-JS eviction is not an input pass.
			// Merge its drops with any unfinished retry rather than replacing it.
			work.inputs, work.success = idx.appliedResolution.Load(), true
			if retry := idx.resolutionRetry; retry != nil {
				work.inputs, work.success = retry.inputs, retry.files != nil
				for rel := range retry.files {
					work.failed = append(work.failed, filepath.Join(idx.rootPath, filepath.FromSlash(rel)))
				}
			}
		}
		// Preserve a failure of unknown extent. Otherwise retry only callers
		// whose invalidation might have been dropped. Restart sees the fence.
		if work.success || len(work.failed) > 0 {
			for _, file := range dropped {
				if rel, owned := idx.graphPathRelKey(file); owned {
					work.failed = append(work.failed, filepath.Join(idx.rootPath, filepath.FromSlash(rel)))
				}
			}
		}
		work.success, work.warm = false, false
		passErr = fmt.Errorf("%w: binding write did not apply; %d caller edges need retry", resolver.ErrReindexWriteRefused, len(dropped))
		// Non-JS callers can fail outside the input-change preflight.
		passErr = errors.Join(passErr, idx.invalidateResolutionDigest())
	}
	if work.catchupErr != nil {
		work.success, work.warm = false, false
		if !work.retryScoped {
			work.failed = nil
		}
		passErr = errors.Join(passErr, work.catchupErr, idx.invalidateResolutionDigest())
	}
	if work.inputs == nil && len(work.dropped) == 0 {
		return passErr
	}
	prior := idx.appliedResolution.Load()
	needsStamp := prior == nil || work.inputs == nil || prior.digest != work.inputs.digest || idx.resolutionRetry != nil || idx.resolutionInputsChanged
	idx.resolutionInputsChanged = false
	if !work.warm {
		defer idx.activeResolution.Store(nil)
	}
	if !needsStamp && !work.warm && passErr == nil {
		return nil
	}
	if !work.success {
		retry := &resolutionRetry{inputs: work.inputs}
		if len(work.failed) != 0 {
			retry.files = make(map[string]bool, len(work.failed))
			for _, file := range work.failed {
				retry.files[idx.relKey(file)] = true
			}
		} else if work.retryScoped && len(work.dropped) == 0 {
			// Scoped binding-write refusal is already captured in pendingResolutionRetry;
			// keep inputs pinned without forcing disk re-extraction of unchanged files,
			// while leaving the durable digest empty for restart recovery.
			if prev := idx.resolutionRetry; prev != nil && prev.files != nil {
				retry.files = prev.files
			} else {
				retry.files = map[string]bool{}
			}
		}
		idx.resolutionRetry = retry
		idx.appliedResolution.Store(nil)
	} else {
		idx.resolutionRetry = nil
	}
	if work.warm {
		idx.resolutionMu.Lock()
		idx.warmResolution = &work
		idx.resolutionMu.Unlock()
		return nil
	}
	if !work.success {
		return passErr
	}
	if err := idx.stampResolution(work.inputs); err != nil {
		idx.resolutionRetry = &resolutionRetry{inputs: work.inputs}
		return err
	}
	return nil
}

// stampResolution certifies inputs. The input record goes first: a crash
// before the digest write leaves the digest cleared, so the record is never
// trusted on its own.
func (idx *Indexer) stampResolution(inputs *resolutionInputs) error {
	if store := idx.resolutionStore(); store != nil {
		if inputStore, ok := store.(resolutionInputStore); ok {
			if err := inputStore.SetTSAliasInputs(idx.repoPrefix, inputs.reads); err != nil {
				return err
			}
		}
		if err := store.SetTSAliasDigest(idx.repoPrefix, inputs.digest); err != nil {
			return err
		}
	}
	idx.appliedResolution.Store(inputs)
	return nil
}

func (idx *Indexer) inputsStaleForFile(rel string) bool {
	return idx.resolutionRetry != nil && idx.resolutionRetry.files[rel] || idx.resolutionInputsChanged && closureJSTSPath(rel)
}

// Capture the passes before the warm resolver starts. A repository mutated in
// the meantime cancels its old completion in refreshResolutionInputs.
func (mi *MultiIndexer) warmResolutionPasses(scope map[string]struct{}) map[*Indexer]*resolutionCompletion {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var pending map[*Indexer]*resolutionCompletion
	for prefix, idx := range mi.indexers {
		if _, ok := scope[prefix]; len(scope) > 0 && !ok {
			continue
		}
		idx.resolutionMu.Lock()
		work := idx.warmResolution
		idx.resolutionMu.Unlock()
		if work != nil {
			if pending == nil {
				pending = make(map[*Indexer]*resolutionCompletion)
			}
			pending[idx] = work
		}
	}
	return pending
}

func finishWarmResolutions(pending map[*Indexer]*resolutionCompletion) error {
	for idx, work := range pending {
		idx.resolutionMu.Lock()
		var err error
		if idx.warmResolution == work {
			if work.success {
				err = idx.stampResolution(work.inputs)
			}
			if err == nil {
				idx.warmResolution = nil
				idx.activeResolution.Store(nil)
			}
		}
		idx.resolutionMu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// The direct full pass upserts raw extraction rows and resolves after every
// worker has finished. Remove this caller's old outgoing rows only once its
// replacement is ready. EvictFile would also remove other files' incoming
// edges, including ones already written by another worker.
func (idx *Indexer) removePriorAliasEdges(file string) {
	nodes := idx.graph.GetFileNodes(file)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	var prior []*graph.Edge
	for _, edges := range idx.graph.GetOutEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if e.FilePath == file {
				prior = append(prior, e)
			}
		}
	}
	graph.RemoveEdgesExact(idx.graph, prior)
}
