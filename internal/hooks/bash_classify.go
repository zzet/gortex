package hooks

import (
	"regexp"
	"strconv"
	"strings"
)

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
	// BashActionFileList is a read-only, line-oriented file listing whose
	// stdout can be normalized to the existing Glob post-processing path.
	// Ambiguous or execution-capable variants remain passthrough.
	BashActionFileList
	// BashActionReadRange is a bounded sed/awk source read. It is never denied
	// or rewritten; PostToolUse may add graph context for the referenced file.
	BashActionReadRange
	// BashActionWriteSource means the command MUTATES a source-looking file —
	// a shell redirect, `tee`, an in-place editor, or an inline interpreter
	// script that opens the path for writing. Writes holds every recognised
	// target and Path the first of them. Appended last on purpose: the value
	// is compared, never persisted, but renumbering the read actions would
	// still be a needless diff.
	BashActionWriteSource
)

// BashClassification is the result of classifyBashCommand.
type BashClassification struct {
	Action  BashAction
	Pattern string      // for GrepLike / FindName
	Path    string      // for ReadSource / ReadRange / WriteSource
	Writes  []BashWrite // every recognised write target, for WriteSource
	Primary string      // the primary command token (grep, rg, cat, …) — for messages
	// Paths are the files and directories a GrepLike command searches, each
	// joined onto the directory a leading `cd <dir> &&` chain moved into and
	// with a leading `~` expanded; relative entries stay relative to the
	// hook's cwd. Nil when the command names no path (stdin, or recursion from
	// the working directory) or when any operand, or the directory the
	// command runs in, cannot be read off the command line.
	Paths []string
}

// classifyBashCommand inspects a Bash tool_input.command and returns the first
// actionable classification it finds across the command's *primary* segments
// (start-of-line or after ; && ||; a segment after a single `|` is a filter on
// upstream output and is ignored).
//
// A recognised write shape is answered before any read shape. A command that
// both reads and writes source (`cat old.go > new.go`) is a mutation, and the
// mutation is the fact the caller has to act on. Write recognition is the one
// part that looks past a pipe, because `… | tee x.go` writes through the
// segment a read classification would drop.
//
// The parser is intentionally small. It respects single/double quotes but
// does NOT attempt to handle escapes or subshells ($() / backticks). Anything
// it can't classify confidently falls through to BashActionPassthrough — the
// conservative answer since a false deny is more disruptive than a miss.
func classifyBashCommand(cmd string) BashClassification {
	if writes := classifyBashWriteTargets(cmd); len(writes) > 0 {
		return BashClassification{
			Action:  BashActionWriteSource,
			Path:    writes[0].Path,
			Writes:  writes,
			Primary: writes[0].Shape,
		}
	}
	return classifyBashReadCommand(cmd)
}

// classifyBashReadCommand is classifyBashCommand without the write pre-pass.
// Whether a write target matters is only knowable from the daemon, so a
// command that writes an untracked path and reads indexed source in the same
// breath has to be able to come back here for the read answer rather than
// losing it to a write classification that turned out not to apply.
func classifyBashReadCommand(cmd string) BashClassification {
	return classifyBashReadCommandSkipping(cmd, nil)
}

// classifyBashReadCommandSkipping is classifyBashReadCommand with a veto: a
// segment classification skip rejects is passed over and the search goes on
// with the next primary segment. enrichBash uses it to look past a grep whose
// every target lies outside the graph, so a source read chained after it is
// still answered.
func classifyBashReadCommandSkipping(cmd string, skip func(BashClassification) bool) BashClassification {
	segments := primarySegments(cmd)
	cdDir, cdSegments := leadingCdChain(cmd)
	for idx, seg := range segments {
		// A redirect names where output goes, not what the command reads:
		// without removing it, `cat internal/x.go > /tmp/out.go` reports
		// /tmp/out.go as the file being read and the indexed read is lost.
		command, _ := splitSegmentRedirects(seg)
		tokens := skipCommandPrefix(tokenize(command))
		if len(tokens) == 0 {
			continue
		}

		// Command names are matched case-insensitively: PowerShell, which the
		// Copilot and Pi adapters route through this classifier, accepts
		// `Cat` and `GREP` as readily as `cat` and `grep`.
		name := strings.ToLower(tokens[0])
		if c, ok := classifyPowerShellRead(name, tokens, "", false); ok {
			if c.Action == BashActionGrepLike {
				if dir, known := segmentWorkingDir(segments, idx, cdDir, cdSegments); known {
					c, _ = classifyPowerShellRead(name, tokens, dir, true)
				}
				if skip != nil && skip(c) {
					continue
				}
			}
			return c
		}
		tokens = append([]string{name}, tokens[1:]...)

		switch name {
		case "grep", "rg", "ag", "egrep", "fgrep":
			if p, ok := extractGrepPattern(tokens); ok {
				c := BashClassification{Action: BashActionGrepLike, Pattern: p, Primary: tokens[0]}
				if dir, known := segmentWorkingDir(segments, idx, cdDir, cdSegments); known {
					c.Paths = grepSearchPaths(tokens, dir)
				}
				if skip == nil || !skip(c) {
					return c
				}
			}
		case "find":
			if p, ok := extractFindName(tokens); ok {
				return BashClassification{Action: BashActionFindName, Pattern: p, Primary: tokens[0]}
			}
		case "cat", "head", "tail":
			if path, ok := extractReadFile(tokens); ok {
				return BashClassification{Action: BashActionReadSource, Path: path, Primary: tokens[0]}
			}
		case "fd", "fdfind", "ls", "tree":
			if safeFileList(tokens) {
				return BashClassification{Action: BashActionFileList, Primary: tokens[0]}
			}
		case "git":
			if safeGitFileList(tokens) {
				return BashClassification{Action: BashActionFileList, Primary: "git ls-files"}
			}
		case "sed":
			if path, ok := extractSedReadFile(tokens); ok {
				return BashClassification{Action: BashActionReadRange, Path: path, Primary: "sed"}
			}
		case "awk":
			if path, ok := extractAwkReadFile(tokens); ok {
				return BashClassification{Action: BashActionReadRange, Path: path, Primary: "awk"}
			}
		}
	}
	return BashClassification{Action: BashActionPassthrough}
}

// skipCommandPrefix drops the leading `sudo` / `time` wrappers and env
// assignments (FOO=bar cmd) so the first remaining token is the command.
func skipCommandPrefix(tokens []string) []string {
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
	return tokens
}

// primarySegments splits cmd on newlines and ; && || and returns the segments
// that are primary commands (everything up to the first `|` inside each
// statement). Segments after a single `|` are not primary and are dropped, and
// heredoc payloads are not commands at all — see splitCommandSegments.
func primarySegments(cmd string) []string { return splitCommandSegments(cmd, false) }

// allCommandSegments splits cmd the same way but keeps the segments a pipe
// would filter out. Write recognition needs them: the mutation in
// `go run ./gen | tee internal/x.go` lives entirely after the pipe.
func allCommandSegments(cmd string) []string { return splitCommandSegments(cmd, true) }

// splitCommandSegments returns the commands in cmd. A newline separates
// statements exactly as `;` does — without that, one line break hides every
// following command from classification. Heredoc payloads are removed first:
// what an agent writes into a file is data, and a shell snippet quoted inside
// it must never be read as a command of its own.
func splitCommandSegments(cmd string, keepPiped bool) []string {
	cmd, _ = splitHeredocs(cmd)
	var out []string
	cur := make([]byte, 0, len(cmd))
	inSingle, inDouble := false, false
	primary := true
	i, n := 0, len(cmd)
	flush := func() {
		if primary || keepPiped {
			if s := strings.TrimSpace(string(cur)); s != "" {
				out = append(out, s)
			}
		}
		cur = cur[:0]
	}
	for i < n {
		c := cmd[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
			cur = append(cur, c)
			i++
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			cur = append(cur, c)
			i++
			continue
		}
		if inSingle || inDouble {
			cur = append(cur, c)
			i++
			continue
		}
		// A trailing backslash continues the statement onto the next line, so
		// the newline is whitespace rather than a boundary. Splitting there
		// would cut an invocation in half and hide its operands.
		if c == '\n' && len(cur) > 0 && cur[len(cur)-1] == '\\' {
			cur[len(cur)-1] = ' '
			i++
			continue
		}
		// Statement boundaries reset "primary" to true.
		if c == ';' || c == '\n' {
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
		// `>|` is one redirect operator, not a redirect followed by a pipe.
		if c == '|' && i > 0 && cmd[i-1] == '>' {
			cur = append(cur, c)
			i++
			continue
		}
		// Single `|` ends primary; stay non-primary until next statement.
		if c == '|' {
			flush()
			primary = false
			i++
			continue
		}
		cur = append(cur, c)
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
	pattern, _, ok := extractGrepPatternAt(tokens)
	return pattern, ok
}

func extractGrepPatternAt(tokens []string) (string, int, bool) {
	// tokens[0] is the command itself.
	i := 1
	for i < len(tokens) {
		t := tokens[i]
		// -e PATTERN / --regexp PATTERN / --file PATH: the next token is the
		// pattern itself (except --file, but we treat it the same for our
		// purposes — it'll be gated by classifyGrepPattern anyway).
		if t == "-e" || t == "--regexp" {
			if i+1 < len(tokens) {
				return tokens[i+1], i + 1, true
			}
			return "", -1, false
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
		return t, i, true
	}
	return "", -1, false
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

func safeFileList(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	switch tokens[0] {
	case "fd", "fdfind":
		for _, token := range tokens[1:] {
			if token == "-x" || token == "-X" || token == "-0" || token == "-l" ||
				strings.HasPrefix(token, "--exec") || strings.HasPrefix(token, "--print0") ||
				token == "--list-details" || strings.HasPrefix(token, "--format") {
				return false
			}
		}
		return true
	case "ls":
		for _, token := range tokens[1:] {
			if !safeLSOption(token) {
				return false
			}
		}
		return true
	case "tree":
		hasFullPath, hasNoIndent := false, false
		for _, token := range tokens[1:] {
			if !strings.HasPrefix(token, "-") {
				continue
			}
			if token == "--noreport" {
				continue
			}
			if strings.HasPrefix(token, "--") {
				return false
			}
			for _, flag := range strings.TrimPrefix(token, "-") {
				switch flag {
				case 'f':
					hasFullPath = true
				case 'i':
					hasNoIndent = true
				case 'a', 'd', 'n':
					// These filter entries or disable color without decorating paths.
				default:
					return false
				}
			}
		}
		return hasFullPath && hasNoIndent
	default:
		return false
	}
}

func safeGitFileList(tokens []string) bool {
	if len(tokens) < 2 || tokens[0] != "git" || tokens[1] != "ls-files" {
		return false
	}
	for _, token := range tokens[2:] {
		if token == "--stage" || token == "--debug" || token == "--eol" ||
			token == "--unmerged" || token == "--resolve-undo" ||
			strings.HasPrefix(token, "--format") || strings.HasPrefix(token, "--abbrev") {
			return false
		}
		if strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") {
			for _, flag := range strings.TrimPrefix(token, "-") {
				if strings.ContainsRune("zstvfuh", flag) {
					return false
				}
			}
		}
	}
	return true
}

// safeLSOption permits only flags that keep stdout one-path-per-line. The
// default non-TTY shape and -1 are line-oriented; metadata, quoting,
// classification suffixes, columns, commas, and recursion are deliberately
// ignored because PostToolUse would otherwise parse decorations as paths.
func safeLSOption(token string) bool {
	if !strings.HasPrefix(token, "-") || token == "-" {
		return true
	}
	if token == "--" {
		return true
	}
	if strings.HasPrefix(token, "--") {
		switch token {
		case "--all", "--almost-all", "--directory", "--hide-control-chars", "--literal":
			return true
		default:
			return false
		}
	}
	for _, flag := range strings.TrimPrefix(token, "-") {
		if !strings.ContainsRune("aA1d", flag) {
			return false
		}
	}
	return true
}

const maxHookReadRangeLines = 2000

var (
	sedSinglePrintRE = regexp.MustCompile(`^\s*(\d+|\$)\s*p\s*$`)
	sedRangePrintRE  = regexp.MustCompile(`^\s*(\d+)\s*,\s*(\d+)\s*p\s*$`)
	awkRangePrintRE  = regexp.MustCompile(`(?i)^\s*NR\s*>=?\s*(\d+)\s*&&\s*NR\s*<=\s*(\d+)\s*\{\s*print(?:\s+\$0)?\s*\}\s*$`)
)

func extractSedReadFile(tokens []string) (string, bool) {
	if len(tokens) < 3 {
		return "", false
	}
	quiet := false
	for _, token := range tokens[1:] {
		if token == "-i" || strings.HasPrefix(token, "-i") || token == "--in-place" || strings.HasPrefix(token, "--in-place=") {
			return "", false
		}
		if token == "-n" || token == "--quiet" || token == "--silent" {
			quiet = true
		}
	}
	if !quiet {
		return "", false
	}
	path, ok := lastSourceToken(tokens)
	if !ok {
		return "", false
	}
	program := ""
	for i := 1; i < len(tokens); i++ {
		if tokens[i] == "-n" || tokens[i] == "--quiet" || tokens[i] == "--silent" || tokens[i] == path {
			continue
		}
		if strings.HasPrefix(tokens[i], "-") {
			return "", false
		}
		program = tokens[i]
		break
	}
	if !safeSedPrintProgram(program) {
		return "", false
	}
	return path, true
}

func safeSedPrintProgram(program string) bool {
	if sedSinglePrintRE.MatchString(program) {
		return true
	}
	match := sedRangePrintRE.FindStringSubmatch(program)
	return boundedLineRange(match)
}

func extractAwkReadFile(tokens []string) (string, bool) {
	if len(tokens) < 3 {
		return "", false
	}
	path, ok := lastSourceToken(tokens)
	if !ok {
		return "", false
	}
	if !boundedLineRange(awkRangePrintRE.FindStringSubmatch(tokens[1])) {
		return "", false
	}
	return path, true
}

func boundedLineRange(match []string) bool {
	if len(match) != 3 {
		return false
	}
	start, startErr := strconv.Atoi(match[1])
	end, endErr := strconv.Atoi(match[2])
	return startErr == nil && endErr == nil && start > 0 && end >= start && end-start+1 <= maxHookReadRangeLines
}

func lastSourceToken(tokens []string) (string, bool) {
	for i := len(tokens) - 1; i >= 1; i-- {
		if strings.HasPrefix(tokens[i], "-") {
			continue
		}
		if looksLikeSourceFile(tokens[i]) {
			return tokens[i], true
		}
		return "", false
	}
	return "", false
}

// simpleBashCommand accepts exactly one unpiped, unredirected command. It is
// used only by rewrite mode; ambiguity always falls back to advisory context.
func simpleBashCommand(command string) bool {
	if strings.Contains(command, "$(") || strings.Contains(command, "`") || strings.Contains(command, "\n") {
		return false
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ';', '|', '&', '>', '<':
			if !inSingle && !inDouble {
				return false
			}
		}
	}
	return !inSingle && !inDouble && len(primarySegments(command)) == 1
}
