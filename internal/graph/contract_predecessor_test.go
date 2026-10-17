package graph

import (
	"errors"
	"testing"
)

func TestContractSingleMovedWitnessesKeepOtherPreviousRiders(t *testing.T) {
	a := ContractInputWitness{GenerationID: 0, Found: true, State: ContractInputState{RepoPrefix: "a", InputVersion: "v", InputFingerprint: "new", PreviousInputVersion: "v", PreviousInputFingerprint: "old", Accepted: true}}
	b := ContractInputWitness{GenerationID: 4, Found: true, State: ContractInputState{RepoPrefix: "b", CheckoutID: "wt", InputVersion: "v", InputFingerprint: "current", PreviousInputVersion: "v", PreviousInputFingerprint: "unrelated-older", Accepted: true}}
	current := []ContractInputWitness{a, b, a}
	previous, err := ContractWitnessesWithPrevious(current, ContractWitnessSlot(a))
	if err != nil {
		t.Fatal(err)
	}
	if previous[1].State.InputFingerprint != "current" || previous[0].State.InputFingerprint != "old" || previous[2].State.InputFingerprint != "old" {
		t.Fatal("single slot override rolled an unrelated rider or missed a duplicate")
	}
	if err := ValidateSingleMovedContractWitnesses(current, previous, ContractWitnessSlot(a)); err != nil {
		t.Fatal(err)
	}
	previous[1].State.InputFingerprint = "unrelated-older"
	if !errors.Is(ValidateSingleMovedContractWitnesses(current, previous, ContractWitnessSlot(a)), ErrContractInputVector) {
		t.Fatal("two changed physical contributors accepted")
	}
}

func TestContractSingleMovedWitnessesRefusePhysicalAncestryOrAbsenceChange(t *testing.T) {
	current := []ContractInputWitness{{GenerationID: 0, Found: true, State: ContractInputState{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "new", PreviousInputVersion: "v", PreviousInputFingerprint: "old", Accepted: true}}, {GenerationID: 7, State: ContractInputState{RepoPrefix: "companion", CheckoutID: "wt"}}}
	slot := ContractWitnessSlot(current[0])
	previous, err := ContractWitnessesWithPrevious(current, slot)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"generation", "found", "namespace", "actor"} {
		changed := append([]ContractInputWitness(nil), previous...)
		switch kind {
		case "generation":
			changed[1].GenerationID++
		case "found":
			changed[1].Found = true
		case "namespace":
			changed[1].State.RepoPrefix = "other"
		case "actor":
			changed[1].State.CheckoutID = "other"
		}
		if ValidateSingleMovedContractWitnesses(current, changed, slot) == nil {
			t.Fatalf("accepted %s ancestry mismatch", kind)
		}
	}
}

func TestContractSingleMovedCountsBaseAndPositiveAsDifferentPhysicalContributors(t *testing.T) {
	base := ContractInputWitness{GenerationID: 0, Found: true, State: ContractInputState{RepoPrefix: "repo", InputVersion: "v", InputFingerprint: "base-new", PreviousInputVersion: "v", PreviousInputFingerprint: "base-old", Accepted: true}}
	top := ContractInputWitness{GenerationID: 9, Found: true, State: ContractInputState{RepoPrefix: "repo", CheckoutID: "wt", InputVersion: "v", InputFingerprint: "top-new", PreviousInputVersion: "v", PreviousInputFingerprint: "top-old", Accepted: true}}
	historical := []ContractInputWitness{base, top}
	historical[0].State.InputFingerprint = "base-old"
	historical[1].State.InputFingerprint = "top-old"
	if ValidateSingleMovedContractWitnesses([]ContractInputWitness{base, top}, historical, ContractWitnessSlot(base)) == nil {
		t.Fatal("two physical contributors in one repo were collapsed into one repo move")
	}
	// A full root contributes no mutable base: composing it stays independent
	// of a primary mutation and this fallback does not change that current rule.
	state, err := ComposeContractInputState("repo", "wt", []ContractInputWitness{top})
	if err != nil || state.InputFingerprint != "top-new" {
		t.Fatalf("full-root current composition changed: %+v %v", state, err)
	}
}
