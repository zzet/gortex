package main

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestEditMeanControlsActualSeededOffersWithoutChangingDefault(t *testing.T) {
	base := []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "edit-and-probe", "--worktrees", "0", "--agents", "0", "--edit-agents", "2"}
	defaults, err := parseRun(base)
	if err != nil {
		t.Fatal(err)
	}
	original, err := planEditProbe(editProbeCorpus(), defaults)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := parseRun(append(append([]string{}, base...), "--edit-mean", "15s"))
	if err != nil {
		t.Fatalf("explicit edit mean must be accepted: %v", err)
	}
	same, err := planEditProbe(editProbeCorpus(), explicit)
	if err != nil || !reflect.DeepEqual(original, same) {
		t.Fatalf("default offered plan changed: %v", err)
	}
	override, err := parseRun(append(append([]string{}, base...), "--edit-mean", "250ms"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hot := range []bool{false, true} {
		c := editProbeCorpus()
		if hot {
			c.EditSender = editProbeHotFile
		}
		before, err := planEditProbe(c, defaults)
		if err != nil {
			t.Fatal(err)
		}
		after, err := planEditProbe(c, override)
		if err != nil {
			t.Fatal(err)
		}
		if after.MeanNS != int64(250*time.Millisecond) {
			t.Fatalf("wrong offered mean %d", after.MeanNS)
		}
		again, err := planEditProbe(c, override)
		if err != nil || !reflect.DeepEqual(after, again) {
			t.Fatal("mean override is not deterministic")
		}
		for i, step := range after.Steps {
			// Same RNG/seed/editor streams, with integer truncation bounded by
			// the number of arrivals. This checks actual offers, not just a label.
			want := float64(before.Steps[i].DueNS-int64(editProbePeriod)) / 60
			if math.Abs(float64(step.DueNS-int64(editProbePeriod))-want) > 100 || step.ActivateNS != step.DueNS-int64(editProbePeriod) || step.Editor != before.Steps[i].Editor {
				t.Fatalf("mean did not control actual offer %d: %+v", i, step)
			}
			if hot && i%2 == 1 && step.DueNS != after.Steps[i-1].DueNS {
				t.Fatal("mean override broke fixed-editor hot-file pairs")
			}
		}
		budget, err := after.budget(time.Second, 1)
		if err != nil || budget != time.Duration(after.Steps[len(after.Steps)-1].DueNS)+time.Second {
			t.Fatalf("budget did not follow actual final offer: %v %v", budget, err)
		}
	}
	if _, err := parseRun(append(append([]string{}, base...), "--edit-mean", "-1s")); err == nil {
		t.Fatal("negative mean accepted")
	}
	read := []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "read", "--edit-mean", "250ms"}
	if _, err := parseRun(read); err == nil {
		t.Fatal("edit-only mean silently accepted for another profile")
	}
}
