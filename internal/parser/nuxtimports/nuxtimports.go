// Package nuxtimports reads Nuxt's generated app auto-import bindings.
package nuxtimports

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/parser/tsalias"
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
type Collection struct{ scopes []scope }

var exportRE = regexp.MustCompile(`(?m)^\s*export\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

const GeneratedList = ".nuxt/imports.d.ts"

// Load ignores unprepared projects, packages and files outside the repository.
func Load(repoRoot string) *Collection {
	return LoadTree(tsalias.WorkingTree(repoRoot, GeneratedList))
}

// LoadTree reads only generated lists present in the snapshot, never the checkout.
func LoadTree(tree tsalias.Tree) *Collection {
	if tree == nil {
		return nil
	}
	var scopes []scope
	tree.Files(func(rel string) {
		dir := path.Dir(rel)
		if rel != GeneratedList && !strings.HasSuffix(rel, "/"+GeneratedList) {
			return
		}
		root := path.Dir(dir)
		for ancestor := root; ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			if tsalias.SkipDirName(path.Base(ancestor)) {
				return
			}
		}
		data, ok := tree.Read(rel)
		if !ok {
			return
		}
		if root == "." {
			root = ""
		}
		scopes = append(scopes, scope{Root: root, Bindings: parse(".", dir, string(data))})
	})
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
	ambiguous := make(map[string]bool)
	for _, match := range exportRE.FindAllStringSubmatch(source, -1) {
		specifier := match[2]
		if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
			continue
		}
		file := filepath.Join(generatedDir, filepath.FromSlash(specifier))
		rel, err := filepath.Rel(repoRoot, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, item := range strings.Split(match[1], ",") {
			words := strings.Fields(item)
			if len(words) != 1 && (len(words) != 3 || words[1] != "as") {
				continue
			}
			name, local := words[0], words[len(words)-1]
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
		base := path.Base(rel)
		if strings.TrimSuffix(base, path.Ext(base)) == "nuxt.config" {
			return Binding{}
		}
		for part := range strings.SplitSeq(rel, "/") {
			if part == "server" || part == ".nuxt" {
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
