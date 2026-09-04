package blame

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// scopeRepo builds a one-commit git repository holding rel, authored by the
// given identity, and returns its root.
func scopeRepo(t *testing.T, rel, body, name, email string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", name},
		{"config", "user.email", email},
		{"add", "-A"},
		{"commit", "-q", "-m", "initial"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
			"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// lastAuthoredEmail reads authorship from wherever this store keeps it. A
// backend implementing BlameEnrichmentWriter takes the sidecar rows and never
// touches node Meta, so checking Meta alone reads empty on a stamped node and
// would make every assertion here pass for the wrong reason.
func lastAuthoredEmail(g graph.Store, nodeID string) string {
	if reader, ok := g.(graph.BlameEnrichmentReader); ok {
		for _, row := range reader.BlameRows("") {
			if row.NodeID == nodeID {
				return row.Email
			}
		}
	}
	n := g.GetNode(nodeID)
	if n == nil || n.Meta == nil {
		return ""
	}
	la, ok := n.Meta["last_authored"].(map[string]any)
	if !ok {
		return ""
	}
	email, _ := la["email"].(string)
	return email
}

// TestEnrichGraph_DoesNotStampAnotherRepositorysNodes is the multi-repo case
// the daemon actually runs: one combined graph, one EnrichGraph call per
// repository root. stripRepoPrefix resolves a node path by trying it under the
// current root and then retrying without its leading segment, so two
// repositories that share a relative path let the pass over repo-a open
// repo-a's file and stamp repo-b's node with repo-a's author.
//
// The stamp that produced was well-formed, plausible, and about a different
// file — and anything counting stamps as coverage then certified repo-b on the
// strength of it.
func TestEnrichGraph_DoesNotStampAnotherRepositorysNodes(t *testing.T) {
	const rel = "internal/foo.go"
	rootA := scopeRepo(t, rel, "package internal\n\nfunc Shared() {}\n", "alice", "alice@example.invalid")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "repo-a/" + rel + "::Shared", Kind: graph.KindFunction, Name: "Shared",
		FilePath: "repo-a/" + rel, StartLine: 3, EndLine: 3, RepoPrefix: "repo-a",
	})
	// Same relative path, different repository. Its file does not exist under
	// rootA at its full path, which is exactly what sends stripRepoPrefix down
	// the retry that finds repo-a's copy.
	g.AddNode(&graph.Node{
		ID: "repo-b/" + rel + "::Shared", Kind: graph.KindFunction, Name: "Shared",
		FilePath: "repo-b/" + rel, StartLine: 3, EndLine: 3, RepoPrefix: "repo-b",
	})

	if _, err := EnrichGraph(g, rootA, "repo-a"); err != nil {
		t.Fatalf("enrich repo-a: %v", err)
	}

	if got := lastAuthoredEmail(g, "repo-a/"+rel+"::Shared"); got != "alice@example.invalid" {
		t.Fatalf("repo-a node authorship = %q, want alice@example.invalid", got)
	}
	if got := lastAuthoredEmail(g, "repo-b/"+rel+"::Shared"); got != "" {
		t.Fatalf("repo-b node was stamped with %q by a pass over repo-a's root", got)
	}
}

// TestEnrichGraph_EmptyPrefixStillStampsASingleRepoGraph is the other half of
// the scope rule. A single-repo graph carries no prefix at all, so scoping had
// to keep "" meaning "this graph's only repository" rather than "no repository".
func TestEnrichGraph_EmptyPrefixStillStampsASingleRepoGraph(t *testing.T) {
	const rel = "internal/foo.go"
	root := scopeRepo(t, rel, "package internal\n\nfunc Solo() {}\n", "bob", "bob@example.invalid")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rel + "::Solo", Kind: graph.KindFunction, Name: "Solo",
		FilePath: rel, StartLine: 3, EndLine: 3,
	})

	count, err := EnrichGraph(g, root, "")
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count == 0 {
		t.Fatal("an unprefixed single-repo graph was skipped by the scope check")
	}
	if got := lastAuthoredEmail(g, rel+"::Solo"); got != "bob@example.invalid" {
		t.Fatalf("authorship = %q, want bob@example.invalid", got)
	}
}

// TestEnrichGraph_PrefixedPassSkipsUnprefixedNodes pins the direction that
// keeps the multi-repo daemon honest: a pass for one repository must not reach
// the shared-externals bucket, whose nodes carry no prefix and belong to no
// repository root.
func TestEnrichGraph_PrefixedPassSkipsUnprefixedNodes(t *testing.T) {
	const rel = "internal/foo.go"
	root := scopeRepo(t, rel, "package internal\n\nfunc Shared() {}\n", "carol", "carol@example.invalid")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rel + "::Shared", Kind: graph.KindFunction, Name: "Shared",
		FilePath: rel, StartLine: 3, EndLine: 3,
	})

	if _, err := EnrichGraph(g, root, "repo-a"); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if got := lastAuthoredEmail(g, rel+"::Shared"); got != "" {
		t.Fatalf("an unprefixed node was stamped by a repo-a pass: %q", got)
	}
}
