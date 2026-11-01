package main

import (
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
)

// Every oracle reports into exactly one visibility group: the edited file's
// own graph, its call relations, or search.
var editProbeOracleGroups = map[string]string{
	"source": "file_graph", "summary": "file_graph", "editing_context": "file_graph", "old_source_absent": "file_graph",
	"callers": "relations", "call_chain": "relations",
	"search_visible": "search", "text": "search",
}

// A group is visible when its slowest member first answered correctly; a
// member that never did leaves the group unavailable rather than faster.
func editProbeGroupTime(firstCorrect map[string]Metric, group string) Metric {
	var latest *float64
	members := 0
	for _, name := range slices.Sorted(maps.Keys(firstCorrect)) {
		if editProbeOracleGroups[name] != group {
			continue
		}
		members++
		value := firstCorrect[name].Value
		if value == nil {
			return unavailable("ms", name+" never answered correctly")
		}
		if latest == nil || *value > *latest {
			latest = value
		}
	}
	if members == 0 {
		return unavailable("ms", "cell has no "+group+" oracle")
	}
	return measured(*latest, "ms")
}

// The derived step runs after every client-visible answer can already be
// correct, so no probe observes it. It is timed on the daemon's own clock,
// from the receipt origin to patch_derived_finished in this record's own
// publication timeline, and is not additive with client-side times. A
// coalesced receipt's derived step ran in the serving publication's timeline.
func editProbeDerived(status map[string]any) Metric {
	if _, coalesced := status["coalesced_publication"]; coalesced {
		return unavailable("ms", "coalesced receipt: derived ran in the serving publication's timeline")
	}
	timeline, ok := status["publication_phases"].(map[string]any)
	if !ok {
		return unavailable("ms", "status exposes no publication_phases")
	}
	if timeline["clock"] != "monotonic" || timeline["terminal"] != true {
		return unavailable("ms", "publication timeline is not terminal on a monotonic clock")
	}
	phases, _ := timeline["phases"].([]any)
	offsets := map[string]int64{}
	for _, entry := range phases {
		row, _ := entry.(map[string]any)
		name, _ := row["phase"].(string)
		if name != "received" && name != "patch_derived_finished" {
			continue
		}
		ns, err := strconv.ParseInt(fmt.Sprint(row["offset_ns"]), 10, 64)
		if _, repeated := offsets[name]; err != nil || repeated {
			return unavailable("ms", "invalid or repeated "+name+" offset")
		}
		offsets[name] = ns
	}
	origin, received := offsets["received"]
	finished, derived := offsets["patch_derived_finished"]
	switch {
	case !received || !derived:
		return unavailable("ms", "publication timeline has no received/derived pair")
	case finished < origin:
		return unavailable("ms", "derived step precedes receipt origin")
	}
	return measured(float64(finished-origin)/1e6, "ms")
}

const editProbeBootstrapResamples = 2000

type editProbeInterval struct {
	Low  Metric `json:"low"`
	High Metric `json:"high"`
}

// Nearest-rank quantiles (the same rule as Distribution), each with a seeded
// percentile-bootstrap 95% interval. A censored record ended (deadline, run
// end or failure) before its value was observed: it ranks above every
// observed value, so a quantile whose rank falls among censored records is
// unavailable rather than understated. A not-applicable record cannot carry
// the value. Neither is ever sampled as zero.
type editProbeQuantiles struct {
	Count         int               `json:"count"`
	Censored      int               `json:"censored"`
	NotApplicable int               `json:"not_applicable"`
	P50           Metric            `json:"p50"`
	P95           Metric            `json:"p95"`
	P99           Metric            `json:"p99"`
	Max           Metric            `json:"max"`
	P50CI         editProbeInterval `json:"p50_ci95"`
	P95CI         editProbeInterval `json:"p95_ci95"`
	P99CI         editProbeInterval `json:"p99_ci95"`
	Resamples     int               `json:"bootstrap_resamples"`
}

type editProbeSummary struct {
	Offered  int                           `json:"offered"`
	Outcomes map[string]int                `json:"outcomes"`
	Measures map[string]editProbeQuantiles `json:"measures"`
}

func editProbeRank(q float64, n int) int { return max(0, int(math.Ceil(q*float64(n)))-1) }

// The resampling stream depends only on the plan seed and the measure name, so
// a report is reproducible and one measure's interval cannot shift another's.
// Censored records join the ranking as +Inf. A quantile needs at least
// ceil(1/(1-q)) ranked records before its nearest rank falls below the sample
// maximum; with fewer its interval is unavailable.
func editProbeQuantilesOf(values []float64, censored, notApplicable int, unit string, seed int64, measure string) editProbeQuantiles {
	ranked := append(slices.Clone(values), slices.Repeat([]float64{math.Inf(1)}, censored)...)
	beyond := unavailable(unit, fmt.Sprintf("nearest rank falls among %d censored records", censored))
	bounded := func(m Metric) Metric {
		if m.Value != nil && math.IsInf(*m.Value, 1) {
			return beyond
		}
		return m
	}
	d := distribution(ranked, unit)
	out := editProbeQuantiles{Count: len(values), Censored: censored, NotApplicable: notApplicable, P50: bounded(d.P50), P95: bounded(d.P95), P99: bounded(d.P99), Max: bounded(d.Max)}
	quantiles := []float64{.50, .95, .99}
	minimum := []int{2, 20, 100} // ceil(1/(1-q))
	intervals := []*editProbeInterval{&out.P50CI, &out.P95CI, &out.P99CI}
	estimates := make([][]float64, len(quantiles))
	for i := range quantiles {
		if len(ranked) < minimum[i] {
			reason := fmt.Sprintf("bootstrap interval needs at least %d observations", minimum[i])
			*intervals[i] = editProbeInterval{Low: unavailable(unit, reason), High: unavailable(unit, reason)}
			continue
		}
		estimates[i] = make([]float64, 0, editProbeBootstrapResamples)
	}
	if estimates[0] == nil {
		return out
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(measure))
	rng := rand.New(rand.NewPCG(uint64(seed), hash.Sum64()))
	sample := make([]float64, len(ranked))
	for range editProbeBootstrapResamples {
		for i := range sample {
			sample[i] = ranked[rng.IntN(len(ranked))]
		}
		slices.Sort(sample)
		for i, q := range quantiles {
			if estimates[i] != nil {
				estimates[i] = append(estimates[i], sample[editProbeRank(q, len(sample))])
			}
		}
	}
	for i := range quantiles {
		if estimates[i] == nil {
			continue
		}
		slices.Sort(estimates[i])
		*intervals[i] = editProbeInterval{Low: bounded(measured(estimates[i][editProbeRank(.025, len(estimates[i]))], unit)), High: bounded(measured(estimates[i][editProbeRank(.975, len(estimates[i]))], unit))}
	}
	out.Resamples = editProbeBootstrapResamples
	return out
}

// One summary over the complete offered denominator. A completed record lacks
// only values that do not apply to it (a native write's ACK, a coalesced
// receipt's derived time); any other record ended before its missing values
// were observed, so they are censored. A measure a record does not carry at
// all is not applicable to it.
func summarizeEditProbe(records []editProbeRecord, seed int64) *editProbeSummary {
	out := &editProbeSummary{Offered: len(records), Outcomes: map[string]int{}, Measures: map[string]editProbeQuantiles{}}
	type measure struct {
		observed []float64
		censored int
	}
	values := map[string]*measure{}
	for _, record := range records {
		out.Outcomes[record.Outcome]++
		add := func(name string, metric Metric) {
			m := values[name]
			if m == nil {
				m = &measure{}
				values[name] = m
			}
			switch {
			case metric.Value != nil:
				m.observed = append(m.observed, *metric.Value)
			case record.Outcome != "complete":
				m.censored++
			}
		}
		add("t_ack_ms", record.Ack)
		add("t_readback_ms", record.Readback)
		add("t_file_graph_ms", record.FileGraph)
		add("t_relations_ms", record.Relations)
		add("t_search_ms", record.Search)
		add("t_derived_ms", record.Derived)
		for _, readback := range record.Readbacks {
			add("readback_ms."+readback.Kind, readback.Latency)
		}
		for name, metric := range record.PhasesMS {
			add("phases_ms."+name, metric)
		}
		for name, metric := range record.FirstCorrect {
			add("first_correct_ms."+name, metric)
		}
	}
	for name, m := range values {
		out.Measures[name] = editProbeQuantilesOf(m.observed, m.censored, len(records)-len(m.observed)-m.censored, "ms", seed, name)
	}
	return out
}
