package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// Expected absence is a declared producer refusal with its own identity and
// endpoint. It is never an empty successful body, a transport error or a
// blanket permission to ignore missing symbols.
type editProbeExpectedAbsence struct{ symbol, message string }

func (e *editProbeExpectedAbsence) Error() string { return e.message }

func editProbeExactNotFound(raw json.RawMessage, scopedID string) (string, bool) {
	if scopedID == "" || strings.ContainsAny(scopedID, "\r\n\x00") {
		return "", false
	}
	var response struct {
		IsError    bool `json:"isError"`
		Structured any  `json:"structuredContent"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &response) != nil || !response.IsError || response.Structured != nil || len(response.Content) != 1 || response.Content[0].Type != "text" {
		return "", false
	}
	text := response.Content[0].Text
	first, _, _ := strings.Cut(text, "\n")
	exact := "symbol not found: " + scopedID
	if first != exact {
		// handleGetSymbolSource appends this exact same-line delimiter. The
		// complete old ID must precede it; a prefix match is not absence proof.
		hint, suggested := strings.CutPrefix(first, exact+" — did you mean: ")
		if !suggested || strings.TrimSpace(hint) == "" {
			return "", false
		}
	}
	return text, true
}

func (c *editProbeCaller) expectedAbsence(data json.RawMessage, identity wireCallIdentity) error {
	logical := c.request.ExpectedAbsentSymbol
	if logical == "" || c.request.Tool != "read" || c.request.Operation != "source" || (c.role != "probe" && c.role != "readback") {
		return nil
	}
	target, ok := c.request.Arguments["target"].(map[string]any)
	if !ok || target["symbol"] != logical {
		return nil
	}
	c.state.mu.Lock()
	proven := c.state.absenceBefore[logical]
	editStarted := c.state.editWrite
	c.state.mu.Unlock()
	// A reply from a request sent before the real edit wire start does not
	// establish post-edit absence, even when its delivery happens afterward.
	if !proven || editStarted.IsZero() || identity.WriteStarted.IsZero() || identity.WriteStarted.Before(editStarted) {
		return nil
	}
	scoped := c.runtime.p.Repos[0].Prefix + "/" + logical
	if message, matched := editProbeExactNotFound(data, scoped); matched {
		return &editProbeExpectedAbsence{symbol: scoped, message: message}
	}
	return nil
}

// Before-state proof is obtained on the same bound prober session during
// untimed setup. A not-found response cannot establish its own applicability.
func editProbePresenceProof(raw json.RawMessage, c Corpus, logical string) bool {
	separator := strings.LastIndex(logical, "::")
	if separator < 1 || logical[:separator] != c.File || !token.IsIdentifier(logical[separator+2:]) {
		return false
	}
	if checkToolResult(raw) != nil {
		return false
	}
	body, err := decodeEditProbeResult(raw)
	if err != nil {
		return false
	}
	name := logical[separator+2:]
	id, _ := body["id"].(string)
	path, _ := body["file_path"].(string)
	source, _ := body["source"].(string)
	if !editProbeSourceID(id, c.File, name) || !editProbeFixturePath(path, c.File) || body["name"] != name {
		return false
	}
	switch filepath.Ext(c.File) {
	case ".ts":
		valid, correct := editProbeTypeScriptFixture(source, name, "")
		_, calls := editProbeTypeScriptFixture(source, name, c.Symbol[strings.LastIndex(c.Symbol, "::")+2:])
		if c.EditKind == "modify_caller_body" {
			_, alternate := editProbeTypeScriptFixture(source, name, "Alternate")
			calls = calls || alternate
		}
		return valid && (correct || calls)
	case ".py":
		valid, correct := editProbePythonFixture(source, name, "")
		_, calls := editProbePythonFixture(source, name, c.Symbol[strings.LastIndex(c.Symbol, "::")+2:])
		if c.EditKind == "modify_caller_body" {
			_, alternate := editProbePythonFixture(source, name, "Alternate")
			calls = calls || alternate
		}
		return valid && (correct || calls)
	case ".go":
		parsed, err := parser.ParseFile(token.NewFileSet(), "before.go", source, 0)
		if err != nil {
			parsed, err = parser.ParseFile(token.NewFileSet(), "before.go", "package fixture\n"+source, 0)
		}
		if err != nil {
			return false
		}
		count := 0
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name && fn.Body != nil {
				count++
			}
		}
		return count == 1
	}
	return false
}

// A new call edge alone cannot prove rename reconciliation: a stale old
// caller may remain beside it. Retain both positive new-edge and complete
// old-identity exclusion checks on the actual relation result.
func editProbeRenameCensus(body map[string]any, c Corpus, step editProbeStep) bool {
	truncated, ok := body["truncated"].(bool)
	if !ok || truncated {
		return false
	}
	nodes, nok := editProbeCompleteCount(body, "nodes", "total_nodes")
	edges, eok := editProbeCompleteCount(body, "edges", "total_edges")
	if !nok || !eok {
		return false
	}
	old := step.BeforeSymbol[strings.LastIndex(step.BeforeSymbol, "::")+2:]
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		id, _ := node["id"].(string)
		if editProbeSourceID(id, c.File, old) {
			return false
		}
	}
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		from, fok := edge["from"].(string)
		to, tok := edge["to"].(string)
		if !fok || !tok || editProbeSourceID(from, c.File, old) || editProbeSourceID(to, c.File, old) {
			return false
		}
	}
	return true
}
