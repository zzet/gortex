package graph

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

type declarationVisitorStub struct {
	*exactNamesVisitorStub
	declarations int
}

func (r *declarationVisitorStub) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*Node) bool) error {
	r.declarations++
	for _, n := range r.nodes {
		if !yield(n) {
			return ctx.Err()
		}
	}
	return r.err
}

// Unwrap must not let the optimized reader bypass this wrapper's error.
type guardedDeclarationReader struct {
	Reader
	cause error
}

func (r *guardedDeclarationReader) Unwrap() Reader { return r.Reader }
func (r *guardedDeclarationReader) VisitNodesByNamesContext(context.Context, []string, func(*Node) bool) error {
	return r.cause
}
func TestExactNameDeclarationsOverlayMasksAndCheckedFallback(t *testing.T) {
	layer := NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.MarkFile("deleted.go", true)
	layer.MarkRemoved("Old", "removed")
	layer.AddNode("covered.go", &Node{ID: "covered.go::new", Name: "New", FilePath: "covered.go"})
	layer.AddNode("", &Node{ID: "identity", Name: "New", FilePath: "outside.go"})
	base := &declarationVisitorStub{exactNamesVisitorStub: &exactNamesVisitorStub{nodes: []*Node{
		{ID: "covered.go::old", Name: "Old"}, {ID: "deleted.go::old", Name: "Old"}, {ID: "identity", Name: "Old"}, {ID: "removed", Name: "Old"}, {ID: "visible.go::old", Name: "Old"},
	}}}
	view := NewOverlaidViewWithLayer(base, layer)
	var got []string
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), view, []string{"New", "Old"}, func(n *Node) bool { got = append(got, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"covered.go::new", "identity", "visible.go::old"}) || base.declarations != 1 || base.calls != 0 {
		t.Fatalf("visible=%v declarations=%d full=%d", got, base.declarations, base.calls)
	}
	base.declarations = 0
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), view, []string{"New", "Old"}, func(*Node) bool { return false }); err != nil || base.declarations != 0 {
		t.Fatalf("stop=%v calls=%d", err, base.declarations)
	}
	cause := errors.New("guarded incomplete source")
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), &guardedDeclarationReader{Reader: base, cause: cause}, []string{"New", "Old"}, func(*Node) bool { t.Fatal("guard bypass"); return true }); !errors.Is(err, cause) {
		t.Fatalf("guard=%v", err)
	}
	broken := &contextualBatchFallbackLayer{OverlayLayer: NewOverlayLayer(), err: cause}
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), NewOverlaidViewWithLayer(base, broken), []string{"A", "B"}, func(*Node) bool { t.Fatal("partial source blessed"); return true }); !errors.Is(err, cause) {
		t.Fatalf("contextual layer=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := VisitNodeDeclarationsByNamesContext(ctx, base, []string{"A", "B"}, func(*Node) bool { cancel(); return true }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type guardedRepoDeclarationReader struct {
	*declarationVisitorStub
	cause error
}

func (r *guardedRepoDeclarationReader) VisitNodesByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error {
	return r.cause
}
func TestExactNameDeclarationsPreserveRepoWrapperRefusal(t *testing.T) {
	cause := errors.New("repo source unavailable")
	r := &guardedRepoDeclarationReader{declarationVisitorStub: &declarationVisitorStub{exactNamesVisitorStub: &exactNamesVisitorStub{}}, cause: cause}
	err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), r, []string{"A", "B"}, "repo", func(*Node) bool { t.Fatal("repo source bypassed"); return true })
	if !errors.Is(err, cause) || r.declarations != 0 {
		t.Fatalf("repo guard=%v optimizedcalls=%d", err, r.declarations)
	}
}

func TestDeltaWriterDeclarationFallbackPreservesOwnedNames(t *testing.T) {
	base := New()
	base.AddBatch([]*Node{
		{ID: "repo/covered.go::old", Name: "Match", RepoPrefix: "repo", FilePath: "repo/covered.go"},
		{ID: "repo/deleted.go::gone", Name: "Match", RepoPrefix: "repo", FilePath: "repo/deleted.go"},
		{ID: "repo/visible.go::kept", Name: "Match", RepoPrefix: "repo", FilePath: "repo/visible.go"},
		{ID: "other/out.go::foreign", Name: "Match", RepoPrefix: "other", FilePath: "other/out.go"},
	}, nil)
	delta := NewDeltaWriter(base, nil)
	delta.EvictFiles([]string{"repo/covered.go", "repo/deleted.go"})
	delta.AddNode(&Node{ID: "repo/covered.go::new", Name: "Match", RepoPrefix: "repo", FilePath: "repo/covered.go"})
	var ids []string
	if err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), delta, []string{"Match", "Missing"}, "repo", func(n *Node) bool { ids = append(ids, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"repo/covered.go::new", "repo/visible.go::kept"}) {
		t.Fatalf("delta declarations=%v", ids)
	}
	seen := 0
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), delta, []string{"Match", "Missing"}, func(*Node) bool { seen++; return false }); err != nil || seen != 1 {
		t.Fatalf("stop=%d err=%v", seen, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := VisitNodeDeclarationsByNamesInRepoContext(ctx, delta, []string{"Match"}, "repo", func(*Node) bool { t.Fatal("cancelled lookup visited a row"); return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

type scopedDeclarationGuardLayer struct {
	*OverlayLayer
	cause error
	broad int
}

func (l *scopedDeclarationGuardLayer) VisitNodesByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error {
	return l.cause
}
func (l *scopedDeclarationGuardLayer) VisitNodeDeclarationsByNamesContext(context.Context, []string, func(*Node) bool) error {
	l.broad++
	return nil
}

func TestExactNameDeclarationRepoChainKeepsMasksAndRefusal(t *testing.T) {
	base := New()
	base.AddBatch([]*Node{
		{ID: "repo/covered.go::old", Name: "Match", RepoPrefix: "repo", FilePath: "repo/covered.go"},
		{ID: "repo/deleted.go::old", Name: "Match", RepoPrefix: "repo", FilePath: "repo/deleted.go"},
		{ID: "repo/visible.go::kept", Name: "Match", RepoPrefix: "repo", FilePath: "repo/visible.go"},
		{ID: "foreign/file.go::other", Name: "Match", RepoPrefix: "foreign", FilePath: "foreign/file.go"},
	}, nil)
	layer := NewOverlayLayer()
	layer.MarkFile("repo/covered.go", false)
	layer.MarkFile("repo/deleted.go", true)
	layer.AddNode("repo/covered.go", &Node{ID: "repo/covered.go::new", Name: "Match", RepoPrefix: "repo", FilePath: "repo/covered.go"})
	view := NewOverlaidViewWithLayer(base, layer)
	var got []string
	if err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), view, []string{"Match", "Missing"}, "repo", func(n *Node) bool { got = append(got, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"repo/covered.go::new", "repo/visible.go::kept"}) {
		t.Fatalf("selected=%v", got)
	}
	seen := 0
	if err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), view, []string{"Match"}, "repo", func(*Node) bool { seen++; return false }); err != nil || seen != 1 {
		t.Fatalf("stop=%d err=%v", seen, err)
	}
	cause := errors.New("scoped layer unavailable")
	guard := &scopedDeclarationGuardLayer{OverlayLayer: NewOverlayLayer(), cause: cause}
	broken := NewOverlaidViewWithLayer(base, guard)
	if err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), broken, []string{"Match"}, "repo", func(*Node) bool { t.Fatal("guard bypassed"); return true }); !errors.Is(err, cause) || guard.broad != 0 {
		t.Fatalf("scope guard=%v broad=%d", err, guard.broad)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := VisitNodeDeclarationsByNamesInRepoContext(ctx, view, []string{"Match"}, "repo", func(*Node) bool { t.Fatal("cancelled lookup delivered row"); return true }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
