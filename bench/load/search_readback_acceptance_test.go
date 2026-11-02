package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func searchAcceptanceArgs(editors, count int) []string {
	return []string{"--binary", "/owned/gortex", "--manifest", "/owned/manifest.json", "--scratch", "/owned/scratch", "--scenario", editProbeScenario, "--agents", "0", "--worktrees", "0", "--seed", "42", "--edit-agents", fmt.Sprint(editors), "--edit-count", fmt.Sprint(count)}
}

func TestSearchReadbackAcceptanceOptsIntoThousandProbesAtEveryPopulation(t *testing.T) {
	for _, editors := range []int{1, 4, 8} {
		t.Run(fmt.Sprint(editors), func(t *testing.T) {
			args := append(searchAcceptanceArgs(editors, 1000), "--search-readback-acceptance")
			o, err := parseRun(args)
			if err != nil {
				t.Fatalf("explicit search acceptance profile rejected: %v", err)
			}
			c := editProbeRenameCorpus(t, "go", 1000)
			c.EditTool = "edit.file"
			plan, err := planEditProbe(c, o)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Count != 1000 || len(plan.Steps) != 1000 {
				t.Fatalf("offered search population lost: count=%d steps=%d", plan.Count, len(plan.Steps))
			}
			seen := make(map[int]int)
			for _, step := range plan.Steps {
				seen[step.Editor]++
			}
			if len(seen) != editors {
				t.Fatalf("missing actual editor assignments: %v", seen)
			}
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["search_readback_acceptance"] != true {
				t.Fatal("acceptance plan does not distinguish immediate search witnesses from polling")
			}
		})
	}
}

func TestSearchReadbackAcceptanceDoesNotBroadenOrdinaryEditPlans(t *testing.T) {
	o, err := parseRun(searchAcceptanceArgs(4, 100))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planEditProbe(editProbeCorpus(), o)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Count != 100 || plan.MeanNS != int64(15*time.Second) {
		t.Fatalf("ordinary offered plan changed: %+v", plan)
	}
	if _, err := parseRun(searchAcceptanceArgs(4, 1000)); err == nil {
		t.Fatal("ordinary profile silently accepted the acceptance-only population")
	}
}

func TestSearchReadbackAcceptanceRefusesUnselectedPopulations(t *testing.T) {
	for _, cell := range [][2]int{{2, 1000}, {16, 1000}, {4, 100}, {4, 1}} {
		if _, err := parseRun(append(searchAcceptanceArgs(cell[0], cell[1]), "--search-readback-acceptance")); err == nil {
			t.Fatalf("unselected search acceptance population admitted: %v", cell)
		}
	}
}

func TestSearchReadbackAcceptanceNeverCreditsLaterPollingOrMissingWitnesses(t *testing.T) {
	fixture := func() (editProbePlan, []editProbeRecord) { return completeSearchReadbackCensus(t) }
	plan, records := fixture()
	if got := summarizeSearchReadbacks(plan, records); !got.Pass || got.Witnesses["search_visible"].Correct != 1000 || len(got.EditsByEditor) != 4 {
		t.Fatalf("complete immediate census refused: %+v", got)
	}
	for _, fault := range []string{"later_poll", "missing", "refusal", "error", "line", "duplicate", "ack", "editor", "censored"} {
		t.Run(fault, func(t *testing.T) {
			p, rows := fixture()
			switch fault {
			case "later_poll":
				rows[0].Readbacks[0].Correct = false
				rows[0].FirstCorrect["search_visible"] = measured(1, "ms")
			case "missing":
				rows[0].Readbacks = rows[0].Readbacks[1:]
			case "refusal":
				rows[0].Readbacks[0].Refusal = "view pending"
			case "error":
				rows[0].Readbacks[0].Error = "decode failed"
			case "line":
				rows[0].Readbacks[0].ExpectedLine++
			case "duplicate":
				rows[0].Readbacks = append(rows[0].Readbacks, rows[0].Readbacks[0])
			case "ack":
				rows[0].Ack = unavailable("ms", "missing ACK")
			case "editor":
				rows[0].Editor = 99
			case "censored":
				rows = rows[:999]
			}
			if got := summarizeSearchReadbacks(p, rows); got.Pass {
				t.Fatalf("invalid immediate population accepted: %s %+v", fault, got)
			}
		})
	}
}

func completeSearchReadbackCensus(t *testing.T) (editProbePlan, []editProbeRecord) {
	t.Helper()
	o, err := parseRun(append(searchAcceptanceArgs(4, 1000), "--search-readback-acceptance"))
	if err != nil {
		t.Fatal(err)
	}
	c := editProbeRenameCorpus(t, "go", 1000)
	c.EditTool = "edit.file"
	plan, err := planEditProbe(c, o)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]editProbeRecord, len(plan.Steps))
	for i, step := range plan.Steps {
		records[i] = newEditProbeRecord(c, step)
		records[i].Ack = measured(1, "ms")
		for _, kind := range []string{"search_visible", "text", "old_search_absent", "old_text_absent"} {
			records[i].Readbacks = append(records[i].Readbacks, editProbeReadback{Kind: kind, Correct: true, Latency: measured(1, "ms"), ExpectedLine: step.ExpectedSearchLine, ExpectedPath: "fixture/" + c.File, ExpectedID: "fixture/" + c.File + "::" + step.Marker})
		}
		addSearchACKFixture(t, &records[i], searchACKFixture())
	}
	return plan, records
}
