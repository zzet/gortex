package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/parser"
)

// The pre-write parse gate refuses an edit_file / write_file that would leave
// a file *more* syntactically broken than it already is. It parses the
// candidate content with tree-sitter before the atomic swap, so a corrupting
// edit is rejected at gate time instead of being discovered after it has
// already landed on disk (and poisoned the graph). Only a regression blocks:
// editing an already-broken file, or one whose language the gate cannot parse,
// is never refused — the gate never stands in the way of a fix.

// parseGateLanguage maps a file path to the language name understood by
// astquery.DefaultLanguageResolver. Returns "" for files whose syntax the gate
// cannot check; those writes skip the gate entirely (a safe no-op).
func parseGateLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".rb":
		return "ruby"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".scala", ".sc":
		return "scala"
	case ".rs":
		return "rust"
	case ".ex", ".exs":
		return "elixir"
	case ".php":
		return "php"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".mq4", ".mq5", ".mqh":
		// MQL shares the tree-sitter-mql5 grammar (a tree-sitter-cpp fork).
		return "mql"
	case ".sh", ".bash":
		return "bash"
	}
	return ""
}

// parseErrorCount parses content for the given language and returns the count
// of tree-sitter ERROR / MISSING nodes plus whether the language is one the
// gate can actually parse. (0, false) means "no opinion" — the gate skips.
func parseErrorCount(lang string, content []byte) (int, bool) {
	if lang == "" {
		return 0, false
	}
	sl := astquery.DefaultLanguageResolver(lang)
	if sl == nil {
		return 0, false
	}
	tree, err := parser.ParseFile(content, sl)
	if err != nil || tree == nil {
		// A failure here is a tree-sitter cancellation / timeout, not a
		// syntax verdict — stay silent rather than block on infrastructure.
		return 0, false
	}
	pt := parser.NewParseTree(tree, content, lang)
	defer pt.Release()
	return pt.CountParseErrors(), true
}

// parseGateResult is the verdict of the pre-write syntax gate.
type parseGateResult struct {
	Checked   bool   // the language was parseable and the gate ran
	Blocked   bool   // newContent introduces parse errors the old content did not
	OldErrors int    // parse errors in the pre-edit content
	NewErrors int    // parse errors in the candidate content
	Language  string // gate language (may be set even when Checked is false)
}

// parseGateEnabled reports whether the pre-write parse gate is active. On by
// default; set GORTEX_EDIT_PARSE_GATE=0 (false / off / no) to disable globally.
func parseGateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_EDIT_PARSE_GATE"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// checkParseGate decides whether writing newContent over oldContent would
// introduce new syntax errors. oldContent may be nil (a brand-new file), in
// which case any parse error in newContent counts as a regression. The gate
// blocks only a regression — newErrors strictly greater than oldErrors.
func checkParseGate(path string, oldContent, newContent []byte) parseGateResult {
	lang := parseGateLanguage(path)
	newErrs, newOK := parseErrorCount(lang, newContent)
	if !newOK {
		return parseGateResult{Language: lang}
	}
	oldErrs := 0
	if len(oldContent) > 0 {
		if e, ok := parseErrorCount(lang, oldContent); ok {
			oldErrs = e
		}
	}
	return parseGateResult{
		Checked:   true,
		Blocked:   newErrs > oldErrs,
		OldErrors: oldErrs,
		NewErrors: newErrs,
		Language:  lang,
	}
}

// parseGateError renders the agent-facing refusal message for a blocked write.
func parseGateError(relPath string, r parseGateResult) string {
	return fmt.Sprintf(
		"parse gate: writing %s would introduce %d new %s parse error(s) (was %d, would be %d) — the edit appears to leave the file syntactically broken and was refused. Fix the fragment, or pass allow_parse_errors=true to write anyway.",
		relPath, r.NewErrors-r.OldErrors, r.Language, r.OldErrors, r.NewErrors)
}

// parseGateInfo renders the gate verdict for inclusion in a tool response.
// Returns nil when the gate did not run (unparseable language), so a clean
// edit on a supported language stays quiet unless something is notable.
func parseGateInfo(r parseGateResult, allowed bool) map[string]any {
	if !r.Checked {
		return nil
	}
	if r.OldErrors == r.NewErrors && r.NewErrors == 0 {
		return nil // nothing to report — a clean file stayed clean
	}
	m := map[string]any{
		"language":   r.Language,
		"old_errors": r.OldErrors,
		"new_errors": r.NewErrors,
		"blocked":    r.Blocked && !allowed,
	}
	if r.Blocked && allowed {
		m["overridden"] = true
	}
	return m
}
