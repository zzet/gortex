package query

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

type searchContextEmptyBackend struct{}

func (*searchContextEmptyBackend) Add(string, ...string) {}
func (*searchContextEmptyBackend) Remove(string)         {}
func (*searchContextEmptyBackend) Search(string, int) []search.SearchResult {
	return []search.SearchResult{}
}
func (*searchContextEmptyBackend) Count() int { return 1 }
func (*searchContextEmptyBackend) Close()     {}

type searchContextReader struct {
	graph.Reader
	exact             []*graph.Node
	substring         []*graph.Node
	exactErr          error
	substringErr      error
	exactContextCalls atomic.Int32
	subContextCalls   atomic.Int32
	exactLegacyCalls  atomic.Int32
	subLegacyCalls    atomic.Int32
}

func (r *searchContextReader) GetNodesByIDs([]string) map[string]*graph.Node {
	return map[string]*graph.Node{}
}

func (r *searchContextReader) FindNodesByName(string) []*graph.Node {
	r.exactLegacyCalls.Add(1)
	return r.exact
}

func (r *searchContextReader) FindNodesByNameContaining(string, int) []*graph.Node {
	r.subLegacyCalls.Add(1)
	return r.substring
}

func (r *searchContextReader) FindNodesByNameContext(context.Context, string) ([]*graph.Node, error) {
	r.exactContextCalls.Add(1)
	return r.exact, r.exactErr
}

func (r *searchContextReader) FindNodesByNameContainingContext(context.Context, string, int) ([]*graph.Node, error) {
	r.subContextCalls.Add(1)
	return r.substring, r.substringErr
}

func newSearchContextEngine(r graph.Reader) *Engine {
	eng := NewEngine(graph.New()).WithReader(r)
	eng.SetSearch(&searchContextEmptyBackend{})
	eng.SetRerank(nil)
	return eng
}

func searchContextCandidateIDs(in []*rerank.Candidate) []string {
	out := make([]string, 0, len(in))
	for _, candidate := range in {
		if candidate != nil && candidate.Node != nil {
			out = append(out, candidate.Node.ID)
		}
	}
	return out
}

func TestGatherBackendCandidatesUsesContextNameLookupsAndKeepsNonContextPartial(t *testing.T) {
	reader := &searchContextReader{
		exact: []*graph.Node{{ID: "exact", Name: "needle", Kind: graph.KindFunction}},
		substring: []*graph.Node{
			{ID: "z", Name: "needle-z", Kind: graph.KindMethod},
			{ID: "a", Name: "needle-a", Kind: graph.KindFunction},
			{ID: "file", Name: "needle.go", Kind: graph.KindFile},
		},
		substringErr: errors.New("best effort backend failure"),
	}
	got := newSearchContextEngine(reader).gatherBackendCandidates(context.Background(), "needle", 4, QueryOptions{SkipVectorChannel: true}, nil)
	want := []string{"exact", "a", "z"}
	if ids := searchContextCandidateIDs(got); len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("candidate IDs = %v, want %v", ids, want)
	}
	if reader.exactContextCalls.Load() != 1 || reader.subContextCalls.Load() != 1 {
		t.Fatalf("context calls exact/sub = %d/%d, want 1/1", reader.exactContextCalls.Load(), reader.subContextCalls.Load())
	}
	if reader.exactLegacyCalls.Load()+reader.subLegacyCalls.Load() != 0 {
		t.Fatalf("legacy name lookups = %d/%d", reader.exactLegacyCalls.Load(), reader.subLegacyCalls.Load())
	}
}

func TestGatherBackendCandidatesDiscardsPartialOnContextError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exact      []*graph.Node
		exactErr   error
		substring  []*graph.Node
		subErr     error
		wantSubRun int32
	}{
		{name: "exact", exact: []*graph.Node{{ID: "partial", Kind: graph.KindFunction}}, exactErr: context.Canceled, wantSubRun: 0},
		{name: "substring", substring: []*graph.Node{{ID: "partial", Kind: graph.KindFunction}}, subErr: errors.Join(errors.New("wrapped"), context.DeadlineExceeded), wantSubRun: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &searchContextReader{exact: tc.exact, exactErr: tc.exactErr, substring: tc.substring, substringErr: tc.subErr}
			got := newSearchContextEngine(reader).gatherBackendCandidates(context.Background(), "needle", 4, QueryOptions{SkipVectorChannel: true}, nil)
			if got != nil {
				t.Fatalf("canceled lookup published partial candidates: %v", searchContextCandidateIDs(got))
			}
			if reader.subContextCalls.Load() != tc.wantSubRun {
				t.Fatalf("substring calls = %d, want %d", reader.subContextCalls.Load(), tc.wantSubRun)
			}
			if reader.exactLegacyCalls.Load()+reader.subLegacyCalls.Load() != 0 {
				t.Fatal("context error fell back to a legacy lookup")
			}
		})
	}
}
