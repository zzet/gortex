package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

var ErrContractInputVector = errors.New("graph: invalid or unavailable contract input vector")

// Witnesses capture the actual physical source actor, including absence. The
// caller selects necessary ancestry; only inherited views include generation0.
type ContractInputWitness struct {
	GenerationID int64
	State        ContractInputState
	Found        bool
}

// ComposeContractInputState uses a cumulative top-positive summary plus live0
// only when actually inherited. Witnesses are in selected bottom-to-top order.
// Every positive state summarizes its prior top-positive state and ordered
// accepted relevant deltas, excludes live0, and carries unchanged on no delta.
// Complete physical witnesses remain separate for publication CAS.
func ComposeContractInputState(repo, checkout string, witnesses []ContractInputWitness) (ContractInputState, error) {
	return composeContractInputState(repo, checkout, witnesses, false)
}

// ComposePreviousContractInputState only proposes a candidate historical key;
// callers must separately prove requested debt disjointness and key availability.
func ComposePreviousContractInputState(repo, checkout string, witnesses []ContractInputWitness) (ContractInputState, error) {
	return composeContractInputState(repo, checkout, witnesses, true)
}

func composeContractInputState(repo, checkout string, witnesses []ContractInputWitness, previous bool) (ContractInputState, error) {
	state := ContractInputState{RepoPrefix: repo, CheckoutID: checkout, Accepted: true}
	if len(witnesses) == 0 || len(witnesses) > 1024 {
		return state, ErrContractInputVector
	}
	type sources struct{ base, positive *ContractInputState }
	selected := make(map[string]sources)
	for _, w := range witnesses {
		if w.GenerationID < 0 {
			return state, ErrContractInputVector
		}
		if !w.Found {
			if w.GenerationID == 0 {
				return state, ErrContractInputVector
			}
			continue
		}
		copyState := w.State
		row := selected[copyState.RepoPrefix]
		if w.GenerationID == 0 {
			row.base = &copyState
		} else {
			row.positive = &copyState
		}
		selected[copyState.RepoPrefix] = row
	}
	if _, ok := selected[repo]; !ok {
		return state, ErrContractInputVector
	}
	type identity struct{ Repo, Version, Fingerprint string }
	repos := make([]string, 0, len(selected))
	for r := range selected {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var identities []identity
	for _, r := range repos {
		row := selected[r]
		var last *identity
		for _, input := range []*ContractInputState{row.base, row.positive} {
			if input == nil {
				continue
			}
			version, fingerprint := input.InputVersion, input.InputFingerprint
			if previous && input.PreviousInputVersion != "" && input.PreviousInputFingerprint != "" {
				version, fingerprint = input.PreviousInputVersion, input.PreviousInputFingerprint
			} else if previous && !input.Accepted {
				return state, ErrContractInputVector
			}
			if version == "" || fingerprint == "" {
				return state, ErrContractInputVector
			}
			if !previous && !input.Accepted {
				state.Accepted = false
			}
			key := identity{r, version, fingerprint}
			if last != nil && *last == key {
				continue
			}
			identities = append(identities, key)
			last = &identities[len(identities)-1]
		}
	}
	if len(identities) == 0 {
		return state, ErrContractInputVector
	}
	if len(identities) == 1 {
		state.InputVersion, state.InputFingerprint = identities[0].Version, identities[0].Fingerprint
		return state, nil
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return state, err
	}
	sum := sha256.Sum256(encoded)
	state.InputVersion = "contract-selected-input-v1"
	state.InputFingerprint = hex.EncodeToString(sum[:])
	return state, nil
}
