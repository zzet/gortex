package codeowners

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestParse_BasicRules(t *testing.T) {
	src := []byte(`# global ownership
* @everyone
*.go @go-team
/docs/ @docs-team @alice
internal/auth/ @org/security
`)
	rules := Parse(src)
	if len(rules) != 4 {
		t.Fatalf("expected 4 rules, got %d", len(rules))
	}
	if rules[0].Pattern != "*" || rules[0].Owners[0] != "@everyone" {
		t.Errorf("rule 0 wrong: %+v", rules[0])
	}
	if len(rules[2].Owners) != 2 {
		t.Errorf("rule 2 owners = %v", rules[2].Owners)
	}
}

func TestParse_StripsInlineComments(t *testing.T) {
	src := []byte("*.go @go-team   # owners of all go files\n")
	rules := Parse(src)
	if len(rules) != 1 || rules[0].Owners[0] != "@go-team" {
		t.Errorf("got %+v", rules)
	}
}

func TestMatchFile_LastMatchWins(t *testing.T) {
	rules := Parse([]byte(`* @everyone
*.go @go-team
internal/auth/ @security
`))
	cases := []struct {
		path string
		want []string
	}{
		{"README.md", []string{"@everyone"}},
		{"main.go", []string{"@go-team"}},
		{"internal/auth/jwt.go", []string{"@security"}},
	}
	for _, tc := range cases {
		got := MatchFile(tc.path, rules)
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("path %q: got %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestLoadFromRepo(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, ".github")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subdir, "CODEOWNERS")
	if err := os.WriteFile(path, []byte("* @everyone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, src, ok := LoadFromRepo(dir)
	if !ok {
		t.Fatalf("expected to find CODEOWNERS")
	}
	if src != ".github/CODEOWNERS" {
		t.Errorf("source path = %q", src)
	}
	if len(rules) != 1 {
		t.Errorf("rules = %d", len(rules))
	}
}

func TestLoadFromRepo_NoFile(t *testing.T) {
	dir := t.TempDir()
	_, _, ok := LoadFromRepo(dir)
	if ok {
		t.Errorf("expected ok=false for empty dir")
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	nodes, edges := BuildGraphArtifacts("pkg/foo.go", []string{"@org/security", "@alice"}, "go")
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	if nodes[0].ID != "team::org/security" {
		t.Errorf("team id = %q", nodes[0].ID)
	}
	if nodes[0].Meta["kind"] != "team" {
		t.Errorf("kind = %v", nodes[0].Meta["kind"])
	}
	if nodes[1].ID != "team::alice" {
		t.Errorf("person id = %q", nodes[1].ID)
	}
	if nodes[1].Meta["kind"] != "person" {
		t.Errorf("kind = %v", nodes[1].Meta["kind"])
	}
	if edges[0].Kind != graph.EdgeOwns {
		t.Errorf("edge kind = %q", edges[0].Kind)
	}
	if edges[0].From != "team::org/security" || edges[0].To != "pkg/foo.go" {
		t.Errorf("edge endpoints wrong: %s -> %s", edges[0].From, edges[0].To)
	}
}

func TestBuildGraphArtifacts_PreservesCallerPathSpelling(t *testing.T) {
	// The indexer keys eviction and incremental replacement by the
	// exact relPath spelling it hands the builder; a re-spelled edge
	// endpoint dangles from a nonexistent file node on Windows.
	// Written out, not composed with filepath.Join: on a POSIX runner
	// Join yields exactly what the pre-fix ToSlash call returned, so
	// the assertion would hold with or without the fix.
	const rel = `src\data\foo.go`
	nodes, edges := BuildGraphArtifacts(rel, []string{"@alice"}, "go")
	if len(nodes) != 1 || len(edges) != 1 {
		t.Fatalf("nodes = %d, edges = %d", len(nodes), len(edges))
	}
	if nodes[0].FilePath != rel {
		t.Errorf("node file path = %q, want %q", nodes[0].FilePath, rel)
	}
	if edges[0].To != rel {
		t.Errorf("edge.To = %q, want %q", edges[0].To, rel)
	}
	if edges[0].FilePath != rel {
		t.Errorf("edge file path = %q, want %q", edges[0].FilePath, rel)
	}
}
