package goanalysis

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// A path-independent dependency export still names the declaration in this
// checkout. The handle's call must keep the same target on the layer below;
// treating the imported position as outside the repository loses that edge.
func TestTypecheckCacheTrimmedExportsKeepContextDeclarations(t *testing.T) {
	root, g, reader := declarationFixture(t)
	p := newTestProvider(t)
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		trimmed := *cfg
		trimmed.BuildFlags = append(slices.Clone(cfg.BuildFlags), "-trimpath")
		return packages.Load(&trimmed, patterns...)
	}
	scope := cachedScope
	scope.Declarations = reader
	result, err := runCheckoutScope(t, p, g, root, scope)
	require.NoError(t, err)
	require.NotNil(t, result.Compiler)
	require.NotNil(t, result.Compiler.Cache)
	require.Empty(t, result.Compiler.Cache.Bypass)
	require.Equal(t, 1, result.Compiler.Cache.ClosureMisses)
	require.Contains(t, strings.Join(scopeOutput(t, g, p), "\n"),
		"edge api/api.go::Measure -calls-> impl/impl.go::Double",
		"trimmed dependency positions must map onto this checkout's unchanged declarations")
	reader.mu.Lock()
	asked := slices.Clone(reader.asked)
	reader.mu.Unlock()
	require.Contains(t, asked, "impl/impl.go")
	require.NotContains(t, asked, "other/other.go", "unprojected sibling uses must stay excluded")
}

func TestTypecheckCacheTrimmedExportsKeepLocalReplacementDeclarations(t *testing.T) {
	for _, inside := range []bool{true, false} {
		name := "outside"
		if inside {
			name = "inside"
		}
		t.Run(name, func(t *testing.T) {
			root, g, reader := declarationFixture(t)
			replacement := resolvedTempDir(t)
			if inside {
				replacement = filepath.Join(root, "localextra")
			}
			writeGoMod(t, replacement, "example.com/scope/extra")
			writeFile(t, replacement, "impl.go", readFixture(t, root, "impl/impl.go"))
			writeFile(t, root, "go.mod", readFixture(t, root, "go.mod")+
				"\nrequire example.com/scope/extra v0.0.0\nreplace example.com/scope/extra => "+filepath.ToSlash(replacement)+"\n")
			writeFile(t, root, "api/api.go", strings.Replace(readFixture(t, root, "api/api.go"),
				`"example.com/scope/impl"`, `"example.com/scope/extra"`, 1))
			// The layer below has a same-name declaration. Only a replacement
			// whose listed source is actually inside this checkout may use it.
			nodes := reader.nodes["impl/impl.go"]
			delete(reader.nodes, "impl/impl.go")
			for _, node := range nodes {
				node.ID = strings.Replace(node.ID, "impl/impl.go", "localextra/impl.go", 1)
				node.FilePath = "localextra/impl.go"
			}
			reader.nodes["localextra/impl.go"] = nodes
			p := newTestProvider(t)
			p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
				trimmed := *cfg
				trimmed.BuildFlags = append(slices.Clone(cfg.BuildFlags), "-trimpath")
				return packages.Load(&trimmed, patterns...)
			}
			scope := cachedScope
			scope.Declarations = reader
			result, err := runCheckoutScope(t, p, g, root, scope)
			require.NoError(t, err)
			require.NotNil(t, result.Compiler.Cache)
			require.Empty(t, result.Compiler.Cache.Bypass)
			joined := strings.Join(scopeOutput(t, g, p), "\n")
			want := "edge api/api.go::Measure -calls-> localextra/impl.go::Double"
			if inside {
				require.Contains(t, joined, want, "local replacements keep their current checkout declarations")
				require.Contains(t, reader.asked, "localextra/impl.go")
			} else {
				require.NotContains(t, joined, want, "an outside replacement must not bind to a checkout's decoy declaration")
				require.NotContains(t, reader.asked, "localextra/impl.go")
			}
		})
	}
}

func TestCheckoutTrimmedDeclarationFilesRequireListedCheckoutIdentity(t *testing.T) {
	root := resolvedTempDir(t)
	inside := filepath.Join(root, "impl")
	outside := resolvedTempDir(t)
	owned := &packages.Package{PkgPath: "example.com/scope/impl", Dir: inside,
		GoFiles:         []string{filepath.Join(inside, "impl.go")},
		CompiledGoFiles: []string{filepath.Join(inside, "_cgo_gotypes.go")}}
	foreign := &packages.Package{PkgPath: "example.com/scope/other", Dir: outside,
		GoFiles: []string{filepath.Join(outside, "impl.go")}}
	pkg := &packages.Package{Imports: map[string]*packages.Package{"owned": owned, "foreign": foreign}}
	aliases := checkoutTrimmedDeclarationFiles([]*packages.Package{pkg}, root, "example.com/scope")
	require.Equal(t, map[string]string{"example.com/scope/impl/impl.go": "impl/impl.go"}, aliases,
		"only exact listed Go files acquire an alias; generated, unknown and outside paths remain unavailable")
}

// Identical checkout content should share dependency export artifacts even
// when its absolute directory differs. A source change must still produce a
// different artifact; no retained state or metadata is transferred between
// these independent checkout loads.
func TestTypecheckCacheExportsReuseAcrossCheckouts(t *testing.T) {
	first, second := scopeFixture(t), scopeFixture(t)
	p := newTestProvider(t)
	export := func(root string) string {
		t.Helper()
		listing, err := p.runListing(t.Context(), root, []string{"./api"}, nil)
		require.NoError(t, err)
		for _, pkg := range listing.pkgs {
			if pkg.PkgPath == "example.com/scope/helper" {
				require.Equal(t, filepath.Join(root, "helper"), pkg.Dir,
					"package metadata must stay bound to the checkout being loaded")
				require.NotEmpty(t, pkg.ExportFile)
				return pkg.ExportFile
			}
		}
		t.Fatal("helper dependency absent from listing")
		return ""
	}
	firstExport := export(first)
	require.Equal(t, firstExport, export(second), "unchanged dependencies share a path-independent Go cache artifact")
	writeFile(t, second, "helper/helper.go", strings.Replace(readFixture(t, second, "helper/helper.go"),
		"w.N = n", "w.N = n + 1", 1))
	require.NotEqual(t, firstExport, export(second), "changed dependency content cannot reuse the old artifact")
	require.Equal(t, firstExport, export(first), "another checkout's edit cannot replace this checkout's dependency")
}
