package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The primary checkout's per-save path re-derives capability edges only for the
// changed files, the sources whose edges into them the eviction deleted, and the
// receiver callers of methods whose mutated-field set changed. These tests pin
// that bound to its oracle: after each edit, every capability row
// (accesses_field, reads_env, executes_process) equals the row a whole index of
// the edited tree writes.

func perSaveCapabilityTree() map[string]string {
	return map[string]string{
		"pkg/state.go": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() { s.count++ }

func (s *Store) Name() string { return s.name }
`,
		"pkg/use.go": `package pkg

import (
	"os"
	"os/exec"
)

type Server struct {
	store *Store
	hits  int
}

func (sv *Server) Handle(k string) {
	sv.store.Put(k, 1)
	sv.hits++
}

func (sv *Server) Label() string { return sv.store.Name() }

func (sv *Server) Count() int { return sv.store.count }

func (sv *Server) Run() error { return exec.Command("true").Run() }

func Home() string { return os.Getenv("HOME") }
`,
	}
}

var perSaveCapabilityEdits = map[string]string{
	// A body edit that changes no method's mutated-field set.
	"body": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() { s.count++ }

func (s *Store) Name() string {
	n := s.name
	return n
}
`,
	// Name starts mutating the receiver: its callers' summaries change.
	"adds_mutation": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() { s.count++ }

func (s *Store) Name() string {
	s.count++
	return s.name
}
`,
	// bump stops mutating: Put keeps its direct writes, loses the indirect one.
	"removes_mutation": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() {}

func (s *Store) Name() string { return s.name }
`,
	// Every line moves: carried rows keep their line, re-derived rows move.
	"line_shift": `package pkg

// Store is the shared state.

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() { s.count++ }

func (s *Store) Name() string { return s.name }
`,
	// A field the other file reads changes type: its ID survives but its
	// contract does not, so the incoming reads are re-bound and the derived
	// access into it is re-derived rather than carried.
	"field_retyped": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	count int64
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() { s.count++ }

func (s *Store) Name() string { return s.name }
`,
	// A field the other file reads disappears (its ID does not survive).
	"field_removed": `package pkg

import "sync"

type Store struct {
	mu    sync.Mutex
	items map[string]int
	name  string
}

func (s *Store) Put(k string, v int) {
	s.mu.Lock()
	s.items[k] = v
	s.bump()
	s.mu.Unlock()
}

func (s *Store) bump() {}

func (s *Store) Name() string { return s.name }
`,
}

func capabilityRows(t *testing.T, store *store_sqlite.Store) []string {
	t.Helper()
	var rows []string
	for _, kind := range []graph.EdgeKind{graph.EdgeAccessesField, graph.EdgeReadsEnv, graph.EdgeExecutesProcess} {
		for edge := range store.EdgesByKind(kind) {
			meta, _ := json.Marshal(edge.Meta)
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s",
				edge.Kind, edge.From, edge.To, edge.FilePath, edge.Line, edge.Origin, meta))
		}
	}
	sort.Strings(rows)
	return rows
}

func diffRows(left, right []string) (onlyLeft, onlyRight []string) {
	seen := make(map[string]int, len(right))
	for _, row := range right {
		seen[row]++
	}
	for _, row := range left {
		if seen[row] > 0 {
			seen[row]--
			continue
		}
		onlyLeft = append(onlyLeft, row)
	}
	for row, n := range seen {
		for ; n > 0; n-- {
			onlyRight = append(onlyRight, row)
		}
	}
	sort.Strings(onlyRight)
	return onlyLeft, onlyRight
}

func TestPerSaveCapabilityMatchesAWholeIndex(t *testing.T) {
	builderIsolateGit(t)
	for name, edited := range perSaveCapabilityEdits {
		t.Run(name, func(t *testing.T) {
			dir := builderTempDir(t, "cap-"+name)
			builderWriteTree(t, dir, perSaveCapabilityTree())
			store := builderOpenStore(t, "cap-"+name)
			builderIndex(t, store, dir)
			core, logs := observer.New(zap.InfoLevel)
			idx := New(store, builderRegistry(), config.Default().Index, zap.New(core))
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			idx.rootPath = dir
			// A first save of the unchanged bytes stamps the per-save
			// fingerprints (a whole index does not), so the edit below takes
			// the fingerprint-classified path rather than the legacy one.
			path := filepath.Join(dir, "pkg", "state.go")
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
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("IndexFile: %v", err)
			}
			perSave := capabilityRows(t, store)
			if name == "body" {
				// Nothing outside the file changed: the pass re-derives only
				// the rows the file's own nodes source.
				own := 0
				for _, row := range perSave {
					if strings.Contains(row, "|repo/pkg/state.go::") && strings.Split(row, "|")[1] != "" &&
						strings.HasPrefix(strings.Split(row, "|")[1], "repo/pkg/state.go::") {
						own++
					}
				}
				for _, entry := range logs.All()[before:] {
					if entry.Message != "incremental derived passes complete" {
						continue
					}
					if got, _ := entry.ContextMap()["capability_edges"].(int64); int(got) != own {
						t.Fatalf("body edit re-derived %d capability rows, want the file's own %d", got, own)
					}
				}
			}

			cleanDir := builderTempDir(t, "cap-clean-"+name)
			tree := perSaveCapabilityTree()
			tree["pkg/state.go"] = edited
			builderWriteTree(t, cleanDir, tree)
			clean := builderOpenStore(t, "cap-clean-"+name)
			builderIndex(t, clean, cleanDir)
			whole := capabilityRows(t, clean)
			// The whole index ran from another directory: compare
			// repository-relative rows (both stores use the same prefix).
			onlyPerSave, onlyWhole := diffRows(perSave, whole)
			if len(onlyPerSave) > 0 || len(onlyWhole) > 0 {
				t.Fatalf("capability rows differ from a whole index of the edited tree\nper-save only:\n  %s\nwhole-index only:\n  %s",
					strings.Join(onlyPerSave, "\n  "), strings.Join(onlyWhole, "\n  "))
			}
			if len(whole) == 0 {
				t.Fatal("the fixture produced no capability rows")
			}
		})
	}
}

// A reparse evicts and re-adds every declaration of the file, so a complete
// receipt names all of them as evicted; only the names the definition files no
// longer declare are reachable by name alone.
func TestVanishedReceiptNamesKeepsOnlyUndeclaredNames(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go"},
		{ID: "repo/a.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "repo/a.go"},
		{ID: "repo/a.go::T.M", Kind: graph.KindMethod, Name: "M", QualName: "T.M", FilePath: "repo/a.go"},
	}, nil)
	receipt := &graph.MutationReceipt{
		Complete:        true,
		DefinitionFiles: []string{"repo/a.go"},
		EvictedNames:    []string{"Gone", "Keep", "M", "T.M", "T.Gone"},
	}
	got := vanishedReceiptNames(g, receipt)
	if fmt.Sprint(got) != fmt.Sprint([]string{"Gone", "T.Gone"}) {
		t.Fatalf("vanished names = %v, want [Gone T.Gone]", got)
	}
	receipt.DefinitionFiles = nil
	if got := vanishedReceiptNames(g, receipt); len(got) != len(receipt.EvictedNames) {
		t.Fatalf("without definition files every evicted name must stay: %v", got)
	}
}

// A scoped pass reports the last whole-repository count instead of recounting;
// a whole-root pass recounts.
func TestScopedIncrementalPassReusesTheRepositoryCount(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "counts")
	builderWriteTree(t, dir, perSaveCapabilityTree())
	store := builderOpenStore(t, "counts")
	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	full, err := idx.Index(dir)
	if err != nil {
		t.Fatal(err)
	}
	if full.CountsCached {
		t.Fatal("a whole index reported cached counts")
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "state.go"), []byte(perSaveCapabilityEdits["adds_mutation"]), 0o644); err != nil {
		t.Fatal(err)
	}
	scoped, err := idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "pkg", "state.go")})
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.CountsCached || scoped.NodeCount != full.NodeCount || scoped.EdgeCount != full.EdgeCount {
		t.Fatalf("scoped pass counts = %d/%d cached=%v, want the whole index's %d/%d cached",
			scoped.NodeCount, scoped.EdgeCount, scoped.CountsCached, full.NodeCount, full.EdgeCount)
	}
	whole, err := idx.IncrementalReindexPaths(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if whole.CountsCached {
		t.Fatal("a whole-root pass reported cached counts")
	}
}

// A source outside the changed files whose field access into them the eviction
// deleted (the prior's restoration set) is re-derived from its own, intact
// read/write edges.
func TestCapabilityPriorRestoresDeletedIncomingAccesses(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "cap-restore")
	builderWriteTree(t, dir, perSaveCapabilityTree())
	store := builderOpenStore(t, "cap-restore")
	builderIndex(t, store, dir)
	const source = "repo/pkg/use.go::Server.Count"
	var deleted []*graph.Edge
	for _, edge := range store.GetOutEdges(source) {
		if edge.Kind == graph.EdgeAccessesField {
			deleted = append(deleted, edge)
		}
	}
	if len(deleted) == 0 {
		t.Fatal("fixture: Server.Count has no field access")
	}
	want := capabilityRows(t, store)
	graph.RemoveEdgesExact(store, deleted)
	prior := newCapabilityPrior()
	prior.files["repo/pkg/state.go"] = struct{}{}
	for _, node := range store.GetFileNodes("repo/pkg/state.go") {
		prior.priorIDs[node.ID] = struct{}{}
		if node.Kind == graph.KindMethod {
			receiver, _ := node.Meta["receiver"].(string)
			prior.receivers[node.ID] = receiver
		}
		writes := map[string]struct{}{}
		for _, edge := range store.GetOutEdges(node.ID) {
			if edge.Kind == graph.EdgeAccessesField && capabilityAccessIsWrite(edge) {
				writes[edge.To] = struct{}{}
			}
		}
		prior.writes[node.ID] = writes
	}
	prior.restoration[source] = map[string]struct{}{}
	synthesizeCapabilityEdgesForFilesWithPrior(store, prior, []string{"repo/pkg/state.go"})
	if got := capabilityRows(t, store); fmt.Sprint(got) != fmt.Sprint(want) {
		onlyGot, onlyWant := diffRows(got, want)
		t.Fatalf("restoration did not re-derive the deleted access\nextra: %v\nmissing: %v", onlyGot, onlyWant)
	}
}
