package main

import "encoding/json"

const partialPendingMetadata = "partial_pending"

func metadataObservationTool(tool string) bool {
	return tool == "read.summary" || tool == "read.editing_context"
}

// This observes only explicit flags in the one declared successful JSON body.
// Missing/unparseable/other-format data is UNOBSERVED, never complete metadata.
// The existing strict ordered-note decoder supplies the same payload boundary.
func observePartialMetadata(r request, args map[string]any, role string, raw json.RawMessage) string {
	if role != "workload" || r.Tool != "read" || args["operation"] != r.Operation || !metadataObservationTool(r.name()) {
		return ""
	}
	payload, err := editOutputPayload(raw, false)
	if err != nil {
		return ""
	}
	var body struct {
		Freshness *struct {
			Complete *bool  `json:"complete"`
			State    string `json:"graph_freshness"`
		} `json:"freshness"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Freshness == nil || body.Freshness.Complete == nil || *body.Freshness.Complete || body.Freshness.State != "pending" {
		return ""
	}
	return partialPendingMetadata
}
