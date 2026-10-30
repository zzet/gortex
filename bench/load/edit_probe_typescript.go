package main

import (
	"path/filepath"
	"strings"
	"text/scanner"
)

// This cell uses explicit numeric, zero-argument function fixtures. It is not
// an arbitrary TypeScript parser: comments/strings cannot supply a declaration
// or call, and unsupported syntax fails coverage instead of becoming proof.
func editProbeTypeScriptFixture(source, marker, callee string) (valid, correct bool) {
	var scan scanner.Scanner
	scan.Init(strings.NewReader(source))
	invalid := false
	scan.Error = func(*scanner.Scanner, string) { invalid = true }
	kind, text := scan.Scan(), scan.TokenText()
	advance := func() { kind = scan.Scan(); text = scan.TokenText() }
	take := func(want string) bool {
		if text != want {
			return false
		}
		advance()
		return true
	}
	functions, matches := 0, 0
	for kind != scanner.EOF {
		if text == "export" {
			advance()
		}
		if !take("function") || kind != scanner.Ident {
			return false, false
		}
		name := text
		advance()
		for _, token := range []string{"(", ")", ":", "number", "{", "return"} {
			if !take(token) {
				return false, false
			}
		}
		called := ""
		if kind == scanner.Int {
			for _, digit := range text {
				if digit < '0' || digit > '9' {
					return false, false
				}
			}
			advance()
		} else if kind == scanner.Ident {
			called = text
			advance()
			if !take("(") || !take(")") {
				return false, false
			}
		} else {
			return false, false
		}
		if !take(";") || !take("}") {
			return false, false
		}
		functions++
		if name == marker {
			matches++
			correct = called == callee
		}
	}
	return !invalid && functions > 0, !invalid && matches == 1 && correct
}

func editProbeFixtureSource(file, source, marker, callee string) bool {
	switch filepath.Ext(file) {
	case ".go":
		return editProbeSourceCall(source, marker, callee)
	case ".ts":
		valid, correct := editProbeTypeScriptFixture(source, marker, callee)
		return valid && correct
	default:
		return false
	}
}

func editProbeFixturePath(path, file string) bool {
	return path == file || strings.HasSuffix(path, "/"+file)
}
