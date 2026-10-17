package graph

import (
	"fmt"
	"slices"
	"sort"
)

// ErrContractPredecessorRetired is reserved for known historical attachment or
// source custody which disappeared or entered retirement, not ordinary absence.
var ErrContractPredecessorRetired = fmt.Errorf("contract predecessor retired: %w", ErrContractProjectionStale)

type ContractInputPhysicalSlot struct {
	GenerationID           int64
	RepoPrefix, CheckoutID string
}

func ContractWitnessSlot(w ContractInputWitness) ContractInputPhysicalSlot {
	return ContractInputPhysicalSlot{w.GenerationID, w.State.RepoPrefix, w.State.CheckoutID}
}

// CanonicalContractWitnesses deduplicates identical physical witnesses without
// treating previous-rider bookkeeping as historical publication identity.
func CanonicalContractWitnesses(witnesses []ContractInputWitness) ([]ContractInputWitness, error) {
	if len(witnesses) == 0 || len(witnesses) > 1024 {
		return nil, ErrContractInputVector
	}
	rows := map[ContractInputPhysicalSlot]ContractInputWitness{}
	for _, w := range witnesses {
		if w.GenerationID < 0 {
			return nil, ErrContractInputVector
		}
		w.State.PreviousInputVersion, w.State.PreviousInputFingerprint = "", ""
		slot := ContractWitnessSlot(w)
		if prior, found := rows[slot]; found && prior != w {
			return nil, ErrContractInputVector
		}
		rows[slot] = w
	}
	out := make([]ContractInputWitness, 0, len(rows))
	for _, w := range rows {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := ContractWitnessSlot(out[i]), ContractWitnessSlot(out[j])
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.GenerationID != b.GenerationID {
			return a.GenerationID < b.GenerationID
		}
		return a.CheckoutID < b.CheckoutID
	})
	return out, nil
}

// EffectiveContractInputSlots follows ComposeContractInputState's base/top
// selection. Historical intermediate layers remain custody, not contributors.
func EffectiveContractInputSlots(witnesses []ContractInputWitness) map[ContractInputPhysicalSlot]bool {
	type pair struct{ base, top *ContractInputWitness }
	repos := map[string]pair{}
	for _, w := range witnesses {
		if !w.Found {
			continue
		}
		row := repos[w.State.RepoPrefix]
		copy := w
		if w.GenerationID == 0 {
			row.base = &copy
		} else {
			row.top = &copy
		}
		repos[w.State.RepoPrefix] = row
	}
	out := map[ContractInputPhysicalSlot]bool{}
	for _, row := range repos {
		if row.base != nil {
			out[ContractWitnessSlot(*row.base)] = true
		}
		if row.top != nil {
			out[ContractWitnessSlot(*row.top)] = true
		}
	}
	return out
}

// ContractWitnessesWithPrevious changes exactly one authorized effective
// physical slot. Unlike ComposePrevious it never rolls back other prior riders.
func ContractWitnessesWithPrevious(witnesses []ContractInputWitness, slot ContractInputPhysicalSlot) ([]ContractInputWitness, error) {
	if !EffectiveContractInputSlots(witnesses)[slot] {
		return nil, ErrContractInputVector
	}
	out := slices.Clone(witnesses)
	found := false
	var previousVersion, previousFingerprint string
	for i, w := range out {
		if ContractWitnessSlot(w) != slot {
			continue
		}
		if !w.Found || w.State.PreviousInputVersion == "" || w.State.PreviousInputFingerprint == "" ||
			(w.State.PreviousInputVersion == w.State.InputVersion && w.State.PreviousInputFingerprint == w.State.InputFingerprint) {
			return nil, ErrContractInputVector
		}
		if found && (previousVersion != w.State.PreviousInputVersion || previousFingerprint != w.State.PreviousInputFingerprint) {
			return nil, ErrContractInputVector
		}
		found = true
		previousVersion, previousFingerprint = w.State.PreviousInputVersion, w.State.PreviousInputFingerprint
		out[i].State.InputVersion, out[i].State.InputFingerprint = previousVersion, previousFingerprint
		out[i].State.Accepted = true
	}
	if !found {
		return nil, ErrContractInputVector
	}
	return out, nil
}

func ComposeContractInputStateWithPrevious(repo, checkout string, witnesses []ContractInputWitness, slot ContractInputPhysicalSlot) (ContractInputState, error) {
	previous, err := ContractWitnessesWithPrevious(witnesses, slot)
	if err != nil {
		return ContractInputState{}, err
	}
	return ComposeContractInputState(repo, checkout, previous)
}

// ValidateSingleMovedContractWitnesses requires the exact physical ancestry.
// Folds/generation substitution and all other namespace/found differences
// conservatively refuse this fallback; ordinary current composition is unchanged.
func ValidateSingleMovedContractWitnesses(current, historical []ContractInputWitness, slot ContractInputPhysicalSlot) error {
	a, err := CanonicalContractWitnesses(current)
	if err != nil {
		return err
	}
	b, err := CanonicalContractWitnesses(historical)
	if err != nil {
		return err
	}
	if len(a) != len(b) || !EffectiveContractInputSlots(current)[slot] {
		return ErrContractInputVector
	}
	moved := 0
	for i, w := range a {
		old := b[i]
		if ContractWitnessSlot(w) != ContractWitnessSlot(old) || w.Found != old.Found {
			return ErrContractInputVector
		}
		if w == old {
			continue
		}
		if ContractWitnessSlot(w) != slot || !w.Found || !old.State.Accepted ||
			(w.State.InputVersion == old.State.InputVersion && w.State.InputFingerprint == old.State.InputFingerprint) {
			return ErrContractInputVector
		}
		moved++
	}
	if moved != 1 {
		return ErrContractInputVector
	}
	return nil
}
