package parser

import (
	"bytes"
	"path"
	"path/filepath"
	"strings"
)

// sniffPrefixCap bounds how much of a file the content probes scan.
// Language is overwhelmingly decided by the first few lines, so
// capping keeps detection cheap on large files.
const sniffPrefixCap = 64 * 1024

// shebangInterpreters maps a script interpreter basename to the Gortex
// language whose extractor should handle the file. Only interpreters
// with a registered extractor are listed; DetectLanguageContent still
// verifies the language is registered before returning it.
var shebangInterpreters = map[string]string{
	"sh":         "bash",
	"bash":       "bash",
	"zsh":        "bash",
	"ksh":        "bash",
	"dash":       "bash",
	"ash":        "bash",
	"python":     "python",
	"python2":    "python",
	"python3":    "python",
	"perl":       "perl",
	"ruby":       "ruby",
	"node":       "javascript",
	"nodejs":     "javascript",
	"deno":       "typescript",
	"bun":        "typescript",
	"php":        "php",
	"lua":        "lua",
	"tclsh":      "tcl",
	"wish":       "tcl",
	"pwsh":       "powershell",
	"powershell": "powershell",
	"groovy":     "groovy",
	"elixir":     "elixir",
	"julia":      "julia",
	"rscript":    "r",
}

// sniffShebang reads the interpreter from a leading `#!` line and maps
// it to a language. It handles direct interpreters (`#!/bin/bash`),
// the `env` indirection (`#!/usr/bin/env python3`, including `env -S`
// and `env VAR=val interp`), and trailing interpreter version
// suffixes (`python3.11`). Returns ("", false) when there is no
// shebang or the interpreter is unrecognised.
func sniffShebang(content []byte) (string, bool) {
	b := bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if len(b) < 2 || b[0] != '#' || b[1] != '!' {
		return "", false
	}
	line := b[2:]
	if nl := bytes.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return "", false
	}
	interp := strings.ToLower(path.Base(fields[0]))
	if interp == "env" {
		// `#!/usr/bin/env [-S] [VAR=val] interp` — the real
		// interpreter is the first token that is neither an env flag
		// nor a NAME=VALUE assignment.
		interp = ""
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") || strings.Contains(f, "=") {
				continue
			}
			interp = strings.ToLower(path.Base(f))
			break
		}
		if interp == "" {
			return "", false
		}
	}
	if lang, ok := shebangInterpreters[interp]; ok {
		return lang, true
	}
	if lang, ok := shebangInterpreters[trimInterpVersion(interp)]; ok {
		return lang, true
	}
	return "", false
}

// trimInterpVersion strips a trailing version suffix from an
// interpreter name: "python3.11" -> "python", "ruby2" -> "ruby".
func trimInterpVersion(interp string) string {
	end := len(interp)
	for end > 0 {
		c := interp[end-1]
		if (c >= '0' && c <= '9') || c == '.' {
			end--
			continue
		}
		break
	}
	return interp[:end]
}

// ambiguousExtensions is the set of extensions whose default language
// mapping is a guess that sniffAmbiguous may overturn from content. It
// gates sniffAmbiguous, so no extension can be refined without being
// listed here, and ExtensionNeedsContentProbe reports the same set to
// callers that detected a language before reading the file.
var ambiguousExtensions = map[string]bool{
	".json": true,
	".h":    true,
	".inc":  true,
	".m":    true,
	".xml":  true,
}

// ExtensionNeedsContentProbe reports whether a file's extension maps to
// more than one plausible language. A language detected for such a file
// without its bytes in hand is provisional — the extension's default,
// never a content-backed answer — so a caller that later reads the file
// must re-detect with the content before choosing an extractor.
func ExtensionNeedsContentProbe(filePath string) bool {
	return ambiguousExtensions[strings.ToLower(filepath.Ext(filePath))]
}

// sniffAmbiguous refines the language of a file whose extension maps
// to more than one plausible language. ext is the file extension
// (with leading dot). It returns (lang, true) when the content
// clearly indicates a specific language, and ("", false) when the
// extension is not ambiguous or the probe is inconclusive — the
// caller then keeps the extension's default mapping.
func sniffAmbiguous(filePath, ext string, content []byte) (string, bool) {
	if len(content) == 0 || !ambiguousExtensions[strings.ToLower(ext)] {
		return "", false
	}
	probe := content
	if len(probe) > sniffPrefixCap {
		probe = probe[:sniffPrefixCap]
	}
	switch strings.ToLower(ext) {
	case ".json":
		// A Shopify OS 2.0 JSON template / section group (templates/*.json,
		// sections/*.json) wires sections by `type`; every other .json keeps
		// the generic JSON default (package.json, tsconfig.json, …).
		if isShopifyThemeJSONPath(filePath) && hasShopifyTemplateMarkers(probe) {
			return "liquid_json", true
		}
	case ".h":
		// C / C++ / Objective-C header.
		if hasObjCMarkers(probe) {
			return "objc", true
		}
		if hasCppMarkers(probe) {
			return "cpp", true
		}
	case ".inc":
		// A contested extension: assembly, PHP, and Pascal each claim it by
		// default. Reroute to C only when the fragment carries an unmistakable
		// C signal (a C preprocessor directive, or a C block comment alongside
		// braces) and none of the other claimants' markers — so an X-macro /
		// generated-C ".inc" is parsed as C while PHP / Pascal / asm includes
		// keep their default language untouched.
		if hasCFragmentMarkers(probe) && !hasPHPMarkers(probe) && !hasPascalMarkers(probe) {
			return "c", true
		}
	case ".m":
		// Objective-C / MATLAB / Mathematica source.
		if hasObjCMarkers(probe) {
			return "objc", true
		}
		if hasMatlabMarkers(probe) {
			return "matlab", true
		}
		if hasMathematicaMarkers(probe) {
			return "mathematica", true
		}
	case ".xml":
		// A MyBatis mapper / Spring beans / ABA QIK descriptor XML routes
		// to its specific extractor; every other .xml keeps the generic
		// "xml" default.
		if hasMyBatisMapperMarkers(probe) {
			return "mybatis", true
		}
		if hasSpringBeansMarkers(probe) {
			return "spring", true
		}
		if hasQikXMLMarkers(probe) {
			return "qikxml", true
		}
	}
	return "", false
}

// isShopifyThemeJSONPath reports whether a path lives under a Shopify theme's
// `templates/` or `sections/` directory — the OS 2.0 JSON-template locations.
func isShopifyThemeJSONPath(filePath string) bool {
	p := "/" + strings.TrimPrefix(strings.ReplaceAll(filePath, "\\", "/"), "/")
	return strings.Contains(p, "/templates/") || strings.Contains(p, "/sections/")
}

// hasShopifyTemplateMarkers reports whether the JSON content carries the OS 2.0
// section-group shape — a `"sections"` object plus a `"type"` key naming a
// section file. A cheap byte pre-filter; the extractor parses and validates.
func hasShopifyTemplateMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(`"sections"`)) && bytes.Contains(b, []byte(`"type"`))
}


// hasQikXMLMarkers reports whether the content is an ABA/Sabre QIK
// LocalDescRef descriptor (DATAITEM or TABLE AppObjectDesc). Inlined in
// package parser to avoid an import cycle with languages.IsQikXML.
func hasQikXMLMarkers(b []byte) bool {
	lower := bytes.ToLower(b)
	if !bytes.Contains(lower, []byte("<localdescref")) {
		return false
	}
	return bytes.Contains(lower, []byte(`class="dataitem"`)) ||
		bytes.Contains(lower, []byte(`class="table"`)) ||
		bytes.Contains(lower, []byte(`class='dataitem'`)) ||
		bytes.Contains(lower, []byte(`class='table'`))
}

// hasMyBatisMapperMarkers reports whether the content is a MyBatis mapper
// XML document — a `<mapper` root element or the MyBatis mapper DTD. Kept
// in package parser (inlined rather than calling languages.IsMyBatisMapper)
// to avoid an import cycle.
func hasMyBatisMapperMarkers(b []byte) bool {
	return bytes.Contains(b, []byte("<mapper")) || bytes.Contains(b, []byte("mybatis.org//DTD Mapper"))
}

// hasSpringBeansMarkers reports whether the content is a Spring beans XML
// document — the Spring beans schema namespace, or a `<beans>`/`<bean>`
// DTD-style document. Inlined in package parser to avoid an import cycle
// with languages.IsSpringBeansXML.
func hasSpringBeansMarkers(b []byte) bool {
	return bytes.Contains(b, []byte("springframework.org/schema/beans")) ||
		(bytes.Contains(b, []byte("<beans")) && bytes.Contains(b, []byte("<bean")))
}

// hasObjCMarkers reports whether the content carries a syntax
// construct unique to Objective-C.
func hasObjCMarkers(b []byte) bool {
	for _, m := range []string{
		"@interface", "@implementation", "@protocol", "@property", "#import ",
	} {
		if bytes.Contains(b, []byte(m)) {
			return true
		}
	}
	return false
}

// hasCppMarkers reports whether the content carries a syntax construct
// that is valid C++ but not C — enough to type an ambiguous `.h`
// header as C++ rather than C.
func hasCppMarkers(b []byte) bool {
	for _, m := range []string{
		"::", "namespace ", "template<", "template <",
		"public:", "private:", "protected:", "virtual ",
		"nullptr", "constexpr",
	} {
		if bytes.Contains(b, []byte(m)) {
			return true
		}
	}
	return false
}

// hasCFragmentMarkers reports whether a contested-extension file carries an
// unmistakable C signal — a line-leading C preprocessor directive, or a C block
// comment together with a brace (the shape of a generated initializer / X-macro
// table). Deliberately conservative so it never steals a PHP / Pascal / asm
// include.
func hasCFragmentMarkers(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.TrimLeft(line, " \t")
		for _, d := range [][]byte{
			[]byte("#include"), []byte("#define"), []byte("#ifndef"),
			[]byte("#ifdef"), []byte("#pragma"), []byte("#undef"), []byte("#if "),
		} {
			if bytes.HasPrefix(t, d) {
				return true
			}
		}
	}
	return bytes.Contains(b, []byte("/*")) && bytes.Contains(b, []byte("{"))
}

// hasPHPMarkers reports whether the content opens a PHP block — the marker that
// keeps a Drupal `.inc` / `.module` include on the PHP extractor.
func hasPHPMarkers(b []byte) bool {
	return bytes.Contains(b, []byte("<?php")) || bytes.Contains(b, []byte("<?=")) ||
		bytes.Contains(b, []byte("<?\n")) || bytes.Contains(b, []byte("<? "))
}

// hasPascalMarkers reports whether the content looks like a Pascal / Delphi
// include — a unit/section keyword or a `{$...}` compiler directive.
func hasPascalMarkers(b []byte) bool {
	if bytes.Contains(b, []byte("{$")) {
		return true
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.ToLower(bytes.TrimLeft(line, " \t"))
		for _, kw := range [][]byte{
			[]byte("unit "), []byte("program "), []byte("interface"),
			[]byte("implementation"), []byte("procedure "), []byte("uses "),
		} {
			if bytes.HasPrefix(t, kw) {
				return true
			}
		}
	}
	return false
}

// hasMatlabMarkers reports whether the content looks like MATLAB —
// a `classdef` or a line-leading `function` definition.
func hasMatlabMarkers(b []byte) bool {
	if bytes.Contains(b, []byte("classdef ")) {
		return true
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.TrimLeft(line, " \t")
		if bytes.HasPrefix(t, []byte("function ")) || bytes.HasPrefix(t, []byte("function[")) {
			return true
		}
	}
	return false
}

// hasMathematicaMarkers reports whether the content looks like a
// Wolfram Language / Mathematica package. The probes are deliberately
// conservative — a `BeginPackage[` call or a `(* ::` cell marker —
// so a C-style `(*ptr)` dereference never trips it.
func hasMathematicaMarkers(b []byte) bool {
	return bytes.Contains(b, []byte("BeginPackage[")) ||
		bytes.Contains(b, []byte("(* ::")) ||
		bytes.Contains(b, []byte("(*::"))
}
