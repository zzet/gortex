package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// These observations are independent of the load driver's eventual-visibility
// polling. One offer owns its original ACK and exactly one immediate quartet.
const readbackReferenceSamples = 100

var readbackReferenceTools = map[string][]string{
	"add_intrafile_call": {"edit.file"},
	"append_leaf":        {"edit.write"},
	"modify_body":        {"edit.symbol"},
	"modify_caller_body": {"edit.batch"},
	"change_signature":   {"edit.batch"},
	"add_crossfile_call": {"edit.file"},
	"rename":             {"refactor.rename"},
	"delete":             {"refactor.delete"},
	"relocate":           {"refactor.move"},
}

type readbackReferenceCell struct {
	Language string `json:"language,omitempty"`
	Agents   int    `json:"agents"`
	ACKMode  string `json:"ack_mode"` // "" preserves the production default full.
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Offers   int    `json:"offers"`
}

func (c readbackReferenceCell) validate() error {
	if _, err := readbackReferenceLanguageExtension(c.Language); err != nil {
		return err
	}
	if c.Kind == "relocate" && c.Language != "" && c.Language != "go" {
		return errors.New("unsupported language: relocation is Go-only; use existing explicit refusal cells")
	}
	if c.Agents != 1 && c.Agents != 4 && c.Agents != 8 && c.Agents != 16 && c.Agents != 32 {
		return errors.New("readback reference agents must be 1/4/8/16/32")
	}
	if c.ACKMode != "" && c.ACKMode != "local" {
		return errors.New("reference cells select omitted/default full or explicit local")
	}
	if c.Offers != readbackReferenceSamples {
		return errors.New("reference cell fixes exactly 100 offers; never enlarge samples after a failure")
	}
	for _, tool := range readbackReferenceTools[c.Kind] {
		if tool == c.Tool {
			return nil
		}
	}
	return fmt.Errorf("unsupported readback kind/tool %s/%s", c.Kind, c.Tool)
}

type readbackReferenceSymbol struct {
	Name      string `json:"name"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Body      string `json:"body"`
}
type readbackReferenceImage struct {
	Prefix    string                                 `json:"prefix"`
	File      string                                 `json:"file"`
	Bytes     string                                 `json:"bytes"`
	Symbols   map[string]readbackReferenceSymbol     `json:"symbols"`
	Auxiliary map[string]readbackReferenceDefinition `json:"auxiliary_definitions,omitempty"`
}

type readbackReferenceDefinition struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
}

// The fixture uses plain UTF-8 Go declarations starting at column zero. Its
// independent parser supplies line spans; no producer node/hash is an oracle.
func readbackReferenceParse(file, source string) (readbackReferenceImage, error) {
	out := readbackReferenceImage{File: file, Bytes: source, Symbols: map[string]readbackReferenceSymbol{}, Auxiliary: map[string]readbackReferenceDefinition{}}
	set := token.NewFileSet()
	f, err := parser.ParseFile(set, file, source, parser.AllErrors)
	if err != nil {
		return out, err
	}
	lines := strings.Split(source, "\n")
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil {
			return out, errors.New("reference fixture expects plain functions")
		}
		a, b := set.Position(fn.Pos()).Line, set.Position(fn.End()).Line
		if a < 1 || b < a || b > len(lines) {
			return out, errors.New("invalid independent fixture span")
		}
		if _, duplicate := out.Symbols[fn.Name.Name]; duplicate {
			return out, errors.New("duplicate fixture function")
		}
		out.Symbols[fn.Name.Name] = readbackReferenceSymbol{Name: fn.Name.Name, StartLine: a, EndLine: b, Body: strings.Join(lines[a-1:b], "\n")}
		add := func(id *ast.Ident, kind string) {
			if id == nil || id.Name == "_" {
				return
			}
			line := set.Position(id.Pos()).Line
			key := fn.Name.Name + "#param:" + id.Name
			if kind == "local" {
				key = fmt.Sprintf("%s#local:%s@+%d", fn.Name.Name, id.Name, line-a+1)
			}
			out.Auxiliary[key] = readbackReferenceDefinition{Name: id.Name, Kind: kind, StartLine: line}
		}
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				for _, id := range field.Names {
					add(id, "param")
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncLit:
				return false // No fixture closure is attributed to its parent.
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE {
					for _, expr := range node.Lhs {
						id, _ := expr.(*ast.Ident)
						add(id, "local")
					}
				}
			case *ast.ValueSpec:
				for _, id := range node.Names {
					add(id, "local")
				}
			case *ast.RangeStmt:
				if node.Tok == token.DEFINE {
					for _, expr := range []ast.Expr{node.Key, node.Value} {
						id, _ := expr.(*ast.Ident)
						add(id, "local")
					}
				}
			}
			return true
		})
	}
	if len(out.Symbols) == 0 {
		return out, errors.New("fixture must retain an independently parsed survivor")
	}
	return out, nil
}

// A missing/partial/empty body cannot silently become exact. Refusals use the
// existing precise scoped not-found decoder only when deletion is expected.
func readbackReferenceSHA(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}
func readbackReferenceID(image readbackReferenceImage, name string) string {
	return image.Prefix + "/" + image.File + "::" + name
}
func readbackReferenceBody(raw json.RawMessage) (map[string]any, bool, bool) {
	var env struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Content) == 0 || env.Content[0].Type != "text" {
		return nil, false, false
	}
	var body map[string]any
	if !json.Valid([]byte(env.Content[0].Text)) {
		return nil, env.IsError, false
	}
	decoder := json.NewDecoder(strings.NewReader(env.Content[0].Text))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil {
		return nil, env.IsError, false
	}
	return body, env.IsError, true
}
func readbackReferenceCorrect(endpoint string, raw json.RawMessage, image readbackReferenceImage, target string, absent bool) bool {
	if image.Prefix == "" || len(image.Symbols) == 0 {
		return false
	}
	expected, present := image.Symbols[target]
	if present == absent {
		return false
	}
	if absent && (endpoint == "source" || endpoint == "symbol") {
		if _, ok := editProbeExactNotFound(raw, readbackReferenceID(image, target)); ok {
			return true
		}
		b, isError, ok := readbackReferenceBody(raw)
		if !ok || !isError && endpoint == "source" {
			return false
		}
		data, _ := b["data"].(map[string]any)
		if b["condition"] != "symbol_not_found" || data["id"] != readbackReferenceID(image, target) {
			return false
		}
		if b["message"] == "symbol not found: removed by pending edit" && data["reason"] == "removed by pending edit" {
			return true
		}
		expectedGuidance := fmt.Sprintf("no symbol with id %q is in the index — it may be spelled differently, live in an unindexed file, or be a local. Search for it rather than reading by id.", readbackReferenceID(image, target))
		return endpoint == "symbol" && !isError && b["message"] == expectedGuidance
	}
	body, isError, ok := readbackReferenceBody(raw)
	if !ok || isError {
		return false
	}
	switch endpoint {
	case "source":
		source, ok := body["source"].(string)
		return ok && body["id"] == readbackReferenceID(image, target) && body["name"] == target && readbackReferenceSpan(body, expected) && readbackReferenceSHA(source) == readbackReferenceSHA(expected.Body)
	case "symbol":
		node, ok := body["node"].(map[string]any)
		if !ok || node["id"] != readbackReferenceID(image, target) || !readbackReferenceSpan(node, expected) {
			return false
		}
		for _, key := range []string{"in_edges", "out_edges"} {
			value, present := body[key]
			if !present {
				return false
			}
			if _, slice := value.([]any); value != nil && !slice {
				return false
			}
		}
		return true
	case "summary":
		rows, ok := body["nodes"].([]any)
		if !ok || body["truncated"] != false {
			return false
		}
		count, err := strconvEditProbeUint(body["total_nodes"])
		return err == nil && count == uint64(len(rows)) && readbackReferenceNodes(rows, image)
	case "editing_context":
		file, ok := body["file"].(map[string]any)
		if !ok || file["id"] != image.Prefix+"/"+image.File {
			return false
		}
		rows, ok := body["defines"].([]any)
		return ok && readbackReferenceDefines(rows, image)
	case "file":
		content, ok := body["content"].(string)
		path, _ := body["path"].(string)
		return ok && editProbeFixturePath(path, image.File) && readbackReferenceSHA(content) == readbackReferenceSHA(image.Bytes)
	}
	return false
}

// The public defines schema supplies starts, not full-node end lines. Required
// functions must all be present. Optional local/parameter rows are checked
// against the independently parsed source and can never substitute a function.
func readbackReferenceDefines(rows []any, image readbackReferenceImage) bool {
	seenIDs, functions := map[string]bool{}, map[string]bool{}
	prefix := image.Prefix + "/" + image.File + "::"
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		id, _ := row["id"].(string)
		name, _ := row["name"].(string)
		kind, _ := row["kind"].(string)
		line, err := strconvEditProbeUint(row["start_line"])
		if err != nil || seenIDs[id] || !strings.HasPrefix(id, prefix) {
			return false
		}
		if path, present := row["file_path"]; present && path != image.File && path != image.Prefix+"/"+image.File {
			return false
		}
		seenIDs[id] = true
		if kind == "function" {
			expected, present := image.Symbols[name]
			if !present || id != readbackReferenceID(image, name) || line != uint64(expected.StartLine) {
				return false
			}
			if end, present := row["end_line"]; present {
				last, endErr := strconvEditProbeUint(end)
				if endErr != nil || last != uint64(expected.EndLine) {
					return false
				}
			}
			functions[name] = true
			continue
		}
		expected, present := image.Auxiliary[strings.TrimPrefix(id, prefix)]
		if !present || kind != expected.Kind || name != expected.Name || line != uint64(expected.StartLine) {
			return false
		}
	}
	return len(functions) == len(image.Symbols)
}
func readbackReferenceSpan(row map[string]any, symbol readbackReferenceSymbol) bool {
	a, errA := strconvEditProbeUint(row["start_line"])
	b, errB := strconvEditProbeUint(row["end_line"])
	return errA == nil && errB == nil && a == uint64(symbol.StartLine) && b == uint64(symbol.EndLine)
}
func readbackReferenceNodes(rows []any, image readbackReferenceImage) bool {
	seen := map[string]bool{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		kind, _ := row["kind"].(string)
		if kind == "file" || kind == "package" {
			continue
		}
		name, _ := row["name"].(string)
		expected, ok := image.Symbols[name]
		if !ok || kind != "function" || seen[name] || row["id"] != readbackReferenceID(image, name) || !readbackReferenceSpan(row, expected) {
			return false
		}
		seen[name] = true
	}
	return len(seen) == len(image.Symbols)
}

type readbackReferenceObservation struct {
	Offered  int  `json:"offered"`
	Correct  int  `json:"correct"`
	Missing  int  `json:"missing"`
	Failures int  `json:"failures"`
	Pass     bool `json:"pass"`
}

func summarizeReadbackReference(offered int, correct []bool) readbackReferenceObservation {
	out := readbackReferenceObservation{Offered: offered}
	if offered <= 0 || len(correct) > offered {
		return out
	}
	out.Missing = offered - len(correct)
	for _, ok := range correct {
		if ok {
			out.Correct++
		} else {
			out.Failures++
		}
	}
	out.Pass = out.Missing == 0 && int64(out.Correct)*1000 >= int64(offered)*999
	return out
}
