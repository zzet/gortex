package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type recordingContentReplacer struct {
	err   error
	calls [][]graph.ContentFTSFileReplacement
}

type failingCheckedContentGraph struct {
	graph.Store
	err          error
	checkedCalls int
	legacyCalls  int
}

func (s *failingCheckedContentGraph) AddBatch([]*graph.Node, []*graph.Edge) {
	s.legacyCalls++
}

func (s *failingCheckedContentGraph) AddBatchChecked([]*graph.Node, []*graph.Edge) error {
	s.checkedCalls++
	return s.err
}

func (r *recordingContentReplacer) WipeContent(string) error     { return nil }
func (r *recordingContentReplacer) WipeContentFile(string) error { return nil }
func (r *recordingContentReplacer) AppendContent(string, []graph.ContentFTSItem) error {
	return nil
}
func (r *recordingContentReplacer) SearchContent(string, string, int) ([]graph.ContentHit, error) {
	return nil, nil
}
func (r *recordingContentReplacer) BuildContentIndex() error { return nil }
func (r *recordingContentReplacer) ScanContent(string, func(string, string, string) bool) error {
	return nil
}
func (r *recordingContentReplacer) ReplaceContentFiles(_ string, files []graph.ContentFTSFileReplacement) error {
	copyFiles := make([]graph.ContentFTSFileReplacement, len(files))
	for i, file := range files {
		copyFiles[i] = file
		copyFiles[i].Items = append([]graph.ContentFTSItem(nil), file.Items...)
	}
	r.calls = append(r.calls, copyFiles)
	return r.err
}

func testContentNode(id, filePath, body string) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindDoc, FilePath: filePath,
		Meta: map[string]any{"data_class": "content", "section_text": body},
	}
}

func TestParseContentBatchCommitsOneSidecarAndOneGraphBatch(t *testing.T) {
	sink := &recordingContentReplacer{}
	g := graph.New()
	idx := &Indexer{graph: g, contentSink: sink, repoPrefix: "repo", logger: zap.NewNop()}
	batch := newParseContentBatch(idx)
	require.NotNil(t, batch)

	bodyA := "head " + string(make([]byte, contentSnippetCap+50)) + " tail-a"
	bodyB := "head " + string(make([]byte, contentSnippetCap+50)) + " tail-b"
	nodeA := testContentNode("repo/a.md::0", "a.md", bodyA)
	nodeB := testContentNode("repo/b.md::0", "b.md", bodyB)
	durable := 0
	require.True(t, batch.add([]*graph.Node{nodeA}, nil, func() { durable++ }))
	require.True(t, batch.add([]*graph.Node{nodeB}, nil, func() { durable++ }))
	require.Zero(t, g.NodeCount(), "content graph rows wait for their sidecar commit")
	require.Zero(t, durable)

	batch.flush()
	require.Len(t, sink.calls, 1)
	require.Len(t, sink.calls[0], 2)
	require.Equal(t, bodyA, sink.calls[0][0].Items[0].Body)
	require.Equal(t, 2, durable)
	require.Equal(t, 2, g.NodeCount())
	stored := g.GetNode(nodeA.ID)
	require.NotNil(t, stored)
	require.LessOrEqual(t, len(contentBody(stored)), contentSnippetCap)
	require.Equal(t, true, stored.Meta["content_indexed"])
	// Worker-owned extractor results are never mutated by a concurrent flush.
	require.Equal(t, bodyA, contentBody(nodeA))
}

func TestParseContentBatchRetainsFullGraphBodyWhenSidecarFails(t *testing.T) {
	sink := &recordingContentReplacer{err: errors.New("injected replacement failure")}
	g := graph.New()
	idx := &Indexer{graph: g, contentSink: sink, repoPrefix: "repo", logger: zap.NewNop()}
	batch := newParseContentBatch(idx)
	body := "full fallback body " + string(make([]byte, contentSnippetCap+50))
	node := testContentNode("repo/a.md::0", "a.md", body)
	require.True(t, batch.add([]*graph.Node{node}, nil, nil))
	batch.flush()
	require.Equal(t, body, contentBody(g.GetNode(node.ID)))
	require.NotEqual(t, true, g.GetNode(node.ID).Meta["content_indexed"])
}

func TestParseContentBatchGraphFailureSuppressesDurabilityCallbacks(t *testing.T) {
	wantErr := errors.New("graph write failed")
	sink := &recordingContentReplacer{}
	store := &failingCheckedContentGraph{Store: graph.New(), err: wantErr}
	idx := &Indexer{graph: store, contentSink: sink, repoPrefix: "repo", logger: zap.NewNop()}
	batch := newParseContentBatch(idx)
	body := "full body " + string(make([]byte, contentSnippetCap+50))
	callbacks := 0
	require.True(t, batch.add(
		[]*graph.Node{testContentNode("repo/a.md::0", "a.md", body)}, nil,
		func() { callbacks++ },
	))
	require.ErrorIs(t, batch.flush(), wantErr)
	require.Len(t, sink.calls, 1)
	require.Equal(t, 1, store.checkedCalls)
	require.Zero(t, store.legacyCalls)
	require.Zero(t, callbacks)
	require.Zero(t, store.NodeCount())
	require.Empty(t, batch.pending)
	require.Zero(t, batch.itemCount)
	require.Zero(t, batch.bodyBytes)

	require.True(t, batch.add(
		[]*graph.Node{testContentNode("repo/b.md::0", "b.md", body)}, nil,
		func() { callbacks++ },
	))
	require.ErrorIs(t, batch.flush(), wantErr)
	require.Len(t, sink.calls, 1, "sticky graph error must not repeat the sidecar write")
	require.Equal(t, 1, store.checkedCalls)
	require.Zero(t, callbacks)
}

func TestReplaceContentSectionsSendsAuthoritativeEmptyFile(t *testing.T) {
	sink := &recordingContentReplacer{}
	idx := &Indexer{graph: graph.New(), contentSink: sink, repoPrefix: "repo", logger: zap.NewNop()}
	require.True(t, idx.replaceContentSections("emptied.md", nil, false))
	require.Len(t, sink.calls, 1)
	require.Equal(t, "emptied.md", sink.calls[0][0].FilePath)
	require.Empty(t, sink.calls[0][0].Items)
}
