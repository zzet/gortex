package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// PrefixDiagnostics answers graph.PrefixDiagnosticsReader natively.
//
// The predicates below are the SQL transcription of
// graph.IsAuditableRepoSourceNode and graph.ClassifyNodePrefix, and the two
// must be changed together. The auditable predicate admits repository source
// while excluding exact virtual external paths and the contract/topic/rationale
// identity namespaces whose IDs deliberately are not path-prefixed.
//
// Each population is answered by one aggregate plus one bounded sample
// query rather than a node pull: on a warm multi-repo store the node table
// runs to millions of rows, and this is called from health and boot paths
// that must not materialise it.
const (
	// Keep this exact predicate in lockstep with
	// graph.IsAuditableRepoSourceNode. Prefix checks use instr(...)=1 instead
	// of a broad punctuation/URI heuristic so real parser nodes cannot escape
	// the audit merely because their path contains punctuation.
	auditableRepoSourceNodePredicate = `file_path <> '' AND ` +
		`kind NOT IN ('contract', 'contract_bridge', 'topic', 'rationale') AND ` +
		`instr(file_path, 'external::') <> 1 AND ` +
		`instr(file_path, 'external-call::') <> 1`

	// A code node no repository claims. Post-flip this must be zero;
	// a non-zero count is the ghost-graph signature.
	unownedCodeNodePredicate = `repo_prefix = '' AND (` + auditableRepoSourceNodePredicate + `)`

	// An owned node whose minted identity does not carry the prefix its
	// repo_prefix column claims. instr(x, y) is 1 when y starts x.
	misprefixedNodePredicate = `repo_prefix <> '' AND (` + auditableRepoSourceNodePredicate + `) AND (` +
		`instr(file_path, repo_prefix || '/') <> 1 OR instr(id, repo_prefix || '/') <> 1)`

	// A code node whose repository claim and minted identity agree. The
	// negation of misprefixedNodePredicate over the same owned population,
	// so owned + misprefixed + unowned partitions every audited source node.
	ownedCodeNodePredicate = `repo_prefix <> '' AND (` + auditableRepoSourceNodePredicate + `) AND ` +
		`instr(file_path, repo_prefix || '/') = 1 AND instr(id, repo_prefix || '/') = 1`

	// Keep all four exact diagnostic counts in one statement. Each predicate
	// still classifies the same generation rows, while SQLite walks that
	// generation once instead of repeating the full scan for every field.
	prefixDiagnosticsCountQuery = `SELECT COUNT(*), ` +
		`COALESCE(SUM(CASE WHEN ` + ownedCodeNodePredicate + ` THEN 1 ELSE 0 END), 0), ` +
		`COALESCE(SUM(CASE WHEN ` + unownedCodeNodePredicate + ` THEN 1 ELSE 0 END), 0), ` +
		`COALESCE(SUM(CASE WHEN ` + misprefixedNodePredicate + ` THEN 1 ELSE 0 END), 0) ` +
		`FROM nodes WHERE view_gen = ?`
)

func (s *Store) PrefixDiagnostics(sampleLimit int) graph.PrefixDiagnostics {
	var d graph.PrefixDiagnostics
	if err := s.db.QueryRow(prefixDiagnosticsCountQuery, s.viewGen).Scan(
		&d.Scanned,
		&d.OwnedCodeNodes,
		&d.UnownedCodeNodes,
		&d.MisprefixedNodes,
	); err != nil {
		panicOnFatal(err)
		return graph.PrefixDiagnostics{}
	}
	if sampleLimit > 0 && d.UnownedCodeNodes > 0 {
		d.UnownedSamples = s.sampleNodeIDs(unownedCodeNodePredicate, sampleLimit)
	}
	if sampleLimit > 0 && d.MisprefixedNodes > 0 {
		d.MisprefixedSamples = s.sampleNodeIDs(misprefixedNodePredicate, sampleLimit)
	}
	return d
}

// sampleNodeIDs returns up to sampleLimit IDs matching predicate. A query
// failure reports no samples rather than panicking on a non-fatal error: this
// is a diagnostic, and a health probe that takes the daemon down would be
// worse than one that stays quiet.
func (s *Store) sampleNodeIDs(predicate string, sampleLimit int) []string {
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE `+predicate+` AND view_gen = ? LIMIT ?`, s.viewGen, sampleLimit)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	return collectStringColumn(rows, sampleLimit)
}

func collectStringColumn(rows *sql.Rows, capHint int) []string {
	out := make([]string, 0, capHint)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, v)
	}
	return out
}
