package main

import (
	"path/filepath"
	"strings"
)

// Only this fixture's optional/default parameter is admitted. Existing
// zero-argument callers remain valid; unsupported signatures are not parsed by
// a new generic language framework.
func editProbeSignaturePlain(file, source string) string {
	switch filepath.Ext(file) {
	case ".go":
		return strings.ReplaceAll(source, "(optional ...int)", "()")
	case ".ts":
		return strings.ReplaceAll(source, "(optional?: number)", "()")
	case ".py":
		return strings.ReplaceAll(source, "(optional=0)", "()")
	}
	return source
}
func editProbeBodyPlain(c Corpus, source string) string {
	if c.EditKind == "change_signature" {
		return editProbeSignaturePlain(c.File, source)
	}
	return source
}
func editProbeFollowingCaller(file, name, target string) string {
	switch filepath.Ext(file) {
	case ".go":
		return "func " + name + "() int { return " + target + "() }"
	case ".ts":
		return "export function " + name + "(): number { return " + target + "(); }"
	case ".py":
		return "def " + name + "():\n    return " + target + "()"
	}
	return ""
}
func editProbeBodySignature(rows []any, c Corpus, step editProbeStep, meta bool) bool {
	name := step.TargetSymbol[strings.LastIndex(step.TargetSymbol, "::")+2:]
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		id, _ := row["id"].(string)
		if !editProbeSourceID(id, c.File, name) {
			continue
		}
		if meta {
			row, ok = row["meta"].(map[string]any)
			if !ok {
				return false
			}
		}
		signature, _ := row["signature"].(string)
		switch filepath.Ext(c.File) {
		case ".go":
			return strings.Contains(signature, "optional")
		case ".ts":
			return signature == "function "+name+"()"
		case ".py":
			return signature == "def "+name+"(...)"
		}
		return false
	}
	return false
}
func editProbeBodySpan(body map[string]any, c Corpus, step editProbeStep) bool {
	rows, ok := body["nodes"].([]any)
	if !ok {
		return false
	}
	name := step.TargetSymbol[strings.LastIndex(step.TargetSymbol, "::")+2:]
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		id, _ := row["id"].(string)
		if editProbeSourceID(id, c.File, name) {
			start, se := strconvEditProbeUint(row["start_line"])
			end, ee := strconvEditProbeUint(row["end_line"])
			return se == nil && ee == nil && start > 0 && end >= start && end-start == uint64(step.ExpectedLineGap-2)
		}
	}
	return false
}

func editProbeSignatureHeaders(file, before, after, name string) bool {
	switch filepath.Ext(file) {
	case ".go":
		return strings.HasPrefix(before, "func "+name+"()") && strings.HasPrefix(after, "func "+name+"(optional ...int)")
	case ".ts":
		return (strings.HasPrefix(before, "export function "+name+"()") && strings.HasPrefix(after, "export function "+name+"(optional?: number)")) || (strings.HasPrefix(before, "function "+name+"()") && strings.HasPrefix(after, "function "+name+"(optional?: number)"))
	case ".py":
		return strings.HasPrefix(before, "def "+name+"():") && strings.HasPrefix(after, "def "+name+"(optional=0):")
	}
	return false
}
