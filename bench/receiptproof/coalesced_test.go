package receiptproof

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCoalescedFunctionalProofRejectsSupersessionAndUnknownAttempts(t *testing.T) {
	for _, mode := range []string{"qualified", "preexisting_separate_winner", "preexisting_direct_original", "preexisting_accepted_stamps", "preexisting_physical_window", "same_generation", "accepted_original", "missing_handoff", "wrong_winner", "wrong_retry", "partial", "bad_window", "A_failure", "P_failure", "bad_status_digest", "foreign_rpc", "unknown_B_phase", "empty_B_phase", "unknown_mismatch", "empty_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			data, err := os.ReadFile("testdata/coalesced.json")
			if err != nil {
				t.Fatal(err)
			}
			var p Coalesced
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "preexisting_separate_winner", "preexisting_direct_original", "preexisting_accepted_stamps", "preexisting_physical_window":
				p.Status.Refusal = "preexisting_path_work"
				p.Status.BindingPredicate, p.Status.BackgroundStage, p.Status.AcceptedStage = "", "", ""
				p.AccountingError = "A/P/B accounting unavailable: requested B observed unavailable: preexisting_path_work; B: missing duration"
				if mode == "preexisting_accepted_stamps" {
					p.Status.AcceptedStage = "accepted_stamps_retained"
				}
				if mode == "preexisting_physical_window" {
					p.Status.AcceptedStage = "physical_window_retained"
				}
				if mode == "preexisting_direct_original" {
					p.Status.PUnavailable = false
					p.Status.Applied = 3
				}
			case "same_generation":
				p.Status.Applied = 3
			case "accepted_original":
				p.Status.PUnavailable = false
			case "missing_handoff":
				p.Status.NativeHandoffs = nil
			case "wrong_winner":
				p.Winner.Ticket = 9
			case "wrong_retry":
				p.Winner.AttemptRetry = 1
			case "partial":
				p.Winner.PartitionComplete = false
			case "bad_window":
				p.ServingPhysical.ResidualNS = 1
			case "A_failure":
				p.AccountingError += "; missing A phase disk_write"
			case "P_failure":
				p.AccountingError += "; missing applicable P phase apply"
			case "bad_status_digest":
				p.Status.SHA256 = "unknown"
			case "unknown_B_phase":
				p.AccountingError += "; missing B phase unknown"
			case "empty_B_phase":
				p.AccountingError += "; missing B phase "
			case "unknown_mismatch":
				p.AccountingError += "; B/P phase mismatch: unknown"
			case "empty_mismatch":
				p.AccountingError += "; B/P phase mismatch: "
			case "foreign_rpc":
				p.Status.RPCID = "foreign"
			}
			if p.Matches("commit-a", "edit-a", "rpc-2", 3, 4) != (mode == "qualified" || mode == "preexisting_separate_winner") {
				t.Fatalf("mode=%s proof misclassified", mode)
			}
		})
	}
}

func TestNativeNoOpEveryDeepPhaseMustBeExplicitlyUnexecuted(t *testing.T) {
	for _, phase := range noOpDeepPhases {
		for _, bad := range []string{"executed", "fabricated_zero", "missing_state"} {
			t.Run(phase+"/"+bad, func(t *testing.T) {
				n := &NativeNoOp{ExecutionDisposition: "native_mtime_duplicate", SumState: "partial", PhasesNS: map[string]*int64{}, PhaseStates: map[string]string{}}
				for _, name := range noOpDeepPhases {
					n.PhasesNS[name] = nil
					n.PhaseStates[name] = "not_applicable"
				}
				for _, name := range noOpControlPhases {
					v := int64(1000000)
					n.PhasesNS[name] = &v
					n.PhaseStates[name] = "measured"
				}
				w := Window{WallNS: 5000000, SumNS: 5000000, WithinFivePercent: true}
				if !n.complete(w) {
					t.Fatal("valid no-op partition rejected")
				}
				switch bad {
				case "executed":
					v := int64(1)
					n.PhasesNS[phase] = &v
					n.PhaseStates[phase] = "measured"
				case "fabricated_zero":
					v := int64(0)
					n.PhasesNS[phase] = &v
				case "missing_state":
					delete(n.PhaseStates, phase)
				}
				if n.complete(w) {
					t.Fatal("executed or unknown deep phase qualified as no-op")
				}
			})
		}
	}
}
