// Package nuxtimports reads Nuxt's generated app auto-import bindings.
package nuxtimports

import (
	"context"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/parser/tsalias"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// Binding names the repository-relative module path and its original export.
type Binding struct {
	File string
	Name string
}

type scope struct {
	Root     string
	Bindings map[string]Binding
}

// Collection keeps generated lists scoped to their Nuxt project roots.
type Collection struct {
	scopes []scope
}

var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

const GeneratedList = ".nuxt/imports.d.ts"

var configFiles = [...]string{"nuxt.config.ts", "nuxt.config.js", "nuxt.config.mts", "nuxt.config.mjs", "nuxt.config.cts", "nuxt.config.cjs"}

func ConfigFileNames() [len(configFiles)]string { return configFiles }

func WorkingTree(root string) tsalias.Tree {
	return tsalias.WorkingTree(root, GeneratedList, configFiles[:]...)
}

// WorkingTreeVisits reports whether the walk of WorkingTree visits rel.
func WorkingTreeVisits(rel string) bool {
	return tsalias.WorkingTreeVisits(rel, GeneratedList, configFiles[:]...)
}

// Load ignores unprepared projects, packages and files outside the repository.
func Load(repoRoot string) *Collection {
	return LoadTree(WorkingTree(repoRoot))
}

// LoadTree reads only generated lists present in the snapshot, never the checkout.
func LoadTree(tree tsalias.Tree) *Collection {
	if tree == nil {
		return nil
	}
	roots := make(map[string]map[string]Binding)
	tree.Files(func(rel string) {
		dir := path.Dir(rel)
		generated := rel == GeneratedList || strings.HasSuffix(rel, "/"+GeneratedList)
		root := dir
		if generated {
			root = path.Dir(dir)
		} else if !slices.Contains(configFiles[:], path.Base(rel)) {
			return
		}
		for ancestor := root; ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			if tsalias.SkipDirName(path.Base(ancestor)) {
				return
			}
		}
		data, ok := tree.Read(rel)
		if generated && !ok {
			return
		}
		if root == "." {
			root = ""
		}
		if generated {
			roots[root] = parse(".", dir, string(data))
		} else if _, exists := roots[root]; !exists && path.Base(path.Dir(root)) != "layers" {
			roots[root] = nil
		}
	})
	scopes := make([]scope, 0, len(roots))
	for root, bindings := range roots {
		scopes = append(scopes, scope{Root: root, Bindings: bindings})
	}
	return collection(scopes)
}

func collection(scopes []scope) *Collection {
	if len(scopes) == 0 {
		return nil
	}
	sort.Slice(scopes, func(i, j int) bool {
		if len(scopes[i].Root) != len(scopes[j].Root) {
			return len(scopes[i].Root) > len(scopes[j].Root)
		}
		return scopes[i].Root < scopes[j].Root
	})
	return &Collection{scopes: scopes}
}

func parse(repoRoot, generatedDir, source string) map[string]Binding {
	bindings := make(map[string]Binding)
	if !strings.Contains(source, "export") {
		return bindings
	}
	src := []byte(source)
	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(typescript.GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil || tree == nil {
		return bindings
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		return bindings
	}
	ambiguous := make(map[string]bool)
	for i, n := 0, int(root.NamedChildCount()); i < n; i++ {
		stmt := root.NamedChild(i)
		if stmt == nil {
			continue
		}
		// Stop on syntax errors so error recovery after an unclosed comment or
		// string cannot interpret trailing text as a real export.
		if stmt.HasError() {
			break
		}
		if stmt.Type() != "export_statement" || exportStatementTypeOnly(stmt, src) {
			continue
		}
		specifier := plainStringContent(stmt.ChildByFieldName("source"), src)
		if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
			continue
		}
		file := filepath.Join(generatedDir, filepath.FromSlash(specifier))
		rel, err := filepath.Rel(repoRoot, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		clause := findChild(stmt, "export_clause")
		if clause == nil {
			continue
		}
		for j, m := 0, int(clause.NamedChildCount()); j < m; j++ {
			sp := clause.NamedChild(j)
			if sp == nil || sp.Type() != "export_specifier" || sp.HasError() || specifierTypeOnly(sp, src) {
				continue
			}
			name, local := specifierNames(sp, src)
			if !identifierRE.MatchString(name) || !identifierRE.MatchString(local) {
				continue
			}
			binding := Binding{File: filepath.ToSlash(rel), Name: name}
			if previous, exists := bindings[local]; exists && previous != binding {
				ambiguous[local] = true
			}
			bindings[local] = binding
		}
	}
	for name := range ambiguous {
		delete(bindings, name)
	}
	return bindings
}

func findChild(node *sitter.Node, kind string) *sitter.Node {
	for i, n := 0, int(node.ChildCount()); i < n; i++ {
		if c := node.Child(i); c != nil && c.Type() == kind {
			return c
		}
	}
	return nil
}

func plainStringContent(node *sitter.Node, src []byte) string {
	if node == nil || node.Type() != "string" || node.NamedChildCount() != 1 {
		return ""
	}
	frag := node.NamedChild(0)
	if frag == nil || frag.Type() != "string_fragment" {
		return ""
	}
	return frag.Content(src)
}

func exportStatementTypeOnly(stmt *sitter.Node, src []byte) bool {
	for i, n := 0, int(stmt.ChildCount()); i < n; i++ {
		c := stmt.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "export_clause" {
			break
		}
		if c.Type() == "type" || (!c.IsNamed() && c.Content(src) == "type") {
			return true
		}
	}
	return false
}

func specifierTypeOnly(sp *sitter.Node, src []byte) bool {
	for i, n := 0, int(sp.ChildCount()); i < n; i++ {
		c := sp.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "type" || (!c.IsNamed() && c.Content(src) == "type") {
			return true
		}
	}
	return false
}

func specifierNames(sp *sitter.Node, src []byte) (name, local string) {
	nameNode := sp.ChildByFieldName("name")
	if nameNode == nil || nameNode.Type() != "identifier" {
		return "", ""
	}
	name = nameNode.Content(src)
	if aliasNode := sp.ChildByFieldName("alias"); aliasNode != nil {
		if aliasNode.Type() != "identifier" {
			return "", ""
		}
		return name, aliasNode.Content(src)
	}
	return name, name
}

// Resolve never applies app imports to Nitro server files or generated files.
func (c *Collection) Resolve(callerFile, name string) Binding {
	if c == nil {
		return Binding{}
	}
	callerFile = filepath.ToSlash(callerFile)
	for _, scope := range c.scopes {
		rel := callerFile
		if scope.Root != "" {
			if !strings.HasPrefix(callerFile, scope.Root+"/") {
				continue
			}
			rel = strings.TrimPrefix(callerFile, scope.Root+"/")
		}
		if slices.Contains(configFiles[:], path.Base(rel)) {
			return Binding{}
		}
		layer, isLayer := strings.CutPrefix(rel, "layers/")
		_, inLayer, _ := strings.Cut(layer, "/")
		if strings.HasPrefix(rel, "server/") || isLayer && strings.HasPrefix(inLayer, "server/") {
			return Binding{}
		}
		for part := range strings.SplitSeq(rel, "/") {
			if part == ".nuxt" {
				return Binding{}
			}
		}
		return scope.Bindings[name]
	}
	return Binding{}
}

// Equal compares the generated bindings, not their filesystem timestamps.
func (c *Collection) Equal(other *Collection) bool {
	if c == nil || other == nil {
		return c == other
	}
	if len(c.scopes) != len(other.scopes) {
		return false
	}
	for i, left := range c.scopes {
		right := other.scopes[i]
		if left.Root != right.Root || len(left.Bindings) != len(right.Bindings) {
			return false
		}
		for name, binding := range left.Bindings {
			if binding != right.Bindings[name] {
				return false
			}
		}
	}
	return true
}
