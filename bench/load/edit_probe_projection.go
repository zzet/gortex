package main

import (
	"context"
	"encoding/json"
	"time"
)

// An edit-and-probe report bookends the live pending-projection counter that
// gortex://index-health adds after its cached base payload, beside
// stats_before and stats_after. Each bookend is one five-second resources/read
// on the run-level editor connection, outside the offered edits and their
// immediate readbacks. A missing or null section, a missing or malformed count
// and a failed call stay unavailable: none is ever reported as zero.
type editProbeProjectionBookend struct {
	ObservedAt *time.Time      `json:"observed_at,omitempty"`
	WireID     int64           `json:"wire_id,omitempty"`
	Divergence Metric          `json:"projection_divergence"`
	Section    json.RawMessage `json:"pending_projection,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// New reports always carry both bookends; an older report without them has
// no counter evidence, which is equally unavailable.
func unobservedProjectionBookend() *editProbeProjectionBookend {
	return &editProbeProjectionBookend{Divergence: unavailable("count", "index-health bookend not observed")}
}

func (r *editProbeRuntime) projectionBookend(ctx context.Context) *editProbeProjectionBookend {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := unobservedProjectionBookend()
	observed := time.Now().UTC()
	out.ObservedAt = &observed
	raw, identity, err := r.editors[0].callWithIdentity(ctx, "resources/read", map[string]any{"uri": "gortex://index-health"})
	out.WireID = identity.ID
	if err != nil {
		out.Error = err.Error()
		out.Divergence = unavailable("count", "index-health resource call failed")
		return out
	}
	out.Divergence = projectionCounter(raw)
	out.Section = pendingProjectionSection(raw)
	return out
}

func pendingProjectionSection(raw json.RawMessage) json.RawMessage {
	payload, err := textPayload(raw, "contents")
	if err != nil {
		return nil
	}
	var body struct {
		Pending json.RawMessage `json:"pending_projection"`
	}
	if json.Unmarshal(payload, &body) != nil || len(body.Pending) == 0 || string(body.Pending) == "null" {
		return nil
	}
	return body.Pending
}

func projectionCounter(raw json.RawMessage) Metric {
	payload, err := textPayload(raw, "contents")
	if err != nil {
		return unavailable("count", "index-health resource payload unavailable")
	}
	var body struct {
		Pending json.RawMessage `json:"pending_projection"`
	}
	if json.Unmarshal(payload, &body) != nil || len(body.Pending) == 0 || string(body.Pending) == "null" {
		return unavailable("count", "pending_projection section absent; never inferred zero")
	}
	var section map[string]json.RawMessage
	if json.Unmarshal(body.Pending, &section) != nil {
		return unavailable("count", "pending_projection section malformed")
	}
	var count json.Number
	value := section["projection_divergence"]
	if len(value) == 0 || value[0] < '0' || value[0] > '9' || json.Unmarshal(value, &count) != nil {
		return unavailable("count", "projection_divergence absent or malformed")
	}
	n, err := count.Int64()
	if err != nil || n < 0 || n > 1<<53-1 {
		return unavailable("count", "projection_divergence is not an exact nonnegative integer")
	}
	return measured(float64(n), "count")
}
