package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	toon "github.com/toon-format/toon-go"
)

// This typed, harness-local proof cannot be supplied by a JSON response. It
// records the selected wire decoder, never fabricates producer count fields.
type editProbeTOONWire struct{ operation string }

const editProbeTOONWireKey = "\x00load.toon.wire"

func decodeEditProbeOutput(raw json.RawMessage, format, operation string) (map[string]any, error) {
	switch format {
	case "", "json":
	case "toon":
	default:
		return nil, errors.New("unsupported edit output mode")
	}
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.IsError || len(envelope.Content) < 1 || len(envelope.Content) > 3 || envelope.Content[0].Type != "text" || envelope.Content[0].Text == "" {
		return nil, errors.New("output requires one successful payload and only known auxiliary notes")
	}
	lastNoteKind := 0
	for _, item := range envelope.Content[1:] {
		kind := editProbeSessionNoteKind(item.Text)
		if item.Type != "text" || kind <= lastNoteKind {
			return nil, errors.New("unknown or duplicate auxiliary output content")
		}
		lastNoteKind = kind
	}
	text := []byte(envelope.Content[0].Text)
	if format == "" || format == "json" {
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.UseNumber()
		var body map[string]any
		if !json.Valid(text) {
			return nil, errors.New("requested JSON returned non-JSON payload")
		}
		if err := decoder.Decode(&body); err != nil {
			return nil, err
		}
		return body, nil
	}
	if json.Valid(text) {
		return nil, errors.New("requested TOON returned JSON; no format credit")
	}
	decoded, err := toon.Decode(text, toon.WithStrictMode(true))
	if err != nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok || len(object) == 0 {
		return nil, errors.New("TOON probe payload must be a nonempty object")
	}
	if !editProbeTOONNumbersSafe(object) {
		return nil, errors.New("TOON numeric value cannot be proved exact")
	}
	// The pinned decoder uses float64. Only exact safe integer values can
	// provide IDs/counts; timing-status queries stay JSON/UseNumber separately.
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	body[editProbeTOONWireKey] = editProbeTOONWire{operation: operation}
	return body, nil
}

// These are the optional text-only notes appended by mcp/momentum.go. Selecting
// the first payload never changes the retained raw envelope or its byte budget.
func editProbeSessionNoteKind(text string) int {
	const nudge = ". Every location returned so far is real and citeable — if you are localizing or answering a question, consider writing your conclusion from what you already hold and fetching only what it still lacks. This note appears once.)"
	const escalation = " granular reads in a row. You likely have enough — answer NOW with your current best candidate and its evidence: name the file and the symbol, citing the locations already returned. A confident partial answer beats an exhausted turn budget with no answer."
	const alternative = " If one more look is genuinely needed, a single explore call with the request in plain words returns the ranked neighborhood — symbols, source, call paths — in one response."
	for _, form := range []struct {
		prefix, suffix string
		minimum, kind  int
	}{
		{"(Session note: graph read #", nudge, 6, 1},
		{"(Session note: ", escalation + " This note will not repeat.)", 10, 2},
		{"(Session note: ", escalation + alternative + " This note will not repeat.)", 10, 2},
	} {
		if !strings.HasPrefix(text, form.prefix) || !strings.HasSuffix(text, form.suffix) {
			continue
		}
		digits := strings.TrimSuffix(strings.TrimPrefix(text, form.prefix), form.suffix)
		n, err := strconv.Atoi(digits)
		if err == nil && n >= form.minimum && strconv.Itoa(n) == digits {
			return form.kind
		}
	}
	return 0
}

func editProbeTOONNumbersSafe(value any) bool {
	switch value := value.(type) {
	case float64:
		return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) < 1<<53
	case map[string]any:
		for _, nested := range value {
			if !editProbeTOONNumbersSafe(nested) {
				return false
			}
		}
	case []any:
		for _, nested := range value {
			if !editProbeTOONNumbersSafe(nested) {
				return false
			}
		}
	}
	return true
}

func editProbeOutputMode(c Corpus) (string, error) {
	switch c.EditOutput {
	case "", "json":
		return "json", nil
	case "toon":
		return "toon", nil
	default:
		return "", fmt.Errorf("unsupported edit output mode %q (want json or toon)", c.EditOutput)
	}
}
func editProbeRequestOutput(req request) string {
	output, exists := req.Arguments["output"]
	if !exists {
		return "json"
	}
	fields, ok := output.(map[string]any)
	if !ok {
		return "invalid"
	}
	format, exists := fields["format"]
	if !exists {
		return "json"
	}
	value, ok := format.(string)
	if !ok {
		return "invalid"
	}
	return value
}
func editProbeWithOutput(req request, format string) request {
	if format == "toon" {
		req.Arguments = copyJSON(req.Arguments)
		if req.Arguments == nil {
			req.Arguments = make(map[string]any)
		}
		req.Arguments["output"] = map[string]any{"format": "toon", "max_bytes": 0}
	}
	return req
}
func editProbeOutputOracles(c Corpus, oracles []editProbeOracle) []editProbeOracle {
	if c.EditOutput != "toon" {
		return oracles
	}
	for i := range oracles {
		oracles[i].call = editProbeWithOutput(oracles[i].call, "toon")
	}
	return oracles
}

// This certifies only the actual format's complete emitted row census. It
// never inserts a JSON total_nodes/total_edges value that TOON did not serve.
// The known compact traversal producer emits total+truncated, retains all edge
// rows, and omits total_edges precisely when no edge rows were clipped.
func editProbeTOONCompleteRows(body map[string]any, key, total string) bool {
	proof, ok := body[editProbeTOONWireKey].(editProbeTOONWire)
	if !ok || body["truncated"] != false {
		return false
	}
	switch proof.operation {
	case "relations.callers", "trace.call_chain":
	default:
		return false
	}
	if truncated, exists := body["_truncated_by_budget"]; exists && truncated != false {
		return false
	}
	nodes, exists := body["nodes"]
	rows, array := nodes.([]any)
	if exists && nodes == nil {
		rows, array = []any{}, true
	}
	declared, err := strconvEditProbeUint(body["total"])
	if !exists || !array || err != nil || declared != uint64(len(rows)) {
		return false
	}
	return key == "nodes" && total == "total_nodes" || key == "edges" && total == "total_edges"
}
