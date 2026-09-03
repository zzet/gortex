package exporter

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/zzet/gortex/internal/graph"
)

// WriteGraphML emits the graph as GraphML XML, the de-facto interchange
// format for graph visualizers (yEd, Gephi, Cytoscape Desktop, networkx).
//
// All Gortex node properties are projected to GraphML <data> attributes.
// Free-form Meta is JSON-encoded into a single `meta_json` attribute so no
// information is lost — viewers that don't care about it ignore it.
func WriteGraphML(w io.Writer, g graph.Reader, opts Options) (Stats, error) {
	cw := &countingWriter{w: w}
	nodes, edges, _ := snapshot(g, opts)

	stats := Stats{}

	if _, err := io.WriteString(cw, xml.Header); err != nil {
		return stats, err
	}
	if _, err := io.WriteString(cw,
		`<graphml xmlns="http://graphml.graphdrawing.org/xmlns" `+
			`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" `+
			`xsi:schemaLocation="http://graphml.graphdrawing.org/xmlns `+
			`http://graphml.graphdrawing.org/xmlns/1.0/graphml.xsd">`+"\n",
	); err != nil {
		return stats, err
	}

	// <key> declarations. Any data element used inside <node> / <edge> needs
	// a key. We declare a fixed set covering the strongly-typed gortex fields
	// plus a `meta_json` catch-all per element type.
	keys := []graphMLKey{
		// Node keys.
		{ID: "name", For: "node", AttrName: "name", AttrType: "string"},
		{ID: "kind", For: "node", AttrName: "kind", AttrType: "string"},
		{ID: "qual_name", For: "node", AttrName: "qual_name", AttrType: "string"},
		{ID: "file_path", For: "node", AttrName: "file_path", AttrType: "string"},
		{ID: "start_line", For: "node", AttrName: "start_line", AttrType: "int"},
		{ID: "end_line", For: "node", AttrName: "end_line", AttrType: "int"},
		{ID: "language", For: "node", AttrName: "language", AttrType: "string"},
		{ID: "repo_prefix", For: "node", AttrName: "repo_prefix", AttrType: "string"},
		{ID: "workspace_id", For: "node", AttrName: "workspace_id", AttrType: "string"},
		{ID: "project_id", For: "node", AttrName: "project_id", AttrType: "string"},
		{ID: "node_meta", For: "node", AttrName: "meta_json", AttrType: "string"},

		// Edge keys.
		{ID: "edge_kind", For: "edge", AttrName: "kind", AttrType: "string"},
		{ID: "confidence", For: "edge", AttrName: "confidence", AttrType: "double"},
		{ID: "confidence_label", For: "edge", AttrName: "confidence_label", AttrType: "string"},
		{ID: "origin", For: "edge", AttrName: "origin", AttrType: "string"},
		{ID: "edge_file_path", For: "edge", AttrName: "file_path", AttrType: "string"},
		{ID: "edge_line", For: "edge", AttrName: "line", AttrType: "int"},
		{ID: "cross_repo", For: "edge", AttrName: "cross_repo", AttrType: "boolean"},
		{ID: "edge_meta", For: "edge", AttrName: "meta_json", AttrType: "string"},
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(cw,
			`  <key id=%q for=%q attr.name=%q attr.type=%q/>`+"\n",
			k.ID, k.For, k.AttrName, k.AttrType,
		); err != nil {
			return stats, err
		}
	}

	if _, err := io.WriteString(cw, `  <graph id="G" edgedefault="directed">`+"\n"); err != nil {
		return stats, err
	}

	for _, n := range nodes {
		if err := writeGraphMLNode(cw, n); err != nil {
			return stats, err
		}
		stats.NodesWritten++
	}

	for i, e := range edges {
		if err := writeGraphMLEdge(cw, e, i); err != nil {
			return stats, err
		}
		stats.EdgesWritten++
	}

	if _, err := io.WriteString(cw, "  </graph>\n</graphml>\n"); err != nil {
		return stats, err
	}

	stats.BytesWritten = cw.n
	return stats, nil
}

type graphMLKey struct {
	ID       string
	For      string
	AttrName string
	AttrType string
}

func writeGraphMLNode(w io.Writer, n *graph.Node) error {
	if _, err := fmt.Fprintf(w, `    <node id=%q>`+"\n", n.ID); err != nil {
		return err
	}
	writeData := func(key, val string) error {
		if val == "" {
			return nil
		}
		_, err := fmt.Fprintf(w, `      <data key=%q>%s</data>`+"\n", key, xmlEscape(val))
		return err
	}
	writeIntData := func(key string, val int) error {
		if val <= 0 {
			return nil
		}
		_, err := fmt.Fprintf(w, `      <data key=%q>%d</data>`+"\n", key, val)
		return err
	}

	if err := writeData("name", n.Name); err != nil {
		return err
	}
	if err := writeData("kind", string(n.Kind)); err != nil {
		return err
	}
	if err := writeData("qual_name", n.QualName); err != nil {
		return err
	}
	if err := writeData("file_path", n.FilePath); err != nil {
		return err
	}
	if err := writeIntData("start_line", n.StartLine); err != nil {
		return err
	}
	if err := writeIntData("end_line", n.EndLine); err != nil {
		return err
	}
	if err := writeData("language", n.Language); err != nil {
		return err
	}
	if err := writeData("repo_prefix", n.RepoPrefix); err != nil {
		return err
	}
	if err := writeData("workspace_id", n.WorkspaceID); err != nil {
		return err
	}
	if err := writeData("project_id", n.ProjectID); err != nil {
		return err
	}
	if metaJSON := metaToJSON(n.Meta); metaJSON != "" {
		if err := writeData("node_meta", metaJSON); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "    </node>\n")
	return err
}

func writeGraphMLEdge(w io.Writer, e *graph.Edge, idx int) error {
	if _, err := fmt.Fprintf(w,
		`    <edge id="e%d" source=%q target=%q>`+"\n",
		idx, e.From, e.To,
	); err != nil {
		return err
	}

	writeData := func(key, val string) error {
		if val == "" {
			return nil
		}
		_, err := fmt.Fprintf(w, `      <data key=%q>%s</data>`+"\n", key, xmlEscape(val))
		return err
	}

	if err := writeData("edge_kind", string(e.Kind)); err != nil {
		return err
	}
	if e.Confidence > 0 {
		if _, err := fmt.Fprintf(w, `      <data key="confidence">%g</data>`+"\n", e.Confidence); err != nil {
			return err
		}
	}
	if err := writeData("confidence_label", e.ConfidenceLabel); err != nil {
		return err
	}
	if err := writeData("origin", e.Origin); err != nil {
		return err
	}
	if err := writeData("edge_file_path", e.FilePath); err != nil {
		return err
	}
	if e.Line > 0 {
		if _, err := fmt.Fprintf(w, `      <data key="edge_line">%d</data>`+"\n", e.Line); err != nil {
			return err
		}
	}
	if e.CrossRepo {
		if _, err := io.WriteString(w, `      <data key="cross_repo">true</data>`+"\n"); err != nil {
			return err
		}
	}
	if metaJSON := metaToJSON(e.Meta); metaJSON != "" {
		if err := writeData("edge_meta", metaJSON); err != nil {
			return err
		}
	}

	_, err := io.WriteString(w, "    </edge>\n")
	return err
}

// xmlEscape escapes XML reserved characters for use inside <data> bodies.
// Uses encoding/xml's EscapeText behind a small wrapper so this stays
// faithful to the standard library's rules without re-implementing them.
func xmlEscape(s string) string {
	var buf escapeBuffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

type escapeBuffer struct {
	bytes []byte
}

func (e *escapeBuffer) Write(p []byte) (int, error) {
	e.bytes = append(e.bytes, p...)
	return len(p), nil
}
func (e *escapeBuffer) String() string { return string(e.bytes) }

// metaToJSON serializes Meta to a JSON object string. Returns empty when
// Meta is nil/empty so the caller can omit the data element entirely.
func metaToJSON(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	// Use the flattened, sanitized, sorted view so the output is stable.
	pairs := flattenMeta(meta)
	if len(pairs) == 0 {
		return ""
	}
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		out[p.Key] = p.Value
	}
	data, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(data)
}
