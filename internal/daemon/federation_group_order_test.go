package daemon

import (
	"context"
	"encoding/json"
	"testing"
)

// TestFederator_GroupedCapIsSourceOrderIndependent pins that a capped
// grouped merge is a function of the row set, not of which peer
// answered first: two rows identical in every column except Context
// are distinct rows (the dedup key says so), so the comparator must
// order them by Context too — otherwise reversing equivalent peer
// inputs changes which row survives limit:1.
func TestFederator_GroupedCapIsSourceOrderIndependent(t *testing.T) {
	rowA := `{"grouped_by":"file","file_count":1,"total_uses":1,"truncated":false,"groups":[
		{"file":"pkg/a.go","count":1,"uses":[
			{"line":5,"edge_kind":"calls","context":"alpha()","symbol_id":"pkg/a.go::Use","symbol_name":"Use"}]}]}`
	rowB := `{"grouped_by":"file","file_count":1,"total_uses":1,"truncated":false,"groups":[
		{"file":"pkg/a.go","count":1,"uses":[
			{"line":5,"edge_kind":"calls","context":"beta()","symbol_id":"pkg/a.go::Use","symbol_name":"Use"}]}]}`

	survivor := func(t *testing.T, localJSON, remoteJSON string) string {
		t.Helper()
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
		out := testFederator().Augment(context.Background(), "find_usages",
			groupedProductionBody(`,"limit":1`), envelope(localJSON),
			[]ServerEntry{{Slug: "r2", URL: remote.URL}})
		tool, _ := unwrapToolJSON(out)
		var resp struct {
			Groups []struct {
				Uses []struct {
					Context string `json:"context"`
				} `json:"uses"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(tool, &resp); err != nil {
			t.Fatalf("unmarshal merged grouped response: %v", err)
		}
		if len(resp.Groups) != 1 || len(resp.Groups[0].Uses) != 1 {
			t.Fatalf("limit:1 must keep exactly one row, got %+v", resp.Groups)
		}
		return resp.Groups[0].Uses[0].Context
	}

	ab := survivor(t, rowA, rowB)
	ba := survivor(t, rowB, rowA)
	if ab != ba {
		t.Fatalf("capped grouped output depends on source order: local-first %q vs reversed %q", ab, ba)
	}
}
