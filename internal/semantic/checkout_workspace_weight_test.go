package semantic

import (
	"reflect"
	"testing"
	"testing/synctest"

	"go.uber.org/zap"
)

// withCheckoutWorkspaceWeights installs a per-language weight table for one
// test, standing in for the LSP registry's own registration. The registry
// lives in a package that imports this one, so weights reach the cap by
// registration rather than a direct call — and a test that wants a heavy
// server declares one the same way the registry does.
func withCheckoutWorkspaceWeights(t *testing.T, weights map[string]int) {
	t.Helper()
	checkoutWeightMu.Lock()
	previous := checkoutWeightRegistrations
	checkoutWeightRegistrations = []func() map[string]int{
		func() map[string]int { return weights },
	}
	checkoutWeightMu.Unlock()
	t.Cleanup(func() {
		checkoutWeightMu.Lock()
		checkoutWeightRegistrations = previous
		checkoutWeightMu.Unlock()
	})
}

// TestCheckoutWorkspacesChargesAHeavyServerSeveralSlots is the weighting's
// reason to exist: the cap bounds memory, and a gigabytes-resident server
// costs an order of magnitude more of it than the 200-500MB subprocess the
// budget is denominated in. Counting workspaces let four of the heavy ones sit
// inside a cap of four; counting slots stops at two.
func TestCheckoutWorkspacesChargesAHeavyServerSeveralSlots(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})

	t.Run("two heavy pairs spend the whole default budget", func(t *testing.T) {
		w := NewCheckoutWorkspaces(0, zap.NewNop())

		holdFirst := acquire(t, w, "java", "/family/first")
		holdSecond := acquire(t, w, "java", "/family/second")

		if _, ok := w.Acquire("java", "/family/third"); ok {
			t.Error("a third heavy workspace was admitted over a budget two of them spend")
		}
		// Nor does an ordinary server fit behind them: the budget is spent and
		// every pair holding it is mid-pass, so this is the refusal that skips
		// a stage rather than an eviction.
		if _, ok := w.Acquire("go", "/family/third"); ok {
			t.Error("an ordinary workspace was admitted over a spent budget")
		}

		holdFirst()
		holdSecond()
	})

	t.Run("four ordinary pairs still fit", func(t *testing.T) {
		w := NewCheckoutWorkspaces(0, zap.NewNop())

		holds := []func(){
			acquire(t, w, "go", "/family/first"),
			acquire(t, w, "go", "/family/second"),
			acquire(t, w, "go", "/family/third"),
			acquire(t, w, "go", "/family/fourth"),
		}
		if _, ok := w.Acquire("go", "/family/fifth"); ok {
			t.Error("a fifth ordinary workspace was admitted over a budget of four slots")
		}

		for _, release := range holds {
			release()
		}
	})
}

// TestCheckoutWorkspacesEvictionFreesTheWholeHeavyWeight pins that eviction is
// weight-consistent: dropping one heavy pair gives back every slot it charged.
// An evictor that credited one slot per victim would stop a second server to
// make the same room, which is a warm workspace lost for nothing.
func TestCheckoutWorkspacesEvictionFreesTheWholeHeavyWeight(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(4, zap.NewNop())
		w.SetStopper(stopper)

		acquire(t, w, "java", "/family/first")()
		acquire(t, w, "java", "/family/second")()
		// The budget is spent, so the ordinary pair displaces the heavy one
		// used longest ago — and the two slots that pair held are more than
		// this admission needs.
		acquire(t, w, "go", "/family/third")()

		wantLive := []CheckoutWorkspaceRef{
			{Language: "java", Root: "/family/second"},
			{Language: "go", Root: "/family/third"},
		}
		if live := w.Live(); !reflect.DeepEqual(live, wantLive) {
			t.Errorf("live = %v, want %v", live, wantLive)
		}
		// The leftover slot is real: a second ordinary pair fits without
		// costing the surviving heavy one its server.
		acquire(t, w, "go", "/family/fourth")()

		synctest.Wait()
		want := []CheckoutWorkspaceRef{{Language: "java", Root: "/family/first"}}
		if got := stopper.calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("stopped %v, want %v", got, want)
		}
	})
}

// TestCheckoutWorkspacesEvictRootFreesTheWholeHeavyWeight pins the same
// weight-consistency on the other eviction path. EvictRoot is what every
// departing checkout runs, and a credit short of what the pair charged leaks
// budget per departure: the slots stay spent on servers that are gone, and the
// checkouts still being served lose admissions to them until the daemon
// restarts.
func TestCheckoutWorkspacesEvictRootFreesTheWholeHeavyWeight(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(4, zap.NewNop())
		w.SetStopper(stopper)

		// Two of the four slots go to the checkout that is about to depart,
		// and the third to one that stays — held, so nothing below can reach
		// its slot by evicting it.
		acquire(t, w, "java", "/family/departing")()
		staying := acquire(t, w, "go", "/family/staying")

		if got := w.EvictRoot("/family/departing"); got != 1 {
			t.Fatalf("EvictRoot dropped %d pairs, want the departed checkout's one", got)
		}

		// Three ordinary pairs now fit beside the held one, which is exactly
		// the budget the departure gave back. A credit of one slot per
		// departed pair — or none — leaves the last of them nothing to be
		// admitted into.
		holds := []func(){
			acquire(t, w, "go", "/family/first"),
			acquire(t, w, "go", "/family/second"),
			acquire(t, w, "go", "/family/third"),
		}
		if got := w.Live(); len(got) != 4 {
			t.Errorf("live = %v, want the held pair and three admitted beside it", got)
		}

		synctest.Wait()
		// The readmissions came out of the freed budget, so none of them cost
		// a surviving workspace its server.
		want := []CheckoutWorkspaceRef{{Language: "java", Root: "/family/departing"}}
		if got := stopper.calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("stopped %v, want %v", got, want)
		}

		for _, release := range holds {
			release()
		}
		staying()
	})
}

// TestCheckoutWorkspaceCapRaiseWidensTheWeightedBudget keeps the operator
// knob's meaning under the weighting: checkout_lsp_max_workspaces buys slots,
// so an operator whose machine has the memory for a third heavy server says so
// with a number rather than losing the knob to a per-server rule.
func TestCheckoutWorkspaceCapRaiseWidensTheWeightedBudget(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})
	w := NewManager(Config{CheckoutLSPMaxWorkspaces: 6}, zap.NewNop()).CheckoutWorkspaces()

	holds := []func(){
		acquire(t, w, "java", "/family/first"),
		acquire(t, w, "java", "/family/second"),
		acquire(t, w, "java", "/family/third"),
	}
	if _, ok := w.Acquire("java", "/family/fourth"); ok {
		t.Error("a fourth heavy workspace was admitted over a raised budget three of them spend")
	}

	for _, release := range holds {
		release()
	}
}

// TestCheckoutWorkspacesStarveWithoutStoppingWhatCannotHelp is the same rule
// one step in: a heavy admission whose weight exceeds what eviction can
// possibly reach is refused before anything is stopped. Evicting toward a
// budget the admission will never reach costs the other checkouts their warm
// servers and starves anyway, so the starved admission stays what it was under
// unit weights — one skipped stage, no subprocess.
func TestCheckoutWorkspacesStarveWithoutStoppingWhatCannotHelp(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(4, zap.NewNop())
		w.SetStopper(stopper)

		// Three slots are held by in-flight passes and out of reach; the
		// fourth is a warm pair, so eviction can free one slot and no more.
		holds := []func(){
			acquire(t, w, "go", "/family/first"),
			acquire(t, w, "go", "/family/second"),
			acquire(t, w, "go", "/family/third"),
		}
		acquire(t, w, "go", "/family/fourth")()

		if _, ok := w.Acquire("java", "/family/fifth"); ok {
			t.Fatal("a heavy workspace was admitted into a single reachable slot")
		}

		synctest.Wait()
		if got := stopper.calls(); len(got) != 0 {
			t.Errorf("the starved admission stopped %v, want the live set left warm", got)
		}
		if got := w.Live(); len(got) != 4 {
			t.Errorf("live = %v, want the four pairs the refusal did not touch", got)
		}

		for _, release := range holds {
			release()
		}
	})
}

// TestCheckoutWorkspacesRefuseAServerHeavierThanTheWholeBudget covers the
// other end of the knob: a budget an operator tightened below one server's
// weight cannot hold that server, and discovering it by evicting the live set
// first would cost the other checkouts their warm servers for nothing.
func TestCheckoutWorkspacesRefuseAServerHeavierThanTheWholeBudget(t *testing.T) {
	withCheckoutWorkspaceWeights(t, map[string]int{"java": 2})
	synctest.Test(t, func(t *testing.T) {
		stopper := &recordingStopper{}
		w := NewCheckoutWorkspaces(1, zap.NewNop())
		w.SetStopper(stopper)

		acquire(t, w, "go", "/family/first")()

		if _, ok := w.Acquire("java", "/family/second"); ok {
			t.Error("a heavy workspace was admitted into a budget smaller than its weight")
		}

		synctest.Wait()
		if got := stopper.calls(); len(got) != 0 {
			t.Errorf("the refusal stopped %v, want nothing evicted", got)
		}
		if live := w.Live(); len(live) != 1 || live[0].Language != "go" {
			t.Errorf("live = %v, want the ordinary pair alone", live)
		}
	})
}
