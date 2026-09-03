package store_sqlite

import (
	"database/sql"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// sqliteMutationReceiptState is guarded exclusively by Store.writeMu. Sharing
// that lock with every graph mutation makes receipt boundaries atomic without a
// second gate or lock-ordering edge.
type sqliteMutationReceiptState struct {
	next   graph.MutationReceiptToken
	active map[graph.MutationReceiptToken]*sqliteMutationReceiptAccumulator
}

type sqliteMutationReceiptAccumulator struct {
	complete           bool
	incompleteReason   string
	resolutionRelevant bool
	changedFiles       map[string]struct{}
	unresolvedFiles    map[string]struct{}
	definitionFiles    map[string]struct{}
	targetNames        map[string]struct{}
	evictedNames       map[string]struct{}
	targetIDs          map[string]struct{}
	importCandidates   map[string]struct{}
}

// noteIncomplete voids the receipt, keeping the FIRST cause.
func (a *sqliteMutationReceiptAccumulator) noteIncomplete(reason string) {
	a.complete = false
	if a.incompleteReason == "" {
		a.incompleteReason = reason
	}
}

type sqliteMutationNodeIdentity struct {
	kind       string
	name       string
	qualName   string
	filePath   string
	repoPrefix string
}

func newSQLiteMutationReceiptAccumulator() *sqliteMutationReceiptAccumulator {
	return &sqliteMutationReceiptAccumulator{
		complete:         true,
		changedFiles:     make(map[string]struct{}),
		unresolvedFiles:  make(map[string]struct{}),
		definitionFiles:  make(map[string]struct{}),
		targetNames:      make(map[string]struct{}),
		evictedNames:     make(map[string]struct{}),
		targetIDs:        make(map[string]struct{}),
		importCandidates: make(map[string]struct{}),
	}
}

func (a *sqliteMutationReceiptAccumulator) receipt() graph.MutationReceipt {
	return graph.MutationReceipt{
		Complete:           a.complete,
		IncompleteReason:   a.incompleteReason,
		ResolutionRelevant: a.resolutionRelevant,
		ChangedFiles:       sortedSQLiteReceiptKeys(a.changedFiles),
		UnresolvedFiles:    sortedSQLiteReceiptKeys(a.unresolvedFiles),
		DefinitionFiles:    sortedSQLiteReceiptKeys(a.definitionFiles),
		TargetNames:        sortedSQLiteReceiptKeys(a.targetNames),
		EvictedNames:       sortedSQLiteReceiptKeys(a.evictedNames),
		TargetIDs:          sortedSQLiteReceiptKeys(a.targetIDs),
		ImportCandidates:   sortedSQLiteReceiptKeys(a.importCandidates),
	}
}

func sortedSQLiteReceiptKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// BeginMutationReceipt starts an independent observation window. writeMu is
// the receipt boundary: a concurrent writer is wholly before or wholly inside
// the window, never split across it.
func (s *Store) BeginMutationReceipt() graph.MutationReceiptToken {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mutationReceipts.next++
	if s.mutationReceipts.next == 0 {
		s.mutationReceipts.next++
	}
	if s.mutationReceipts.active == nil {
		s.mutationReceipts.active = make(map[graph.MutationReceiptToken]*sqliteMutationReceiptAccumulator)
	}
	token := s.mutationReceipts.next
	s.mutationReceipts.active[token] = newSQLiteMutationReceiptAccumulator()
	return token
}

// EndMutationReceipt closes one observation window. Unknown or already-ended
// tokens fail closed.
func (s *Store) EndMutationReceipt(token graph.MutationReceiptToken) graph.MutationReceipt {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	acc := s.mutationReceipts.active[token]
	if acc == nil {
		return graph.MutationReceipt{Complete: false, IncompleteReason: "unknown_receipt_token"}
	}
	delete(s.mutationReceipts.active, token)
	return acc.receipt()
}

func (s *Store) hasActiveMutationReceiptsLocked() bool {
	return len(s.mutationReceipts.active) != 0
}

func (s *Store) markMutationReceiptsIncompleteLocked() {
	if len(s.mutationReceipts.active) == 0 {
		return
	}
	reason := graph.ReceiptIncompleteCallerReason()
	for _, acc := range s.mutationReceipts.active {
		acc.noteIncomplete(reason)
	}
}

func (s *Store) mergeMutationReceiptLocked(delta *sqliteMutationReceiptAccumulator) {
	if delta == nil {
		return
	}
	for _, acc := range s.mutationReceipts.active {
		if !delta.complete {
			reason := delta.incompleteReason
			if reason == "" {
				reason = "merged_incomplete_delta"
			}
			acc.noteIncomplete(reason)
		}
		acc.resolutionRelevant = acc.resolutionRelevant || delta.resolutionRelevant
		mergeSQLiteReceiptSet(acc.changedFiles, delta.changedFiles)
		mergeSQLiteReceiptSet(acc.unresolvedFiles, delta.unresolvedFiles)
		mergeSQLiteReceiptSet(acc.definitionFiles, delta.definitionFiles)
		mergeSQLiteReceiptSet(acc.targetNames, delta.targetNames)
		mergeSQLiteReceiptSet(acc.evictedNames, delta.evictedNames)
		mergeSQLiteReceiptSet(acc.targetIDs, delta.targetIDs)
		mergeSQLiteReceiptSet(acc.importCandidates, delta.importCandidates)
	}
}

func mergeSQLiteReceiptSet(dst, src map[string]struct{}) {
	for value := range src {
		if value != "" {
			dst[value] = struct{}{}
		}
	}
}

func recordSQLiteAddedNode(acc *sqliteMutationReceiptAccumulator, n *graph.Node) {
	if acc == nil || n == nil || !graph.IsReferenceableSymbol(n.Kind) {
		return
	}
	if n.ID != "" {
		acc.targetIDs[n.ID] = struct{}{}
	}
	if n.Name != "" {
		acc.targetNames[n.Name] = struct{}{}
	}
	if n.QualName != "" {
		acc.targetNames[n.QualName] = struct{}{}
	}
	acc.resolutionRelevant = true
	if n.FilePath != "" {
		acc.definitionFiles[n.FilePath] = struct{}{}
	} else {
		acc.noteIncomplete("node_write_without_exact_file")
	}
}

// recordSQLiteChangedNodeIdentity records both sides of a single-ID node
// upsert. The old and final rows are known exactly, so their file frontier is
// sufficient for ResolveFilesAndIncoming to rebuild the current definition's
// incoming unresolved buckets without a whole-graph fallback. Missing files
// remain fail-closed only for referenceable sides; changes confined to
// non-referenceable nodes cannot affect name resolution.
func recordSQLiteChangedNodeIdentity(
	acc *sqliteMutationReceiptAccumulator,
	old sqliteMutationNodeIdentity,
	final *graph.Node,
) {
	if acc == nil || final == nil {
		return
	}
	oldReferenceable := graph.IsReferenceableSymbol(graph.NodeKind(old.kind))
	finalReferenceable := graph.IsReferenceableSymbol(final.Kind)
	if !oldReferenceable && !finalReferenceable {
		return
	}

	acc.resolutionRelevant = true
	if final.ID != "" {
		acc.targetIDs[final.ID] = struct{}{}
	}
	for _, name := range []string{old.name, old.qualName, final.Name, final.QualName} {
		if name != "" {
			acc.targetNames[name] = struct{}{}
		}
	}
	// The old identity's names vanished with it: the file still enters the
	// definition frontier, but it no longer declares those names, so nothing
	// in the file pass enumerates their stubs. They belong to the name
	// frontier for the same reason an evicted definition's names do.
	//
	// The one name that does NOT need the name pass is a name the final
	// identity still declares in the form the file frontier enumerates, and
	// that form is narrow: UnresolvedNameCandidateIDs reads Name only, and
	// collectIncrementalFileFrontierMode visits referenceable kinds only. So
	// matching final.QualName is not grounds to skip (that form is never
	// enumerated), and neither is matching final.Name when the final kind is
	// no longer referenceable (that node is never visited).
	for _, name := range []string{old.name, old.qualName} {
		if name == "" {
			continue
		}
		if finalReferenceable && name == final.Name {
			continue
		}
		acc.evictedNames[name] = struct{}{}
	}
	for _, filePath := range []string{old.filePath, final.FilePath} {
		if filePath != "" {
			acc.definitionFiles[filePath] = struct{}{}
		}
	}
	if oldReferenceable && old.filePath == "" || finalReferenceable && final.FilePath == "" {
		acc.noteIncomplete("node_identity_change_without_exact_file")
	}
}

func recordSQLiteAddedEdge(acc *sqliteMutationReceiptAccumulator, e *graph.Edge, exactFile string) {
	if acc == nil || e == nil {
		return
	}
	if e.To != "" {
		acc.targetIDs[e.To] = struct{}{}
	}
	if name := graph.UnresolvedName(e.To); name != "" {
		acc.targetNames[name] = struct{}{}
	}
	if e.Kind == graph.EdgeImports {
		if name := graph.UnresolvedName(e.To); name != "" {
			acc.importCandidates[name] = struct{}{}
		} else if e.To != "" {
			acc.importCandidates[e.To] = struct{}{}
		}
		if e.Alias != "" {
			acc.importCandidates[e.Alias] = struct{}{}
		}
	}
	if exactFile != "" {
		acc.changedFiles[exactFile] = struct{}{}
	}
	if !graph.IsUnresolvedTarget(e.To) {
		return
	}
	acc.resolutionRelevant = true
	if graph.HasRestubProvenance(e) {
		// A restubbed surviving edge is rebound by the incoming/name
		// frontier, which restores its stashed provenance; its source file
		// must not join UnresolvedFiles, or the forward file pass
		// re-resolves it first and the restored tier is lost.
		return
	}
	if exactFile != "" {
		acc.unresolvedFiles[exactFile] = struct{}{}
	} else {
		acc.noteIncomplete("edge_write_without_exact_file")
	}
}

func sqliteIdentityForNode(n *graph.Node) sqliteMutationNodeIdentity {
	return sqliteMutationNodeIdentity{
		kind:       string(n.Kind),
		name:       n.Name,
		qualName:   n.QualName,
		filePath:   n.FilePath,
		repoPrefix: n.RepoPrefix,
	}
}

func (i sqliteMutationNodeIdentity) equalsNode(n *graph.Node) bool {
	return i.kind == string(n.Kind) && i.name == n.Name && i.qualName == n.QualName &&
		i.filePath == n.FilePath && i.repoPrefix == n.RepoPrefix
}

// mutationNodeIdentitiesTx preloads node identities in bounded batches. It is
// called only while receipts are active, so the steady indexing path pays no
// extra reads.
func mutationNodeIdentitiesTx(tx *sql.Tx, viewGen int64, ids []string) (map[string]sqliteMutationNodeIdentity, error) {
	const chunkSize = 900
	unique := make(map[string]struct{}, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		ordered = append(ordered, id)
	}
	identities := make(map[string]sqliteMutationNodeIdentity, len(ordered))
	for start := 0; start < len(ordered); start += chunkSize {
		end := start + chunkSize
		if end > len(ordered) {
			end = len(ordered)
		}
		chunk := ordered[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		for _, id := range chunk {
			args = append(args, id)
		}
		// An id names one row per generation; the receipt describes the
		// mutation the writing handle made, so it reads that handle's row.
		args = append(args, viewGen)
		rows, err := tx.Query(
			`SELECT id, kind, name, qual_name, file_path, repo_prefix FROM nodes WHERE id IN (`+placeholders+`) AND view_gen = ?`,
			args...,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var identity sqliteMutationNodeIdentity
			if err := rows.Scan(&id, &identity.kind, &identity.name, &identity.qualName, &identity.filePath, &identity.repoPrefix); err != nil {
				_ = rows.Close()
				return nil, err
			}
			identities[id] = identity
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return identities, nil
}

var _ graph.MutationReceiptStore = (*Store)(nil)
