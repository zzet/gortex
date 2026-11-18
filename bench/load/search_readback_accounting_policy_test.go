package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Exercise the real preparation path. Cancellation before session setup keeps
// every offered edit censored; no fake runtime override can hide its policy.
func TestSearchReadbackPreparationKeepsItsEvidencePolicySeparateFromEditGate(t *testing.T) {
	for _, cell := range []struct {
		name     string
		search   bool
		baseline bool
		gate     bool
		required bool
		role     string
		count    int
	}{
		{name: "search correctness candidate", search: true, role: "candidate", count: 1000},
		{name: "ordinary edit candidate", required: true, role: "candidate", count: 100},
		{name: "five-mode edit gate candidate", gate: true, required: true, role: "candidate", count: 100},
		{name: "ordinary baseline", baseline: true, role: "baseline", count: 100},
		{name: "search correctness baseline", search: true, baseline: true, role: "baseline", count: 1000},
	} {
		t.Run(cell.name, func(t *testing.T) {
			editors := 1
			if cell.gate {
				editors = 4
			}
			args := searchAcceptanceArgs(editors, cell.count)
			if cell.gate {
				args = append(args, "--edit-gate-modes")
			}
			if cell.search {
				args = append(args, "--search-readback-acceptance", "--edit-mean=1s")
			}
			if cell.baseline {
				args = append(args, "--edit-probe-role=baseline")
			}
			options, err := parseRun(args)
			if err != nil {
				t.Fatal(err)
			}
			corpus := editProbeRenameCorpus(t, "go", cell.count)
			corpus.EditTool = "edit.file"
			initial := corpus.EditInitialContent
			if cell.gate {
				corpus = editProbeCorpus()
				initial = "package p\nfunc Example() int { return 1 }\n" + corpus.EditAnchor + "\n"
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, corpus.File), []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			socketRoot, err := os.MkdirTemp(os.TempDir(), "sp-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(socketRoot); err != nil {
					t.Error(err)
				}
			})
			phase := &Phase{Metrics: make(map[string]Metric)}
			private := &privateRun{Root: root, SocketDir: socketRoot, Socket: filepath.Join(socketRoot, "absent.sock"), Repos: []privateRepo{{Path: root, Prefix: "fixture", Corpus: corpus}}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			run, clients, setupErr := prepareEditProbe(ctx, options, private, nil, make([]*socketClient, editors), phase)
			if setupErr == nil || run == nil || len(clients) != 0 || phase.EditProbe == nil {
				t.Fatalf("control did not reach actual canceled preparation: run=%v clients=%d report=%v err=%v", run != nil, len(clients), phase.EditProbe != nil, setupErr)
			}
			t.Cleanup(func() { _ = run.close() })
			if run.requireAccounting != cell.required || phase.EditProbe.AccountingRequired != cell.required || phase.EditProbe.Role != cell.role {
				t.Errorf("wrong named-profile policy: runtime_required=%v report_required=%v role=%q", run.requireAccounting, phase.EditProbe.AccountingRequired, phase.EditProbe.Role)
			}
			if err := run.finish(); err == nil {
				t.Fatal("unstarted edits lost their explicit failure")
			}
			if len(phase.EditProbe.Records) != cell.count {
				t.Fatalf("canceled preparation lost offered edits: %d", len(phase.EditProbe.Records))
			}
			for _, record := range phase.EditProbe.Records {
				if record.StartedNS != nil || record.Ack.Value != nil || record.Outcome != "phase_censored" {
					t.Fatalf("canceled setup fabricated an edit/result: %+v", record)
				}
				if record.Accounting.State != "not_observed" || record.Accounting.A != nil || record.Accounting.P != nil || record.Accounting.B != nil || record.PhasesMS["parse"].Value != nil {
					t.Fatalf("absent accounting became an observed window: %+v", record.Accounting)
				}
			}
		})
	}
}
