package indexer

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/resolver"
)

// npmPackageLookup lazily snapshots manifest evidence once per resolver. A new
// resolver captures fresh roots and reads fresh manifests after edits or track/untrack.
func (mi *MultiIndexer) npmPackageLookup() resolver.NpmPackageLookup {
	type entry struct {
		dir          string
		workspaceID  string
		exports      *packageExports
		declared     map[string]string
		typesExports map[string]string
		arrayRoot    npmArrayRoot
		hasExports   bool
		types        string
		main         string

		recognizedRoot bool
	}
	roots := map[string]string{}
	workspaces := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
			workspaces[prefix] = prefix
			if idx := mi.GetIndexer(prefix); idx != nil && idx.WorkspaceID() != "" {
				workspaces[prefix] = idx.WorkspaceID()
			}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	packages := map[string][]entry{}
	var once sync.Once
	build := func() {
		for prefix, root := range roots {
			rootSrc, _ := readDiskFile(joinPath(root, "package.json"))
			workspace := &modules.WorkspaceManifest{Ecosystem: modules.WorkspaceNPM, Patterns: modules.ParsePackageJSONWorkspaces(rootSrc)}
			if len(workspace.Patterns) == 0 {
				src, _ := readDiskFile(joinPath(root, "pnpm-workspace.yaml"))
				workspace.Ecosystem = modules.WorkspacePnpm
				workspace.Patterns = modules.ParsePnpmWorkspaceYAML(src)
			}
			dirs := []string{"."}
			for _, member := range modules.ResolveWorkspaceMembers(root, workspace) {
				dirs = append(dirs, member.Dir)
			}
			for _, dir := range dirs {
				if path.IsAbs(dir) || hasDotDotSegment(dir) {
					continue
				}
				src := rootSrc
				if dir != "." {
					src, _ = readDiskFile(joinPath(root, joinRel(dir, "package.json")))
				}
				var manifest struct {
					Name    string          `json:"name"`
					Exports json.RawMessage `json:"exports"`
					Types   string          `json:"types"`
					Main    string          `json:"main"`
				}
				if json.Unmarshal(src, &manifest) != nil || manifest.Name == "" {
					continue
				}
				var conditions map[string]json.RawMessage
				_ = json.Unmarshal(manifest.Exports, &conditions)
				if root, ok := conditions["."]; ok {
					conditions = nil
					_ = json.Unmarshal(root, &conditions)
				}
				exports := parsePackageExports(src)
				declared := declaredNpmExports(manifest.Exports)
				// Keep unsupported and blocked keys in the target map so an
				// exact key cannot fall through to a broader wildcard target.
				if exports != nil {
					for key := range declared {
						if _, ok := exports.subpaths[key]; !ok {
							exports.subpaths[key] = ""
						}
					}
				}
				packages[manifest.Name] = append(packages[manifest.Name], entry{
					recognizedRoot: hasRecognizedNpmCondition(conditions),

					dir: path.Join(prefix, dir), workspaceID: workspaces[prefix], exports: exports, declared: declared, typesExports: npmTypesExportTargets(manifest.Exports), arrayRoot: npmExportsArrayRoot(manifest.Exports), hasExports: hasPackageExports(manifest.Exports), types: manifest.Types, main: manifest.Main,
				})
			}
		}
	}
	return func(specifier string) map[string]resolver.NpmPackageTarget {
		once.Do(build)
		name, sub := splitPackageSpecifier(specifier)
		if name == "" || hasDotDotSegment(sub) || len(packages[name]) == 0 {
			return nil
		}
		targets := map[string]resolver.NpmPackageTarget{}
		for _, pkg := range packages[name] {
			targets[pkg.dir] = resolver.NpmPackageTarget{WorkspaceID: pkg.workspaceID}
			var target string
			if pkg.exports != nil {
				target = resolveExportsSubpath(pkg.exports.subpaths, sub)
			}
			declaration := resolveExportsSubpath(pkg.declared, sub)
			if pkg.hasExports && declaration != "declared" {
				continue
			}
			if sub == "" && pkg.arrayRoot.isArray {
				// Node picks the first supported array target before it looks at
				// the filesystem; an array of only null items blocks the root.
				if pkg.arrayRoot.blocked {
					continue
				}
				target = pkg.arrayRoot.target
			}
			if sub == "" && target == "" && pkg.recognizedRoot {
				continue // A recognized null/empty root blocks legacy candidates.
			}
			// Root entries with unsupported conditions keep legacy candidates.
			// A subpath must name its target; never invent a directory probe.
			paths := []string{target, resolveExportsSubpath(pkg.typesExports, sub)}
			if !pkg.hasExports || (target == "" && sub == "") {
				if sub == "" {
					paths = append(paths, pkg.types, pkg.main, "index")
				} else {
					paths = append(paths, sub)
				}
			}
			for _, target := range paths {
				// Never let manifest paths bind another package/repository.
				if target == "" || path.IsAbs(target) || hasDotDotSegment(target) {
					continue
				}
				candidate := targets[pkg.dir]
				candidate.Entries = append(candidate.Entries, path.Join(pkg.dir, target))
				targets[pkg.dir] = candidate
			}
		}
		return targets
	}
}

// declaredNpmExports retains entry keys independently of supported conditions.
// Values are matcher markers, not paths, so wildcard substitution leaves them intact.
func declaredNpmExports(raw json.RawMessage) map[string]string {
	var asString string
	if json.Unmarshal(raw, &asString) == nil && hasPackageExports(raw) {
		return map[string]string{".": "declared"}
	}
	if npmExportsArrayRoot(raw).isArray {
		return map[string]string{".": "declared"}
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	if !hasSubpathKeys(entries) {
		return map[string]string{".": "declared"}
	}
	declared := make(map[string]string, len(entries))
	for key, target := range entries {
		declared[key] = "declared"
		if bytes.Equal(bytes.TrimSpace(target), []byte("null")) {
			declared[key] = "blocked"
		}
	}
	return declared
}

// hasPackageExports reports whether package.json declares exports. Node treats
// a null exports field like an absent one and falls back to main.
func hasPackageExports(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// npmArrayRoot describes a top-level exports array, which Node reads as an
// ordered list of root targets.
type npmArrayRoot struct {
	isArray bool
	target  string // first supported target, "" when none
	blocked bool   // no item names a target, not even an unsupported one
}

// npmExportsArrayRoot picks the first supported target of a top-level exports
// array, as Node does before it checks the filesystem. Nested arrays are
// searched in order; items that are not "./" paths are skipped. When no item
// is supported but some item has only conditions the indexer does not read,
// the root is neither targeted nor blocked, so legacy candidates apply.
func npmExportsArrayRoot(raw json.RawMessage) npmArrayRoot {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return npmArrayRoot{}
	}
	target, unsupported := firstNpmArrayTarget(items)
	return npmArrayRoot{isArray: true, target: target, blocked: target == "" && !unsupported}
}

func firstNpmArrayTarget(items []json.RawMessage) (target string, unsupported bool) {
	for _, item := range items {
		var nested []json.RawMessage
		if json.Unmarshal(item, &nested) == nil && nested != nil {
			t, u := firstNpmArrayTarget(nested)
			if t != "" {
				return t, false
			}
			unsupported = unsupported || u
			continue
		}
		var conditions map[string]json.RawMessage
		isObject := json.Unmarshal(item, &conditions) == nil && conditions != nil
		if t := pickConditionalTarget(item); strings.HasPrefix(t, "./") {
			return t, false
		}
		// Only conditions the indexer cannot read leave the root to legacy
		// candidates; a recognized condition with a null or invalid target
		// resolves to nothing, as in Node.
		if isObject && !hasRecognizedNpmCondition(conditions) {
			unsupported = true
		}
	}
	return "", unsupported
}

// hasRecognizedNpmCondition reports whether a conditional exports object
// names a condition pickConditionalTarget reads.
func hasRecognizedNpmCondition(conditions map[string]json.RawMessage) bool {
	_, imp := conditions["import"]
	_, def := conditions["default"]
	return imp || def
}

// npmTypesExportTargets keeps declaration-file alternatives separate from the
// runtime target. The resolver tries them only after the runtime file misses.
func npmTypesExportTargets(raw json.RawMessage) map[string]string {
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return nil
	}
	if !hasSubpathKeys(entries) {
		return map[string]string{".": npmTypesCondition(raw)}
	}
	targets := make(map[string]string, len(entries))
	for key, value := range entries {
		targets[key] = npmTypesCondition(value)
	}
	return targets
}

func npmTypesCondition(raw json.RawMessage) string {
	var conditions map[string]json.RawMessage
	if json.Unmarshal(raw, &conditions) != nil {
		return ""
	}
	if target := pickConditionalTarget(conditions["types"]); target != "" {
		return target
	}
	for _, condition := range []string{"import", "default"} {
		if target := npmTypesCondition(conditions[condition]); target != "" {
			return target
		}
	}
	return ""
}
