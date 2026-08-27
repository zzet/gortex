package respbudget

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestApplyScalarSkeletonFloor pins the documented budget floor: a cap
// smaller than the payload's scalar skeleton empties every list and
// stamps the truncation meta, and the result lands exactly on the
// skeleton — never above it, never a corrupted byte-cut below it.
func TestApplyScalarSkeletonFloor(t *testing.T) {
	payload := map[string]any{
		"summary": strings.Repeat("scalar text the trim must not cut ", 8),
		"items":   []any{"row-one", "row-two", "row-three"},
		"total":   3,
	}

	const cap = 100
	got, trimmed := Apply(payload, cap)
	if !trimmed {
		t.Fatalf("a cap below the skeleton still trims (empties) the lists")
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Apply returns the generic map on trim, got %T", got)
	}
	if items, _ := m["items"].([]any); len(items) != 0 {
		t.Fatalf("every list must be emptied below the floor, items kept %d rows", len(items))
	}
	if m[TruncatedKey] != true {
		t.Fatalf("the truncation meta must be stamped even below the floor")
	}

	// The floor itself: the same payload with the list emptied and the
	// meta stamped. The trimmed result must marshal to exactly that —
	// the minimum honest representation, nothing more.
	floor := map[string]any{
		"summary":               payload["summary"],
		"items":                 []any{},
		"total":                 3,
		TruncatedKey:            true,
		"_max_returned_items":   0,
		"_original_count_items": 3,
	}
	floorBytes, err := json.Marshal(floor)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBytes) != len(floorBytes) {
		t.Fatalf("below the cap the result must land on the scalar-skeleton floor: got %d bytes, floor is %d", len(gotBytes), len(floorBytes))
	}
	if len(gotBytes) <= cap {
		t.Fatalf("fixture must exercise the below-floor case: floor %d fits cap %d", len(gotBytes), cap)
	}
}
