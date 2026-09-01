// Package modules parses dependency-manifest files and emits
// KindModule nodes plus EdgeDependsOnModule edges so agents can
// answer "what external packages does this repo depend on" or
// "which files import lodash@4" with a single graph query.
//
// Scope (v1): Go's go.mod. Other ecosystems (package.json, pnpm-
// lock, requirements.txt, Cargo.toml, etc.) are tracked as future
// follow-ups; the scanner's API is shaped so they can land
// alongside without changing the call sites.
package modules

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/graph"
)

// Spec is one parsed dependency entry from a manifest file.
// Indirect is true for entries marked `// indirect` in go.mod —
// agents may want to scope queries to direct deps only, so the
// flag rides along on the graph node's meta.
type Spec struct {
	Ecosystem string // "go", "npm", "pypi", … — for v1 always "go"
	Path      string // module path / package name
	Version   string // version string, "" for unpinned
	Indirect  bool
	Replace   string // replacement path when go.mod has a `replace` directive, "" otherwise
	Line      int    // 1-based line in the manifest where the spec was found
	// Alias holds the real package name when an npm dependency is
	// declared as an alias — `"shared": "npm:@acme/shared-lib@1.4.0"`
	// makes Path="shared", Version the verbatim `npm:` string, and
	// Alias="@acme/shared-lib". Empty for every non-aliased entry.
	Alias string
}

// ParseGoMod walks go.mod source and returns one Spec per
// dependency. Handles three shapes:
//
//	require github.com/foo/bar v1.0.0          // single-line
//	require ( ... )                            // grouped block
//	replace github.com/foo/bar => ./local/bar  // local replacements
//
// `// indirect` markers attach to the relevant Spec. Comments and
// blank lines are skipped. Errors silently produce a partial Spec
// list — the indexer treats malformed go.mod as best-effort, not
// fatal.
func ParseGoMod(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var specs []Spec
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inRequire := false
	inReplace := false
	lineNum := 0
	replaces := map[string]string{}
	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// Block markers.
		switch {
		case strings.HasPrefix(line, "require ("):
			inRequire = true
			continue
		case strings.HasPrefix(line, "replace ("):
			inReplace = true
			continue
		case line == ")":
			inRequire = false
			inReplace = false
			continue
		}
		// Replace directives — collect first so we can stamp the
		// replacement onto the require Spec produced from the same
		// module path. Single-line and block forms both supported.
		if strings.HasPrefix(line, "replace ") || inReplace {
			from, to := parseReplace(line)
			if from != "" {
				replaces[from] = to
			}
			continue
		}
		// Require directives.
		var modulePath, version string
		var directiveLine = lineNum
		switch {
		case strings.HasPrefix(line, "require ") && !strings.Contains(line, "("):
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				modulePath = parts[1]
				version = parts[2]
			}
		case inRequire:
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.Contains(parts[0], "/") {
				modulePath = parts[0]
				version = parts[1]
			}
		}
		if modulePath == "" {
			continue
		}
		spec := Spec{
			Ecosystem: "go",
			Path:      modulePath,
			Version:   version,
			Indirect:  strings.Contains(raw, "// indirect"),
			Line:      directiveLine,
		}
		specs = append(specs, spec)
	}
	// Stamp replace targets onto the matching require Spec — done
	// after the full pass so block-form replaces declared after
	// requires still attach correctly.
	for i := range specs {
		if to, ok := replaces[specs[i].Path]; ok {
			specs[i].Replace = to
		}
	}
	return specs
}

// ParsePackageJSON walks an npm-style package.json file's
// dependency blocks (`dependencies`, `devDependencies`,
// `peerDependencies`, `optionalDependencies`) and returns one
// Spec per declared package. The Indirect flag is repurposed for
// devDependencies/peerDependencies/optionalDependencies — they
// aren't "indirect" in the same Go-module sense, but the flag
// lets agents scope to "production-only" deps without a separate
// axis. The exact source block lives on Replace as a tag string
// (not the most semantically clean home, but it avoids growing
// the Spec struct for an ecosystem-specific field) — production
// deps get an empty Replace.
//
// Version strings are kept verbatim — npm semver ranges
// (`^1.2.0`, `~3.4.1`, `>=2 <3`) are not normalised, since
// resolved-versions belong in the lockfile, not package.json.
// A future package-lock.json / pnpm-lock.yaml extractor will
// supersede the version string with the resolved one.
func ParsePackageJSON(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	var specs []Spec
	specs = append(specs, packageJSONBlock(manifest.Dependencies, "")...)
	specs = append(specs, packageJSONBlock(manifest.DevDependencies, "dev")...)
	specs = append(specs, packageJSONBlock(manifest.PeerDependencies, "peer")...)
	specs = append(specs, packageJSONBlock(manifest.OptionalDependencies, "optional")...)
	return specs
}

// ParseNpmAlias extracts the real package name from an npm-alias
// dependency value. npm lets a dependency be re-pointed at a
// different package by giving its version range the `npm:` prefix:
//
//	"shared":  "npm:@acme/shared-lib@1.4.0" → "@acme/shared-lib"
//	"lodash4": "npm:lodash@4.17.21"         → "lodash"
//	"left":    "npm:left-pad"               → "left-pad"  (no version)
//
// The real name may itself be `@scope/name` (one slash) or a plain
// `name`. ok is false for any value without the `npm:` prefix or
// with an empty real name.
func ParseNpmAlias(value string) (realName string, ok bool) {
	const prefix = "npm:"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	rest := value[len(prefix):]
	if rest == "" {
		return "", false
	}
	// Strip the trailing `@version`. A leading `@` belongs to the
	// scope (`@acme/shared-lib`), so search for the version `@` from
	// the second byte onward — index 0 is never a version separator.
	name := rest
	if at := strings.Index(rest[1:], "@"); at >= 0 {
		name = rest[:at+1]
	}
	if name == "" {
		return "", false
	}
	return name, true
}

func packageJSONBlock(deps map[string]string, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, version := range deps {
		spec := Spec{
			Ecosystem: "npm",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind, // dev/peer/optional, "" for production
		}
		if alias, ok := ParseNpmAlias(version); ok {
			spec.Alias = alias
		}
		out = append(out, spec)
	}
	// Stable order per block — JSON map iteration is randomised, and
	// downstream consumers (BuildGraphArtifacts dedup, prefix-sort
	// in LinkImports) tolerate any order, but tests want
	// determinism.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// ParsePackageLockJSON walks an npm v2/v3 package-lock.json
// manifest's `packages` map and returns one Spec per declared
// package with its resolved version. The lockfile carries the
// concrete versions npm picked at install time, which is far
// more useful for "is lodash@4 still in use" queries than the
// semver ranges in package.json.
//
// v1 lockfiles (with a top-level `dependencies` map and no
// `packages` entry) are not handled — they're rare in the
// modern npm ecosystem and a parallel parser would mostly
// duplicate this logic. The lockfileVersion field on the
// manifest tells the caller which shape they have; the v1
// shape can land as another switch case alongside this when
// prioritised.
//
// The empty-string key in `packages` represents the root
// project itself — skipped, the same way ParsePackageJSON
// doesn't emit a self-reference. Production / dev classification
// follows the same Replace-as-tag convention as the rest:
// dev=true entries get Replace="dev"; everything else lands as
// production.
func ParsePackageLockJSON(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version  string `json:"version"`
			Dev      bool   `json:"dev"`
			Optional bool   `json:"optional"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	if manifest.LockfileVersion < 2 {
		// v1 lockfiles use a different shape we don't handle yet.
		return nil
	}
	specs := make([]Spec, 0, len(manifest.Packages))
	for key, entry := range manifest.Packages {
		if key == "" {
			// The root project's own manifest entry — skip.
			continue
		}
		// Keys are `node_modules/<name>` (or
		// `node_modules/<scope>/<name>` for scoped, or nested
		// `node_modules/foo/node_modules/bar` for transitive).
		// We strip the leading `node_modules/` prefix and use the
		// remainder verbatim as the package name. Nested paths
		// preserve their full chain, which keeps duplicate-name
		// entries at different versions distinguishable.
		name := strings.TrimPrefix(key, "node_modules/")
		if name == "" || entry.Version == "" {
			continue
		}
		spec := Spec{
			Ecosystem: "npm",
			Path:      name,
			Version:   entry.Version,
		}
		switch {
		case entry.Dev:
			spec.Indirect = true
			spec.Replace = "dev"
		case entry.Optional:
			spec.Indirect = true
			spec.Replace = "optional"
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Path < specs[j].Path
	})
	return specs
}

// ParseYarnLock walks a yarn classic (v1) lockfile and returns
// one Spec per resolved dependency. Format is yarn's bespoke
// pseudo-yaml (key blocks separated by blank lines, each starting
// with one or more `<name>@<range>` keys followed by `version
// "X.Y.Z"`). The parser is line-oriented and intentionally
// minimal — it picks up the resolved version per dependency
// block and ignores the rest (resolved/integrity/dependencies
// fields aren't needed for graph identity).
//
// Berry (yarn 2+) lockfiles use real YAML and a different shape;
// they're recognised as a separate dispatch row when needed.
func ParseYarnLock(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var (
		specs        []Spec
		pendingNames []string
		seen         = make(map[string]struct{})
	)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			pendingNames = nil
			continue
		}
		// Block header: a line that ends with `:` and contains
		// one or more `<name>@<range>` keys joined by `, `.
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
			pendingNames = parseYarnHeaderNames(strings.TrimSuffix(line, ":"))
			continue
		}
		// Inside a block — pick up the version line.
		if strings.HasPrefix(line, "version ") {
			version := strings.Trim(strings.TrimPrefix(line, "version "), `"`)
			if version == "" || len(pendingNames) == 0 {
				continue
			}
			for _, name := range pendingNames {
				key := name + "@" + version
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				specs = append(specs, Spec{
					Ecosystem: "npm",
					Path:      name,
					Version:   version,
				})
			}
			pendingNames = nil
		}
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Path != specs[j].Path {
			return specs[i].Path < specs[j].Path
		}
		return specs[i].Version < specs[j].Version
	})
	return specs
}

// parseYarnHeaderNames extracts the package names from a yarn-
// classic lockfile block header. Each header consists of one or
// more "<name>@<range>" entries separated by `, `. The names can
// be quoted ("@scope/pkg@^1.0.0") or bare. We strip the @range
// suffix and de-dup so a header like `lodash@^4.0.0, lodash@^4.17.0`
// yields just one "lodash" name.
func parseYarnHeaderNames(header string) []string {
	parts := strings.Split(header, ", ")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p == "" {
			continue
		}
		// Find the @ that introduces the version range, NOT the
		// scope @ at position 0. Scoped packages start with @ —
		// we want the second @, or the first when the package
		// is unscoped.
		var atIdx int
		if strings.HasPrefix(p, "@") {
			// scoped: skip past the leading @ then find the next
			// one.
			atIdx = strings.Index(p[1:], "@")
			if atIdx >= 0 {
				atIdx++
			}
		} else {
			atIdx = strings.Index(p, "@")
		}
		if atIdx < 0 {
			continue
		}
		name := p[:atIdx]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// ParsePnpmLock walks a pnpm v6+ lockfile (real YAML, top-level
// `packages` map keyed by `/<name>@<version>` paths). Each entry
// resolves to one Spec with the canonical name+version.
func ParsePnpmLock(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	// pnpm-lock.yaml has the shape:
	//   packages:
	//     /react@18.2.0:
	//       resolution: {...}
	//     /lodash@4.17.21:
	//       ...
	// We only need the keys; the per-package metadata is large
	// (integrity hashes, transitive deps) and not needed for
	// graph identity. Parsing keys via line-oriented scan stays
	// independent of the YAML library's behaviour around large
	// nested structures.
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var (
		specs      []Spec
		inPackages bool
		seen       = make(map[string]struct{})
	)
	for scanner.Scan() {
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// The top-level `packages:` key marks where the per-pkg
		// blocks live. A subsequent unindented top-level key
		// ends the section.
		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
			inPackages = trimmed == "packages:"
			continue
		}
		if !inPackages {
			continue
		}
		// Per-pkg block headers look like `  /<name>@<version>:`
		// at exactly two-space indent. Sub-keys are deeper.
		if !strings.HasPrefix(raw, "  /") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "/"), ":")
		key = strings.TrimSuffix(key, "(") // strip peer-suffix start when present
		// The version is the last `@` segment, accounting for
		// scoped packages (which start with `@scope/`).
		var atIdx int
		if strings.HasPrefix(key, "@") {
			atIdx = strings.LastIndex(key, "@")
		} else {
			atIdx = strings.Index(key, "@")
		}
		if atIdx <= 0 {
			continue
		}
		name := key[:atIdx]
		version := key[atIdx+1:]
		// Trim peer-dep suffix (`@types/node@20.11.5_typescript@5.3.3`).
		if i := strings.Index(version, "_"); i >= 0 {
			version = version[:i]
		}
		if name == "" || version == "" {
			continue
		}
		dedupKey := name + "@" + version
		if _, ok := seen[dedupKey]; ok {
			continue
		}
		seen[dedupKey] = struct{}{}
		specs = append(specs, Spec{
			Ecosystem: "npm",
			Path:      name,
			Version:   version,
		})
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Path != specs[j].Path {
			return specs[i].Path < specs[j].Path
		}
		return specs[i].Version < specs[j].Version
	})
	return specs
}

// ParsePyProject walks a Python pyproject.toml file's dependency
// declarations and returns one Spec per declared package. Both
// PEP 621 (`[project] dependencies = ["pkg>=1.0", ...]`) and
// Poetry (`[tool.poetry.dependencies] pkg = "^1.0"`) shapes are
// recognised; optional-dependency groups (`[project.optional-
// dependencies]`) are surfaced with the group name in Replace
// (analogous to how npm dev/peer/optional reuse the field).
//
// Version constraints are kept verbatim — PEP 440 specifiers and
// Poetry's caret/tilde ranges aren't normalised; resolved
// versions live in the lockfile (poetry.lock / uv.lock /
// requirements.txt) which a future extractor will supersede.
func ParsePyProject(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Project struct {
			Dependencies         []string            `toml:"dependencies"`
			OptionalDependencies map[string][]string `toml:"optional-dependencies"`
		} `toml:"project"`
		Tool struct {
			Poetry struct {
				Dependencies    map[string]any `toml:"dependencies"`
				DevDependencies map[string]any `toml:"dev-dependencies"`
			} `toml:"poetry"`
		} `toml:"tool"`
	}
	if err := toml.Unmarshal(source, &manifest); err != nil {
		return nil
	}

	var specs []Spec
	// PEP 621 dependencies: list of `name<spec>` strings.
	for _, dep := range manifest.Project.Dependencies {
		name, version := splitPEP508(dep)
		if name == "" {
			continue
		}
		specs = append(specs, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
		})
	}
	// PEP 621 optional groups: each group name lands on Replace so
	// agents can scope by group later. Sorted within each group for
	// determinism.
	groupNames := make([]string, 0, len(manifest.Project.OptionalDependencies))
	for g := range manifest.Project.OptionalDependencies {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		entries := manifest.Project.OptionalDependencies[group]
		groupSpecs := make([]Spec, 0, len(entries))
		for _, dep := range entries {
			name, version := splitPEP508(dep)
			if name == "" {
				continue
			}
			groupSpecs = append(groupSpecs, Spec{
				Ecosystem: "pypi",
				Path:      name,
				Version:   version,
				Indirect:  true,
				Replace:   group,
			})
		}
		sort.Slice(groupSpecs, func(i, j int) bool {
			return groupSpecs[i].Path < groupSpecs[j].Path
		})
		specs = append(specs, groupSpecs...)
	}
	// Poetry shape: name → spec (string or table). When a table is
	// given we take its `version` field, when a bare string we keep
	// it verbatim.
	specs = append(specs, poetryBlock(manifest.Tool.Poetry.Dependencies, "")...)
	specs = append(specs, poetryBlock(manifest.Tool.Poetry.DevDependencies, "dev")...)
	return specs
}

// poetryBlock converts a Poetry `[tool.poetry.*-dependencies]`
// table into Spec entries. Values can be either bare version
// strings or tables containing a `version` key (for
// extras-bearing entries like `requests = { version = "^2.0",
// extras = ["socks"] }`).
func poetryBlock(deps map[string]any, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, raw := range deps {
		if name == "python" {
			// `python = "^3.10"` is a Python interpreter
			// constraint, not a dependency. Skip.
			continue
		}
		var version string
		switch v := raw.(type) {
		case string:
			version = v
		case map[string]any:
			if s, ok := v["version"].(string); ok {
				version = s
			}
		}
		out = append(out, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// pep508Re strips a PEP 508 dependency string into name + version
// constraint. Examples: `requests>=2.0` → ("requests", ">=2.0"),
// `flask[async]==2.0.0` → ("flask", "==2.0.0"), `numpy` → ("numpy", "").
// Environment markers (`pkg; python_version<'3.9'`) and URL
// installs (`pkg @ https://...`) are dropped — they encode
// install-context, not the dependency identity our graph cares
// about.
var pep508Re = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)`)

func splitPEP508(dep string) (name, version string) {
	dep = strings.TrimSpace(dep)
	// Drop environment markers and URL specs at the first ';' / ' @ '.
	if idx := strings.Index(dep, ";"); idx >= 0 {
		dep = strings.TrimSpace(dep[:idx])
	}
	if idx := strings.Index(dep, " @ "); idx >= 0 {
		dep = strings.TrimSpace(dep[:idx])
	}
	// Drop extras (the `[group]` suffix).
	if idx := strings.Index(dep, "["); idx >= 0 {
		end := strings.Index(dep, "]")
		if end < 0 {
			return "", ""
		}
		dep = strings.TrimSpace(dep[:idx]) + strings.TrimSpace(dep[end+1:])
	}
	m := pep508Re.FindString(dep)
	if m == "" {
		return "", ""
	}
	name = m
	version = strings.TrimSpace(dep[len(m):])
	return name, version
}

// ParseRequirementsTxt walks a pip requirements file and returns
// one Spec per non-comment, non-blank entry. Each line is parsed
// like a PEP 508 dependency; `-r other.txt` and `-e .` lines are
// ignored — they're install-mode directives, not dependency
// declarations.
//
// A future enhancement could chase `-r` includes recursively, but
// the first-order coverage from a single file is the
// 90th-percentile use case.
func ParseRequirementsTxt(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var specs []Spec
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Drop trailing inline comment.
		if i := strings.Index(line, "#"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "-") {
			// `-r`, `-e`, `--index-url`, etc. — skip.
			continue
		}
		name, version := splitPEP508(line)
		if name == "" {
			continue
		}
		specs = append(specs, Spec{
			Ecosystem: "pypi",
			Path:      name,
			Version:   version,
		})
	}
	return specs
}

// ParseCargoToml walks a Rust Cargo.toml manifest's dependency
// tables and returns one Spec per declared crate. Three blocks
// are recognised: [dependencies] for production deps,
// [dev-dependencies] (and [dev_dependencies]) for test-only
// deps, and [build-dependencies] for build-script deps. The
// latter two stamp Replace = "dev" / "build" and Indirect = true,
// matching the npm/pyproject convention.
//
// Each entry's value can be either a bare version string ("1.0",
// "^1.0", "~1.0", "=1.0") or a table such as
// `serde = { version = "1.0", features = [...] }`. Git/path/
// registry-only deps without a version field are skipped — they
// resolve to a workspace-internal source rather than a versioned
// crates.io release, and the graph cares about external versioned
// identity.
func ParseCargoToml(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Dependencies      map[string]any `toml:"dependencies"`
		DevDependencies   map[string]any `toml:"dev-dependencies"`
		DevDependenciesU  map[string]any `toml:"dev_dependencies"`
		BuildDependencies map[string]any `toml:"build-dependencies"`
	}
	if err := toml.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	var specs []Spec
	specs = append(specs, cargoBlock(manifest.Dependencies, "")...)
	// Cargo's canonical key is `dev-dependencies`, but the
	// underscore form is occasionally seen in older manifests.
	// Both fold into a single Replace="dev" set.
	specs = append(specs, cargoBlock(manifest.DevDependencies, "dev")...)
	specs = append(specs, cargoBlock(manifest.DevDependenciesU, "dev")...)
	specs = append(specs, cargoBlock(manifest.BuildDependencies, "build")...)
	return specs
}

func cargoBlock(deps map[string]any, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, raw := range deps {
		var version string
		switch v := raw.(type) {
		case string:
			version = v
		case map[string]any:
			if s, ok := v["version"].(string); ok {
				version = s
			}
			// Git/path deps without a version field resolve to
			// repo-local sources; skip them.
			if version == "" {
				continue
			}
		default:
			continue
		}
		out = append(out, Spec{
			Ecosystem: "cargo",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// ParsePomXML walks a Maven pom.xml manifest and returns one Spec
// per <dependency> entry. Maven coordinates take the form
// `<groupId>:<artifactId>` for the dependency name with the
// version a separate child element. The scope attribute
// (compile/test/provided/runtime/system/import) maps to Spec
// fields the same way npm dev/peer/optional do — `compile` is
// the implicit production scope and lands without a Replace
// tag; everything else stamps Replace = "<scope>" and
// Indirect = true so cleanup queries can scope to production-only
// deps.
//
// Maven property substitution (`${project.version}`,
// `${spring.version}` etc.) is left verbatim in the version
// string. A future enhancement could resolve properties from
// <properties> and parent inheritance; the v1 keeps it simple
// since the canonical source-of-truth for resolved versions is
// the lockfile or `mvn dependency:tree` output rather than
// pom.xml itself.
func ParsePomXML(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Dependencies struct {
			Dependency []struct {
				GroupID    string `xml:"groupId"`
				ArtifactID string `xml:"artifactId"`
				Version    string `xml:"version"`
				Scope      string `xml:"scope"`
			} `xml:"dependency"`
		} `xml:"dependencies"`
	}
	if err := xml.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	specs := make([]Spec, 0, len(manifest.Dependencies.Dependency))
	for _, d := range manifest.Dependencies.Dependency {
		if d.GroupID == "" || d.ArtifactID == "" {
			continue
		}
		scope := strings.TrimSpace(d.Scope)
		isProduction := scope == "" || scope == "compile"
		spec := Spec{
			Ecosystem: "maven",
			Path:      d.GroupID + ":" + d.ArtifactID,
			Version:   strings.TrimSpace(d.Version),
		}
		if !isProduction {
			spec.Indirect = true
			spec.Replace = scope
		}
		specs = append(specs, spec)
	}
	// Stable order — XML parsing preserves source order, but the
	// downstream dedup + edge emission prefers alphabetical for
	// diff-able output across runs.
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Path < specs[j].Path
	})
	return specs
}

// parseReplace extracts the from/to module paths from a replace
// directive line. Returns ("", "") when the line doesn't have the
// expected `from [version] => to [version]` shape. Replace
// versions are dropped — they don't add graph signal beyond what
// the require directive already carries.
func parseReplace(line string) (from, to string) {
	line = strings.TrimPrefix(line, "replace ")
	idx := strings.Index(line, "=>")
	if idx < 0 {
		return "", ""
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+2:])
	if left == "" || right == "" {
		return "", ""
	}
	// Drop optional version on the from side (`module v1.x => target`).
	if parts := strings.Fields(left); len(parts) > 0 {
		left = parts[0]
	}
	if parts := strings.Fields(right); len(parts) > 0 {
		right = parts[0]
	}
	return left, right
}

// ModuleNodeID returns the canonical ID for a module node. The
// `module::` prefix is reserved for shared external-dependency
// nodes; the version is included so two repos that depend on
// `lodash@3` and `lodash@4` produce two distinct nodes that can be
// joined for "version skew" queries.
func ModuleNodeID(ecosystem, path, version string) string {
	id := "module::" + ecosystem + ":" + path
	if version != "" {
		id += "@" + version
	}
	return id
}

// BuildGraphArtifacts converts the parsed Spec list into
// (modules, edges) pairs. Modules are de-duplicated within the
// returned slice — graph.AddNode is idempotent on ID, so one node
// per (ecosystem, path, version) tuple is guaranteed even when the
// caller appends from multiple manifest files.
//
// filePath is the unprefixed manifest path (typically "go.mod"),
// preserved verbatim — the edge endpoint must match the synthetic
// manifest file node the indexer mints from the same spelling
// (OS-native separators on Windows) or the edge dangles.
// applyRepoPrefix downstream handles multi-repo namespacing for
// the file→module edge, but module IDs themselves do not get
// prefixed — the synthetic `module::` prefix matches the existing
// `external::` / `annotation::` convention the exporter already
// recognises.
func BuildGraphArtifacts(filePath string, specs []Spec) ([]*graph.Node, []*graph.Edge) {
	if len(specs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(specs))
	nodes := make([]*graph.Node, 0, len(specs))
	edges := make([]*graph.Edge, 0, len(specs))
	for _, s := range specs {
		id := ModuleNodeID(s.Ecosystem, s.Path, s.Version)
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			meta := map[string]any{
				"ecosystem": s.Ecosystem,
				"path":      s.Path,
				"version":   s.Version,
				"indirect":  s.Indirect,
			}
			if s.Replace != "" {
				meta["replace"] = s.Replace
			}
			nodes = append(nodes, &graph.Node{
				ID:       id,
				Kind:     graph.KindModule,
				Name:     shortName(s.Path),
				FilePath: filePath,
				Language: ecosystemLanguage(s.Ecosystem),
				Meta:     meta,
			})
		}
		edges = append(edges, &graph.Edge{
			From:     filePath,
			To:       id,
			Kind:     graph.EdgeDependsOnModule,
			FilePath: filePath,
			Line:     s.Line,
			Origin:   graph.OriginASTResolved,
		})
	}
	return nodes, edges
}

// LinkImports walks every KindImport node in the graph and emits
// an EdgeDependsOnModule edge to the matching module node from the
// given Spec list. Matching is by longest path prefix — an import
// of `github.com/foo/bar/sub` resolves to the spec for
// `github.com/foo/bar` when no exact match exists. Returns the
// number of edges emitted.
//
// Imports of repo-internal packages (the indexed module's own
// path) are deliberately skipped — they aren't external
// dependencies. Multi-version imports (Go's `module/v2` shape)
// match the longest spec; a manifest declaring both `bar` and
// `bar/v2` will resolve `import bar/v2/sub` to the v2 spec.
func LinkImports(g graph.Store, specs []Spec, ownModulePath string) int {
	if g == nil {
		return 0
	}
	return LinkImportsIn(g, g.AllNodes(), specs, ownModulePath)
}

// LinkImportsIn is LinkImports scoped to a caller-supplied import-node
// slice. Walking g.AllNodes() inside a warmup loop is O(R · N) — every
// repo's manifest pass touches every node in the whole graph. Callers
// in multi-repo mode should pass the repo's own KindImport nodes (e.g.
// from g.GetRepoNodes(repoPrefix) filtered by Kind) so each pass stays
// O(repo size).
func LinkImportsIn(g graph.Store, importNodes []*graph.Node, specs []Spec, ownModulePath string) int {
	if g == nil || len(specs) == 0 || len(importNodes) == 0 {
		return 0
	}
	// Index specs by path once. When two specs share a path (which
	// should not happen in a well-formed manifest), the first wins —
	// matching the previous sorted-slice implementation.
	specByPath := make(map[string]Spec, len(specs))
	for _, s := range specs {
		if _, ok := specByPath[s.Path]; ok {
			continue
		}
		specByPath[s.Path] = s
	}

	edges := make([]*graph.Edge, 0, len(importNodes))
	for _, n := range importNodes {
		if n.Kind != graph.KindImport {
			continue
		}
		importPath, _ := n.Meta["path"].(string)
		if importPath == "" {
			continue
		}
		if ownModulePath != "" && (importPath == ownModulePath ||
			strings.HasPrefix(importPath, ownModulePath+"/")) {
			continue
		}
		spec, ok, _ := matchLongestPrefix(importPath, specByPath)
		if !ok {
			continue
		}
		moduleID := ModuleNodeID(spec.Ecosystem, spec.Path, spec.Version)
		edges = append(edges, &graph.Edge{
			From:     n.ID,
			To:       moduleID,
			Kind:     graph.EdgeDependsOnModule,
			FilePath: n.FilePath,
			Line:     n.StartLine,
			Origin:   graph.OriginASTResolved,
		})
	}
	g.AddBatch(nil, edges)
	return len(edges)
}

// matchLongestPrefix returns the spec whose path is the longest exact
// path-segment prefix of importPath. It probes the exact import first,
// then removes one slash-delimited segment at a time. The work is thus
// bounded by import depth rather than the number of manifest entries.
// probes is returned so scale tests can enforce that invariant directly.
func matchLongestPrefix(importPath string, candidates map[string]Spec) (spec Spec, ok bool, probes int) {
	candidate := importPath
	for candidate != "" {
		probes++
		if spec, ok = candidates[candidate]; ok {
			return spec, true, probes
		}
		slash := strings.LastIndexByte(candidate, '/')
		if slash < 0 {
			break
		}
		candidate = candidate[:slash]
	}
	return Spec{}, false, probes
}

// ecosystemLanguage maps a dependency ecosystem to the source language its
// modules belong to. Module nodes used to hardcode "go", which fabricated Go
// presence in every repo whose manifest was merely a Cargo.toml or
// package-lock.json — the per-repo language census then admitted whole
// Go-compiler enrichment passes against repos with no Go source at all.
// Unknown ecosystems carry no language claim.
func ecosystemLanguage(ecosystem string) string {
	switch ecosystem {
	case "go":
		return "go"
	case "npm":
		return "javascript"
	case "pypi":
		return "python"
	case "cargo":
		return "rust"
	case "maven":
		return "java"
	case "composer":
		return "php"
	default:
		return ""
	}
}

// shortName returns the last meaningful segment of a module path —
// useful for the Name field surfaced by Brief listings. Strips the
// `vN` major-version suffix when present (`github.com/foo/bar/v2`
// → `bar`, not `v2`).
func shortName(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if isMajorVersionSegment(last) && len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return last
}

func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseComposerJSON reads a PHP composer.json into dependency Specs.
//
// composer.json's `require` block mixes real packages with PLATFORM
// constraints — `php`, `hhvm`, the `ext-*` PHP extensions, `lib-*` system
// libraries and the `composer-*` runtime pseudo-packages. Those name no
// installable package and would mint module nodes nothing can ever depend on,
// so they are skipped.
//
// `require-dev` follows the package.json convention: Indirect true with the
// block tagged on Replace, so an agent can scope to production dependencies
// without a second axis. Version constraints stay verbatim (`^3.0`, `~1.2.3`,
// `>=2 <3`) — composer.lock carries the resolved versions.
func ParseComposerJSON(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Require    map[string]string `json:"require"`
		RequireDev map[string]string `json:"require-dev"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	var specs []Spec
	specs = append(specs, composerBlock(manifest.Require, "")...)
	specs = append(specs, composerBlock(manifest.RequireDev, "dev")...)
	return specs
}

// ParseComposerLock reads the resolved package set from composer.lock. The
// lockfile supersedes the manifest's version ranges with exact versions, the
// same way package-lock.json supersedes package.json.
func ParseComposerLock(source []byte) []Spec {
	if len(source) == 0 {
		return nil
	}
	var lock struct {
		Packages    []composerLockEntry `json:"packages"`
		PackagesDev []composerLockEntry `json:"packages-dev"`
	}
	if err := json.Unmarshal(source, &lock); err != nil {
		return nil
	}
	var specs []Spec
	specs = append(specs, composerLockBlock(lock.Packages, "")...)
	specs = append(specs, composerLockBlock(lock.PackagesDev, "dev")...)
	return specs
}

type composerLockEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func composerLockBlock(entries []composerLockEntry, kind string) []Spec {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" || isComposerPlatformPackage(e.Name) {
			continue
		}
		out = append(out, Spec{
			Ecosystem: "composer",
			Path:      e.Name,
			Version:   e.Version,
			Indirect:  kind != "",
			Replace:   kind,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func composerBlock(deps map[string]string, kind string) []Spec {
	if len(deps) == 0 {
		return nil
	}
	out := make([]Spec, 0, len(deps))
	for name, version := range deps {
		if isComposerPlatformPackage(name) {
			continue
		}
		out = append(out, Spec{
			Ecosystem: "composer",
			Path:      name,
			Version:   version,
			Indirect:  kind != "",
			Replace:   kind,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// isComposerPlatformPackage reports whether a composer requirement names the
// runtime rather than an installable package.
func isComposerPlatformPackage(name string) bool {
	switch name {
	case "php", "php-64bit", "php-ipv6", "php-zts", "php-debug", "hhvm":
		return true
	}
	return strings.HasPrefix(name, "ext-") ||
		strings.HasPrefix(name, "lib-") ||
		strings.HasPrefix(name, "composer-") ||
		name == "composer"
}

// ComposerAutoloadRoots reads composer.json's PSR-4 and PSR-0 autoload maps
// into namespace-prefix → repo-relative-directory pairs, covering both the
// `autoload` and `autoload-dev` blocks. A prefix may map to several
// directories, and composer allows the value to be either a string or an array
// of strings.
//
// This is what tells first-party namespaces from vendor ones: a `use` of a
// namespace under one of these prefixes names code in this repo, anything else
// comes from a dependency.
func ComposerAutoloadRoots(source []byte) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Autoload    composerAutoload `json:"autoload"`
		AutoloadDev composerAutoload `json:"autoload-dev"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, block := range []composerAutoload{manifest.Autoload, manifest.AutoloadDev} {
		for _, m := range []map[string]json.RawMessage{block.PSR4, block.PSR0} {
			for prefix, raw := range m {
				prefix = strings.TrimRight(strings.TrimSpace(prefix), `\`)
				if prefix == "" {
					continue // the catch-all "" prefix maps everything; not a root
				}
				for _, dir := range composerAutoloadDirs(raw) {
					out[prefix] = appendUniqueString(out[prefix], dir)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

type composerAutoload struct {
	PSR4 map[string]json.RawMessage `json:"psr-4"`
	PSR0 map[string]json.RawMessage `json:"psr-0"`
}

// composerAutoloadDirs normalises an autoload value, which composer allows to
// be a bare string or an array of strings.
func composerAutoloadDirs(raw json.RawMessage) []string {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if d := normalizeComposerDir(one); d != "" {
			return []string{d}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	out := make([]string, 0, len(many))
	for _, d := range many {
		if d := normalizeComposerDir(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func normalizeComposerDir(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimPrefix(d, "./")
	return strings.TrimRight(d, "/")
}

func appendUniqueString(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// ComposerOwnName returns the `name` a composer.json declares for itself
// (`vendor/package`), used to keep a repo's own package out of its dependency
// list.
func ComposerOwnName(source []byte) string {
	if len(source) == 0 {
		return ""
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Name)
}
