package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
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

// There is no GCX decoder here, and read_file answers a GCX request with TOON.
// A GCX answer is reported unavailable, never decoded or credited as TOON.
var errEditOutputGCXUnavailable = errors.New("GCX output unavailable: no GCX decoder, and read_file answers a GCX request with TOON")

func decodeEditProbeOutput(raw json.RawMessage, format, operation string) (map[string]any, error) {
	facade := format == "facade"
	if facade {
		if err := verifyEditProbeFacade(raw, operation); err != nil {
			return nil, err
		}
		format = "json"
	}
	switch format {
	case "", "json":
	case "toon":
	case "compact":
		return decodeEditProbeCompactOutput(raw, operation)
	case "cli":
		return decodeEditProbeCLIOutput(raw)
	case "gcx":
		fields, err := editProbeGCXTierFields(raw, operation)
		return fields, errors.Join(errEditOutputGCXUnavailable, err)
	default:
		return nil, errors.New("unsupported edit output mode")
	}
	text, err := editOutputPayload(raw, false)
	if err != nil {
		return nil, err
	}
	var tierFields map[string]any
	if format == "toon" {
		text, tierFields, err = editProbeTierPrefix(text)
		if err != nil {
			return nil, err
		}
	}
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
		if facade {
			body[editProbeFacadeWireKey] = editProbeFacadeWire{operation: operation}
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
	if err := editProbeKeepTierFields(body, tierFields); err != nil {
		return nil, err
	}
	body[editProbeTOONWireKey] = editProbeTOONWire{operation: operation}
	return body, nil
}

// One successful text payload, followed only by the known ordered auxiliary
// session notes. Only compact text may be empty: it is a zero-row answer.
func editOutputPayload(raw json.RawMessage, allowEmpty bool) ([]byte, error) {
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.IsError || len(envelope.Content) < 1 || len(envelope.Content) > 3 || envelope.Content[0].Type != "text" || envelope.Content[0].Text == nil || *envelope.Content[0].Text == "" && !allowEmpty {
		return nil, errors.New("output requires one successful payload and only known auxiliary notes")
	}
	lastNoteKind := 0
	for _, item := range envelope.Content[1:] {
		kind := 0
		if item.Text != nil {
			kind = editProbeSessionNoteKind(*item.Text)
		}
		if item.Type != "text" || kind <= lastNoteKind {
			return nil, errors.New("unknown or duplicate auxiliary output content")
		}
		lastNoteKind = kind
	}
	return []byte(*envelope.Content[0].Text), nil
}

// Compact text is one line per symbol: kind, qualified name, path:line, and
// for symbol search an optional enclosing owner. Subgraph answers may follow
// their rows with a truncation line, one edge-count footer and concurrency
// notes. There are no node IDs, edge rows, signatures or freshness fields.
type editProbeCompactRow struct {
	Kind, Qualified, Path, Owner string
	Line                         uint64
}

// This typed, harness-local answer cannot be supplied by a JSON response.
type editProbeCompactAnswer struct {
	operation string
	rows      []editProbeCompactRow
	truncated bool
	edges     int // -1 without an edge footer
}

const editProbeCompactKey = "\x00load.compact.rows"

// Only these operations have a compact text producer; every other request
// stays JSON.
var editProbeCompactOperations = map[string]bool{"read.summary": true, "search.symbols": true, "relations.callers": true, "trace.call_chain": true}

var (
	editProbeCompactRowLine      = regexp.MustCompile(`^([a-z_]+) (\S+) (\S+):([1-9][0-9]*)( \(in (\S+)\))?$`)
	editProbeCompactTruncated    = regexp.MustCompile(`^\.\.\. truncated \([0-9]+ total\)$`)
	editProbeCompactEdges        = regexp.MustCompile(`^edges: ([0-9]+)( of [0-9]+)? total(, [0-9]+ (EXTRACTED|INFERRED|AMBIGUOUS))*$`)
	editProbeCompactConcurrency  = regexp.MustCompile(`^concurrency \S+:( sync_guarded)?( cross_concurrent)?$`)
	editProbeCompactBudgetMarker = "... trimmed to byte budget; pass max_bytes:0 for the full result"
)

func decodeEditProbeCompactOutput(raw json.RawMessage, operation string) (map[string]any, error) {
	if !editProbeCompactOperations[operation] {
		return nil, errors.New("operation has no compact text producer")
	}
	text, err := editOutputPayload(raw, true)
	if err != nil {
		return nil, err
	}
	text, tierFields, err := editProbeTierPrefix(text)
	if err != nil {
		return nil, err
	}
	if json.Valid(text) {
		return nil, errors.New("requested compact text returned JSON; no format credit")
	}
	if len(text) > 0 && text[len(text)-1] != '\n' {
		return nil, errors.New("compact text must end each line with a newline")
	}
	answer := editProbeCompactAnswer{operation: operation, edges: -1}
	subgraph := operation != "search.symbols"
	stage := 0 // rows, then truncation, then the edge footer, then concurrency notes
	for _, line := range strings.Split(string(text), "\n")[:bytes.Count(text, []byte("\n"))] {
		switch match := editProbeCompactRowLine.FindStringSubmatch(line); {
		case match != nil && stage == 0 && (match[6] == "" || !subgraph):
			number, err := strconv.ParseUint(match[4], 10, 64)
			if err != nil {
				return nil, err
			}
			answer.rows = append(answer.rows, editProbeCompactRow{Kind: match[1], Qualified: match[2], Path: match[3], Owner: match[6], Line: number})
		case subgraph && stage < 1 && (editProbeCompactTruncated.MatchString(line) || line == editProbeCompactBudgetMarker):
			answer.truncated, stage = true, 1
		case subgraph && stage < 2 && editProbeCompactEdges.MatchString(line):
			edges, err := strconv.Atoi(editProbeCompactEdges.FindStringSubmatch(line)[1])
			if err != nil {
				return nil, err
			}
			answer.edges, stage = edges, 2
		case subgraph && editProbeCompactConcurrency.MatchString(line):
			stage = 3
		default:
			return nil, fmt.Errorf("compact line outside the %s grammar: %q", operation, line)
		}
	}
	body := map[string]any{editProbeCompactKey: answer}
	if err := editProbeKeepTierFields(body, tierFields); err != nil {
		return nil, err
	}
	return body, nil
}

// `gortex call --format json` prints the tool's first text payload as JSON,
// re-encoded through float64, and wraps a prose answer as
// {"tool","format":"text","text"}. Only a JSON object with exactly
// representable numbers is an answer; the wrapper is never JSON credit.
func decodeEditProbeCLIOutput(stdout []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("CLI stdout is not one JSON object: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("CLI stdout has more than one JSON value")
	}
	if _, wrapped := body["text"].(string); wrapped && len(body) == 3 && body["format"] == "text" && body["tool"] != nil {
		return nil, errors.New("CLI wrapped a prose answer; no JSON credit")
	}
	if !editProbeCLINumbersExact(body) {
		return nil, errors.New("CLI numeric value cannot be proved exact after float64 re-encoding")
	}
	return body, nil
}

func editProbeCLINumbersExact(value any) bool {
	switch value := value.(type) {
	case json.Number:
		number, err := value.Float64()
		return err == nil && editProbeTOONNumbersSafe(number)
	case map[string]any:
		for _, nested := range value {
			if !editProbeCLINumbersExact(nested) {
				return false
			}
		}
	case []any:
		for _, nested := range value {
			if !editProbeCLINumbersExact(nested) {
				return false
			}
		}
	}
	return true
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
