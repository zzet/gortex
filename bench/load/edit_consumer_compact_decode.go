package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type editCompactConsumerWire struct{ operation string }

const editCompactConsumerWireKey = "\x00load.compact.consumer"

func editCompactConsumerOperation(operation string) bool {
	return operation == "refactor.rename" || operation == "change.verify" || operation == "analyze.dead_code" || operation == "change.detect"
}

func editCompactConsumerOperationFor(name string) string {
	switch name {
	case "consumer_rename":
		return "refactor.rename"
	case "consumer_verify":
		return "change.verify"
	case "consumer_dead_code":
		return "analyze.dead_code"
	case "consumer_detect":
		return "change.detect"
	default:
		return ""
	}
}

// This decoder consumes actual, versioned producer records. Counts, subject
// identities and paths come from the wire, never from the requested fixture.
// It deliberately supplies no idle freshness field. A labelled pending answer
// retains its exact scope and bounded paths, and can never become completion.
func decodeEditCompactConsumer(raw json.RawMessage, operation string) (map[string]any, error) {
	text, err := editOutputPayload(raw, false)
	if err != nil {
		return nil, err
	}
	text, tiers, err := editProbeTierPrefix(text)
	if err != nil {
		return nil, err
	}
	if len(text) == 0 || json.Valid(text) || text[len(text)-1] != '\n' {
		return nil, errors.New("compact consumer requires complete non-JSON records ending in newline")
	}
	lines := strings.Split(string(text[:len(text)-1]), "\n")
	header := strings.Split(lines[0], "\t")
	body := make(map[string]any)
	var bad error
	str := func(raw string) string {
		value, e := strconv.Unquote(raw)
		if e != nil || len(raw) < 2 || raw[0] != '"' {
			bad = errors.New("compact consumer string is not quoted")
		}
		return value
	}
	num := func(raw string) json.Number {
		_, e := strconv.ParseUint(raw, 10, 63)
		if e != nil || raw == "" || len(raw) > 1 && raw[0] == '0' || strings.HasPrefix(raw, "+") {
			bad = errors.New("compact consumer count is not an exact nonnegative integer")
		}
		return json.Number(raw)
	}
	boolean := func(raw string) bool {
		if raw != "true" && raw != "false" {
			bad = errors.New("compact consumer boolean is invalid")
		}
		return raw == "true"
	}
	counts := make(map[string]json.Number)
	switch {
	case operation == "refactor.rename" && len(header) == 5 && header[0] == "rename_v1":
		body["old_name"], body["new_name"], body["dry_run"] = str(header[1]), str(header[2]), boolean(header[3])
		counts["edits"] = num(header[4])
		body["edits"] = []any{}
		if body["dry_run"] != true {
			return nil, errors.New("compact rename consumer requires actual dry-run output")
		}
	case operation == "change.verify" && len(header) == 6 && header[0] == "verify_v1":
		body["clean"], body["checked_callers"], body["checked_impls"] = boolean(header[1]), num(header[2]), num(header[3])
		counts["violations"], counts["errors"] = num(header[4]), num(header[5])
		body["violations"], body["errors"] = []any{}, []any{}
	case operation == "analyze.dead_code" && len(header) == 4 && header[0] == "dead_code_v1":
		body["total"], body["pending_unknown_count"], body["truncated"] = num(header[1]), num(header[2]), boolean(header[3])
		counts["dead_code"], counts["pending_unknown"] = body["total"].(json.Number), body["pending_unknown_count"].(json.Number)
		body["dead_code"], body["pending_unknown"] = []any{}, []any{}
	case operation == "change.detect" && len(header) == 4 && header[0] == "detect_v1":
		body["scope"] = str(header[1])
		counts["changed_files"], counts["changed_symbols"] = num(header[2]), num(header[3])
		body["changed_files"], body["changed_symbols"] = []any{}, []any{}
	default:
		return nil, errors.New("missing or mismatched compact consumer census header")
	}
	if bad != nil {
		return nil, bad
	}
	seen, records := make(map[string]bool), make(map[string]bool)
	var pendingFiles []any
	pendingStage := false
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "pending_file ") {
			path := str(strings.TrimPrefix(line, "pending_file "))
			if path == "" || seen["pending_file:"+path] {
				return nil, errors.New("empty or duplicate compact pending path")
			}
			seen["pending_file:"+path] = true
			pendingFiles = append(pendingFiles, path)
			pendingStage = true
			continue
		}
		if pendingStage {
			return nil, errors.New("compact consumer records follow the pending path footer")
		}
		fields := strings.Split(line, "\t")
		if records[line] {
			return nil, errors.New("duplicate compact consumer record")
		}
		records[line] = true
		appendRow := func(key string, row any) { body[key] = append(body[key].([]any), row) }
		switch tag := fields[0]; {
		case operation == "refactor.rename" && tag == "edit" && len(fields) == 7:
			file, line := str(fields[1]), num(fields[2])
			key := "edit:" + file + ":" + line.String()
			if file == "" || seen[key] {
				return nil, errors.New("empty or duplicate compact edit location")
			}
			seen[key] = true
			appendRow("edits", map[string]any{"file": file, "line": line, "old_text": str(fields[3]), "new_text": str(fields[4]), "confidence": str(fields[5]), "reason": str(fields[6])})
		case operation == "refactor.rename" && tag == "rename_status" && len(fields) == 3 && !seen[tag]:
			body["status"], body["written"] = str(fields[1]), boolean(fields[2])
			seen[tag] = true
			if body["written"] != false || body["status"] != "would_apply" && body["status"] != "no_edits" && body["status"] != "provisional_only" {
				return nil, errors.New("compact dry-run status is contradictory")
			}
		case operation == "change.verify" && tag == "violation" && len(fields) == 7:
			appendRow("violations", map[string]any{"kind": str(fields[1]), "symbol_id": str(fields[2]), "name": str(fields[3]), "file_path": str(fields[4]), "line": num(fields[5]), "description": str(fields[6])})
		case operation == "change.verify" && tag == "error" && len(fields) == 2:
			appendRow("errors", str(fields[1]))
		case operation == "change.verify" && strings.HasPrefix(line, "return_usage "):
			row, err := editCompactLegacyReturnUsage(line)
			if err != nil {
				return nil, err
			}
			id := row["symbol_id"].(string)
			if seen["return_usage:"+id] {
				return nil, errors.New("duplicate return-usage subject")
			}
			seen["return_usage:"+id] = true
			if _, exists := body["return_usage"]; !exists {
				body["return_usage"] = []any{}
			}
			appendRow("return_usage", row)
		case operation == "analyze.dead_code" && (tag == "dead" || tag == "pending_unknown") && len(fields) == 6:
			key := "dead_code"
			if tag == "pending_unknown" {
				key = "pending_unknown"
			}
			id := str(fields[2])
			if id == "" || seen["dead_id:"+id] {
				return nil, errors.New("empty or contradictory compact dead-code identity")
			}
			seen["dead_id:"+id] = true
			appendRow(key, map[string]any{"kind": str(fields[1]), "id": id, "name": str(fields[3]), "file_path": str(fields[4]), "start_line": num(fields[5])})
		case operation == "change.detect" && tag == "changed_file" && len(fields) == 2:
			file := str(fields[1])
			if file == "" || seen["changed_file:"+file] {
				return nil, errors.New("empty or duplicate compact changed file")
			}
			seen["changed_file:"+file] = true
			appendRow("changed_files", file)
		case operation == "change.detect" && tag == "changed_symbol" && len(fields) == 6:
			id := str(fields[2])
			if id == "" || seen["changed_symbol:"+id] {
				return nil, errors.New("empty or duplicate compact changed symbol")
			}
			seen["changed_symbol:"+id] = true
			appendRow("changed_symbols", map[string]any{"kind": str(fields[1]), "id": id, "name": str(fields[3]), "file_path": str(fields[4]), "start_line": num(fields[5])})
		case operation == "change.detect" && tag == "complete" && len(fields) == 2 && !seen[tag]:
			body[tag] = boolean(fields[1])
			seen[tag] = true
		case (operation == "change.detect" && (tag == "risk" || tag == "summary" || tag == "note" || tag == "graph_status" || tag == "detail" || tag == "repo" || tag == "repo_root") || operation == "analyze.dead_code" && tag == "note") && len(fields) == 2 && !seen[tag]:
			body[tag] = str(fields[1])
			seen[tag] = true
		default:
			return nil, fmt.Errorf("unknown, malformed or duplicate compact %s record %q", operation, line)
		}
		if bad != nil {
			return nil, bad
		}
	}
	if bad != nil {
		return nil, bad
	}
	for key, count := range counts {
		n, _ := strconv.ParseUint(count.String(), 10, 63)
		if n != uint64(len(body[key].([]any))) {
			return nil, errors.New("compact consumer row census is incomplete")
		}
	}
	if operation == "refactor.rename" && !seen["rename_status"] {
		return nil, errors.New("compact rename lacks its actual status")
	}
	if operation == "refactor.rename" && (body["status"] == "no_edits") != (len(body["edits"].([]any)) == 0) {
		return nil, errors.New("compact rename status contradicts its edit census")
	}
	if operation == "change.detect" {
		if status, exists := body["graph_status"]; exists {
			if status != "ready" && status != "pending" && status != "failed" {
				return nil, errors.New("unknown compact detect graph status")
			}
			if status != "ready" && body["complete"] != false {
				return nil, errors.New("unavailable compact detect graph must remain incomplete")
			}
		}
	}
	if operation == "change.verify" && body["clean"] != (len(body["violations"].([]any)) == 0) {
		return nil, errors.New("compact verify clean/violation contradiction")
	}
	if !editConsumerPayloadValid(body) {
		return nil, errors.New("compact consumer reports errors or truncation")
	}
	if len(tiers) > 0 {
		labels := tiers[editProbeTierLabelsKey].(editProbeTierLabels)
		wantScope := "relations"
		if operation == "analyze.dead_code" {
			wantScope = "derived"
		}
		if len(labels) != 1 || labels[0].Scope != wantScope {
			return nil, errors.New("compact consumer has a different or ambiguous tier scope")
		}
		n, _ := strconv.ParseUint(labels[0].Count.String(), 10, 63)
		if uint64(len(pendingFiles)) != min(n, uint64(16)) {
			return nil, errors.New("compact consumer pending paths lack the actual bounded census")
		}
		if labels[0].State != "fresh" {
			if body["complete"] == true {
				return nil, errors.New("compact consumer claims complete while labelled pending")
			}
			body["complete"] = false
		} else if body["complete"] == false {
			return nil, errors.New("compact consumer claims fresh while incomplete")
		}
		if err := editProbeKeepTierFields(body, tiers); err != nil {
			return nil, err
		}
		body["pending_files"] = pendingFiles
	} else if len(pendingFiles) > 0 {
		return nil, errors.New("compact pending paths have no producer scope label")
	}
	body[editCompactConsumerWireKey] = editCompactConsumerWire{operation: operation}
	return body, nil
}

// The legacy producer writes one distribution per changed function. Its
// classified counts plus the optional final unclassified count must account
// for every call site. This census is independent of CheckedCallers (which
// also includes the verifier's traversal); it never supplies that field.
func editCompactLegacyReturnUsage(line string) (map[string]any, error) {
	if !strings.HasPrefix(line, "return_usage ") || strings.ContainsAny(line, "\t\r\n") || strings.Count(line, " call_sites:") != 1 {
		return nil, errors.New("malformed legacy return-usage line")
	}
	id, tail, _ := strings.Cut(strings.TrimPrefix(line, "return_usage "), " call_sites:")
	if id == "" || id != strings.TrimSpace(id) {
		return nil, errors.New("missing legacy return-usage subject")
	}
	fields := strings.Split(tail, " ")
	parse := func(raw string) (uint64, error) {
		n, err := strconv.ParseUint(raw, 10, 63)
		if err != nil || raw == "" || len(raw) > 1 && raw[0] == '0' || strings.HasPrefix(raw, "+") {
			return 0, errors.New("invalid legacy return-usage count")
		}
		return n, nil
	}
	total, err := parse(fields[0])
	if err != nil || total == 0 {
		return nil, errors.New("invalid legacy call-site census")
	}
	row := map[string]any{"symbol_id": id, "call_sites": json.Number(fields[0])}
	counts := make(map[string]any)
	seen := make(map[string]bool)
	sum := uint64(0)
	for i, field := range fields[1:] {
		label, raw, ok := strings.Cut(field, ":")
		if !ok || label == "" || label == "call_sites" || seen[label] {
			return nil, errors.New("invalid or duplicate legacy return-usage category")
		}
		for _, c := range label {
			if c != '_' && (c < 'a' || c > 'z') {
				return nil, errors.New("invalid legacy return-usage category")
			}
		}
		seen[label] = true
		n, err := parse(raw)
		if err != nil || n > total-sum {
			return nil, errors.New("legacy return-usage exceeds its call-site census")
		}
		sum += n
		if label == "unclassified" {
			if i != len(fields)-2 {
				return nil, errors.New("legacy unclassified count must be last")
			}
			row["unclassified"] = json.Number(raw)
		} else {
			counts[label] = json.Number(raw)
		}
	}
	if sum != total {
		return nil, errors.New("legacy return-usage call-site census is incomplete")
	}
	if len(counts) > 0 {
		row["counts"] = counts
	}
	return row, nil
}
