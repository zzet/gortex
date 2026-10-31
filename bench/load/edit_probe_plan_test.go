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
