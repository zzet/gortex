package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/sql"
)

// SQLExtractor extracts SQL source files.
type SQLExtractor struct {
	lang *sitter.Language
}

func NewSQLExtractor() *SQLExtractor {
	return &SQLExtractor{lang: sql.GetLanguage()}
}

func (e *SQLExtractor) Language() string     { return "sql" }
func (e *SQLExtractor) Extensions() []string { return []string{".sql"} }

func (e *SQLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	// Parse with dollar-quoted bodies blanked so a PL/pgSQL body the
	// grammar cannot read never takes its CREATE FUNCTION header down
	// with it. The mask keeps byte offsets and lines, so the tree is
	// read against the original src below.
	tree, err := parser.ParseFile(maskSQLDollarQuotedBodies(src), e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "sql",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// Databricks source-format notebooks are plain `.sql` files with
	// `-- COMMAND ----------` separators; emit cell nodes alongside
	// any DDL-shaped nodes the regular walk produces. No-op for
	// non-Databricks SQL files.
	MaybeEnrichDatabricks(filePath, fileNode.ID, src, result)

	// Specialised SQL dispatch: dbt and SQLMesh models are `.sql`
	// files but carry their own model/column/lineage graph shape, so
	// they short-circuit the generic DDL walk below.
	switch classifySQLFile(filePath, src) {
	case "dbt":
		extractDbtSQLModel(filePath, fileNode.ID, src, result)
		return result, nil
	case "sqlmesh":
		extractSQLMeshSQLModel(filePath, fileNode.ID, src, result)
		return result, nil
	}

	seen := make(map[string]bool)
	e.walkDDL(root, src, filePath, fileNode.ID, seen, result, 0)

	return result, nil
}

// maxDDLWalkDepth bounds the recursive descent. Real DDL nests a handful
// of levels; a deeper tree is either pathological or an error cascade,
// and neither is worth unbounded stack.
const maxDDLWalkDepth = 64

// walkDDL descends the whole tree looking for CREATE statements rather
// than only visiting `statement` children of the root.
//
// The recursion is load-bearing, not defensive. tree-sitter-sql's error
// recovery can re-parent well-formed statements under an ERROR node that
// swallows the rest of the file — historically a `CREATE FUNCTION … $$
// BEGIN … END $$` body it could not read, routine in Postgres
// migrations (Extract now blanks those bodies before parsing, see
// maskSQLDollarQuotedBodies). Under a top-level-only walk every table
// declared after the first such error silently disappeared. Descending
// through ERROR recovers those statements: the sub-nodes are still
// parsed correctly, they are just re-parented under the error.
func (e *SQLExtractor) walkDDL(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult, depth int) {
	if depth > maxDDLWalkDepth {
		return
	}
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "create_table":
			e.extractCreateTable(child, src, filePath, fileID, seen, result)
		case "create_view":
			e.extractCreateView(child, src, filePath, fileID, seen, result)
		case "create_function", "create_procedure":
			e.extractCreateFunction(child, src, filePath, fileID, seen, result)
		case "create_index":
			e.extractCreateIndex(child, src, filePath, fileID, seen, result)
		case "create_trigger":
			e.extractCreateTrigger(child, src, filePath, fileID, seen, result)
		default:
			e.walkDDL(child, src, filePath, fileID, seen, result, depth+1)
		}
	}
}

func (e *SQLExtractor) extractCreateTable(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	obj := findObject(node, src)
	if obj.Name == "" || seen[obj.key("table")] {
		return
	}
	seen[obj.key("table")] = true
	name := obj.Name
	id := filePath + "::" + obj.qualified()
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: obj.meta(map[string]any{"sql_type": "table", "type_flavor": "table"}),
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})

	// Extract column names as variables with EdgeMemberOf.
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == "column_definitions" {
			for k, _nc := 0, int(child.NamedChildCount()); k < _nc; k++ {
				col := child.NamedChild(k)
				if col.Type() == "column_definition" {
					colName := stripSQLQuoting(firstNamedChildOfType(col, "identifier", src))
					if colName != "" {
						colID := id + "." + colName
						if !seen[colID] {
							seen[colID] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: colID, Kind: graph.KindVariable, Name: colName,
								FilePath: filePath, StartLine: int(col.StartPoint().Row) + 1, EndLine: int(col.EndPoint().Row) + 1,
								Language: "sql",
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: colID, To: id, Kind: graph.EdgeMemberOf,
								FilePath: filePath, Line: int(col.StartPoint().Row) + 1,
							})
						}
					}
				}
			}
		}
	}
}

func (e *SQLExtractor) extractCreateView(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	obj := findObject(node, src)
	if obj.Name == "" || seen[obj.key("view")] {
		return
	}
	seen[obj.key("view")] = true
	id := filePath + "::" + obj.qualified()
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: obj.Name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: obj.meta(map[string]any{"sql_type": "view", "type_flavor": "view"}),
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *SQLExtractor) extractCreateFunction(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	obj := findObject(node, src)
	if obj.Name == "" || seen[obj.key("function")] {
		return
	}
	seen[obj.key("function")] = true
	// Postgres keeps functions and procedures in one namespace, so a
	// procedure shares the function dedupe key and node kind; the
	// sql_type marker tells the two apart.
	var base map[string]any
	if node.Type() == "create_procedure" {
		base = map[string]any{"sql_type": "procedure"}
	}
	id := filePath + "::" + obj.qualified()
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: obj.Name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: obj.meta(base),
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

// extractCreateIndex names the index after its own identifier, not the
// table it indexes. `CREATE INDEX idx ON schema.tbl (col)` puts the index
// name in a bare `identifier` child and the ON-target in the statement's
// first `object_reference`; the shared findObject path would therefore
// name every index after the table it covers.
func (e *SQLExtractor) extractCreateIndex(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	obj := findDeclaredIdentifier(node, src)
	if obj.Name == "" || seen[obj.key("index")] {
		return
	}
	seen[obj.key("index")] = true
	meta := obj.meta(map[string]any{"sql_type": "index"})
	if target := findObject(node, src); target.Name != "" {
		meta["indexes"] = target.qualified()
	}
	id := filePath + "::" + obj.qualified()
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: obj.Name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *SQLExtractor) extractCreateTrigger(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	// A trigger's own name is the statement's first `object_reference`
	// (`CREATE TRIGGER trg BEFORE UPDATE ON tbl …`), so the shared path
	// is correct here — unlike CREATE INDEX.
	obj := findObject(node, src)
	if obj.Name == "" || seen[obj.key("trigger")] {
		return
	}
	seen[obj.key("trigger")] = true
	id := filePath + "::" + obj.qualified()
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: obj.Name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: obj.meta(map[string]any{"sql_type": "trigger"}),
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

// sqlObject is the identifier a CREATE statement declares, split into
// the object's own name and the schema it was qualified with.
type sqlObject struct {
	Name   string // bare object name, quoting stripped
	Schema string // schema qualifier; "" when the name was unqualified
}

// qualified renders the name the graph keys the object under. Staying
// bare for unqualified names keeps node IDs stable for the single-schema
// files that already worked.
func (o sqlObject) qualified() string {
	if o.Schema == "" {
		return o.Name
	}
	return o.Schema + "." + o.Name
}

// key is the per-file dedupe key. It is qualified and kind-scoped so
// `identity.accounts` and `billing.accounts` in one migration are two
// tables rather than one, and a trigger may share a table's name.
func (o sqlObject) key(kind string) string { return kind + "::" + o.qualified() }

// meta merges the schema qualifier into a node's metadata, leaving base
// untouched (and nil-preserving) when the name was unqualified.
func (o sqlObject) meta(base map[string]any) map[string]any {
	if o.Schema == "" {
		return base
	}
	if base == nil {
		base = map[string]any{}
	}
	base["schema"] = o.Schema
	base["qualified_name"] = o.qualified()
	return base
}

// findObject resolves the object a CREATE statement declares from its
// first `object_reference`.
//
// A schema-qualified reference (`identity.kyc_submissions`) exposes one
// `identifier` child per dotted segment, so the object's own name is the
// LAST segment. Reading the first segment named every table after its
// schema instead — which then collapsed under the per-file dedupe, so a
// migration declaring `identity.accounts` and `identity.kyc_submissions`
// produced a single bogus symbol called `identity` and lost the rest.
func findObject(node *sitter.Node, src []byte) sqlObject {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == "object_reference" {
			return objectFromReference(child, src)
		}
	}
	// Fallback: a bare identifier child, no object_reference wrapper.
	return sqlObject{Name: stripSQLQuoting(firstNamedChildOfType(node, "identifier", src))}
}

// objectFromReference splits an `object_reference` into schema + name.
// Segments carry their own quoting (`"billing"."balance_transactions"`,
// MySQL backticks), which is stripped per segment so a quoted name
// containing a dot stays intact — something splitting the raw text on
// the last dot would get wrong.
func objectFromReference(ref *sitter.Node, src []byte) sqlObject {
	var segments []string
	for i, _nc := 0, int(ref.NamedChildCount()); i < _nc; i++ {
		child := ref.NamedChild(i)
		if child.Type() == "identifier" {
			segments = append(segments, stripSQLQuoting(child.Content(src)))
		}
	}
	switch len(segments) {
	case 0:
		return sqlObject{}
	case 1:
		return sqlObject{Name: segments[0]}
	default:
		// `db.schema.table` keeps only the immediate parent as schema,
		// matching internal/sql's splitSchemaTable.
		return sqlObject{Name: segments[len(segments)-1], Schema: segments[len(segments)-2]}
	}
}

// findDeclaredIdentifier resolves the name of a statement that declares
// its object as a bare identifier ahead of an ON-target — CREATE INDEX.
func findDeclaredIdentifier(node *sitter.Node, src []byte) sqlObject {
	if name := stripSQLQuoting(firstNamedChildOfType(node, "identifier", src)); name != "" {
		return sqlObject{Name: name}
	}
	return findObject(node, src)
}

// stripSQLQuoting removes the three identifier-quoting styles: double
// quotes (ANSI), backticks (MySQL), brackets (T-SQL).
func stripSQLQuoting(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= 2 {
		first, last := name[0], name[len(name)-1]
		switch {
		case first == '"' && last == '"',
			first == '`' && last == '`',
			first == '[' && last == ']':
			return name[1 : len(name)-1]
		}
	}
	return name
}

func firstNamedChildOfType(node *sitter.Node, nodeType string, src []byte) string {
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		if child.Type() == nodeType {
			return child.Content(src)
		}
	}
	return ""
}

var _ parser.Extractor = (*SQLExtractor)(nil)
