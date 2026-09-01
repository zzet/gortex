package respbudget

import (
	"encoding/json"
	"strings"
	"testing"
)

type typedEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Kind    string `json:"kind"`
	Context string `json:"context,omitempty"`
}

// TestApplyTrimsTypedSlices pins the normalization contract: Apply must
// trim a map payload whose lists are TYPED Go slices ([]*graph.Edge in
// the live get_symbol detail:"full" path), not only pre-normalized
// []any values. A fast path that inspects the caller's map directly
// sees no []any and silently returns the oversized payload.
func TestApplyTrimsTypedSlices(t *testing.T) {
	edges := make([]typedEdge, 40)
	for i := range edges {
		edges[i] = typedEdge{
			From:    "pkg/service.go::Handler",
			To:      "pkg/store.go::Query",
			Kind:    "calls",
			Context: strings.Repeat("ctx", 20),
		}
	}
	payload := map[string]any{
		"node":      map[string]any{"id": "pkg/service.go::Handler", "kind": "function"},
		"out_edges": edges,
		"in_edges":  edges[:10],
	}

	const cap = 600
	got, trimmed := Apply(payload, cap)
	if !trimmed {
		t.Fatalf("Apply did not trim a payload of typed slices over the cap")
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal trimmed payload: %v", err)
	}
	if len(out) > cap {
		t.Fatalf("trimmed payload is %d bytes, over the %d cap", len(out), cap)
	}
	if !Trimmed(out) {
		t.Fatalf("trimmed payload does not carry %s", TruncatedKey)
	}
	// The caller's map must stay untouched: handlers reuse payloads.
	if len(payload["out_edges"].([]typedEdge)) != 40 {
		t.Fatalf("Apply mutated the caller's payload map")
	}
}
