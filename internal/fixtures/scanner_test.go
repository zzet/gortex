package fixtures

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestIsFixturePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"testdata/foo.json", true},
		{"pkg/sub/testdata/golden.txt", true},
		{"testdata/", true}, // bare directory
		{"src/parser.go", false},
		{"mytestdata/foo.json", false}, // not a whole segment
		{"data/test/foo.json", false},  // wrong directory
		{"", false},
	}
	for _, tc := range cases {
		if got := IsFixturePath(tc.path); got != tc.want {
			t.Errorf("IsFixturePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestTestContractSource(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// testdata category — Go convention, delegated to IsFixturePath.
		{"pkg/testdata/foo.json", "testdata"},
		{"testdata/x.go", "testdata"},

		// bench_fixtures: top-level and nested under repo prefix.
		{"bench/fixtures/di/laravel/routes/web.php", "bench_fixtures"},
		{"gortex/bench/fixtures/di/laravel/routes/web.php", "bench_fixtures"},

		// js_fixtures: __fixtures__ convention.
		{"src/__fixtures__/users.ts", "js_fixtures"},
		{"__fixtures__/users.ts", "js_fixtures"},

		// Production paths return "" (no tag).
		{"src/parser.go", ""},
		{"internal/contracts/http.go", ""},
		{"bench/runner.go", ""},                     // not bench/fixtures/
		{"app/fixtures/seed.sql", ""},               // bare fixtures/, not bench-prefixed
		{"src/__fixturesx__/foo.ts", ""},            // not a whole segment
		{"", ""},
	}
	for _, tc := range cases {
		if got := TestContractSource(tc.path); got != tc.want {
			t.Errorf("TestContractSource(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	t.Run("under testdata", func(t *testing.T) {
		nodes := BuildGraphArtifacts("pkg/testdata/foo.json", "json")
		if len(nodes) != 1 {
			t.Fatalf("expected 1 fixture node, got %d", len(nodes))
		}
		n := nodes[0]
		if n.Kind != graph.KindFixture {
			t.Errorf("kind = %q", n.Kind)
		}
		if n.ID != "pkg/testdata/foo.json" {
			t.Errorf("id = %q", n.ID)
		}
		if n.Name != "foo.json" {
			t.Errorf("name = %q", n.Name)
		}
	})
	t.Run("not a fixture", func(t *testing.T) {
		nodes := BuildGraphArtifacts("pkg/parser.go", "go")
		if len(nodes) != 0 {
			t.Errorf("expected nil, got %+v", nodes)
		}
	})
}

func TestBuildGraphArtifacts_PreservesCallerPathSpelling(t *testing.T) {
	// The standalone fixture node deliberately reuses the file path
	// as its node ID so it merges with the file identity; that only
	// works when the spelling matches the extractor's relPath exactly
	// (OS-native separators for subdirectory files on Windows).
	// The spelling is written out rather than composed with
	// filepath.Join so a backslash is present on every runner. The
	// qualifying `testdata/` segment keeps its forward slash because
	// IsFixturePath normalizes through filepath.ToSlash, which is the
	// identity on POSIX. Name is asserted by TestBuildGraphArtifacts:
	// it comes from filepath.Base, whose separator set is the running
	// platform's.
	const rel = `testdata/sub\foo.bin`
	nodes := BuildGraphArtifacts(rel, "binary")
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	if nodes[0].ID != rel {
		t.Errorf("node id = %q, want %q", nodes[0].ID, rel)
	}
	if nodes[0].FilePath != rel {
		t.Errorf("node file path = %q, want %q", nodes[0].FilePath, rel)
	}
}

func TestReclassifyFileToFixture(t *testing.T) {
	t.Run("upgrades file to fixture", func(t *testing.T) {
		n := &graph.Node{
			ID:       "pkg/testdata/foo.json",
			Kind:     graph.KindFile,
			FilePath: "pkg/testdata/foo.json",
		}
		ok := ReclassifyFileToFixture(n)
		if !ok {
			t.Fatal("expected reclassification")
		}
		if n.Kind != graph.KindFixture {
			t.Errorf("kind after = %q", n.Kind)
		}
		if v, _ := n.Meta["fixture"].(bool); !v {
			t.Errorf("fixture meta missing")
		}
	})
	t.Run("leaves non-fixture file alone", func(t *testing.T) {
		n := &graph.Node{
			ID:       "pkg/parser.go",
			Kind:     graph.KindFile,
			FilePath: "pkg/parser.go",
		}
		ok := ReclassifyFileToFixture(n)
		if ok {
			t.Errorf("regular file should not be reclassified")
		}
		if n.Kind != graph.KindFile {
			t.Errorf("kind changed to %q", n.Kind)
		}
	})
	t.Run("ignores non-file kind", func(t *testing.T) {
		n := &graph.Node{
			ID:   "pkg/testdata/foo.json::Bar",
			Kind: graph.KindFunction,
		}
		ok := ReclassifyFileToFixture(n)
		if ok {
			t.Errorf("non-file should not be reclassified")
		}
	})
}
