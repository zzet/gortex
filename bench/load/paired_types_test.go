package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPairedSchemaGolden(t *testing.T) {
	data, err := json.MarshalIndent(pairedSchema(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := filepath.Join("testdata", "paired-v1.schema.json")
	if os.Getenv("UPDATE_LOAD_GOLDEN") == "1" {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("paired schema changed: %v", err)
	}
}

func TestPairedOrdersAndIdenticalTupleHash(t *testing.T) {
	labels := []string{"tag", "main", "candidate"}
	counts := make(map[string]int)
	for i := range 15 {
		order := rotatingOrder(labels, uint64(i))
		counts[order[0]]++
		if len(order) != 3 || order[0] == order[1] || order[1] == order[2] || order[0] == order[2] {
			t.Fatal(order)
		}
	}
	if !reflect.DeepEqual(counts, map[string]int{"tag": 5, "main": 5, "candidate": 5}) {
		t.Fatalf("unbalanced first-build order: %v", counts)
	}
	tuple := requestTuple{Tool: "read.source", Role: "workload", Agent: 1, Step: 2, Slot: 3, RequestHash: "normalized-source-request"}
	a, err := hashJSON(tuple)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashJSON(tuple)
	if err != nil || a != b {
		t.Fatal("identical logical tuple did not retain hash")
	}
	tuple.Role = "probe"
	probe, err := hashJSON(tuple)
	if err != nil || probe == a {
		t.Fatal("workload/probe denominators share tuple identity")
	}
	tuple.Role = "workload"
	tuple.Repo = "other"
	scope, err := hashJSON(tuple)
	if err != nil || scope == a {
		t.Fatal("different repository scope shares tuple identity")
	}
}

func TestMatchedRatiosUseMedianAndRetainExclusions(t *testing.T) {
	var records []pairedRecord
	for i, ratio := range []float64{1, 3} {
		records = append(records, pairedRecord{Tuple: requestTuple{Tool: "read.source", Role: "workload", RequestHash: "same"}, Order: rotatingOrder([]string{"tag", "main"}, uint64(i)), Samples: map[string]pairedSample{
			"tag": {Outcome: "success", RequestHash: "same", Latency: measured(10, "ms")}, "main": {Outcome: "success", RequestHash: "same", Latency: measured(10*ratio, "ms")},
		}})
	}
	for _, sample := range []pairedSample{{Outcome: "error", RequestHash: "same", Latency: measured(1, "ms")}, {Outcome: "success", RequestHash: "different", Latency: measured(1, "ms")}, {Outcome: "phase_censored", RequestHash: "same", Latency: unavailable("ms", "censored")}} {
		records = append(records, pairedRecord{Tuple: requestTuple{Tool: "read.source", Role: "workload", RequestHash: "same"}, Order: []string{"main", "tag"}, Samples: map[string]pairedSample{"tag": {Outcome: "success", RequestHash: "same", Latency: measured(10, "ms")}, "main": sample}})
	}
	row := matchedLatencyRatios([]string{"tag", "main"}, records)["main/tag"]["workload.read.source"]
	if row.Median.Value == nil || *row.Median.Value != 2 || row.Matched != 2 || row.Excluded != 3 {
		t.Fatalf("matched median/error accounting: %+v", row)
	}
	if len(records) != 5 || records[4].Samples["main"].Outcome != "phase_censored" {
		t.Fatal("incomplete record erased")
	}
	if err := validateBuildLabels([]string{"tag", "main", "candidate"}); err != nil {
		t.Fatal(err)
	}
	for _, labels := range [][]string{{"main"}, {"tag", "tag"}, {"tag", "main", "candidate", "extra"}} {
		if validateBuildLabels(labels) == nil {
			t.Fatalf("invalid build list accepted: %v", labels)
		}
	}
}

func TestMatchedRatiosExcludeZeroClockSamplesOnEitherBuild(t *testing.T) {
	for _, test := range []struct {
		a, b    float64
		matched int
	}{{0, 1, 0}, {1, 0, 0}, {0, 0, 0}, {-1, 1, 0}, {1, -1, 0}, {.1, .2, 1}} {
		record := pairedRecord{Tuple: requestTuple{Tool: "read.source", Role: "workload", RequestHash: "same"}, Order: []string{"tag", "main"}, Samples: map[string]pairedSample{
			"tag":  {Outcome: "success", RequestHash: "same", Latency: measured(test.a, "ms")},
			"main": {Outcome: "success", RequestHash: "same", Latency: measured(test.b, "ms")},
		}}
		row := matchedLatencyRatios([]string{"tag", "main"}, []pairedRecord{record})["main/tag"]["workload.read.source"]
		if row.Matched != test.matched || row.Excluded != 1-test.matched || (row.Median.Value != nil) != (test.matched == 1) {
			t.Fatalf("unresolved clock sample became a ratio: a%g b%g %+v", test.a, test.b, row)
		}
		if *record.Samples["tag"].Latency.Value != test.a || *record.Samples["main"].Latency.Value != test.b {
			t.Fatal("excluded raw clock sample was rewritten")
		}
	}
}
