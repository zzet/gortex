package hooks

import (
	"os"
	"path/filepath"
	"strings"
)

// A Bash grep is the one read shape the scope gate cannot judge by its target:
// the gate sees only the cwd, so `grep -n hook ~/.gortex/config.yaml` from a
// tracked checkout was redirected to a graph that has nothing for the file it
// searches (#893). The helpers below read the searched paths off the command
// line so enrichBash can give the call the same answer `cat` of that file and
// the Grep tool with `path:` already get.

// leadingCdChain reads the `cd <dir> &&` statements a command opens with and
// returns the directory they end in (relative to the hook's cwd unless a step
// was absolute) and how many primary segments they span. Only `&&` chains
// count: after `cd x;` or `cd x ||` the next command may run where it started.
// A step whose directory cannot be read off the command line (`cd`, `cd -`,
// `cd $DIR`) ends the chain.
func leadingCdChain(cmd string) (dir string, segments int) {
	rest := strings.TrimSpace(cmd)
	for {
		primary := primarySegments(rest)
		if len(primary) == 0 || !strings.HasPrefix(rest, primary[0]) {
			return dir, segments
		}
		tokens := skipCommandPrefix(tokenize(primary[0]))
		if len(tokens) != 2 || tokens[0] != "cd" {
			return dir, segments
		}
		step, ok := literalShellPath(tokens[1])
		if !ok {
			return dir, segments
		}
		after := strings.TrimSpace(rest[len(primary[0]):])
		if !strings.HasPrefix(after, "&&") {
			return dir, segments
		}
		dir = joinShellPath(dir, step)
		segments++
		rest = strings.TrimSpace(after[2:])
	}
}

// segmentWorkingDir reports the directory primary segment idx runs in, as
// leadingCdChain left it. known is false when an earlier segment may have
// moved the shell somewhere the command line does not prove — anything but
// the segment straight after a leading `cd` chain, once any directory change
// has happened.
func segmentWorkingDir(segments []string, idx int, cdDir string, cdSegments int) (string, bool) {
	if idx == cdSegments {
		return cdDir, true
	}
	if cdSegments > 0 {
		return "", false
	}
	for _, seg := range segments[:idx] {
		tokens := skipCommandPrefix(tokenize(seg))
		if len(tokens) > 0 && (tokens[0] == "cd" || tokens[0] == "pushd" || tokens[0] == "popd") {
			return "", false
		}
	}
	return "", true
}

// The single-letter options whose value is the next token, or the rest of
// the cluster. They differ per tool — `-r` is recursion to grep but a
// replacement string to rg, `-f` a pattern file to grep but follow-symlinks
// to ag — so one shared table would misread one of them.
const (
	grepShortFlagsTakingArg = "efABCmdDX"
	rgShortFlagsTakingArg   = "efABCmdErtTgjM"
	agShortFlagsTakingArg   = "ABCGmp"
)

// grepLongFlagsTakingArg are the long options of grep, rg and ag that take
// their value as the next token when it is not attached with `=`.
var grepLongFlagsTakingArg = map[string]bool{
	"--regexp": true, "--file": true,
	"--include": true, "--exclude": true, "--exclude-dir": true, "--exclude-from": true,
	"--max-count": true, "--context": true, "--after-context": true, "--before-context": true,
	"--directories": true, "--devices": true, "--binary-files": true, "--label": true,
	"--glob": true, "--iglob": true, "--type": true, "--type-not": true, "--type-add": true, "--type-clear": true,
	"--replace": true, "--max-depth": true, "--depth": true, "--threads": true, "--encoding": true,
	"--max-columns": true, "--max-filesize": true, "--ignore-file": true,
	"--pre": true, "--pre-glob": true, "--sort": true, "--sortr": true, "--colors": true,
	"--engine": true, "--path-separator": true, "--context-separator": true,
	"--file-search-regex": true,
}

// grepSearchPaths returns the file and directory operands of a grep/rg/ag
// invocation, joined onto dir. The first positional is the pattern unless one
// came from -e/-f (--regexp/--file); everything after `--` is positional.
// Option values (`-g GLOB`, `--include GLOB`, `-t TYPE`, `-f FILE`) are not
// searched paths and are dropped.
//
// Nil when no path is named, and when any operand is not a literal path
// (stdin `-`, a variable, `~user`): one operand the hook cannot place leaves
// the whole search unproven. A misread option only ever turns a word into a
// relative operand, which resolves into the tracked cwd and keeps the policy.
func grepSearchPaths(tokens []string, dir string) []string {
	shortWithArg := grepShortFlagsTakingArg
	switch tokens[0] {
	case "rg":
		shortWithArg = rgShortFlagsTakingArg
	case "ag":
		shortWithArg = agShortFlagsTakingArg
	}
	var positional []string
	patternFromFlag := false
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		if t == "--" {
			positional = append(positional, tokens[i+1:]...)
			break
		}
		if strings.HasPrefix(t, "--") {
			name, _, attached := strings.Cut(t, "=")
			if name == "--regexp" || name == "--file" {
				patternFromFlag = true
			}
			// ag's --ignore takes a pattern; rg's is a bare switch and grep reads
			// it as --ignore-case, so swallowing a value there drops an operand.
			if !attached && (grepLongFlagsTakingArg[name] || name == "--ignore" && tokens[0] == "ag") {
				i++
			}
			continue
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			for j := 1; j < len(t); j++ {
				if strings.IndexByte(shortWithArg, t[j]) < 0 {
					continue
				}
				if t[j] == 'e' || t[j] == 'f' {
					patternFromFlag = true
				}
				if j == len(t)-1 {
					i++ // the value is the next token, not the rest of the cluster
				}
				break
			}
			continue
		}
		positional = append(positional, t)
	}
	if !patternFromFlag && len(positional) > 0 {
		positional = positional[1:]
	}
	if len(positional) == 0 {
		return nil
	}
	paths := make([]string, 0, len(positional))
	for _, operand := range positional {
		p, ok := literalShellPath(operand)
		if !ok {
			return nil
		}
		paths = append(paths, joinShellPath(dir, p))
	}
	return paths
}

// literalShellPath returns operand as the path the shell hands the command,
// expanding a leading `~` or `~/`. ok is false for anything whose value the
// command line alone does not fix: stdin `-`, a parameter or command
// substitution, or another user's home.
func literalShellPath(operand string) (string, bool) {
	if operand == "" || operand == "-" || strings.ContainsAny(operand, "$`") {
		return "", false
	}
	if !strings.HasPrefix(operand, "~") {
		return operand, true
	}
	if operand != "~" && !strings.HasPrefix(operand, "~/") {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, strings.TrimPrefix(operand, "~")), true
}

// joinShellPath resolves p the way the shell does after `cd dir`: an absolute
// p stands alone, a relative one is taken from dir.
func joinShellPath(dir, p string) string {
	if dir == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// bashSearchOutsideTrackedRepos reports whether every path a Bash search
// names is provably disjoint from every tracked repo: not inside one, and —
// since grep -r / rg descend — not a parent of one either. A glob operand is
// judged by its literal directory prefix, which contains everything the glob
// can expand to.
//
// Like hookCallTargetsTrackedRepo it answers true only on proof. No paths
// (stdin, cwd recursion), an unknown tracked-repo list, or a relative operand
// with no absolute cwd all return false and leave the policy as it was.
func bashSearchOutsideTrackedRepos(paths []string, cwd string) bool {
	repos := hookTrackedReposFn()
	if len(paths) == 0 || len(repos) == 0 {
		return false
	}
	for _, p := range paths {
		abs, ok := hookAbsScopePath(globLiteralPrefix(p), cwd)
		if !ok {
			return false
		}
		if _, tracked := trackedRepoForPath(abs); tracked {
			return false
		}
		parent := abs
		if !strings.HasSuffix(parent, string(filepath.Separator)) {
			parent += string(filepath.Separator) // the filesystem root already ends in one
		}
		for _, r := range repos {
			if r.Path != "" && strings.HasPrefix(r.Path, parent) {
				return false
			}
		}
	}
	return true
}

// bashSearchNonSource reports whether every path a Bash search names is a
// literal non-source file: docs or config the Grep tool and `cat` already
// leave alone. No paths, a directory, or any operand the shell could still
// rewrite return false and keep the policy.
//
// Only plain words qualify. A glob is refused outright: what it expands to
// depends on shell options the hook cannot see (nullglob drops a non-matching
// operand, so `rg sym none*.md` searches the cwd; nocaseglob and globs that
// match a directory reach source the pattern does not name). Quotes,
// parentheses (zsh glob qualifiers) and backslash escapes are refused too:
// `x.g\o` is x.go to the shell.
func bashSearchNonSource(paths []string, cwd string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if strings.ContainsAny(p, `"'()*?[]{}`) || (filepath.Separator != '\\' && strings.Contains(p, `\`)) {
			return false
		}
		if looksLikeSourceFile(p) || !scopeNamesFile(cwd, p) {
			return false
		}
	}
	return true
}

// searchOperandsUncertain reports whether the hook cannot trust the operands
// it read off the command's searches, so no search may be passed over:
//
//   - a grep/rg/ag after a pipe names operands of its own. Only primary
//     segments are classified, so passing over the grep that feeds it would
//     silence it. A piped grep that filters stdin names none.
//   - a search uses ag, or a long option the parser does not know (neither a
//     searchSwitches switch nor in grepLongFlagsTakingArg) without an
//     attached `=value`. An option that takes the next word as its value
//     (`--group-separator README.md`, or an abbreviation GNU getopt accepts)
//     would otherwise be read as the searched file, while the tool, left
//     with no operand, searches the working directory.
func searchOperandsUncertain(cmd string) bool {
	all := allCommandSegments(cmd)
	piped := searchesNamingPaths(all) > searchesNamingPaths(primarySegments(cmd))
	if piped {
		return true
	}
	for _, seg := range all {
		tokens := searchTokens(seg)
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] == "ag" {
			return true
		}
		for _, t := range tokens[1:] {
			if t == "--" {
				break
			}
			if strings.HasPrefix(t, "--") && !strings.Contains(t, "=") && !searchSwitches[t] && !grepLongFlagsTakingArg[t] {
				return true
			}
		}
	}
	return false
}

// searchSwitches are long options of grep and rg that never take a value.
var searchSwitches = map[string]bool{
	"--recursive": true, "--dereference-recursive": true, "--ignore-case": true,
	"--no-ignore-case": true, "--smart-case": true, "--case-sensitive": true,
	"--line-number": true, "--no-line-number": true, "--count": true,
	"--count-matches": true, "--files-with-matches": true, "--files-without-match": true,
	"--word-regexp": true, "--line-regexp": true, "--fixed-strings": true,
	"--extended-regexp": true, "--basic-regexp": true, "--perl-regexp": true,
	"--invert-match": true, "--no-filename": true, "--with-filename": true,
	"--only-matching": true, "--quiet": true, "--silent": true, "--no-messages": true,
	"--text": true, "--byte-offset": true, "--null": true, "--initial-tab": true,
	"--hidden": true, "--no-ignore": true, "--follow": true, "--no-heading": true,
	"--heading": true, "--column": true, "--vimgrep": true, "--json": true,
	"--trim": true, "--multiline": true, "--pcre2": true, "--stats": true,
	"--line-buffered": true, "--null-data": true, "--binary": true, "--colour": true,
	"--color": true, "--no-config": true, "--passthru": true, "--files": true,
	"--unrestricted": true, "--no-ignore-vcs": true, "--no-ignore-dot": true,
	"--no-ignore-parent": true, "--no-ignore-global": true, "--crlf": true,
	"--search-zip": true, "--pretty": true, "--one-file-system": true,
	"--no-require-git": true, "--mmap": true, "--no-mmap": true, "--sort-files": true,
	"--glob-case-insensitive": true, "--no-group-separator": true,
}

// searchTokens returns seg's tokens when it runs grep, egrep, fgrep, rg or
// ag (lowercased command first), and nil otherwise.
func searchTokens(seg string) []string {
	command, _ := splitSegmentRedirects(seg)
	tokens := skipCommandPrefix(tokenize(command))
	if len(tokens) == 0 {
		return nil
	}
	switch name := strings.ToLower(tokens[0]); name {
	case "grep", "rg", "ag", "egrep", "fgrep":
		return append([]string{name}, tokens[1:]...)
	}
	return nil
}

func searchesNamingPaths(segments []string) int {
	n := 0
	for _, seg := range segments {
		if tokens := searchTokens(seg); tokens != nil {
			if _, at, ok := extractGrepPatternAt(tokens); ok && at < len(tokens)-1 {
				n++
			}
		}
	}
	return n
}

// globLiteralPrefix cuts a path at the first component holding a glob
// metacharacter, returning the directory every expansion lies under.
func globLiteralPrefix(p string) string {
	i := strings.IndexAny(p, "*?[{")
	if i < 0 {
		return p
	}
	cut := strings.LastIndex(p[:i], string(filepath.Separator))
	switch {
	case cut < 0:
		return "."
	case cut == 0:
		return string(filepath.Separator)
	default:
		return p[:cut]
	}
}
