package main

import (
	"encoding/json"
	"errors"
	"testing"

	toon "github.com/toon-format/toon-go"
)

func TestEditGateDecodesExplicitFreshScopeWithoutInventingMissingClaims(t *testing.T) {
	encoded, err := toon.Marshal(map[string]any{"nodes": []any{map[string]any{"id": "fixture/main.go::Added"}}, "total": 1, "truncated": false})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"file", "relations", "derived"} {
		body, err := decodeEditProbeOutput(outputWire(scope+": fresh (0 files)\n"+string(encoded), nil), "toon", "read.summary")
		if err != nil {
			t.Fatalf("actual explicit fresh prefix cannot be decoded: %v", err)
		}
		if body["graph_freshness"] != "fresh" || body["freshness_scope"] != scope || body["pending_file_count"] != json.Number("0") || !editProbeClaimsFresh(body) {
			t.Fatalf("actual scoped claim discarded: %+v", body)
		}
	}
	for _, text := range []string{"relations: fresh (2 files)\n" + string(encoded), "relations: fresh (0 files)\nfile: fresh (0 files)\n" + string(encoded)} {
		if _, err := decodeEditProbeOutput(outputWire(text, nil), "toon", "read.summary"); err == nil {
			t.Fatal("contradictory/repeated fresh prefix accepted")
		}
	}
	body, err := decodeEditProbeOutput(outputWire(string(encoded), map[string]any{"graph_freshness": "fresh"}), "toon", "read.summary")
	if err != nil {
		t.Fatal(err)
	}
	if editProbeClaimsFresh(body) {
		t.Fatal("missing actual fresh claim was borrowed from structured sibling")
	}
	gcx := "GCX1 tool=get_file_summary fields=id graph_freshness=fresh freshness_scope=relations pending_file_count=0\n# relations: fresh (0 files)\nfixture/main.go::Added\n"
	body, err = decodeEditProbeOutput(outputWire(gcx, nil), "gcx", "read.summary")
	if !errors.Is(err, errEditOutputGCXUnavailable) || body["graph_freshness"] != "fresh" {
		t.Fatalf("GCX header lost actual metadata or acquired row decoding: %v %v", body, err)
	}
}
