package indexer

import (
	"context"
	"strings"
	"testing"
)

// A body-only edit changes no declaration a referrer can bind to, so the
// affected-by plan finds nothing to re-resolve — even in a file whose
// declarations share names (methods of one name on two receivers, locals of
// one name in several functions), where each key has several nodes.
func TestEditDeltaBodyOnlyEditAffectsNoReferrer(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "affected-body")
	repoDir := builderTempDir(t, "checkout-affected-body")
	src := `package store

import "errors"

type A struct{}
type B struct{}

func (a *A) Evict(path string) (int, error) {
	n, err := a.count(path)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (b *B) Evict(path string) (int, error) {
	out, err := b.count(path)
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (a *A) count(path string) (int, error) {
	if path == "" {
		err := errors.New("empty")
		return 0, err
	}
	return len(path), nil
}

func (b *B) count(path string) (int, error) { return len(path), nil }
`
	caller := `package store

func Use(a *A, b *B) int {
	x, _ := a.Evict("p")
	y, _ := b.Evict("q")
	return x + y
}
`
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, map[string]string{"go.mod": "module example.com/s\n\ngo 1.22\n", "store/store.go": src, "store/use.go": caller})
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	edited := strings.Replace(src, "\tn, err := a.count(path)\n", "\t_ = 1\n\t_ = 2\n\tn, err := a.count(path)\n", 1)
	builderWriteFile(t, repoDir, "store/store.go", edited)
	req := h.request()
	req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{int64(1) << 40}}
	recordLastEditDelta(nil)
	if _, _, err := builder.BuildDirtyLayer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	delta := LastEditDeltaReport()
	if delta == nil {
		t.Fatal("no edit delta")
	}
	if delta.AffectedByKeys != 0 || delta.AffectedByFiles != 0 {
		t.Fatalf("a body-only edit changed %d declaration keys %q and re-resolved %d files", delta.AffectedByKeys, delta.AffectedByKeySample, delta.AffectedByFiles)
	}
}
