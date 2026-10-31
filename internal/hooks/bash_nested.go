package hooks

import (
	"path/filepath"
	"strings"
	"time"
)

// bash_nested.go answers the commands a command runs as text: the string of
// `bash -c '…'`, and the literal command lines an inline Python script hands
// to os.system, os.popen or subprocess. Each gets the whole Bash policy, as
// if the agent had typed it, one level deep: a command nested in a nested
// command has no opinion.

// maxNestedCommands bounds the nested commands one command is examined for,
// and maxNestedCommand the size of each; past them the hook stops looking.
const (
	maxNestedCommands = 8
	maxNestedCommand  = 64 << 10
)

// isNestingShell names the shells whose `-c` string is a command of its own.
func isNestingShell(name string) bool {
	switch name {
	case "bash", "sh", "dash", "zsh":
		return true
	}
	return false
}

// shellInline returns the command string of a shell run with -c: `bash -c
// CMD`, `bash -lc CMD`, `sh -ec CMD`, `bash -o pipefail -c CMD`. The string
// is the first operand after the options; without -c the operand is a
// script file, which is not inline.
func shellInline(words []string) (string, bool) {
	hasC := false
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--" || w == "-":
			if hasC && i+1 < len(words) {
				return words[i+1], true
			}
			return "", false
		case w == "--rcfile" || w == "--init-file":
			i++
		case strings.HasPrefix(w, "--"):
		case len(w) > 1 && (w[0] == '-' || w[0] == '+'):
			for _, f := range w[1:] {
				switch f {
				case 'c':
					hasC = hasC || w[0] == '-'
				case 'o', 'O':
					i++ // -o pipefail, +O extglob
				}
			}
		default:
			return w, hasC
		}
	}
	return "", false
}

// shellQuoteWord quotes one word for a POSIX shell, leaving a word that
// needs no quoting bare, so shellWords reads it back exactly.
func shellQuoteWord(w string) string {
	if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:,+@%-") == "" {
		return w
	}
	return shellSingleQuote(w)
}

// nestedCommand is one command another command runs, and the directory it
// runs in.
type nestedCommand struct {
	cmd, cwd string
}

// nestedCommands lists the commands cmd runs as text, in order: each shell
// -c string, then each literal command line of its inline Python. A command
// whose working directory the hook cannot tell — after a cd it cannot
// follow, under a launcher that moves it, or from a script that may chdir —
// is left out rather than read with absolute paths only: it fails open.
func nestedCommands(shells []inlineScript, scripts []analyzedScript, dirs commandDirs, cwd string) []nestedCommand {
	var out []nestedCommand
	add := func(cmd, dir string) {
		switch {
		case filepath.IsAbs(dir):
		case dir == "":
			dir = cwd
		case cwd == "":
			return
		default:
			dir = filepath.Join(cwd, dir)
		}
		out = append(out, nestedCommand{cmd, dir})
	}
	for _, shell := range shells {
		if dir, ok := dirs.at(shell); ok {
			add(shell.code, dir)
		}
	}
	for _, script := range scripts {
		if dir, ok := dirs.pythonDir(script.inlineScript); ok {
			for _, line := range script.facts.commands {
				add(line, dir)
			}
		}
	}
	return out
}

// enrichNestedCommands runs the Bash policy over the nested commands of a
// command whose own answer is outer, which is not a deny. A nested deny wins;
// otherwise outer stands, or the first nested answer when outer is empty.
// The commands share the outer command's deadline, and those past the bound
// or the deadline are not examined.
func enrichNestedCommands(outer enrichResult, nested []nestedCommand, probe fileScopeProbe, depth int, deadline time.Time) enrichResult {
	answer := outer
	for i, n := range nested {
		if i >= maxNestedCommands || !time.Now().Before(deadline) {
			break
		}
		if len(n.cmd) > maxNestedCommand {
			continue
		}
		r := enrichBashCommand(n.cmd, n.cwd, probe, depth+1, deadline)
		if r.deny {
			return r
		}
		if answer == (enrichResult{}) {
			answer = r
		}
	}
	return answer
}
