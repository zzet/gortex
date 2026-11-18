package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

func editProbeMoveFixture(c Corpus) error {
	if filepath.Ext(c.File) != ".go" || filepath.Base(c.File) != c.File || filepath.Ext(c.EditDestination) != ".go" || filepath.Base(c.EditDestination) != c.EditDestination || c.File == c.EditDestination {
		return errors.New("move fixture requires distinct root Go files in the same package")
	}
	if err := editProbeInitialFixture(c); err != nil {
		return err
	}
	source, err := parser.ParseFile(token.NewFileSet(), c.File, c.EditInitialContent, 0)
	if err != nil {
		return err
	}
	target, err := parser.ParseFile(token.NewFileSet(), c.EditDestination, c.EditDestinationContent, 0)
	if err != nil {
		return err
	}
	if source.Name.Name != target.Name.Name {
		return errors.New("move fixture requires exact same-package source and destination")
	}
	return nil
}
func editProbeMoveOracles(c Corpus, step editProbeStep) []editProbeOracle {
	target := map[string]any{"symbol": step.TargetSymbol}
	file := map[string]any{"file": step.DestinationFile}
	name := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	return []editProbeOracle{
		{"source", request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": target, "options": map[string]any{"context_lines": 0}}}},
		{"summary", request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}}},
		{"editing_context", request{Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}}},
		{"search_visible", request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"limit": 1000, "max_per_file": 0, "query_class": "symbol", "expand": "off"}}}},
		{"text", request{Tool: "search", Operation: "text", Arguments: map[string]any{"operation": "text", "query": "func " + name, "options": map[string]any{"limit": 1000}}}},
		{"callers", request{Tool: "relations", Operation: "callers", Arguments: map[string]any{"operation": "callers", "target": map[string]any{"symbol": c.Symbol}, "options": map[string]any{"depth": 1, "limit": 1000}}}},
		{"call_chain", request{Tool: "trace", Operation: "call_chain", Arguments: map[string]any{"operation": "call_chain", "target": target, "options": map[string]any{"depth": 1, "limit": 1000}}}},
		{"old_source_absent", request{Tool: "read", Operation: "source", ExpectedAbsentSymbol: step.BeforeSymbol, Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": step.BeforeSymbol}}}},
	}
}
func editProbeMoveCorrect(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	name := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	if oracle == "source_file_after_move" {
		path, _ := body["path"].(string)
		source, _ := body["content"].(string)
		if !editProbeFixturePath(path, c.File) {
			return false
		}
		file, err := parser.ParseFile(token.NewFileSet(), "source.go", source, 0)
		if err != nil {
			return false
		}
		survivor := false
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Name.Name == name {
					return false
				}
				if fn.Name.Name == callee {
					survivor = true
				}
			}
		}
		return survivor
	}
	switch oracle {
	case "source":
		id, _ := body["id"].(string)
		path, _ := body["file_path"].(string)
		source, _ := body["source"].(string)
		return body["name"] == name && editProbeSourceID(id, step.DestinationFile, name) && editProbeFixturePath(path, step.DestinationFile) && editProbeFixtureSource(step.DestinationFile, source, name, callee)
	case "file":
		path, _ := body["path"].(string)
		source, _ := body["content"].(string)
		return editProbeFixturePath(path, step.DestinationFile) && editProbeFixtureSource(step.DestinationFile, source, name, callee)
	case "summary", "editing_context", "search_visible":
		key := map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"}[oracle]
		rows, ok := body[key].([]any)
		if !ok {
			return false
		}
		found := false
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			id, _ := row["id"].(string)
			if editProbeSourceID(id, c.File, name) {
				return false
			}
			if editProbeSourceID(id, step.DestinationFile, name) {
				found = true
			}
		}
		return found
	case "text":
		rows, _ := body["matches"].([]any)
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			path, _ := row["path"].(string)
			id, _ := row["symbol_id"].(string)
			text, _ := row["text"].(string)
			if editProbeFixturePath(path, step.DestinationFile) && editProbeSourceID(id, step.DestinationFile, name) && editProbeSourceCall(text, name, callee) {
				return true
			}
		}
	case "callers", "call_chain":
		truncated, ok := body["truncated"].(bool)
		nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
		edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
		if !ok || truncated || !nok || !eok {
			return false
		}
		for _, raw := range nodes {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			id, _ := row["id"].(string)
			if editProbeSourceID(id, c.File, name) {
				return false
			}
		}
		found := false
		for _, raw := range edges {
			row, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			from, _ := row["from"].(string)
			to, _ := row["to"].(string)
			if editProbeSourceID(from, c.File, name) || editProbeSourceID(to, c.File, name) {
				return false
			}
			if row["kind"] == "calls" && editProbeSourceID(from, step.DestinationFile, name) && editProbeSourceID(to, c.File, callee) {
				found = true
			}
		}
		return found
	}
	return false
}
