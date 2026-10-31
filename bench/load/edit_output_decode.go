package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	toon "github.com/toon-format/toon-go"
)

// The strict edit output decoder. It depends only on the standard library and
// the pinned TOON decoder, never on edit-probe plans or runtime state, so other
// edit-loop measurements can decode the same MCP envelopes the same way: one
// successful text payload plus only the known ordered auxiliary session notes,
// requested JSON never credited from TOON (or TOON from JSON), exact numbers
// only, and a harness-private marker recording which wire was decoded.

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
