package modules

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestParseGoMod_Variants(t *testing.T) {
	src := []byte(`module github.com/example/x

go 1.22

require github.com/spf13/cobra v1.10.0

require (
	github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06
	github.com/stretchr/testify v1.11.1
	go.uber.org/zap v1.27.1 // indirect
)

replace github.com/foo/bar => ./local/bar
`)
	specs := ParseGoMod(src)
	if len(specs) != 4 {
		t.Fatalf("expected 4 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}

	if got["github.com/spf13/cobra"].Version != "v1.10.0" {
		t.Errorf("cobra version = %q", got["github.com/spf13/cobra"].Version)
	}
	if !got["go.uber.org/zap"].Indirect {
		t.Errorf("zap should be indirect")
	}
	if got["go.uber.org/zap"].Indirect != true {
		t.Errorf("zap indirect flag wrong")
	}
	if got["github.com/sabhiram/go-gitignore"].Indirect {
		t.Errorf("go-gitignore should not be indirect")
	}
}

func TestParseGoMod_ReplaceDirective(t *testing.T) {
	src := []byte(`module x

require github.com/foo/bar v1.0.0
replace github.com/foo/bar => ./local/bar
`)
	specs := ParseGoMod(src)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Replace != "./local/bar" {
		t.Errorf("replace = %q", specs[0].Replace)
	}
}

func TestParseGoMod_Empty(t *testing.T) {
	if got := ParseGoMod(nil); got != nil {
		t.Errorf("nil input should yield nil specs")
	}
	if got := ParseGoMod([]byte("module x\n")); len(got) != 0 {
		t.Errorf("module-only manifest should have no deps")
	}
}

func TestModuleNodeID(t *testing.T) {
	cases := []struct {
		ecosystem, path, version, want string
	}{
		{"go", "github.com/foo/bar", "v1.0.0", "module::go:github.com/foo/bar@v1.0.0"},
		{"go", "github.com/foo/bar", "", "module::go:github.com/foo/bar"},
		{"npm", "lodash", "4.17.0", "module::npm:lodash@4.17.0"},
	}
	for _, c := range cases {
		if got := ModuleNodeID(c.ecosystem, c.path, c.version); got != c.want {
			t.Errorf("ModuleNodeID(%q,%q,%q) = %q, want %q",
				c.ecosystem, c.path, c.version, got, c.want)
		}
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0", Line: 5},
		{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0", Line: 6}, // dup
		{Ecosystem: "go", Path: "go.uber.org/zap", Version: "v1.27.1", Indirect: true, Line: 7},
	}
	nodes, edges := BuildGraphArtifacts("go.mod", specs)

	if len(nodes) != 2 {
		t.Errorf("expected 2 unique nodes, got %d", len(nodes))
	}
	if len(edges) != 3 {
		t.Errorf("expected 3 edges (one per spec, dups produce dup edges), got %d", len(edges))
	}
	for _, e := range edges {
		if e.From != "go.mod" {
			t.Errorf("edge from = %q", e.From)
		}
		if e.Kind != graph.EdgeDependsOnModule {
			t.Errorf("edge kind = %q", e.Kind)
		}
	}

	for _, n := range nodes {
		if n.Kind != graph.KindModule {
			t.Errorf("node kind = %q", n.Kind)
		}
		if n.Meta["ecosystem"] != "go" {
			t.Errorf("ecosystem meta = %v", n.Meta["ecosystem"])
		}
	}
	// Verify the indirect flag on the zap node.
	for _, n := range nodes {
		if n.Meta["path"] == "go.uber.org/zap" {
			if v, _ := n.Meta["indirect"].(bool); !v {
				t.Errorf("zap indirect flag missing")
			}
		}
	}
}

func TestBuildGraphArtifacts_PreservesCallerPathSpelling(t *testing.T) {
	// The indexer mints the synthetic manifest file node with the
	// exact relPath spelling; a re-spelled EdgeDependsOnModule From
	// endpoint dangles from a nonexistent node on Windows for any
	// manifest below the repo root.
	// Written out, not composed with filepath.Join: on a POSIX runner
	// Join yields exactly what the pre-fix ToSlash call returned, so
	// the assertion would hold with or without the fix.
	const rel = `services\api\go.mod`
	specs := []Spec{{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0", Line: 5}}
	nodes, edges := BuildGraphArtifacts(rel, specs)
	if len(nodes) != 1 || len(edges) != 1 {
		t.Fatalf("nodes = %d, edges = %d", len(nodes), len(edges))
	}
	if nodes[0].FilePath != rel {
		t.Errorf("node file path = %q, want %q", nodes[0].FilePath, rel)
	}
	if edges[0].From != rel {
		t.Errorf("edge.From = %q, want %q", edges[0].From, rel)
	}
	if edges[0].FilePath != rel {
		t.Errorf("edge file path = %q, want %q", edges[0].FilePath, rel)
	}
}

func TestParsePackageJSON_AllBlocks(t *testing.T) {
	src := []byte(`{
  "name": "my-app",
  "version": "1.0.0",
  "dependencies": {
    "react": "^18.2.0",
    "lodash": "4.17.21"
  },
  "devDependencies": {
    "vitest": "^1.0.0"
  },
  "peerDependencies": {
    "next": ">=13.0.0"
  },
  "optionalDependencies": {
    "fsevents": "^2.3.0"
  }
}`)
	specs := ParsePackageJSON(src)
	if len(specs) != 5 {
		t.Fatalf("expected 5 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
		if s.Ecosystem != "npm" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}
	if got["react"].Version != "^18.2.0" {
		t.Errorf("react version = %q", got["react"].Version)
	}
	if got["react"].Indirect {
		t.Errorf("react should NOT be indirect (production dep)")
	}
	if !got["vitest"].Indirect || got["vitest"].Replace != "dev" {
		t.Errorf("vitest should be dev-indirect: %+v", got["vitest"])
	}
	if got["next"].Replace != "peer" {
		t.Errorf("next.Replace = %q", got["next"].Replace)
	}
	if got["fsevents"].Replace != "optional" {
		t.Errorf("fsevents.Replace = %q", got["fsevents"].Replace)
	}
}

func TestParsePackageJSON_Empty(t *testing.T) {
	if got := ParsePackageJSON(nil); got != nil {
		t.Errorf("nil input → nil specs")
	}
	if got := ParsePackageJSON([]byte("{}")); len(got) != 0 {
		t.Errorf("empty manifest → empty specs")
	}
}

func TestParsePackageJSON_Malformed(t *testing.T) {
	if got := ParsePackageJSON([]byte("not json")); got != nil {
		t.Errorf("malformed input → nil")
	}
}

func TestParseNpmAlias(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"npm:@acme/shared-lib@1.4.0", "@acme/shared-lib", true},
		{"npm:lodash@4.17.21", "lodash", true},
		{"npm:left-pad", "left-pad", true},     // no version
		{"npm:@scope/pkg", "@scope/pkg", true}, // scoped, no version
		{"npm:@scope/pkg@^2.0.0", "@scope/pkg", true},
		{"^18.2.0", "", false},          // ordinary semver range
		{"4.17.21", "", false},          // bare version
		{"github:user/repo", "", false}, // git shorthand, not npm:
		{"npm:", "", false},             // prefix only
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := ParseNpmAlias(c.in)
		if name != c.wantName || ok != c.wantOK {
			t.Errorf("ParseNpmAlias(%q) = (%q, %t), want (%q, %t)",
				c.in, name, ok, c.wantName, c.wantOK)
		}
	}
}

func TestParsePackageJSON_NpmAlias(t *testing.T) {
	src := []byte(`{
  "name": "my-app",
  "dependencies": {
    "shared": "npm:@acme/shared-lib@1.4.0",
    "lodash4": "npm:lodash@4.17.21",
    "react": "^18.2.0"
  },
  "devDependencies": {
    "test-utils": "npm:@acme/test-utils"
  }
}`)
	specs := ParsePackageJSON(src)
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}

	// Aliased production dep: Path is the alias key, Alias the real name.
	if got["shared"].Alias != "@acme/shared-lib" {
		t.Errorf("shared.Alias = %q, want @acme/shared-lib", got["shared"].Alias)
	}
	if got["shared"].Version != "npm:@acme/shared-lib@1.4.0" {
		t.Errorf("shared.Version should keep the verbatim npm: string, got %q", got["shared"].Version)
	}
	if got["lodash4"].Alias != "lodash" {
		t.Errorf("lodash4.Alias = %q, want lodash", got["lodash4"].Alias)
	}
	// Aliased dev dep — alias is captured in devDependencies too.
	if got["test-utils"].Alias != "@acme/test-utils" {
		t.Errorf("test-utils.Alias = %q, want @acme/test-utils", got["test-utils"].Alias)
	}
	if got["test-utils"].Replace != "dev" {
		t.Errorf("test-utils should be a dev dep, got Replace=%q", got["test-utils"].Replace)
	}
	// Ordinary dep — no alias.
	if got["react"].Alias != "" {
		t.Errorf("react.Alias should be empty, got %q", got["react"].Alias)
	}
}

func TestParsePackageJSON_StableOrder(t *testing.T) {
	// JSON map iteration is randomised — our packageJSONBlock
	// helper sorts within each block to keep tests deterministic.
	src := []byte(`{"dependencies": {"zoo": "1.0", "alpha": "2.0", "beta": "3.0"}}`)
	specs := ParsePackageJSON(src)
	if len(specs) != 3 {
		t.Fatalf("expected 3, got %d", len(specs))
	}
	if specs[0].Path != "alpha" || specs[1].Path != "beta" || specs[2].Path != "zoo" {
		t.Errorf("not alphabetically sorted: %+v", specs)
	}
}

func TestParsePackageLockJSON_v3(t *testing.T) {
	src := []byte(`{
  "name": "myapp",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "myapp",
      "version": "1.0.0"
    },
    "node_modules/lodash": {
      "version": "4.17.21",
      "resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
      "integrity": "sha512-..."
    },
    "node_modules/react": {
      "version": "18.2.0"
    },
    "node_modules/vitest": {
      "version": "1.0.4",
      "dev": true
    },
    "node_modules/@scope/util": {
      "version": "2.5.0"
    },
    "node_modules/foo/node_modules/bar": {
      "version": "1.0.0"
    }
  }
}`)
	specs := ParsePackageLockJSON(src)
	if len(specs) != 5 {
		t.Fatalf("expected 5 specs (root entry skipped), got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
		if s.Ecosystem != "npm" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}
	if got["lodash"].Version != "4.17.21" {
		t.Errorf("lodash version = %q (lockfile resolved value, not semver range)", got["lodash"].Version)
	}
	if got["react"].Version != "18.2.0" {
		t.Errorf("react version = %q", got["react"].Version)
	}
	if !got["vitest"].Indirect || got["vitest"].Replace != "dev" {
		t.Errorf("vitest dev classification wrong: %+v", got["vitest"])
	}
	if got["@scope/util"].Version != "2.5.0" {
		t.Errorf("scoped package version = %q", got["@scope/util"].Version)
	}
	if got["foo/node_modules/bar"].Version != "1.0.0" {
		t.Errorf("nested-transitive path %q should preserve full chain (multi-version differentiator)",
			"foo/node_modules/bar")
	}
}

func TestParsePackageLockJSON_RejectsV1(t *testing.T) {
	src := []byte(`{
  "name": "myapp",
  "lockfileVersion": 1,
  "dependencies": {
    "lodash": {"version": "4.17.21"}
  }
}`)
	if got := ParsePackageLockJSON(src); got != nil {
		t.Errorf("v1 lockfile should yield nil specs (unsupported shape), got %+v", got)
	}
}

func TestParsePackageLockJSON_Empty(t *testing.T) {
	if got := ParsePackageLockJSON(nil); got != nil {
		t.Errorf("nil input should yield nil")
	}
	if got := ParsePackageLockJSON([]byte(`{"lockfileVersion": 3, "packages": {}}`)); len(got) != 0 {
		t.Errorf("empty packages map should yield empty specs")
	}
}

func TestParseYarnLock_v1(t *testing.T) {
	src := []byte(`# THIS IS AN AUTOGENERATED FILE. DO NOT EDIT THIS FILE DIRECTLY.
# yarn lockfile v1


lodash@^4.17.0:
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/lodash/-/lodash-4.17.21.tgz#..."
  integrity sha512-...

react@^18.2.0:
  version "18.2.0"
  resolved "https://registry.yarnpkg.com/react/-/react-18.2.0.tgz#..."

"@types/node@*", "@types/node@^20.0.0":
  version "20.10.5"
  resolved "https://registry.yarnpkg.com/@types/node/-/node-20.10.5.tgz#..."
`)
	specs := ParseYarnLock(src)
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]string{}
	for _, s := range specs {
		got[s.Path] = s.Version
		if s.Ecosystem != "npm" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}
	if got["lodash"] != "4.17.21" {
		t.Errorf("lodash = %q", got["lodash"])
	}
	if got["react"] != "18.2.0" {
		t.Errorf("react = %q", got["react"])
	}
	if got["@types/node"] != "20.10.5" {
		t.Errorf("@types/node = %q (scoped name should be preserved as is)", got["@types/node"])
	}
}

func TestParseYarnLock_DedupesMultiRangeBlocks(t *testing.T) {
	// `lodash@^4.0.0, lodash@^4.17.0:` is one block declaring two
	// ranges that resolve to the same version. Should yield one
	// Spec, not two.
	src := []byte(`lodash@^4.0.0, lodash@^4.17.0:
  version "4.17.21"
`)
	specs := ParseYarnLock(src)
	if len(specs) != 1 {
		t.Errorf("expected 1 deduped spec, got %d", len(specs))
	}
}

func TestParseYarnLock_Empty(t *testing.T) {
	if got := ParseYarnLock(nil); got != nil {
		t.Errorf("nil input should yield nil")
	}
}

func TestParsePnpmLock_v6(t *testing.T) {
	src := []byte(`lockfileVersion: '6.0'

settings:
  autoInstallPeers: true

importers:
  .:
    dependencies:
      react:
        specifier: ^18.2.0
        version: 18.2.0

packages:

  /react@18.2.0:
    resolution: {integrity: sha512-...}
    dependencies:
      loose-envify: 1.4.0

  /lodash@4.17.21:
    resolution: {integrity: sha512-...}

  /@types/node@20.10.5:
    resolution: {integrity: sha512-...}
`)
	specs := ParsePnpmLock(src)
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]string{}
	for _, s := range specs {
		got[s.Path] = s.Version
		if s.Ecosystem != "npm" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}
	if got["react"] != "18.2.0" || got["lodash"] != "4.17.21" {
		t.Errorf("got %+v", got)
	}
	if got["@types/node"] != "20.10.5" {
		t.Errorf("scoped pkg @types/node = %q", got["@types/node"])
	}
}

func TestParsePnpmLock_StripsPeerSuffix(t *testing.T) {
	// pnpm encodes peer-dep resolutions in the key:
	// `/some-pkg@1.0.0_react@18.2.0` — the `_react@...` suffix
	// is the peer combination, not part of the version. We strip
	// it so the canonical version stays clean.
	src := []byte(`packages:
  /some-pkg@1.0.0_react@18.2.0:
    resolution: {integrity: sha512-...}
`)
	specs := ParsePnpmLock(src)
	if len(specs) != 1 || specs[0].Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %+v", specs)
	}
}

func TestParsePnpmLock_Empty(t *testing.T) {
	if got := ParsePnpmLock(nil); got != nil {
		t.Errorf("nil input should yield nil")
	}
	if got := ParsePnpmLock([]byte("lockfileVersion: '6.0'\n")); len(got) != 0 {
		t.Errorf("manifest with no packages should yield empty specs")
	}
}

func TestParsePyProject_PEP621(t *testing.T) {
	src := []byte(`[project]
name = "myproj"
version = "0.1.0"
dependencies = [
    "litellm>=1.50.0",
    "docker>=7.0.0",
    "pyyaml",
    "flask[async]==2.0.0",
]

[project.optional-dependencies]
dev = [
    "pytest>=8.0.0",
    "ruff",
]
docs = [
    "mkdocs>=1.0",
]
`)
	specs := ParsePyProject(src)
	if len(specs) != 7 {
		t.Fatalf("expected 7 specs (4 prod + 2 dev + 1 docs), got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}
	if got["litellm"].Version != ">=1.50.0" {
		t.Errorf("litellm version = %q", got["litellm"].Version)
	}
	if got["pyyaml"].Version != "" {
		t.Errorf("unconstrained pkg should have empty version, got %q", got["pyyaml"].Version)
	}
	if got["flask"].Version != "==2.0.0" {
		t.Errorf("flask extras suffix should be stripped: %q", got["flask"].Version)
	}
	if got["pytest"].Replace != "dev" || !got["pytest"].Indirect {
		t.Errorf("pytest should be dev-indirect: %+v", got["pytest"])
	}
	if got["mkdocs"].Replace != "docs" {
		t.Errorf("mkdocs.Replace = %q", got["mkdocs"].Replace)
	}
}

func TestParsePyProject_Poetry(t *testing.T) {
	src := []byte(`[tool.poetry]
name = "myproj"

[tool.poetry.dependencies]
python = "^3.10"
requests = "^2.0"
django = { version = "^4.2", extras = ["bcrypt"] }

[tool.poetry.dev-dependencies]
pytest = "^8.0"
`)
	specs := ParsePyProject(src)
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}

	if _, ok := got["python"]; ok {
		t.Errorf("python interpreter constraint must not produce a Spec")
	}
	if got["requests"].Version != "^2.0" {
		t.Errorf("requests version = %q", got["requests"].Version)
	}
	if got["django"].Version != "^4.2" {
		t.Errorf("django version (from table) = %q", got["django"].Version)
	}
	if got["pytest"].Replace != "dev" {
		t.Errorf("pytest should be dev: %+v", got["pytest"])
	}
}

func TestParseRequirementsTxt(t *testing.T) {
	src := []byte(`# top-level comment
flask>=2.0.0
django==4.2.7  # inline comment
requests
-r other.txt
-e .
--index-url https://pypi.org/simple

# blank line above

git+https://github.com/x/y.git ; sys_platform == "darwin"
`)
	specs := ParseRequirementsTxt(src)
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}
	if got["flask"].Version != ">=2.0.0" {
		t.Errorf("flask version = %q", got["flask"].Version)
	}
	if got["django"].Version != "==4.2.7" {
		t.Errorf("django version (with inline comment stripped) = %q", got["django"].Version)
	}
	if _, ok := got["requests"]; !ok {
		t.Errorf("unconstrained 'requests' should still produce a spec")
	}
	if _, ok := got["other.txt"]; ok {
		t.Errorf("-r include must not be treated as a dep")
	}
	// `git+https://...` becomes `git` after splitPEP508 stripping;
	// it's a degraded recovery rather than ideal, but at least it
	// doesn't blow up. The git URL test pin documents current
	// behaviour.
	if _, ok := got["git"]; !ok {
		t.Logf("note: git+url shape recovers as 'git' (acknowledged degraded form)")
	}
}

func TestSplitPEP508(t *testing.T) {
	cases := []struct {
		in, name, version string
	}{
		{"requests>=2.0", "requests", ">=2.0"},
		{"flask[async]==2.0.0", "flask", "==2.0.0"},
		{"numpy", "numpy", ""},
		{"pkg ; python_version<'3.9'", "pkg", ""},
		{"foo @ https://example.com/foo.tar.gz", "foo", ""},
	}
	for _, c := range cases {
		name, version := splitPEP508(c.in)
		if name != c.name || version != c.version {
			t.Errorf("splitPEP508(%q) = (%q, %q), want (%q, %q)",
				c.in, name, version, c.name, c.version)
		}
	}
}

func TestParseCargoToml_AllBlocks(t *testing.T) {
	src := []byte(`[package]
name = "myapp"
version = "0.1.0"

[dependencies]
serde = "1.0"
tokio = { version = "1.30", features = ["full"] }
local-only = { path = "../local" }
git-only = { git = "https://example.com/git/repo.git" }

[dev-dependencies]
assert_cmd = "2"

[build-dependencies]
cc = "1"
`)
	specs := ParseCargoToml(src)
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
		if s.Ecosystem != "cargo" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}

	if len(specs) != 4 {
		t.Errorf("expected 4 specs (serde + tokio + assert_cmd + cc; local/git skipped), got %d: %+v",
			len(specs), specs)
	}
	if got["serde"].Version != "1.0" {
		t.Errorf("serde version = %q", got["serde"].Version)
	}
	if got["tokio"].Version != "1.30" {
		t.Errorf("tokio version (from table) = %q", got["tokio"].Version)
	}
	if _, ok := got["local-only"]; ok {
		t.Errorf("path-only dep must not produce a Spec")
	}
	if _, ok := got["git-only"]; ok {
		t.Errorf("git-only dep without version must not produce a Spec")
	}
	if !got["assert_cmd"].Indirect || got["assert_cmd"].Replace != "dev" {
		t.Errorf("assert_cmd should be dev-indirect: %+v", got["assert_cmd"])
	}
	if got["cc"].Replace != "build" {
		t.Errorf("cc.Replace = %q (want build)", got["cc"].Replace)
	}
}

func TestParseCargoToml_UnderscoreDevDeps(t *testing.T) {
	// Older Cargo manifests sometimes use the underscore form.
	src := []byte(`[package]
name = "p"

[dev_dependencies]
mockall = "0.11"
`)
	specs := ParseCargoToml(src)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Replace != "dev" {
		t.Errorf("Replace = %q (underscore form should still be dev)", specs[0].Replace)
	}
}

func TestParseCargoToml_Empty(t *testing.T) {
	if got := ParseCargoToml(nil); got != nil {
		t.Errorf("nil input should yield nil")
	}
	if got := ParseCargoToml([]byte("[package]\nname = \"x\"\n")); len(got) != 0 {
		t.Errorf("manifest with no deps should yield empty specs")
	}
}

func TestParsePomXML_AllScopes(t *testing.T) {
	src := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <groupId>com.example</groupId>
  <artifactId>myapp</artifactId>
  <version>1.0.0</version>
  <dependencies>
    <dependency>
      <groupId>org.springframework</groupId>
      <artifactId>spring-core</artifactId>
      <version>5.3.0</version>
    </dependency>
    <dependency>
      <groupId>com.fasterxml.jackson.core</groupId>
      <artifactId>jackson-databind</artifactId>
      <version>2.15.0</version>
      <scope>compile</scope>
    </dependency>
    <dependency>
      <groupId>junit</groupId>
      <artifactId>junit</artifactId>
      <version>4.13.2</version>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>javax.servlet</groupId>
      <artifactId>servlet-api</artifactId>
      <version>2.5</version>
      <scope>provided</scope>
    </dependency>
  </dependencies>
</project>`)
	specs := ParsePomXML(src)
	if len(specs) != 4 {
		t.Fatalf("expected 4 specs, got %d: %+v", len(specs), specs)
	}

	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
		if s.Ecosystem != "maven" {
			t.Errorf("ecosystem = %q for %q", s.Ecosystem, s.Path)
		}
	}
	if got["org.springframework:spring-core"].Version != "5.3.0" {
		t.Errorf("spring-core version = %q", got["org.springframework:spring-core"].Version)
	}
	// No-scope and explicit `compile` both treated as production.
	if got["org.springframework:spring-core"].Indirect {
		t.Errorf("missing scope should be production (Indirect=false)")
	}
	if got["com.fasterxml.jackson.core:jackson-databind"].Indirect {
		t.Errorf("explicit compile should be production")
	}
	if got["junit:junit"].Replace != "test" || !got["junit:junit"].Indirect {
		t.Errorf("junit/test scope wrong: %+v", got["junit:junit"])
	}
	if got["javax.servlet:servlet-api"].Replace != "provided" {
		t.Errorf("servlet-api Replace = %q", got["javax.servlet:servlet-api"].Replace)
	}
}

func TestParsePomXML_SkipsIncompleteCoordinate(t *testing.T) {
	src := []byte(`<?xml version="1.0"?>
<project>
  <dependencies>
    <dependency>
      <artifactId>missing-group</artifactId>
      <version>1.0</version>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <version>2.0</version>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>real</artifactId>
      <version>3.0</version>
    </dependency>
  </dependencies>
</project>`)
	specs := ParsePomXML(src)
	if len(specs) != 1 {
		t.Errorf("expected 1 spec (the only complete coordinate), got %d", len(specs))
	}
}

func TestParsePomXML_PropertyVersionVerbatim(t *testing.T) {
	// Property substitution is left verbatim; resolving it from
	// <properties> is a future enhancement noted inline.
	src := []byte(`<?xml version="1.0"?>
<project>
  <dependencies>
    <dependency>
      <groupId>org.springframework</groupId>
      <artifactId>spring-core</artifactId>
      <version>${spring.version}</version>
    </dependency>
  </dependencies>
</project>`)
	specs := ParsePomXML(src)
	if len(specs) != 1 || specs[0].Version != "${spring.version}" {
		t.Errorf("property reference should be kept verbatim, got %+v", specs)
	}
}

func TestLinkImports_LongestPrefix(t *testing.T) {
	g := graph.New()
	// Two import nodes — one for an exact match, one for a sub-package.
	g.AddNode(&graph.Node{
		ID:       "pkg/a.go::import::github.com/spf13/cobra",
		Kind:     graph.KindImport,
		FilePath: "pkg/a.go",
		Meta:     map[string]any{"path": "github.com/spf13/cobra"},
	})
	g.AddNode(&graph.Node{
		ID:       "pkg/b.go::import::github.com/spf13/cobra/doc",
		Kind:     graph.KindImport,
		FilePath: "pkg/b.go",
		Meta:     map[string]any{"path": "github.com/spf13/cobra/doc"},
	})
	g.AddNode(&graph.Node{
		ID:       "pkg/c.go::import::own/internal/foo",
		Kind:     graph.KindImport,
		FilePath: "pkg/c.go",
		Meta:     map[string]any{"path": "own/internal/foo"},
	})

	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/spf13/cobra", Version: "v1.0.0"},
		{Ecosystem: "go", Path: "go.uber.org/zap", Version: "v1.27.1"},
	}

	emitted := LinkImports(g, specs, "own")
	if emitted != 2 {
		t.Errorf("expected 2 edges (cobra exact + cobra/doc prefix; own/internal skipped), got %d", emitted)
	}

	wantTo := "module::go:github.com/spf13/cobra@v1.0.0"
	hits := 0
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeDependsOnModule && e.To == wantTo {
			hits++
		}
	}
	if hits != 2 {
		t.Errorf("expected 2 edges to %q, got %d", wantTo, hits)
	}
}

func TestLinkImports_PrefersLongerSpecForVersionedImports(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "f::import::github.com/foo/bar/v2/sub",
		Kind:     graph.KindImport,
		FilePath: "f.go",
		Meta:     map[string]any{"path": "github.com/foo/bar/v2/sub"},
	})

	// Both v1 and v2 exist — the longest match wins.
	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/foo/bar", Version: "v1.0.0"},
		{Ecosystem: "go", Path: "github.com/foo/bar/v2", Version: "v2.1.0"},
	}

	if got := LinkImports(g, specs, ""); got != 1 {
		t.Fatalf("expected 1 edge, got %d", got)
	}
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeDependsOnModule {
			if e.To != "module::go:github.com/foo/bar/v2@v2.1.0" {
				t.Errorf("wrong module target: %q (longest spec should win)", e.To)
			}
		}
	}
}

func TestLinkImports_SkipsWhenNoMatch(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "f::import::stdlib",
		Kind:     graph.KindImport,
		FilePath: "f.go",
		Meta:     map[string]any{"path": "fmt"},
	})

	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/foo/bar"},
	}

	if got := LinkImports(g, specs, ""); got != 0 {
		t.Errorf("stdlib import shouldn't match external module, got %d edges", got)
	}
}

type linkImportBatchRecorder struct {
	graph.Store
	calls int
	edges []*graph.Edge
}

func (r *linkImportBatchRecorder) AddBatch(_ []*graph.Node, edges []*graph.Edge) {
	r.calls++
	r.edges = append(r.edges, edges...)
}

func TestLinkImportsIn_NPMScopesAliasesAndBoundaries(t *testing.T) {
	store := &linkImportBatchRecorder{Store: graph.New()}
	imports := []*graph.Node{
		{ID: "skip-kind", Kind: graph.KindFunction, Meta: map[string]any{"path": "@acme/ui"}},
		{ID: "own", Kind: graph.KindImport, Meta: map[string]any{"path": "@ours/app/internal"}},
		{ID: "scoped", Kind: graph.KindImport, FilePath: "ui.ts", StartLine: 3, Meta: map[string]any{"path": "@acme/ui/theme/dark"}},
		{ID: "boundary", Kind: graph.KindImport, Meta: map[string]any{"path": "@acme/ui-kit"}},
		{ID: "alias", Kind: graph.KindImport, FilePath: "widget.ts", StartLine: 7, Meta: map[string]any{"path": "widget/runtime"}},
		{ID: "alias-target", Kind: graph.KindImport, Meta: map[string]any{"path": "@vendor/widget/runtime"}},
	}
	specs := []Spec{
		{Ecosystem: "npm", Path: "@acme/ui", Version: "2.0.0"},
		{Ecosystem: "npm", Path: "widget", Version: "1.4.0", Alias: "@vendor/widget"},
	}

	if got := LinkImportsIn(store, imports, specs, "@ours/app"); got != 2 {
		t.Fatalf("emitted %d edges, want 2", got)
	}
	if store.calls != 1 {
		t.Fatalf("AddBatch called %d times, want one batched write", store.calls)
	}
	if len(store.edges) != 2 {
		t.Fatalf("recorded %d edges, want 2", len(store.edges))
	}
	want := []struct {
		from string
		to   string
		line int
	}{
		{"scoped", "module::npm:@acme/ui@2.0.0", 3},
		{"alias", "module::npm:widget@1.4.0", 7},
	}
	for i, edge := range store.edges {
		if edge.From != want[i].from || edge.To != want[i].to || edge.Line != want[i].line {
			t.Errorf("edge[%d] = (%q, %q, line %d), want (%q, %q, line %d)",
				i, edge.From, edge.To, edge.Line, want[i].from, want[i].to, want[i].line)
		}
		if edge.Kind != graph.EdgeDependsOnModule || edge.Origin != graph.OriginASTResolved {
			t.Errorf("edge[%d] lost module-link metadata: %+v", i, edge)
		}
	}
}

func TestMatchLongestPrefix_ReferenceParity(t *testing.T) {
	specs := []Spec{
		{Ecosystem: "go", Path: "github.com/acme/lib", Version: "v1"},
		{Ecosystem: "go", Path: "github.com/acme/lib/v2", Version: "v2"},
		{Ecosystem: "npm", Path: "@scope/pkg", Version: "3"},
		{Ecosystem: "npm", Path: "alias", Version: "4", Alias: "@scope/real"},
		{Ecosystem: "cargo", Path: "serde", Version: "1"},
	}
	candidates := make(map[string]Spec, len(specs))
	for _, spec := range specs {
		if _, exists := candidates[spec.Path]; !exists {
			candidates[spec.Path] = spec
		}
	}
	reference := func(importPath string) (Spec, bool) {
		bestLength := -1
		var best Spec
		for _, spec := range specs {
			if importPath != spec.Path && !strings.HasPrefix(importPath, spec.Path+"/") {
				continue
			}
			if len(spec.Path) > bestLength {
				bestLength = len(spec.Path)
				best = spec
			}
		}
		return best, bestLength >= 0
	}
	imports := []string{
		"github.com/acme/lib",
		"github.com/acme/lib/internal",
		"github.com/acme/lib/v2/sub/package",
		"github.com/acme/library",
		"@scope/pkg",
		"@scope/pkg/subpath/deep",
		"@scope/pkg-extra",
		"alias/runtime",
		"@scope/real/runtime",
		"serde",
		"serde_json",
		"missing/dependency/path",
	}
	for _, importPath := range imports {
		want, wantOK := reference(importPath)
		got, gotOK, _ := matchLongestPrefix(importPath, candidates)
		if gotOK != wantOK || (gotOK && got != want) {
			t.Errorf("matchLongestPrefix(%q) = (%+v, %v), want (%+v, %v)",
				importPath, got, gotOK, want, wantOK)
		}
	}
}

func TestMatchLongestPrefix_WorkIndependentOfDependencyCount(t *testing.T) {
	sparse := map[string]Spec{
		"dep-09999":  {Path: "dep-09999"},
		"@scope/pkg": {Path: "@scope/pkg"},
	}
	large := make(map[string]Spec, 10_002)
	for i := 0; i < 10_000; i++ {
		path := fmt.Sprintf("dep-%05d", i)
		large[path] = Spec{Path: path}
	}
	large["@scope/pkg"] = Spec{Path: "@scope/pkg"}

	imports := []string{
		"dep-09999/sub/deep",
		"@scope/pkg/internal/file",
		"missing/dependency/with/depth",
	}
	sparseProbes := 0
	largeProbes := 0
	for _, importPath := range imports {
		_, _, probes := matchLongestPrefix(importPath, sparse)
		sparseProbes += probes
		_, _, probes = matchLongestPrefix(importPath, large)
		largeProbes += probes
	}
	if largeProbes != sparseProbes {
		t.Fatalf("10k dependencies changed lookup work: large=%d probes, sparse=%d", largeProbes, sparseProbes)
	}
	if largeProbes > 12 {
		t.Fatalf("lookup made %d probes for three shallow imports; want work bounded by path depth", largeProbes)
	}
}

func TestShortName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github.com/foo/bar", "bar"},
		{"github.com/foo/bar/v2", "bar"},
		{"github.com/foo/bar/v10", "bar"},
		{"foo", "foo"},
		{"", ""},
	}
	for _, c := range cases {
		if got := shortName(c.in); got != c.want {
			t.Errorf("shortName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
