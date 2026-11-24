package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	wire "github.com/gortexhq/gcx-go"
)

type editConsumerGCXDeadCode struct{}

const editConsumerGCXDeadCodeKey = "\x00load.consumer.gcx.dead_code"

var editConsumerGCXFields = []string{"id", "kind", "name", "path", "line", "reason"}

// The GCX request is kept on the wire. Rename and verify use the server's
// TOON fallback, while dead-code analysis has its own GCX producer.
func decodeEditConsumerGCXOutput(raw json.RawMessage, operation string) (map[string]any, string, error) {
	var body map[string]any
	var err error
	served := "gcx"
	switch operation {
	case "refactor.rename", "change.verify":
		served = "toon"
		body, err = decodeEditProbeOutput(raw, served, operation)
	case "analyze.dead_code":
		body, err = decodeEditConsumerDeadCodeGCX(raw)
	default:
		err = errors.New("no GCX consumer oracle for " + operation)
	}
	if err != nil {
		return nil, "unavailable", err
	}
	return body, served, nil
}

func decodeEditConsumerDeadCodeGCX(raw json.RawMessage) (map[string]any, error) {
	text, err := editOutputPayload(raw, false)
	if err != nil {
		return nil, err
	}
	if len(text) == 0 || text[len(text)-1] != '\n' {
		return nil, errors.New("dead-code GCX requires complete terminated sections")
	}
	decoder := wire.NewDecoder(bytes.NewReader(text))
	header, err := decoder.Header()
	if err != nil {
		return nil, err
	}
	fields, pending, err := editConsumerGCXFreshness(text, header)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	dead, err := editConsumerGCXRows(decoder, header, "analyze.dead_code", seen)
	if err != nil {
		return nil, err
	}
	unknown := []any{}
	section, err := decoder.NextSection()
	switch {
	case err == nil && pending:
		unknown, err = editConsumerGCXRows(decoder, section, "analyze.pending_unknown", seen)
		if err != nil {
			return nil, err
		}
		if _, err := decoder.NextSection(); err != io.EOF {
			return nil, errors.New("unexpected section after GCX pending_unknown")
		}
	case err != io.EOF:
		return nil, errors.New("unexpected section after GCX dead_code")
	}
	if pending && len(unknown) == 0 {
		return nil, errors.New("pending dead-code GCX has no typed unknown rows")
	}
	// The request disables the output byte budget. The producer distributes
	// this exact row limit across both sections, so at the limit its emitted
	// counts cannot distinguish a complete from a truncated result.
	if len(dead)+len(unknown) >= editConsumerDeadCodeLimit {
		return nil, errors.New("dead-code GCX row limit does not prove completeness")
	}
	fields["dead_code"], fields["pending_unknown"] = dead, unknown
	fields["total"] = json.Number(strconv.Itoa(len(dead)))
	fields["pending_unknown_count"] = json.Number(strconv.Itoa(len(unknown)))
	fields["truncated"] = false
	fields[editConsumerGCXDeadCodeKey] = editConsumerGCXDeadCode{}
	return fields, nil
}

func editConsumerGCXFreshness(text []byte, header wire.Header) (map[string]any, bool, error) {
	fields := make(map[string]any)
	state, scope, count := header.Meta["graph_freshness"], header.Meta["freshness_scope"], header.Meta["pending_file_count"]
	complete := header.Meta["complete"]
	expectedComment := ""
	pending := state == "pending" || state == "partial"
	if state != "" || scope != "" || count != "" || complete != "" {
		paths, err := strconv.ParseUint(count, 10, 63)
		if err != nil || strconv.FormatUint(paths, 10) != count || scope != "derived" ||
			(pending && (complete != "false" || paths == 0) || !pending && (state != "fresh" || complete != "" || paths != 0)) {
			return nil, false, errors.New("invalid dead-code GCX freshness metadata")
		}
		expectedComment = fmt.Sprintf("%s derived: %s (%s files)", wire.CommentPrefix, state, count)
		fields["graph_freshness"], fields["freshness_scope"], fields["pending_file_count"] = state, scope, json.Number(count)
		if pending {
			fields["complete"] = false
		}
	}
	lines := strings.Split(string(text), "\n")
	if expectedComment != "" && lines[1] != expectedComment {
		return nil, false, errors.New("GCX freshness comment does not match its header")
	}
	for i, line := range lines {
		if strings.HasPrefix(line, wire.Tag) {
			keys := make(map[string]bool)
			for _, token := range strings.Fields(strings.TrimPrefix(line, wire.Tag)) {
				key, _, _ := strings.Cut(token, "=")
				if keys[key] {
					return nil, false, errors.New("duplicate GCX header key " + key)
				}
				keys[key] = true
			}
		}
		if strings.HasPrefix(line, wire.CommentPrefix) && (i != 1 || line != expectedComment) {
			return nil, false, errors.New("unexpected GCX dead-code comment")
		}
	}
	return fields, pending, nil
}

func editConsumerGCXRows(decoder *wire.Decoder, header wire.Header, tool string, seen map[string]bool) ([]any, error) {
	count := header.Meta["count"]
	n, err := strconv.Atoi(count)
	if header.Tool != tool || !slices.Equal(header.Fields, editConsumerGCXFields) ||
		err != nil || n < 0 || n >= editConsumerDeadCodeLimit || strconv.Itoa(n) != count {
		return nil, errors.New("invalid GCX section or unproved row count for " + tool)
	}
	rows := make([]any, 0, n)
	for {
		row, err := decoder.NextRow()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line, err := strconv.ParseUint(row["line"], 10, 63)
		if len(rows) == n || err != nil || line == 0 || row["id"] == "" || row["kind"] == "" ||
			row["name"] == "" || row["path"] == "" || seen[row["id"]] {
			return nil, errors.New("invalid or duplicate GCX subject row")
		}
		seen[row["id"]] = true
		rows = append(rows, map[string]any{"id": row["id"], "kind": row["kind"], "name": row["name"],
			"file_path": row["path"], "line": json.Number(row["line"])})
	}
	if len(rows) != n {
		return nil, errors.New("GCX section row count does not match its header")
	}
	return rows, nil
}
