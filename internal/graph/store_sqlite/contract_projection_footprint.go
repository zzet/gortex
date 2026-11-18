package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
)

// ContractProjectionDeclaration is physical potential membership, including
// non-constants which count toward a name query's limit. It contains no value
// equality proof: touching its ID can still require fresh full-node reads.
type ContractProjectionDeclaration struct {
	ID, Name, Kind, FilePath, RepoPrefix string
}

type ContractProjectionEdge struct {
	From, To, Kind, FilePath string
	Line                     int
}

type ContractProjectionConstant struct {
	ID, RepoPrefix, FilePath string
}

// ContractProjectionFootprint owns a bounded physical sparse layer frontier.
// It is NOT a selected-view certificate: callers must account for inherited
// rows revealed by removed masks, census/source policy, and current debt. A
// successful read says nothing about equality of full node/edge/value answers.
// No store, reader, transaction, source or lease survives in this result.
type ContractProjectionFootprint struct {
	Cursor        ContractProjectionCursor
	Declarations  []ContractProjectionDeclaration
	Edges         []ContractProjectionEdge
	Constants     []ContractProjectionConstant
	Bindings      []graph.SemanticBindingSite
	Files         []FileMask
	Identities    []NodeIdentityMask
	EdgeSources   []EdgeSourceMask
	Producers     []ProducerCompleteness
	RetainedBytes int64
}

type contractFootprintQuery struct {
	table, columns, order string
	width                 int64
	count                 int64
	bytes                 int64
}

// ContractProjectionFootprintContext reads only this positive generation in
// one read-only snapshot, outside the global writer gate. The caller reserves
// budgetBytes WITHIN the existing contract budget. Row/byte admission happens
// in SQLite before any identity strings or row arrays are scanned into Go.
// A changed committed clock, error, unsupported mask or bound refuses the
// WHOLE result; no partial frontier may be used as exclusion proof.
func (s *Store) ContractProjectionFootprintContext(ctx context.Context, budgetBytes int64) (ContractProjectionFootprint, error) {
	return s.contractProjectionFootprintContext(ctx, budgetBytes, nil)
}

func (s *Store) contractProjectionFootprintContext(ctx context.Context, budgetBytes int64, afterSnapshot func()) (ContractProjectionFootprint, error) {
	var out ContractProjectionFootprint
	if ctx == nil || s == nil || s.coreless() || s.viewGen <= 0 || budgetBytes < 4096 || budgetBytes > 64<<20 {
		return out, ErrCatalogInvalidValue
	}
	before, err := s.CaptureContractProjectionClock(ctx)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	queries := []contractFootprintQuery{
		{"nodes", "id,name,kind,file_path,repo_prefix", "id", int64(unsafe.Sizeof(ContractProjectionDeclaration{})), 0, 0},
		{"edges", "from_id,to_id,kind,file_path,line", "from_id,to_id,kind,file_path,line", int64(unsafe.Sizeof(ContractProjectionEdge{})), 0, 0},
		{"constant_values", "node_id,repo_prefix,file_path", "node_id", int64(unsafe.Sizeof(ContractProjectionConstant{})), 0, 0},
		{"semantic_binding_types", "repo_prefix,file_path,line,name", "repo_prefix,file_path,line,name", int64(unsafe.Sizeof(graph.SemanticBindingSite{})), 0, 0},
		{"generation_file_masks", "repo_prefix,file_path,ownership_mode", "repo_prefix,file_path", int64(unsafe.Sizeof(FileMask{})), 0, 0},
		{"generation_node_tombstones", "node_id,claim_kind", "node_id", int64(unsafe.Sizeof(NodeIdentityMask{})), 0, 0},
		{"generation_edge_sources", "source_id,ownership_mode", "source_id", int64(unsafe.Sizeof(EdgeSourceMask{})), 0, 0},
		{"generation_producer_completeness", "producer,state,reason", "producer", int64(unsafe.Sizeof(ProducerCompleteness{})), 0, 0},
	}
	charged, total := int64(unsafe.Sizeof(out)), int64(0)
	for i := range queries {
		q := &queries[i]
		var lengths []string
		for _, column := range strings.Split(q.columns, ",") {
			lengths = append(lengths, "COALESCE(length(CAST("+column+" AS BLOB)),0)")
		}
		query := "SELECT COUNT(*),COALESCE(SUM(" + strings.Join(lengths, "+") + "),0) FROM " + q.table + " WHERE view_gen=?"
		if err := tx.QueryRowContext(ctx, query, s.viewGen).Scan(&q.count, &q.bytes); err != nil {
			return ContractProjectionFootprint{}, err
		}
		if q.count < 0 || q.count > graph.ContractProjectionRowLimit || q.bytes < 0 || total > graph.ContractProjectionRowLimit-q.count || q.bytes > budgetBytes || q.count > budgetBytes/q.width {
			return ContractProjectionFootprint{}, graph.ErrContractProjectionLimit
		}
		total += q.count
		// String backing rounds to <=max(16,2*len); widths cover headers.
		// The bound deliberately includes integer columns as extra strings.
		charged += footprintAllocation(q.count*q.width) + 16*int64(len(lengths))*q.count + 2*q.bytes
		if charged > budgetBytes {
			return ContractProjectionFootprint{}, graph.ErrContractProjectionLimit
		}
	}
	if afterSnapshot != nil {
		afterSnapshot()
	}
	out.Declarations = make([]ContractProjectionDeclaration, queries[0].count)
	out.Edges = make([]ContractProjectionEdge, queries[1].count)
	out.Constants = make([]ContractProjectionConstant, queries[2].count)
	out.Bindings = make([]graph.SemanticBindingSite, queries[3].count)
	out.Files = make([]FileMask, queries[4].count)
	out.Identities = make([]NodeIdentityMask, queries[5].count)
	out.EdgeSources = make([]EdgeSourceMask, queries[6].count)
	out.Producers = make([]ProducerCompleteness, queries[7].count)
	for i, q := range queries {
		if err := scanFootprintRows(ctx, tx, s.viewGen, q, func(row int, rows *sql.Rows) error {
			switch i {
			case 0:
				r := &out.Declarations[row]
				return rows.Scan(&r.ID, &r.Name, &r.Kind, &r.FilePath, &r.RepoPrefix)
			case 1:
				r := &out.Edges[row]
				return rows.Scan(&r.From, &r.To, &r.Kind, &r.FilePath, &r.Line)
			case 2:
				r := &out.Constants[row]
				return rows.Scan(&r.ID, &r.RepoPrefix, &r.FilePath)
			case 3:
				r := &out.Bindings[row]
				return rows.Scan(&r.RepoPrefix, &r.FilePath, &r.Line, &r.Name)
			case 4:
				r := &out.Files[row]
				return rows.Scan(&r.RepoPrefix, &r.FilePath, &r.Mode)
			case 5:
				r := &out.Identities[row]
				return rows.Scan(&r.NodeID, &r.Kind)
			case 6:
				r := &out.EdgeSources[row]
				return rows.Scan(&r.SourceID, &r.Mode)
			case 7:
				r := &out.Producers[row]
				return rows.Scan(&r.Producer, &r.State, &r.Reason)
			}
			return ErrCatalogInvalidValue
		}); err != nil {
			return ContractProjectionFootprint{}, err
		}
	}
	// Detach every scanned string. The retained charge must not depend on a
	// driver's choice of backing buffer or substring allocation.
	for i := range out.Declarations {
		r := &out.Declarations[i]
		r.ID, r.Name, r.Kind, r.FilePath, r.RepoPrefix = strings.Clone(r.ID), strings.Clone(r.Name), strings.Clone(r.Kind), strings.Clone(r.FilePath), strings.Clone(r.RepoPrefix)
	}
	for i := range out.Edges {
		r := &out.Edges[i]
		r.From, r.To, r.Kind, r.FilePath = strings.Clone(r.From), strings.Clone(r.To), strings.Clone(r.Kind), strings.Clone(r.FilePath)
	}
	for i := range out.Constants {
		r := &out.Constants[i]
		r.ID, r.RepoPrefix, r.FilePath = strings.Clone(r.ID), strings.Clone(r.RepoPrefix), strings.Clone(r.FilePath)
	}
	for i := range out.Bindings {
		r := &out.Bindings[i]
		r.RepoPrefix, r.FilePath, r.Name = strings.Clone(r.RepoPrefix), strings.Clone(r.FilePath), strings.Clone(r.Name)
	}
	for i := range out.Files {
		r := &out.Files[i]
		r.RepoPrefix, r.FilePath, r.Mode = strings.Clone(r.RepoPrefix), strings.Clone(r.FilePath), OwnershipMode(strings.Clone(string(r.Mode)))
	}
	for i := range out.Identities {
		r := &out.Identities[i]
		r.NodeID, r.Kind = strings.Clone(r.NodeID), NodeIdentityMaskKind(strings.Clone(string(r.Kind)))
	}
	for i := range out.EdgeSources {
		r := &out.EdgeSources[i]
		r.SourceID, r.Mode = strings.Clone(r.SourceID), OwnershipMode(strings.Clone(string(r.Mode)))
	}
	for i := range out.Producers {
		r := &out.Producers[i]
		r.Producer, r.State, r.Reason = strings.Clone(r.Producer), ProducerState(strings.Clone(string(r.State))), strings.Clone(r.Reason)
	}
	for _, mask := range out.Files {
		if mask.FilePath == "" || !validCatalogValue(mask.Mode, fileOwnershipModes) {
			return ContractProjectionFootprint{}, ErrGenerationMaskInvalidValue
		}
	}
	for _, mask := range out.Identities {
		if mask.NodeID == "" || mask.Kind != NodeIdentityMaskLegacy && mask.Kind != NodeIdentityMaskReplace {
			return ContractProjectionFootprint{}, ErrGenerationMaskInvalidValue
		}
	}
	for _, mask := range out.EdgeSources {
		if mask.SourceID == "" || mask.Mode != OwnershipReplace {
			return ContractProjectionFootprint{}, ErrGenerationMaskInvalidValue
		}
	}
	for _, producer := range out.Producers {
		if producer.Producer == "" || !validCatalogValue(producer.State, producerStates) {
			return ContractProjectionFootprint{}, ErrGenerationMaskInvalidValue
		}
	}
	if err := tx.Commit(); err != nil {
		return ContractProjectionFootprint{}, err
	}
	after, err := s.CaptureContractProjectionClock(ctx)
	if err != nil {
		return ContractProjectionFootprint{}, err
	}
	if before != after {
		return ContractProjectionFootprint{}, graph.ErrContractProjectionStale
	}
	out.Cursor, out.RetainedBytes = after, charged
	return out, nil
}

func footprintAllocation(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	charge := int64(16)
	for charge < bytes {
		charge *= 2
	}
	return charge
}

func scanFootprintRows(ctx context.Context, tx *sql.Tx, generation int64, q contractFootprintQuery, scan func(int, *sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, "SELECT "+q.columns+" FROM "+q.table+" WHERE view_gen=? ORDER BY "+q.order, generation)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	at := 0
	for rows.Next() {
		if int64(at) >= q.count {
			return graph.ErrContractProjectionIncomplete
		}
		if err := scan(at, rows); err != nil {
			return err
		}
		at++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if int64(at) != q.count {
		return fmt.Errorf("%w: footprint row count moved", graph.ErrContractProjectionIncomplete)
	}
	return ctx.Err()
}
