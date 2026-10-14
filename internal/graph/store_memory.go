package graph

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MemoryStore is the Tier-3 memory-resident store backend
// (odd/tasks/tier3-memory-store.md, issue davalillo/gortex#19): primary state
// is an in-memory Graph; every mutation is appended to a JSONL journal before
// it is applied; a snapshot dumped through the sqlite bulk loader is the real
// durability boundary (LOCAL-001: the store is a derived cache, so losing an
// un-flushed window costs one free re-reconcile).
//
// The design goal is the write pattern, not raw durability: the journal turns
// the sqlite read-modify-write amplification (~10^6x for a single small edit)
// into one sequential append per mutation (~1x).
//
// Reads and everything else are inherited from the embedded *Graph, which
// satisfies the full Store contract (compile assertion in store.go). Only the
// interface's write methods are overridden to journal first, then delegate.
type MemoryStore struct {
	*Graph

	// applyMu serializes journal-append + delegate-apply inside one critical
	// section per mutation (issue davalillo/gortex#20): without it, two
	// concurrent writers can interleave as A-journals, B-journals, B-applies,
	// A-applies and the journal order no longer matches the applied order —
	// a post-crash replay would diverge from the state that actually existed.
	applyMu sync.Mutex

	mu          sync.Mutex
	journalPath string
	journalFile *os.File
	journalW    *bufio.Writer
	journalCW   *countWriter
	journalEnc  *json.Encoder
	journalErr  error

	// fts is the optional in-RAM FTS engine (issue davalillo/gortex#21):
	// a store_sqlite.Store on a ":memory:" DSN answering the FTS side of
	// search with the sqlite backend's own SQL. Derived state — not
	// journaled, not persisted, rebuilt from resident graph state at boot.
	// See store_memory_fts.go.
	fts FTSEngine
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore builds an empty memory-resident store. Call OpenJournal to
// attach durability and ReplayJournal to restore a prior state.
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	m := &MemoryStore{Graph: New()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// journalNode is Node with the JSON-excluded Meta field (marked json:"-" on
// the stored struct because agent-facing responses must not pay its bytes)
// captured explicitly. The journal must round-trip the complete struct, so it
// re-serializes it here.
type journalNode struct {
	Node
	Meta map[string]any `json:"meta,omitempty"`
}

func journalNodeOf(n *Node) *journalNode {
	if n == nil {
		return nil
	}
	return &journalNode{Node: *n, Meta: n.Meta}
}

func (jn *journalNode) toNode() *Node {
	if jn == nil {
		return nil
	}
	n := jn.Node
	n.Meta = jn.Meta
	return &n
}

// journalEdge is Edge with Meta and Confidence captured explicitly (both are
// json:"-" on the stored struct).
type journalEdge struct {
	Edge
	Meta       map[string]any `json:"meta,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
}

func journalEdgeOf(e *Edge) *journalEdge {
	if e == nil {
		return nil
	}
	return &journalEdge{Edge: *e, Meta: e.Meta, Confidence: e.Confidence}
}

func (je *journalEdge) toEdge() *Edge {
	if je == nil {
		return nil
	}
	e := je.Edge
	e.Meta = je.Meta
	e.Confidence = je.Confidence
	return &e
}

type journalReindex struct {
	Edge            *journalEdge `json:"edge,omitempty"`
	OldFrom         string       `json:"old_from,omitempty"`
	OldTo           string       `json:"old_to,omitempty"`
	OldKind         EdgeKind     `json:"old_kind,omitempty"`
	RefreshIdentity bool         `json:"refresh_identity,omitempty"`
	OldFilePath     string       `json:"old_file_path,omitempty"`
	OldLine         int          `json:"old_line,omitempty"`
}

type journalProvenance struct {
	Edge      *journalEdge `json:"edge,omitempty"`
	NewOrigin string       `json:"new_origin,omitempty"`
}

// journalOp is one mutation record. Ops mirror the Store write surface
// one-to-one so replay reproduces state exactly.
type journalOp struct {
	Op         string              `json:"op"`
	TS         int64               `json:"ts,omitempty"`
	Node       *journalNode        `json:"node,omitempty"`
	Nodes      []journalNode       `json:"nodes,omitempty"`
	Edge       *journalEdge        `json:"edge,omitempty"`
	Edges      []journalEdge       `json:"edges,omitempty"`
	Reindexes  []journalReindex    `json:"reindexes,omitempty"`
	Provenance []journalProvenance `json:"provenance,omitempty"`
	NewOrigin  string              `json:"new_origin,omitempty"`
	OldTo      string              `json:"old_to,omitempty"`
	From       string              `json:"from,omitempty"`
	To         string              `json:"to,omitempty"`
	Kind       EdgeKind            `json:"kind,omitempty"`
	Path       string              `json:"path,omitempty"`
	RepoPrefix string              `json:"repo_prefix,omitempty"`
}

// countWriter tracks journal bytes without an extra stat per record.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// OpenJournal attaches durability: mutations are appended to path (JSONL,
// one record per line, flushed per mutation — sequential writes only).
// Call it after ReplayJournal. Until the journal is open (or after it is
// closed or has degraded) the store behaves as plain in-memory.
func (m *MemoryStore) OpenJournal(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("memory journal: create directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("memory journal: open: %w", err)
	}
	m.journalPath = path
	m.journalFile = f
	m.journalCW = &countWriter{w: f}
	m.journalW = bufio.NewWriterSize(m.journalCW, 256*1024)
	m.journalEnc = json.NewEncoder(m.journalW)
	m.journalErr = nil
	return nil
}

// CloseJournal flushes and closes the journal. The store keeps working
// in-memory afterwards.
func (m *MemoryStore) CloseJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalFile == nil {
		return nil
	}
	err := m.journalW.Flush()
	if cerr := m.journalFile.Close(); err == nil {
		err = cerr
	}
	m.journalEnc = nil
	m.journalW = nil
	m.journalFile = nil
	return err
}

// ResetJournal truncates the journal. Called after a successful snapshot
// dump: the snapshot subsumes every mutation journaled so far.
func (m *MemoryStore) ResetJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalEnc == nil {
		return nil
	}
	if err := m.journalW.Flush(); err != nil {
		return fmt.Errorf("memory journal: flush before reset: %w", err)
	}
	if err := m.journalFile.Truncate(0); err != nil {
		return fmt.Errorf("memory journal: truncate: %w", err)
	}
	if _, err := m.journalFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("memory journal: seek: %w", err)
	}
	m.journalW.Reset(m.journalFile)
	m.journalCW.n = 0
	m.journalErr = nil
	return nil
}

// JournalErr reports the first append failure, if any. Policy: once the
// journal degrades, the store keeps serving in-memory and does not retry —
// durability is best-effort by the derived-cache contract.
func (m *MemoryStore) JournalErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.journalErr
}

// JournalBytes reports bytes appended to the journal since the last reset.
func (m *MemoryStore) JournalBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalCW == nil {
		return 0
	}
	return m.journalCW.n
}

// journaled runs one mutation with journal-append and delegate-apply inside
// the same critical section: the journal order is, by construction, the
// apply order (issue davalillo/gortex#20).
func (m *MemoryStore) journaled(op *journalOp, apply func()) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.record(op)
	apply()
}

// record journals one mutation before it is applied. Journal-first: the
// record describes the intent; the delegate applies it after. Callers hold
// applyMu, so journal order equals apply order.
func (m *MemoryStore) record(op *journalOp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalEnc == nil || m.journalErr != nil {
		return
	}
	op.TS = time.Now().UnixMicro()
	if err := m.journalEnc.Encode(op); err != nil {
		m.journalErr = fmt.Errorf("memory journal append: %w", err)
		return
	}
	if err := m.journalW.Flush(); err != nil {
		m.journalErr = fmt.Errorf("memory journal flush: %w", err)
	}
}

// --- Journalled write overrides --------------------------------------------
//
// Every Store write method journals first, then delegates to the embedded
// *Graph. Reads are not overridden.

func (m *MemoryStore) AddNode(n *Node) {
	m.journaled(&journalOp{Op: "add_node", Node: journalNodeOf(n)}, func() { m.Graph.AddNode(n) })
}

// journalAddBatchChunk bounds one journal line: a whole-repo first index can
// AddBatch hundreds of thousands of entries, and a single multi-megabyte
// JSONL line would be fragile to replay and to cap.
const journalAddBatchChunk = 1000

func (m *MemoryStore) AddBatch(nodes []*Node, edges []*Edge) {
	m.journaled(&journalOp{Op: "add_batch"}, func() {
		for start := 0; start < len(nodes); start += journalAddBatchChunk {
			end := min(start+journalAddBatchChunk, len(nodes))
			chunk := make([]journalNode, 0, end-start)
			for _, n := range nodes[start:end] {
				chunk = append(chunk, *journalNodeOf(n))
			}
			m.record(&journalOp{Op: "add_batch_nodes", Nodes: chunk})
		}
		for start := 0; start < len(edges); start += journalAddBatchChunk {
			end := min(start+journalAddBatchChunk, len(edges))
			chunk := make([]journalEdge, 0, end-start)
			for _, e := range edges[start:end] {
				chunk = append(chunk, *journalEdgeOf(e))
			}
			m.record(&journalOp{Op: "add_batch_edges", Edges: chunk})
		}
		m.Graph.AddBatch(nodes, edges)
	})
}

func (m *MemoryStore) AddEdge(e *Edge) {
	m.journaled(&journalOp{Op: "add_edge", Edge: journalEdgeOf(e)}, func() { m.Graph.AddEdge(e) })
}

func (m *MemoryStore) SetEdgeProvenance(e *Edge, newOrigin string) bool {
	var changed bool
	m.journaled(&journalOp{Op: "set_edge_provenance", Edge: journalEdgeOf(e), NewOrigin: newOrigin},
		func() { changed = m.Graph.SetEdgeProvenance(e, newOrigin) })
	return changed
}

func (m *MemoryStore) ReindexEdge(e *Edge, oldTo string) {
	m.journaled(&journalOp{Op: "reindex_edge", Edge: journalEdgeOf(e), OldTo: oldTo},
		func() { m.Graph.ReindexEdge(e, oldTo) })
}

func (m *MemoryStore) ReindexEdges(batch []EdgeReindex) {
	jr := make([]journalReindex, len(batch))
	for i, r := range batch {
		jr[i] = journalReindex{
			Edge:            journalEdgeOf(r.Edge),
			OldFrom:         r.OldFrom,
			OldTo:           r.OldTo,
			OldKind:         r.OldKind,
			RefreshIdentity: r.RefreshIdentity,
			OldFilePath:     r.OldFilePath,
			OldLine:         r.OldLine,
		}
	}
	m.journaled(&journalOp{Op: "reindex_edges", Reindexes: jr}, func() { m.Graph.ReindexEdges(batch) })
}

func (m *MemoryStore) SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) (changed int) {
	jp := make([]journalProvenance, len(batch))
	for i, u := range batch {
		jp[i] = journalProvenance{Edge: journalEdgeOf(u.Edge), NewOrigin: u.NewOrigin}
	}
	m.journaled(&journalOp{Op: "set_edge_provenance_batch", Provenance: jp},
		func() { changed = m.Graph.SetEdgeProvenanceBatch(batch) })
	return changed
}

func (m *MemoryStore) RemoveEdge(from, to string, kind EdgeKind) bool {
	var removed bool
	m.journaled(&journalOp{Op: "remove_edge", From: from, To: to, Kind: kind},
		func() { removed = m.Graph.RemoveEdge(from, to, kind) })
	return removed
}

func (m *MemoryStore) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_file", Path: filePath},
		func() { nodesRemoved, edgesRemoved = m.Graph.EvictFile(filePath) })
	return nodesRemoved, edgesRemoved
}

func (m *MemoryStore) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_repo", RepoPrefix: repoPrefix},
		func() { nodesRemoved, edgesRemoved = m.Graph.EvictRepo(repoPrefix) })
	return nodesRemoved, edgesRemoved
}

// --- Replay -----------------------------------------------------------------

// ReplayJournal applies a journal file into the current in-memory state. A
// torn tail (crash between the flush and the next record) ends replay at the
// last complete line — derived-cache semantics, the next snapshot makes it
// moot. Returns the number of operations applied.
func (m *MemoryStore) ReplayJournal(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memory journal: open for replay: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	ops := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var op journalOp
		if err := json.Unmarshal(line, &op); err != nil {
			// Torn tail or corrupt line: keep what applied so far.
			return ops, nil
		}
		m.applyOp(&op)
		ops++
	}
	return ops, sc.Err()
}

func (m *MemoryStore) applyOp(op *journalOp) {
	switch op.Op {
	case "add_node":
		if n := op.Node.toNode(); n != nil {
			m.Graph.AddNode(n)
		}
	case "add_batch_nodes":
		nodes := make([]*Node, len(op.Nodes))
		for i := range op.Nodes {
			nodes[i] = op.Nodes[i].toNode()
		}
		m.Graph.AddBatch(nodes, nil)
	case "add_batch_edges":
		edges := make([]*Edge, len(op.Edges))
		for i := range op.Edges {
			edges[i] = op.Edges[i].toEdge()
		}
		m.Graph.AddBatch(nil, edges)
	case "add_edge":
		if e := op.Edge.toEdge(); e != nil {
			m.Graph.AddEdge(e)
		}
	case "set_edge_provenance":
		if e := op.Edge.toEdge(); e != nil {
			m.Graph.SetEdgeProvenance(e, op.NewOrigin)
		}
	case "reindex_edge":
		if e := op.Edge.toEdge(); e != nil {
			m.Graph.ReindexEdge(e, op.OldTo)
		}
	case "reindex_edges":
		batch := make([]EdgeReindex, len(op.Reindexes))
		for i, r := range op.Reindexes {
			batch[i] = EdgeReindex{
				Edge:            r.Edge.toEdge(),
				OldFrom:         r.OldFrom,
				OldTo:           r.OldTo,
				OldKind:         r.OldKind,
				RefreshIdentity: r.RefreshIdentity,
				OldFilePath:     r.OldFilePath,
				OldLine:         r.OldLine,
			}
		}
		m.Graph.ReindexEdges(batch)
	case "set_edge_provenance_batch":
		batch := make([]EdgeProvenanceUpdate, len(op.Provenance))
		for i, u := range op.Provenance {
			batch[i] = EdgeProvenanceUpdate{Edge: u.Edge.toEdge(), NewOrigin: u.NewOrigin}
		}
		m.Graph.SetEdgeProvenanceBatch(batch)
	case "remove_edge":
		m.Graph.RemoveEdge(op.From, op.To, op.Kind)
	case "evict_file":
		m.Graph.EvictFile(op.Path)
	case "evict_repo":
		m.Graph.EvictRepo(op.RepoPrefix)
	}
}
