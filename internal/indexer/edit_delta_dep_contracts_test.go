package indexer

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// countingContractStore counts the contract-identity reads.
type countingContractStore struct {
	graph.Store
	reads int
}

func (s *countingContractStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	s.reads++
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

// The dependency contracts a delta's resolver reads are kept per stack and
// repository: two deltas over the stack read them once, both see the stack's
// dep:: rows, and a change set holding a module manifest installs no source
// (the delta reads its own view).
func TestEditDeltaDepContractsAreKeptPerStack(t *testing.T) {
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaDeps)
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "dep::example.com/lib", Kind: graph.KindContract, Name: "lib", RepoPrefix: "repo"},
		{ID: "env::HOME", Kind: graph.KindContract, Name: "HOME", RepoPrefix: "repo"},
	}, nil)
	store := &countingContractStore{Store: g}
	var got []string
	for delta := 0; delta < 2; delta++ {
		idx := &Indexer{repoPrefix: "repo", graph: store, resolver: resolver.New(store)}
		release := installEditDeltaDeps(idx, "stack", []string{"a/a.go"}, nil)
		got = got[:0]
		for row := range editDeltaDepSourceForTest(idx)([]string{"repo"}) {
			got = append(got, row.ID)
		}
		release()
		if editDeltaDepSourceForTest(idx) != nil {
			t.Fatalf("delta %d: the released source is still recorded", delta)
		}
	}
	if fmt.Sprint(got) != "[dep::example.com/lib]" {
		t.Fatalf("dependency contracts = %v", got)
	}
	if store.reads != 1 {
		t.Fatalf("two deltas read the contracts %d times, want once", store.reads)
	}
	idx := &Indexer{repoPrefix: "repo", graph: store, resolver: resolver.New(store)}
	release := installEditDeltaDeps(idx, "stack", []string{"go.mod"}, nil)
	defer release()
	if editDeltaDepSourceForTest(idx) != nil {
		t.Fatal("a change set holding go.mod installed the per-stack source")
	}
}

func editDeltaDepSourceForTest(idx *Indexer) func([]string) iter.Seq[graph.RepoNodeIdentity] {
	source, ok := editDeltaDepSources.Load(idx.resolver)
	if !ok {
		return nil
	}
	return source.(func([]string) iter.Seq[graph.RepoNodeIdentity])
}

func editDeltaDepSourceCount() int {
	n := 0
	editDeltaDepSources.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// A working-tree delta records its resolver's dependency-contract source only
// while it runs: the record holds the delta's resolver and view, so a record
// kept past the delta keeps the whole delta live. Each delta's production log
// line sees exactly its own record, and none is left once the delta ends.
func TestEditDeltaDepSourceIsReleasedWhenTheDeltaEnds(t *testing.T) {
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaDeps)
	builderIsolateGit(t)
	store := builderOpenStore(t, "dep-source-release")
	repoDir := builderTempDir(t, "checkout-dep-source-release")
	src := "package store\n\nfunc Count(path string) int {\n\treturn len(path)\n}\n"
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, map[string]string{"go.mod": "module example.com/s\n\ngo 1.22\n", "store/store.go": src})
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	during := -1
	core, _ := observer.New(zap.InfoLevel)
	builder.Logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "indexer: working-tree edit delta" {
			during = editDeltaDepSourceCount()
		}
		return nil
	}))
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	for delta := 1; delta <= 3; delta++ {
		builderWriteFile(t, repoDir, "store/store.go", strings.Replace(src, "return len(path)", fmt.Sprintf("n := len(path) + %d\n\treturn n", delta), 1))
		req := h.request()
		req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{int64(1) << 40}}
		recordLastEditDelta(nil)
		during = -1
		if _, _, err := builder.BuildDirtyLayer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if LastEditDeltaReport() == nil {
			t.Fatalf("delta %d: the edit was not built as a delta", delta)
		}
		if during != 1 {
			t.Fatalf("delta %d: %d dependency-contract sources recorded while the delta ran, want its own one", delta, during)
		}
		if after := editDeltaDepSourceCount(); after != 0 {
			t.Fatalf("delta %d: %d dependency-contract sources recorded after the delta ended, want none", delta, after)
		}
	}
}
