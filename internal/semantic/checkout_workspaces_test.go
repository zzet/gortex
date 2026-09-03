package semantic

import (
	"reflect"
	"sort"
	"sync"
	"testing"
	"testing/synctest"

	"go.uber.org/zap"
)

// recordingStopper stands in for the LSP router: it records what the registry
// asked it to stop, which is the only observable an eviction has when no real
// language server is involved.
type recordingStopper struct {
	mu      sync.Mutex
	stopped []CheckoutWorkspaceRef
}

func (s *recordingStopper) CloseCheckoutWorkspace(language, root string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, CheckoutWorkspaceRef{Language: language, Root: root})
	return 1
}

func (s *recordingStopper) calls() []CheckoutWorkspaceRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CheckoutWorkspaceRef(nil), s.stopped...)
}

// blockingStopper wedges the shutdown handshake for as long as the test wants,
// standing in for a language server that takes its time exiting.
type blockingStopper struct{ until <-chan struct{} }

func (s blockingStopper) CloseCheckoutWorkspace(string, string) int {
	<-s.until
	return 1
}

// acquire admits one pair and fails the test when the cap refused it, for the
// steps of a test that are setup rather than the claim.
func acquire(t *testing.T, w *CheckoutWorkspaces, language, root string) func() {
	t.Helper()
	release, ok := w.Acquire(language, root)
	if !ok {
		t.Fatalf("the cap refused %s at %s", language, root)
	}
	return release
}

// TestCheckoutWorkspacesEvictsTheLeastRecentlyUsedPair is the cap's defining
// behaviour: a third pair over a cap of two takes the slot of whichever of the
// first two was used longest ago, and the evicted pair's servers are stopped
// rather than merely forgotten.
func TestCheckoutWorkspacesEvictsTheLeastRecentlyUsedPair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(2, zap.NewNop())
		w.SetStopper(stopper)

		acquire(t, w, "go", "/family/first")()
		acquire(t, w, "go", "/family/second")()
		// Touching the first pair again makes the second the oldest.
		acquire(t, w, "go", "/family/first")()

		acquire(t, w, "go", "/family/third")()

		// The slot is free the moment the bookkeeping says so; the subprocess
		// behind it is stopped off the admitting caller's goroutine.
		live := w.Live()
		wantLive := []CheckoutWorkspaceRef{
			{Language: "go", Root: "/family/first"},
			{Language: "go", Root: "/family/third"},
		}
		if !reflect.DeepEqual(live, wantLive) {
			t.Errorf("live = %v, want %v", live, wantLive)
		}

		synctest.Wait()
		want := []CheckoutWorkspaceRef{{Language: "go", Root: "/family/second"}}
		if got := stopper.calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("stopped %v, want %v", got, want)
		}
	})
}

// TestCheckoutWorkspacesEvictionDoesNotHoldTheRegistryLock is the reason the
// stop runs off the caller: a shutdown handshake takes as long as the language
// server takes, and every other checkout's admission would wait behind it.
func TestCheckoutWorkspacesEvictionDoesNotHoldTheRegistryLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := make(chan struct{})
		w := NewCheckoutWorkspaces(1, zap.NewNop())
		w.SetStopper(blockingStopper{until: blocked})

		acquire(t, w, "go", "/family/first")()
		// Evicts the first pair, whose stopper is wedged until this test says
		// otherwise.
		acquire(t, w, "go", "/family/second")()
		synctest.Wait()

		// The registry answers while the stop is still in flight.
		if got := w.Live(); len(got) != 1 || got[0].Root != "/family/second" {
			t.Fatalf("live = %v, want the second pair alone", got)
		}
		acquire(t, w, "go", "/family/third")()
		close(blocked)
	})
}

// TestCheckoutWorkspacesEvictRootStopsADepartedCheckout pins the other way a
// pair goes away: the checkout it served left, so its servers are stopped then
// rather than when some other checkout happens to want the slot.
func TestCheckoutWorkspacesEvictRootStopsADepartedCheckout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(4, zap.NewNop())
		w.SetStopper(stopper)

		acquire(t, w, "go", "/family/first")()
		acquire(t, w, "typescript", "/family/first")()
		held := acquire(t, w, "go", "/family/second")

		if got := w.EvictRoot("/family/first"); got != 2 {
			t.Errorf("EvictRoot dropped %d pairs, want both of the checkout's", got)
		}
		// A pair a pass still holds survives its checkout's departure, the way
		// it survives the cap: the pass is reading that very server.
		if got := w.EvictRoot("/family/second"); got != 0 {
			t.Errorf("EvictRoot dropped %d held pairs, want none", got)
		}
		if got := w.Live(); len(got) != 1 || got[0].Root != "/family/second" {
			t.Errorf("live = %v, want the held pair alone", got)
		}
		held()

		synctest.Wait()
		want := []CheckoutWorkspaceRef{
			{Language: "go", Root: "/family/first"},
			{Language: "typescript", Root: "/family/first"},
		}
		got := stopper.calls()
		sort.Slice(got, func(i, j int) bool { return got[i].Language < got[j].Language })
		if !reflect.DeepEqual(got, want) {
			t.Errorf("stopped %v, want %v", got, want)
		}
	})
}

// TestCheckoutWorkspacesRefusesWhenEveryPairIsHeld pins the starvation case:
// the evictor never cuts a pass short, so a cap full of in-flight passes is a
// refusal rather than an eviction. The refused caller is what turns that into
// a skipped enrichment stage instead of a blocked build.
func TestCheckoutWorkspacesRefusesWhenEveryPairIsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(2, zap.NewNop())
		w.SetStopper(stopper)

		holdOne := acquire(t, w, "go", "/family/first")
		holdTwo := acquire(t, w, "go", "/family/second")

		if _, ok := w.Acquire("go", "/family/third"); ok {
			t.Fatal("the cap admitted a third pair while both slots were held")
		}
		synctest.Wait()
		if got := stopper.calls(); len(got) != 0 {
			t.Errorf("a held pair was stopped: %v", got)
		}

		// Releasing one makes it evictable, and the refused pair gets in.
		holdOne()
		release := acquire(t, w, "go", "/family/third")
		release()
		holdTwo()

		synctest.Wait()
		want := []CheckoutWorkspaceRef{{Language: "go", Root: "/family/first"}}
		if got := stopper.calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("stopped %v, want %v", got, want)
		}
	})
}

// TestCheckoutWorkspacesKeysByLanguageAndRoot proves the two dimensions are
// both part of the key: one root in two languages is two servers, and one
// language at two roots is two servers.
func TestCheckoutWorkspacesKeysByLanguageAndRoot(t *testing.T) {
	w := NewCheckoutWorkspaces(4, zap.NewNop())

	acquire(t, w, "go", "/family/first")()
	acquire(t, w, "typescript", "/family/first")()
	acquire(t, w, "go", "/family/second")()
	// The same pair spelled with a trailing separator is the same working
	// copy, so it must not take a second slot.
	acquire(t, w, "go", "/family/second/")()

	want := []CheckoutWorkspaceRef{
		{Language: "go", Root: "/family/first"},
		{Language: "typescript", Root: "/family/first"},
		{Language: "go", Root: "/family/second"},
	}
	if got := w.Live(); !reflect.DeepEqual(got, want) {
		t.Errorf("live = %v, want %v", got, want)
	}
}

// TestCheckoutWorkspacesDefaultsTheCap pins the shipped default, since the
// knob's whole job is to be optional.
func TestCheckoutWorkspacesDefaultsTheCap(t *testing.T) {
	if got := NewCheckoutWorkspaces(0, zap.NewNop()).Cap(); got != defaultCheckoutWorkspaceCap {
		t.Errorf("default cap = %d, want %d", got, defaultCheckoutWorkspaceCap)
	}
	if got := NewCheckoutWorkspaces(2, zap.NewNop()).Cap(); got != 2 {
		t.Errorf("configured cap = %d, want 2", got)
	}
	if got := NewManager(Config{CheckoutLSPMaxWorkspaces: 3}, zap.NewNop()).CheckoutWorkspaces().Cap(); got != 3 {
		t.Errorf("manager cap = %d, want 3", got)
	}
}
