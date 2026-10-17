package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// NodeIdentityMaskKind separates node identity from outgoing-set ownership.
// Legacy is deliberately stronger: it retains ALL old tombstone behavior,
// including a same-generation row carried underneath the tombstone.
type NodeIdentityMaskKind string

const (
	NodeIdentityMaskLegacy  NodeIdentityMaskKind = "legacy_tombstone"
	NodeIdentityMaskReplace NodeIdentityMaskKind = "identity_replace"
)

type NodeIdentityMask struct {
	NodeID string
	Kind   NodeIdentityMaskKind
}

// SetNodeIdentityReplacements declares explicit complete node-row replacement,
// independent of file ownership and outgoing adjacency. Publish refuses a
// replacement without a row in this exact generation. A prior legacy claim is
// never weakened, even if this method is called after SetNodeTombstones.
func (s *Store) SetNodeIdentityReplacements(nodeIDs []string) error {
	return s.setNodeIdentityMasks(nodeIDs, NodeIdentityMaskReplace)
}

func (s *Store) setNodeIdentityMasks(nodeIDs []string, kind NodeIdentityMaskKind) error {
	if err := s.requireDerivedGeneration(); err != nil {
		return err
	}
	if kind != NodeIdentityMaskLegacy && kind != NodeIdentityMaskReplace {
		return fmt.Errorf("%w: node identity kind %q", ErrGenerationMaskInvalidValue, kind)
	}
	for _, id := range nodeIDs {
		if err := requireMaskID("node_id", id); err != nil {
			return err
		}
	}
	// Both input orderings converge to legacy if either request owns outgoing
	// adjacency. Unknown stored kinds fail NOT NULL, rather than being silently
	// downgraded by an older writer. The VALUES batch and join share one write
	// transaction and the existing payload mutation gate.
	return s.writeMaskRowsWithSuffix(`INSERT INTO generation_node_tombstones
  (view_gen, node_id, claim_kind) VALUES `, `
ON CONFLICT(view_gen, node_id) DO UPDATE SET claim_kind = CASE
  WHEN generation_node_tombstones.claim_kind = 'legacy_tombstone' THEN 'legacy_tombstone'
  WHEN generation_node_tombstones.claim_kind = 'identity_replace'
    AND excluded.claim_kind = 'legacy_tombstone' THEN 'legacy_tombstone'
  WHEN generation_node_tombstones.claim_kind = 'identity_replace' THEN 'identity_replace'
  ELSE NULL END`, len(nodeIDs), func(i int) []any {
		return []any{s.viewGen, nodeIDs[i], string(kind)}
	})
}

// NodeIdentityMasksContext is a checked, generation-keyed marker enumeration.
// It returns no authoritative partial result on query/scan/iteration failure.
func (s *Store) NodeIdentityMasksContext(ctx context.Context) ([]NodeIdentityMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, claim_kind
FROM generation_node_tombstones WHERE view_gen = ? ORDER BY node_id`, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeIdentityMask, 0)
	for rows.Next() {
		var mask NodeIdentityMask
		if err := rows.Scan(&mask.NodeID, &mask.Kind); err != nil {
			return nil, err
		}
		if mask.NodeID == "" || (mask.Kind != NodeIdentityMaskLegacy && mask.Kind != NodeIdentityMaskReplace) {
			return nil, fmt.Errorf("%w: node identity mask %q kind %q", ErrGenerationMaskInvalidValue, mask.NodeID, mask.Kind)
		}
		out = append(out, mask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) NodeIdentityMasks() ([]NodeIdentityMask, error) {
	return s.NodeIdentityMasksContext(context.Background())
}

// NodeIdentityMaskSummariesContext loads ONLY the explicit identity-mask
// IDs already captured by the caller's checked mask enumeration. It neither
// scans the upper generation nor classifies a node from its shape/path. Every
// identity-only ID must have a complete same-generation summary; absence is
// an integrity error, not permission to inherit a lower/gen-zero row. Legacy
// tombstones may legitimately have no row. Carried legacy rows are included
// so a monotone upgrade does not drop the node from scoped enumeration.
// Callers must use an immutable generation; this is not a live-build cache.
func (s *Store) NodeIdentityMaskSummariesContext(ctx context.Context, masks []NodeIdentityMask) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[string]NodeIdentityMaskKind, len(masks))
	ids := make([]string, 0, len(masks))
	for _, mask := range masks {
		if err := requireMaskID("node_id", mask.NodeID); err != nil {
			return nil, err
		}
		if mask.Kind != NodeIdentityMaskLegacy && mask.Kind != NodeIdentityMaskReplace {
			return nil, fmt.Errorf("%w: node identity kind %q", ErrGenerationMaskInvalidValue, mask.Kind)
		}
		if kind, duplicate := seen[mask.NodeID]; duplicate {
			if kind != mask.Kind {
				return nil, fmt.Errorf("%w: conflicting captured identity masks", ErrGenerationMaskInvalidValue)
			}
			continue
		}
		seen[mask.NodeID] = mask.Kind
		ids = append(ids, mask.NodeID)
	}
	sort.Strings(ids)
	out := make([]*graph.Node, 0, len(ids))
	for start := 0; start < len(ids); start += generationMaskChunk {
		end := min(start+generationMaskChunk, len(ids))
		args := make([]any, 1, end-start+1)
		args[0] = s.viewGen
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, nodeIdentitySummaryQuery(end-start), args...)
		if err != nil {
			return nil, err
		}
		found := make(map[string]struct{}, end-start)
		for rows.Next() {
			node, err := scanNodeSummary(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			found[node.ID] = struct{}{}
			out = append(out, node)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		for _, id := range ids[start:end] {
			if _, exists := found[id]; !exists && seen[id] == NodeIdentityMaskReplace {
				return nil, fmt.Errorf("%w: generation %d has missing identity replacement row %q", ErrGenerationMaskIntegrity, s.viewGen, id)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func nodeIdentitySummaryQuery(count int) string {
	return `SELECT ` + lookupNodeSummaryCols + `
FROM nodes WHERE view_gen = ? AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", count), ",") + `) ORDER BY id`
}

// validateNodeIdentityMasks runs after the publisher seals and drains writers.
// Only explicit identity replacements require rows. Legacy tombstones remain
// valid with or without a row, exactly as before this format extension.
func (s *Store) validateNodeIdentityMasks() error {
	rows, err := s.db.Query(`SELECT m.node_id, m.claim_kind
FROM generation_node_tombstones AS m
WHERE m.view_gen = ? AND (
  m.claim_kind NOT IN ('legacy_tombstone', 'identity_replace') OR
  (m.claim_kind = 'identity_replace' AND NOT EXISTS (
    SELECT 1 FROM nodes AS n WHERE n.id = m.node_id AND n.view_gen = m.view_gen)))
ORDER BY m.node_id LIMIT ?`, s.viewGen, generationMaskViolationLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return err
		}
		violations = append(violations, fmt.Sprintf("node %q kind %q", id, kind))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(violations) != 0 {
		return fmt.Errorf("%w: generation %d: invalid node identity claims: %s", ErrGenerationMaskIntegrity, s.viewGen, strings.Join(violations, "; "))
	}
	return nil
}

// addNodeIdentityMaskKinds is the proposed v24 step. It preserves the existing
// primary key and all rows; missing kind means precisely the legacy contract.
// Version allocation and the expected v23 registry are landing-time guards.
func addNodeIdentityMaskKinds(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo('generation_node_tombstones') WHERE name = 'claim_kind'`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	_, err := tx.Exec(`ALTER TABLE generation_node_tombstones ADD COLUMN claim_kind TEXT NOT NULL DEFAULT 'legacy_tombstone'`)
	return err
}
