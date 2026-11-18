package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// Eligibility validates metadata even on non-seeds. Keep every candidate's
// raw metadata in this covering index instead of fetching rich primary rows.
// Metadata is opaque here: the existing checked binary/legacy decoder still
// determines membership and refuses malformed non-seeds.
const contractSeedCandidateIndexName = "nodes_contract_seed_candidates"
const contractSeedCandidateKindsSQL = "('" + string(graph.KindField) + "','" + string(graph.KindType) + "','" + string(graph.KindInterface) + "','" + string(graph.KindMethod) + "')"
const contractSeedCandidateIndexDDL = `CREATE INDEX IF NOT EXISTS nodes_contract_seed_candidates ON nodes(repo_prefix,kind,view_gen,id,language,meta) WHERE kind IN ` + contractSeedCandidateKindsSQL

func contractSeedCandidateRowsQuery() string {
	return `WITH requested_kinds(kind) AS (
 SELECT CAST(value AS TEXT) FROM json_each(?)
 )
 SELECT n.id,n.kind,n.language,n.meta
 FROM requested_kinds AS k
 CROSS JOIN nodes AS n INDEXED BY ` + contractSeedCandidateIndexName + `
 ON n.repo_prefix=? AND n.kind=k.kind AND n.view_gen=?
 WHERE n.kind IN ` + contractSeedCandidateKindsSQL
}
