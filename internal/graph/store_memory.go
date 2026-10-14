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

	// mtimes is the per-repo file-mtime sidecar (issue davalillo/gortex#26):
	// the same rows store_sqlite keeps in its file_mtimes table, held in RAM
	// so the memory backend satisfies the FileMtime* capability family and
	// warm boots can seed ReconcileRepoCtx from the store. Not journaled —
	// a crash rolls mtimes back to the last snapshot, which only costs one
	// extra reconcile (files look changed), never a wrong result. The
	// snapshot dump persists it and the snapshot load restores it; see
	// serverstack/backend_memory.go.
	mtimeMu sync.Mutex
	mtimes  map[string]map[string]int64

	// Replay diagnostics (issue davalillo/gortex#25 batch A). Written only
	// by ReplayJournal while it holds applyMu; read afterwards via
	// ReplayAnomalies. Not journaled, not persisted.
	replayUnknownOps     int
	replayCorruptMidFile bool
	replayCorruptLine    int
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

// ColdLoadBatch applies the snapshot's node/edge bulk to the empty store
// during backend construction (issue davalillo/gortex#25 A8). It holds
// applyMu so the cold-load writes run under the same mutual exclusion every
// other graph write uses; the journal is not attached yet, so nothing is
// journaled and the inert-guard read-modify dance is unnecessary. Only the
// construction path (serverstack.openMemoryBackend, single-goroutine) may
// call this — the store is not published to any reader until it returns.
func (m *MemoryStore) ColdLoadBatch(nodes []*Node, edges []*Edge) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.Graph.AddBatch(nodes, edges)
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

// journaledEffective is journaled with the issue davalillo/gortex#22 guard:
// the inert check runs under applyMu BEFORE record, so journal order still
// equals apply order — the ops that reach record are exactly the ones whose
// apply changes state. Inert mutations are applied exactly as before, only
// unjournaled (see store_memory_journal_guard.go for the consistency and
// concurrency reasoning).
func (m *MemoryStore) journaledEffective(op *journalOp, apply func(), inert func() bool) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if !inert() {
		m.record(op)
	}
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
	m.journaledEffective(&journalOp{Op: "add_node", Node: journalNodeOf(n)},
		func() { m.Graph.AddNode(n) },
		func() bool { return nodeInert(m.existingNode(n.ID), n) })
}

// journalAddBatchChunk bounds one journal line: a whole-repo first index can
// AddBatch hundreds of thousands of entries, and a single multi-megabyte
// JSONL line would be fragile to replay and to cap.
const journalAddBatchChunk = 1000

func (m *MemoryStore) AddBatch(nodes []*Node, edges []*Edge) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	// Journal only the batch's effective members (issue davalillo/gortex#22):
	// inert re-adds are applied exactly as before but not journaled. The
	// old bare "add_batch" marker line carried no data and was silently
	// ignored by replay — dropped.
	for start := 0; start < len(nodes); start += journalAddBatchChunk {
		end := min(start+journalAddBatchChunk, len(nodes))
		chunk := make([]journalNode, 0, end-start)
		for _, n := range nodes[start:end] {
			if n == nil || nodeInert(m.existingNode(n.ID), n) {
				continue
			}
			chunk = append(chunk, *journalNodeOf(n))
		}
		if len(chunk) > 0 {
			m.record(&journalOp{Op: "add_batch_nodes", Nodes: chunk})
		}
	}
	for start := 0; start < len(edges); start += journalAddBatchChunk {
		end := min(start+journalAddBatchChunk, len(edges))
		chunk := make([]journalEdge, 0, end-start)
		for _, e := range edges[start:end] {
			if e == nil || edgeInert(m.existingEdge(e), e) {
				continue
			}
			chunk = append(chunk, *journalEdgeOf(e))
		}
		if len(chunk) > 0 {
			m.record(&journalOp{Op: "add_batch_edges", Edges: chunk})
		}
	}
	m.Graph.AddBatch(nodes, edges)
}

func (m *MemoryStore) AddEdge(e *Edge) {
	m.journaledEffective(&journalOp{Op: "add_edge", Edge: journalEdgeOf(e)},
		func() { m.Graph.AddEdge(e) },
		func() bool { return edgeInert(m.existingEdge(e), e) })
}

func (m *MemoryStore) SetEdgeProvenance(e *Edge, newOrigin string) bool {
	var changed bool
	m.journaledEffective(
		&journalOp{Op: "set_edge_provenance", Edge: journalEdgeOf(e), NewOrigin: newOrigin},
		func() { changed = m.Graph.SetEdgeProvenance(e, newOrigin) },
		func() bool {
			// SetEdgeProvenance only rewrites the existing edge's Origin;
			// a missing edge or an unchanged origin cannot change state.
			old := m.existingEdge(e)
			return old == nil || old.Origin == newOrigin
		})
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
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	// Journal only the effective updates (issue davalillo/gortex#22); the
	// full batch still applies unchanged.
	jp := make([]journalProvenance, 0, len(batch))
	for _, u := range batch {
		if u.Edge == nil {
			continue
		}
		old := m.existingEdge(u.Edge)
		if old == nil || old.Origin == u.NewOrigin {
			continue
		}
		jp = append(jp, journalProvenance{Edge: journalEdgeOf(u.Edge), NewOrigin: u.NewOrigin})
	}
	if len(jp) > 0 {
		m.record(&journalOp{Op: "set_edge_provenance_batch", Provenance: jp})
	}
	changed = m.Graph.SetEdgeProvenanceBatch(batch)
	return changed
}

func (m *MemoryStore) RemoveEdge(from, to string, kind EdgeKind) bool {
	var removed bool
	m.journaledEffective(
		&journalOp{Op: "remove_edge", From: from, To: to, Kind: kind},
		func() { removed = m.Graph.RemoveEdge(from, to, kind) },
		func() bool {
			// Mirror Graph.RemoveEdge's own match (To == to && Kind == kind,
			// first encounter in the out-bucket): nothing matching means the
			// removal cannot change state.
			for _, e := range m.Graph.GetOutEdges(from) {
				if e != nil && e.To == to && e.Kind == kind {
					return false
				}
			}
			return true
		})
	return removed
}

func (m *MemoryStore) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_file", Path: filePath},
		func() { nodesRemoved, edgesRemoved = m.Graph.EvictFile(filePath) })
	return nodesRemoved, edgesRemoved
}

func (m *MemoryStore) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_repo", RepoPrefix: repoPrefix},
		func() {
			nodesRemoved, edgesRemoved = m.Graph.EvictRepo(repoPrefix)
			// The sidecar rides the repo, like the sqlite backend's rows
			// (davalillo/gortex#26). Not journaled: the snapshot dump copies
			// whatever is live at close time, so clearing here keeps the
			// next snapshot free of the evicted repo's rows.
			m.mtimeMu.Lock()
			delete(m.mtimes, repoPrefix)
			m.mtimeMu.Unlock()
		})
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
	// Replay mutates the graph without the journal attached; hold applyMu so
	// the loop cannot interleave with any writer. Nothing inside calls back
	// into the journaled wrappers, so the lock is non-reentrant-safe.
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.replayUnknownOps = 0
	m.replayCorruptMidFile = false
	m.replayCorruptLine = 0
	ops := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var op journalOp
		if err := json.Unmarshal(line, &op); err != nil {
			// Keep what applied so far — derived-cache semantics. But name
			// the failure shape (issue davalillo/gortex#25 A2): a corrupt
			// line with more non-empty lines after it is mid-file corruption
			// (a journal this process cannot have written), not the torn
			// tail a crash between flushes produces. Look ahead without
			// applying anything.
			m.replayCorruptLine = lineNo
			for sc.Scan() {
				if len(sc.Bytes()) > 0 {
					m.replayCorruptMidFile = true
					break
				}
			}
			return ops, nil
		}
		if m.applyOp(&op) {
			ops++
		}
	}
	return ops, sc.Err()
}

// ReplayAnomalies reports what the last ReplayJournal saw beyond ordinary
// operations: unknown op ids (skipped by applyOp) and whether a corrupt line
// sat mid-file rather than at the torn tail a crash produces. Zero values
// mean a clean replay. Read only after ReplayJournal returned.
func (m *MemoryStore) ReplayAnomalies() (unknownOps int, corruptMidFile bool, corruptLine int) {
	return m.replayUnknownOps, m.replayCorruptMidFile, m.replayCorruptLine
}

func (m *MemoryStore) applyOp(op *journalOp) bool {
	switch op.Op {
	case "add_node":
		if n := op.Node.toNode(); n != nil {
			m.Graph.AddNode(n)
		}
	case "add_batch_nodes":
		nodes := make([]*Node, 0, len(op.Nodes))
		for i := range op.Nodes {
			if n := op.Nodes[i].toNode(); n != nil {
				nodes = append(nodes, n)
			}
		}
		m.Graph.AddBatch(nodes, nil)
	case "add_batch_edges":
		edges := make([]*Edge, 0, len(op.Edges))
		for i := range op.Edges {
			if e := op.Edges[i].toEdge(); e != nil {
				edges = append(edges, e)
			}
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
	default:
		// An op id this build does not know: a journal written by a newer
		// gortex, or corruption. Apply nothing — replay must never invent a
		// mutation — but count it so the caller can surface the gap instead
		// of silently diverging from the journal (issue davalillo/gortex#25
		// A1).
		m.replayUnknownOps++
		return false
	}
	return true
}
