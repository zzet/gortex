package main

import (
	"go/token"
	"strings"
)

// This fixture cell permits only top-level zero-argument definitions with one
// four-space-indented return statement. Extra/nested bodies, strings, inline
// comments and unsupported syntax are refused rather than interpreted loosely.
func editProbePythonFixture(source, marker, callee string) (valid, correct bool) {
	lines := strings.Split(source, "\n")
	functions, matches := 0, 0
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "def ") || !strings.HasSuffix(line, "():") {
			return false, false
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, "def "), "():")
		if !token.IsIdentifier(name) {
			return false, false
		}
		i++
		if i >= len(lines) {
			return false, false
		}
		body := strings.TrimSuffix(lines[i], "\r")
		if !strings.HasPrefix(body, "    return ") {
			return false, false
		}
		expression := strings.TrimSpace(strings.TrimPrefix(body, "    return "))
		called := ""
		if strings.HasSuffix(expression, "()") {
			called = strings.TrimSuffix(expression, "()")
			if !token.IsIdentifier(called) {
				return false, false
			}
		} else {
			if expression == "" {
				return false, false
			}
			for _, digit := range expression {
				if digit < '0' || digit > '9' {
					return false, false
				}
			}
		}
		functions++
		if name == marker {
			matches++
			correct = called == callee
		}
	}
	return functions > 0, matches == 1 && correct
}
