package main

import (
	"strings"
	"testing"
)

func TestSearchReadbackProfileRequiresAConsistentLocalACKPolicy(t *testing.T) {
	corpus := editProbeRenameCorpus(t, "go", 1000)
	corpus.EditTool = "edit.file"
	hashes := map[string]string{}
	for _, mode := range []string{"", "local", "full"} {
		t.Run("selected_"+mode, func(t *testing.T) {
			args := append(searchAcceptanceArgs(4, 1000), "--search-readback-acceptance", "--edit-mean=1s")
			if mode != "" {
				args = append(args, "--edit-ack", mode)
			}
			options, err := parseRun(args)
			if mode == "full" {
				if err == nil {
					t.Fatal("local-only search profile accepted conflicting full ACK configuration")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			report, err := newRunReport(options, []Corpus{corpus}, strings.Repeat("a", 40), Host{}, "owned-local-profile")
			if err != nil {
				t.Fatal(err)
			}
			if got := copyJSON(report.Workload)["edit_ack_mode"]; got != "local" {
				t.Errorf("normalized workload does not match actual local bootstrap policy: %v", got)
			}
			hashes[mode] = report.RequestPlan
		})
	}
	if hashes[""] != hashes["local"] {
		t.Fatal("implicit and explicit local search profiles have different normalized workload identities")
	}
}
