package tsalias

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// WorkingTreeVisits agrees with the walk: every path the walk visits is
// reported, and paths it prunes or never offers are not.
func TestWorkingTreeVisitsMatchesTheWalk(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"tsconfig.json",
		"app/tsconfig.json",
		"app/nuxt.config.ts",
		"app/page.ts",
		"app/.nuxt/imports.d.ts",
		"app/.nuxt/tsconfig.json",
		"node_modules/pkg/tsconfig.json",
		"node_modules/pkg/.nuxt/imports.d.ts",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const extra = ".nuxt/imports.d.ts"
	scope := []string{"nuxt.config.ts"}
	var visited []string
	WorkingTree(root, extra, scope...).Files(func(rel string) { visited = append(visited, rel) })
	for _, rel := range visited {
		if !WorkingTreeVisits(rel, extra, scope...) {
			t.Errorf("walk visited %s but WorkingTreeVisits says no", rel)
		}
	}
	for _, rel := range []string{"app/page.ts", "app/.nuxt/tsconfig.json", "node_modules/pkg/tsconfig.json", "node_modules/pkg/.nuxt/imports.d.ts", "../tsconfig.json"} {
		if slices.Contains(visited, rel) {
			t.Fatalf("fixture: the walk visited %s", rel)
		}
		if WorkingTreeVisits(rel, extra, scope...) {
			t.Errorf("WorkingTreeVisits(%s) = true for a path the walk never offers", rel)
		}
	}
	// A pruned directory's generated list is visited whether or not it exists.
	if !WorkingTreeVisits("web/.nuxt/imports.d.ts", extra, scope...) {
		t.Error("an absent generated list under a descended directory must count as visited")
	}
}
