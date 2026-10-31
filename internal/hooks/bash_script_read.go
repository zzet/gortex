package hooks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/toolref"
)

// bash_script_read.go catches the read door the cat/grep classifier cannot
// see: inline Python (`python3 -c`, `python3 - <<EOF`) that opens indexed
// source. Agents reach for it once Read and cat are redirected, so it gets
// the verdict those would: a source file the daemon holds as indexed is a
// Read, and a script that reads a file and writes it back is an edit,
// answered like every other shell write. The script is parsed with the
// tree-sitter Python grammar (python_script_facts.go); anything the hook
// cannot parse or resolve stays silent — a false deny costs more than the
// miss.

// maxInlineScript bounds the script the hook parses, and maxInlineScripts the
// scripts it analyses in one command; past them the hook has no opinion.
// Real commands carry one to three, and the probe budget can answer for only
// a few.
const (
	maxInlineScript  = 256 << 10
	maxInlineScripts = 64
	// inlineScriptTime bounds the analysis of every script in one command.
	inlineScriptTime = 500 * time.Millisecond
)

// pythonScript is one inline Python program and where it starts in the
// command, so a `cd` before it can be told from one after it.
type pythonScript struct {
	code       string
	pos        int
	cwdUnknown bool // a launcher ran it elsewhere (env -C, uv run --directory)
}

// pythonBinaryRE matches python, python3, python3.12, pypy3 and the like.
var pythonBinaryRE = regexp.MustCompile(`^(?:python|pypy)[0-9.]*$`)

// interpreterLaunchers run the command that follows them: `uv run python`,
// `env FOO=1 python3`, `timeout 5 python3` — the launchers agents use in
// front of inline Python. positional is how many launcher
// arguments precede the command; valued lists the options that take a value;
// chdir lists the options that run the command in another directory.
var interpreterLaunchers = map[string]struct {
	positional int
	valued     []string
	chdir      []string
}{
	"env":     {valued: []string{"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}, chdir: []string{"-C", "--chdir"}},
	"timeout": {positional: 1, valued: []string{"-s", "--signal", "-k", "--kill-after"}},
	"uv": {positional: 1, valued: []string{"--with", "--with-requirements", "--python", "-p", "--project",
		"--directory", "--extra", "--group", "--env-file", "--package", "--index-url", "--cache-dir"},
		chdir: []string{"--directory"}},
}

// pythonCommand returns the words from the Python binary on, looking past
// sudo/time/VAR=value prefixes and launchers, or nil when words run no
// Python. cwdChanged reports a launcher option that runs it in another
// directory.
func pythonCommand(words []string) (python []string, cwdChanged bool) {
	words = skipCommandPrefix(words)
	for len(words) > 0 {
		name := baseCommandName(words[0])
		if pythonBinaryRE.MatchString(name) {
			return words, cwdChanged
		}
		launcher, ok := interpreterLaunchers[name]
		if !ok {
			return nil, false
		}
		words = words[1:]
		skipOptions := func() {
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || strings.Contains(words[0], "=")) {
				opt := words[0]
				words = words[1:]
				flag, _, attached := strings.Cut(opt, "=")
				for _, v := range launcher.chdir {
					// --chdir, --chdir=DIR, -C DIR, and a short flag carrying
					// its value or clustered with others (-C/tmp, -iC/tmp).
					short := len(v) == 2 && !strings.HasPrefix(opt, "--") && strings.ContainsRune(opt[1:], rune(v[1]))
					if flag == v || short {
						cwdChanged = true
					}
				}
				for _, v := range launcher.valued {
					if !attached && opt == v && len(words) > 0 {
						words = words[1:]
						break
					}
				}
			}
		}
		skipOptions()
		if launcher.positional > 0 && len(words) >= launcher.positional {
			words = words[launcher.positional:]
		}
		skipOptions()
	}
	return nil, false
}

// pythonInline reads a Python command line: the code of `-c CODE`, or
// fromStdin when the program comes from standard input (`python3 -` or no
// script at all). `-m module` and a script file are not inline code.
func pythonInline(words []string) (code string, fromStdin, ok bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "-c":
			if i+1 < len(words) {
				return words[i+1], false, true
			}
			return "", false, false
		case w == "-m":
			return "", false, false
		case w == "-":
			return "", true, true
		case w == "-X" || w == "-W" || w == "--check-hash-based-pycs":
			i++
		case strings.HasPrefix(w, "-X") || strings.HasPrefix(w, "-W"):
			// -Werror::DeprecationWarning, -Xdev: the value is attached.
		case strings.HasPrefix(w, "--"):
		case strings.HasPrefix(w, "-"):
			// A flag cluster ending in c (-uc CODE) or carrying the code
			// (-cCODE).
			if j := strings.IndexByte(w, 'c'); j > 0 {
				if j < len(w)-1 {
					return w[j+1:], false, true
				}
				if i+1 < len(words) {
					return words[i+1], false, true
				}
				return "", false, false
			}
		default:
			return "", false, false // a script file
		}
	}
	return "", true, true
}

// shellWords splits a command the way a POSIX shell does for quoting: single
// quotes are literal, double quotes keep their content with \" \\ \$ \`
// unescaped, and a backslash outside quotes escapes the next byte.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	has := false
	flush := func() {
		if has {
			out = append(out, cur.String())
		}
		cur.Reset()
		has = false
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
			} else {
				cur.WriteString(s[i+1 : i+1+j])
				i += j + 2
			}
			has = true
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					if s[i+1] != '\n' {
						cur.WriteByte(s[i+1])
					}
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			i++
			has = true
		case c == '\\' && i+1 < len(s):
			if s[i+1] != '\n' {
				cur.WriteByte(s[i+1])
			}
			i += 2
			has = true
		case c == ' ' || c == '\t' || c == '\n':
			flush()
			i++
		default:
			cur.WriteByte(c)
			has = true
			i++
		}
	}
	flush()
	return out
}

// pythonScriptsAt returns every inline Python program in cmd with its
// position: each `-c` statement, then each heredoc body fed to a Python
// reading standard input. Positions come from cursors that only move
// forward, so a long command costs one pass and a repeated statement gets
// its own position.
func pythonScriptsAt(cmd string) []pythonScript {
	if !strings.Contains(cmd, "python") && !strings.Contains(cmd, "pypy") {
		return nil
	}
	var scripts []pythonScript
	// A text not found verbatim (a segment the splitter rewrote, such as
	// a backslash-newline) loses the cursor: what follows has no position,
	// which only gives up on relative paths, and the scan stays one pass.
	at := func(cursor *int, text string) int {
		if *cursor < 0 {
			return -1
		}
		i := strings.Index(cmd[*cursor:], text)
		if i < 0 {
			*cursor = -1
			return -1
		}
		pos := *cursor + i
		*cursor = pos + len(text)
		return pos
	}
	segCursor := 0
	for _, segment := range allCommandSegments(cmd) {
		pos := at(&segCursor, segment)
		command, _ := splitSegmentRedirects(segment)
		if words, moved := pythonCommand(shellWords(command)); words != nil {
			if code, fromStdin, ok := pythonInline(words); ok && !fromStdin && code != "" {
				scripts = append(scripts, pythonScript{code: code, pos: pos, cwdUnknown: moved})
			}
		}
	}
	_, heredocs := splitHeredocs(cmd)
	docCursor := 0
	for _, heredoc := range heredocs {
		// The body follows its opener line, so search from there: a copy of
		// the same text earlier in the command (an echo, a comment) is not it.
		pos := -1
		if at(&docCursor, heredoc.opener) >= 0 {
			pos = at(&docCursor, heredoc.body)
		}
		// A line that opens two heredocs, or a here-string and a heredoc:
		// which body is whose is not worth telling apart.
		if strings.Count(heredoc.opener, "<<") > 1 {
			continue
		}
		before := heredoc.opener
		if i := strings.Index(before, "<<"); i >= 0 {
			before = before[:i]
		}
		segments := allCommandSegments(before)
		if len(segments) == 0 {
			continue
		}
		command, _ := splitSegmentRedirects(segments[len(segments)-1])
		if words, moved := pythonCommand(shellWords(command)); words != nil {
			if _, fromStdin, ok := pythonInline(words); ok && fromStdin {
				scripts = append(scripts, pythonScript{code: heredoc.body, pos: pos, cwdUnknown: moved})
			}
		}
	}
	return scripts
}

// segmentChangesDir reports whether a segment runs cd or pushd anywhere in
// it: `(cd x`, `{ cd x`, `builtin cd x`, `sudo -u me cd x`. Reading too many
// segments as a cd only gives up on relative paths, which fails open.
func segmentChangesDir(segment string) bool {
	for _, tok := range tokenize(segment) {
		if t := strings.TrimLeft(tok, "({"); t == "cd" || t == "pushd" {
			return true
		}
	}
	return false
}

// cdPositions returns where each `cd` / `pushd` the leading chain does not
// account for starts in cmd.
func cdPositions(cmd string, leading int) []int {
	var positions []int
	offset, seen := 0, 0
	for _, segment := range allCommandSegments(cmd) {
		at := -1
		if offset >= 0 {
			// Lost once, lost for good: see pythonScriptsAt.
			if at = strings.Index(cmd[offset:], segment); at >= 0 {
				at += offset
				offset = at + len(segment)
			} else {
				offset = -1
			}
		}
		if !segmentChangesDir(segment) {
			continue
		}
		seen++
		if seen > leading {
			positions = append(positions, at)
		}
	}
	return positions
}

// enrichBashScriptRead answers inline Python that reads indexed source.
// answered is false when the command runs no such script, so the caller
// falls through to its own verdict.
func enrichBashScriptRead(cmd, cwd string, scopeProbe fileScopeProbe) (enrichResult, bool) {
	started := time.Now()
	deadline := started.Add(inlineScriptTime)
	scripts := pythonScriptsAt(cmd)
	if len(scripts) == 0 {
		return enrichResult{}, false
	}
	if len(scripts) > maxInlineScripts {
		scripts = scripts[:maxInlineScripts]
	}
	cdDir, cdSegments := leadingCdChain(cmd)
	cds := cdPositions(cmd, cdSegments)
	resolve := func(script pythonScript, p string) (string, bool) {
		if filepath.IsAbs(p) {
			return p, true
		}
		// A cd the chain cannot follow, made before the script runs, or a
		// chdir inside it: the relative path is no longer the hook's.
		for _, at := range cds {
			if at < 0 || script.pos < 0 || at < script.pos {
				return "", false
			}
		}
		// A possible working-directory change inside the script (os.chdir,
		// fchdir, chdir imported or aliased from os): a relative path no
		// longer resolves against the hook's cwd.
		if script.cwdUnknown || strings.Contains(script.code, "chdir") {
			return "", false
		}
		if cdDir != "" {
			return filepath.Join(cdDir, p), true
		}
		return p, true
	}

	type candidate struct {
		script  pythonScript
		path    string
		guessed bool // the write target is unnamed: it may be another file
	}
	var edits, reads []candidate
	for _, script := range scripts {
		facts, ok := analyzePythonScript(script.code, deadline)
		if !ok {
			continue
		}
		for _, p := range facts.reads {
			if facts.writeBack[p] {
				edits = append(edits, candidate{script, p, facts.guessed[p]})
			} else {
				reads = append(reads, candidate{script, p, false})
			}
		}
	}

	// Edits are probed before reads: a script that rewrites indexed source
	// answers first, as a shell write does whatever else the command reads,
	// and the budget cannot be spent on reads earlier in the command.
	probes := 0
	probe := func(c candidate) (fileIndexStatus, bool) {
		if probes >= bashWriteProbeLimit {
			return fileIndexStatus{}, false
		}
		at, ok := resolve(c.script, c.path)
		if !ok {
			return fileIndexStatus{}, false
		}
		probes++
		st := scopeProbe(cwd, at)
		return st, st.Indexed || st.ReadOnly || st.Loading
	}
	for _, c := range edits {
		if st, answer := probe(c); answer {
			if st.ReadOnly {
				return readOnlyNativeAllowance(true), true
			}
			if st.Loading {
				continue
			}
			// Read, modify, write back: an edit by another name, so it gets
			// the shell-write answer, advisory unless edit blocking is on.
			verb := "rewrites"
			if c.guessed {
				verb = "may rewrite"
			}
			logHookDecision("Bash", "", DecisionRedirectedWrite, st.Count, time.Since(started))
			return writeRedirect(
				fmt.Sprintf(
					"Bash Python script %s %s (indexed source, %d symbols). A shell write goes around the graph, which only catches up afterwards, and around the parse gate `edit` runs before it touches disk. Use:",
					verb, c.path, st.Count),
				"`edit(operation:\"symbol\", target:{symbol:\"<id>\"})` for one symbol",
				"`edit(operation:\"file\", target:{file:\"<path>\"})` for a guarded replacement — this one parse-gates",
				"`edit(operation:\"write\", target:{file:\"<path>\"})` for a whole-file write — this one too",
			), true
		}
	}
	for _, c := range reads {
		if st, answer := probe(c); answer {
			// The same silences the Read door keeps.
			if st.ReadOnly {
				return readOnlyNativeAllowance(false), true
			}
			if st.Loading {
				return enrichResult{context: repoLoadingNativeAllowance(st.LoadingRepo)}, true
			}
			var reason strings.Builder
			fmt.Fprintf(&reason,
				"[Gortex] BLOCKED: Bash Python script reads indexed source %s (%d symbols). Use graph tools instead:\n",
				c.path, st.Count)
			reason.WriteString("  - one symbol: `read(target:{symbol:\"<id>\"})`\n")
			reason.WriteString("  - file overview: `read(operation:\"summary\", target:{file:\"<path>\"})`\n")
			reason.WriteString("  - text inside it: `search(operation:\"text\", query:\"<literal>\")`\n")
			reason.WriteString(gcxTip)
			reason.WriteString(toolref.MCPRequiredLine())
			return enrichResult{deny: true, reason: reason.String()}, true
		}
	}

	return enrichResult{}, false
}
