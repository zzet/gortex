package lsp

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic"
)

// TestHeavyServersCarryACheckoutWorkspaceWeight names the servers the checkout
// budget charges extra for. The list is the documented one — an Eclipse JDT
// workspace and either Roslyn driver run gigabytes resident — and every other
// server stays out of the table, because the budget's unit is an ordinary
// server and only exceptions need declaring.
func TestHeavyServersCarryACheckoutWorkspaceWeight(t *testing.T) {
	weights := CheckoutWorkspaceWeights()

	for _, language := range []string{"java", "csharp"} {
		if got := weights[language]; got != heavyCheckoutWorkspaceWeight {
			t.Errorf("%s weighs %d slots, want %d", language, got, heavyCheckoutWorkspaceWeight)
		}
	}
	for _, language := range []string{"go", "typescript", "python", "rust"} {
		if got, weighted := weights[language]; weighted {
			t.Errorf("%s carries an explicit weight of %d, want none", language, got)
		}
	}
}

// TestTheRegistryContributesItsWeightsToTheBudget pins the wiring rather than
// the table. The budget lives in the package this one imports, so a weight a
// spec declares reaches it only through the registration this package's init
// performs; read the table directly and that registration can be gone with
// every other test still green, while the shipped binary charges a
// gigabytes-resident server the same slot as a 200-500MB one.
func TestTheRegistryContributesItsWeightsToTheBudget(t *testing.T) {
	// Three slots hold one heavy workspace and one ordinary one, and a third
	// pair fits behind them only if the heavy one was charged a single slot.
	// Both live pairs stay held, so a refusal here is the budget's and not an
	// eviction the test happened to observe.
	w := semantic.NewCheckoutWorkspaces(3, zap.NewNop())

	heavy, ok := w.Acquire("java", "/family/first")
	if !ok {
		t.Fatal("a budget of three slots refused the heavy workspace")
	}
	ordinary, ok := w.Acquire("go", "/family/first")
	if !ok {
		t.Fatal("a budget of three slots refused the ordinary workspace beside it")
	}

	if _, ok := w.Acquire("go", "/family/second"); ok {
		t.Error("a third workspace fitted three slots, so the registry's weights never reached the budget")
	}

	heavy()
	ordinary()
}

// TestOneHeavyWorkspaceFitsTheShippedDefaultBudget is the limit on how heavy a
// heavy server may be declared: the shipped budget must still admit one, with
// room for an ordinary server beside it. A weight that refused the only
// language server a Java or C# checkout can have would ration nothing — it
// would delete the stage.
func TestOneHeavyWorkspaceFitsTheShippedDefaultBudget(t *testing.T) {
	w := semantic.NewCheckoutWorkspaces(0, zap.NewNop())

	release, ok := w.Acquire("java", "/family/first")
	if !ok {
		t.Fatal("the shipped default budget refused a single heavy workspace")
	}
	release()

	release, ok = w.Acquire("go", "/family/first")
	if !ok {
		t.Fatal("the shipped default budget left no room beside a heavy workspace")
	}
	release()
}
