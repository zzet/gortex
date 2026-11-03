package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// Unavailable/incorrect/censored offers occupy the high end of the rank
// denominator. They cannot become measured zeros or vanish from p95/p99.
func readbackReferenceTiming(rows []readbackReferenceRecord, endpoint string, offered int) Distribution {
	var values []float64
	for _, row := range rows {
		p, ok := row.Probes[endpoint]
		if ok && p.Correct && p.Error == "" && p.Latency.Value != nil && !math.IsNaN(*p.Latency.Value) && !math.IsInf(*p.Latency.Value, 0) && *p.Latency.Value >= 0 {
			values = append(values, *p.Latency.Value)
		}
	}
	slices.Sort(values)
	out := Distribution{Count: len(values)}
	q := func(p float64) Metric {
		if offered <= 0 || len(rows) > offered {
			return unavailable("ms", "invalid offered denominator")
		}
		rank := int(math.Ceil(p*float64(offered))) - 1
		if rank < 0 || rank >= len(values) {
			return unavailable("ms", "quantile enters unavailable/incorrect/censored offers")
		}
		return measured(values[rank], "ms")
	}
	out.P50, out.P95, out.P99, out.Max = q(.5), q(.95), q(.99), q(1)
	return out
}

type readbackReferenceComparisonIdentity struct {
	BinarySHA256     string `json:"binary_sha256"`
	ManifestSHA256   string `json:"manifest_sha256"`
	HostSHA256       string `json:"host_sha256"`
	PopulationSHA256 string `json:"population_sha256"`
	ReadPlanSHA256   string `json:"read_plan_sha256"`
	ArrivalSHA256    string `json:"arrival_sha256"`
	Agents           int    `json:"agents"`
	Kind             string `json:"kind"`
	ToolSurface      string `json:"tool_surface"`
}

func readbackReferenceBaselineEqual(a, b readbackReferenceComparisonIdentity) bool {
	return a.BinarySHA256 != "" && a.ManifestSHA256 != "" && a.HostSHA256 != "" && a.PopulationSHA256 != "" && a.ReadPlanSHA256 != "" && a.ArrivalSHA256 != "" && a.ToolSurface != "" && a == b
}
func readbackReferenceReadLimit(endpoint string, agents int) (float64, error) {
	index := -1
	for i, n := range []int{1, 4, 8, 16, 32} {
		if n == agents {
			index = i
		}
	}
	if index < 0 {
		return 0, errors.New("unknown reference agent count")
	}
	switch endpoint {
	case "source", "symbol", "file":
		return []float64{50, 50, 60, 80, 100}[index], nil
	case "summary", "editing_context":
		return []float64{60, 125, 140, 160, 250}[index], nil
	}
	return 0, errors.New("unknown reference endpoint")
}
func compareReadbackReferenceTiming(endpoint string, agents int, under, baseline Distribution, identity, baseIdentity readbackReferenceComparisonIdentity) error {
	if identity.Agents != agents || baseIdentity.Agents != agents || !readbackReferenceBaselineEqual(identity, baseIdentity) {
		return errors.New("no proven-equivalent same-build/host/population/read-plan baseline")
	}
	limit, err := readbackReferenceReadLimit(endpoint, agents)
	if err != nil {
		return err
	}
	if under.P95.Value == nil || baseline.P95.Value == nil {
		return errors.New("p95 unavailable: missing baseline or censored rank")
	}
	if *under.P95.Value > limit || *under.P95.Value > *baseline.P95.Value+25 {
		return fmt.Errorf("%s p95 exceeds absolute%.0fms or own-baseline+25ms", endpoint, limit)
	}
	if endpoint == "source" && agents == 4 && (under.P99.Value == nil || *under.P99.Value > 200) {
		return errors.New("source p99 at4 agents exceeds200ms or is unavailable")
	}
	return nil
}

// Reuse the load driver's accepted same-workload/host/corpus comparator for
// the separate main read-only mix. Its generic15% display threshold is not the
// #1014 verdict: this criterion enforces5% and refuses missing tool samples.
func compareReadbackReferenceMain(main, candidate Report, mainRevision, candidateRevision string) error {
	if !commitPattern.MatchString(mainRevision) || !commitPattern.MatchString(candidateRevision) || main.GitSHA != mainRevision || candidate.GitSHA != candidateRevision {
		return errors.New("main/candidate read-only binary revisions are not the declared pins")
	}
	if err := comparable(main, candidate); err != nil {
		return err
	}
	if len(main.Corpus) == 0 || !main.Complete || !candidate.Complete || len(main.Errors) != 0 || len(candidate.Errors) != 0 || !main.Host.Validated || main.Host.Profile != "reference" {
		return errors.New("main read-only comparison requires complete reference reports")
	}
	if main.Workload.Scenario != "read" || main.Workload.EditAgents != 0 || main.Workload.LinkedWorktrees != 0 || main.Workload.Agents != 4 && main.Workload.Agents != 8 || len(main.Phases) != 1 || len(main.Phases[0].Edits) != 0 || len(candidate.Phases[0].Edits) != 0 {
		return errors.New("main comparison is the explicit no-edit4/8-agent reading mix")
	}
	for _, corpus := range main.Corpus {
		ops, err := readingPlan(corpus)
		if err != nil {
			return err
		}
		for _, op := range ops {
			before := readbackReferenceMainP95(main.Phases[0], op.name())
			after := readbackReferenceMainP95(candidate.Phases[0], op.name())
			if before.Value == nil || after.Value == nil {
				return fmt.Errorf("main read-only mix lacks conservative%s p95: %s / %s", op.name(), before.Reason, after.Reason)
			}
			if *after.Value > *before.Value*1.05 {
				return fmt.Errorf("main read-only%s p95 exceeds5%% regression bound", op.name())
			}
		}
	}
	return nil
}

// Existing read-only calls may retry BUSY. Group their real final attempts as
// the existing logical offer; elapsed logical time includes prior retries.
// Every failed/censored logical offer remains at the high end of the rank.
func readbackReferenceMainP95(phase Phase, tool string) Metric {
	type key struct {
		agent, step, slot int
		request           string
	}
	final := map[key]Attempt{}
	for _, a := range phase.Attempts {
		if a.Role != "workload" || a.Tool != tool {
			continue
		}
		k := key{a.Agent, a.Step, a.Slot, a.Request}
		old, exists := final[k]
		if exists && old.Retry == a.Retry {
			return unavailable("ms", "duplicate logical attempt")
		}
		if !exists || a.Retry > old.Retry {
			final[k] = a
		}
	}
	var values []float64
	for _, a := range final {
		if a.Outcome == "success" && a.LogicalMS >= 0 && !math.IsNaN(a.LogicalMS) && !math.IsInf(a.LogicalMS, 0) {
			values = append(values, a.LogicalMS)
		}
	}
	slices.Sort(values)
	rank := int(math.Ceil(.95*float64(len(final)))) - 1
	if rank < 0 || rank >= len(values) {
		return unavailable("ms", "no observed p95 rank including all failures/censors")
	}
	return measured(values[rank], "ms")
}
