package main

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

func readbackReferenceLanguageExtension(language string) (string, error) {
	switch language {
	case "", "go":
		return ".go", nil
	case "typescript":
		return ".ts", nil
	case "python":
		return ".py", nil
	}
	return "", errors.New("readback measurement supports the existing Go/TypeScript/Python fixtures only")
}

type readbackReferenceLiteralFunction struct {
	name, source string
	optional     bool
}

// These are producer-owned literal fixture fragments, not arbitrary source or
// a replacement parser. Positions come from the exact emitted chunks before
// any product response is observed. Extra/foreign declarations cannot supply
// a missing expected function in the existing strict response checker.
func readbackReferenceLiteralImage(file, prefix string, functions []readbackReferenceLiteralFunction) (readbackReferenceImage, error) {
	image := readbackReferenceImage{File: file, Symbols: map[string]readbackReferenceSymbol{}, Auxiliary: map[string]readbackReferenceDefinition{}}
	var source strings.Builder
	source.WriteString(prefix)
	line := 1 + strings.Count(prefix, "\n")
	for i, fn := range functions {
		if !simpleSymbolName(fn.name) || fn.source == "" {
			return image, errors.New("invalid literal readback declaration")
		}
		if _, duplicate := image.Symbols[fn.name]; duplicate {
			return image, errors.New("duplicate literal readback declaration")
		}
		if i != 0 {
			source.WriteString("\n\n")
			line += 2
		}
		body := strings.TrimSuffix(fn.source, "\n")
		end := line + strings.Count(body, "\n")
		image.Symbols[fn.name] = readbackReferenceSymbol{Name: fn.name, StartLine: line, EndLine: end, Body: body}
		if fn.optional {
			image.Auxiliary[fn.name+"#param:optional@0"] = readbackReferenceDefinition{Name: "optional", Kind: "param", StartLine: line}
		}
		source.WriteString(body)
		line = end
	}
	source.WriteByte('\n')
	image.Bytes = source.String()
	return image, nil
}

func readbackReferenceLanguageTrialFor(cell readbackReferenceCell, arrival editProbeArrival, ordinal int) (readbackReferenceTrial, error) {
	out := readbackReferenceTrial{Ordinal: ordinal, Editor: arrival.Editor, DueNS: arrival.DueNS, Kind: cell.Kind, Before: map[string]readbackReferenceImage{}, After: map[string]readbackReferenceImage{}}
	if err := cell.validate(); err != nil {
		return out, err
	}
	if cell.Language != "typescript" && cell.Language != "python" {
		return out, errors.New("literal foreign fixture requires TypeScript or Python")
	}
	ext, _ := readbackReferenceLanguageExtension(cell.Language)
	file := fmt.Sprintf("zz_gortex_readback_probe/%s/%s/p%03d%s", cell.Language, cell.Kind, ordinal, ext)
	name := fmt.Sprintf("GortexReadback%03d", ordinal)
	stable, callee, replacement := name+"Stable", name+"Callee", name+"Renamed"
	declaration := func(symbol, expression string) readbackReferenceLiteralFunction {
		// Reuse the established shared1005 plain declaration shape.
		source := strings.Replace(editProbeFollowingDeclaration(file, symbol), "return 2", "return "+expression, 1)
		return readbackReferenceLiteralFunction{name: symbol, source: source}
	}
	target, survivor := declaration(name, "7"), declaration(stable, "2")
	before := []readbackReferenceLiteralFunction{target, survivor}
	after := append([]readbackReferenceLiteralFunction(nil), before...)
	prefix := ""
	out.ProbeFile, out.Target = file, name
	switch cell.Kind {
	case "append_leaf":
		before = []readbackReferenceLiteralFunction{survivor}
		after = []readbackReferenceLiteralFunction{survivor, target}
	case "modify_body":
		if cell.Language == "typescript" {
			after[0].source = "export function " + name + "(): number {\n return 8;\n}"
		} else {
			after[0].source = "def " + name + "():\n\n    return 8"
		}
	case "modify_caller_body":
		before[0].source = editProbeFollowingCaller(file, name, callee)
		after[0].source = "export function " + name + "(): number {\n " + callee + "();\n return " + callee + "();\n}"
		if cell.Language == "python" {
			after[0].source = "def " + name + "():\n    " + callee + "()\n    return " + callee + "()"
		}
		before = append(before, declaration(callee, "7"))
		after = append(after, declaration(callee, "7"))
	case "add_intrafile_call":
		after[0] = declaration(name, callee+"()")
		before = append(before, declaration(callee, "7"))
		after = append(after, declaration(callee, "7"))
	case "add_crossfile_call":
		after[0] = declaration(name, callee+"()")
		helper := path.Join(path.Dir(file), fmt.Sprintf("callee%03d%s", ordinal, ext))
		prefix = editProbeCrossFileImport(Corpus{File: path.Base(file), EditCallee: path.Base(helper) + "::" + callee}) + "\n"
		if strings.TrimSpace(prefix) == "" {
			return out, errors.New("existing shared cross-file import adapter unavailable")
		}
		image, err := readbackReferenceLiteralImage(helper, "", []readbackReferenceLiteralFunction{declaration(callee, "7")})
		if err != nil {
			return out, err
		}
		out.Before[helper], out.After[helper] = image, image
	case "change_signature":
		after[0].optional = true
		if cell.Language == "typescript" {
			after[0].source = "export function " + name + "(optional?: number): number { return 7; }"
		} else {
			after[0].source = "def " + name + "(optional=0):\n    return 7"
		}
	case "rename":
		after[0] = declaration(replacement, "7")
		out.Target = replacement
	case "delete":
		after = []readbackReferenceLiteralFunction{survivor}
		out.Absent = true
	default:
		return out, errors.New("unsupported literal readback fixture kind")
	}
	b, err := readbackReferenceLiteralImage(file, prefix, before)
	if err != nil {
		return out, err
	}
	a, err := readbackReferenceLiteralImage(file, prefix, after)
	if err != nil {
		return out, err
	}
	out.Before[file], out.After[file] = b, a
	if err := readbackReferenceSetMutationRequest(&out, cell, file, name, replacement, b.Bytes, a.Bytes, ""); err != nil {
		return out, err
	}
	return out, nil
}
