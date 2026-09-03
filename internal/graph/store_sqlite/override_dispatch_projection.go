package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const maxOverrideDispatchCallPageSize = 4096

var overrideDispatchSpeculativeKey = []byte(graph.MetaSpeculative)

// ScanOverrideDispatchCalls keyset-pages the compact unresolved Java/PHP call
// census beneath a frozen high-water mark. Each page cursor is closed before
// yield, so the resolver may exact-refetch identities without store re-entry.
func (s *Store) ScanOverrideDispatchCalls(
	repoPrefixes []string,
	pageSize int,
	yield func([]graph.OverrideDispatchCall) bool,
) {
	if yield == nil || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return
	}
	repoPrefixes = normalizeOverrideDispatchScope(repoPrefixes)
	if repoPrefixes != nil && len(repoPrefixes) == 0 {
		return
	}
	if pageSize <= 0 {
		pageSize = graph.DefaultOverrideDispatchCallPageSize
	} else if pageSize > maxOverrideDispatchCallPageSize {
		pageSize = maxOverrideDispatchCallPageSize
	}

	highWater, ok := s.edgeKindHighWater([]string{string(graph.EdgeCalls)})
	if !ok || highWater <= 0 {
		return
	}
	for after := int64(0); after < highWater; {
		page, next, ok := s.overrideDispatchCallPage(repoPrefixes, after, highWater, pageSize)
		if !ok || next <= after {
			return
		}
		after = next
		if len(page) > 0 && !yield(page) {
			return
		}
	}
}

func (s *Store) overrideDispatchCallPage(
	repoPrefixes []string,
	after, highWater int64,
	limit int,
) ([]graph.OverrideDispatchCall, int64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	query := `SELECT e.id, e.from_id, e.to_id, e.file_path, e.line, e.meta, caller.language
FROM edges AS e INDEXED BY edges_by_kind
JOIN nodes AS caller ON caller.id = e.from_id AND caller.view_gen = e.view_gen
WHERE e.kind = ? AND e.id > ? AND e.id <= ? AND e.view_gen = ?
  AND (substr(e.to_id, 1, 12) = 'unresolved::'
       OR instr(e.to_id, '::unresolved::') > 0)
  AND caller.language IN ('java', 'php')`
	args := make([]any, 0, len(repoPrefixes)+5)
	args = append(args, string(graph.EdgeCalls), after, highWater, s.viewGen)
	if repoPrefixes != nil {
		query += ` AND e.from_repo IN (` + inPlaceholders(len(repoPrefixes)) + `)`
		args = append(args, toAnyArgs(repoPrefixes)...)
	}
	query += ` ORDER BY e.id LIMIT ?`
	args = append(args, limit)

	rows, err := s.queryActiveWriteLocked(context.Background(), query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	defer rows.Close()

	page := make([]graph.OverrideDispatchCall, 0, limit)
	next := after
	for rows.Next() {
		var (
			rowID    int64
			identity graph.EdgeIdentity
			meta     sql.RawBytes
			language string
		)
		if err := rows.Scan(
			&rowID,
			&identity.From,
			&identity.To,
			&identity.FilePath,
			&identity.Line,
			&meta,
			&language,
		); err != nil {
			panicOnFatal(err)
			return nil, after, false
		}
		next = rowID
		speculative, err := decodeOverrideDispatchSpeculative(meta)
		if err != nil {
			panicOnFatal(err)
			return nil, after, false
		}
		if speculative {
			continue
		}
		identity.Kind = graph.EdgeCalls
		page = append(page, graph.OverrideDispatchCall{
			EdgeIdentity: identity, CallerLanguage: language,
		})
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	return page, next, true
}

func decodeOverrideDispatchSpeculative(blob []byte) (bool, error) {
	if len(blob) == 0 {
		return false, nil
	}
	if !isFlatMeta(blob) {
		meta, err := decodeMeta(blob)
		if err != nil {
			return false, err
		}
		speculative, _ := meta[graph.MetaSpeculative].(bool)
		return speculative, nil
	}

	d := &metaDecoder{buf: blob[2:]}
	count, err := d.readCount()
	if err != nil {
		return false, err
	}
	speculative := false
	for i := 0; i < count; i++ {
		key, err := d.readRawString()
		if err != nil {
			return false, err
		}
		if !bytes.Equal(key, overrideDispatchSpeculativeKey) {
			if err := d.skipValue(); err != nil {
				return false, err
			}
			continue
		}
		value, err := d.readValue()
		if err != nil {
			return false, err
		}
		speculative, _ = value.(bool)
	}
	return speculative, nil
}

func normalizeOverrideDispatchScope(repoPrefixes []string) []string {
	if repoPrefixes == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(repoPrefixes))
	out := make([]string, 0, len(repoPrefixes))
	for _, prefix := range repoPrefixes {
		if _, duplicate := seen[prefix]; duplicate {
			continue
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

var _ graph.OverrideDispatchCallBatchScanner = (*Store)(nil)
