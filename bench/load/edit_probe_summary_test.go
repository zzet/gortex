package main

import (
	"testing"
)

func TestEditProbeEveryOracleNameHasExactlyOneGroup(t *testing.T) {
	c := editProbeCorpus()
	marker := "GortexEditProbe0000000000000001N000"
	steps := map[string]editProbeStep{
		"default": {Marker: marker},
		"rename":  {Marker: marker, BeforeSymbol: c.File + "::" + marker + "Before"},
		"body":    {Marker: marker, BeforeSymbol: c.File + "::" + marker + "Before", TargetSymbol: c.File + "::" + marker + "Before", FollowingSymbol: c.File + "::" + marker + "After"},
		"delete":  {Marker: marker, BeforeSymbol: c.File + "::" + marker + "Before", Deleted: true},
		"move":    {Marker: marker, BeforeSymbol: c.File + "::" + marker + "Before", TargetSymbol: "target.go::" + marker + "Before", Relocated: true, DestinationFile: "target.go"},
	}
	for name, step := range steps {
		groups := map[string]int{}
		for _, oracle := range editProbeOracles(c, step) {
			group, ok := editProbeOracleGroups[oracle.name]
			if !ok {
				t.Fatalf("%s: oracle %s has no visibility group", name, oracle.name)
			}
			groups[group]++
		}
		if groups["file_graph"] == 0 || groups["relations"] == 0 || groups["search"] == 0 {
			t.Fatalf("%s: a visibility group has no member: %v", name, groups)
		}
	}
}

func TestEditProbeGroupTimesUseLatestMemberAndNeverInventZero(t *testing.T) {
	first := map[string]Metric{
		"source": measured(10, "ms"), "summary": measured(30, "ms"), "editing_context": measured(20, "ms"),
		"callers": unavailable("ms", "not observed"), "call_chain": measured(5, "ms"),
		"search_visible": measured(7, "ms"), "text": measured(9, "ms"),
	}
	record := editProbeRecord{FirstCorrect: first}
	record.groupTimes()
	if record.FileGraph.Value == nil || *record.FileGraph.Value != 30 || record.Search.Value == nil || *record.Search.Value != 9 {
		t.Fatalf("group time is not its slowest member: %+v %+v", record.FileGraph, record.Search)
	}
	if record.Relations.Value != nil || record.Relations.Reason != "callers never answered correctly" {
		t.Fatalf("missing member made relations faster: %+v", record.Relations)
	}
	if got := editProbeGroupTime(map[string]Metric{"source": measured(1, "ms")}, "relations"); got.Value != nil || got.Reason != "cell has no relations oracle" {
		t.Fatalf("empty group invented a time: %+v", got)
	}
	unsent := newEditProbeRecord(editProbeCorpus(), editProbeStep{Marker: "M"})
	for name, metric := range map[string]Metric{"readback": unsent.Readback, "file_graph": unsent.FileGraph, "relations": unsent.Relations, "search": unsent.Search, "derived": unsent.Derived} {
		if metric.Value != nil || metric.Reason == "" || metric.Unit != "ms" {
			t.Fatalf("unsent edit %s is not explicitly unavailable: %+v", name, metric)
		}
	}
}

func TestEditProbeDerivedTimeRequiresOwnedTimeline(t *testing.T) {
	timeline := func(terminal bool, phases ...any) map[string]any {
		return map[string]any{"publication_phases": map[string]any{"clock": "monotonic", "terminal": terminal, "phases": phases}}
	}
	row := func(phase string, ns int64) any { return map[string]any{"phase": phase, "offset_ns": ns} }
	owned := timeline(true, row("received", 0), row("receipt_committed", 4_000_000), row("patch_derived_finished", 12_500_000))
	if got := editProbeDerived(owned); got.Value == nil || *got.Value != 12.5 {
		t.Fatalf("owned derived time: %+v", got)
	}
	coalesced := timeline(true, row("received", 0), row("patch_derived_finished", 1))
	coalesced["coalesced_publication"] = map[string]any{}
	for name, status := range map[string]map[string]any{
		"no_timeline": {"graph_status": "fresh"},
		"pending":     timeline(false, row("received", 0), row("patch_derived_finished", 1)),
		"no_derived":  timeline(true, row("received", 0)),
		"repeated":    timeline(true, row("received", 0), row("patch_derived_finished", 1), row("patch_derived_finished", 2)),
		"reversed":    timeline(true, row("received", 5), row("patch_derived_finished", 1)),
		"coalesced":   coalesced,
	} {
		if got := editProbeDerived(status); got.Value != nil || got.Reason == "" {
			t.Fatalf("%s: derived time without an owned terminal timeline: %+v", name, got)
		}
	}
}
