package search

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// projectNameMinLen is the shortest project name that earns de-weighting. A
// short name (≤4 chars) is too likely to be a real, discriminating query word
// (e.g. "auth", "http") to suppress; only a longer name — which a developer
// rarely types as a deliberate search term — is treated as the ambient repo
// noise that appears in every file path.
const projectNameMinLen = 5

// DetectProjectName returns the project's name from (in order) the go.mod module
// path's last segment, the package.json "name" field, or the repo directory's
// base name — but only when it is at least projectNameMinLen runes. Returns ""
// when no name qualifies, which disables de-weighting.
func DetectProjectName(root string) string {
	return DetectProjectNameWith(root, os.ReadFile)
}

// DetectProjectNameWith is DetectProjectName reading the root manifests through
// readFile, so a caller that accounts for every working-copy read it makes
// sees these too.
func DetectProjectNameWith(root string, readFile func(string) ([]byte, error)) string {
	if name := projectNameFromGoMod(root, readFile); name != "" {
		return qualifyProjectName(name)
	}
	if name := projectNameFromPackageJSON(root, readFile); name != "" {
		return qualifyProjectName(name)
	}
	return qualifyProjectName(filepath.Base(root))
}

func qualifyProjectName(name string) string {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < projectNameMinLen {
		return ""
	}
	return name
}

func projectNameFromGoMod(root string, readFile func(string) ([]byte, error)) string {
	data, err := readFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			mod := strings.TrimSpace(rest)
			if i := strings.LastIndexByte(mod, '/'); i >= 0 {
				mod = mod[i+1:]
			}
			return mod
		}
	}
	return ""
}

func projectNameFromPackageJSON(root string, readFile func(string) ([]byte, error)) string {
	data, err := readFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	name := pkg.Name
	// A scoped package (`@acme/widgets`) de-weights on the bare package name.
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// StripProjectNameFromPath removes path segments equal to the project name from
// a file path BEFORE it is tokenized into the BM25 path field. The project
// name is the repo-root directory, so it appears in every indexed path — a
// query that happens to contain it would otherwise match every document's path
// field and contribute a useless uniform boost. Dropping it from the indexed
// (not the stored) path lets the de-weighting compose with the rest of the
// rerank instead of flattening it. A name below the length floor, or a path
// that doesn't contain it, is returned unchanged.
func StripProjectNameFromPath(filePath, projectName string) string {
	if projectName == "" || len([]rune(projectName)) < projectNameMinLen {
		return filePath
	}
	if !strings.Contains(filePath, projectName) {
		return filePath
	}
	segs := strings.Split(filePath, "/")
	out := segs[:0]
	for _, s := range segs {
		if strings.EqualFold(s, projectName) {
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, "/")
}
