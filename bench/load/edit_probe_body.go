package main

import (
	"path/filepath"
	"strings"
)

func editProbeFollowingDeclaration(file, name string) string {
	switch filepath.Ext(file) {
	case ".go":
		return "func " + name + "() int { return 2 }"
	case ".ts":
		return "export function " + name + "(): number { return 2; }"
	case ".py":
		return "def " + name + "():\n    return 2"
	}
	return ""
}

func editProbeBodyOracles(c Corpus, step editProbeStep) []editProbeOracle {
	target := map[string]any{"symbol": step.TargetSymbol}
	file := map[string]any{"file": c.File}
	return []editProbeOracle{
		{"source", request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": target}}},
		{"summary", request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}}},
		{"editing_context", request{Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}}},
		{"search_visible", request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": step.Marker}}},
		{"text", request{Tool: "search", Operation: "text", Arguments: map[string]any{"operation": "text", "query": "return " + c.Symbol[strings.LastIndex(c.Symbol, "::")+2:] + "()", "options": map[string]any{"limit": 1000}}}},
		{"callers", request{Tool: "relations", Operation: "callers", Arguments: map[string]any{"operation": "callers", "target": map[string]any{"symbol": c.Symbol}, "options": map[string]any{"depth": 1, "limit": 1000}}}},
		{"call_chain", request{Tool: "trace", Operation: "call_chain", Arguments: map[string]any{"operation": "call_chain", "target": target, "options": map[string]any{"depth": 1, "limit": 1000}}}},
	}
}

// The precreated following declaration is an unchanged reference point. Any
// upstream edits move both rows equally; only this target's actual line growth
// changes their relative start-line gap. Presence of a precreated name alone
// cannot establish first-correct visibility.
func editProbeBodyGap(rows []any, c Corpus, step editProbeStep) bool {
	targetName := step.TargetSymbol[strings.LastIndex(step.TargetSymbol, "::")+2:]
	followingName := step.FollowingSymbol[strings.LastIndex(step.FollowingSymbol, "::")+2:]
	var target, following uint64
	targetCount, followingCount := 0, 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		id, _ := row["id"].(string)
		if editProbeSourceID(id, c.File, targetName) {
			n, err := strconvEditProbeUint(row["start_line"])
			if err != nil || n == 0 {
				return false
			}
			target = n
			targetCount++
		}
		if editProbeSourceID(id, c.File, followingName) {
			n, err := strconvEditProbeUint(row["start_line"])
			if err != nil || n == 0 {
				return false
			}
			following = n
			followingCount++
		}
	}
	return targetCount == 1 && followingCount == 1 && following > target && following-target == uint64(step.ExpectedLineGap)
}

func editProbeBodyCorrect(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	name := step.TargetSymbol[strings.LastIndex(step.TargetSymbol, "::")+2:]
	expected := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", name))
	switch oracle {
	case "source":
		id, _ := body["id"].(string)
		path, _ := body["file_path"].(string)
		source, _ := body["source"].(string)
		return body["name"] == name && editProbeSourceID(id, c.File, name) && editProbeFixturePath(path, c.File) && strings.TrimSpace(source) == expected && editProbeFixtureSource(c.File, source, name, callee)
	case "file":
		path, _ := body["path"].(string)
		source, _ := body["content"].(string)
		return editProbeFixturePath(path, c.File) && strings.Count(source, expected) == 1 && editProbeFixtureSource(c.File, source, name, callee)
	case "summary", "editing_context", "search_visible":
		key := map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"}[oracle]
		rows, ok := body[key].([]any)
		return ok && editProbeBodyGap(rows, c, step)
	case "text":
		rows, ok := body["matches"].([]any)
		if !ok {
			return false
		}
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := row["symbol_id"].(string)
			path, _ := row["path"].(string)
			text, _ := row["text"].(string)
			statement := "return " + callee + "()"
			if filepath.Ext(c.File) == ".ts" {
				statement += ";"
			}
			if editProbeSourceID(id, c.File, name) && editProbeFixturePath(path, c.File) && strings.TrimSpace(text) == statement {
				return true
			}
		}
		return false
	case "callers", "call_chain":
		if c.EditKind == "modify_caller_body" && oracle == "call_chain" {
			truncated, ok := body["truncated"].(bool)
			_, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
			_, eok := editProbeCompleteCount(body, "edges", "total_edges")
			if !ok || truncated || !nok || !eok {
				return false
			}
		}
		edges, ok := body["edges"].([]any)
		if !ok {
			return false
		}
		found := false
		for _, raw := range edges {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			from, _ := row["from"].(string)
			to, _ := row["to"].(string)
			if row["kind"] == "calls" && editProbeSourceID(from, c.File, name) {
				if c.EditKind == "modify_caller_body" && editProbeSourceID(to, c.File, "Alternate") {
					return false
				}
				if editProbeSourceID(to, c.File, callee) {
					found = true
				}
			}
		}
		return found
	}
	return false
}
