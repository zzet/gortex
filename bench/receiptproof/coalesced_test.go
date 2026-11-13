package receiptproof

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCoalescedFunctionalProofRejectsSupersessionAndUnknownAttempts(t *testing.T) {
	for _, mode := range []string{"qualified", "same_generation", "accepted_original", "missing_handoff", "wrong_winner", "wrong_retry", "partial", "bad_window", "A_failure", "P_failure", "bad_status_digest", "foreign_rpc", "unknown_B_phase", "empty_B_phase", "unknown_mismatch", "empty_mismatch"} {
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
			if p.Matches("commit-a", "edit-a", "rpc-2", 3, 4) != (mode == "qualified") {
				t.Fatalf("mode=%s proof misclassified", mode)
			}
		})
	}
}
