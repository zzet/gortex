package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// A method that both reads and writes a field keeps one direct capability row
// for it, and the whole index keeps the read. The per-save pass decides
// whether a method's mutated-field set changed (and so whether its receiver
// callers need re-deriving) from the writes, and it re-derives rows in the
// whole index's representative order; both must agree with the whole index,
// or every save of such a file re-derives — and republishes — every
// transitive receiver caller.
func capabilityWriteSetTree() map[string]string {
	return map[string]string{
		"pkg/state.go": `package pkg

import "sync"

type Store struct {
	mu sync.Mutex
	n  int
}

func (s *Store) Bump() int {
	if s.n > 10 {
		return s.n
	}
	s.n = s.n + 1
	return 0
}

func (s *Store) Reset() int {
	s.n = 0
	return s.n
}

func (s *Store) Other() int { return 1 }
`,
		"pkg/use.go": `package pkg

func (s *Store) Touch() { s.Bump() }

func (s *Store) Outer() {
	s.Touch()
}

func (s *Store) Far() {
	s.Outer()
}

func (s *Store) Both() { s.Reset(); s.Bump() }
`,
	}
}

var capabilityWriteSetEdits = map[string]string{
	// A declaration is added (the file is re-parsed and its methods are the
	// pass's roots) but nothing any method mutates changes: no receiver
	// caller is re-derived.
	"unrelated_body": strings.Replace(capabilityWriteSetTree()["pkg/state.go"],
		"func (s *Store) Other() int { return 1 }", "func (s *Store) Other() int { return 1 }\n\nfunc (s *Store) Extra() int { return 3 }", 1),
	// Bump stops writing n (it still reads it): Touch, Outer and Far lose their
	// indirect mutation of n.
	"stops_writing": strings.Replace(capabilityWriteSetTree()["pkg/state.go"],
		"\ts.n = s.n + 1\n", "\t_ = s.n + 1\n", 1),
	// Other is renamed in place: no other method's inputs move, so their
	// rows are republished; Other writes nothing.
	"rename_in_place": strings.Replace(capabilityWriteSetTree()["pkg/state.go"],
		"func (s *Store) Other() int", "func (s *Store) OtherRen() int", 1),
	// A line above every method: every row's line moves by one, so the
	// methods are reused with their rows moved, never at their old lines.
	"line_shift": strings.Replace(capabilityWriteSetTree()["pkg/state.go"],
		"type Store struct {", "// Store holds state.\ntype Store struct {", 1),
	// use.go: a line above the receiver callers moves every call site; their
	// indirect mutations come from the prior and must land at the new lines.
	"use:line_shift": strings.Replace(capabilityWriteSetTree()["pkg/use.go"],
		"func (s *Store) Touch()", "// Touch bumps.\nfunc (s *Store) Touch()", 1),
	// use.go: Touch stops calling Bump; Outer and Far (same file, inputs
	// unchanged) lose the indirect mutation they had through it.
	"use:touch_stops": strings.Replace(capabilityWriteSetTree()["pkg/use.go"],
		"func (s *Store) Touch() { s.Bump() }", "func (s *Store) Touch() { _ = s }", 1),
	// use.go: Far is renamed; Touch, Outer and Both keep their inputs and
	// their indirect mutations come from the prior.
	"use:rename_in_place": strings.Replace(capabilityWriteSetTree()["pkg/use.go"],
		"func (s *Store) Far()", "func (s *Store) FarRen()", 1),
}

func TestPerSaveCapabilityWriteSetMatchesTheWholeIndex(t *testing.T) {
	builderIsolateGit(t)
	for name, edited := range capabilityWriteSetEdits {
		t.Run(name, func(t *testing.T) {
			// Keep the semantic case name while using Windows-safe disk paths.
			fixtureName := strings.ReplaceAll(name, ":", "-")
			dir := builderTempDir(t, "capws-"+fixtureName)
			builderWriteTree(t, dir, capabilityWriteSetTree())
			store := builderOpenStore(t, "capws-"+fixtureName)
			builderIndex(t, store, dir)
			core, logs := observer.New(zap.InfoLevel)
			idx := New(store, builderRegistry(), config.Default().Index, zap.New(core))
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			idx.rootPath = dir
			file := "state.go"
			if strings.HasPrefix(name, "use:") {
				file = "use.go"
			}
			path := filepath.Join(dir, "pkg", file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("warm-up IndexFile: %v", err)
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			before := logs.Len()
			reusedBefore, rowsBefore := capabilitySourcesReused.Load(), capabilityRowsReused.Load()
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("IndexFile: %v", err)
			}
			reusedSources := capabilitySourcesReused.Load() - reusedBefore
			reusedRows := capabilityRowsReused.Load() - rowsBefore
			switch name {
			case "rename_in_place":
				if reusedSources == 0 {
					t.Fatal("a rename in place evaluated every method again")
				}
			case "use:rename_in_place":
				if reusedRows == 0 {
					t.Fatal("a rename in place took no receiver caller's indirect mutations from the prior")
				}
			case "line_shift":
				if reusedSources == 0 {
					t.Fatal("a save that only moved every method evaluated every method again")
				}
			case "use:line_shift":
				if reusedRows == 0 {
					t.Fatal("a save that only moved every call site took no indirect mutation from the prior")
				}
			}
			perSave := capabilityRows(t, store)
			if name == "unrelated_body" {
				own := 0
				for _, row := range perSave {
					if strings.HasPrefix(strings.Split(row, "|")[1], "repo/pkg/state.go::") {
						own++
					}
				}
				seen := false
				for _, entry := range logs.All()[before:] {
					if entry.Message != "incremental derived passes complete" {
						continue
					}
					seen = true
					if got, _ := entry.ContextMap()["capability_edges"].(int64); int(got) != own {
						t.Fatalf("an unrelated body edit re-derived %d capability rows, want the file's own %d (a receiver caller was re-derived)", got, own)
					}
				}
				if !seen {
					t.Fatal("no derived pass ran")
				}
			}
			cleanDir := builderTempDir(t, "capws-clean-"+fixtureName)
			tree := capabilityWriteSetTree()
			tree["pkg/"+file] = edited
			builderWriteTree(t, cleanDir, tree)
			clean := builderOpenStore(t, "capws-clean-"+fixtureName)
			builderIndex(t, clean, cleanDir)
			whole := capabilityRows(t, clean)
			onlyPerSave, onlyWhole := diffRows(perSave, whole)
			if len(onlyPerSave) > 0 || len(onlyWhole) > 0 {
				t.Fatalf("capability rows differ from a whole index of the edited tree\nper-save only:\n  %s\nwhole-index only:\n  %s",
					strings.Join(onlyPerSave, "\n  "), strings.Join(onlyWhole, "\n  "))
			}
			if name == "stops_writing" {
				for _, row := range whole {
					if strings.Contains(row, "use.go::Store.Outer|") && strings.Contains(row, "Store.n|") {
						t.Fatalf("fixture: the whole index still records Outer mutating n: %s", row)
					}
				}
			}
		})
	}
}

// Two receiver calls on one line that both mutate a field derive two rows
// under one stored identity; both writers keep the same one whatever order
// they listed them in.
func TestCollapseCapabilityIdentitiesIsOrderIndependent(t *testing.T) {
	row := func(via string) *graph.Edge {
		return &graph.Edge{From: "m", To: "f", Kind: graph.EdgeAccessesField, FilePath: "a.go", Line: 3,
			Meta: map[string]any{"access": "write", "indirect": true, "via": via}}
	}
	for _, order := range [][]string{{"getProcesses", "getCommunities"}, {"getCommunities", "getProcesses"}} {
		out := collapseCapabilityIdentities([]*graph.Edge{row(order[0]), row(order[1]), {From: "m", To: "g", Kind: graph.EdgeAccessesField, FilePath: "a.go", Line: 3}})
		if len(out) != 2 {
			t.Fatalf("collapsed to %d rows, want 2", len(out))
		}
		if via, _ := out[0].Meta["via"].(string); via != "getCommunities" {
			t.Fatalf("order %v kept via %q, want the smallest", order, via)
		}
	}
}

// adjacencyRecordingStore records every batched adjacency read by node ID.
type adjacencyRecordingStore struct {
	graph.Store
	out, in map[string]struct{}
}

func (s *adjacencyRecordingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	for _, id := range ids {
		s.out[id] = struct{}{}
	}
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *adjacencyRecordingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	for _, id := range ids {
		s.in[id] = struct{}{}
	}
	return s.Store.GetInEdgesByNodeIDs(ids)
}

// The capability pass's inputs over a per-save whose mutated-field sets are
// unchanged are the changed file's own sources: it reads no other source's
// adjacency and walks no receiver caller backwards (the derived-pass scope
// contract, TestIncrementalDerivedPassesReadOnlyTheDeltasFiles, for the
// capability pass's own reads).
func TestCapabilityPassReadsOnlyTheChangedSourcesInputs(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "capws-inputs")
	builderWriteTree(t, dir, capabilityWriteSetTree())
	store := builderOpenStore(t, "capws-inputs")
	builderIndex(t, store, dir)
	const file = "repo/pkg/state.go"
	prior := newCapabilityPrior()
	prior.files[file] = struct{}{}
	for _, node := range store.GetFileNodes(file) {
		prior.priorIDs[node.ID] = struct{}{}
		if node.Kind == graph.KindMethod {
			receiver, _ := node.Meta["receiver"].(string)
			prior.receivers[node.ID] = receiver
		}
		prior.writes[node.ID] = capabilityFieldWrites(store.GetOutEdges(node.ID))
	}
	want := capabilityRows(t, store)
	rec := &adjacencyRecordingStore{Store: store, out: map[string]struct{}{}, in: map[string]struct{}{}}
	synthesizeCapabilityEdgesForFilesWithPrior(rec, prior, []string{file})
	for id := range rec.out {
		if !strings.HasPrefix(id, file+"::") && id != file {
			t.Errorf("the capability pass read the adjacency of %s, outside the changed file", id)
		}
	}
	if len(rec.in) > 0 {
		t.Errorf("the capability pass walked receiver callers backwards from %d sources though no mutated-field set changed", len(rec.in))
	}
	if got := capabilityRows(t, store); strings.Join(got, "\n") != strings.Join(want, "\n") {
		onlyGot, onlyWant := diffRows(got, want)
		t.Fatalf("re-derivation changed the rows\nextra: %v\nmissing: %v", onlyGot, onlyWant)
	}
}

// A base whose capability rows an older derivation wrote (here: a write row
// the current derivation never produces) must not make every save of the
// file look like a change of its methods' mutated sets: the pass decides
// from the methods' inputs, so an edit that changes none of them re-derives
// only the file's own sources, and the stale row of a re-derived source is
// replaced.
func TestPerSaveCapabilityIgnoresRowsAnOlderDerivationWrote(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "capws-stale")
	builderWriteTree(t, dir, capabilityWriteSetTree())
	store := builderOpenStore(t, "capws-stale")
	builderIndex(t, store, dir)
	stale := &graph.Edge{From: "repo/pkg/state.go::Store.Bump", To: "repo/pkg/state.go::Store.mu",
		Kind: graph.EdgeAccessesField, FilePath: "repo/pkg/state.go", Line: 6, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"access": "write", "indirect": true, "via": "Lock"}}
	store.AddEdge(stale)
	core, logs := observer.New(zap.InfoLevel)
	idx := New(store, builderRegistry(), config.Default().Index, zap.New(core))
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	idx.rootPath = dir
	path := filepath.Join(dir, "pkg", "state.go")
	edited := capabilityWriteSetEdits["unrelated_body"]
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	before := logs.Len()
	if err := idx.IndexFile(path); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	rows := capabilityRows(t, store)
	own := 0
	for _, row := range rows {
		if strings.HasPrefix(strings.Split(row, "|")[1], "repo/pkg/state.go::") {
			own++
		}
		if strings.Contains(row, "Store.Bump|repo/pkg/state.go::Store.mu|") {
			t.Fatalf("the stale row survived the re-derivation of its source: %s", row)
		}
	}
	seen := false
	for _, entry := range logs.All()[before:] {
		if entry.Message != "incremental derived passes complete" {
			continue
		}
		seen = true
		if got, _ := entry.ContextMap()["capability_edges"].(int64); int(got) != own {
			t.Fatalf("a save over stale base rows re-derived %d capability rows, want the file's own %d (receiver callers were re-derived)", got, own)
		}
	}
	if !seen {
		t.Fatal("no derived pass ran")
	}
}
