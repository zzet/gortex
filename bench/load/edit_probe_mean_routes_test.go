package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestEditMeanPreservesGlobalScheduleAcrossPinnedRoutes(t *testing.T) {
	for _, cell := range []struct {
		name                   string
		repos, linked, editors int
	}{
		{"six_repositories", 6, 0, 6}, {"four_checkouts", 1, 3, 4},
	} {
		t.Run(cell.name, func(t *testing.T) {
			args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", fmt.Sprint(cell.editors), "--worktrees", fmt.Sprint(cell.linked), "--edit-repos", fmt.Sprint(cell.repos), "--edit-count", "100", "--seed", "42"}
			o, err := parseRun(args)
			if err != nil {
				t.Fatal(err)
			}
			corpora := editProbeRoutingCorpora()
			if cell.linked > 0 {
				corpora = corpora[:1]
			}
			defaults, err := planEditProbeRoutes(corpora, o)
			if err != nil {
				t.Fatal(err)
			}
			o.EditMean = 15 * time.Second
			explicit, err := planEditProbeRoutes(corpora, o)
			if err != nil || !reflect.DeepEqual(defaults, explicit) {
				t.Fatalf("default routed schedule changed: %v", err)
			}
			o.EditMean = 250 * time.Millisecond
			overridden, err := planEditProbeRoutes(corpora, o)
			if err != nil {
				t.Fatal(err)
			}
			again, err := planEditProbeRoutes(corpora, o)
			if err != nil || !reflect.DeepEqual(overridden, again) {
				t.Fatal("routed mean lost determinism")
			}
			reference, err := makeEditProbeTrafficPlanWithMean(corpora[0], o.Seed, o.EditCount, o.Editors, o.EditMean)
			if err != nil {
				t.Fatal(err)
			}
			if overridden.MeanNS != int64(o.EditMean) || len(overridden.Steps) != 100 || len(overridden.Routes) != cell.editors {
				t.Fatal("routed mean or denominator changed")
			}
			for i, step := range overridden.Steps {
				want := reference.Steps[i]
				if step.DueNS != want.DueNS || step.ActivateNS != want.ActivateNS || step.Editor != want.Editor || step.Ordinal != want.Ordinal || step.Route != overridden.Routes[step.Editor%len(overridden.Routes)].ID {
					t.Fatalf("route reseeded or rescheduled global offer%d: %+v", i, step)
				}
			}
		})
	}
}
