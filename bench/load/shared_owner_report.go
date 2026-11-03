package main

import (
	"bytes"
	"encoding/json"
	"errors"
)

type editProbeSharedReference struct {
	DefinitionKey string                `json:"definition_key"`
	Member        editProbeSharedMember `json:"member"`
	Membership    json.RawMessage       `json:"same_status_membership"`
}

type editProbeSharedOwnerDefinition struct {
	State                      string           `json:"state"`
	Definition                 json.RawMessage  `json:"definition"`
	FirstConflictingDefinition json.RawMessage  `json:"first_conflicting_definition,omitempty"`
	Execution                  *editProbeWindow `json:"execution"`
	Background                 *editProbeWindow `json:"background"`
	Reasons                    []string         `json:"reasons"`
}

// Definitions are deduplicated report data. Every record still needs its own
// fresh, same-status membership proof; this map is never a liveness authority.
type editProbeSharedOwnerReport struct {
	Definitions          map[string]*editProbeSharedOwnerDefinition `json:"definitions"`
	DistinctOwners       int                                        `json:"distinct_observed_owners"`
	CompleteOwnerWindows int                                        `json:"complete_owner_windows"`
	Execution            Distribution                               `json:"execution_wall_ms"`
	Background           Distribution                               `json:"background_wall_ms"`
}

func (r *editProbeSharedOwnerReport) observe(proof editProbeSharedProof, proofErr error) (*editProbeSharedReference, error) {
	if proof.ownerKey == "" || len(proof.definition) == 0 {
		if proofErr == nil {
			proofErr = errors.New("shared definition has no independently bound owner")
		}
		return nil, proofErr
	}
	if proofErr == nil && (proof.execution == nil || proof.background == nil || len(proof.membership) == 0 || !proof.execution.WithinFivePercent || !proof.background.WithinFivePercent) {
		proofErr = errors.New("this reply has no complete fresh owner-window witness")
	}
	if r.Definitions == nil {
		r.Definitions = make(map[string]*editProbeSharedOwnerDefinition)
	}
	entry := r.Definitions[proof.ownerKey]
	if entry != nil && !bytes.Equal(entry.Definition, proof.definition) {
		entry.State = "contradictory_definition"
		if len(entry.FirstConflictingDefinition) == 0 {
			entry.FirstConflictingDefinition = bytes.Clone(proof.definition)
		}
		entry.Execution, entry.Background = nil, nil
		entry.Reasons = []string{"a frozen owner key returned contradictory definitions; original raw replies remain in each edit's evidence"}
		return nil, errors.New("shared frozen definition changed under the same owner key")
	}
	if entry != nil && entry.State == "contradictory_definition" {
		return nil, errors.New("shared owner already has contradictory frozen definitions")
	}
	if entry == nil {
		entry = &editProbeSharedOwnerDefinition{State: "unavailable", Definition: bytes.Clone(proof.definition)}
		if proof.execution != nil {
			value := *proof.execution
			entry.Execution = &value
		}
		if proof.background != nil {
			value := *proof.background
			entry.Background = &value
		}
		if proofErr == nil && entry.Execution != nil && entry.Background != nil {
			entry.State = "observed_owner_windows"
		} else if proofErr != nil {
			entry.Reasons = []string{proofErr.Error()}
		} else {
			entry.Reasons = []string{"complete shared owner E/B windows unavailable"}
		}
		r.Definitions[proof.ownerKey] = entry
	}
	if proofErr != nil {
		return nil, proofErr
	}
	if entry.State != "observed_owner_windows" || len(proof.membership) == 0 {
		return nil, errors.New("shared reference has no complete current owner windows/witness")
	}
	return &editProbeSharedReference{DefinitionKey: proof.ownerKey, Member: proof.member, Membership: bytes.Clone(proof.membership)}, nil
}

// definitionError invalidates previously validated references after a later
// contradiction. It cannot validate a new reply's process or membership.
func (r *editProbeSharedOwnerReport) definitionError(ref *editProbeSharedReference) error {
	if r == nil || ref == nil || ref.DefinitionKey == "" || len(ref.Membership) == 0 {
		return errors.New("shared reference/definition unavailable")
	}
	entry := r.Definitions[ref.DefinitionKey]
	if entry == nil || entry.State != "observed_owner_windows" || entry.Execution == nil || entry.Background == nil {
		return errors.New("shared reference has no noncontradictory owner windows")
	}
	return nil
}

func (r *editProbeSharedOwnerReport) summarize() {
	if r == nil {
		return
	}
	r.DistinctOwners, r.CompleteOwnerWindows = len(r.Definitions), 0
	var execution, background []float64
	for _, entry := range r.Definitions {
		if entry.State == "contradictory_definition" {
			continue
		}
		if entry.Execution != nil {
			execution = append(execution, float64(entry.Execution.WallNS)/1e6)
		}
		if entry.Background != nil {
			background = append(background, float64(entry.Background.WallNS)/1e6)
		}
		if entry.State == "observed_owner_windows" {
			r.CompleteOwnerWindows++
		}
	}
	// B already contains its single E reference. They have separate owner-level
	// distributions and are never added to each other, to A, or across members.
	r.Execution, r.Background = distribution(execution, "ms"), distribution(background, "ms")
}
