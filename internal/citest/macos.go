package citest

import (
	"bytes"
	"context"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// macOSLabel opts one pull request into the macOS test partitions.
const macOSLabel = "ci:macos"

// MacOSSelection records whether a plan runs the macOS test partitions and
// why. Pull requests leave them out unless a change can behave differently on
// macOS; every other event runs them. The macOS build job is not affected.
type MacOSSelection struct {
	Selected bool         `json:"selected"`
	Reason   string       `json:"reason"`
	Label    string       `json:"label,omitempty"`
	Matched  []MacOSMatch `json:"matched_paths,omitempty"`
}

// MacOSMatch is one changed path and the rule that made it macOS-sensitive.
type MacOSMatch struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

var (
	darwinFileSuffix = regexp.MustCompile(`_darwin(_[a-z0-9]+)?(_test)?\.go$`)
	ciCommandDir     = regexp.MustCompile(`^scripts/ci-[^/]+/`)
	watcherFamily    = regexp.MustCompile(`(?i)watch|storm|realtime|poller`)
	nativeSource     = []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx", ".m", ".mm", ".s", ".S"}
	// Daemon start, launch, socket and process-identity sources, by file
	// name prefix. Their tests share the prefix and are included with them.
	daemonFamilies = map[string][]string{
		"internal/daemon/": {"autostart", "client", "server", "paths", "peer_pid", "pidfile", "runtime_files", "runtime_record", "fdlimit"},
		"cmd/gortex/":      {"daemon.go", "daemon_test.go", "daemon_service", "daemon_supervisor", "daemon_detach", "daemon_handoff"},
	}
)

// macOSPathRule names the rule that makes name macOS-sensitive from its path
// alone, or returns "". It also covers deleted paths, which have no content.
func macOSPathRule(name string) string {
	base := path.Base(name)
	switch {
	case strings.HasPrefix(name, ".github/workflows/"):
		return "CI workflow"
	case ciCommandDir.MatchString(name):
		return "CI command"
	case strings.HasPrefix(name, "internal/citest/"):
		return "CI planner and runner"
	case base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum":
		return "Go module or checksum file"
	case strings.HasSuffix(name, ".go") && darwinFileSuffix.MatchString(base):
		return "darwin file suffix"
	case strings.HasPrefix(name, "internal/platform/"):
		return "platform layer"
	case strings.HasPrefix(name, "internal/pathkey/"):
		return "path keys and case folding"
	case strings.HasPrefix(name, "internal/thirdparty/fswatcher/") || strings.HasPrefix(name, "internal/thirdparty/fsnotify/"):
		return "file watcher backend"
	case path.Dir(name) == "internal/indexer" && watcherFamily.MatchString(base):
		return "file watcher"
	case slices.Contains(nativeSource, path.Ext(base)) && !slices.Contains(strings.Split(name, "/"), "testdata"):
		return "C, C++ or Objective-C source"
	}
	for dir, prefixes := range daemonFamilies {
		if path.Dir(name)+"/" != dir {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(base, prefix) {
				return "daemon start, socket or process code"
			}
		}
	}
	return ""
}

// macOSContentRule reads a Go file's header and imports, so a new darwin file
// is covered without a list to maintain.
func macOSContentRule(name string, data []byte) string {
	if expr := buildConstraint(data); expr != nil {
		if mentionsTag(expr, "darwin") {
			return "darwin build constraint"
		}
		if expr.Eval(goosTags("darwin")) && !expr.Eval(goosTags("linux")) {
			return "build constraint that differs on macOS"
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
	if err != nil {
		return "Go file that does not parse"
	}
	for _, spec := range file.Imports {
		if spec.Path.Value == `"C"` {
			return "cgo source"
		}
	}
	if bytes.Contains(data, []byte(`"darwin"`)) {
		return "darwin runtime check"
	}
	return ""
}

// buildConstraint returns the file's //go:build (or legacy +build) expression.
func buildConstraint(data []byte) constraint.Expr {
	var plus []constraint.Expr
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "/*") || strings.HasPrefix(line, "*") {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			break // constraints must precede the package clause
		}
		if constraint.IsGoBuild(line) {
			if expr, err := constraint.Parse(line); err == nil {
				return expr
			}
			return &constraint.TagExpr{Tag: "darwin"} // unparseable: assume sensitive
		}
		if constraint.IsPlusBuild(line) {
			if expr, err := constraint.Parse(line); err == nil {
				plus = append(plus, expr)
			}
		}
	}
	if len(plus) == 0 {
		return nil
	}
	expr := plus[0]
	for _, next := range plus[1:] {
		expr = &constraint.AndExpr{X: expr, Y: next}
	}
	return expr
}

func mentionsTag(expr constraint.Expr, tag string) bool {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return e.Tag == tag
	case *constraint.NotExpr:
		return mentionsTag(e.X, tag)
	case *constraint.AndExpr:
		return mentionsTag(e.X, tag) || mentionsTag(e.Y, tag)
	case *constraint.OrExpr:
		return mentionsTag(e.X, tag) || mentionsTag(e.Y, tag)
	}
	return false
}

// goosTags evaluates a constraint as CI builds it: native GOOS, unix, cgo and
// the runner's architecture. Other tags are off on both sides.
func goosTags(goos string) func(string) bool {
	return func(tag string) bool {
		switch tag {
		case goos, "unix", "cgo", "gc":
			return true
		case "arm64":
			return goos == "darwin"
		case "amd64":
			return goos == "linux"
		}
		return strings.HasPrefix(tag, "go1.")
	}
}

// macOSMatches lists the changed paths that make a PR macOS-sensitive. A
// path present at head is read from the checkout; a deleted or renamed-away
// path is read from the comparison base.
func (p *Planner) macOSMatches(ctx context.Context, base string, changes []Change) ([]MacOSMatch, error) {
	matches := []MacOSMatch{}
	for _, change := range changes {
		for i, name := range change.Paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rule := macOSPathRule(name)
			if rule == "" && strings.HasSuffix(name, ".go") {
				oldSide := change.Status == "D" || (len(change.Paths) == 2 && i == 0)
				data, err := p.changedContent(ctx, base, name, oldSide)
				if err != nil {
					rule = "Go file that cannot be read"
				} else {
					rule = macOSContentRule(name, data)
				}
			}
			if rule != "" {
				matches = append(matches, MacOSMatch{Path: name, Rule: rule})
			}
		}
	}
	return matches, nil
}

func (p *Planner) changedContent(ctx context.Context, base, name string, oldSide bool) ([]byte, error) {
	if !oldSide {
		return os.ReadFile(filepath.Join(p.Root, filepath.FromSlash(name)))
	}
	fn := p.command
	if fn == nil {
		fn = metadataCommand
	}
	return fn(ctx, p.Root, baseEnv(p.Env), "git", "show", base+":"+name)
}

// planMacOS decides whether the plan runs the macOS test partitions. Only a
// pull request whose changed paths are known can leave them out.
func (p *Planner) planMacOS(ctx context.Context, event Event, base string, changes []Change, changesKnown bool) MacOSSelection {
	switch {
	case event.Name != "pull_request":
		return MacOSSelection{Selected: true, Reason: "full macOS matrix for " + event.Name}
	case event.Full || slices.Contains(event.Labels, "ci:full"):
		return MacOSSelection{Selected: true, Reason: "explicit full-suite override"}
	case event.Author == "dependabot[bot]":
		return MacOSSelection{Selected: true, Reason: "Dependabot dependency update"}
	}
	s := MacOSSelection{}
	if slices.Contains(event.Labels, macOSLabel) {
		s.Selected, s.Label = true, macOSLabel
	}
	if !changesKnown {
		s.Selected = true
		s.Reason = "changed paths are unknown"
		if s.Label != "" {
			s.Reason = "pull request has the " + macOSLabel + " label; " + s.Reason
		}
		return s
	}
	matches, err := p.macOSMatches(ctx, base, changes)
	if err != nil {
		return MacOSSelection{Selected: true, Label: s.Label, Reason: "macOS path check failed: " + err.Error()}
	}
	s.Matched = matches
	var reasons []string
	if s.Label != "" {
		reasons = append(reasons, "pull request has the "+macOSLabel+" label")
	}
	if len(matches) > 0 {
		s.Selected = true
		rules := []string{}
		for _, m := range matches {
			if !slices.Contains(rules, m.Rule) {
				rules = append(rules, m.Rule)
			}
		}
		reasons = append(reasons, fmt.Sprintf("%d changed path(s) are macOS-sensitive (%s)", len(matches), strings.Join(rules, ", ")))
	}
	if !s.Selected {
		reasons = append(reasons, "no macOS-sensitive change and no "+macOSLabel+" label")
	}
	s.Reason = strings.Join(reasons, "; ")
	return s
}

// planMatrix drops the macOS rows from a matrix when macOS is not selected.
func (p Policy) planMatrix(m Matrix, macOS bool) Matrix {
	out := Matrix{Include: []MatrixRow{}}
	for _, row := range m.Include {
		if macOS || !p.isDarwin(row.OS) {
			out.Include = append(out.Include, row)
		}
	}
	return out
}

func (p Policy) isDarwin(runner string) bool {
	return slices.ContainsFunc(p.Targets, func(t Target) bool { return t.Runner == runner && t.GOOS == "darwin" })
}
