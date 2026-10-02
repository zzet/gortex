package mcp

import (
	"strings"
	"testing"
)

func TestParseGateLanguage(t *testing.T) {
	cases := map[string]string{
		"foo.go":        "go",
		"a/b/c.py":      "python",
		"x.tsx":         "tsx",
		"x.ts":          "typescript",
		"x.jsx":         "javascript",
		"main.rs":       "rust",
		"App.java":      "java",
		"EA.mq5":        "mql",
		"ea.mq4":        "mql",
		"include.mqh":   "mql",
		"README.md":     "",
		"data.json":     "",
		"Makefile":      "",
		"noext":         "",
		"weird.UNKNOWN": "",
	}
	for path, want := range cases {
		if got := parseGateLanguage(path); got != want {
			t.Errorf("parseGateLanguage(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestParseErrorCountGo(t *testing.T) {
	clean := []byte("package main\n\nfunc Add(a, b int) int { return a + b }\n")
	if n, ok := parseErrorCount("go", clean); !ok || n != 0 {
		t.Fatalf("clean Go: got (%d, %v), want (0, true)", n, ok)
	}

	broken := []byte("package main\n\nfunc Add(a, b int) int { return a + \n")
	if n, ok := parseErrorCount("go", broken); !ok || n == 0 {
		t.Fatalf("broken Go: got (%d, %v), want (>0, true)", n, ok)
	}

	// A NUL byte is a content verdict (parser.ErrBinarySource), not a
	// parse-infrastructure failure: the gate must keep its opinion so an
	// edit that would write binary bytes is treated as a regression.
	nul := []byte("package main\n\nfunc Add(a, b int) int { return a\x00 + b }\n")
	if n, ok := parseErrorCount("go", nul); !ok || n == 0 {
		t.Fatalf("NUL-bearing Go: got (%d, %v), want (>0, true)", n, ok)
	}

	// Unsupported language degrades to no-opinion.
	if n, ok := parseErrorCount("cobol", clean); ok || n != 0 {
		t.Fatalf("unsupported lang: got (%d, %v), want (0, false)", n, ok)
	}
	if n, ok := parseErrorCount("", clean); ok || n != 0 {
		t.Fatalf("empty lang: got (%d, %v), want (0, false)", n, ok)
	}

	// MQL rides the tree-sitter-cpp-fork grammar; the gate must have an
	// opinion (regression: .mq* fell through and the gate silently skipped).
	mqlClean := []byte("int Add(int a, int b) { return a + b; }\n")
	if n, ok := parseErrorCount("mql", mqlClean); !ok || n != 0 {
		t.Fatalf("clean MQL: got (%d, %v), want (0, true)", n, ok)
	}
	mqlBroken := []byte("int Add(int a, int b { return a + b; }\n")
	if n, ok := parseErrorCount("mql", mqlBroken); !ok || n == 0 {
		t.Fatalf("broken MQL: got (%d, %v), want (>0, true)", n, ok)
	}
}

func TestCheckParseGate(t *testing.T) {
	clean := []byte("package main\n\nfunc Add(a, b int) int { return a + b }\n")
	broken := []byte("package main\n\nfunc Add(a, b int) int { return a + \n")

	// clean -> broken: a regression, must block.
	if r := checkParseGate("x.go", clean, broken); !r.Checked || !r.Blocked {
		t.Errorf("clean->broken: got %+v, want Checked && Blocked", r)
	}
	// clean -> NUL-bearing: the ErrBinarySource verdict counts as a
	// regression, so an edit that writes binary bytes is refused.
	nul := []byte("package main\n\nfunc Add(a, b int) int { return a\x00 + b }\n")
	if r := checkParseGate("x.go", clean, nul); !r.Checked || !r.Blocked {
		t.Errorf("clean->nul: got %+v, want Checked && Blocked", r)
	}
	// clean -> clean: no regression.
	if r := checkParseGate("x.go", clean, clean); !r.Checked || r.Blocked {
		t.Errorf("clean->clean: got %+v, want Checked && !Blocked", r)
	}
	// broken -> broken: never block an edit to an already-broken file.
	if r := checkParseGate("x.go", broken, broken); !r.Checked || r.Blocked {
		t.Errorf("broken->broken: got %+v, want Checked && !Blocked", r)
	}
	// broken -> clean: a fix, never blocked.
	if r := checkParseGate("x.go", broken, clean); !r.Checked || r.Blocked {
		t.Errorf("broken->clean: got %+v, want Checked && !Blocked", r)
	}
	// new file (nil old) -> broken: any error is a regression.
	if r := checkParseGate("x.go", nil, broken); !r.Checked || !r.Blocked {
		t.Errorf("new->broken: got %+v, want Checked && Blocked", r)
	}
	// unsupported language: gate does not run, never blocks.
	if r := checkParseGate("README.md", clean, broken); r.Checked || r.Blocked {
		t.Errorf("unsupported: got %+v, want !Checked && !Blocked", r)
	}
}

// TestCheckParseGate_BinaryVerdict pins the text→binary transition as a
// verdict decided before the error counts: scoring a NUL as exactly one
// parse error would let a NUL slip into a file that already has two
// tree-sitter errors (new=1 < old=2) — an edit a count-only gate allows,
// but which main's parse refused because there the NUL added errors on
// top of the existing ones.
func TestCheckParseGate_BinaryVerdict(t *testing.T) {
	twoErrs := []byte("package main\n\nfunc A() { x := }\nfunc B() { y := }\n")
	nul := []byte("package main\n\nfunc A() { x := }\nfunc B() { y := \"a\x00b\" }\n")

	// broken -> NUL-bearing: the transition blocks regardless of counts.
	if r := checkParseGate("a.go", twoErrs, nul); !r.Checked || !r.Blocked || !r.BecameBinary {
		t.Errorf("broken->nul: got %+v, want Checked && Blocked && BecameBinary", r)
	}
	// Binary -> binary: no baseline to regress, never blocked.
	if r := checkParseGate("a.go", nul, nul); !r.Checked || r.Blocked || r.BecameBinary {
		t.Errorf("nul->nul: got %+v, want Checked && !Blocked && !BecameBinary", r)
	}
	// Binary -> text is a fix (binary carries no parse-error baseline).
	if r := checkParseGate("a.go", nul, twoErrs); !r.Checked || r.Blocked {
		t.Errorf("nul->broken: got %+v, want Checked && !Blocked", r)
	}
}

func TestParseGateErrorBinary(t *testing.T) {
	msg := parseGateError("a.go", parseGateResult{Checked: true, Blocked: true, BecameBinary: true, Language: "go"})
	if !strings.Contains(msg, "binary (NUL-bearing)") || strings.Contains(msg, "new go parse error") {
		t.Errorf("binary refusal message = %q, want the binary wording without a count delta", msg)
	}
}

func TestParseGateInfo(t *testing.T) {
	if parseGateInfo(parseGateResult{}, false) != nil {
		t.Error("unchecked gate should produce no info")
	}
	if parseGateInfo(parseGateResult{Checked: true}, false) != nil {
		t.Error("clean->clean gate should stay quiet")
	}
	info := parseGateInfo(parseGateResult{Checked: true, Blocked: true, OldErrors: 0, NewErrors: 2, Language: "go"}, false)
	if info == nil || info["blocked"] != true {
		t.Errorf("blocked gate info = %v, want blocked:true", info)
	}
	info = parseGateInfo(parseGateResult{Checked: true, Blocked: true, NewErrors: 2, Language: "go"}, true)
	if info == nil || info["blocked"] != false || info["overridden"] != true {
		t.Errorf("overridden gate info = %v, want blocked:false overridden:true", info)
	}
	// A text→binary block carries no count delta but must still report.
	info = parseGateInfo(parseGateResult{Checked: true, Blocked: true, BecameBinary: true, Language: "go"}, false)
	if info == nil || info["blocked"] != true || info["became_binary"] != true {
		t.Errorf("binary gate info = %v, want blocked:true became_binary:true", info)
	}
}
