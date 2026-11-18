package indexer

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A changed file whose prior rows carry no fingerprints gets the fingerprints
// of its HEAD content, parsed exactly as a fresh parse is, and only when that
// parse yields the rows the stack holds: a HEAD whose content is not what the
// rows were derived from gives nothing (the planner then stays conservative).
func TestHeadPriorFingerprintsMatchAFreshParseOfTheRowsContent(t *testing.T) {
	resetEditDeltaPriorFingerprints()
	t.Cleanup(resetEditDeltaPriorFingerprints)
	builderIsolateGit(t)
	root := builderTempDir(t, "prior-fingerprints")
	builderGit(t, root, "init", "--initial-branch=main")
	head := "package p\n\nimport \"strings\"\n\nfunc Upper(s string) string { return strings.ToUpper(s) }\n\nfunc lower(s string) string { return strings.ToLower(s) }\n"
	builderWriteTree(t, root, map[string]string{"go.mod": "module example.test/p\n\ngo 1.23\n", "p/p.go": head})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "head")
	sha := builderGit(t, root, "rev-parse", "HEAD")
	// The working tree has moved on; the prior is HEAD's.
	builderWriteFile(t, root, "p/p.go", head+"\nfunc Extra() {}\n")

	idx := newTestIndexer(graph.New())
	idx.rootPath = root
	idx.SetRepoPrefix("repo")
	abs := filepath.Join(root, "p", "p.go")
	wantGraph, wantDerived, rows, ok := idx.extractionFingerprintsOfContent(abs, []byte(head))
	if !ok || !wantDerived.complete() {
		t.Fatal("fixture precondition: HEAD's content does not fingerprint")
	}
	source := headPriorFingerprints(idx, root, sha, "stack")
	gotGraph, gotDerived, ok := source(abs, rows)
	if !ok || gotGraph != wantGraph || gotDerived != wantDerived {
		t.Fatalf("HEAD fingerprints = %+v %+v (%t), want %+v %+v", gotGraph, gotDerived, ok, wantGraph, wantDerived)
	}
	// A local the stored rows lost (folded after the parse) does not make
	// them another content's.
	var withoutLocal []*graph.Node
	for _, n := range rows {
		if n.Kind != graph.KindLocal && n.Kind != graph.KindParam {
			withoutLocal = append(withoutLocal, n)
		}
	}
	if len(withoutLocal) == len(rows) {
		t.Fatal("fixture precondition: HEAD's parse has no local or parameter")
	}
	resetEditDeltaPriorFingerprints()
	if _, gotDerived, ok := source(abs, withoutLocal); !ok || gotDerived != wantDerived {
		t.Fatal("rows without a folded local were refused HEAD's fingerprints")
	}
	// Rows from other content are not given HEAD's fingerprints.
	resetEditDeltaPriorFingerprints()
	_, _, other, _ := idx.extractionFingerprintsOfContent(abs, []byte(head+"\nfunc Other() {}\n"))
	if _, _, ok := source(abs, other); ok {
		t.Fatal("rows derived from other content were given HEAD's fingerprints")
	}
}
