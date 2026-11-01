package main

import (
	"reflect"
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

func TestEditProbeQuantilesUseNearestRankWithSeededBootstrap(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64((i*37)%100 + 1) // 1..100, shuffled
	}
	got := editProbeQuantilesOf(values, 0, 3, "ms", 42, "t_ack_ms")
	want := distribution(values, "ms")
	if got.Count != 100 || got.Censored != 0 || got.NotApplicable != 3 || *got.P50.Value != *want.P50.Value || *got.P95.Value != *want.P95.Value || *got.P99.Value != *want.P99.Value || *got.Max.Value != 100 {
		t.Fatalf("not the nearest-rank distribution: %+v", got)
	}
	if got.Resamples != editProbeBootstrapResamples {
		t.Fatalf("resamples %d", got.Resamples)
	}
	for name, pair := range map[string]struct {
		estimate Metric
		ci       editProbeInterval
	}{"p50": {got.P50, got.P50CI}, "p95": {got.P95, got.P95CI}, "p99": {got.P99, got.P99CI}} {
		if pair.ci.Low.Value == nil || pair.ci.High.Value == nil || *pair.ci.Low.Value > *pair.estimate.Value || *pair.ci.High.Value < *pair.estimate.Value {
			t.Fatalf("%s interval does not bracket its estimate: %+v %+v", name, pair.ci, pair.estimate)
		}
	}
	if again := editProbeQuantilesOf(values, 0, 3, "ms", 42, "t_ack_ms"); !reflect.DeepEqual(again, got) {
		t.Fatal("same seed and measure changed the interval")
	}
	if other := editProbeQuantilesOf(values, 0, 3, "ms", 43, "t_ack_ms"); reflect.DeepEqual(other.P50CI, got.P50CI) && reflect.DeepEqual(other.P95CI, got.P95CI) {
		t.Fatal("seed does not drive the resampling stream")
	}
	few := editProbeQuantilesOf(values[:19], 0, 0, "ms", 42, "t_ack_ms")
	if few.P50CI.Low.Value == nil || few.P95CI.Low.Value != nil || few.P95CI.High.Reason != "bootstrap interval needs at least 20 observations" || few.P99CI.Low.Value != nil {
		t.Fatalf("too few observations for p95/p99 intervals: %+v", few)
	}
	none := editProbeQuantilesOf(nil, 0, 5, "ms", 42, "t_ack_ms")
	if none.Count != 0 || none.NotApplicable != 5 || none.P50.Value != nil || none.P50CI.Low.Value != nil || none.Resamples != 0 {
		t.Fatalf("no observations invented a value: %+v", none)
	}
}

// Records cut off before a value was observed rank above every observed
// value: dropping them would make the tail look faster than it was.
func TestEditProbeQuantilesRankCensoredRecordsAboveObservedValues(t *testing.T) {
	observed := make([]float64, 95)
	for i := range observed {
		observed[i] = float64(i + 1)
	}
	got := editProbeQuantilesOf(observed, 5, 2, "ms", 42, "t_search_ms")
	if got.Count != 95 || got.Censored != 5 || got.NotApplicable != 2 || *got.P50.Value != 50 || *got.P95.Value != 95 {
		t.Fatalf("censored records shifted the observed ranks: %+v", got)
	}
	reason := "nearest rank falls among 5 censored records"
	if got.P99.Value != nil || got.P99.Reason != reason || got.Max.Value != nil || got.Max.Reason != reason || got.P99CI.High.Value != nil {
		t.Fatalf("a quantile among censored records was reported: %+v", got)
	}
	if dropped := distribution(observed, "ms"); *dropped.P99.Value != 95 {
		t.Fatalf("observed-only p99 = %v", *dropped.P99.Value)
	}
	if all := editProbeQuantilesOf(nil, 3, 0, "ms", 42, "t_search_ms"); all.P50.Value != nil || all.P50.Reason != "nearest rank falls among 3 censored records" || all.P50CI.Low.Value != nil {
		t.Fatalf("only censored records invented a value: %+v", all)
	}
}

func TestEditProbeSummaryCountsCensoredAndNotApplicableWithoutZero(t *testing.T) {
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	report := newEditProbeReport(plan, corpus, true)
	// 95 complete records (five without a derived time, which then does not
	// apply), two that hit the deadline after their ACK before callers was
	// correct, and three never sent.
	for i := range 97 {
		record := &report.Records[i]
		record.Outcome = "complete"
		record.Ack = measured(float64(100+i), "ms")
		if i >= 95 {
			record.Outcome = "product_deadline"
			continue
		}
		record.Readbacks = []editProbeReadback{{Kind: "file", Latency: measured(2, "ms")}}
		record.FirstCorrect["callers"] = measured(float64(200+i), "ms")
		if i < 90 {
			record.Derived = measured(float64(i), "ms")
		}
	}
	summary := summarizeEditProbe(report.Records, plan.Seed)
	if summary.Offered != 100 || summary.Outcomes["complete"] != 95 || summary.Outcomes["product_deadline"] != 2 || summary.Outcomes["phase_censored"] != 3 {
		t.Fatalf("outcome denominator: %+v", summary.Outcomes)
	}
	ack := summary.Measures["t_ack_ms"]
	if ack.Count != 97 || ack.Censored != 3 || ack.NotApplicable != 0 || *ack.P50.Value != 149 || *ack.P95.Value != 194 || ack.P99.Value != nil {
		t.Fatalf("unsent edits were dropped from the ACK ranking: %+v", ack)
	}
	callers := summary.Measures["first_correct_ms.callers"]
	if callers.Count != 95 || callers.Censored != 5 || *callers.P95.Value != 294 || callers.P99.Value != nil {
		t.Fatalf("deadline-censored callers made the tail faster: %+v", callers)
	}
	if derived := summary.Measures["t_derived_ms"]; derived.Count != 90 || derived.NotApplicable != 5 || derived.Censored != 5 {
		t.Fatalf("derived denominator: %+v", derived)
	}
	if file := summary.Measures["readback_ms.file"]; file.Count != 95 || file.NotApplicable != 5 || file.Censored != 0 {
		t.Fatalf("readback denominator: %+v", file)
	}
	for name, measure := range summary.Measures {
		if measure.Count+measure.Censored+measure.NotApplicable != 100 {
			t.Fatalf("%s does not cover the offered denominator: %+v", name, measure)
		}
	}
}
