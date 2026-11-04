//go:build acceptance

package mcp

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type projectionLatencyCandidate struct {
	Path         string `json:"path"`
	ChargedBytes int64  `json:"charged_bytes"`
	RawSHA256    string `json:"raw_sha256"`
	SourceBytes  int    `json:"source_bytes"`
	Nodes        int    `json:"nodes"`
	Edges        int    `json:"edges"`
	Functions    int    `json:"functions"`
	NamesCharged int64  `json:"names_charged_bytes"`
	StateCharged int64  `json:"state_charged_bytes"`
}

// Selection is setup, never a timed handler operation. Round-robin directories
// avoid an alphabetical package prefix; actual charge feasibility preserves
// the full requested count without replacing the corpus with cheapest files.
func selectProjectionLatencyPopulation(candidates []projectionLatencyCandidate, count int, maxBytes int64) ([]projectionLatencyCandidate, error) {
	if count <= 0 || count > projectionMaxFiles || maxBytes <= 0 || maxBytes > projectionMaxBytes {
		return nil, fmt.Errorf("invalid projection count or unchanged byte cap")
	}
	if len(candidates) < count {
		return nil, fmt.Errorf("only %d candidates for %d entries", len(candidates), count)
	}
	groups := map[string][]projectionLatencyCandidate{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.Path == "" || seen[candidate.Path] || candidate.ChargedBytes <= 0 {
			return nil, fmt.Errorf("invalid or duplicate charged projection: %q", candidate.Path)
		}
		seen[candidate.Path] = true
		dir := path.Dir(candidate.Path)
		groups[dir] = append(groups[dir], candidate)
	}
	var directories []string
	for dir, group := range groups {
		directories = append(directories, dir)
		sort.Slice(group, func(i, j int) bool { return group[i].Path < group[j].Path })
	}
	slices.Sort(directories)
	var ordered []projectionLatencyCandidate
	for round := 0; len(ordered) < len(candidates); round++ {
		for _, dir := range directories {
			if round < len(groups[dir]) {
				ordered = append(ordered, groups[dir][round])
			}
		}
	}
	minimum := func(items []projectionLatencyCandidate, needed int, limit int64) int64 {
		if len(items) < needed {
			return limit + 1
		}
		charges := make([]int64, len(items))
		for i, item := range items {
			charges[i] = item.ChargedBytes
		}
		slices.Sort(charges)
		var total int64
		for _, charge := range charges[:needed] {
			if charge > limit-total {
				return limit + 1
			}
			total += charge
		}
		return total
	}
	if minimum(ordered, count, maxBytes) > maxBytes {
		return nil, fmt.Errorf("%d actual projections cannot fit the unchanged %d-byte cap", count, maxBytes)
	}
	var selected []projectionLatencyCandidate
	var used int64
	for i, candidate := range ordered {
		if candidate.ChargedBytes > maxBytes-used {
			continue
		}
		remaining := maxBytes - used - candidate.ChargedBytes
		if minimum(ordered[i+1:], count-len(selected)-1, remaining) > remaining {
			continue
		}
		selected = append(selected, candidate)
		used += candidate.ChargedBytes
		if len(selected) == count {
			return selected, nil
		}
	}
	return nil, fmt.Errorf("capacity selection could not retain all %d projections", count)
}

func TestProjectionLatencyPopulationFitsActualChargeWithoutSmallestOnlySelection(t *testing.T) {
	all := []projectionLatencyCandidate{
		{Path: "a/one.go", ChargedBytes: 40},
		{Path: "a/two.go", ChargedBytes: 40},
		{Path: "b/one.go", ChargedBytes: 15},
		{Path: "b/two.go", ChargedBytes: 15},
		{Path: "c/one.go", ChargedBytes: 15},
		{Path: "c/two.go", ChargedBytes: 15},
	}
	selected, err := selectProjectionLatencyPopulation(all, 4, 100)
	require.NoError(t, err)
	require.Len(t, selected, 4)
	var total int64
	for _, item := range selected {
		total += item.ChargedBytes
	}
	require.LessOrEqual(t, total, int64(100), "an invalid lexical prefix must not reach timing")
	require.Contains(t, selected, all[0], "capacity must not silently mean selecting only the cheapest files")
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	repeated, err := selectProjectionLatencyPopulation(all, 4, 100)
	require.NoError(t, err)
	require.Equal(t, selected, repeated, "input enumeration must not change the pinned cohort")
}

func TestProjectionLatencyPopulationNeverCreditsAnIncompleteOrUnchargedSet(t *testing.T) {
	for _, candidates := range [][]projectionLatencyCandidate{
		{{Path: "a.go", ChargedBytes: 60}, {Path: "b.go", ChargedBytes: 60}},
		{{Path: "a.go", ChargedBytes: 10}},
		{{Path: "a.go", ChargedBytes: 10}, {Path: "a.go", ChargedBytes: 10}},
		{{Path: "a.go", ChargedBytes: 0}, {Path: "b.go", ChargedBytes: 10}},
		{{Path: "a.go", ChargedBytes: -1}, {Path: "b.go", ChargedBytes: 10}},
	} {
		selected, err := selectProjectionLatencyPopulation(candidates, 2, 100)
		require.Error(t, err)
		require.Nil(t, selected)
	}
}

func TestProjectionLatencyPopulationManifestKeepsExactCountChargeAndSource(t *testing.T) {
	valid := projectionLatencyPopulationManifest{
		Revision: strings.Repeat("a", 40), Policy: projectionLatencySelectionPolicy,
		Requested: 2, Admitted: 2, MaxFiles: projectionMaxFiles, MaxBytes: projectionMaxBytes,
		ChargedBytes: 55, IndexedFiles: 12, NativeSymbols: 20, EligibleCount: 8,
		NativeSymbolsSHA256: strings.Repeat("a", 64), NativeSymbolsEncoding: projectionLatencyNativeMembershipEncoding,
		SelectedNodes: 2, SelectedFuncs: 2,
		InputsVerified: true,
		Entries: []projectionLatencyCandidate{
			{Path: "a/one.go", ChargedBytes: 40, RawSHA256: strings.Repeat("a", 64), SourceBytes: 20, Nodes: 1, Functions: 1},
			{Path: "b/one.go", ChargedBytes: 15, RawSHA256: strings.Repeat("b", 64), SourceBytes: 20, Nodes: 1, Functions: 1},
		},
	}
	require.NoError(t, validateProjectionLatencyPopulationManifest(valid, valid.Revision, 2))
	for _, change := range []func(*projectionLatencyPopulationManifest){
		func(m *projectionLatencyPopulationManifest) { m.Admitted-- },
		func(m *projectionLatencyPopulationManifest) { m.InputsVerified = false },
		func(m *projectionLatencyPopulationManifest) { m.ChargedBytes-- },
		func(m *projectionLatencyPopulationManifest) { m.NativeSymbols = 0 },
		func(m *projectionLatencyPopulationManifest) { m.SelectedFuncs-- },
		func(m *projectionLatencyPopulationManifest) { m.MaxBytes++ },
		func(m *projectionLatencyPopulationManifest) { m.Revision = strings.Repeat("b", 40) },
		func(m *projectionLatencyPopulationManifest) { m.Entries[0].Path = "../outside.go" },
		func(m *projectionLatencyPopulationManifest) { m.Entries[0].RawSHA256 = "unknown" },
		func(m *projectionLatencyPopulationManifest) { m.Entries[0].Functions = 0 },
	} {
		bad := valid
		bad.Entries = append([]projectionLatencyCandidate(nil), valid.Entries...)
		change(&bad)
		require.Error(t, validateProjectionLatencyPopulationManifest(bad, valid.Revision, 2))
	}
	require.Error(t, validateProjectionLatencyPopulationManifest(valid, valid.Revision, projectionMaxFiles), "a smaller proof cannot credit512")
}
