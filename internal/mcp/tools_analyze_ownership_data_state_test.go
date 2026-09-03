package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// addBlameNodeInRepo is addBlameNode with an explicit repo prefix, for the
// multi-repo cases where the caveat has to name WHICH repository is missing
// data rather than only reporting that some are. An empty email and zero
// timestamp produce an indexed symbol with no authorship at all.
func addBlameNodeInRepo(g graph.Store, repo, id, file, email string, ts int64) {
	n := &graph.Node{
		ID:         id,
		Kind:       graph.KindFunction,
		Name:       id,
		FilePath:   file,
		StartLine:  1,
		EndLine:    1,
		RepoPrefix: repo,
	}
	if email != "" || ts != 0 {
		n.Meta = map[string]any{
			"last_authored": map[string]any{"email": email, "timestamp": ts},
		}
	}
	g.AddNode(n)
}

func ownershipDataStateOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	state, ok := out["data_state"].(map[string]any)
	if !ok {
		t.Fatalf("no data_state on the answer: %+v", out)
	}
	return state
}

func TestAnalyzeOwnership_UnenrichedZeroSaysItIsNotAbsence(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Indexed symbols, no blame pass. The old answer was "no owners matched",
	// which reads exactly like "nobody owns this code".
	srv.graph.AddNode(&graph.Node{ID: "f.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "f.go"})
	srv.graph.AddNode(&graph.Node{ID: "f.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "f.go"})

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	if owners, _ := out["owners"].([]any); len(owners) != 0 {
		t.Fatalf("expected an empty answer, got %d owners", len(owners))
	}
	state := ownershipDataStateOf(t, out)
	if state["state"] != "never_built" {
		t.Fatalf("state = %v, want never_built", state["state"])
	}
	if state["source"] != "blame_enrichment" {
		t.Fatalf("source = %v", state["source"])
	}
	note, _ := state["note"].(string)
	if !strings.Contains(note, "NOT evidence") {
		t.Fatalf("the note does not refuse the absence reading: %q", note)
	}
	// The third property of the template: name the recovery AND rule out the
	// one that looks obvious. Indexing never reads git blame, so a caller who
	// reaches for reindex_repository gets the same empty answer back.
	recovery, _ := state["recovery"].(string)
	if !strings.Contains(recovery, "gortex enrich blame") {
		t.Fatalf("recovery does not name the pass: %q", recovery)
	}
	if !strings.Contains(recovery, "reindex_repository") {
		t.Fatalf("recovery does not rule out reindex_repository: %q", recovery)
	}
}

func TestAnalyzeOwnership_PartialNamesTheUnstampedRepoEvenWithRows(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNodeInRepo(srv.graph, "repo-a", "a/f.go::A", "a/f.go", "alice@x", now)
	// repo-b is indexed but was never blamed. Its symbols are invisible to
	// this answer, and the row from repo-a makes the answer look complete.
	addBlameNodeInRepo(srv.graph, "repo-b", "b/g.go::B", "b/g.go", "", 0)

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	if owners, _ := out["owners"].([]any); len(owners) != 1 {
		t.Fatalf("expected the repo-a owner to still be returned, got %d", len(owners))
	}
	state := ownershipDataStateOf(t, out)
	if state["state"] != "partial" {
		t.Fatalf("state = %v, want partial — a non-empty undercount is the dangerous shape", state["state"])
	}
	// The property is which repositories are named, not how many: the test
	// server's own fixture nodes are unstamped too, and reporting them is
	// correct. What must hold is that the unstamped repo appears and the
	// stamped one does not — a caveat that named repo-a would be telling the
	// caller to re-enrich the one repository that is already covered.
	repos, _ := state["repos"].([]any)
	named := map[string]bool{}
	for _, r := range repos {
		s, _ := r.(string)
		named[s] = true
	}
	if !named["repo-b"] {
		t.Fatalf("repos = %v, does not name the unstamped repo", repos)
	}
	if named["repo-a"] {
		t.Fatalf("repos = %v, names a repo that IS stamped", repos)
	}
}

func TestAnalyzeOwnership_BuiltZeroIsReportedAsReal(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", time.Now().Unix())

	// Blame is stamped and an owner exists; min_symbols removed the answer. A
	// caller told only "no owners matched" would go looking for enrichment
	// that is not missing.
	out := callAnalyzeOwnership(t, srv, map[string]any{"min_symbols": 5.0})
	if owners, _ := out["owners"].([]any); len(owners) != 0 {
		t.Fatalf("expected the threshold to empty the answer, got %d", len(owners))
	}
	state := ownershipDataStateOf(t, out)
	if state["state"] != "built" {
		t.Fatalf("state = %v, want built", state["state"])
	}
	if _, hasRecovery := state["recovery"]; hasRecovery {
		t.Fatal("a built state offered a recovery; there is nothing to recover")
	}
	note, _ := state["note"].(string)
	if !strings.Contains(note, "min_symbols") {
		t.Fatalf("the note does not name the threshold that emptied it: %q", note)
	}
}

func TestAnalyzeOwnership_FilterMissIsNotBlamedOnEnrichment(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameNode(srv.graph, "internal/auth/jwt.go::Verify", "internal/auth/jwt.go", "alice@x", time.Now().Unix())

	out := callAnalyzeOwnership(t, srv, map[string]any{"path_prefix": "internal/db/"})
	state := ownershipDataStateOf(t, out)
	if state["state"] != "built" {
		t.Fatalf("state = %v, want built — no symbol was in scope to be owned", state["state"])
	}
	note, _ := state["note"].(string)
	if !strings.Contains(note, "filters") {
		t.Fatalf("the note sends the caller after enrichment instead of the filter: %q", note)
	}
}

func TestAnalyzeOwnership_CompleteAnswerCarriesNoCaveat(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addBlameNode(srv.graph, "f.go::A", "f.go", "alice@x", now)
	addBlameNode(srv.graph, "f.go::B", "f.go", "bob@x", now)

	out := callAnalyzeOwnership(t, srv, map[string]any{})
	if _, present := out["data_state"]; present {
		t.Fatalf("a complete answer was annotated: %+v", out["data_state"])
	}
}

func TestOwnershipDataStateClassification(t *testing.T) {
	// The state machine on its own. Every row is a shape the handler can
	// produce; the end-to-end tests above pin that it produces them.
	for _, tc := range []struct {
		name       string
		candidates map[string]int
		stamped    map[string]int
		owners     int
		want       string
		wantRepos  []string
	}{
		{"nothing in scope", map[string]int{}, map[string]int{}, 0, dataStateBuilt, nil},
		{"scope holds only empty repos", map[string]int{"repo-a": 0}, map[string]int{}, 0, dataStateBuilt, nil},
		{"no repo stamped", map[string]int{"repo-a": 3, "repo-b": 2}, map[string]int{}, 0, dataStateNeverBuilt, []string{"repo-a", "repo-b"}},
		{"one repo stamped", map[string]int{"repo-a": 3, "repo-b": 2}, map[string]int{"repo-a": 3}, 1, dataStatePartial, []string{"repo-b"}},
		{"every repo stamped", map[string]int{"repo-a": 3}, map[string]int{"repo-a": 3}, 0, dataStateBuilt, nil},
		{"owners dropped by threshold", map[string]int{"repo-a": 3}, map[string]int{"repo-a": 3}, 2, dataStateBuilt, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownershipDataState(tc.candidates, tc.stamped, tc.owners)
			if got.State != tc.want {
				t.Fatalf("state = %q, want %q", got.State, tc.want)
			}
			if len(got.Repos) != len(tc.wantRepos) {
				t.Fatalf("repos = %v, want %v", got.Repos, tc.wantRepos)
			}
			for i, want := range tc.wantRepos {
				if got.Repos[i] != want {
					t.Fatalf("repos = %v, want %v", got.Repos, tc.wantRepos)
				}
			}
			if got.Note == "" {
				t.Fatal("every state must carry a note; a bare token is the thing this replaces")
			}
			if (got.State == dataStateBuilt) != (got.Recovery == "") {
				t.Fatalf("state %q with recovery %q: only a recoverable state may name a recovery",
					got.State, got.Recovery)
			}
		})
	}
}

func TestOwnershipDataStateLineCarriesTheNote(t *testing.T) {
	// The compact encoding is prose only — a caller there sees the line or
	// nothing, so the note and the recovery must both survive it.
	c := ownershipDataState(map[string]int{"repo-a": 2}, map[string]int{}, 0)
	line := c.line()
	if !strings.HasPrefix(line, "data_state: never_built (blame_enrichment)") {
		t.Fatalf("line does not lead with the machine-matchable token: %q", line)
	}
	for _, want := range []string{"repo-a", "NOT evidence", "gortex enrich blame", "reindex_repository"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line dropped %q: %q", want, line)
		}
	}
}
