// Package nuxtimports reads Nuxt's generated app auto-import bindings.
package nuxtimports

import (
	"path"
	"path/filepath"
	"regexp"
	"slices"
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
type Collection struct {
	scopes       []scope
	localsByStem map[string][]string
}

var exportRE = regexp.MustCompile(`(?m)^\s*export\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
var identifierRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

const GeneratedList = ".nuxt/imports.d.ts"

var configFiles = [...]string{"nuxt.config.ts", "nuxt.config.js", "nuxt.config.mts", "nuxt.config.mjs", "nuxt.config.cts", "nuxt.config.cjs"}

func ConfigFileNames() [len(configFiles)]string { return configFiles }

func WorkingTree(root string) tsalias.Tree {
	return tsalias.WorkingTree(root, GeneratedList, configFiles[:]...)
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
		} else if _, exists := roots[root]; !exists {
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
	c := &Collection{scopes: scopes, localsByStem: make(map[string][]string)}
	for _, scope := range scopes {
		for local, binding := range scope.Bindings {
			stem := moduleStem(binding.File)
			c.localsByStem[stem] = append(c.localsByStem[stem], local)
		}
	}
	return c
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

// VisitLocals reports candidate caller spellings for the incoming stub frontier.
// Forward resolution still checks the exact extension, caller scope and export.
func (c *Collection) VisitLocals(file string, visit func(string)) {
	if c == nil {
		return
	}
	stem := moduleStem(file)
	for _, name := range c.localsByStem[stem] {
		visit(name)
	}
	if path.Base(stem) == "index" {
		for _, name := range c.localsByStem[path.Dir(stem)] {
			visit(name)
		}
	}
}

func moduleStem(file string) string {
	if strings.HasSuffix(file, ".d.ts") {
		return strings.TrimSuffix(file, ".d.ts")
	}
	switch ext := path.Ext(file); ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return strings.TrimSuffix(file, ext)
	}
	return file
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
