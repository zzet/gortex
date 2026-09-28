package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Per-pass derivation stamps, and the correction of a sealed generation's
// derived rows.
//
// A generation's derived rows (capability edges, the derived fingerprints on
// its file nodes, …) are a function of its extracted rows AND of the pass that
// derived them. When a pass changes, rows an older derivation wrote stay in
// every generation already published: those are sealed, and a payload write to
// them is refused (checkManagedPayloadWriteTx → ErrPayloadGenerationSealed).
//
// generation_derivation_stamps records, per generation and pass, the
// derivation version that wrote the pass's rows. It is registered with the
// input-manifest tables (schema.go), so it is created by the idempotent schema
// DDL (no schema version bump), swept with the generation on retirement,
// carried by a whole-generation copy and absent from a flattened one (which
// therefore reads as "derived by an unknown version" and is re-derived). A
// generation with no stamp for a pass reads as version 0.
//
// A generation whose stamp is older than the running pass is corrected in
// place by a DerivedCorrection: a derived-rows-only write under the writer gate
// and the maintenance lane, in bounded chunks, each chunk its own transaction
// that re-checks the generation's lifecycle state (never a retiring or failed
// generation) and the stamp it started from, and a final compare-and-set of the
// stamp. Chunks replace rows by source, so a correction interrupted between
// chunks (a crash, a shutdown) leaves the stamp at the old version and simply
// runs again: re-running a chunk is idempotent. Only the rows the request
// declares may be written: edges of the pass's edge kinds, whose source is in
// the chunk's source set, and new versions of node rows the generation already
// holds (same identity, kind and file) — never a new identity.
//
// Readers that cache a published generation's rows (graph-level projections,
// per-stack layer caches) key their entries on GenerationCorrectionEpoch,
// which a finished correction advances.

// generationDerivationStampsTableBody is the stamps table. pass is the
// producer's own name for the derived pass; derivation_version its integer
// version (≥ 1).
const generationDerivationStampsTableBody = ` (
    view_gen           INTEGER NOT NULL,
    pass               TEXT NOT NULL,
    derivation_version INTEGER NOT NULL,
    stamped_at         INTEGER NOT NULL,
    PRIMARY KEY (view_gen, pass)
) WITHOUT ROWID`

// Derivation-stamp and correction errors.
var (
	// ErrDerivationStampInvalid: an empty pass name or a version below 1.
	ErrDerivationStampInvalid = errors.New("store_sqlite: invalid derivation stamp")
	// ErrDerivationStampMoved: the generation's stamp for the pass is no longer
	// the version the correction started from (another correction finished).
	ErrDerivationStampMoved = errors.New("store_sqlite: derivation stamp moved during the correction")
	// ErrDerivedCorrectionRefused: the generation cannot be corrected (it is
	// retiring, failed, or has no catalog row), or a chunk carried a row the
	// correction may not write.
	ErrDerivedCorrectionRefused = errors.New("store_sqlite: derived-row correction refused")
)

// maintenanceDerivedCorrection is the lane job kind of one correction chunk.
const maintenanceDerivedCorrection maintenanceJob = "derived_correction"

// DerivedCorrectionMaxChunkRows bounds the rows one chunk may write (edges
// plus node rows). At the ≈0.1 ms per row a cold one-file delta costs on the
// live-sized store (write_rows probe), 4,096 rows hold the writer well under
// the reclaim's 2 s cap. Callers split larger corrections.
const DerivedCorrectionMaxChunkRows = 4096

// derivedCorrectionWriterWait bounds the wait for the write gate per chunk.
const derivedCorrectionWriterWait = 5 * time.Second

// DerivationStamps returns this handle's generation's stamps (pass → version).
// A pass with no row is absent (version 0).
func (s *Store) DerivationStamps(ctx context.Context) (map[string]int, error) {
	if s.coreless() {
		return nil, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT pass, derivation_version FROM generation_derivation_stamps WHERE view_gen = ?`, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var pass string
		var version int
		if err := rows.Scan(&pass, &version); err != nil {
			return nil, err
		}
		out[pass] = version
	}
	return out, rows.Err()
}

// WriteDerivationStamps records the versions of the passes that derived this
// generation's rows. It goes through the payload write gate, so it is accepted
// while the generation is building (and at the mutable base generation) and
// refused once it is published (ErrPayloadGenerationSealed): a published
// generation's stamp moves only through a DerivedCorrection.
func (s *Store) WriteDerivationStamps(ctx context.Context, stamps map[string]int) error {
	if err := validateDerivationStamps(stamps); err != nil {
		return err
	}
	if s.coreless() {
		return errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	now := time.Now().UnixNano()
	for _, pass := range sortedStampPasses(stamps) {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO generation_derivation_stamps
  (view_gen, pass, derivation_version, stamped_at) VALUES (?, ?, ?, ?)`, s.viewGen, pass, stamps[pass], now); err != nil {
			return fmt.Errorf("store_sqlite: write derivation stamp %s at generation %d: %w", pass, s.viewGen, err)
		}
	}
	return tx.Commit()
}

// StaleDerivationGeneration is one live generation whose stamp for a pass is
// older than the running version.
type StaleDerivationGeneration struct {
	GenerationID int64
	State        ViewGenerationState
	Version      int // 0: no stamp
}

// GenerationsWithStaleDerivation lists the catalogued generations in state
// ready, superseded or building whose stamp for pass is below version (or
// absent), oldest first. Retiring and failed generations are never listed: they
// are not corrected. The base generation (0) has no catalog row and is not
// listed; its rows are ordinary writes.
func (s *Store) GenerationsWithStaleDerivation(ctx context.Context, pass string, version int) ([]StaleDerivationGeneration, error) {
	if pass == "" || version < 1 {
		return nil, fmt.Errorf("%w: pass %q version %d", ErrDerivationStampInvalid, pass, version)
	}
	if s.coreless() {
		return nil, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.generation_id, g.state, COALESCE(st.derivation_version, 0)
  FROM view_generations g
  LEFT JOIN generation_derivation_stamps st ON st.view_gen = g.generation_id AND st.pass = ?
 WHERE g.state IN (?, ?, ?) AND COALESCE(st.derivation_version, 0) < ?
 ORDER BY g.generation_id`, pass,
		string(ViewGenerationReady), string(ViewGenerationSuperseded), string(ViewGenerationBuilding), version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleDerivationGeneration
	for rows.Next() {
		var g StaleDerivationGeneration
		var state string
		if err := rows.Scan(&g.GenerationID, &state, &g.Version); err != nil {
			return nil, err
		}
		g.State = ViewGenerationState(state)
		out = append(out, g)
	}
	return out, rows.Err()
}

// GenerationCorrectionEpoch is the number of derived-row corrections finished
// on generation g in this process. A reader that caches a sealed generation's
// rows keys the cache entry on it.
func (s *Store) GenerationCorrectionEpoch(g int64) uint64 {
	if s.coreless() {
		return 0
	}
	if v, ok := s.derivedCorrectionEpochs.Load(g); ok {
		return v.(*atomic.Uint64).Load()
	}
	return 0
}

func (s *Store) bumpGenerationCorrectionEpoch(g int64) {
	v, _ := s.derivedCorrectionEpochs.LoadOrStore(g, &atomic.Uint64{})
	v.(*atomic.Uint64).Add(1)
}

// DerivedCorrectionRequest names what one correction may write.
type DerivedCorrectionRequest struct {
	GenerationID int64
	Pass         string
	// FromVersion is the stamp the correction expects (0: no stamp);
	// ToVersion the one it records when it finishes (> FromVersion).
	FromVersion, ToVersion int
	// EdgeKinds are the pass's derived edge kinds: the only edges a chunk may
	// delete or insert.
	EdgeKinds []graph.EdgeKind
}

// DerivedCorrectionStats is what a correction wrote so far.
type DerivedCorrectionStats struct {
	Chunks int
	// ChangedChunks are the chunks that changed a row; the others found the
	// pass's rows already as given and wrote nothing.
	ChangedChunks int
	Sources       int
	EdgesDeleted  int64
	EdgesInserted int64
	NodesUpdated  int64
	MaxChunkHold  time.Duration
	Elapsed       time.Duration
}

// DerivedCorrection is one in-progress correction of a generation's derived
// rows for one pass. It is not safe for concurrent use.
type DerivedCorrection struct {
	s        *Store
	req      DerivedCorrectionRequest
	kinds    map[graph.EdgeKind]struct{}
	kindList []any
	started  time.Time
	stats    DerivedCorrectionStats
	finished bool
}

// BeginDerivedCorrection validates the request against the generation's
// current state and stamp and returns the correction. Nothing is written until
// the first chunk.
func (s *Store) BeginDerivedCorrection(ctx context.Context, req DerivedCorrectionRequest) (*DerivedCorrection, error) {
	if s.coreless() {
		return nil, errMaintenanceNoCore
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if req.GenerationID <= baseViewGeneration {
		return nil, fmt.Errorf("%w: generation %d is not a catalogued generation", ErrDerivedCorrectionRefused, req.GenerationID)
	}
	if req.Pass == "" || req.ToVersion < 1 || req.FromVersion < 0 || req.ToVersion <= req.FromVersion {
		return nil, fmt.Errorf("%w: pass %q %d -> %d", ErrDerivationStampInvalid, req.Pass, req.FromVersion, req.ToVersion)
	}
	if len(req.EdgeKinds) == 0 {
		return nil, fmt.Errorf("%w: a correction needs the pass's edge kinds", ErrDerivedCorrectionRefused)
	}
	c := &DerivedCorrection{s: s, req: req, kinds: make(map[graph.EdgeKind]struct{}, len(req.EdgeKinds)), started: time.Now()}
	for _, kind := range req.EdgeKinds {
		if kind == "" {
			return nil, fmt.Errorf("%w: empty edge kind", ErrDerivedCorrectionRefused)
		}
		if _, dup := c.kinds[kind]; !dup {
			c.kinds[kind] = struct{}{}
			c.kindList = append(c.kindList, string(kind))
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only transaction
	if err := c.checkTx(ctx, tx); err != nil {
		return nil, err
	}
	return c, nil
}

// checkTx refuses a generation that is not correctable or whose stamp moved.
func (c *DerivedCorrection) checkTx(ctx context.Context, tx *sql.Tx) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id = ?`, c.req.GenerationID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: generation %d has no catalog row", ErrDerivedCorrectionRefused, c.req.GenerationID)
	}
	if err != nil {
		return err
	}
	switch ViewGenerationState(state) {
	case ViewGenerationReady, ViewGenerationSuperseded, ViewGenerationBuilding:
	default:
		return fmt.Errorf("%w: generation %d is %s", ErrDerivedCorrectionRefused, c.req.GenerationID, state)
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT derivation_version FROM generation_derivation_stamps WHERE view_gen = ? AND pass = ?`,
		c.req.GenerationID, c.req.Pass).Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if version != c.req.FromVersion {
		return fmt.Errorf("%w: generation %d pass %s is at %d, the correction started from %d",
			ErrDerivationStampMoved, c.req.GenerationID, c.req.Pass, version, c.req.FromVersion)
	}
	return nil
}

// ReplaceSourceEdges replaces, in one bounded transaction, every edge of the
// pass's kinds whose source is in sources with edges. Each edge must be of one
// of the pass's kinds and have its source in sources; nodes are new versions
// of node rows the generation already holds (same identity, kind and file
// path), for derived columns such as the fingerprints a pass stamps on file
// nodes. The chunk is refused whole when any row falls outside that, or when it
// carries more than DerivedCorrectionMaxChunkRows rows.
func (c *DerivedCorrection) ReplaceSourceEdges(ctx context.Context, sources []string, edges []*graph.Edge, nodes []*graph.Node) error {
	if c.finished {
		return fmt.Errorf("%w: correction already finished", ErrDerivedCorrectionRefused)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(edges)+len(nodes) > DerivedCorrectionMaxChunkRows {
		return fmt.Errorf("%w: chunk of %d rows exceeds %d", ErrDerivedCorrectionRefused, len(edges)+len(nodes), DerivedCorrectionMaxChunkRows)
	}
	sourceSet := make(map[string]struct{}, len(sources))
	for _, id := range sources {
		if id == "" {
			return fmt.Errorf("%w: empty source id", ErrDerivedCorrectionRefused)
		}
		sourceSet[id] = struct{}{}
	}
	for _, e := range edges {
		if e == nil {
			return fmt.Errorf("%w: nil edge", ErrDerivedCorrectionRefused)
		}
		if _, ok := c.kinds[e.Kind]; !ok {
			return fmt.Errorf("%w: edge kind %s is not one of the pass's kinds", ErrDerivedCorrectionRefused, e.Kind)
		}
		if _, ok := sourceSet[e.From]; !ok {
			return fmt.Errorf("%w: edge source %s is outside the chunk's sources", ErrDerivedCorrectionRefused, e.From)
		}
	}
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			return fmt.Errorf("%w: nil or anonymous node", ErrDerivedCorrectionRefused)
		}
	}
	if len(sourceSet) == 0 && len(nodes) == 0 {
		return nil
	}
	s := c.s
	return s.runMaintenance(ctx, maintenanceDerivedCorrection, false, func(ctx context.Context) error {
		wctx, cancel := context.WithTimeout(ctx, derivedCorrectionWriterWait)
		err := s.writeMu.LockContext(wctx)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: %s: writer gate: %w", ErrMaintenanceBusy, maintenanceDerivedCorrection, err)
		}
		held := time.Now()
		defer func() {
			c.stats.MaxChunkHold = max(c.stats.MaxChunkHold, time.Since(held))
			s.writeMu.Unlock()
		}()
		if s.bulkConn != nil {
			return fmt.Errorf("%w: %s: a bulk window is open", ErrMaintenanceBusy, maintenanceDerivedCorrection)
		}
		// The seal is bypassed on purpose, and only here: the transaction
		// is on the raw writer, the generation's state and stamp are
		// re-checked inside it, and only the declared rows are written.
		tx, releaseChunk, err := s.beginMaintenanceWriteLocked(ctx)
		if err != nil {
			return err
		}
		defer releaseChunk()
		defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
		if err := c.checkTx(ctx, tx); err != nil {
			return err
		}
		if err := c.checkNodesExistTx(ctx, tx, nodes); err != nil {
			return err
		}
		// The corrected generation's analyses are stale: invalidate them
		// durably in the same transaction.
		invalidated := s.analysisGenerationPresent
		if invalidated {
			if err := invalidateAnalysisGenerationTx(tx); err != nil {
				return err
			}
		}
		before, err := c.sourceEdgeRowsTx(ctx, tx, sourceSet)
		if err != nil {
			return err
		}
		deleted, err := c.deleteSourceEdgesTx(ctx, tx, sourceSet)
		if err != nil {
			return err
		}
		var buffers jsonbIngestBuffers
		var inserted int
		if len(edges) > 0 {
			if jsonbIngestEnabled() && jsonbIngestSupported(tx) {
				inserted, _, _, err = insertEdgeChunksJSONBTxWithBuffers(tx, c.req.GenerationID, edges, false, &buffers)
			} else {
				limit := s.sqliteBatchVariableLimitLocked()
				inserted, _, _, err = insertEdgeChunksTxLimited(tx, c.req.GenerationID, edges, false, &limit)
			}
			if err != nil {
				return err
			}
		}
		var updated int
		if len(nodes) > 0 {
			if jsonbIngestEnabled() && jsonbIngestSupported(tx) {
				updated, _, _, err = insertNodeChunksJSONBTxWithBuffers(tx, c.req.GenerationID, nodes, false, &buffers)
			} else {
				limit := s.sqliteBatchVariableLimitLocked()
				updated, _, _, err = insertNodeChunksTxLimited(tx, c.req.GenerationID, nodes, false, &limit)
			}
			if err != nil {
				return err
			}
		}
		after, err := c.sourceEdgeRowsTx(ctx, tx, sourceSet)
		if err != nil {
			return err
		}
		if updated == 0 && equalRowMultisets(before, after) {
			// Nothing changed: roll back (the deferred Rollback) so the
			// chunk writes no WAL, invalidates no analysis and moves no
			// epoch.
			c.stats.Chunks++
			c.stats.Sources += len(sourceSet)
			return nil
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if invalidated {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(true)
		c.stats.ChangedChunks++
		c.stats.Chunks++
		c.stats.Sources += len(sourceSet)
		c.stats.EdgesDeleted += deleted
		c.stats.EdgesInserted += int64(inserted)
		c.stats.NodesUpdated += int64(updated)
		return nil
	})
}

// checkNodesExistTx refuses a node row the generation does not already hold
// with the same kind, file path and name: a correction restamps derived
// columns, it never adds, moves, renames or re-kinds an identity (sealed
// generations' name indexes stay valid).
func (c *DerivedCorrection) checkNodesExistTx(ctx context.Context, tx *sql.Tx, nodes []*graph.Node) error {
	for _, n := range nodes {
		var kind, file, name string
		err := tx.QueryRowContext(ctx, `SELECT kind, file_path, name FROM nodes WHERE id = ? AND view_gen = ?`, n.ID, c.req.GenerationID).Scan(&kind, &file, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: node %s is not in generation %d", ErrDerivedCorrectionRefused, n.ID, c.req.GenerationID)
		}
		if err != nil {
			return err
		}
		if kind != string(n.Kind) || file != n.FilePath || name != n.Name {
			return fmt.Errorf("%w: node %s would change kind, file or name (%s %s %q -> %s %s %q)",
				ErrDerivedCorrectionRefused, n.ID, kind, file, name, n.Kind, n.FilePath, n.Name)
		}
	}
	return nil
}

// deleteSourceEdgesTx removes the pass's edges of the sources through the
// (view_gen, from_id, kind) index, in statements of at most 400 sources.
func (c *DerivedCorrection) deleteSourceEdgesTx(ctx context.Context, tx *sql.Tx, sources map[string]struct{}) (int64, error) {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	kindMarks := strings.TrimSuffix(strings.Repeat("?,", len(c.kindList)), ",")
	var deleted int64
	const perStatement = 400
	for start := 0; start < len(ids); start += perStatement {
		end := min(start+perStatement, len(ids))
		args := make([]any, 0, 1+end-start+len(c.kindList))
		args = append(args, c.req.GenerationID)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		args = append(args, c.kindList...)
		res, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE view_gen = ? AND from_id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", end-start), ",")+`) AND kind IN (`+kindMarks+`)`, args...)
		if err != nil {
			return deleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// Finish records the new stamp (compare-and-set from FromVersion), advances
// the generation's correction epoch and logs the correction. A correction whose
// stamp moved meanwhile is refused with ErrDerivationStampMoved.
func (c *DerivedCorrection) Finish(ctx context.Context) (DerivedCorrectionStats, error) {
	if c.finished {
		return c.stats, fmt.Errorf("%w: correction already finished", ErrDerivedCorrectionRefused)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s := c.s
	wctx, cancel := context.WithTimeout(ctx, derivedCorrectionWriterWait)
	err := s.writeMu.LockContext(wctx)
	cancel()
	if err != nil {
		return c.stats, fmt.Errorf("%w: %s: writer gate: %w", ErrMaintenanceBusy, maintenanceDerivedCorrection, err)
	}
	defer s.writeMu.Unlock()
	tx, releaseFinish, err := s.beginMaintenanceWriteLocked(ctx)
	if err != nil {
		return c.stats, err
	}
	defer releaseFinish()
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if err := c.checkTx(ctx, tx); err != nil {
		return c.stats, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO generation_derivation_stamps
  (view_gen, pass, derivation_version, stamped_at) VALUES (?, ?, ?, ?)`,
		c.req.GenerationID, c.req.Pass, c.req.ToVersion, time.Now().UnixNano()); err != nil {
		return c.stats, err
	}
	if err := tx.Commit(); err != nil {
		return c.stats, err
	}
	c.finished = true
	if c.stats.ChangedChunks > 0 {
		// Caches of the generation's rows stay valid when no row changed.
		s.bumpGenerationCorrectionEpoch(c.req.GenerationID)
	}
	c.stats.Elapsed = time.Since(c.started)
	log.Printf("store_sqlite: derived rows corrected generation=%d pass=%s version=%d->%d chunks=%d sources=%d edges_deleted=%d edges_inserted=%d nodes_updated=%d max_chunk_hold=%s elapsed=%s",
		c.req.GenerationID, c.req.Pass, c.req.FromVersion, c.req.ToVersion, c.stats.Chunks, c.stats.Sources,
		c.stats.EdgesDeleted, c.stats.EdgesInserted, c.stats.NodesUpdated,
		c.stats.MaxChunkHold.Round(time.Millisecond), c.stats.Elapsed.Round(time.Millisecond))
	return c.stats, nil
}

// Stats reports what the correction has written so far.
func (c *DerivedCorrection) Stats() DerivedCorrectionStats { return c.stats }

func validateDerivationStamps(stamps map[string]int) error {
	for pass, version := range stamps {
		if pass == "" || version < 1 {
			return fmt.Errorf("%w: pass %q version %d", ErrDerivationStampInvalid, pass, version)
		}
	}
	return nil
}

func sortedStampPasses(stamps map[string]int) []string {
	out := make([]string, 0, len(stamps))
	for pass := range stamps {
		out = append(out, pass)
	}
	sort.Strings(out)
	return out
}

// sourceEdgeRowsTx reads the pass's edge rows of the sources, every column,
// as sorted row strings (the same query before and after a chunk's rewrite,
// so equal strings mean equal rows).
func (c *DerivedCorrection) sourceEdgeRowsTx(ctx context.Context, tx *sql.Tx, sources map[string]struct{}) ([]string, error) {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	kindMarks := strings.TrimSuffix(strings.Repeat("?,", len(c.kindList)), ",")
	var out []string
	const perStatement = 400
	for start := 0; start < len(ids); start += perStatement {
		end := min(start+perStatement, len(ids))
		args := make([]any, 0, 1+end-start+len(c.kindList))
		args = append(args, c.req.GenerationID)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		args = append(args, c.kindList...)
		rows, err := tx.QueryContext(ctx, `SELECT quote(from_id)||','||quote(to_id)||','||quote(kind)||','||quote(file_path)||','||quote(line)||','||
  quote(confidence)||','||quote(confidence_label)||','||quote(origin)||','||quote(tier)||','||quote(cross_repo)||','||quote(meta)||','||
  quote(resolve_terminal)||','||quote(resolve_terminal_reason)||','||quote(semantic_source)
  FROM edges WHERE view_gen = ? AND from_id IN (`+strings.TrimSuffix(strings.Repeat("?,", end-start), ",")+`) AND kind IN (`+kindMarks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func equalRowMultisets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
