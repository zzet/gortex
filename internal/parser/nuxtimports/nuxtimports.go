// Package nuxtimports reads Nuxt's generated app auto-import bindings.
package nuxtimports

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Binding names the repository-relative source stem and its original export.
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

// Load ignores unprepared projects, packages and files outside the repository.
func Load(repoRoot string) *Collection {
	var scopes []scope
	_ = filepath.WalkDir(repoRoot, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		switch entry.Name() {
		case "node_modules", ".git", ".hg", ".svn", "vendor", "build", "dist", "target", ".next":
			return filepath.SkipDir
		case ".nuxt":
			data, readErr := os.ReadFile(filepath.Join(p, "imports.d.ts"))
			if readErr != nil {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(repoRoot, filepath.Dir(p))
			if relErr != nil {
				return filepath.SkipDir
			}
			if rel == "." {
				rel = ""
			}
			bindings := parse(repoRoot, p, string(data))
			scopes = append(scopes, scope{Root: filepath.ToSlash(rel), Bindings: bindings})
			return filepath.SkipDir
		}
		return nil
	})
	if len(scopes) == 0 {
		return nil
	}
	sort.Slice(scopes, func(i, j int) bool { return len(scopes[i].Root) > len(scopes[j].Root) })
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
		if rel == "server" || strings.HasPrefix(rel, "server/") || strings.HasPrefix(rel, ".nuxt/") {
			return Binding{}
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

// Stem allows extension and index probing through the ordinary JS/TS resolver.
func (b Binding) Stem() string {
	switch path.Ext(b.File) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return strings.TrimSuffix(b.File, path.Ext(b.File))
	}
	return b.File
}
