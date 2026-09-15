package blame

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestParse_PorcelainBasic(t *testing.T) {
	// Synthetic porcelain: line 1 from Alice, lines 2-3 reuse the
	// same commit (cached header), line 4 from Bob (new commit
	// with full header). Tab-prefixed source lines are required —
	// porcelain emits `\t` even for blank source.
	out := []byte("1234567890abcdef1234567890abcdef12345678 1 1 3\n" +
		"author Alice\n" +
		"author-mail <test@example.com>\n" +
		"author-time 1700000000\n" +
		"author-tz +0000\n" +
		"committer Alice\n" +
		"committer-mail <test@example.com>\n" +
		"committer-time 1700000000\n" +
		"committer-tz +0000\n" +
		"summary first\n" +
		"filename foo.go\n" +
		"\tpackage main\n" +
		"1234567890abcdef1234567890abcdef12345678 2 2\n" +
		"\t\n" +
		"1234567890abcdef1234567890abcdef12345678 3 3\n" +
		"\tfunc Hello() {}\n" +
		"abcdef0123456789abcdef0123456789abcdef01 4 4 1\n" +
		"author Bob\n" +
		"author-mail <test@example.com>\n" +
		"author-time 1710000000\n" +
		"author-tz +0000\n" +
		"committer Bob\n" +
		"committer-mail <test@example.com>\n" +
		"committer-time 1710000000\n" +
		"committer-tz +0000\n" +
		"summary edit\n" +
		"filename foo.go\n" +
		"\t// later edit\n")
	got, err := Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(got), got)
	}
	if got[1].Email != "test@example.com" {
		t.Errorf("line 1 email = %q", got[1].Email)
	}
	if got[1].Timestamp.Unix() != 1700000000 {
		t.Errorf("line 1 timestamp = %v", got[1].Timestamp)
	}
	if got[4].Email != "test@example.com" {
		t.Errorf("line 4 email = %q", got[4].Email)
	}
	if got[2].Email != "test@example.com" {
		t.Errorf("line 2 email = %q (should reuse cached header)", got[2].Email)
	}
	if got[3].Email != "test@example.com" {
		t.Errorf("line 3 email = %q", got[3].Email)
	}
}

func TestParse_EmptyInput(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d lines", len(got))
	}
}

func TestPickLatest(t *testing.T) {
	older := Author{Commit: "a", Email: "alice@x", Timestamp: time.Unix(1000, 0)}
	newer := Author{Commit: "b", Email: "bob@x", Timestamp: time.Unix(2000, 0)}
	lines := map[int]Author{
		1: older,
		2: newer,
		3: older,
	}
	got := pickLatest(lines, 1, 3)
	if got == nil {
		t.Fatal("expected a result")
	}
	if got.Commit != "b" {
		t.Errorf("expected newest (Bob/b), got %+v", got)
	}
}

func TestPickLatest_NoCoverage(t *testing.T) {
	lines := map[int]Author{1: {}, 2: {}}
	got := pickLatest(lines, 10, 20)
	if got != nil {
		t.Errorf("expected nil for uncovered range, got %+v", got)
	}
}

func TestPickLatest_StartGreaterThanEnd(t *testing.T) {
	// Some node kinds emit StartLine == EndLine; ensure the
	// degenerate range still resolves a single-line author.
	a := Author{Commit: "x", Timestamp: time.Unix(1000, 0)}
	got := pickLatest(map[int]Author{5: a}, 5, 0) // EndLine == 0 → treated as StartLine
	if got == nil || got.Commit != "x" {
		t.Errorf("expected author at line 5, got %+v", got)
	}
}

func TestEnrichGraph_StampsLastAuthored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	if err := runCmd(t, repoDir, "git", "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.email", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.name", "Tester"); err != nil {
		t.Fatal(err)
	}
	source := "package main\n\nfunc Hello() {}\n"
	if err := writeFile(filepath.Join(repoDir, "main.go"), source); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "commit", "-q", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "main.go::Hello",
		Kind:      graph.KindFunction,
		Name:      "Hello",
		FilePath:  "main.go",
		StartLine: 3,
		EndLine:   3,
	})

	count, err := EnrichGraph(g, repoDir, "")
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 enriched node, got %d", count)
	}

	// last_authored now persists in the typed sidecar (change A), not Meta.
	byID := map[string]graph.BlameEnrichment{}
	for _, e := range g.BlameRows("") {
		byID[e.NodeID] = e
	}
	la, ok := byID["main.go::Hello"]
	if !ok {
		t.Fatalf("blame row for main.go::Hello missing from sidecar; rows=%+v", byID)
	}
	if la.Email != "test@example.com" {
		t.Errorf("email = %v", la.Email)
	}
	if la.Commit == "" {
		t.Errorf("commit empty")
	}
	if la.Timestamp == 0 {
		t.Errorf("timestamp zero")
	}
	if _, present := g.GetNode("main.go::Hello").Meta["last_authored"]; present {
		t.Errorf("last_authored must not remain in Node.Meta after sidecar migration")
	}
}

func TestEnrichGraph_EmitsAuthoredEdgeAndPersonNode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	if err := runCmd(t, repoDir, "git", "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.email", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.name", "Alice"); err != nil {
		t.Fatal(err)
	}
	source := "package main\n\nfunc Hello() {}\n"
	if err := writeFile(filepath.Join(repoDir, "main.go"), source); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "add", "main.go"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "commit", "-q", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "main.go::Hello",
		Kind:      graph.KindFunction,
		Name:      "Hello",
		FilePath:  "main.go",
		StartLine: 3,
		EndLine:   3,
	})

	if _, err := EnrichGraph(g, repoDir, ""); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	personID := PersonNodeID("test@example.com")
	person := g.GetNode(personID)
	if person == nil {
		t.Fatalf("person node %q not added", personID)
	}
	if person.Kind != graph.KindTeam {
		t.Errorf("person.Kind = %v, want KindTeam", person.Kind)
	}
	if person.Meta["kind"] != "person" {
		t.Errorf("person.Meta.kind = %v, want \"person\"", person.Meta["kind"])
	}

	edges := g.GetOutEdges(personID)
	var authored *graph.Edge
	for _, e := range edges {
		if e.Kind == graph.EdgeAuthored && e.To == "main.go::Hello" {
			authored = e
			break
		}
	}
	if authored == nil {
		t.Fatalf("EdgeAuthored from %s to main.go::Hello missing; out-edges = %+v", personID, edges)
	}
	if authored.Origin != graph.OriginASTResolved {
		t.Errorf("authored.Origin = %q, want ast_resolved", authored.Origin)
	}
	if _, ok := authored.Meta["commit"].(string); !ok {
		t.Errorf("authored.Meta.commit not a string: %v", authored.Meta["commit"])
	}
	if _, ok := authored.Meta["timestamp"].(int64); !ok {
		t.Errorf("authored.Meta.timestamp not int64: %T", authored.Meta["timestamp"])
	}
}

func TestEnrichGraph_PersonNodeRepoScoped(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	if err := runCmd(t, repoDir, "git", "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.email", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "config", "user.name", "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(repoDir, "lib.go"), "package main\n\nfunc Foo() {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "add", "lib.go"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(t, repoDir, "git", "commit", "-q", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:         "myrepo/lib.go::Foo",
		Kind:       graph.KindFunction,
		Name:       "Foo",
		FilePath:   "myrepo/lib.go",
		StartLine:  3,
		EndLine:    3,
		RepoPrefix: "myrepo",
	})

	if _, err := EnrichGraph(g, repoDir, "myrepo"); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	scopedID := "myrepo/" + PersonNodeID("test@example.com")
	if g.GetNode(scopedID) == nil {
		t.Fatalf("repo-scoped person node %q missing", scopedID)
	}
	if g.GetNode(PersonNodeID("test@example.com")) != nil {
		t.Errorf("unscoped person node leaked into multi-repo graph")
	}
}

func TestPersonNodeID(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"Alice@Example.com", "team::alice@example.com"},
		{"  bob@example.com  ", "team::bob@example.com"},
		{"", "team::"},
	}
	for _, c := range cases {
		if got := PersonNodeID(c.in); got != c.out {
			t.Errorf("PersonNodeID(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestShouldEnrichBlame(t *testing.T) {
	cases := []struct {
		kind graph.NodeKind
		want bool
	}{
		{graph.KindFunction, true},
		{graph.KindMethod, true},
		{graph.KindType, true},
		{graph.KindConstant, true},
		{graph.KindFile, false},
		{graph.KindImport, false},
		{graph.KindTodo, false},
		{graph.KindLicense, false},
		{graph.KindTeam, false},
		{graph.KindFlag, false},
	}
	for _, c := range cases {
		if got := shouldEnrichBlame(c.kind); got != c.want {
			t.Errorf("shouldEnrichBlame(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

func TestEnrichGraph_CachesPathNormalization(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := graph.New()
	for _, id := range []string{"repo/main.go::First", "repo/main.go::Second"} {
		g.AddNode(&graph.Node{
			ID:        id,
			Kind:      graph.KindFunction,
			FilePath:  "repo/main.go",
			StartLine: 1,
			EndLine:   1,
		})
	}

	originalFileExists := fileExists
	statCalls := 0
	fileExists = func(path string) bool {
		statCalls++
		return originalFileExists(path)
	}
	t.Cleanup(func() { fileExists = originalFileExists })

	if _, err := EnrichGraph(g, repoDir, ""); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if statCalls != 2 {
		t.Fatalf("file existence checks = %d, want 2 for one distinct prefixed path", statCalls)
	}
}

func TestFileExists_MatchesTestF(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fileExists(regular) {
		t.Error("regular file reported missing")
	}
	if fileExists(dir) {
		t.Error("directory reported as a regular file")
	}
	if fileExists(filepath.Join(dir, "missing")) {
		t.Error("missing path reported as a regular file")
	}

	link := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, link); err == nil && !fileExists(link) {
		t.Error("symlink to a regular file reported missing")
	}
}

// --- helpers ---

func runCmd(t *testing.T, dir, name string, args ...string) error {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &cmdError{name: name, args: args, out: string(out), err: err}
	}
	return nil
}

type cmdError struct {
	name string
	args []string
	out  string
	err  error
}

func (e *cmdError) Error() string {
	return e.name + " " + strings.Join(e.args, " ") + ": " + e.err.Error() + "\n" + e.out
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
