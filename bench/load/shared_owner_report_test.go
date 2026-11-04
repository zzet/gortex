package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// Report-data controls only: the proof parser and owned-process tests supply
// their separate liveness/ownership evidence before any runtime credit.
func sharedReportProofFixture() editProbeSharedProof {
	return editProbeSharedProof{
		ownerKey: "report-owner-one", definition: json.RawMessage(`{"test_definition":1}`), membership: json.RawMessage(`{"test_same_status":true}`),
		member:     editProbeSharedMember{Receipt: "physical-a", Ticket: 1, PhysicalSequence: 7},
		execution:  &editProbeWindow{WallNS: 1_000_000, SumNS: 1_000_000, WithinFivePercent: true},
		background: &editProbeWindow{WallNS: 5_000_000, SumNS: 5_000_000, WithinFivePercent: true},
	}
}

func TestSharedReportCountsOneOwnerAndNeverAddsNestedWindows(t *testing.T) {
	var report editProbeSharedOwnerReport
	first := sharedReportProofFixture()
	a, err := report.observe(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := sharedReportProofFixture()
	second.member = editProbeSharedMember{Receipt: "physical-b", Ticket: 2, PhysicalSequence: 8}
	b, err := report.observe(second, nil)
	if err != nil || a.DefinitionKey != b.DefinitionKey || a.Member == b.Member {
		t.Fatalf("two original members did not keep distinct references to one owner: %+v %+v %v", a, b, err)
	}
	report.summarize()
	if report.DistinctOwners != 1 || report.CompleteOwnerWindows != 1 || report.Execution.Count != 1 || report.Background.Count != 1 || report.Execution.P95.Value == nil || *report.Execution.P95.Value != 1 || report.Background.P95.Value == nil || *report.Background.P95.Value != 5 {
		t.Fatalf("shared E/B was multiplied across members or added into another window: %+v", report)
	}
	// No alias back to parser scratch, per-reply input, or its scalar pointers.
	first.definition[0] = '!'
	first.membership[0] = '!'
	first.execution.WallNS = 99
	if report.Definitions[a.DefinitionKey].Definition[0] != '{' || a.Membership[0] != '{' || report.Definitions[a.DefinitionKey].Execution.WallNS != 1_000_000 {
		t.Fatal("report normalization retained mutable input aliases")
	}
}

func TestSharedReportContradictoryDefinitionInvalidatesEveryOldReference(t *testing.T) {
	var report editProbeSharedOwnerReport
	proof := sharedReportProofFixture()
	ref, err := report.observe(proof, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := sharedReportProofFixture()
	changed.definition = json.RawMessage(`{"test_definition":2}`)
	if next, err := report.observe(changed, nil); err == nil || next != nil {
		t.Fatal("a frozen owner silently acquired a second definition")
	}
	if report.definitionError(ref) == nil {
		t.Fatal("an earlier reference kept window credit after owner contradiction")
	}
	entry := report.Definitions[ref.DefinitionKey]
	if entry.State != "contradictory_definition" || entry.Execution != nil || entry.Background != nil || !bytes.Equal(entry.Definition, proof.definition) || !bytes.Equal(entry.FirstConflictingDefinition, changed.definition) {
		t.Fatalf("contradictory source data was overwritten or retained numeric credit: %+v", entry)
	}
	if next, err := report.observe(proof, nil); err == nil || next != nil {
		t.Fatal("repeating the first definition revived a contradicted owner")
	}
	report.summarize()
	if report.DistinctOwners != 1 || report.CompleteOwnerWindows != 0 || report.Execution.Count != 0 || report.Background.Count != 0 || report.Execution.P95.Value != nil || report.Background.P95.Value != nil {
		t.Fatalf("a contradiction became a successful zero/missing population: %+v", report)
	}
}

func TestSharedReportCannotReviveMissingWitnessOrUpgradeFrozenPartial(t *testing.T) {
	var report editProbeSharedOwnerReport
	proof := sharedReportProofFixture()
	if _, err := report.observe(proof, nil); err != nil {
		t.Fatal(err)
	}
	missing := sharedReportProofFixture()
	missing.membership = nil
	if ref, err := report.observe(missing, nil); err == nil || ref != nil {
		t.Fatal("dictionary presence substituted for this reply's missing witness")
	}
	var partial editProbeSharedOwnerReport
	proof = sharedReportProofFixture()
	proof.background = nil
	if ref, err := partial.observe(proof, errors.New("actual background unavailable")); err == nil || ref != nil {
		t.Fatal("an E-only definition passed full owner accounting")
	}
	partial.summarize()
	if partial.Execution.Count != 1 || partial.Background.Count != 0 || partial.Background.P95.Value != nil || partial.CompleteOwnerWindows != 0 {
		t.Fatalf("a missing B was inferred or measured as zero: %+v", partial)
	}
	later := sharedReportProofFixture()
	later.definition = json.RawMessage(`{"test_definition":1,"new_background":true}`)
	if ref, err := partial.observe(later, nil); err == nil || ref != nil {
		t.Fatal("a frozen E-only definition was later upgraded under the same key")
	}
	var unbound editProbeSharedOwnerReport
	if _, err := unbound.observe(editProbeSharedProof{}, nil); err == nil || len(unbound.Definitions) != 0 {
		t.Fatal("a caller-chosen dictionary key created an observed owner")
	}
}
