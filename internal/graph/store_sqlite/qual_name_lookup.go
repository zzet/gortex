package store_sqlite

import "encoding/json"

// nodes_by_qual is the partial qualified-name lookup index. The explicit
// predicate lets SQLite prove the index is applicable, while one JSON value
// keeps even very large resolver pages to a single bind and indexed query.
// Qualified names may repeat, so id supplies a deterministic secondary order
// within each candidate slice returned by the Store API.
const nodesByQualNameLookupSQL = `SELECT ` + lookupNodeCols + `
FROM nodes INDEXED BY nodes_by_qual
WHERE qual_name <> ''
  AND qual_name IN (
    SELECT CAST(value AS TEXT)
    FROM json_each(?)
    WHERE CAST(value AS TEXT) <> ''
  )
  AND view_gen = ?
ORDER BY qual_name, id`

func qualNameLookupPayload(qualNames []string) string {
	payload, err := json.Marshal(qualNames)
	if err != nil {
		// Encoding a []string cannot fail. Keep the lookup fail-closed if the
		// standard library ever changes that contract.
		return "[]"
	}
	return string(payload)
}
