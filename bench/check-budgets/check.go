package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type result struct {
	Metric            string   `json:"metric"`
	Scenario          string   `json:"scenario"`
	HostClass         string   `json:"host_class"`
	Mode              string   `json:"mode"`
	OwningRelease     string   `json:"owning_release"`
	FutureTarget      *float64 `json:"declared_block_target"`
	Unit              string   `json:"unit"`
	Before            *float64 `json:"before"`
	After             *float64 `json:"after"`
	Worst             *float64 `json:"worst_hard_repeat"`
	Limit             *float64 `json:"limit"`
	EffectiveHeadroom *float64 `json:"effective_added_headroom"`
	Baseline          string   `json:"baseline"`
	Allowances        []string `json:"allowances"`
	Pass              bool     `json:"pass"`
	Reason            string   `json:"reason"`
}

func median(values []float64) (float64, error) {
	if len(values) < 3 {
		return 0, errors.New("at least three repeats required")
	}
	values = slices.Clone(values)
	for _, v := range values {
		if !finiteNonnegative(v) {
			return 0, errors.New("invalid repeat value")
		}
	}
	slices.Sort(values)
	i := len(values) / 2
	if len(values)%2 == 1 {
		return values[i], nil
	}
	return values[i-1]/2 + values[i]/2, nil
}

func assess(p policy, b budget, candidate, main, tag []float64, lane string) result {
	r := result{Metric: b.Metric, Scenario: b.Scenario, HostClass: b.HostClass, Mode: b.Mode, OwningRelease: b.OwningRelease, FutureTarget: b.Target, Unit: b.Unit, Allowances: slices.Clone(b.Allowance)}
	after, err := median(candidate)
	if err != nil {
		r.Reason = "candidate: " + err.Error()
		return r
	}
	before, err := median(main)
	if err != nil {
		r.Reason = "main: " + err.Error()
		return r
	}
	r.Before, r.After, r.Baseline = &before, &after, "main (comparison only)"
	limit := float64(0)
	if b.Mode == "block" {
		limit = *b.Target
		zero := float64(0)
		r.EffectiveHeadroom = &zero
		for i := range r.Allowances {
			r.Allowances[i] += " (guard-only; not applied in block mode)"
		}
	} else {
		shipped, err := median(tag)
		if err != nil {
			r.Reason = "v0.64.10: " + err.Error()
			return r
		}
		chosen, origin := shipped, "v0.64.10"
		axis := metricSpecs[b.Metric].axis
		if (axis == "disk" || axis == "memory") && before > shipped+shipped*p.Baseline.NoiseFraction {
			chosen, origin = before, "main (shipped disk/memory baseline exceeded by >15%)"
		}
		r.Before, r.Baseline = &chosen, origin
		noiseAdjusted := chosen + chosen*p.Baseline.NoiseFraction
		limit = noiseAdjusted
		floor := float64(0)
		for index, name := range b.Allowance {
			for _, a := range p.Allowances {
				if a.Name != name {
					continue
				}
				if a.Kind == "additive" {
					limit += a.Value
				} else {
					floor = max(floor, a.Value)
				}
				r.Allowances[index] = fmt.Sprintf("%s: %s %g %s (%s)", a.Name, a.Kind, a.Value, a.Unit, a.OwningRelease)
			}
		}
		limit = max(limit, floor)
		added := limit - noiseAdjusted
		r.EffectiveHeadroom = &added
	}
	if !finiteNonnegative(limit) {
		r.Reason = "nonfinite effective limit"
		return r
	}
	r.Limit = &limit
	compare := func(value float64) bool {
		switch b.Compare {
		case "le":
			return value <= limit
		case "lt":
			return value < limit
		case "ge":
			return value >= limit
		case "eq":
			return value == limit
		default:
			return false
		}
	}
	r.Pass = compare(after)
	if metricSpecs[b.Metric].everyRepeat {
		worst := candidate[0]
		for _, v := range candidate {
			r.Pass = r.Pass && compare(v)
			if b.Compare == "ge" {
				worst = min(worst, v)
			} else {
				worst = max(worst, v)
			}
		}
		r.Worst = &worst
	}
	if !r.Pass {
		r.Reason = "absolute/guard limit breached (hard maxima and safety counters apply to every repeat)"
	}
	if lane == "smoke" && strings.HasSuffix(b.Metric, "_p95") && after > before+before*.20 {
		r.Pass, r.Reason = false, "smoke p95 is >20% worse than main median"
	}
	return r
}

func (d dataset) samples(role string, b budget) ([]float64, []report, error) {
	var values []float64
	var reports []report
	planned := d.config.repeats(b.Scenario)
	if planned < max(3, b.MinRepeats) {
		return nil, nil, fmt.Errorf("%s needs at least %d planned repeats", b.Scenario, max(3, b.MinRepeats))
	}
	for repeat := 1; repeat <= planned; repeat++ {
		var found *report
		for _, run := range d.runs {
			if run.input.Role == role && run.input.HostClass == b.HostClass && run.input.Scenario == b.Scenario && run.input.Repeat == repeat {
				copy := run.data
				found = &copy
				break
			}
		}
		if found == nil {
			return nil, nil, fmt.Errorf("%s missing planned repeat %d", role, repeat)
		}
		v, err := extract(b.Metric, b.HostClass, *found)
		if b.Metric == "read_p95_vs_load8" || b.Metric == "read_p95_vs_main" {
			// Ratio of medians, not pooled raw latencies or mismatched tools.
			reference := b
			reference.Metric = "read_p95"
			baseRole := role
			if b.Metric == "read_p95_vs_load8" {
				reference.Scenario = "load-8"
			} else {
				baseRole = "main"
			}
			base, referenceReports, baseErr := d.samples(baseRole, reference)
			denominator, medianErr := median(base)
			v, err = readP95(found.Phases[0])
			if baseErr != nil || medianErr != nil || denominator <= 0 || len(referenceReports) != planned {
				return nil, nil, errors.New("missing comparable reference read p95 denominator")
			}
			matched := comparable(*found, referenceReports[repeat-1])
			if b.Metric == "read_p95_vs_load8" {
				matched = comparableReadLoads(*found, referenceReports[repeat-1])
			}
			if !matched {
				return nil, nil, errors.New("load-8 reference host/corpus/workload differs beyond agent count")
			}
			v /= denominator
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s repeat %d: %w", role, repeat, err)
		}
		if len(reports) > 0 && !comparableRepeats(reports[0], *found) {
			return nil, nil, errors.New("repeat host/corpus/workload identities are incomparable")
		}
		values, reports = append(values, v), append(reports, *found)
	}
	return values, reports, nil
}

func check(p policy, d dataset, lane string) ([]result, error) {
	if d.config.Hosts[hostedSmokeClass] != "" && lane != "smoke" {
		return nil, errors.New("hosted ARM smoke evidence cannot satisfy nightly or release reference capacity")
	}
	if !slices.Contains([]string{"smoke", "nightly", "release"}, lane) {
		return nil, errors.New("lane must be smoke, nightly or release")
	}
	var out []result
	for _, b := range p.Budgets {
		if !slices.Contains(b.Lane, lane) || d.config.Hosts[b.HostClass] == "" {
			continue
		}
		candidate, cr, ce := d.samples("candidate", b)
		main, mr, me := d.samples("main", b)
		var tag []float64
		var tr []report
		var te error
		if b.Mode == "guard" {
			tag, tr, te = d.samples("tag", b)
		}
		row := assess(p, b, candidate, main, tag, lane)
		if ce != nil || me != nil || te != nil {
			row.Pass, row.Reason = false, fmt.Sprintf("missing evidence: candidate=%v main=%v tag=%v", ce, me, te)
		} else {
			for i := range cr {
				if !comparable(cr[i], mr[i]) || (b.Mode == "guard" && !comparable(cr[i], tr[i])) {
					row.Pass, row.Reason = false, "baseline and candidate host/corpus/workload identities differ in a matching repeat"
				}
			}
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, errors.New("no budget rows selected for the requested lane and reviewed host classes")
	}
	return out, nil
}
