package exporter

import (
	"fmt"
	"io"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// WriteCypher emits the graph as a Cypher script suitable for `cypher-shell`,
// Memgraph Lab, or Neo4j Browser. Every node carries the secondary label
// `GortexNode` so a client can `MATCH (n:GortexNode) DETACH DELETE n` to
// reset before re-loading.
//
// The output is intentionally minimal — pure statements, one per line, no
// comments and no DDL. Comments and `CREATE INDEX … IF NOT EXISTS` syntax
// are not portable between Neo4j 5.x and Memgraph's `.cypherl` loader, so
// we keep the file lowest-common-denominator. Run-instructions and a hint
// for adding the recommended index are emitted on stderr by the CLI, not
// baked into the file.
//
// The MATCH+CREATE per edge is deliberately simple. For graphs > ~50K edges
// loading is slow without an index; create one before loading the edges:
//
//	CREATE INDEX ON :GortexNode(id);   // Memgraph
//	CREATE INDEX FOR (n:GortexNode) ON (n.id);   // Neo4j 5.x
func WriteCypher(w io.Writer, g graph.Reader, opts Options) (Stats, error) {
	cw := &countingWriter{w: w}
	nodes, edges, _ := snapshot(g, opts)

	stats := Stats{}

	for _, n := range nodes {
		if err := writeCypherNode(cw, n); err != nil {
			return stats, err
		}
		stats.NodesWritten++
	}
	for _, e := range edges {
		if err := writeCypherEdge(cw, e); err != nil {
			return stats, err
		}
		stats.EdgesWritten++
	}

	stats.BytesWritten = cw.n
	return stats, nil
}

// writeCypherNode emits one CREATE statement per node:
//
//	CREATE (:Function:GortexNode {id: "...", name: "...", ...});
func writeCypherNode(w io.Writer, n *graph.Node) error {
	label := nodeLabel(n.Kind)

	props := []propPair{
		{"id", n.ID},
		{"name", n.Name},
	}
	if n.QualName != "" {
		props = append(props, propPair{"qual_name", n.QualName})
	}
	if n.FilePath != "" {
		props = append(props, propPair{"file_path", n.FilePath})
	}
	if n.StartLine > 0 {
		props = append(props, propPair{"start_line", int64(n.StartLine)})
	}
	if n.EndLine > 0 {
		props = append(props, propPair{"end_line", int64(n.EndLine)})
	}
	if n.Language != "" {
		props = append(props, propPair{"language", n.Language})
	}
	if n.RepoPrefix != "" {
		props = append(props, propPair{"repo_prefix", n.RepoPrefix})
	}
	if n.WorkspaceID != "" {
		props = append(props, propPair{"workspace_id", n.WorkspaceID})
	}
	if n.ProjectID != "" {
		props = append(props, propPair{"project_id", n.ProjectID})
	}
	for _, e := range flattenMeta(n.Meta) {
		// Don't shadow our top-level keys.
		if isReservedNodeKey(e.Key) {
			continue
		}
		props = append(props, propPair(e))
	}

	_, err := fmt.Fprintf(w, "CREATE (:%s:GortexNode %s);\n", label, cypherProps(props))
	return err
}

// writeCypherEdge emits one MATCH + CREATE per edge.
func writeCypherEdge(w io.Writer, e *graph.Edge) error {
	relType := edgeRelType(e.Kind)

	props := []propPair{}
	if e.Confidence > 0 {
		props = append(props, propPair{"confidence", e.Confidence})
	}
	if e.ConfidenceLabel != "" {
		props = append(props, propPair{"confidence_label", e.ConfidenceLabel})
	}
	if e.Origin != "" {
		props = append(props, propPair{"origin", e.Origin})
	}
	if e.FilePath != "" {
		props = append(props, propPair{"file_path", e.FilePath})
	}
	if e.Line > 0 {
		props = append(props, propPair{"line", int64(e.Line)})
	}
	if e.CrossRepo {
		props = append(props, propPair{"cross_repo", true})
	}
	for _, m := range flattenMeta(e.Meta) {
		if isReservedEdgeKey(m.Key) {
			continue
		}
		props = append(props, propPair(m))
	}

	_, err := fmt.Fprintf(w,
		"MATCH (a:GortexNode {id: %s}), (b:GortexNode {id: %s}) CREATE (a)-[:%s %s]->(b);\n",
		cypherString(e.From), cypherString(e.To), relType, cypherProps(props),
	)
	return err
}

// propPair is one (key, value) row inside a Cypher property map.
type propPair struct {
	Key   string
	Value any
}

// cypherProps formats a slice of pairs as a Cypher map literal: `{k: v, k: v}`.
// Empty slice → `{}` (always emit braces; Cypher's grammar accepts an empty map).
func cypherProps(pairs []propPair) string {
	if len(pairs) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Key)
		b.WriteString(": ")
		b.WriteString(cypherValue(p.Value))
	}
	b.WriteByte('}')
	return b.String()
}

// cypherValue formats a single value as a Cypher literal.
func cypherValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return cypherString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int32:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case uint:
		return fmt.Sprintf("%d", x)
	case uint32:
		return fmt.Sprintf("%d", x)
	case uint64:
		return fmt.Sprintf("%d", x)
	case float32:
		return fmt.Sprintf("%g", x)
	case float64:
		return fmt.Sprintf("%g", x)
	default:
		return cypherString(fmt.Sprintf("%v", x))
	}
}

// cypherString returns a single-quoted Cypher string with the standard escape
// set: backslash, single quote, newline, carriage return, tab.
func cypherString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func isReservedNodeKey(k string) bool {
	switch k {
	case "id", "name", "qual_name", "file_path", "start_line", "end_line",
		"language", "repo_prefix", "workspace_id", "project_id":
		return true
	}
	return false
}

func isReservedEdgeKey(k string) bool {
	switch k {
	case "confidence", "confidence_label", "origin", "file_path", "line", "cross_repo":
		return true
	}
	return false
}
