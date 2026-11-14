package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func editProbeContextFixture() (Corpus, editProbeStep, map[string]any) {
	c := editProbeCorpus()
	c.File = "internal/graph/store_sqlite/store.go"
	c.Symbol = c.File + "::Open"
	c.EditKind = "add_intrafile_call"
	step := editProbeStep{Marker: "GortexEditProbe0000000000000001N008"}
	// A source response includes context, not necessarily a complete Go file.
	// This exact import-tail shape occurred in a retained paired readback.
	source := "\t_ \"modernc.org/sqlite\"\n)\n\n" +
		"func " + step.Marker + "() { _, _ = Open(\"\") }\n\n" +
		"func GortexEditProbe0000000000000001N006() { _, _ = Open(\"\") }\n"
	body := map[string]any{
		"name": step.Marker, "id": "gortex/" + c.File + "::" + step.Marker,
		"file_path": "gortex/" + c.File, "source": source,
		"from_line": json.Number("39"), "start_line": json.Number("42"), "end_line": json.Number("42"),
	}
	return c, step, body
}

func TestEditProbeSourceContextUsesAdvertisedDeclaration(t *testing.T) {
	c, step, body := editProbeContextFixture()
	if !editProbeCorrect("source", body, c, step, "Open") {
		t.Fatal("correct target declaration rejected because surrounding context starts inside an import block")
	}
}

func TestEditProbeSourceContextRejectsWrongOrIncompleteEvidence(t *testing.T) {
	changes := map[string]func(map[string]any){
		"missing_from":         func(b map[string]any) { delete(b, "from_line") },
		"missing_end":          func(b map[string]any) { delete(b, "end_line") },
		"zero_from":            func(b map[string]any) { b["from_line"] = json.Number("0") },
		"negative_start":       func(b map[string]any) { b["start_line"] = json.Number("-1") },
		"fractional_start":     func(b map[string]any) { b["start_line"] = json.Number("42.5") },
		"wrong_numeric_type":   func(b map[string]any) { b["start_line"] = float64(42) },
		"start_before_context": func(b map[string]any) { b["start_line"] = json.Number("38") },
		"reversed_span":        func(b map[string]any) { b["end_line"] = json.Number("41") },
		"outside_context":      func(b map[string]any) { b["end_line"] = json.Number("99") },
		"neighbor_instead":     func(b map[string]any) { b["start_line"], b["end_line"] = json.Number("44"), json.Number("44") },
		"includes_neighbor":    func(b map[string]any) { b["end_line"] = json.Number("44") },
		"wrong_name":           func(b map[string]any) { b["name"] = "Another" },
		"wrong_path":           func(b map[string]any) { b["file_path"] = "other.go" },
		"wrong_id":             func(b map[string]any) { b["id"] = "ext::Open" },
		"missing_id":           func(b map[string]any) { delete(b, "id") },
		"wrong_callee": func(b map[string]any) {
			b["source"] = strings.ReplaceAll(b["source"].(string), "Open(\"\")", "Other(\"\")")
		},
		"callee_only_in_neighbor": func(b map[string]any) {
			b["source"] = strings.Replace(b["source"].(string), "Open(\"\")", "Other(\"\")", 1)
		},
		"truncated_target": func(b map[string]any) {
			b["source"] = "\t_ \"modernc.org/sqlite\"\n)\n\nfunc GortexEditProbe0000000000000001N008() { _, _ = Open(\"\")"
		},
		"pending":               func(b map[string]any) { b["complete"], b["source_state"] = false, "pending"; delete(b, "source") },
		"pending_with_body":     func(b map[string]any) { b["source_state"] = "pending" },
		"unavailable_with_body": func(b map[string]any) { b["source_state"] = "unavailable" },
		"incomplete_with_body":  func(b map[string]any) { b["complete"] = false },
		"truncated_flag":        func(b map[string]any) { b["truncated"] = true },
		"bodies_elided":         func(b map[string]any) { b["bodies_elided"] = true },
		"salience_truncated":    func(b map[string]any) { b["salience_truncated"] = true },
		"budget_truncated":      func(b map[string]any) { b["_truncated_by_budget"] = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c, step, body := editProbeContextFixture()
			change(body)
			if editProbeCorrect("source", body, c, step, "Open") {
				t.Fatal("wrong, partial, unavailable or unbound source evidence accepted")
			}
		})
	}
}

func TestEditProbeSourceContextPreservesMetadataAbsentFixture(t *testing.T) {
	c, step, body := editProbeContextFixture()
	for _, key := range []string{"from_line", "start_line", "end_line"} {
		delete(body, key)
	}
	body["source"] = "func " + step.Marker + "() { _, _ = Open(\"\") }"
	if !editProbeCorrect("source", body, c, step, "Open") {
		t.Fatal("legacy complete-declaration fixture without range metadata changed")
	}
}
