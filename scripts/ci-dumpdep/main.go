// ci-dumpdep fails when a binary links code that makes the Go linker keep
// every exported method of every reachable type.
//
// The linker's dead-code pass drops methods that nothing can call. A call to
// reflect.Value.Method, or to MethodByName with a name that is not a
// compile-time constant, defeats it: the linker cannot know which method will
// be asked for, so it keeps all of them. Executing a text/template or
// html/template makes exactly that call to evaluate fields. Rendering the
// daemon's service unit files with text/template once kept about 6 MB of
// otherwise unused methods in the stripped gortex binary.
//
// It reads the reachability listing a -dumpdep link prints:
//
//	go build -ldflags=-dumpdep -o /dev/null ./cmd/gortex/ 2>&1 | go run ./scripts/ci-dumpdep
//
// Each "parent -> symbol" line marks symbol reachable from parent. The linker
// appends <ReflectMethod> to a function the compiler flagged as such a
// method lookup, and <UsedInIface> to a type converted to an interface.
// Every real program has such types, so a listing without that flag is not
// one this tool can read: it is rejected rather than reported clean.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	reflectMethodFlag = "<ReflectMethod>"
	usedInIfaceFlag   = "<UsedInIface>"
)

// templateExecution is the function every text/template and html/template
// execution runs through; Execute and ExecuteTemplate are wrappers the
// compiler may inline into their caller. It always reaches the flagged
// lookup, and reporting it puts the caller that executes a template at the
// end of the chain rather than template internals.
const templateExecution = "text/template.(*Template).execute"

// finding is a reachable symbol that keeps all exported methods, with the
// chain of symbols that reached it, root first.
type finding struct {
	symbol string
	path   []string
}

// symbolName drops the attribute flags -dumpdep appends to a symbol.
func symbolName(s string) string {
	for _, flag := range []string{reflectMethodFlag, usedInIfaceFlag} {
		s = strings.TrimSuffix(s, flag)
	}
	return strings.TrimSuffix(s, " ")
}

// maxOtherLines bounds how much non-listing input, such as a build failure,
// is echoed back when the input turns out not to be a link listing.
const maxOtherLines = 100

// check reads a -dumpdep listing and returns the reachable symbols that make
// the linker keep every exported method. Lines that are not dependency edges,
// such as the package header go build prints, are skipped.
func check(r io.Reader) ([]finding, error) {
	parent := map[string]string{}
	var offenders, other []string
	flagged := 0
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		from, to, ok := strings.Cut(scanner.Text(), " -> ")
		if !ok {
			if len(other) < maxOtherLines {
				other = append(other, scanner.Text())
			}
			continue
		}
		if strings.HasSuffix(to, usedInIfaceFlag) {
			flagged++
		}
		name := symbolName(to)
		if _, seen := parent[name]; !seen {
			parent[name] = symbolName(from)
		}
		if strings.HasSuffix(to, reflectMethodFlag) || name == templateExecution {
			offenders = append(offenders, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if _, ok := parent["main.main"]; !ok {
		msg := "main.main is not in the input; pipe in the output of a -dumpdep link of a command"
		if len(other) > 0 {
			msg += ". The input began:\n" + strings.Join(other, "\n")
		}
		return nil, errors.New(msg)
	}
	// Without the attribute flags a <ReflectMethod> could not be seen
	// either, so a listing without them must not pass as clean.
	if flagged == 0 {
		return nil, errors.New("no symbol carries the " + usedInIfaceFlag + " flag, so this is not a -dumpdep listing this tool can read")
	}
	findings := make([]finding, 0, len(offenders))
	for _, name := range offenders {
		path := []string{name}
		for at := name; len(path) <= len(parent); {
			from, ok := parent[at]
			if !ok || from == "_" {
				break
			}
			path = append(path, from)
			at = from
		}
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		findings = append(findings, finding{symbol: name, path: path})
	}
	return findings, nil
}

func run(stdin io.Reader, stdout, stderr io.Writer) int {
	findings, err := check(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "ci-dumpdep: %v\n", err)
		return 1
	}
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "No reachable dynamic method lookup: the linker can drop unused methods.")
		return 0
	}
	fmt.Fprintf(stderr, "ci-dumpdep: %d reachable symbols make the linker keep every exported method of every reachable type.\n", len(findings))
	fmt.Fprintln(stderr, "Render text with fmt or strings rather than text/template or html/template, and look methods up by constant name.")
	for _, f := range findings {
		fmt.Fprintf(stderr, "\n%s, reached by:\n", f.symbol)
		for _, hop := range f.path {
			fmt.Fprintf(stderr, "  %s\n", hop)
		}
	}
	return 1
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}
