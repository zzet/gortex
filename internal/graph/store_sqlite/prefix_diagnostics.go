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
)

func (s *Store) PrefixDiagnostics(sampleLimit int) graph.PrefixDiagnostics {
	var d graph.PrefixDiagnostics
	d.OwnedCodeNodes, _ = s.countAndSampleNodes(ownedCodeNodePredicate, 0)
	d.UnownedCodeNodes, d.UnownedSamples = s.countAndSampleNodes(unownedCodeNodePredicate, sampleLimit)
	d.MisprefixedNodes, d.MisprefixedSamples = s.countAndSampleNodes(misprefixedNodePredicate, sampleLimit)
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = ?`, s.viewGen).Scan(&d.Scanned); err != nil {
		panicOnFatal(err)
	}
	return d
}

// countAndSampleNodes returns the number of nodes matching predicate plus up
// to sampleLimit of their IDs. A query failure reports zero rather than
// panicking on a non-fatal error: this is a diagnostic, and a health probe
// that takes the daemon down would be worse than one that stays quiet.
func (s *Store) countAndSampleNodes(predicate string, sampleLimit int) (count int, samples []string) {
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE `+predicate+` AND view_gen = ?`, s.viewGen).Scan(&count); err != nil {
		panicOnFatal(err)
		return 0, nil
	}
	if count == 0 || sampleLimit <= 0 {
		return count, nil
	}
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE `+predicate+` AND view_gen = ? LIMIT ?`, s.viewGen, sampleLimit)
	if err != nil {
		panicOnFatal(err)
		return count, nil
	}
	defer rows.Close()
	samples = collectStringColumn(rows, sampleLimit)
	return count, samples
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
