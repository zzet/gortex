package main

import (
	"errors"
	"fmt"
	"go/format"
	"path"
	"strings"
)

type readbackReferenceTrial struct {
	Ordinal       int                               `json:"ordinal"`
	Editor        int                               `json:"editor"`
	DueNS         int64                             `json:"due_ns"`
	Kind          string                            `json:"kind"`
	Request       request                           `json:"request"`
	Before        map[string]readbackReferenceImage `json:"before"`
	After         map[string]readbackReferenceImage `json:"after"`
	ProbeFile     string                            `json:"probe_file"`
	Target        string                            `json:"target"`
	Absent        bool                              `json:"absent"`
	TransactionID string                            `json:"transaction_id,omitempty"`
}

func readbackReferenceFormat(source string) (string, error) {
	b, err := format.Source([]byte(source))
	return string(b), err
}
func readbackReferenceTrialFor(cell readbackReferenceCell, arrival editProbeArrival, ordinal int) (readbackReferenceTrial, error) {
	if cell.Language != "" && cell.Language != "go" {
		return readbackReferenceLanguageTrialFor(cell, arrival, ordinal)
	}
	out := readbackReferenceTrial{Ordinal: ordinal, Editor: arrival.Editor, DueNS: arrival.DueNS, Kind: cell.Kind, Before: map[string]readbackReferenceImage{}, After: map[string]readbackReferenceImage{}}
	if err := cell.validate(); err != nil {
		return out, err
	}
	name := fmt.Sprintf("GortexReadback%03d", ordinal)
	stable := name + "Stable"
	callee := name + "Callee"
	replacement := name + "Renamed"
	file := fmt.Sprintf("zz_gortex_readback_probe/%s/p%03d.go", cell.Kind, ordinal)
	destination := fmt.Sprintf("zz_gortex_readback_probe/%s/d%03d.go", cell.Kind, ordinal)
	header := "package readbackprobe\n\n"
	target := fmt.Sprintf("func %s() int { return 7 }\n", name)
	survivor := fmt.Sprintf("func %s() int { return 2 }\n", stable)
	before := header + target + "\n" + survivor
	var after string
	out.ProbeFile, out.Target = file, name
	switch cell.Kind {
	case "append_leaf":
		before = header + survivor
		after = header + survivor + "\n" + target
	case "modify_body":
		after = header + fmt.Sprintf("func %s() int {\n value := 7\n return value\n}\n\n", name) + survivor
	case "change_signature":
		after = header + fmt.Sprintf("func %s(optional int) int { return 7 + optional }\n\n", name) + survivor
	case "add_intrafile_call":
		helper := fmt.Sprintf("\nfunc %s() int { return 7 }\n", callee)
		before += helper
		after = strings.Replace(before, "return 7", "return "+callee+"()", 1)
	case "modify_caller_body":
		target = fmt.Sprintf("func %s() int { return %s() }\n", name, callee)
		helper := fmt.Sprintf("\nfunc %s() int { return 7 }\n", callee)
		before = header + target + "\n" + survivor + helper
		after = header + fmt.Sprintf("func %s() int {\n value := 0\n return value + %s()\n}\n\n", name, callee) + survivor + helper
	case "add_crossfile_call":
		after = strings.Replace(before, "return 7", "return "+callee+"()", 1)
	case "rename":
		after = strings.Replace(before, "func "+name+"()", "func "+replacement+"()", 1)
		out.Target = replacement
	case "delete":
		after = header + survivor
		out.Absent = true
	case "relocate":
		before = header + target + "\n" + survivor
		after = header + survivor
		out.ProbeFile = destination
	default:
		return out, errors.New("unsupported readback fixture kind")
	}
	b, err := readbackReferenceFormat(before)
	if err != nil {
		return out, err
	}
	a, err := readbackReferenceFormat(after)
	if err != nil {
		return out, err
	}
	put := func(into map[string]readbackReferenceImage, file, source string) error {
		image, err := readbackReferenceParse(file, source)
		if err == nil {
			into[file] = image
		}
		return err
	}
	if err := put(out.Before, file, b); err != nil {
		return out, err
	}
	if err := put(out.After, file, a); err != nil {
		return out, err
	}
	if cell.Kind == "add_crossfile_call" {
		helper, err := readbackReferenceFormat(header + fmt.Sprintf("func %s() int { return 7 }\n", callee))
		if err != nil {
			return out, err
		}
		helperFile := path.Join(path.Dir(file), fmt.Sprintf("callee%03d.go", ordinal))
		if err := put(out.Before, helperFile, helper); err != nil {
			return out, err
		}
		if err := put(out.After, helperFile, helper); err != nil {
			return out, err
		}
	}
	if cell.Kind == "relocate" {
		destBefore, err := readbackReferenceFormat(header + fmt.Sprintf("func %sDestination() int { return 3 }\n", name))
		if err != nil {
			return out, err
		}
		destAfter, err := readbackReferenceFormat(destBefore + "\n" + target)
		if err != nil {
			return out, err
		}
		if err := put(out.Before, destination, destBefore); err != nil {
			return out, err
		}
		if err := put(out.After, destination, destAfter); err != nil {
			return out, err
		}
	}
	if err := readbackReferenceSetMutationRequest(&out, cell, file, name, replacement, b, a, destination); err != nil {
		return out, err
	}
	return out, nil
}

func readbackReferenceSetMutationRequest(out *readbackReferenceTrial, cell readbackReferenceCell, file, name, replacement, before, after, destination string) error {
	options := map[string]any{"mutation_id": fmt.Sprintf("readback-%s-%03d", cell.Kind, out.Ordinal), "dry_run": false}
	switch cell.Tool {
	case "edit.file":
		out.Request = request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": file}, "match": before, "replacement": after, "options": options}}
	case "edit.symbol":
		old := out.Before[file].Symbols[name]
		newBody := out.After[file].Symbols[name].Body
		out.Request = request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": file + "::" + name}, "match": old.Body, "replacement": newBody, "options": options}}
	case "edit.write":
		out.Request = request{Tool: "edit", Operation: "write", Arguments: map[string]any{"operation": "write", "target": map[string]any{"file": file}, "content": after, "options": options}}
	case "edit.batch":
		out.TransactionID = fmt.Sprintf("readback-batch-%s-%03d", cell.Kind, out.Ordinal)
		out.Request = editProbeBatchContentRequest(Corpus{File: file}, before, after, out.TransactionID)
	case "refactor.rename":
		out.Request = request{Tool: "refactor", Operation: "rename", Arguments: map[string]any{"operation": "rename", "target": map[string]any{"symbol": file + "::" + name}, "new_name": replacement, "dry_run": false}}
	case "refactor.delete":
		out.Request = request{Tool: "refactor", Operation: "delete", Arguments: map[string]any{"operation": "delete", "target": map[string]any{"symbol": file + "::" + name}, "dry_run": false, "force": false}}
	case "refactor.move":
		out.Request = request{Tool: "refactor", Operation: "move", Arguments: map[string]any{"operation": "move", "target": map[string]any{"symbol": file + "::" + name}, "destination": destination, "dry_run": false}}
	default:
		return errors.New("readback fixture has no declared mutation adapter")
	}
	return nil
}

func readbackReferenceProbeRequests(trial readbackReferenceTrial) map[string]request {
	target := map[string]any{"symbol": trial.ProbeFile + "::" + trial.Target}
	file := map[string]any{"file": trial.ProbeFile}
	return map[string]request{
		"source":          {Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": target, "options": map[string]any{"context_lines": 0, "compress_bodies": false}}},
		"summary":         {Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}},
		"editing_context": {Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}},
		"file":            {Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file", "target": file}},
		// The actual full-metadata handler is legacy-only on this producer.
		// read.symbol aliases source; no facade operation exposes get_symbol.
		"symbol": {Tool: "get_symbol", Arguments: map[string]any{"id": trial.ProbeFile + "::" + trial.Target, "detail": "full", "format": "json"}},
	}
}
