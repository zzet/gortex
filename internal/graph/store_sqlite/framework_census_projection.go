package store_sqlite

import (
	"bytes"
	"database/sql"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

type frameworkCensusMetaField uint8

const (
	frameworkCensusMetaUnknown frameworkCensusMetaField = iota
	frameworkCensusMetaVia
	frameworkCensusMetaRegistryValue
	frameworkCensusMetaGRPCRegisterService
	frameworkCensusMetaGRPCService
	frameworkCensusMetaGRPCMethod
	frameworkCensusMetaExpressHandlerRef
	frameworkCensusMetaRecvConst
	frameworkCensusMetaReceiverExpr
	frameworkCensusMetaRustPath
	frameworkCensusMetaRustRecv
	frameworkCensusMetaReceiverType
	frameworkCensusMetaRustRecvExpr
	frameworkCensusMetaSynthesizedBy
)

var frameworkCensusMetaKeys = [...]struct {
	name  string
	raw   []byte
	field frameworkCensusMetaField
}{
	{name: "via", raw: []byte("via"), field: frameworkCensusMetaVia},
	{name: "registry_value", raw: []byte("registry_value"), field: frameworkCensusMetaRegistryValue},
	{name: "grpc_register_service", raw: []byte("grpc_register_service"), field: frameworkCensusMetaGRPCRegisterService},
	{name: "grpc_service", raw: []byte("grpc_service"), field: frameworkCensusMetaGRPCService},
	{name: "grpc_method", raw: []byte("grpc_method"), field: frameworkCensusMetaGRPCMethod},
	{name: "express_handler_ref", raw: []byte("express_handler_ref"), field: frameworkCensusMetaExpressHandlerRef},
	{name: "recv_const", raw: []byte("recv_const"), field: frameworkCensusMetaRecvConst},
	{name: "receiver_expr", raw: []byte("receiver_expr"), field: frameworkCensusMetaReceiverExpr},
	{name: "rust_path", raw: []byte("rust_path"), field: frameworkCensusMetaRustPath},
	{name: "rust_recv", raw: []byte("rust_recv"), field: frameworkCensusMetaRustRecv},
	{name: "receiver_type", raw: []byte("receiver_type"), field: frameworkCensusMetaReceiverType},
	{name: "rust_recv_expr", raw: []byte("rust_recv_expr"), field: frameworkCensusMetaRustRecvExpr},
	{name: "synthesized_by", raw: []byte("synthesized_by"), field: frameworkCensusMetaSynthesizedBy},
}

// FrameworkCensusEdgesSeq keeps the full metadata blob cursor-local and emits
// only the identity and scalar keys consumed by framework admission. No store
// re-entry occurs while the read cursor is open; framework passes exact-refetch
// their selected identities only after this sequence has closed.
func (s *Store) FrameworkCensusEdgesSeq(kinds ...graph.EdgeKind) iter.Seq[graph.FrameworkCensusEdge] {
	uniq, _ := aggDedupeEdgeKinds(kinds)
	return func(yield func(graph.FrameworkCensusEdge) bool) {
		for _, kind := range uniq {
			if !s.scanFrameworkCensusKind(kind, yield) {
				return
			}
		}
	}
}

func (s *Store) scanFrameworkCensusKind(
	kind graph.EdgeKind,
	yield func(graph.FrameworkCensusEdge) bool,
) bool {
	withMeta := kind != graph.EdgeAnnotated
	query := `SELECT from_id, to_id, file_path, line`
	if withMeta {
		query += `, meta`
	}
	query += `
FROM edges INDEXED BY edges_by_kind
WHERE kind = ? AND view_gen = ?
ORDER BY id`
	rows, err := s.db.Query(query, string(kind), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return false
	}
	defer rows.Close()
	for rows.Next() {
		row := graph.FrameworkCensusEdge{EdgeIdentity: graph.EdgeIdentity{Kind: kind}}
		var meta sql.RawBytes
		if withMeta {
			err = rows.Scan(&row.From, &row.To, &row.FilePath, &row.Line, &meta)
		} else {
			err = rows.Scan(&row.From, &row.To, &row.FilePath, &row.Line)
		}
		if err != nil {
			panicOnFatal(err)
			return false
		}
		if withMeta {
			if err := decodeFrameworkCensusMeta(kind, meta, &row); err != nil {
				panicOnFatal(err)
				return false
			}
		}
		if !yield(row) {
			return false
		}
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return false
	}
	return true
}

func decodeFrameworkCensusMeta(
	kind graph.EdgeKind,
	blob []byte,
	row *graph.FrameworkCensusEdge,
) error {
	if len(blob) == 0 || row == nil {
		return nil
	}
	if !isFlatMeta(blob) {
		meta, err := decodeMeta(blob)
		if err != nil {
			return err
		}
		for _, key := range frameworkCensusMetaKeys {
			if !frameworkCensusFieldAllowed(kind, key.field) {
				continue
			}
			value, present := meta[key.name]
			if !present {
				continue
			}
			if key.field == frameworkCensusMetaExpressHandlerRef {
				row.HasExpressHandlerRef = true
				continue
			}
			assignFrameworkCensusMeta(row, key.field, value)
		}
		return nil
	}

	d := &metaDecoder{buf: blob[2:]}
	count, err := d.readCount()
	if err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		key, err := d.readRawString()
		if err != nil {
			return err
		}
		field := frameworkCensusField(kind, key)
		if field == frameworkCensusMetaUnknown {
			if err := d.skipValue(); err != nil {
				return err
			}
			continue
		}
		if field == frameworkCensusMetaExpressHandlerRef {
			if err := d.skipValue(); err != nil {
				return err
			}
			row.HasExpressHandlerRef = true
			continue
		}
		value, err := d.readValue()
		if err != nil {
			return err
		}
		assignFrameworkCensusMeta(row, field, value)
	}
	return nil
}

func frameworkCensusField(kind graph.EdgeKind, key []byte) frameworkCensusMetaField {
	for _, candidate := range frameworkCensusMetaKeys {
		if bytes.Equal(key, candidate.raw) && frameworkCensusFieldAllowed(kind, candidate.field) {
			return candidate.field
		}
	}
	return frameworkCensusMetaUnknown
}

func frameworkCensusFieldAllowed(kind graph.EdgeKind, field frameworkCensusMetaField) bool {
	switch kind {
	case graph.EdgeCalls:
		return field != frameworkCensusMetaSynthesizedBy
	case graph.EdgeReferences:
		return field == frameworkCensusMetaVia ||
			field == frameworkCensusMetaReceiverExpr ||
			field == frameworkCensusMetaSynthesizedBy
	case graph.EdgeImports:
		return field == frameworkCensusMetaSynthesizedBy
	default:
		return false
	}
}

func assignFrameworkCensusMeta(
	row *graph.FrameworkCensusEdge,
	field frameworkCensusMetaField,
	value any,
) {
	text, _ := value.(string)
	switch field {
	case frameworkCensusMetaVia:
		row.Via = text
	case frameworkCensusMetaRegistryValue:
		row.RegistryValue = text
	case frameworkCensusMetaGRPCRegisterService:
		row.GRPCRegisterService = text
	case frameworkCensusMetaGRPCService:
		row.GRPCService = text
	case frameworkCensusMetaGRPCMethod:
		row.GRPCMethod = text
	case frameworkCensusMetaRecvConst:
		row.RecvConst = text
	case frameworkCensusMetaReceiverExpr:
		row.ReceiverExpr = text
	case frameworkCensusMetaRustPath:
		row.RustPath = text
	case frameworkCensusMetaRustRecv:
		row.RustRecv = text
	case frameworkCensusMetaReceiverType:
		row.ReceiverType = text
	case frameworkCensusMetaRustRecvExpr:
		row.RustRecvExpr = text
	case frameworkCensusMetaSynthesizedBy:
		row.SynthesizedBy = text
	}
}

var _ graph.FrameworkCensusEdgeSequencer = (*Store)(nil)
