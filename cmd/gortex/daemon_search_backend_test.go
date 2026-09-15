package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// fakeSymbolSearcher is a minimal graph.SymbolSearcher stand-in so
// resolveSearchBackend's SymbolSearcherBackend branch can be exercised
// without a real sqlite store.
type fakeSymbolSearcher struct{}

func (fakeSymbolSearcher) UpsertSymbolFTS(string, string) error                    { return nil }
func (fakeSymbolSearcher) BulkUpsertSymbolFTS(string, []graph.SymbolFTSItem) error { return nil }
func (fakeSymbolSearcher) BuildSymbolIndex() error                                 { return nil }
func (fakeSymbolSearcher) SearchSymbols(string, int) ([]graph.SymbolHit, error)    { return nil, nil }

// countingSymbolSearcher is a store that can answer the authoritative
// document count, as the real SQLite store does.
type countingSymbolSearcher struct {
	fakeSymbolSearcher
	count int
}

func (c countingSymbolSearcher) SymbolFTSCount() (int, error) { return c.count, nil }

func TestResolveSearchBackend_SymbolSearcherBackend(t *testing.T) {
	b := search.NewSymbolSearcherBackend(fakeSymbolSearcher{})
	b.Add("node-1")
	b.Add("node-2")

	info := resolveSearchBackend(b)

	assert.Equal(t, "sqlite-fts5", info.Name)
	assert.True(t, info.DiskResident, "the FTS5 index lives inside the graph store, not in-process heap")
	assert.Zero(t, info.Bytes, "no fabricated byte count for a disk-resident backend")
	// Add and Remove are no-ops because the native store owns the corpus.
	// A store that cannot answer the authoritative count must leave the figure
	// unreported rather than fabricate an adapter-local document count.
	assert.False(t, info.DocCountKnown, "an unavailable count must remain unknown")
	assert.Zero(t, info.DocCount)
}

func TestResolveSearchBackend_SymbolSearcherBackend_CountFromIndex(t *testing.T) {
	// Adds and removes are no-ops. Count and status both report the native
	// index's authoritative corpus size without duplicating write-path deltas.
	b := search.NewSymbolSearcherBackend(countingSymbolSearcher{count: 48572})
	b.Add("node-1")
	b.Remove("node-2")
	b.Remove("node-3")
	assert.Equal(t, 48572, b.Count(), "Count must come from the native index")

	info := resolveSearchBackend(b)

	assert.True(t, info.DocCountKnown)
	assert.Equal(t, 48572, info.DocCount, "DocCount must come from the index, not the adapter delta")
}

func TestResolveSearchBackend_SymbolSearcherBackend_ThroughSwappable(t *testing.T) {
	b := search.NewSymbolSearcherBackend(fakeSymbolSearcher{})
	sw := search.NewSwappable(b)

	info := resolveSearchBackend(sw)

	assert.Equal(t, "sqlite-fts5", info.Name)
	assert.True(t, info.DiskResident)
}

func TestResolveSearchBackend_NullBackend(t *testing.T) {
	// A store with no native symbol search carries the null text backend.
	// Status must name it, not report it as an unrecognised backend: the
	// difference between "nothing is indexing text here" and "we could not
	// identify what is" is exactly what a user reads this row for.
	info := resolveSearchBackend(search.NewNull())

	assert.Equal(t, "none", info.Name)
	assert.False(t, info.DiskResident, "there is no index on disk either")
	assert.Zero(t, info.Bytes)
	assert.True(t, info.DocCountKnown, "zero documents is a known count, not an unanswerable one")
	assert.Zero(t, info.DocCount)
}

func TestRenderDaemonHeader_SearchBackendRow_NullBackend(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{Name: "none", DocCountKnown: true}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	assert.Contains(t, buf.String(), "none  docs=0  heap=0 B",
		"an empty backend still gets a row, with its zeros stated plainly")
}

func TestRenderDaemonHeader_SearchBackendRow_SymbolSearcher(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{
		Name:          "sqlite-fts5",
		DocCount:      48572,
		DocCountKnown: true,
		DiskResident:  true,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5")
	assert.Contains(t, out, "48572")
	assert.Contains(t, out, "disk-resident")
	assert.NotContains(t, out, "heap=0 B", "must not print a fabricated zero heap size")
}

// stubVectorDelegate backs a delegated vector backend for status tests; the
// resolver never queries it, it only needs to be non-nil.
type stubVectorDelegate struct{}

func (stubVectorDelegate) SimilarTo([]float32, int) ([]graph.VectorHit, error) { return nil, nil }

// statusEmbedder satisfies embedding.Provider for hybrid construction.
type statusEmbedder struct{}

func (statusEmbedder) Embed(context.Context, string) ([]float32, error) { return []float32{1, 0, 0}, nil }
func (statusEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (statusEmbedder) Dimensions() int { return 3 }
func (statusEmbedder) Close() error     { return nil }

func TestResolveSearchBackend_ReportsHybridVectorChannel(t *testing.T) {
	// #790 triage failure: the status row names only the peeled text backend,
	// so a live hybrid is indistinguishable from a text-only daemon. The
	// resolver must disclose the vector channel and its corpus size.
	text := search.NewSymbolSearcherBackend(countingSymbolSearcher{count: 48572})
	vector := search.NewDelegatedVector(3, stubVectorDelegate{}, 298050, 6269)
	sw := search.NewSwappable(search.NewHybrid(text, vector, statusEmbedder{}))

	raw, err := json.Marshal(resolveSearchBackend(sw))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"hybrid":true`,
		"a live hybrid must be visible in status")
	assert.Contains(t, string(raw), `"vector_count":298050`,
		"the restored corpus size must be visible in status")
}

func TestRenderDaemonHeader_SearchBackendRow_ShowsHybridVectors(t *testing.T) {
	// Round-trip the resolver output through JSON into the stats struct the
	// renderer consumes, exercising the real populate → render pipeline.
	text := search.NewSymbolSearcherBackend(countingSymbolSearcher{count: 328627})
	vector := search.NewDelegatedVector(3, stubVectorDelegate{}, 298050, 6269)
	raw, err := json.Marshal(resolveSearchBackend(
		search.NewSwappable(search.NewHybrid(text, vector, statusEmbedder{}))))
	require.NoError(t, err)

	var sb daemon.SearchBackendStats
	require.NoError(t, json.Unmarshal(raw, &sb))

	st := sampleStatus()
	st.SearchBackend = sb
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5", "the row still names the text backend")
	assert.Contains(t, out, "vectors=298050",
		"the status row must show the live vector channel")
}

func TestRenderDaemonHeader_OmitsUnknownDocCount(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{
		Name:         "sqlite-fts5",
		DiskResident: true,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5")
	assert.Contains(t, out, "disk-resident")
	assert.NotContains(t, out, "docs=",
		"a backend with no real count must omit the figure, not print docs=0 or a delta")
}
