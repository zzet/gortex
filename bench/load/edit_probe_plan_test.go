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
