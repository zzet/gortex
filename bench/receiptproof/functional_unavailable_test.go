package receiptproof

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFunctionalUnavailableRequiresExplicitOwnedUnavailability(t *testing.T) {
	for _, mode := range []string{"direct43", "preexisting86", "direct53", "accepted_stamps", "physical_window", "foreign_rpc", "foreign_background", "handoff", "missing_absence", "read_assisted", "winner", "unknown_P", "unknown_B", "B_P_mismatch", "A_failure", "bad_window_identity"} {
		t.Run(mode, func(t *testing.T) {
			data, err := os.ReadFile("testdata/superseded.json")
			if err != nil {
				t.Fatal(err)
			}
			var p FunctionalUnavailable
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			p.PhysicalEvidenceAbsent, p.ReadAssistedAbsent = true, true
			switch mode {
			case "preexisting86":
				p.Status.Refusal = "preexisting_path_work"
				p.Status.BindingPredicate, p.Status.BackgroundStage, p.Status.AcceptedStage = "", "", ""
				p.AccountingError = strings.ReplaceAll(p.AccountingError, "serving_attempt_binding_mismatch", "preexisting_path_work")
			case "accepted_stamps":
				p.Status.AcceptedStage = "accepted_stamps_retained"
			case "physical_window":
				p.Status.PUnavailable = false
			case "foreign_rpc":
				p.Status.RPCID = "foreign"
			case "foreign_background":
				p.Status.BackgroundRequest = 99
			case "handoff":
				one := uint64(1)
				p.Status.NativeHandoffs = &one
			case "missing_absence":
				p.PhysicalEvidenceAbsent = false
			case "read_assisted":
				p.ReadAssistedAbsent = false
			case "winner":
				p.Status.CoalescedAbsent = false
			case "unknown_P":
				p.AccountingError += "; missing applicable P phase unknown"
			case "unknown_B":
				p.AccountingError += "; missing B phase unknown"
			case "B_P_mismatch":
				p.AccountingError += "; B/P phase mismatch: apply"
			case "A_failure":
				p.AccountingError += "; missing A phase disk_write"
			case "bad_window_identity":
				p.Status.Requested = 99
			}
			want := mode == "direct43" || mode == "preexisting86" || mode == "direct53"
			if p.Matches("commit-a", "edit-a", "rpc-2", 3, 3) != want {
				t.Fatalf("mode=%s misclassified", mode)
			}
		})
	}
}
