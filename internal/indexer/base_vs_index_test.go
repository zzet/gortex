package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// The committed-base build and the whole index, over the same source and
// without the type checker in either, must give the same returns_to and
// implements rows. On the real store they did not: the base recorded a
// function returning into its own local and no interface satisfaction.
func TestBaseBuildAndWholeIndexAgreeOnReturnsToAndImplements(t *testing.T) {
	builderIsolateGit(t)
	repoDir := builderTempDir(t, "base-vs-index")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, map[string]string{
		"go.mod": "module example.com/s\n\ngo 1.22\n",
		"api/api.go": `package api

type GenerationStore interface{ Generation() int64 }
`,
		"s/load.go": `package s

func load() (int, error) { return 1, nil }
`,
		"s/store.go": `package s

type Reader interface{ Read() error }

type Store struct{ n int }

func (s *Store) Read() error { return nil }

func (s *Store) Generation() int64 { return int64(s.n) }

func Open() (*Store, error) {
	n, err := load()
	if err != nil {
		return nil, err
	}
	var errs []string
	errs = append(errs, "x")
	_ = errs
	return &Store{n: n}, nil
}
`,
	})
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	file := builderGraphPath(builderRepoPrefix, "s/store.go")
	rows := func(r graph.Reader) map[string][]string {
		out := map[string][]string{}
		for _, n := range r.GetFileNodes(file) {
			for _, e := range r.GetOutEdges(n.ID) {
				if e.Kind == graph.EdgeReturnsTo || e.Kind == graph.EdgeImplements {
					out[string(e.Kind)] = append(out[string(e.Kind)], e.From+">"+e.To)
				}
			}
		}
		for k := range out {
			sort.Strings(out[k])
		}
		return out
	}
	whole := builderOpenStore(t, "base-vs-index-whole")
	builderIndex(t, whole, repoDir)
	storePath := filepath.Join(t.TempDir(), "store.sqlite")
	baseID := editDeltaRealDedicatedBase(t, repoDir, storePath, config.Default().Index, zap.NewNop())
	store := builderOpenStoreAt(t, storePath)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	m := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()}
	view, err := m.MaterializeRefView(context.Background(), editDeltaRealDedicatedGraph, baseID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	w, b := rows(whole), rows(view.Reader)
	for _, kind := range []string{string(graph.EdgeReturnsTo), string(graph.EdgeImplements)} {
		t.Logf("%s whole index: %v", kind, w[kind])
		t.Logf("%s base build:  %v", kind, b[kind])
		if strings.Join(w[kind], ",") != strings.Join(b[kind], ",") {
			t.Errorf("%s: the base build and the whole index differ", kind)
		}
	}
}
