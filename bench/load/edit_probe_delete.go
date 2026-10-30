package main

import (
	"encoding/json"
	"strings"
)

func editProbeDeleteOracles(c Corpus, step editProbeStep) []editProbeOracle {
	old := map[string]any{"symbol": step.BeforeSymbol}
	file := map[string]any{"file": c.File}
	name := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	return []editProbeOracle{
		{"source", request{Tool: "read", Operation: "source", ExpectedAbsentSymbol: step.BeforeSymbol, Arguments: map[string]any{"operation": "source", "target": old}}},
		{"summary", request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}}},
		{"editing_context", request{Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}}},
		{"search_visible", request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"query_class": "symbol", "expand": "off"}}}},
		{"text", request{Tool: "search", Operation: "text", Arguments: map[string]any{"operation": "text", "query": name}}},
		{"callers", request{Tool: "relations", Operation: "callers", Arguments: map[string]any{"operation": "callers", "target": map[string]any{"symbol": c.Symbol}, "options": map[string]any{"depth": 1, "limit": 1000}}}},
		{"call_chain", request{Tool: "trace", Operation: "call_chain", Arguments: map[string]any{"operation": "call_chain", "target": old, "options": map[string]any{"depth": 1, "limit": 1000}}}},
	}
}

func editProbeCompleteCount(body map[string]any, key, total string) ([]any, bool) {
	value, present := body[key]
	rows, ok := value.([]any)
	// Actual producer nil slices serialize as explicit null. Only a present
	// key plus its exact zero total can establish the empty collection.
	if present && value == nil {
		rows = []any{}
		ok = true
	}
	count, err := strconvEditProbeUint(body[total])
	return rows, present && ok && err == nil && count == uint64(len(rows))
}

// Deletion has explicit absence oracles. A generic empty object, partial
// relation page or missing collection can never establish removal. Collections
// which should retain the stable callee must prove its concrete node identity.
func editProbeDeleteCorrect(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	old := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	untruncated := func() bool { v, ok := body["truncated"].(bool); return ok && !v }
	cleanNodes := func(rows []any) bool {
		survivor := false
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			id, _ := row["id"].(string)
			if editProbeSourceID(id, c.File, old) {
				return false
			}
			if editProbeSourceID(id, c.File, callee) {
				survivor = true
			}
		}
		return survivor
	}
	cleanEdges := func(rows []any) bool {
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			from, fok := row["from"].(string)
			to, tok := row["to"].(string)
			if !fok || !tok || editProbeSourceID(from, c.File, old) || editProbeSourceID(to, c.File, old) {
				return false
			}
		}
		return true
	}
	switch oracle {
	case "source":
		return false // only the exact typed producer refusal qualifies
	case "file":
		source, _ := body["content"].(string)
		path, _ := body["path"].(string)
		if !editProbeFixturePath(path, c.File) || strings.Contains(source, old) {
			return false
		}
		// The complete public file read must retain a real stable declaration;
		// comments or an empty successful result cannot witness deletion.
		payload, _ := json.Marshal(map[string]any{"id": c.Symbol, "file_path": c.File, "name": callee, "source": source})
		raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}})
		return editProbePresenceProof(raw, c, c.Symbol)
	case "summary", "callers":
		nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
		edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
		return untruncated() && nok && eok && cleanNodes(nodes) && cleanEdges(edges)
	case "editing_context":
		nodes, ok := body["defines"].([]any)
		file, fok := body["file"].(map[string]any)
		id, _ := file["id"].(string)
		if !ok || !fok || !(id == c.File || strings.HasSuffix(id, "/"+c.File)) {
			return false
		}
		if truncated, exists := body["truncated"]; exists && truncated != false {
			return false
		}
		return cleanNodes(nodes)
	case "search_visible":
		rows, ok := editProbeCompleteCount(body, "results", "total")
		if !ok || !untruncated() || body["query_class"] != "symbol" {
			return false
		}
		// Search may publish a complete decomposition/fallback population. Its
		// unrelated rows are not the deleted symbol and must not become a false
		// failure or a fake zero-result assertion.
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			id, ok := row["id"].(string)
			if !ok || id == "" || editProbeSourceID(id, c.File, old) {
				return false
			}
		}
		return true
	case "text":
		rows, ok := editProbeCompleteCount(body, "matches", "count")
		if body["query"] != old || !ok || len(rows) != 0 {
			return false
		}
		for _, key := range []string{"truncated", "_truncated_by_limit"} {
			if v, exists := body[key]; exists && v != false {
				return false
			}
		}
		return true
	case "call_chain":
		nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
		edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
		return untruncated() && nok && eok && len(nodes) == 0 && len(edges) == 0
	}
	return false
}
