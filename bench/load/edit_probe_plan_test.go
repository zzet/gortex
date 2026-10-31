package main

import (
	"context"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func editProbeCorpus() Corpus {
	c := sampleCorpus()
	c.EditAnchor = "func Caller() int { return Example() }"
	c.EditTemplate = "\nfunc {{marker}}() int { return Example() }\n"
	return c
}

func TestEditProbePlanIsFiniteSeededAndSourceIndependent(t *testing.T) {
	c := editProbeCorpus()
	a, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Source = "/another/private/source"
	b, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || len(a.Steps) != 100 {
		t.Fatal("same pinned workload/seed changed offered plan")
	}
	hashes := map[string]bool{}
	last := int64(0)
	for i, step := range a.Steps {
		if step.Ordinal != i || step.DueNS <= last || step.ActivateNS != step.DueNS-int64(editProbePeriod) || hashes[step.RequestHash] {
			t.Fatalf("bad unique planned step%+v", step)
		}
		hashes[step.RequestHash] = true
		last = step.DueNS
	}
	changed, err := makeEditProbePlan(c, 43, 100)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a.Steps, changed.Steps) {
		t.Fatal("seed did not change schedule")
	}
	budget, err := a.budget(time.Minute, 2)
	if err != nil || budget != time.Duration(last)+2*time.Minute {
		t.Fatalf("budget%v %v", budget, err)
	}
	if _, err := a.budget(time.Duration(math.MaxInt64), 2); err == nil {
		t.Fatal("overflow budget accepted")
	}
}

func TestEditProbePlanRejectsUnselectedOrInvalidCells(t *testing.T) {
	for _, name := range []string{"count", "language", "missing_anchor", "no_callee", "missing_call", "no_body", "two_markers", "bad_go"} {
		t.Run(name, func(t *testing.T) {
			c := editProbeCorpus()
			count := 100
			switch name {
			case "count":
				count = 99
			case "language":
				c.File = "sample.rs"
			case "missing_anchor":
				c.EditAnchor = ""
			case "no_callee":
				c.Symbol = ""
			case "missing_call":
				c.EditTemplate = "func {{marker}}() int { return 1 }"
			case "no_body":
				c.EditTemplate = "func {{marker}}() int"
			case "two_markers":
				c.EditTemplate += "// {{marker}}"
			case "bad_go":
				c.EditTemplate = "func {{marker}}( {"
			}
			if _, err := makeEditProbePlan(c, 1, count); err == nil {
				t.Fatal("invalid cell accepted")
			}
		})
	}
}

func TestEditProbeAbsoluteArrivalsDoNotWaitForEarlierACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	firstEntered, secondEntered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	plan := editProbePlan{Steps: []editProbeStep{{Ordinal: 0}, {Ordinal: 1}}}
	done := make(chan error, 1)
	go func() {
		done <- runEditProbeArrivals(ctx, plan, time.Now(), func(context.Context, editProbeStep) {}, func(ctx context.Context, step editProbeStep) {
			if step.Ordinal == 0 {
				close(firstEntered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			} else {
				close(secondEntered)
			}
		}, waitContext)
	}()
	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal("blocked earlier ACK shifted offered later arrival")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The default cell (one editor, no readers, JSON, MCP sender) is the paired
// no-regression workload. Optional dimensions must leave its offered plan, plan
// hash and operation list byte-identical; changing a constant below needs an
// explicit compatibility decision, not a regenerated value.
func TestEditProbeDefaultPlanIdentityIsPinned(t *testing.T) {
	c := editProbeCorpus()
	hundred, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	one, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	toonCorpus := c
	toonCorpus.EditOutput = "toon"
	toonPlan, err := makeEditProbePlan(toonCorpus, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	o := runOptions{Scenario: editProbeScenario, Profile: "agent", Agents: 0, Editors: 1, Seed: 42, Duration: time.Minute, Deadline: 120 * time.Second, WarmTimeout: 30 * time.Minute, StopTimeout: 30 * time.Second, SweepRepetitions: 15, SweepBudget: 10 * time.Minute, EditCount: 100}
	report, err := newRunReport(o, []Corpus{c}, "", Host{}, "run")
	if err != nil {
		t.Fatal(err)
	}
	pinned := []struct {
		name  string
		value any
		want  string
	}{
		{"100-edit plan", hundred, "0b583c59bde46c199bb4b7baa674222b29a0dc17177f0cb46e243e9e4e648ef0"},
		{"1-edit plan", one, "616f10cc464b12060ae6b24e26218a92cee77fcc897d769f51eac20e8b7e6061"},
		{"TOON plan", toonPlan, "15d4158514a8dd1312ca426f38765e09386f27144eaf0bc9d548640649fd100d"},
		{"operations", editProbeOperations(hundred, c), "2fd3b48bf5c4357757b96972c99ec5e6bdd79b0ab369ea229390f73fdb205b73"},
	}
	for _, pin := range pinned {
		got, err := hashJSON(pin.value)
		if err != nil {
			t.Fatal(err)
		}
		if got != pin.want {
			t.Errorf("%s identity changed: %s", pin.name, got)
		}
	}
	if report.RequestPlan != "eefb64b2f6272b3094fcbc908ebf6df6d0d5bfdc7d1b27570b7f0bd960b5f43d" {
		t.Errorf("request plan hash changed: %s", report.RequestPlan)
	}
}

func TestEditProbeEditorStreamsAreIndependentOpenLoopPoisson(t *testing.T) {
	c := editProbeCorpus()
	one, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	streams := func(p editProbePlan) map[int][]int64 {
		out := map[int][]int64{}
		for _, step := range p.Steps {
			out[step.Editor] = append(out[step.Editor], step.DueNS)
		}
		return out
	}
	plans := map[int]editProbePlan{}
	for _, editors := range []int{2, 4} {
		p, err := makeEditProbeTrafficPlan(c, 42, 100, editors)
		if err != nil {
			t.Fatal(err)
		}
		again, err := makeEditProbeTrafficPlan(c, 42, 100, editors)
		if err != nil || !reflect.DeepEqual(p, again) {
			t.Fatal("same seed/editor count changed the offered plan", err)
		}
		if p.Editors != editors || p.SessionModel != editProbeFixedEditorModel || len(p.Steps) != 100 {
			t.Fatalf("editor plan identity: %d %q %d", p.Editors, p.SessionModel, len(p.Steps))
		}
		last := editProbeStep{DueNS: -1}
		for i, step := range p.Steps {
			// Markers and mutation IDs stay keyed by the global ordinal.
			if step.Ordinal != i || step.Marker != one.Steps[i].Marker || step.MutationID != one.Steps[i].MutationID || step.ActivateNS != step.DueNS-int64(editProbePeriod) || step.Editor < 0 || step.Editor >= editors {
				t.Fatalf("bad merged step %+v", step)
			}
			if step.DueNS < last.DueNS || step.DueNS == last.DueNS && step.Editor <= last.Editor {
				t.Fatalf("merge order is not due time then editor: %+v after %+v", step, last)
			}
			last = step
		}
		byEditor := streams(p)
		if len(byEditor) != editors {
			t.Fatalf("an editor offered no arrivals: %v", byEditor)
		}
		// Editor 0 keeps the one-editor stream; adding editors cannot shift it.
		for k, due := range byEditor[0] {
			if due != one.Steps[k].DueNS {
				t.Fatalf("editor 0 arrival %d moved: %d != %d", k, due, one.Steps[k].DueNS)
			}
		}
		plans[editors] = p
	}
	// Each editor's gaps come from its own stream, independent of how many
	// other editors exist; the merged plan only truncates each stream.
	two, four := streams(plans[2]), streams(plans[4])
	for k := range min(len(two[1]), len(four[1])) {
		if two[1][k] != four[1][k] {
			t.Fatalf("editor 1 stream depends on editor count at arrival %d", k)
		}
	}
	if reflect.DeepEqual(four[0][:min(len(four[0]), len(four[1]))], four[1][:min(len(four[0]), len(four[1]))]) {
		t.Fatal("editors share one arrival stream")
	}
	shifted, err := makeEditProbeTrafficPlan(c, 43, 100, 4)
	if err != nil || reflect.DeepEqual(shifted.Steps, plans[4].Steps) {
		t.Fatal("seed did not change the several-editor schedule", err)
	}
}

func TestEditProbeEditorCountRefusals(t *testing.T) {
	c := editProbeCorpus()
	for _, cell := range []struct{ count, editors int }{{100, 0}, {100, editProbeMaxEditors + 1}, {1, 2}, {100, -1}} {
		if _, err := makeEditProbeTrafficPlan(c, 42, cell.count, cell.editors); err == nil {
			t.Fatalf("invalid editor cell accepted: %+v", cell)
		}
	}
	for _, cell := range []struct{ count, editors int }{{1, 1}, {100, 1}, {100, editProbeMaxEditors}} {
		if _, err := makeEditProbeTrafficPlan(c, 42, cell.count, cell.editors); err != nil {
			t.Fatalf("valid editor cell refused: %+v: %v", cell, err)
		}
	}
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--worktrees", "0"}
	for _, editors := range []string{"1", "4", "32"} {
		if o, err := parseRun(append(base, "--edit-agents", editors)); err != nil || o.Editors < 1 {
			t.Fatalf("--edit-agents %s refused: %v", editors, err)
		}
	}
	for _, extra := range [][]string{{"--edit-agents", "0"}, {"--edit-agents", "33"}, {"--edit-agents", "2", "--edit-count", "1"}, {"--edit-agents", "8", "--host", "floor"}} {
		if _, err := parseRun(append(base, extra...)); err == nil {
			t.Fatalf("invalid edit-and-probe editors accepted: %v", extra)
		}
	}
	paired := []string{"--build", "main=/private/main", "--build", "candidate=/private/candidate", "--manifest", "/private/manifest.json", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--worktrees", "0", "--edit-agents", "2"}
	if _, _, err := parsePaired(paired); err == nil {
		t.Fatal("paired no-regression cell widened past one editor")
	}
}
