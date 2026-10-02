package mcp

import "testing"

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
}
