package main

import (
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strings"
)

type searchReadbackCounts struct {
	Observed int `json:"observed"`
	Correct  int `json:"correct"`
	Errors   int `json:"errors"`
	Refusals int `json:"refusals"`
	Missing  int `json:"missing"`
}

type searchReadbackAcceptance struct {
	LocalObjectACKs int                             `json:"local_object_acks"`
	ACKModes        map[string]int                  `json:"actual_ack_modes"`
	FileViewStates  map[string]int                  `json:"actual_file_view_states"`
	Offered         int                             `json:"offered"`
	Editors         int                             `json:"editors"`
	Acknowledged    int                             `json:"acknowledged"`
	Records         int                             `json:"records"`
	Pass            bool                            `json:"pass"`
	Witnesses       map[string]searchReadbackCounts `json:"immediate_witnesses"`
	EditsByEditor   map[int]int                     `json:"actual_edits_by_editor"`
}

// Only the retained first editor-session responses contribute. Polling's
// FirstCorrect metrics are deliberately not read by this acceptance census.
func summarizeSearchReadbacks(plan editProbePlan, records []editProbeRecord) *searchReadbackAcceptance {
	if !plan.SearchReadbackAcceptance {
		return nil
	}
	out := &searchReadbackAcceptance{Offered: plan.Count, Editors: max(1, plan.Editors), Records: len(records), Witnesses: map[string]searchReadbackCounts{}, EditsByEditor: map[int]int{}, ACKModes: map[string]int{}, FileViewStates: map[string]int{}}
	for _, kind := range []string{"search_visible", "text", "old_search_absent", "old_text_absent"} {
		out.Witnesses[kind] = searchReadbackCounts{Missing: plan.Count}
	}
	seen := map[int]bool{}
	valid := plan.Count == 1000 && len(records) == plan.Count
	for _, record := range records {
		if record.Ordinal < 0 || record.Ordinal >= len(plan.Steps) || seen[record.Ordinal] {
			valid = false
			continue
		}
		seen[record.Ordinal] = true
		step := plan.Steps[record.Ordinal]
		if record.Editor != step.Editor || record.Editor < 0 || record.Editor >= out.Editors {
			valid = false
		}
		out.EditsByEditor[record.Editor]++
		mode, _ := record.SearchACK["ack_mode"].(string)
		out.ACKModes[mode]++
		view, _ := record.SearchACK["file_view"].(map[string]any)
		state, _ := view["state"].(string)
		out.FileViewStates[state]++
		if searchReadbackObjectACK(record.SearchACK) {
			out.LocalObjectACKs++
		}
		if record.Ack.Value != nil && !math.IsNaN(*record.Ack.Value) && !math.IsInf(*record.Ack.Value, 0) && *record.Ack.Value >= 0 {
			out.Acknowledged++
		}
		kinds := map[string]bool{}
		for _, back := range record.Readbacks {
			count, required := out.Witnesses[back.Kind]
			if !required {
				continue
			}
			count.Observed++
			if kinds[back.Kind] {
				valid = false
			} else {
				count.Missing--
				kinds[back.Kind] = true
			}
			if back.Error != "" {
				count.Errors++
			}
			if back.Refusal != "" {
				count.Refusals++
			}
			if back.Correct && back.Error == "" && back.Refusal == "" && !back.ObservationOnly && back.ExpectedLine == step.ExpectedSearchLine && back.ExpectedLine > 0 && back.ExpectedPath != "" && back.ExpectedID == back.ExpectedPath+"::"+step.Marker && back.Latency.Value != nil && !math.IsNaN(*back.Latency.Value) && !math.IsInf(*back.Latency.Value, 0) && *back.Latency.Value >= 0 {
				count.Correct++
			}
			out.Witnesses[back.Kind] = count
		}
	}
	for _, count := range out.Witnesses {
		valid = valid && count.Observed == plan.Count && count.Correct == plan.Count && count.Errors == 0 && count.Refusals == 0 && count.Missing == 0
	}
	out.Pass = valid && len(out.EditsByEditor) == out.Editors && out.Acknowledged == plan.Count && out.LocalObjectACKs == plan.Count && (out.Editors == 1 || out.Editors == 4 || out.Editors == 8)
	return out
}

// Line-preserving renames keep the independent pre-dispatch expectation valid
// under concurrent edits. No disk or read RPC is inserted before the searches.
func bindSearchReadbackExpectations(c Corpus, plan *editProbePlan) error {
	if plan.Mode != "json" || plan.Language != "go" || plan.Kind != "rename" || plan.Tool != "edit.file" || c.EditSender != "" || plan.Count != 1000 {
		return errors.New("search readback acceptance requires 1000 JSON Go edit.file line-preserving renames with the ordinary sender")
	}
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, c.File, c.EditInitialContent, 0)
	if err != nil {
		return err
	}
	lines := make(map[string]int)
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			if _, duplicate := lines[function.Name.Name]; duplicate {
				return errors.New("search acceptance fixture has duplicate function names")
			}
			lines[function.Name.Name] = set.Position(function.Pos()).Line
		}
	}
	for i := range plan.Steps {
		step := &plan.Steps[i]
		before, _ := step.Request.Arguments["match"].(string)
		after, _ := step.Request.Arguments["replacement"].(string)
		old := strings.TrimPrefix(step.BeforeSymbol, c.File+"::")
		if old == step.BeforeSymbol || lines[old] <= 0 || !strings.HasPrefix(before, "func "+old+"(") || !strings.HasPrefix(after, "func "+step.Marker+"(") || strings.Count(before, "\n") != strings.Count(after, "\n") {
			return errors.New("search acceptance edit cannot preserve its pre-dispatch declaration line")
		}
		step.ExpectedSearchLine = lines[old]
	}
	return nil
}

func searchReadbackRequest(kind string, repo privateRepo, step editProbeStep) request {
	c := repo.Corpus
	operation, query := "symbols", step.Marker
	if kind == "text" || kind == "old_text_absent" {
		operation = "text"
	}
	if kind == "old_search_absent" || kind == "old_text_absent" {
		query = strings.TrimPrefix(step.BeforeSymbol, c.File+"::")
	}
	if kind == "text" {
		// The marker alone is already a prefix of the original Before name.
		// Include the new declaration boundary so old trigram postings cannot
		// locate the file and accidentally satisfy the pending-write probe.
		query = "func " + step.Marker + "("
	}
	options := map[string]any{"limit": 10000, "max_bytes": 0, "repo": repo.Prefix}
	if kind == "old_search_absent" || kind == "old_text_absent" {
		options["path"] = c.File
	}
	if operation == "symbols" {
		// Use the existing name clause so fuzzy/content neighbors do not
		// replace the selected declaration probe or make absence unprovable.
		query = "name:" + query
		options["query_class"], options["expand"], options["max_per_file"] = "symbol", "off", 0
	}
	return request{Tool: "search", Operation: operation, Arguments: map[string]any{"operation": operation, "query": query, "options": options}}
}

func exactSearchReadback(kind string, body map[string]any, repo privateRepo, step editProbeStep, callee string) bool {
	c := repo.Corpus
	expectedPath := repo.Prefix + "/" + c.File
	if step.ExpectedSearchLine <= 0 {
		return false
	}
	key, total, old := "results", "total", strings.TrimPrefix(step.BeforeSymbol, c.File+"::")
	if kind == "text" || kind == "old_text_absent" {
		key, total = "matches", "count"
	}
	if kind == "old_search_absent" || kind == "old_text_absent" {
		rows, complete := editProbeCompleteCount(body, key, total)
		if !complete || kind == "old_search_absent" && body["truncated"] != false || body["next_cursor"] != nil && body["next_cursor"] != "" {
			return false
		}
		// Symbol pages declare truncated:false. The actual text producer
		// omits that field on a complete page, and discloses only a positive
		// limit/budget cut. Never require a false field the tool did not emit,
		// or credit a contradictory/malformed completeness declaration.
		for _, flag := range []string{"truncated", "_truncated_by_limit", "_truncated_by_budget"} {
			if value, present := body[flag]; present && value != false {
				return false
			}
		}
		if exact, present := body["count_is_exact"]; present && exact != true {
			return false
		}
		if kind == "old_text_absent" {
			// This query is the unique old literal. Any nonzero answer is
			// inconsistent evidence, even if a malformed row omits that text.
			return len(rows) == 0
		}
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			id, hasID := row["id"].(string)
			name, hasName := row["name"].(string)
			if !hasID || id == "" || !hasName || name == "" || id == expectedPath+"::"+old || name == old {
				return false
			}
		}
		return true
	}
	if !editProbeCorrect(kind, body, c, step, callee) {
		return false
	}
	rows, _ := body[key].([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path, _ := row["file_path"].(string)
		lineKey := "start_line"
		if kind == "text" {
			path, _ = row["path"].(string)
			lineKey = "line"
		}
		line, err := strconvEditProbeUint(row[lineKey])
		if path != expectedPath && path != c.File || err != nil || line != uint64(step.ExpectedSearchLine) {
			continue
		}
		if kind == "search_visible" {
			id, _ := row["id"].(string)
			if id == expectedPath+"::"+step.Marker {
				return true
			}
		} else if text, _ := row["text"].(string); strings.Contains(text, "func "+step.Marker+"(") {
			return true
		}
	}
	return false
}

// This is wire-shape evidence, not an assertion of current graph freshness.
// A newer physical mutation can legitimately make this ACK's view historical;
// retain its explicit unavailable/null fields while testing content separately.
func searchReadbackObjectACK(ack map[string]any) bool {
	if ack["ack_mode"] != "local" {
		return false
	}
	hash, _ := ack["content_sha256"].(string)
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 {
		return false
	}
	if hint, _ := ack["graph_status_hint"].(string); hint == "" {
		return false
	}
	if _, ok := ack["readback_exact"].(bool); !ok {
		return false
	}
	view, ok := ack["file_view"].(map[string]any)
	if !ok {
		return false
	}
	state, _ := view["state"].(string)
	if state != "projected" && state != "unavailable" {
		return false
	}
	if state == "unavailable" {
		if reason, _ := view["reason"].(string); reason == "" {
			return false
		}
	}
	if delta, _ := view["delta_state"].(string); delta == "" {
		return false
	}
	counts, present := view["counts"]
	if !present {
		return false
	}
	if counts != nil {
		if _, ok := counts.(map[string]any); !ok {
			return false
		}
	}
	symbols, present := view["symbols"]
	if !present {
		return false
	}
	if symbols != nil {
		if _, ok := symbols.([]any); !ok {
			return false
		}
	}
	if _, ok := view["truncated"].(bool); !ok {
		return false
	}
	syntax, ok := view["syntax_health"].(map[string]any)
	if !ok {
		return false
	}
	if state, _ := syntax["state"].(string); state == "" {
		return false
	}
	if _, ok := syntax["checked"].(bool); !ok {
		return false
	}
	_, present = syntax["errors"]
	return present
}
