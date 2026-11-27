package main

import (
	"encoding/json"
	"errors"
	"math"
)

// editProbeTierAfterCommit is the daemon's own publication latency for one
// edit: milliseconds from the disk commit to each tier's publication, on the
// daemon's monotonic clock, copied from the last receipt status the record
// observed. A tier the daemon had not published by then is null. It sits
// beside the client times (t_file_graph_ms, t_relations_ms, t_derived_ms) and
// replaces none of them.
type editProbeTierAfterCommit struct {
	FileGraph *float64 `json:"file_graph"`
	Relations *float64 `json:"relations"`
	Derived   *float64 `json:"derived"`
}

// editProbeStatusTierAfterCommit reads tier_after_commit_ms from one receipt
// status. A status without it yields nil: the daemon predates the field, or
// the receipt has no commit time or tier snapshot. Each tier must be present
// as a finite number or null; anything else is an error, never a guess.
func editProbeStatusTierAfterCommit(status map[string]any) (*editProbeTierAfterCommit, error) {
	raw, present := status["tier_after_commit_ms"]
	if !present {
		return nil, nil
	}
	tiers, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("tier_after_commit_ms is not an object")
	}
	out := &editProbeTierAfterCommit{}
	for _, tier := range []struct {
		name   string
		target **float64
	}{{"file_graph", &out.FileGraph}, {"relations", &out.Relations}, {"derived", &out.Derived}} {
		value, present := tiers[tier.name]
		if !present {
			return nil, errors.New("tier_after_commit_ms has no " + tier.name + " tier")
		}
		if value == nil {
			continue
		}
		var ms float64
		var err error
		switch number := value.(type) {
		case json.Number:
			ms, err = number.Float64()
		case float64:
			ms = number
		default:
			err = errors.New("not a number")
		}
		if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) {
			return nil, errors.New("tier_after_commit_ms " + tier.name + " is not a finite number or null")
		}
		*tier.target = &ms
	}
	return out, nil
}

// summarizeEditProbeTierAfterCommit adds the daemon tier latencies to the
// summary as tier_after_commit_ms.<tier>, beside the client measures. A tier
// null on a record's final receipt had not been published when the record
// stopped observing, so it is censored even for a completed edit. A record
// without the figure is censored unless it completed; a completed one (a
// native write, a batch or a relocation) does not carry it. Runs where no
// record carries the figure, such as daemons without it, add nothing.
func summarizeEditProbeTierAfterCommit(out *editProbeSummary, records []editProbeRecord, seed int64) {
	carried := false
	for _, record := range records {
		if record.TierAfterCommit != nil {
			carried = true
			break
		}
	}
	if !carried {
		return
	}
	for _, tier := range []string{"file_graph", "relations", "derived"} {
		var observed []float64
		censored, notApplicable := 0, 0
		for _, record := range records {
			figure := record.TierAfterCommit
			if figure == nil {
				if record.Outcome == "complete" {
					notApplicable++
				} else {
					censored++
				}
				continue
			}
			value := map[string]*float64{"file_graph": figure.FileGraph, "relations": figure.Relations, "derived": figure.Derived}[tier]
			if value == nil {
				censored++
				continue
			}
			observed = append(observed, *value)
		}
		name := "tier_after_commit_ms." + tier
		out.Measures[name] = editProbeQuantilesOf(observed, censored, notApplicable, "ms", seed, name)
	}
}
