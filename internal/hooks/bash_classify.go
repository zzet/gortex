package hooks

import "strings"

// BashAction describes what an enrichBash caller should do with a Bash command.
type BashAction int

const (
	// BashActionPassthrough means the command isn't a codebase search — allow.
	BashActionPassthrough BashAction = iota
	// BashActionGrepLike means the command is a primary grep/rg/ag invocation.
	// Pattern holds the extracted search pattern for the daemon probe.
	BashActionGrepLike
	// BashActionFindName means the command is `find … -name "<symbol>…"`.
	// Pattern holds the name with leading/trailing `*`/`?` stripped.
	BashActionFindName
	// BashActionReadSource means the command reads an indexed-looking source
	// file (cat/head/tail of .go/.ts/…). Path holds the file path.
	BashActionReadSource
	// BashActionFileList is a primary command whose stdout is a file list. It
	// is used only for PostToolUse enrichment; PreToolUse remains silent.
	BashActionFileList
)

// BashClassification is the result of classifyBashCommand.
type BashClassification struct {
	Action  BashAction
	Pattern string // for GrepLike / FindName
	Path    string // for ReadSource
	Primary string // the primary command token (grep, rg, cat, …) — for messages
}

// classifyBashCommand inspects a Bash tool_input.command and returns the first
// actionable classification it finds across the command's *primary* segments
// (start-of-line or after ; && ||; a segment after a single `|` is a filter on
// upstream output and is ignored).
//
// The parser is intentionally small. It respects single/double quotes but
// does NOT attempt to handle escapes, subshells ($() / backticks), heredocs,
// or redirects. Anything it can't classify confidently falls through to
// BashActionPassthrough — the conservative answer since a false deny is more
// disruptive than a miss.
func classifyBashCommand(cmd string) BashClassification {
	for _, seg := range primarySegments(cmd) {
		tokens := tokenize(seg)
		if len(tokens) == 0 {
			continue
		}
		// Skip over `sudo` / `time` / env assignments (FOO=bar cmd).
		for len(tokens) > 0 && (tokens[0] == "sudo" || tokens[0] == "time" || strings.Contains(tokens[0], "=")) {
			if strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "-") {
				tokens = tokens[1:]
				continue
			}
			if tokens[0] == "sudo" || tokens[0] == "time" {
				tokens = tokens[1:]
				continue
			}
			break
		}
		if len(tokens) == 0 {
			continue
		}

		switch tokens[0] {
		case "grep", "rg", "ag", "egrep", "fgrep":
			if p, ok := extractGrepPattern(tokens); ok {
				return BashClassification{Action: BashActionGrepLike, Pattern: p, Primary: tokens[0]}
			}
		case "find":
			if p, ok := extractFindName(tokens); ok {
				return BashClassification{Action: BashActionFindName, Pattern: p, Primary: tokens[0]}
			}
		case "cat", "head", "tail":
			if path, ok := extractReadFile(tokens); ok {
				return BashClassification{Action: BashActionReadSource, Path: path, Primary: tokens[0]}
			}
		case "sed", "awk":
			if path, ok := extractReadFile(tokens); ok {
				return BashClassification{Action: BashActionReadSource, Path: path, Primary: tokens[0]}
			}
		case "fd", "fdfind", "ls", "tree":
			return BashClassification{Action: BashActionFileList, Primary: tokens[0]}
		case "git":
			if len(tokens) >= 2 && tokens[1] == "ls-files" {
				return BashClassification{Action: BashActionFileList, Primary: "git ls-files"}
			}
		}
	}
	return BashClassification{Action: BashActionPassthrough}
}

// primarySegments splits cmd on ; && || and returns the segments that are
// primary commands (everything up to the first `|` inside each statement).
// Segments that appear after a single `|` are not primary and are dropped.
func primarySegments(cmd string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	primary := true
	i, n := 0, len(cmd)
	flush := func() {
		if primary {
			s := strings.TrimSpace(cur.String())
			if s != "" {
				out = append(out, s)
			}
		}
		cur.Reset()
	}
	for i < n {
		c := cmd[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(c)
			i++
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(c)
			i++
			continue
		}
		if inSingle || inDouble {
			cur.WriteByte(c)
			i++
			continue
		}
		// Statement boundaries reset "primary" to true.
		if c == ';' {
			flush()
			primary = true
			i++
			continue
		}
		if c == '&' && i+1 < n && cmd[i+1] == '&' {
			flush()
			primary = true
			i += 2
			continue
		}
		if c == '|' && i+1 < n && cmd[i+1] == '|' {
			flush()
			primary = true
			i += 2
			continue
		}
		// Single `|` ends primary; stay non-primary until next statement.
		if c == '|' {
			flush()
			primary = false
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return out
}

// tokenize splits a primary-segment string on whitespace, treating
// single/double-quoted runs as one token (with the quotes stripped).
func tokenize(seg string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	hasContent := false
	flush := func() {
		if hasContent {
			out = append(out, cur.String())
		}
		cur.Reset()
		hasContent = false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			hasContent = true // empty quoted string is still a token
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			hasContent = true
			continue
		}
		if !inSingle && !inDouble && (c == ' ' || c == '\t') {
			flush()
			continue
		}
		cur.WriteByte(c)
		hasContent = true
	}
	flush()
	return out
}

// grepFlagsTakingArg is the short-form flag set for grep/rg where the next
// token is consumed as an argument, not a positional. Kept small — covers
// the flags I actually see in practice.
var grepFlagsTakingArg = map[string]bool{
	"-e": true, "-f": true, "-A": true, "-B": true, "-C": true,
	"-m": true, "--max-count": true,
	"--include": true, "--exclude": true,
	"--regexp": true, "--file": true,
	"-t": true, "-T": true, // rg type / type-not
	"-g": true, // rg glob
}

// extractGrepPattern walks tokens after `grep`/`rg` and returns the search
// pattern — either the argument of `-e`/`--regexp` (which IS the pattern),
// or the first non-flag positional. Returns ok=false if no pattern is
// present (e.g. `grep -h` help invocation).
func extractGrepPattern(tokens []string) (string, bool) {
	// tokens[0] is the command itself.
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		// -e PATTERN / --regexp PATTERN / --file PATH: the next token is the
		// pattern itself (except --file, but we treat it the same for our
		// purposes — it'll be gated by classifyGrepPattern anyway).
		if t == "-e" || t == "--regexp" {
			if i+1 < len(tokens) {
				return tokens[i+1], true
			}
			return "", false
		}
		// --flag=value: one token, skip.
		if strings.HasPrefix(t, "--") && strings.Contains(t, "=") {
			i++
			continue
		}
		if grepFlagsTakingArg[t] {
			i += 2
			continue
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			i++
			continue
		}
		return t, true
	}
	return "", false
}

// extractFindName walks tokens looking for `-name`/`-iname` and returns the
// next token with leading/trailing `*`?“ stripped. Non-symbol-looking
// residues still return ok=true; the caller (enrichBash) gates on
// classifyGrepPattern.
func extractFindName(tokens []string) (string, bool) {
	for i := 1; i < len(tokens)-1; i++ {
		if tokens[i] == "-name" || tokens[i] == "-iname" {
			val := tokens[i+1]
			val = strings.Trim(val, "*?")
			return val, true
		}
	}
	return "", false
}

// extractReadFile returns the last token that looks like a source file path,
// skipping flags. For cat/head/tail the file is usually the last positional,
// so walking in reverse is the simplest match.
func extractReadFile(tokens []string) (string, bool) {
	for i := len(tokens) - 1; i >= 1; i-- {
		t := tokens[i]
		if strings.HasPrefix(t, "-") {
			continue
		}
		if looksLikeSourceFile(t) {
			return t, true
		}
		// First non-flag, non-source token — not a source read.
		return "", false
	}
	return "", false
}
