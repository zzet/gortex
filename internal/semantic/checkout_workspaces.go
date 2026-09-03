package semantic

import (
	"path/filepath"
	"sort"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// Per-checkout language-server workspaces.
//
// A language server is a subprocess rooted at one directory, and a routed
// automatic checkout is a directory of its own: the branch it holds and the
// uncommitted edits on top of it exist nowhere else, so a server rooted at the
// family's primary checkout answers about a different tree. Enriching a
// checkout therefore needs a server per (language, checkout root), not one per
// language.
//
// That multiplies past what a machine holds — a family with six worktrees in
// three languages is eighteen servers — so the pairs are admitted against one
// global cap. Over the cap the least recently used pair is stopped outright. A
// stopped workspace keeps nothing: the only thing it held was a subprocess,
// and the facts it produced already live in the generation whose build ran it.
// A checkout that stops being served gives its pairs back the same way, without
// waiting for another checkout to want the slot.
//
// A pair the cap cannot admit is refused rather than queued. The caller is a
// build, and a build that waits for a server slot is a checkout whose view
// stops moving; skipping the stage and saying so in the generation's
// capability states is the better trade. Recovery needs no bookkeeping either:
// the refused checkout runs the stage again on its next working-tree build,
// which is the same build its own edits already trigger.
//
// The cap counts slots rather than workspaces. A language server's cost is its
// resident set, and those differ by an order of magnitude: most servers here
// are a 200-500MB subprocess, while an Eclipse JDT or Roslyn workspace runs
// into gigabytes. Charging every pair one slot bounded the count and nothing
// else — four of the heavy ones is ~20GB of language servers on top of what
// the tracked repositories keep warm. Each pair therefore charges the weight
// its language's server declared (lsp.ServerSpec.CheckoutWorkspaceWeight,
// reaching this package through RegisterCheckoutWorkspaceWeights), so the
// worst case the cap allows is bounded in approximate memory.

// defaultCheckoutWorkspaceCap bounds the language servers routed checkouts may
// hold at once, in slots: four ordinary servers' worth, or one multi-GB server
// with two ordinary ones beside it. It sits below the router's own
// live-provider cap because these servers are additional to the ones the
// tracked repositories themselves keep warm: a checkout's server answers about
// a worktree, and the worktree is a second copy of a repository that already
// has one.
const defaultCheckoutWorkspaceCap = 4

// WorkspaceStopper stops the language servers one (language, root) pair holds.
//
// It is an optional capability rather than a required collaborator: the LSP
// router implements it, and a manager with no router — every test that
// enriches through an in-process provider — evicts its own bookkeeping and has
// nothing else to stop.
type WorkspaceStopper interface {
	// CloseCheckoutWorkspace stops every language server serving language at
	// root and reports how many it stopped.
	CloseCheckoutWorkspace(language, root string) int
}

// CheckoutWorkspaceRef is one admitted (language, checkout root) pair.
type CheckoutWorkspaceRef struct {
	Language string
	Root     string
}

// CheckoutWorkspaces admits (language, checkout root) pairs against a global
// cap and evicts the least recently used pair when the cap is reached.
type CheckoutWorkspaces struct {
	logger *zap.Logger

	mu      sync.Mutex
	cap     int
	stopper WorkspaceStopper
	// clock orders the live set for eviction. A counter rather than a wall
	// clock: what the evictor needs is the order acquisitions happened in, and
	// two acquisitions inside one timer tick have an order a timestamp loses.
	clock uint64
	// charged is the slots the live pairs spend between them, which is what
	// the cap bounds. Carried alongside the live set rather than recomputed,
	// so admission and eviction cannot disagree about the budget.
	charged int
	// weights maps a language to the slots one of its workspaces charges,
	// snapshotted at construction from what the LSP registry registered. A
	// language absent from it weighs one.
	weights map[string]int
	live    map[CheckoutWorkspaceRef]*checkoutWorkspace
}

// checkoutWorkspace is one live pair's admission state.
type checkoutWorkspace struct {
	// held counts the passes currently reading this workspace. A held pair is
	// never the eviction victim: the pass that holds it is mid-flight against
	// the very server the eviction would stop.
	held int
	// used is the clock value of the most recent acquisition, which is the
	// order the evictor picks its victim in.
	used uint64
	// weight is the slots this pair charged when it was admitted, remembered
	// here so eviction gives back exactly what admission took.
	weight int
}

// NewCheckoutWorkspaces builds the registry. A non-positive cap takes
// defaultCheckoutWorkspaceCap. The per-language weights are read once here:
// they come from a registry compiled into the binary, not from anything that
// changes while the daemon runs.
func NewCheckoutWorkspaces(cap int, logger *zap.Logger) *CheckoutWorkspaces {
	if cap <= 0 {
		cap = defaultCheckoutWorkspaceCap
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CheckoutWorkspaces{
		logger:  logger,
		cap:     cap,
		weights: checkoutWorkspaceWeights(),
		live:    make(map[CheckoutWorkspaceRef]*checkoutWorkspace, cap),
	}
}

// SetStopper installs what stops an evicted pair's servers. Passing nil
// detaches it, leaving the registry to evict its own bookkeeping alone.
func (w *CheckoutWorkspaces) SetStopper(stopper WorkspaceStopper) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopper = stopper
	w.mu.Unlock()
}

// Cap reports the global budget the live pairs spend between them, in slots:
// an ordinary language server spends one and a gigabytes-resident one spends
// several, so the number is a bound on approximate memory rather than on how
// many pairs may be live.
func (w *CheckoutWorkspaces) Cap() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cap
}

// Live lists the admitted pairs in a stable order.
func (w *CheckoutWorkspaces) Live() []CheckoutWorkspaceRef {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	out := make([]CheckoutWorkspaceRef, 0, len(w.live))
	for ref := range w.live {
		out = append(out, ref)
	}
	w.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Root != out[j].Root {
			return out[i].Root < out[j].Root
		}
		return out[i].Language < out[j].Language
	})
	return out
}

// Acquire admits one (language, root) pair, charging the slots its language's
// server weighs, and returns the release that gives its hold back. The second
// return is false when the budget cannot reach that weight — the slots are
// spent on pairs other passes are holding, or the language weighs more than
// the whole budget — which is the caller's signal to skip the stage, and it
// leaves the live set as it found it.
//
// The returned release must be called once. It does not stop the server: the
// point of keeping it alive past the pass is that the next build over the same
// checkout finds it warm.
func (w *CheckoutWorkspaces) Acquire(language, root string) (func(), bool) {
	if w == nil || language == "" || root == "" {
		return nil, false
	}
	ref := CheckoutWorkspaceRef{Language: language, Root: cleanCheckoutRoot(root)}
	release, stops, admitted := w.admit(ref)
	stopEvicted(stops)
	return release, admitted
}

// admit runs the admission bookkeeping under the mutex and hands back the
// stops it owes for the pairs it evicted to make room. The stops are returned
// rather than run here because the mutex is held for the whole of this.
func (w *CheckoutWorkspaces) admit(ref CheckoutWorkspaceRef) (func(), []func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry, live := w.live[ref]; live {
		entry.held++
		w.clock++
		entry.used = w.clock
		viewmetrics.Count(viewmetrics.LSPWorkspaceTotal, viewmetrics.WorkspaceReused)
		return w.releaseFor(ref), nil, true
	}
	weight := w.weightFor(ref.Language)
	stops, admissible := w.makeRoomLocked(weight)
	if !admissible {
		// The budget cannot reach this weight, so the admission is starved.
		// It is the one outcome that costs a stage rather than a subprocess.
		viewmetrics.Count(viewmetrics.LSPWorkspaceTotal, viewmetrics.WorkspaceStarved)
		return nil, nil, false
	}
	w.clock++
	w.live[ref] = &checkoutWorkspace{held: 1, used: w.clock, weight: weight}
	w.charged += weight
	viewmetrics.Count(viewmetrics.LSPWorkspaceTotal, viewmetrics.WorkspaceAcquired)
	return w.releaseFor(ref), stops, true
}

// weightFor is the slots one workspace in this language charges. A language no
// server declared a weight for spends one, which is the unit the cap counts.
func (w *CheckoutWorkspaces) weightFor(language string) int {
	if weight := w.weights[language]; weight > 1 {
		return weight
	}
	return 1
}

// EvictRoot drops every unheld pair at one checkout root, stops its servers,
// and reports how many pairs it dropped.
//
// It is what a departing checkout calls. The servers are rooted at a directory
// nothing serves any more — often one that has been deleted — and without this
// they would keep running until the router's idle reaper or the cap's own
// pressure got to them. A held pair is left alone, exactly as it is under the
// cap: the pass holding it is reading the very server the eviction would stop.
func (w *CheckoutWorkspaces) EvictRoot(root string) int {
	if w == nil || root == "" {
		return 0
	}
	target := cleanCheckoutRoot(root)

	w.mu.Lock()
	var stops []func()
	for ref, entry := range w.live {
		if ref.Root != target || entry.held > 0 {
			continue
		}
		delete(w.live, ref)
		w.charged -= entry.weight
		stops = append(stops, w.stopLocked(ref, entry.weight, "the checkout it served went away"))
	}
	w.mu.Unlock()
	viewmetrics.Add(viewmetrics.LSPWorkspaceTotal, int64(len(stops)), viewmetrics.WorkspaceEvicted)
	stopEvicted(stops)
	return len(stops)
}

// releaseFor builds the release for one pair. The pair may have been evicted
// by the time it runs — a release is not a claim that the workspace is still
// live, only that this pass has stopped reading it.
func (w *CheckoutWorkspaces) releaseFor(ref CheckoutWorkspaceRef) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			if entry, live := w.live[ref]; live && entry.held > 0 {
				entry.held--
			}
			w.mu.Unlock()
		})
	}
}

// makeRoomLocked frees the budget an admission of this weight needs by
// dropping the least recently used unheld pairs, and returns what stops their
// servers. The second return is false when the budget cannot hold the weight
// at all, in which case nothing is dropped.
//
// It picks the whole victim set before dropping any of it, because a refusal
// must cost no servers. A pair a pass holds is out of reach — the pass is
// reading the very server the eviction would stop — so an admission heavier
// than the free slots plus the unheld pairs' is starved however many pairs are
// stopped for it: evicting toward that budget would leave the other checkouts
// cold and the stage skipped anyway. That covers both shapes of the refusal, a
// cap an operator tightened below one heavy server's weight and a live set
// whose reachable slots do not add up to it.
//
// A victim gives back every slot it charged, not one: it is a whole server
// that stops, so an admission behind a heavy victim takes no second one.
func (w *CheckoutWorkspaces) makeRoomLocked(weight int) ([]func(), bool) {
	need := w.charged + weight - w.cap
	if need <= 0 {
		return nil, true
	}
	victims := w.evictionOrderLocked()
	freed, taken := 0, 0
	for _, ref := range victims {
		if freed >= need {
			break
		}
		freed += w.live[ref].weight
		taken++
	}
	if freed < need {
		return nil, false
	}
	stops := make([]func(), 0, taken)
	for _, ref := range victims[:taken] {
		entry := w.live[ref]
		delete(w.live, ref)
		w.charged -= entry.weight
		viewmetrics.Count(viewmetrics.LSPWorkspaceTotal, viewmetrics.WorkspaceEvicted)
		stops = append(stops, w.stopLocked(ref, entry.weight, "the workspace cap admitted another checkout"))
	}
	return stops, true
}

// evictionOrderLocked lists the pairs the cap may take, least recently used
// first, which is the order it takes them in. Held pairs are absent: they are
// not victims at any point in the order.
func (w *CheckoutWorkspaces) evictionOrderLocked() []CheckoutWorkspaceRef {
	out := make([]CheckoutWorkspaceRef, 0, len(w.live))
	for ref, entry := range w.live {
		if entry.held == 0 {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return w.live[out[i]].used < w.live[out[j]].used })
	return out
}

// stopLocked builds the stop for a pair the registry has already dropped. The
// collaborators it needs are read here, under the mutex that guards them, so
// the returned closure can run without one.
func (w *CheckoutWorkspaces) stopLocked(ref CheckoutWorkspaceRef, weight int, why string) func() {
	stopper, logger := w.stopper, w.logger
	return func() {
		stopped := 0
		if stopper != nil {
			stopped = stopper.CloseCheckoutWorkspace(ref.Language, ref.Root)
		}
		logger.Info("checkout language-server workspace evicted",
			zap.String("language", ref.Language),
			zap.String("root", ref.Root),
			zap.String("reason", why),
			zap.Int("servers_stopped", stopped),
			zap.Int("slots_freed", weight),
		)
	}
}

// stopEvicted runs the evicted pairs' stops off the caller's goroutine.
//
// Stopping a workspace is a shutdown handshake with a subprocess followed by a
// wait on it, so it runs neither under the registry mutex — where it would
// stall every other checkout's Acquire and release for its duration — nor on
// the path of the build that displaced it, which has an enrichment pass of its
// own to get on with. The LSP router closes its own LRU victims the same way.
func stopEvicted(stops []func()) {
	for _, stop := range stops {
		go stop()
	}
}

// cleanCheckoutRoot normalises a checkout root to the form the registry keys
// by, so two spellings of one working copy share a slot instead of holding two.
func cleanCheckoutRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}

// RegisterCheckoutWorkspaceWeights lets the LSP registry declare, per language,
// how many slots one of its checkout workspaces charges against the cap. The
// registry imports this package for its own types, so the weights arrive by
// registration rather than a direct call — the same indirection
// RegisterDefaultProviders uses. A language several registrations name takes
// the largest weight any of them declared.
func RegisterCheckoutWorkspaceWeights(fn func() map[string]int) {
	if fn == nil {
		return
	}
	checkoutWeightMu.Lock()
	checkoutWeightRegistrations = append(checkoutWeightRegistrations, fn)
	checkoutWeightMu.Unlock()
}

// checkoutWorkspaceWeights merges what the registrations declare into one
// table. Only the heavy languages appear in it: an absent language weighs one,
// which is the unit the cap counts.
func checkoutWorkspaceWeights() map[string]int {
	checkoutWeightMu.RLock()
	defer checkoutWeightMu.RUnlock()
	out := make(map[string]int)
	for _, fn := range checkoutWeightRegistrations {
		for language, weight := range fn() {
			if weight > out[language] {
				out[language] = weight
			}
		}
	}
	return out
}

var (
	checkoutWeightMu            sync.RWMutex
	checkoutWeightRegistrations []func() map[string]int
)
