package citest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

type Event struct {
	Name, Base, Head, Ref, Author string
	Full                          bool
	Labels                        []string
	PullRequest                   *PullRequest
	MetadataError                 string
}

// PullRequest is event metadata, distinct from Head: CI tests the synthetic
// merge checkout, whose second parent must be this PR's source revision.
type PullRequest struct {
	Number              int
	Head, Base, BaseRef string
}
type Manifest struct {
	Schema    int                 `json:"schema"`
	Head      string              `json:"head"`
	Base      string              `json:"base"`
	Mode      string              `json:"mode"`
	Reason    string              `json:"reason"`
	EmptyOK   bool                `json:"empty_ok"`
	Toolchain string              `json:"toolchain"`
	Module    string              `json:"module"`
	Packages  map[string][]string `json:"packages"`
	Census    map[string][]string `json:"census"`
	Targets   []Target            `json:"targets"`
	Matrix    Matrix              `json:"matrix"`
	SHA256    string              `json:"sha256,omitempty"`
}

func (m Manifest) Seal() string {
	m.SHA256 = ""
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func DecodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return m, err
	}
	keys := []string{"schema", "head", "base", "mode", "reason", "empty_ok", "toolchain", "module", "packages", "census", "targets", "matrix", "sha256"}
	if len(fields) != len(keys) {
		return m, fmt.Errorf("incomplete test selection manifest")
	}
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return m, fmt.Errorf("missing or null manifest field %s", key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	return m, nil
}

type Planner struct {
	Root    string
	Env     []string
	command commandFunc
}

func NewPlanner(root string) *Planner {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Planner{Root: root, Env: os.Environ(), command: metadataCommand}
}
func (p *Planner) exec(ctx context.Context, env []string, args ...string) (string, error) {
	fn := p.command
	if fn == nil {
		fn = metadataCommand
	}
	out, err := fn(ctx, p.Root, baseEnv(env), args...)
	if err == nil && !utf8.Valid(out) {
		err = fmt.Errorf("non-UTF8 metadata")
	}
	return strings.TrimSpace(string(out)), err
}

type Change struct {
	Status string
	Paths  []string
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// testedMergeBase selects only the verified integration's first parent for
// changed-file discovery. Dependency discovery and manifest validation still
// use the checked-out merge, including changes that arrived on main meanwhile.
func (p *Planner) testedMergeBase(ctx context.Context, event Event) (string, error) {
	if event.MetadataError != "" {
		return "", fmt.Errorf("unavailable pull-request metadata: %s", event.MetadataError)
	}
	pr := event.PullRequest
	if pr == nil || pr.Number <= 0 || pr.BaseRef != "main" || event.Author == "" || event.Ref != fmt.Sprintf("refs/pull/%d/merge", pr.Number) {
		return "", fmt.Errorf("unverified pull-request merge event")
	}
	if !commitSHA.MatchString(event.Base) || !commitSHA.MatchString(event.Head) || !commitSHA.MatchString(pr.Head) || pr.Base != event.Base {
		return "", fmt.Errorf("invalid or inconsistent pull-request revisions")
	}
	actual, err := p.exec(ctx, p.Env, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if actual != event.Head {
		return "", fmt.Errorf("checkout does not match the event revision")
	}
	text, err := p.exec(ctx, p.Env, "git", "show", "--no-patch", "--format=%P", event.Head)
	if err != nil {
		return "", err
	}
	parents := strings.Fields(text)
	if len(parents) != 2 || !commitSHA.MatchString(parents[0]) || !commitSHA.MatchString(parents[1]) {
		return "", fmt.Errorf("tested pull-request revision is not a two-parent merge")
	}
	if parents[1] != pr.Head {
		return "", fmt.Errorf("tested merge second parent does not match the pull-request head")
	}
	// A normal base advance preserves ancestry. Rewrites or unrelated metadata
	// are uncertain, so retain the full-suite fallback rather than guessing.
	if _, err := p.exec(ctx, p.Env, "git", "merge-base", "--is-ancestor", event.Base, parents[0]); err != nil {
		return "", fmt.Errorf("tested merge base is not descended from the event base: %w", err)
	}
	return parents[0], nil
}

func (p *Planner) Changes(ctx context.Context, base, head string) ([]Change, error) {
	if !commitSHA.MatchString(base) || !commitSHA.MatchString(head) {
		return nil, fmt.Errorf("missing or invalid comparison revisions")
	}
	for _, rev := range []string{base, head} {
		out, err := p.exec(ctx, p.Env, "git", "rev-parse", "--verify", rev+"^{commit}")
		if err != nil {
			return nil, err
		}
		if out != rev {
			return nil, fmt.Errorf("comparison revision is not an exact commit")
		}
	}
	actual, err := p.exec(ctx, p.Env, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if actual != head {
		return nil, fmt.Errorf("checkout does not match the event revision")
	}
	ancestor, err := p.exec(ctx, p.Env, "git", "merge-base", base, head)
	if err != nil {
		return nil, err
	}
	// Do not trim status output: trailing whitespace can be part of a path.
	fn := p.command
	if fn == nil {
		fn = metadataCommand
	}
	out, err := fn(ctx, p.Root, baseEnv(p.Env), "git", "diff", "--name-status", "-z", "--find-renames", ancestor, head, "--")
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(out) {
		return nil, fmt.Errorf("non-UTF8 Git paths")
	}
	return parseChanges(out)
}
func parseChanges(out []byte) ([]Change, error) {
	changes := []Change{}
	fields := strings.Split(string(out), "\x00")
	for len(fields) > 0 && fields[0] != "" {
		status := fields[0]
		fields = fields[1:]
		n := 1
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			n = 2
		}
		if len(fields) < n {
			return nil, fmt.Errorf("incomplete NUL-delimited Git diff")
		}
		for _, name := range fields[:n] {
			if name == "" {
				return nil, fmt.Errorf("empty Git path")
			}
		}
		changes = append(changes, Change{status, append([]string{}, fields[:n]...)})
		fields = fields[n:]
	}
	if len(fields) != 1 || fields[0] != "" {
		return nil, fmt.Errorf("invalid Git diff terminator")
	}
	return changes, nil
}
func ordinaryChange(c Change) error {
	if len(c.Paths) != 1 || (c.Status != "A" && c.Status != "M") {
		return fmt.Errorf("full fallback for %s: %s", c.Status, strings.Join(c.Paths, " -> "))
	}
	name := c.Paths[0]
	if ordinaryDocumentationPath(name) {
		return nil // filesystem and metadata qualification still required by Select
	}
	if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "scripts/") || strings.HasPrefix(name, ".github/") || strings.HasPrefix(name, "internal/citest/") {
		return fmt.Errorf("full fallback for %s: %s", c.Status, name)
	}
	return nil
}

type Graph struct {
	Module        string
	Census        map[string][]string
	reverse       map[string]set
	owners        map[string]set
	packageDirs   set
	documentation set
}
type goPackage struct {
	ImportPath, Dir                                                                                                               string
	Module                                                                                                                        *struct{ Main bool }
	Incomplete                                                                                                                    bool
	Error                                                                                                                         json.RawMessage
	DepsErrors                                                                                                                    []json.RawMessage
	Imports, TestImports, XTestImports, GoFiles, CgoFiles, TestGoFiles, XTestGoFiles, EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
}

func decodePackages(text string) ([]goPackage, error) {
	d := json.NewDecoder(strings.NewReader(text))
	rows := []goPackage{}
	for {
		var row goPackage
		err := d.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if row.ImportPath == "" || row.Dir == "" || row.Incomplete || len(row.Error) > 0 && !bytes.Equal(row.Error, []byte("null")) || len(row.DepsErrors) > 0 {
			return nil, fmt.Errorf("incomplete Go dependency graph")
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty Go package census")
	}
	return rows, nil
}
func addEdge(edges map[string]set, from, to string) {
	if edges[from] == nil {
		edges[from] = set{}
	}
	edges[from].add(to)
}
func (p *Planner) Discover(ctx context.Context) (Graph, error) {
	var g Graph
	if envValue(baseEnv(p.Env), "GOFLAGS") != "" || envValue(baseEnv(p.Env), "GOEXPERIMENT") != "" || envValue(baseEnv(p.Env), "GOWORK") != "" && envValue(baseEnv(p.Env), "GOWORK") != "off" {
		return g, fmt.Errorf("unsupported GOFLAGS, GOEXPERIMENT or workspace context")
	}
	policy, err := LoadPolicy()
	if err != nil {
		return g, err
	}
	mod, err := os.CreateTemp(p.Root, ".ci-selection-*.mod")
	if err != nil {
		return g, err
	}
	name := mod.Name()
	defer func() { _ = os.Remove(name) }()
	if err := mod.Close(); err != nil {
		return g, err
	}
	sum := strings.TrimSuffix(name, ".mod") + ".sum"
	defer func() { _ = os.Remove(sum) }()
	data, err := os.ReadFile(filepath.Join(p.Root, "go.mod"))
	if err != nil {
		return g, err
	}
	if err = os.WriteFile(name, data, 0600); err != nil {
		return g, err
	}
	data, err = os.ReadFile(filepath.Join(p.Root, "go.sum"))
	if err == nil {
		if err = os.WriteFile(sum, data, 0600); err != nil {
			return g, err
		}
	} else if !os.IsNotExist(err) {
		return g, err
	}
	flags := []string{"-mod=readonly", "-modfile=" + name}
	args := append([]string{"go", "list"}, flags...)
	module, err := p.exec(ctx, p.Env, append(args, "-m")...)
	if err != nil {
		return g, err
	}
	if module == "" || strings.Contains(module, "\n") {
		return g, fmt.Errorf("ambiguous module identity")
	}
	g = Graph{Module: module, Census: map[string][]string{}, reverse: map[string]set{}, owners: map[string]set{}, packageDirs: set{}}
	root, err := filepath.Abs(p.Root)
	if err != nil {
		return g, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return g, err
	}
	for _, target := range policy.Targets {
		var previous []string
		for _, race := range []bool{false, true} {
			env := withEnv(baseEnv(p.Env), map[string]string{"GOOS": target.GOOS, "GOARCH": target.GOARCH, "CGO_ENABLED": "1", "GOWORK": "off"})
			listing := append([]string{"go", "list"}, flags...)
			if race {
				listing = append(listing, "-race")
			}
			out, err := p.exec(ctx, env, append(append([]string{}, listing...), "-json", "./...")...)
			if err != nil {
				return g, err
			}
			rows, err := decodePackages(out)
			if err != nil {
				return g, err
			}
			tests := set{}
			for _, row := range rows {
				for _, dep := range append(append([]string{}, row.TestImports...), row.XTestImports...) {
					tests.add(dep)
				}
			}
			depArgs := append(append([]string{}, listing...), "-deps", "-json", "./...")
			depArgs = append(depArgs, tests.sorted()...)
			out, err = p.exec(ctx, env, depArgs...)
			if err != nil {
				return g, err
			}
			deps, err := decodePackages(out)
			if err != nil {
				return g, err
			}
			for _, row := range deps {
				for _, dep := range row.Imports {
					addEdge(g.reverse, dep, row.ImportPath)
				}
			}
			census := set{}
			for _, row := range rows {
				if row.Module == nil || !row.Module.Main || census.has(row.ImportPath) {
					return g, fmt.Errorf("invalid root Go package census")
				}
				census.add(row.ImportPath)
				dir, err := filepath.EvalSymlinks(row.Dir)
				if err != nil {
					return g, err
				}
				relative, err := filepath.Rel(root, dir)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return g, fmt.Errorf("package outside checkout")
				}
				g.packageDirs.add(filepath.ToSlash(relative))
				own := func(files []string, owner string) {
					for _, file := range files {
						addEdge(g.owners, filepath.ToSlash(filepath.Join(relative, file)), owner)
					}
				}
				own(row.EmbedFiles, "<embed>")
				own(row.TestEmbedFiles, "<embed>")
				own(row.XTestEmbedFiles, "<embed>")
				own(row.GoFiles, row.ImportPath)
				own(row.CgoFiles, row.ImportPath)
				own(row.CgoFiles, "<cgo>")
				own(row.TestGoFiles, row.ImportPath)
				own(row.XTestGoFiles, row.ImportPath)
				for _, dep := range append(append([]string{}, row.TestImports...), row.XTestImports...) {
					addEdge(g.reverse, dep, row.ImportPath)
				}
			}
			current := census.sorted()
			if previous != nil && !slices.Equal(previous, current) {
				return g, fmt.Errorf("race-specific package census requires full testing")
			}
			previous = current
		}
		g.Census[target.Runner] = previous
	}
	return g, nil
}

var specialDirective = regexp.MustCompile(`(?m)^//(?:go:generate|go:embed| Code generated.*DO NOT EDIT)`)

func (g Graph) Select(root string, changes []Change) ([]string, error) {
	selected := set{}
	for _, change := range changes {
		if err := ordinaryChange(change); err != nil {
			return nil, err
		}
		path := change.Paths[0]
		if ordinaryDocumentationPath(path) {
			if err := g.validateDocumentation(root, path); err != nil {
				return nil, err
			}
			continue
		}
		owners := g.owners[path]
		if len(owners) == 0 || owners.has("<cgo>") || owners.has("<embed>") {
			return nil, fmt.Errorf("unmapped, unsupported build-tag, embedded, or cgo input: %s", path)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) || specialDirective.Match(data) {
			return nil, fmt.Errorf("generated, embedded or invalid UTF-8 input: %s", path)
		}
		for name := range owners {
			selected.add(name)
		}
	}
	pending := selected.sorted()
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for dependent := range g.reverse[node] {
			if !selected.has(dependent) {
				selected.add(dependent)
				pending = append(pending, dependent)
			}
		}
	}
	return selected.sorted(), nil
}
func (p *Planner) Plan(ctx context.Context, event Event) (Manifest, error) {
	policy, err := LoadPolicy()
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Schema: 1, Head: event.Head, Base: event.Base, Mode: "full", Packages: map[string][]string{}, Census: map[string][]string{}, Targets: policy.Targets, Matrix: policy.FullMatrix()}
	var changes []Change
	switch {
	case event.Name != "pull_request":
		m.Reason = "full suite for " + event.Name
	case event.Full || slices.Contains(event.Labels, "ci:full"):
		m.Reason = "explicit full-suite override"
	case event.Author == "dependabot[bot]":
		m.Reason = "full suite for Dependabot dependency updates"
	default:
		var comparisonBase string
		comparisonBase, err = p.testedMergeBase(ctx, event)
		if err == nil {
			m.Base = comparisonBase
			changes, err = p.Changes(ctx, comparisonBase, event.Head)
		}
		if err == nil {
			for _, c := range changes {
				if err = ordinaryChange(c); err != nil {
					break
				}
			}
		}
		if err == nil {
			var g Graph
			g, err = p.Discover(ctx)
			if err == nil {
				g.documentation, err = p.documentationInputs(ctx, event.Head, changes)
			}
			if err == nil {
				var selected []string
				selected, err = g.Select(p.Root, changes)
				if err == nil {
					m.Toolchain, err = p.exec(ctx, p.Env, "go", "env", "GOVERSION")
					if err == nil {
						m.Mode = "affected"
						m.EmptyOK = len(selected) == 0
						m.Module = g.Module
						m.Census = g.Census
						m.Packages = map[string][]string{}
						sel := newSet(selected)
						for target, names := range g.Census {
							out := []string{}
							for _, name := range names {
								if sel.has(name) {
									out = append(out, name)
								}
							}
							m.Packages[target] = out
						}
						m.Matrix = policy.SelectedMatrix(m.Module, m.Packages)
						m.Reason = "changed packages and transitive reverse test/import dependents"
						if m.EmptyOK {
							m.Reason = "only verified ordinary documentation or no changed files; explicit empty package selection"
						}
					}
				}
			}
		}
		if err != nil {
			m.Reason = err.Error()
		}
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	m.SHA256 = m.Seal()
	return m, nil
}
func (p *Planner) Validate(ctx context.Context, m Manifest, target string, packages []string) ([]string, error) {
	policy, err := LoadPolicy()
	if err != nil {
		return nil, err
	}
	if m.SHA256 != m.Seal() {
		return nil, fmt.Errorf("test selection integrity check failed")
	}
	if m.Schema != 1 || (m.Mode != "full" && m.Mode != "affected") || m.Reason == "" {
		return nil, fmt.Errorf("invalid test selection manifest")
	}
	if !slices.Equal(m.Targets, policy.Targets) {
		return nil, fmt.Errorf("selection policy does not match checkout")
	}
	head, err := p.exec(ctx, p.Env, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if head != m.Head {
		return nil, fmt.Errorf("stale test selection revision")
	}
	if m.Mode == "full" {
		if m.EmptyOK || len(m.Packages) != 0 || len(m.Census) != 0 || m.Module != "" || m.Toolchain != "" || !slices.Equal(m.Matrix.Include, policy.FullMatrix().Include) {
			return nil, fmt.Errorf("incomplete full-suite manifest")
		}
		return nil, nil
	}
	var native *Target
	for i := range policy.Targets {
		if policy.Targets[i].Runner == target {
			native = &policy.Targets[i]
		}
	}
	if native == nil {
		return nil, fmt.Errorf("unknown selection target")
	}
	version, err := p.exec(ctx, p.Env, "go", "env", "GOVERSION")
	if err != nil {
		return nil, err
	}
	if version != m.Toolchain {
		return nil, fmt.Errorf("native Go toolchain does not match selection")
	}
	module, err := p.exec(ctx, p.Env, "go", "list", "-m")
	if err != nil {
		return nil, err
	}
	if module != m.Module {
		return nil, fmt.Errorf("module identity does not match selection")
	}
	actual, err := p.exec(ctx, p.Env, "go", "env", "GOOS", "GOARCH", "CGO_ENABLED")
	if err != nil {
		return nil, err
	}
	if actual != native.GOOS+"\n"+native.GOARCH+"\n1" || envValue(p.Env, "GOFLAGS") != "" || envValue(baseEnv(p.Env), "GOEXPERIMENT") != "" {
		return nil, fmt.Errorf("native build context changed; use CI_FULL_TESTS=true to run full")
	}
	if len(m.Census) != len(policy.Targets) || len(m.Packages) != len(policy.Targets) {
		return nil, fmt.Errorf("incomplete target census")
	}
	hasSelected := false
	for _, t := range policy.Targets {
		names, ok := m.Census[t.Runner]
		sel, selOK := m.Packages[t.Runner]
		if !ok || !selOK || sel == nil || len(names) == 0 || !slices.Equal(names, newSet(names).sorted()) || !slices.Equal(sel, newSet(sel).sorted()) {
			return nil, fmt.Errorf("invalid selected package census")
		}
		all := newSet(names)
		hasSelected = hasSelected || len(sel) > 0
		for _, name := range sel {
			if !all.has(name) {
				return nil, fmt.Errorf("selected package outside census")
			}
		}
	}
	if m.EmptyOK == hasSelected {
		return nil, fmt.Errorf("explicit empty selection disagrees with selected packages")
	}
	actualPackages := append([]string{}, packages...)
	slices.Sort(actualPackages)
	if !slices.Equal(actualPackages, m.Census[target]) {
		return nil, fmt.Errorf("native package census changed; refusing incomplete selection")
	}
	if !reflect.DeepEqual(m.Matrix, policy.SelectedMatrix(m.Module, m.Packages)) {
		return nil, fmt.Errorf("selected matrix does not cover selected packages")
	}
	return append([]string{}, m.Packages[target]...), nil
}
func (m Manifest) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Test selection\n\nMode: **%s**\n\n%s\n\nTest jobs: **%d / 12**\n\n", m.Mode, m.Reason, len(m.Matrix.Include))
	keys := make([]string, 0, len(m.Packages))
	for key := range m.Packages {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, target := range keys {
		names := m.Packages[target]
		fmt.Fprintf(&b, "### %s: %d / %d packages\n\n", target, len(names), len(m.Census[target]))
		if len(names) == 0 {
			b.WriteString("- No selected packages.\n")
		}
		for _, name := range names {
			fmt.Fprintf(&b, "- `%s`\n", name)
		}
		b.WriteByte('\n')
	}
	b.WriteString("Full lint, builds and security checks remain enabled.\nAffected-run coverage is partial and is not uploaded as whole-repository coverage.\n")
	return b.String()
}
