package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPairedPolicyAllowsOnlyCoupledNativeWorkloads(t *testing.T) {
	base := []string{"--build", "tag=/private/tag", "--build", "main=/private/main", "--manifest", "/private/manifest.json", "--scratch", t.TempDir()}
	options, builds, err := parsePaired(base)
	if err != nil || options.Host != "native" || options.Profile != "agent" || len(builds) != 2 {
		t.Fatalf("paired defaults: %+v %v %v", options, builds, err)
	}
	if _, builds, err := parsePaired(append(base, "--build", "candidate=/private/candidate")); err != nil || len(builds) != 3 {
		t.Fatalf("optional candidate: %v %v", builds, err)
	}
	for _, extra := range [][]string{{"--host", "reference"}, {"--host", "floor"}, {"--profile", "saturation"}, {"--scenario", "cold"}, {"--scenario", "restart"}, {"--scenario", "all"}, {"--cpu-profile", "1s"}, {"--binary", "/other"}, {"--build", "tag=/duplicate"}, {"--build", "candidate=/third", "--build", "extra=/fourth"}} {
		if _, _, err := parsePaired(append(base, extra...)); err == nil {
			t.Fatalf("unsupported paired policy accepted: %v", extra)
		}
	}
}

func TestPairedSummaryLabelsRatiosAsCoupled(t *testing.T) {
	var output bytes.Buffer
	r := pairedReport{Ratios: map[string]map[string]pairedRatio{"main/tag": {"workload.read.source": {Median: measured(.8, "ratio"), Matched: 15, Excluded: 2}, "probe.search.symbols": {Median: unavailable("ratio", "no matches"), Excluded: 1}}}}
	if err := printPairedRatios(&output, r); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"0.8000", "unavailable", "15", "2", "do not establish independent throughput"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("summary omitted %q: %s", text, output.String())
		}
	}
}

func TestPairedEditorOnlyCountsAreExplicit(t *testing.T) {
	base := []string{"--build", "tag=/private/tag", "--build", "main=/private/main", "--manifest", "/private/manifest.json", "--scratch", t.TempDir(), "--agents", "0", "--edit-agents", "4"}
	for _, scenario := range []string{"edit", "many-edit"} {
		options, _, err := parsePaired(append(base, "--scenario", scenario))
		if err != nil || options.Agents != 0 || options.Editors != 4 {
			t.Fatalf("paired editor-only %s: %+v %v", scenario, options, err)
		}
	}
	if _, _, err := parsePaired(append(base, "--scenario", "read")); err == nil {
		t.Fatal("zero-reader read scenario accepted")
	}
}
