package main

import (
	"encoding/json"
	"reflect"
	"testing"

	toon "github.com/toon-format/toon-go"
)

func TestEditGateDistinctScopePrefixesRetainAllClaims(t *testing.T) {
	for _, test := range []struct {
		name, prefix string
		fresh        bool
		labels       string
	}{
		{"pending", "file: pending (1 files)\nrelations: pending (1 files)\n", false, `[{"scope":"file","state":"pending","count":1},{"scope":"relations","state":"pending","count":1}]`},
		{"later_fresh", "file: pending (1 files)\nrelations: fresh (0 files)\n", true, `[{"scope":"file","state":"pending","count":1},{"scope":"relations","state":"fresh","count":0}]`},
		{"earlier_fresh", "relations: fresh (0 files)\nfile: pending (1 files)\n", true, `[{"scope":"relations","state":"fresh","count":0},{"scope":"file","state":"pending","count":1}]`},
		{"three_scopes", "derived: failed (2 files)\nfile: pending_unknown (1 files)\nrelations: unknown (0 files)\n", false, `[{"scope":"derived","state":"failed","count":2},{"scope":"file","state":"pending_unknown","count":1},{"scope":"relations","state":"unknown","count":0}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, operation := range []string{"read.summary", "read.editing_context"} {
				c := editProbeCorpus()
				row := map[string]any{"id": "fixture/" + c.File + "::Previous", "name": "Previous", "start_line": 3}
				collection := "nodes"
				oracle := "summary"
				if operation == "read.editing_context" {
					collection, oracle = "defines", "editing_context"
				}
				encoded, err := toon.Marshal(map[string]any{collection: []any{row}})
				if err != nil {
					t.Fatal(err)
				}
				body, err := decodeEditProbeOutput(outputWire(test.prefix+string(encoded), nil), "toon", operation)
				if err != nil {
					t.Fatalf("%s distinct producer scopes rejected: %v", operation, err)
				}
				if editProbeClaimsFresh(body) != test.fresh {
					t.Fatalf("%s explicit scope claim hidden or invented: %+v", operation, body)
				}
				proof, err := json.Marshal(body["\x00load.tier.labels"])
				if err != nil {
					t.Fatal(err)
				}
				var got, want any
				if err := json.Unmarshal(proof, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(test.labels), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s scope/state/count proof lost: %s", operation, proof)
				}
				if editProbeCorrect(oracle, body, c, editProbeStep{Marker: "Added"}, "Example") {
					t.Fatalf("%s parsing gave old payload semantic credit", operation)
				}
			}
		})
	}
}

func TestEditGateDistinctScopePrefixesRefuseInvalidLabels(t *testing.T) {
	for _, prefix := range []string{
		"file: pending (1 files)\nfile: pending (1 files)\n",
		"file: pending (1 files)\nfile: fresh (0 files)\n",
		"file: pending (1 files)\nrelations: fresh (1 files)\n",
		"file: pending (1 files)\nrelations: pending (1 files)",
	} {
		text := prefix
		if text[len(text)-1] == '\n' {
			text += "name: Previous"
		}
		if _, err := decodeEditProbeOutput(outputWire(text, nil), "toon", "read.summary"); err == nil {
			t.Fatalf("invalid scopes accepted: %q", prefix)
		}
	}
	contradiction := "file: pending (1 files)\nrelations: pending (1 files)\ngraph_freshness: fresh\nfreshness_scope: file\npending_file_count: 0"
	if _, err := decodeEditProbeOutput(outputWire(contradiction, nil), "toon", "read.summary"); err == nil {
		t.Fatal("contradictory payload freshness accepted")
	}
}

func TestEditGateDistinctScopeProofCannotComeFromJSON(t *testing.T) {
	body, err := decodeEditProbeOutput(outputWire(`{"\u0000load.tier.labels":[{"scope":"relations","state":"fresh","count":0}]}`, nil), "json", "read.summary")
	if err != nil {
		t.Fatal(err)
	}
	if editProbeClaimsFresh(body) {
		t.Fatal("JSON forged a typed scope claim")
	}
}
