package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A .vue/.svelte/.astro <script> is parsed by the JS/TS extractor and its
// import edges carry the host file path, so the SFC must count as a JS/TS
// importer or its relative imports never bind to in-repo .ts files.
func TestIsJSTSPathAcceptsSFC(t *testing.T) {
	for _, p := range []string{"src/A.vue", "src/B.svelte", "src/C.astro", "src/d.ts"} {
		if !isJSTSPath(p) {
			t.Errorf("isJSTSPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"src/x.go", "src/y.html", "README"} {
		if isJSTSPath(p) {
			t.Errorf("isJSTSPath(%q) = true, want false", p)
		}
	}
}

func TestResolveJSTSImportTargetFromVueScript(t *testing.T) {
	getNode := func(id string) *graph.Node {
		if id == "src/features/useGetAvailableThemes.ts" {
			return &graph.Node{ID: id, Kind: graph.KindFile}
		}
		return nil
	}
	got := resolveJSTSImportTarget(getNode, nil, "src/components/Foo.vue", "../features/useGetAvailableThemes")
	if got != "src/features/useGetAvailableThemes.ts" {
		t.Fatalf("got %q", got)
	}
}

// `import Foo from './Foo.vue'` names the SFC verbatim. probeJSTSFile must
// accept the explicit .vue/.svelte/.astro extension as-is (no extension
// probing on top of it) or every component-to-component import stays an
// external stub.
func TestProbeJSTSFileAcceptsExplicitSFC(t *testing.T) {
	files := map[string]bool{"src/Foo.vue": true, "src/Bar.svelte": true, "src/Baz.astro": true}
	getNode := func(id string) *graph.Node {
		if files[id] {
			return &graph.Node{ID: id, Kind: graph.KindFile}
		}
		return nil
	}
	for _, stem := range []string{"src/Foo.vue", "src/Bar.svelte", "src/Baz.astro"} {
		if got := probeJSTSFile(getNode, stem); got != stem {
			t.Errorf("probeJSTSFile(%q) = %q, want verbatim", stem, got)
		}
	}
	// No probing: `./Foo` never picks up Foo.vue.
	if got := probeJSTSFile(getNode, "src/Foo"); got != "" {
		t.Errorf("probeJSTSFile(src/Foo) = %q, want \"\"", got)
	}
}
