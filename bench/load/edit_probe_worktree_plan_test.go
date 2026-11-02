package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestEditProbeLinkedCheckoutPlansKeepPrimaryPinAndBothCounts(t *testing.T) {
	for _, linked := range []int{3, 7, 15} {
		t.Run(fmt.Sprintf("total-%d", linked+1), func(t *testing.T) {
			total := linked + 1
			args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", fmt.Sprint(total), "--worktrees", fmt.Sprint(linked), "--edit-count", "100", "--seed", "42"}
			options, err := parseRun(args)
			if err != nil {
				t.Fatalf("explicit linked-checkout plan refused: %v", err)
			}
			corpus := editProbeCorpus()
			report, err := newRunReport(options, []Corpus{corpus}, corpus.Commit, Host{}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(report.Workload.EditProbe)
			if err != nil {
				t.Fatal(err)
			}
			var plan struct {
				Routes []struct {
					ID     string `json:"id"`
					Repo   string `json:"repo"`
					Commit string `json:"commit"`
					File   string `json:"file"`
				} `json:"routes"`
				Steps []struct {
					Editor int    `json:"editor"`
					Route  string `json:"route"`
					DueNS  int64  `json:"due_ns"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(data, &plan); err != nil {
				t.Fatal(err)
			}
			if report.Workload.LinkedWorktrees != linked || len(plan.Routes) != total || len(plan.Steps) != 100 {
				t.Fatalf("primary/linked denominator lost: workload=%d routes=%d steps=%d", report.Workload.LinkedWorktrees, len(plan.Routes), len(plan.Steps))
			}
			ids := make([]string, total)
			for i, route := range plan.Routes {
				expected := corpus.Label
				if i > 0 {
					expected = fmt.Sprintf("linked-%d", i)
				}
				if route.ID != expected || route.Repo != corpus.Label || route.Commit != corpus.Commit || route.File != corpus.File {
					t.Fatalf("checkout%d lost logical primary pin: %+v", i, route)
				}
				ids[i] = route.ID
			}
			reference, err := makeEditProbeTrafficPlan(corpus, 42, 100, total)
			if err != nil {
				t.Fatal(err)
			}
			seen := make([]int, total)
			expectedCounts := make([]int, total)
			for i, step := range plan.Steps {
				if step.Editor < 0 || step.Editor >= total || step.Route != ids[step.Editor] || step.DueNS != reference.Steps[i].DueNS {
					t.Fatalf("checkout changed its fixed-editor offer: %+v", step)
				}
				seen[step.Editor]++
				expectedCounts[reference.Steps[i].Editor]++
			}
			for editor := range total {
				if seen[editor] != expectedCounts[editor] {
					t.Fatalf("editor%d offered denominator changed: actual%d expected%d", editor, seen[editor], expectedCounts[editor])
				}
			}
		})
	}
}

func TestEditProbeLinkedCheckoutPlansRefuseAmbiguousPrimaryOwnership(t *testing.T) {
	options := runOptions{Scenario: editProbeScenario, Seed: 42, EditCount: 100, Editors: 4, EditRepos: 1, Worktrees: 3}
	for _, name := range []string{"two_primaries", "too_few_editors", "readers", "mixed_six_repos", "linked_label_collision"} {
		t.Run(name, func(t *testing.T) {
			corpora, o := []Corpus{editProbeCorpus()}, options
			switch name {
			case "two_primaries":
				corpora = append(corpora, corpora[0])
			case "too_few_editors":
				o.Editors = 3
			case "readers":
				o.Agents = 1
			case "mixed_six_repos":
				o.EditRepos = 6
			case "linked_label_collision":
				corpora[0].Label = "linked-1"
			}
			if _, err := planEditProbeRoutes(corpora, o); err == nil {
				t.Fatal("ambiguous checkout ownership accepted")
			}
		})
	}
}
