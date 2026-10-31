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

// inlineScript is one inline program — a Python script, or the command
// string of `bash -c` — and where it starts in the command, so a `cd` before
// it can be told from one after it.
type inlineScript struct {
	code       string
	pos        int
	cwdUnknown bool // a launcher ran it elsewhere (env -C, uv run --directory)
	wrapped    bool // found only by looking through skipLaunchWrappers' wrappers
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
	return launchedCommand(words, pythonBinaryRE.MatchString, false)
}

// launchedCommand returns the words from the first binary `is` accepts on,
// looking past the same prefixes and launchers as pythonCommand, and with
// wrappers also past skipLaunchWrappers' wrappers before every command.
func launchedCommand(words []string, is func(name string) bool, wrappers bool) (command []string, cwdChanged bool) {
	for words = skipCommandPrefix(words); len(words) > 0; {
		if wrappers {
			if words = skipLaunchWrappers(words); len(words) == 0 {
				break
			}
		}
		name := baseCommandName(words[0])
		if is(name) {
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

// findPython is pythonCommand, then, with extra and when that finds no
// Python, the same search through skipLaunchWrappers' wrappers; wrapped
// reports the second.
func findPython(words []string, extra bool) (python []string, cwdChanged, wrapped bool) {
	if python, cwdChanged = pythonCommand(words); python != nil || !extra {
		return python, cwdChanged, false
	}
	python, cwdChanged = launchedCommand(words, pythonBinaryRE.MatchString, true)
	return python, cwdChanged, python != nil
}

// skipLaunchWrappers is skipCommandPrefix that also looks through the
// wrappers that run the command after them in the same filesystem and
// directory — `exec`, `nohup`, `setsid`, `nice`, `command`, `nvm exec
// <version>`, `direnv exec <dir>` — when finding an inline interpreter or a
// nested shell. The shell read and write shapes do not use it: a wrapped
// first statement would stop their classification short of a later one.
// Wrappers that run the command against another root or host (chroot,
// docker, ssh) are not looked through, and neither is `command -v`, which
// runs nothing.
func skipLaunchWrappers(tokens []string) []string {
	for tokens = skipCommandPrefix(tokens); len(tokens) > 0; tokens = skipCommandPrefix(tokens) {
		var opts []string
		switch t := tokens[0]; {
		case t == "nohup":
			tokens = tokens[1:]
		case t == "exec":
			tokens, _ = skipWrapperOptions(tokens[1:], "a")
		case t == "setsid":
			tokens, _ = skipWrapperOptions(tokens[1:], "")
		case t == "nice":
			tokens, _ = skipWrapperOptions(tokens[1:], "n")
		case t == "command":
			rest := tokens
			if tokens, opts = skipWrapperOptions(tokens[1:], ""); strings.ContainsAny(strings.Join(opts, ""), "vV") {
				return rest
			}
		case t == "nvm" && len(tokens) > 1 && tokens[1] == "exec":
			if tokens, _ = skipWrapperOptions(tokens[2:], ""); len(tokens) > 0 && nvmVersionRE.MatchString(tokens[0]) {
				tokens = tokens[1:]
			}
		case t == "direnv" && len(tokens) > 2 && tokens[1] == "exec":
			tokens = tokens[3:]
		default:
			return tokens
		}
	}
	return tokens
}

// nvmVersionRE matches the version `nvm exec` takes before the command: 20,
// v20.1.0, node, lts/iron. The version is optional, so a word that is not
// one is the command.
var nvmVersionRE = regexp.MustCompile(`^(?:v?[0-9][0-9.]*|node|iojs|stable|lts/\S+)$`)

// skipWrapperOptions drops the options a wrapper takes before its command
// and returns them. A short option in valued takes the next token as its
// value unless the value is attached; `--` ends the options.
func skipWrapperOptions(tokens []string, valued string) (rest, opts []string) {
	for len(tokens) > 0 && strings.HasPrefix(tokens[0], "-") && tokens[0] != "-" {
		opt := tokens[0]
		tokens = tokens[1:]
		if opt == "--" {
			break
		}
		opts = append(opts, opt)
		if last := opt[len(opt)-1:]; !strings.HasPrefix(opt, "--") && strings.Contains(valued, last) && len(tokens) > 0 {
			opts = append(opts, tokens[0])
			tokens = tokens[1:]
		}
	}
	return tokens, opts
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
// position.
func pythonScriptsAt(cmd string) []inlineScript {
	scripts, _ := inlineScriptsAt(cmd, false)
	return scripts
}

// inlineScriptsAt returns every inline Python program in cmd with its
// position — each `-c` statement, then each heredoc body fed to a Python
// reading standard input — and the command string of each `bash -c` (or
// sh, dash, zsh) statement. Positions come from cursors that only move
// forward, so a long command costs one pass and a repeated statement gets
// its own position. Without extra it finds only what pythonCommand finds,
// and no shells: no Python found only through a wrapper.
func inlineScriptsAt(cmd string, extra bool) (scripts, shells []inlineScript) {
	hasPython := strings.Contains(cmd, "python") || strings.Contains(cmd, "pypy")
	if !hasPython && (!extra || !strings.Contains(cmd, "sh")) {
		return nil, nil
	}
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
		words := shellWords(command)
		if python, moved, wrapped := findPython(words, extra); python != nil {
			if code, fromStdin, ok := pythonInline(python); ok && !fromStdin && code != "" {
				scripts = append(scripts, inlineScript{code: code, pos: pos, cwdUnknown: moved, wrapped: wrapped})
			}
		} else if shell, moved := launchedCommand(words, isNestingShell, true); extra && shell != nil {
			if code, ok := shellInline(shell); ok && code != "" {
				shells = append(shells, inlineScript{code: code, pos: pos, cwdUnknown: moved})
			}
		}
	}
	if !hasPython {
		return nil, shells
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
		if words, moved, wrapped := findPython(shellWords(command), extra); words != nil {
			if _, fromStdin, ok := pythonInline(words); ok && fromStdin {
				scripts = append(scripts, inlineScript{code: heredoc.body, pos: pos, cwdUnknown: moved, wrapped: wrapped})
			}
		}
	}
	return scripts, shells
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

// analyzedScript is an inline Python script and what its analysis found.
type analyzedScript struct {
	inlineScript
	facts pyScriptFacts
}

// analyzeInlineScripts analyses up to maxInlineScripts scripts by deadline,
// dropping each one the analysis has no opinion on; extra is the
// analysis mode (see analyzePythonScript). The scripts found only through a
// wrapper come last and are capped on their own.
func analyzeInlineScripts(scripts []inlineScript, deadline time.Time, extra bool) []analyzedScript {
	var out []analyzedScript
	for _, wrapped := range []bool{false, true} {
		n := 0
		for _, script := range scripts {
			if script.wrapped != wrapped || n >= maxInlineScripts {
				continue
			}
			n++
			if facts, ok := analyzePythonScript(script.code, deadline, extra); ok {
				out = append(out, analyzedScript{script, facts})
			}
		}
	}
	return out
}

// commandDirs is where the statements of one command run: the directory its
// leading `cd` chain ends in, and where each cd the chain cannot follow
// starts.
type commandDirs struct {
	dir string
	cds []int
}

func newCommandDirs(cmd string) commandDirs {
	dir, segments := leadingCdChain(cmd)
	return commandDirs{dir: dir, cds: cdPositions(cmd, segments)}
}

// at returns the directory a script runs in — relative to the hook's cwd
// unless absolute, "" for the cwd itself — or false when a cd the chain
// cannot follow comes before it, or a launcher ran it elsewhere.
func (d commandDirs) at(script inlineScript) (string, bool) {
	for _, at := range d.cds {
		if at < 0 || script.pos < 0 || at < script.pos {
			return "", false
		}
	}
	if script.cwdUnknown {
		return "", false
	}
	return d.dir, true
}

// pythonDir is at for a Python script, which may also change its own
// working directory (os.chdir, fchdir, chdir imported or aliased from os):
// then nothing relative in it resolves against the hook's cwd.
func (d commandDirs) pythonDir(script inlineScript) (string, bool) {
	if strings.Contains(script.code, "chdir") {
		return "", false
	}
	return d.at(script)
}

// resolve returns the path a script's literal path p names, relative to
// the hook's cwd unless absolute, or false when it cannot be told.
func (d commandDirs) resolve(script inlineScript, p string) (string, bool) {
	if filepath.IsAbs(p) {
		return p, true
	}
	dir, ok := d.pythonDir(script)
	if !ok {
		return "", false
	}
	if dir != "" {
		return filepath.Join(dir, p), true
	}
	return p, true
}

// scriptWrites returns the files the scripts open for writing through
// os.open, which no shell write pattern sees, as `script write` shapes: such
// a write gets the answer open(p, 'w') gets.
func scriptWrites(scripts []analyzedScript, dirs commandDirs) []BashWrite {
	var writes []BashWrite
	for _, script := range scripts {
		for _, p := range script.facts.writes {
			if at, ok := dirs.resolve(script.inlineScript, p); ok && len(writes) < bashWriteProbeLimit {
				writes = append(writes, BashWrite{Path: at, Shape: "script write"})
			}
		}
	}
	return writes
}

// enrichBashScriptRead answers inline Python that reads indexed source.
// answered is false when the command runs no such script, so the caller
// falls through to its own verdict.
// The deadline bounds the analysis; a zero one is inlineScriptTime from now.
// What only the extra analysis sees is answered by enrichBashCommand after
// this, with its own budget.
func enrichBashScriptRead(cmd, cwd string, scopeProbe fileScopeProbe, deadline time.Time) (enrichResult, bool) {
	started := time.Now()
	if deadline.IsZero() {
		deadline = started.Add(inlineScriptTime)
	}
	scripts := pythonScriptsAt(cmd)
	if len(scripts) == 0 {
		return enrichResult{}, false
	}
	dirs := newCommandDirs(cmd)
	return scriptReadAnswer(analyzeInlineScripts(scripts, deadline, false), false, dirs, cwd, scopeProbe, started)
}

// scriptReadAnswer answers the reads and edits of the scripts: with second
// false, the open()/pathlib reads and edits of the scripts found without a
// wrapper; with second true, os.open reads and the reads and edits of the
// scripts found only through a wrapper. Each call has its own probe budget.
func scriptReadAnswer(scripts []analyzedScript, second bool, dirs commandDirs, cwd string, scopeProbe fileScopeProbe, started time.Time) (enrichResult, bool) {
	type candidate struct {
		script  inlineScript
		path    string
		guessed bool // the write target is unnamed: it may be another file
	}
	var edits, reads []candidate
	for _, script := range scripts {
		facts := script.facts
		if script.wrapped == second {
			for _, p := range facts.reads {
				if facts.writeBack[p] {
					edits = append(edits, candidate{script.inlineScript, p, facts.guessed[p]})
				} else {
					reads = append(reads, candidate{script.inlineScript, p, false})
				}
			}
		}
		if second {
			for _, p := range facts.osReads {
				reads = append(reads, candidate{script.inlineScript, p, false})
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
		at, ok := dirs.resolve(c.script, c.path)
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
