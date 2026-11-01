package mcp

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// The registry refuses a publish over either cap and never evicts a live
// entry to make room: an evicted entry would silently serve stale code. The
// byte cap is the feature's whole memory allowance; file sizes vary too much
// for the file cap alone to bound memory.
const (
	projectionMaxFiles = 512
	projectionMaxBytes = 64 << 20

	// Retained heap of one projected node and edge, measured with
	// BenchmarkProjectionRetainedHeap, for the byte cap.
	projectionNodeBytes = 1440
	projectionEdgeBytes = 540
)

// pendingFileProjections holds the projection of every file a tool wrote
// whose bytes the store has not caught up with yet. Like the mutation path
// locks it is process-wide and keyed by pathkey.MutationKey.
var pendingFileProjections = newPendingProjections()

// pendingProjections is a copy-on-write map from a file's mutation key to the
// projection of its latest tool-written bytes. Readers load the snapshot
// without locking; publishers hold mu only to copy the map and swap it, never
// while building an entry.
type pendingProjections struct {
	enabled bool

	mu      sync.Mutex
	snap    atomic.Pointer[projectionSnapshot]
	refused map[projectionRefusal]int64 // guarded by mu

	published      atomic.Int64
	retired        atomic.Int64
	externalWrites atomic.Int64
	divergence     atomic.Int64
}

type projectionSnapshot struct {
	entries map[string]*projectionEntry
	bytes   int64
}

// projectionEntry is one file's projection. Everything but suspect is
// immutable once published.
type projectionEntry struct {
	key        string
	absPath    string
	graphPath  string
	repoPrefix string
	commitSeq  uint64
	// hash equals the store's FileMetaRow.ContentHash once the store holds
	// these bytes; rawHash covers the bytes on disk for the suspect re-check.
	hash        string
	rawHash     [32]byte
	disk        diskStamp
	language    string
	layer       *graph.OverlayLayer
	parseErrors int
	buildTime   time.Duration
	size        int64
	tombstone   bool
	// suspect is set by a watcher event for the path; the next lookup
	// re-reads the file before serving the entry.
	suspect atomic.Bool
}

// projectionRefusal is why a publish left the live entries as they were.
type projectionRefusal string

const (
	projectionDisabled   projectionRefusal = "disabled"
	projectionPathCase   projectionRefusal = "path_case"
	projectionSuperseded projectionRefusal = "superseded"
	projectionOverFiles  projectionRefusal = "max_files"
	projectionOverBytes  projectionRefusal = "max_bytes"
)

func (r projectionRefusal) Error() string { return "projection not published: " + string(r) }

// errProjectionExternalWrite reports that the file no longer holds the bytes
// its entry was built from. The entry is dropped; the store answers instead.
var errProjectionExternalWrite = errors.New("projection: file changed on disk since it was projected")

func newPendingProjections() *pendingProjections {
	p := &pendingProjections{
		enabled: os.Getenv("GORTEX_PROJECTION") != "0",
		refused: make(map[projectionRefusal]int64),
	}
	p.snap.Store(&projectionSnapshot{entries: map[string]*projectionEntry{}})
	return p
}

// diskStamp is what a lookup compares to notice a write the projection did
// not see: size, modification time and file identity. A nil info stamps a
// file that does not exist.
type diskStamp struct {
	info os.FileInfo
}

// statDiskStamp stamps path without following a leaf symlink, matching
// pathkey.MutationKey.
func statDiskStamp(path string) (diskStamp, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return diskStamp{}, nil
	}
	if err != nil {
		return diskStamp{}, err
	}
	return diskStamp{info: info}, nil
}

func (s diskStamp) matches(other diskStamp) bool {
	if s.info == nil || other.info == nil {
		return s.info == nil && other.info == nil
	}
	return s.info.Size() == other.info.Size() &&
		s.info.ModTime().Equal(other.info.ModTime()) &&
		os.SameFile(s.info, other.info)
}

// projectFile builds the entry for raw, the bytes a tool wrote to absPath as
// commit commitSeq, and disk, the stamp of that write. It reads no store
// state.
func projectFile(ctx context.Context, idx *indexer.Indexer, absPath string, raw []byte, commitSeq uint64, disk diskStamp) (*projectionEntry, error) {
	projection, err := idx.ProjectSource(ctx, absPath, raw)
	if err != nil {
		return nil, err
	}
	return newProjectionEntry(absPath, idx.RepoPrefix(), projection, raw, commitSeq, disk), nil
}

func newProjectionEntry(absPath, repoPrefix string, projection *indexer.Projection, raw []byte, commitSeq uint64, disk diskStamp) *projectionEntry {
	layer := graph.NewOverlayLayer()
	layer.MarkFile(projection.GraphPath, false)
	for _, n := range projection.Nodes {
		layer.AddNode(projection.GraphPath, n)
	}
	for _, e := range projection.Edges {
		layer.AddEdge(e)
	}
	return &projectionEntry{
		key:         pathkey.MutationKey(absPath),
		absPath:     absPath,
		graphPath:   projection.GraphPath,
		repoPrefix:  repoPrefix,
		commitSeq:   commitSeq,
		hash:        projection.ContentHash,
		rawHash:     blake3.Sum256(raw),
		disk:        disk,
		language:    projection.Language,
		layer:       layer,
		parseErrors: projection.ParseErrors,
		buildTime:   projection.BuildTime,
		size:        int64(len(projection.Nodes))*projectionNodeBytes + int64(len(projection.Edges))*projectionEdgeBytes,
	}
}

// newProjectionTombstone is the entry for a file a tool removed: its layer
// hides every symbol the store still holds for the path.
func newProjectionTombstone(absPath, graphPath, repoPrefix string, commitSeq uint64) *projectionEntry {
	layer := graph.NewOverlayLayer()
	layer.MarkFile(graphPath, true)
	return &projectionEntry{
		key:        pathkey.MutationKey(absPath),
		absPath:    absPath,
		graphPath:  graphPath,
		repoPrefix: repoPrefix,
		commitSeq:  commitSeq,
		layer:      layer,
		tombstone:  true,
	}
}

// publish makes e the live projection of its file, replacing an older entry
// at once. It refuses, leaving every live entry in place, when publishing is
// off, when e is older than the live entry, when e names the live entry's
// file in a spelling that matches only after case folding, and when e would
// take the registry over a cap.
func (p *pendingProjections) publish(e *projectionEntry) error {
	if !p.enabled {
		return projectionDisabled
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.snap.Load()
	prev := cur.entries[e.key]
	bytes := cur.bytes + e.size
	if prev != nil {
		bytes -= prev.size
	}
	var refusal projectionRefusal
	switch {
	case prev != nil && prev.graphPath != e.graphPath:
		refusal = projectionPathCase
	case prev != nil && prev.commitSeq > e.commitSeq:
		refusal = projectionSuperseded
	case prev == nil && len(cur.entries) >= projectionMaxFiles:
		refusal = projectionOverFiles
	case bytes > projectionMaxBytes:
		refusal = projectionOverBytes
	}
	if refusal != "" {
		p.refused[refusal]++
		return refusal
	}
	// A watcher event that landed between the write and this publish
	// marked the entry being replaced; it applies to these bytes too.
	if prev != nil && prev.suspect.Load() {
		e.suspect.Store(true)
	}
	next := &projectionSnapshot{entries: maps.Clone(cur.entries), bytes: bytes}
	next.entries[e.key] = e
	p.snap.Store(next)
	p.published.Add(1)
	return nil
}

// lookup returns the live projection of path, or nil when there is none.
// The file is stat-ed before an entry is served; when it no longer holds the
// projected bytes the entry is dropped and errProjectionExternalWrite
// returned, so the caller re-projects or reads the store.
func (p *pendingProjections) lookup(path string) (*projectionEntry, error) {
	snap := p.snap.Load()
	if len(snap.entries) == 0 {
		return nil, nil
	}
	e := snap.entries[pathkey.MutationKey(path)]
	if e == nil {
		return nil, nil
	}
	if !e.onDisk() {
		p.drop(e)
		p.externalWrites.Add(1)
		return nil, errProjectionExternalWrite
	}
	return e, nil
}

func (e *projectionEntry) onDisk() bool {
	stamp, err := statDiskStamp(e.absPath)
	if err != nil || !e.disk.matches(stamp) {
		return false
	}
	if e.tombstone || !e.suspect.Swap(false) {
		return true
	}
	raw, err := os.ReadFile(e.absPath)
	return err == nil && blake3.Sum256(raw) == e.rawHash
}

// markSuspect makes the next lookup of path re-read the file before serving
// its entry. The watcher calls it for every event, before debouncing.
func (p *pendingProjections) markSuspect(path string) {
	snap := p.snap.Load()
	if len(snap.entries) == 0 {
		return
	}
	if e := snap.entries[pathkey.MutationKey(path)]; e != nil {
		e.suspect.Store(true)
	}
}

func (p *pendingProjections) drop(e *projectionEntry) {
	p.dropAll([]*projectionEntry{e})
}

// dropAll removes each entry that is still the live one for its file; an
// entry already replaced by a newer publish stays replaced.
func (p *pendingProjections) dropAll(entries []*projectionEntry) int {
	if len(entries) == 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.snap.Load()
	next := &projectionSnapshot{bytes: cur.bytes}
	dropped := 0
	for _, e := range entries {
		if cur.entries[e.key] != e {
			continue
		}
		if next.entries == nil {
			next.entries = maps.Clone(cur.entries)
		}
		delete(next.entries, e.key)
		next.bytes -= e.size
		dropped++
	}
	if dropped > 0 {
		p.snap.Store(next)
	}
	return dropped
}

// projectionStore is what retirement reads: a file's content identity, then
// its nodes.
type projectionStore interface {
	graph.FileMetaPathReader
	GetFileNodes(filePath string) []*graph.Node
}

// retire drops every entry whose bytes the store now holds, judged by content
// alone: the store's content hash for the file equals the entry's, or, for a
// tombstone, the store has no row for the file. A completed receipt is not
// evidence, and a failed patch leaves the old hash, so its entry stays. The
// store's nodes are compared with the entry's in between two hash reads, and
// a mismatch the second read confirms is counted as divergence.
func (p *pendingProjections) retire(store projectionStore) int {
	snap := p.snap.Load()
	if len(snap.entries) == 0 {
		return 0
	}
	byRepo := make(map[string][]*projectionEntry)
	for _, e := range snap.entries {
		byRepo[e.repoPrefix] = append(byRepo[e.repoPrefix], e)
	}
	var caughtUp []*projectionEntry
	for prefix, entries := range byRepo {
		candidates := storeHolds(store, prefix, entries)
		diverged := make(map[*projectionEntry]bool, len(candidates))
		for _, e := range candidates {
			stored := store.GetFileNodes(e.graphPath)
			diverged[e] = len(indexer.DiffProjectedNodes(e.layer.FileNodes(e.graphPath), stored)) > 0
		}
		for _, e := range storeHolds(store, prefix, candidates) {
			if diverged[e] {
				p.divergence.Add(1)
			}
			caughtUp = append(caughtUp, e)
		}
	}
	retired := p.dropAll(caughtUp)
	p.retired.Add(int64(retired))
	return retired
}

// storeHolds returns the entries whose content the store holds now.
func storeHolds(store projectionStore, prefix string, entries []*projectionEntry) []*projectionEntry {
	if len(entries) == 0 {
		return nil
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.graphPath
	}
	rows, err := store.FileMetasByPaths(prefix, paths)
	if err != nil {
		return nil
	}
	var held []*projectionEntry
	for _, e := range entries {
		row, ok := rows[e.graphPath]
		if e.tombstone {
			ok = !ok
		} else {
			ok = ok && row.ContentHash == e.hash
		}
		if ok {
			held = append(held, e)
		}
	}
	return held
}
