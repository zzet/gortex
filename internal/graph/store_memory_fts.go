package graph

import "fmt"

// FTSEngine is the FTS delegation surface of the memory backend (issue
// davalillo/gortex#21). It is satisfied by a *store_sqlite.Store opened on a
// ":memory:" DSN (wired in internal/serverstack/backend_memory.go): the
// engine answers FTS ranking with the exact SQL the sqlite backend uses, so
// result parity is structural rather than reimplemented, and every FTS byte
// stays in RAM (LOCAL-001: no random 4 KB writes come back through the
// sidecar).
//
// The engine carries the FTS sidecars plus minimal node-row mirrors (fed by
// ensureFTSEngineNodes) so the engine's own repo-prefix join resolves; it
// never receives edges. Bundle materialization reads the resident graph, so
// no graph state is duplicated in the engine beyond that FTS-admitted node
// mirror.
//
// The engine is derived state with the same durability contract as the graph
// itself: it is not journaled and not persisted. It is rebuilt from resident
// state at boot (warmup step, issue #21) and dies with the process.
type FTSEngine interface {
	SymbolSearcher
	SymbolFTSBatchUpserter
	SymbolFTSBatchDeleter
	SymbolFTSRepoResetter
	SymbolFTSRepoReplacer
	SymbolFTSCounter
	SymbolFTSNormalizationState
	SearchSymbolsRepoScoped(query string, repoAllow []string, limit int) ([]SymbolHit, error)

	// AddBatch feeds the engine's minimal node-row mirror (see
	// ensureFTSEngineNodes); only the node slice is ever used.
	AddBatch(nodes []*Node, edges []*Edge)
}

// MemoryOption configures NewMemoryStore.
type MemoryOption func(*MemoryStore)

// WithFTSEngine attaches the in-RAM FTS engine. Without it the store serves
// no text search (the pre-#21 prototype behaviour).
func WithFTSEngine(e FTSEngine) MemoryOption {
	return func(m *MemoryStore) { m.fts = e }
}

// ftsEngine returns the attached engine, or nil.
func (m *MemoryStore) ftsEngine() FTSEngine { return m.fts }

// --- FTS delegation ---------------------------------------------------------
//
// Every graph.SymbolFTS* / search capability the indexer and the search
// adapters capability-probe is implemented here by delegating to the engine.
// The nil-engine guards keep an engine-less MemoryStore inert instead of
// panicking; the daemon always attaches an engine for the memory backend.

func (m *MemoryStore) UpsertSymbolFTS(nodeID, tokens string) error {
	e := m.ftsEngine()
	if e == nil {
		return nil
	}
	// The engine's FTS upsert joins its own nodes table for the owning repo
	// prefix, so the document's node row must exist there first.
	if err := m.ensureFTSEngineNodes([]SymbolFTSItem{{NodeID: nodeID}}); err != nil {
		return err
	}
	return e.UpsertSymbolFTS(nodeID, tokens)
}

// BulkUpsertSymbolFTS delegates directly: the cold-start fast path takes the
// owning repo prefix as a parameter, so it does not depend on engine node
// rows.
func (m *MemoryStore) BulkUpsertSymbolFTS(repoPrefix string, items []SymbolFTSItem) error {
	if e := m.ftsEngine(); e != nil {
		return e.BulkUpsertSymbolFTS(repoPrefix, items)
	}
	return nil
}

func (m *MemoryStore) BatchUpsertSymbolFTS(items []SymbolFTSItem) error {
	e := m.ftsEngine()
	if e == nil {
		return nil
	}
	if err := m.ensureFTSEngineNodes(items); err != nil {
		return err
	}
	return e.BatchUpsertSymbolFTS(items)
}

func (m *MemoryStore) BatchDeleteSymbolFTS(nodeIDs []string) error {
	if e := m.ftsEngine(); e != nil {
		return e.BatchDeleteSymbolFTS(nodeIDs)
	}
	return nil
}

func (m *MemoryStore) ResetSymbolFTS(repoPrefix string) error {
	if e := m.ftsEngine(); e != nil {
		return e.ResetSymbolFTS(repoPrefix)
	}
	return nil
}

// ReplaceSymbolFTS implements the whole-repository replacement WITHOUT
// delegating. store_sqlite's own ReplaceSymbolFTS refuses stores whose read
// and write pools share one handle — the structural situation of a
// ":memory:" engine — and a delegate call inside its open transaction would
// deadlock the single writer connection anyway. The interface's documented
// composed fallback (ResetSymbolFTS + SymbolFTSBatchUpserter chunks) is
// therefore the implementation: the wipe commits first, so a failure part-way
// leaves a partial corpus — acceptable for a derived in-RAM engine whose
// every caller (boot rebuild, normalization reconcile) is idempotent and
// retryable.
func (m *MemoryStore) ReplaceSymbolFTS(repoPrefix string, produce func(emit func([]SymbolFTSItem) error) error) error {
	e := m.ftsEngine()
	if e == nil {
		return nil
	}
	if produce == nil {
		return nil
	}
	if err := e.ResetSymbolFTS(repoPrefix); err != nil {
		return fmt.Errorf("memory fts replace: reset %q: %w", repoPrefix, err)
	}
	return produce(func(items []SymbolFTSItem) error {
		if err := m.ensureFTSEngineNodes(items); err != nil {
			return err
		}
		return e.BatchUpsertSymbolFTS(items)
	})
}

// ensureFTSEngineNodes mirrors the documents' node rows into the engine
// before an incremental FTS upsert: upsertSymbolFTSChunkTx derives the owning
// repo prefix by joining the engine's OWN nodes table (LEFT JOIN nodes ... →
// COALESCE(repo_prefix, '')), and a missing row would file the document under
// the unowned empty prefix, which every repo-scope filter lets through.
// Copies are upsert-by-ID, in RAM only, and bounded by the FTS-admitted node
// set — never the whole graph.
func (m *MemoryStore) ensureFTSEngineNodes(items []SymbolFTSItem) error {
	e := m.ftsEngine()
	if e == nil || len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.NodeID == "" {
			continue
		}
		if _, dup := seen[it.NodeID]; dup {
			continue
		}
		seen[it.NodeID] = struct{}{}
		ids = append(ids, it.NodeID)
	}
	if len(ids) == 0 {
		return nil
	}
	nodes := m.Graph.GetNodesByIDs(ids)
	stubs := make([]*Node, 0, len(ids))
	for _, id := range ids {
		if n := nodes[id]; n != nil {
			stubs = append(stubs, n)
		}
	}
	if len(stubs) == 0 {
		return nil
	}
	e.AddBatch(stubs, nil)
	return nil
}

func (m *MemoryStore) BuildSymbolIndex() error {
	if e := m.ftsEngine(); e != nil {
		return e.BuildSymbolIndex()
	}
	return nil
}

func (m *MemoryStore) SymbolFTSCount() (int, error) {
	if e := m.ftsEngine(); e != nil {
		return e.SymbolFTSCount()
	}
	return 0, nil
}

func (m *MemoryStore) GetSymbolFTSNormalization(repoPrefix string) (mode string, ok bool, err error) {
	if e := m.ftsEngine(); e != nil {
		return e.GetSymbolFTSNormalization(repoPrefix)
	}
	return "", false, nil
}

func (m *MemoryStore) SetSymbolFTSNormalization(repoPrefix, mode string) error {
	if e := m.ftsEngine(); e != nil {
		return e.SetSymbolFTSNormalization(repoPrefix, mode)
	}
	return nil
}

func (m *MemoryStore) SearchSymbols(query string, limit int) ([]SymbolHit, error) {
	if e := m.ftsEngine(); e != nil {
		return e.SearchSymbols(query, limit)
	}
	return nil, nil
}

func (m *MemoryStore) SearchSymbolsRepoScoped(query string, repoAllow []string, limit int) ([]SymbolHit, error) {
	if e := m.ftsEngine(); e != nil {
		return e.SearchSymbolsRepoScoped(query, repoAllow, limit)
	}
	return nil, nil
}

// --- Bundle materialization --------------------------------------------------
//
// The sqlite backend materializes bundles from its own node/edge tables. The
// memory backend's engine has no node rows, so the same two-step shape runs
// across the store boundary: ranked hits come from the engine (FTS5 + bm25),
// the bundle bodies (node + in/out edges) come from the resident graph. A hit
// whose node is gone (evicted between the search and the fetch) is skipped —
// the same graceful degradation the sqlite path applies.

func (m *MemoryStore) SearchSymbolBundles(query string, limit int) ([]SymbolBundle, error) {
	return m.searchSymbolBundlesScoped(query, nil, limit)
}

func (m *MemoryStore) SearchSymbolBundlesRepoScoped(query string, repoAllow []string, limit int) ([]SymbolBundle, error) {
	return m.searchSymbolBundlesScoped(query, repoAllow, limit)
}

func (m *MemoryStore) searchSymbolBundlesScoped(query string, repoAllow []string, limit int) ([]SymbolBundle, error) {
	e := m.ftsEngine()
	if e == nil {
		return nil, nil
	}
	hits, err := e.SearchSymbolsRepoScoped(query, repoAllow, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(hits))
	scoreByID := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.NodeID == "" {
			continue
		}
		if _, dup := scoreByID[h.NodeID]; dup {
			// First hit keeps the score / position; defend against a
			// future ranker that returns an id more than once.
			continue
		}
		scoreByID[h.NodeID] = h.Score
		ids = append(ids, h.NodeID)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	nodes := m.Graph.GetNodesByIDs(ids)
	out := m.Graph.GetOutEdgesByNodeIDs(ids)
	in := m.Graph.GetInEdgesByNodeIDs(ids)

	bundles := make([]SymbolBundle, 0, len(ids))
	for _, id := range ids {
		n := nodes[id]
		if n == nil {
			// Hit references a node evicted between the search and the
			// node fetch — skip; the caller does its own dedup / filter.
			continue
		}
		bundles = append(bundles, SymbolBundle{
			Node:     n,
			Score:    scoreByID[id],
			OutEdges: out[id],
			InEdges:  in[id],
		})
	}
	return bundles, nil
}

// compile-time proof that the delegation surface stays aligned with the
// optional interfaces the indexer and search adapters probe for.
var (
	_ SymbolSearcher              = (*MemoryStore)(nil)
	_ SymbolFTSBatchUpserter      = (*MemoryStore)(nil)
	_ SymbolFTSBatchDeleter       = (*MemoryStore)(nil)
	_ SymbolFTSRepoResetter       = (*MemoryStore)(nil)
	_ SymbolFTSRepoReplacer       = (*MemoryStore)(nil)
	_ SymbolFTSCounter            = (*MemoryStore)(nil)
	_ SymbolFTSNormalizationState = (*MemoryStore)(nil)
	_ SymbolBundleSearcher        = (*MemoryStore)(nil)
	_ ScopedSymbolBundleSearcher  = (*MemoryStore)(nil)
)
