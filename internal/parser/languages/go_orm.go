package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectGoORMModel inspects a Go struct's field list for ORM signals
// (gorm tags, embedded gorm.Model) and, when found, emits an
// EdgeModelsTable from the type to a synthetic KindTable node so
// `analyze kind=models` can answer "which class persists which table?"
// without re-deriving it from migrations.
//
// Table-name resolution at parse time uses the gorm-default convention:
// snake_case + plural (User → users, OrderLine → order_lines). A
// `TableName()` method override on the receiver type is detected in the
// indexer post-pass via inferGoORMTableNameOverrides — we can't see
// methods at struct-decl time because they're emitted by emitMethod
// independently.
//
// The KindTable target ID follows the existing convention from
// internal/parser/languages/go_sql.go: `db::generic::<schema>.<table>`
// without a schema prefix becomes `db::generic::<table>` when the
// dialect is unknown. We use "orm" as the dialect tag so analyzers can
// distinguish raw-SQL provenance ("go_sql") from ORM-derived nodes.
func detectGoORMModel(structNode *sitter.Node, src []byte, ownerID, ownerName, filePath string, result *parser.ExtractionResult) {
	if structNode == nil {
		return
	}
	fieldList := goStructFieldList(structNode)
	if fieldList == nil {
		return
	}

	hasGormTag := false
	embedsGormModel := false
	for i, _nc := 0, int(fieldList.NamedChildCount()); i < _nc; i++ {
		decl := fieldList.NamedChild(i)
		if decl == nil || decl.Type() != "field_declaration" {
			continue
		}
		if structFieldHasGormTag(decl, src) {
			hasGormTag = true
		}
		if structFieldEmbedsGormModel(decl, src) {
			embedsGormModel = true
		}
	}
	if !hasGormTag && !embedsGormModel {
		return
	}

	tableName := defaultGormTableName(ownerName)
	tableID := ormTableNodeID(tableName)

	if !ormTableNodeAlreadyEmitted(result, tableID) {
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:       tableID,
			Kind:     graph.KindTable,
			Name:     tableName,
			FilePath: filePath,
			Language: "go",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "go-orm",
			},
		})
	}

	startLine := int(structNode.StartPoint().Row) + 1
	binding := "gorm-tag"
	if !hasGormTag && embedsGormModel {
		binding = "gorm-embed"
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       tableID,
		Kind:     graph.EdgeModelsTable,
		FilePath: filePath,
		Line:     startLine,
		Origin:   graph.OriginASTResolved,
		Meta: map[string]any{
			"orm":        "gorm",
			"binding":    binding,
			"table_name": tableName,
			"derivation": "convention",
		},
	})
}

// goStructFieldList returns the field_declaration_list node from a
// struct_type, or nil when absent.
func goStructFieldList(structNode *sitter.Node) *sitter.Node {
	for i, _nc := 0, int(structNode.ChildCount()); i < _nc; i++ {
		c := structNode.Child(i)
		if c != nil && c.Type() == "field_declaration_list" {
			return c
		}
	}
	return nil
}

// structFieldHasGormTag reports whether a field_declaration carries a
// `gorm:"..."` struct tag.
func structFieldHasGormTag(decl *sitter.Node, src []byte) bool {
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "raw_string_literal" {
			continue
		}
		text := c.Content(src)
		if strings.Contains(text, "gorm:") {
			return true
		}
	}
	return false
}

// structFieldEmbedsGormModel reports whether a field_declaration is an
// embedded field of type `gorm.Model`. Catches both `gorm.Model` and
// `*gorm.Model` shapes.
func structFieldEmbedsGormModel(decl *sitter.Node, src []byte) bool {
	hasName := false
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "field_identifier" {
			hasName = true
			break
		}
	}
	if hasName {
		// An explicit field name means it isn't an embed.
		return false
	}
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "qualified_type":
			text := strings.TrimSpace(c.Content(src))
			if text == "gorm.Model" {
				return true
			}
		case "pointer_type":
			// `*gorm.Model` — the qualified_type lives inside.
			for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
				inner := c.NamedChild(j)
				if inner != nil && inner.Type() == "qualified_type" &&
					strings.TrimSpace(inner.Content(src)) == "gorm.Model" {
					return true
				}
			}
		}
	}
	return false
}

// defaultGormTableName returns gorm's default table name for a struct
// type — snake_case + plural. Mirrors gorm's NamingStrategy.TableName
// without depending on it: User → users, OrderLine → order_lines,
// HTTPHandler → http_handlers.
func defaultGormTableName(structName string) string {
	if structName == "" {
		return ""
	}
	snake := camelToSnake(structName)
	return pluralize(snake)
}

// camelToSnake converts CamelCase / PascalCase to snake_case. Handles
// acronyms by treating runs of uppercase letters followed by lowercase
// as one word boundary (HTTPHandler → http_handler, not h_t_t_p_handler).
func camelToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && isUpper(r) {
			prev := runes[i-1]
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			// Insert separator at:
			//  - lower→upper boundary (orderLine → order_line)
			//  - end of an acronym run (HTTPHandler → HTTP_Handler):
			//    upper followed by lower means we just left the run.
			if !isUpper(prev) || (next != 0 && !isUpper(next)) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(ormToLower(r))
	}
	return b.String()
}

// pluralize returns the gorm-default plural form. Handles the common
// English suffix rules gorm itself implements:
//   - ending in s, x, z, ch, sh: append "es"
//   - ending in consonant + y: change "y" to "ies"
//   - otherwise: append "s"
//
// Anything fancier (man → men, child → children) is intentionally not
// covered — gorm itself doesn't either, and over-correcting would
// disagree with the actual table names users created.
func pluralize(word string) string {
	if word == "" {
		return ""
	}
	if strings.HasSuffix(word, "s") || strings.HasSuffix(word, "x") ||
		strings.HasSuffix(word, "z") || strings.HasSuffix(word, "ch") ||
		strings.HasSuffix(word, "sh") {
		return word + "es"
	}
	if strings.HasSuffix(word, "y") && len(word) >= 2 {
		prev := word[len(word)-2]
		if !isVowelByte(prev) {
			return word[:len(word)-1] + "ies"
		}
	}
	return word + "s"
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func ormToLower(r rune) rune {
	if isUpper(r) {
		return r + ('a' - 'A')
	}
	return r
}
func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// ormTableNodeID returns the canonical KindTable node ID for an ORM-
// derived table. The "db::orm::" prefix matches the existing
// "db::<dialect>::<schema>.<table>" convention from go_sql.go and
// keeps ORM-derived nodes distinguishable from raw-SQL ones for
// analyzers that care about provenance.
func ormTableNodeID(tableName string) string {
	if tableName == "" {
		return ""
	}
	return "db::orm::" + tableName
}

// ormTableNodeAlreadyEmitted reports whether result.Nodes already
// contains a node with the given ID. ExtractionResult is per-file so
// the dedup window is small; the linear scan is cheaper than a map.
func ormTableNodeAlreadyEmitted(result *parser.ExtractionResult, id string) bool {
	if result == nil {
		return false
	}
	for _, n := range result.Nodes {
		if n != nil && n.ID == id {
			return true
		}
	}
	return false
}

// rewireORMTableNameOverrides walks the file's AST for
// `func (T) TableName() string { return "..." }` methods and rewires
// any convention-derived EdgeModelsTable for type T to the literal
// table name. Mutates result.Edges and result.Nodes in place.
//
// Why a post-pass: at struct-decl time the extractor only sees the
// struct body — it can't know whether a sibling method overrides the
// default name. Method emission runs in the same Extract pass but on a
// different match, so a single sweep at the end is the cheapest place
// to reconcile the two.
func rewireORMTableNameOverrides(root *sitter.Node, src []byte, result *parser.ExtractionResult) {
	if root == nil || result == nil {
		return
	}
	overrides := collectGoTableNameOverrides(root, src)
	if len(overrides) == 0 {
		return
	}
	// Build a quick `<owner-type-name> → existing edge` index over
	// EdgeModelsTable edges so we can patch them by receiver.
	type modelEdge struct {
		edge      *graph.Edge
		ownerName string
	}
	var modelEdges []modelEdge
	for _, e := range result.Edges {
		if e == nil || e.Kind != graph.EdgeModelsTable {
			continue
		}
		ownerName := goOwnerNameFromTypeID(e.From)
		modelEdges = append(modelEdges, modelEdge{edge: e, ownerName: ownerName})
	}
	if len(modelEdges) == 0 {
		return
	}
	for ownerName, tableName := range overrides {
		newID := ormTableNodeID(tableName)
		// Ensure the explicit table node exists; the convention-
		// derived KindTable node we previously emitted may have a
		// different name and stays in the graph (dropping it would
		// orphan any other edges that pointed at it). The
		// EdgeModelsTable rewire is the only relationship that
		// matters for the table-name override semantics.
		if !ormTableNodeAlreadyEmitted(result, newID) {
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       newID,
				Kind:     graph.KindTable,
				Name:     tableName,
				Language: "go",
				FilePath: firstFilePath(result),
				Meta: map[string]any{
					"dialect": "orm",
					"schema":  "",
					"source":  "go-orm",
				},
			})
		}
		for _, me := range modelEdges {
			if me.ownerName != ownerName {
				continue
			}
			me.edge.To = newID
			if me.edge.Meta == nil {
				me.edge.Meta = map[string]any{}
			}
			me.edge.Meta["table_name"] = tableName
			me.edge.Meta["derivation"] = "override"
		}
	}
}

// collectGoTableNameOverrides returns a map from receiver-type name to
// the literal string the type's TableName() method returns. Only
// matches the gorm-shaped signature: a method named TableName with no
// parameters and a single string-literal return.
func collectGoTableNameOverrides(root *sitter.Node, src []byte) map[string]string {
	out := make(map[string]string)
	walkAST(root, func(n *sitter.Node) bool {
		if n == nil || n.Type() != "method_declaration" {
			return true
		}
		nameNode := n.ChildByFieldName("name")
		if nameNode == nil || nameNode.Content(src) != "TableName" {
			return true
		}
		recv := receiverTypeFromMethodNode(n, src)
		if recv == "" {
			return true
		}
		body := n.ChildByFieldName("body")
		if body == nil {
			return true
		}
		literal := firstStringReturnLiteral(body, src)
		if literal == "" {
			return true
		}
		out[recv] = literal
		return false // stop descending into the method body
	})
	return out
}

// walkAST is a small DFS helper. visit returns false to skip n's
// subtree, true to continue.
func walkAST(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkAST(n.NamedChild(i), visit)
	}
}

// receiverTypeFromMethodNode returns the bare receiver type name from
// a method_declaration. Strips pointer wrappers — a method on `*User`
// and a method on `User` both attach to the User type for the
// EdgeModelsTable lookup.
func receiverTypeFromMethodNode(n *sitter.Node, src []byte) string {
	recv := n.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	for i, _nc := 0, int(recv.NamedChildCount()); i < _nc; i++ {
		decl := recv.NamedChild(i)
		if decl == nil || decl.Type() != "parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		text := strings.TrimSpace(typeNode.Content(src))
		text = strings.TrimPrefix(text, "*")
		// Strip generic parameter list, e.g. `User[T]` → `User`.
		if i := strings.Index(text, "["); i > 0 {
			text = text[:i]
		}
		return text
	}
	return ""
}

// firstStringReturnLiteral returns the literal text of the first
// `return "..."` statement reachable from body, with quotes stripped.
// Empty when the return value isn't a single string literal.
func firstStringReturnLiteral(body *sitter.Node, src []byte) string {
	var found string
	walkAST(body, func(n *sitter.Node) bool {
		if found != "" {
			return false
		}
		if n == nil || n.Type() != "return_statement" {
			return true
		}
		// return_statement's first named child is the expression list.
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			if c.Type() == "expression_list" && c.NamedChildCount() == 1 {
				inner := c.NamedChild(0)
				if inner != nil && inner.Type() == "interpreted_string_literal" {
					found = stripGoStringQuotes(inner.Content(src))
					return false
				}
				if inner != nil && inner.Type() == "raw_string_literal" {
					found = strings.Trim(inner.Content(src), "`")
					return false
				}
			}
			if c.Type() == "interpreted_string_literal" {
				found = stripGoStringQuotes(c.Content(src))
				return false
			}
			if c.Type() == "raw_string_literal" {
				found = strings.Trim(c.Content(src), "`")
				return false
			}
		}
		return false
	})
	return found
}

// stripGoStringQuotes removes the surrounding double quotes from a Go
// interpreted_string_literal. Doesn't decode escapes — the table name
// is almost always a simple ASCII identifier; agents can ask the
// runtime if they care about exotic escapes.
func stripGoStringQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// goOwnerNameFromTypeID extracts the bare type name from a node ID of
// shape `<file>::<TypeName>`. Returns "" when the ID doesn't follow
// the convention.
func goOwnerNameFromTypeID(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[i+2:]
	}
	return ""
}

// firstFilePath returns the FilePath of the first node in result, or
// empty when the result is empty. Used to anchor synthetic table
// nodes — without a file path the per-file dedup logic mis-buckets
// them.
func firstFilePath(result *parser.ExtractionResult) string {
	for _, n := range result.Nodes {
		if n != nil && n.FilePath != "" {
			return n.FilePath
		}
	}
	return ""
}
