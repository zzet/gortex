package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// Report serialization only: these named model definitions do not establish
// Server/process liveness or turn a saved report into a new reply's authority.
func TestSharedReportRoundTripPreservesOwnersAndOfferedDenominator(t *testing.T) {
	var owners editProbeSharedOwnerReport
	proof := sharedReportProofFixture()
	ref, err := owners.observe(proof, nil)
	if err != nil {
		t.Fatal(err)
	}
	partial := sharedReportProofFixture()
	partial.ownerKey, partial.definition = "partial-report-owner", json.RawMessage(`{"test_partial_definition":1}`)
	partial.background = nil
	if _, err := owners.observe(partial, errors.New("actual background unavailable")); err == nil {
		t.Fatal("partial report fixture acquired full owner accounting")
	}
	owners.summarize()
	report := sampleReport(t)
	report.Complete, report.Errors = false, []string{"serialization-only mixed outcome fixture"}
	report.Phases[0].EditProbe = &editProbeReport{
		Role: "candidate", AccountingRequired: true, SharedOwners: &owners,
		Records: []editProbeRecord{
			{Ordinal: 4, Outcome: "complete", Accounting: editProbeAccounting{State: "observed_shared_owner_windows_checks_pass", Shared: ref}},
			{Ordinal: 5, Outcome: "phase_censored", Accounting: editProbeAccounting{State: "not_observed"}},
			{Ordinal: 6, Outcome: "diagnostic_failure", Accounting: editProbeAccounting{State: "terminal_unavailable_shared_owner", Reasons: []string{"actual background unavailable"}}},
		},
	}
	path := filepath.Join(t.TempDir(), "shared-report.json")
	if err := writeReport(path, report); err != nil {
		t.Fatal(err)
	}
	loaded, err := readReport(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Phases[0].EditProbe
	if got == nil || got.SharedOwners == nil || len(got.Records) != 3 || got.Records[0].Ordinal != 4 || got.Records[1].Ordinal != 5 || got.Records[2].Ordinal != 6 || got.Records[1].Outcome != "phase_censored" || got.Records[2].Outcome != "diagnostic_failure" {
		t.Fatal("serialized shared owner report removed or reclassified offered failures/censoring")
	}
	if got.SharedOwners.DistinctOwners != 2 || got.SharedOwners.CompleteOwnerWindows != 1 || got.SharedOwners.Execution.Count != 2 || got.SharedOwners.Background.Count != 1 || got.SharedOwners.Definitions[partial.ownerKey].Background != nil || len(got.SharedOwners.Definitions[partial.ownerKey].Reasons) == 0 {
		t.Fatal("serialized partial owner was lost, upgraded or multiplied across edits")
	}
	if got.Records[0].Accounting.Shared == nil || got.Records[0].Accounting.P != nil || got.Records[0].Accounting.B != nil || got.Records[2].Accounting.Shared != nil {
		t.Fatal("roundtrip manufactured per-file windows or revived a partial reference")
	}
	// A loaded dictionary still cannot settle a new status without the actual
	// owned child. Existing independent A fixture data cannot supply that bond.
	runtime := &editProbeRuntime{phase: &Phase{EditProbe: got}, sharedReport: *got.SharedOwners}
	status := editProbeAccountingFixture()
	status["shared_owner_accounting_v1"] = map[string]any{}
	accounting := runtime.accountingForStatus(t.Context(), status, "receipt-a", "edit.file")
	if accounting.State == "observed_shared_owner_windows_checks_pass" || accounting.Shared != nil || accounting.P != nil || accounting.B != nil || len(accounting.Reasons) == 0 {
		t.Fatal("saved report dictionary became fresh process/member authority")
	}
}
