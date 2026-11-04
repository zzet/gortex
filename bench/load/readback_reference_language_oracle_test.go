package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func readbackLanguageOracleBodies(image readbackReferenceImage, target string) map[string]map[string]any {
	nodes, defines := []any{}, []any{}
	var selected map[string]any
	for name, symbol := range image.Symbols {
		row := map[string]any{"id": readbackReferenceID(image, name), "name": name, "kind": "function", "file_path": image.File, "start_line": symbol.StartLine, "end_line": symbol.EndLine}
		nodes = append(nodes, row)
		defines = append(defines, map[string]any{"id": row["id"], "name": name, "kind": "function", "start_line": symbol.StartLine})
		if name == target {
			selected = row
		}
	}
	for suffix, definition := range image.Auxiliary {
		defines = append(defines, map[string]any{"id": image.Prefix + "/" + image.File + "::" + suffix, "name": definition.Name, "kind": definition.Kind, "start_line": definition.StartLine})
	}
	bodies := map[string]map[string]any{
		"summary":         {"nodes": nodes, "total_nodes": len(nodes), "truncated": false},
		"editing_context": {"file": map[string]any{"id": image.Prefix + "/" + image.File}, "defines": defines},
		"file":            {"path": image.Prefix + "/" + image.File, "content": image.Bytes},
	}
	if selected != nil {
		bodies["source"] = map[string]any{"id": selected["id"], "name": target, "source": image.Symbols[target].Body, "start_line": selected["start_line"], "end_line": selected["end_line"]}
		bodies["symbol"] = map[string]any{"node": selected, "in_edges": []any{}, "out_edges": []any{}}
	}
	return bodies
}

func TestReadbackReferenceForeignLiteralImagesKeepStrictReadOracles(t *testing.T) {
	for _, language := range []string{"typescript", "python"} {
		for kind, tools := range readbackReferenceTools {
			if kind == "relocate" {
				continue
			}
			t.Run(language+"/"+kind, func(t *testing.T) {
				cell := readbackReferenceCell{Language: language, Agents: 1, Kind: kind, Tool: tools[0], Offers: 100}
				trial, err := readbackReferenceTrialFor(cell, editProbeArrival{}, 0)
				require.NoError(t, err)
				image := trial.After[trial.ProbeFile]
				image.Prefix = "owned"
				bodies := readbackLanguageOracleBodies(image, trial.Target)
				for endpoint, body := range bodies {
					require.True(t, readbackReferenceCorrect(endpoint, readbackReferenceWire(body), image, trial.Target, trial.Absent), endpoint)
				}
				if trial.Absent {
					absent := absentSourceWire("symbol not found: " + readbackReferenceID(image, trial.Target))
					require.True(t, readbackReferenceCorrect("source", absent, image, trial.Target, true))
					require.False(t, readbackReferenceCorrect("source", absentSourceWire("projection not published: superseded"), image, trial.Target, true))
				} else {
					bodies["source"]["source"] = "wrong current body with correct name and span"
					require.False(t, readbackReferenceCorrect("source", readbackReferenceWire(bodies["source"]), image, trial.Target, false))
				}
				// Public defines have starts, not mandatory end_line fields.
				// Neither a stale neighbor nor a foreign ID may substitute.
				stable := "GortexReadback000Stable"
				for _, raw := range bodies["editing_context"]["defines"].([]any) {
					row := raw.(map[string]any)
					if row["name"] == stable {
						row["start_line"] = image.Symbols[stable].StartLine + 1
					}
				}
				require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(bodies["editing_context"]), image, trial.Target, trial.Absent))
				bodies = readbackLanguageOracleBodies(image, trial.Target)
				nodes := bodies["summary"]["nodes"].([]any)
				nodes[0].(map[string]any)["id"] = "foreign/" + image.File + "::" + nodes[0].(map[string]any)["name"].(string)
				require.False(t, readbackReferenceCorrect("summary", readbackReferenceWire(bodies["summary"]), image, trial.Target, trial.Absent))
				if kind == "modify_body" {
					before := trial.Before[trial.ProbeFile]
					require.Greater(t, image.Symbols[stable].StartLine, before.Symbols[stable].StartLine)
					require.Greater(t, image.Symbols[trial.Target].EndLine, before.Symbols[trial.Target].EndLine)
				}
			})
		}
	}
}

func TestReadbackReferenceForeignSelectionRefusesUnsupportedCapabilities(t *testing.T) {
	for _, language := range []string{"typescript", "python"} {
		cell := readbackReferenceCell{Language: language, Agents: 1, Kind: "relocate", Tool: "refactor.move", Offers: 100}
		_, err := readbackReferenceTrialFor(cell, editProbeArrival{}, 0)
		require.ErrorContains(t, err, "Go-only")
	}
	_, err := readbackReferenceLanguageExtension("rust")
	require.Error(t, err, "six-language projection parity does not expand the literal three-language measurement matrix")
	_, err = readbackReferenceLiteralImage("p.ts", "", []readbackReferenceLiteralFunction{{name: "Duplicate", source: "export function Duplicate(): number { return 1; }"}, {name: "Duplicate", source: "export function Duplicate(): number { return 2; }"}})
	require.Error(t, err)
	_, err = readbackReferenceLiteralImage("p.py", "", []readbackReferenceLiteralFunction{{name: "not-a-name", source: "def Wrong():\n    return 7"}})
	require.Error(t, err)
}
