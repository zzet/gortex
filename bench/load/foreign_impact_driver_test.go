package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestForeignImpactEditPlanKeepsAcceptedOneEditorTrafficAndCoversWindow(t *testing.T) {
	c := editProbeCorpus()
	original, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := foreignImpactEditPlan(c, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, original) {
		t.Fatal("foreign window changed the fixed editor's seeded requests/arrivals")
	}
	if len(plan.Steps) != 100 || plan.MeanNS != int64(15*time.Second) || plan.Steps[99].DueNS-plan.Steps[0].DueNS < int64(10*time.Minute) {
		t.Fatalf("fixed edit traffic cannot support required active window: %+v", plan)
	}
	six, err := makeEditProbeTrafficPlan(c, 42, 100, 6)
	if err != nil {
		t.Fatal(err)
	}
	if six.Steps[99].DueNS-six.Steps[0].DueNS >= int64(foreignImpactDuration) {
		t.Fatal("control no longer demonstrates why the short six-editor stream cannot be padded with idle probes")
	}
}

func TestForeignImpactQueryTargetsRequireEachForeignCheckoutOwnSession(t *testing.T) {
	root := t.TempDir()
	var repos []privateRepo
	var clients []*socketClient
	for _, name := range []string{"one", "two", "three", "four", "five", "six"} {
		repo := privateRepo{Path: filepath.Join(root, name), Prefix: name, Corpus: Corpus{Label: name}}
		repos = append(repos, repo)
		clients = append(clients, &socketClient{cwd: repo.Path})
	}
	targets, err := foreignImpactQueryTargets(repos, clients, 2)
	if err != nil || len(targets) != 5 {
		t.Fatalf("six-root target selection failed: %v %+v", err, targets)
	}
	for _, target := range targets {
		if target.Repo.Corpus.Label == "three" || target.Client.cwd != target.Repo.Path {
			t.Fatal("edited/foreign checkout was substituted")
		}
	}
	if _, err := foreignImpactQueryTargets(repos, append(clients, clients[0]), 2); err == nil {
		t.Fatal("ambiguous duplicate query session accepted")
	}
	if _, err := foreignImpactQueryTargets(repos, clients[:5], 2); err == nil {
		t.Fatal("missing target session fell back to primary")
	}
}

func TestForeignImpactCommandRejectsChangedTrafficAndBudgets(t *testing.T) {
	args := []string{"--binary", "/owned/product", "--manifest", "/owned/manifest", "--scratch", "/owned/runs", "--scenario", "many", "--agents", "6", "--edit-agents", "1", "--worktrees", "0", "--seed", "42", "--duration", "10m"}
	if _, _, err := parseForeignImpact(args); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]string{{"--seed", "43"}, {"--edit-count", "1"}, {"--duration", "9m"}, {"--agents", "7"}, {"--deadline", "119s"}, {"--edited-repo", "6"}} {
		if _, _, err := parseForeignImpact(append(append([]string(nil), args...), changed...)); err == nil {
			t.Fatalf("accepted changed foreign cell %v", changed)
		}
	}
}

func TestForeignImpactEvidenceLimitRetainsPriorRowsAndStopsAtBoundary(t *testing.T) {
	row := foreignImpactSample{TargetRepo: "A", EditedRepo: "B", RPCID: 9}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	writer := foreignImpactEvidenceWriter{w: &dst, limit: int64(len(data) + 1)}
	if err := writer.emit(row); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), dst.Bytes()...)
	if err := writer.emit(row); err == nil {
		t.Fatal("exhausted raw evidence storage silently accepted another offer")
	}
	if !bytes.Equal(before, dst.Bytes()) || writer.bytes != int64(len(before)) {
		t.Fatal("quota failure changed previously retained evidence")
	}
}

func TestForeignImpactRunReportDescribesActualRolesAndPreservesEditPlan(t *testing.T) {
	corpora := editProbeRoutingCorpora()
	o := runOptions{Scenario: "many", Agents: 6, Editors: 1, Seed: 42, Duration: foreignImpactDuration, Deadline: 120 * time.Second, WarmTimeout: 30 * time.Minute, StopTimeout: 30 * time.Second, Profile: "agent", EditCount: 100}
	report, err := newForeignImpactRunReport(o, corpora, 2, corpora[0].Commit, Host{}, "owned-report")
	if err != nil {
		t.Fatal(err)
	}
	if report.Workload.Scenario != "foreign-impact" || report.Workload.Agents != 5 || report.Workload.EditAgents != 1 || report.Workload.SessionModel != "one_B_editor_and_five_independent_rotating_foreign_queries" {
		t.Fatalf("foreign workload mislabeled as ordinary read-only many cell: %+v", report.Workload)
	}
	if report.Workload.EditProbe == nil || report.Workload.EditProbe.Count != 100 || max(1, report.Workload.EditProbe.Editors) != 1 || report.Workload.EditProbe.Repo != corpora[2].Label || report.Workload.DurationSec != 600 {
		t.Fatal("active foreign window or full B edit schedule lost")
	}
	plan, err := foreignImpactEditPlan(corpora[2], 42)
	if err != nil || !reflect.DeepEqual(*report.Workload.EditProbe, plan) {
		t.Fatal("outer report differs from actual seeded B editor plan")
	}
	if len(report.Workload.Operations) < 5 || report.Workload.Operations[0].name() != "edit.file" {
		t.Fatal("outer report omitted offered edits")
	}
	for i, op := range report.Workload.Operations[len(report.Workload.Operations)-5:] {
		foreign := i
		if foreign >= 2 {
			foreign++
		}
		target := op.Arguments["target"].(map[string]any)["symbol"]
		if op.name() != "change.impact" || target != corpora[foreign].Label+"/"+corpora[foreign].Symbol {
			t.Fatal("foreign target route lost or replaced with B")
		}
	}
	before := report.RequestPlan
	for i := range corpora {
		corpora[i].Source = filepath.Join("/other-owned-copy", corpora[i].Label)
	}
	moved, err := newForeignImpactRunReport(o, corpora, 2, corpora[0].Commit, Host{}, "other-report")
	if err != nil || moved.RequestPlan != before {
		t.Fatal("private clone paths changed the actual foreign workload hash")
	}
	differentB, err := newForeignImpactRunReport(o, corpora, 1, corpora[0].Commit, Host{}, "other-report")
	if err != nil || differentB.RequestPlan == before {
		t.Fatal("foreign workload hash omitted edited-repository routing")
	}
}
