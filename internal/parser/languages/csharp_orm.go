package languages

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectCSharpORMModel inspects a C# class/record for a [Table]
// attribute (System.ComponentModel.DataAnnotations.Schema — the EF
// Core mapping attribute, shared by Dapper.Contrib) and emits an
// EdgeModelsTable to a synthetic KindTable node when one is found.
//
// Only the attribute form is decided at extraction time: EF's other
// two table-name sources — DbSet<T> property-name convention and
// fluent ToTable(...) configuration — live in files other than the
// entity's, so they are joined by a resolver pass over stamped facts,
// not here. A class with no [Table] attribute emits nothing.
func csharpEFAttributeArguments(list *sitter.Node, src []byte) ([]csharpEFArgument, bool) {
	if list == nil || list.Type() != "attribute_argument_list" {
		return nil, false
	}
	args := make([]csharpEFArgument, 0, list.NamedChildCount())
	for i, count := 0, int(list.NamedChildCount()); i < count; i++ {
		argument := list.NamedChild(i)
		if argument == nil || argument.Type() != "attribute_argument" || argument.NamedChildCount() == 0 {
			return nil, false
		}
		name := ""
		if nameNode := argument.ChildByFieldName("name"); nameNode != nil {
			if argument.NamedChildCount() < 2 {
				return nil, false
			}
			name = strings.TrimPrefix(strings.TrimSpace(nameNode.Content(src)), "@")
			if name == "" {
				return nil, false
			}
		}
		args = append(args, csharpEFArgument{
			name:  name,
			value: argument.NamedChild(int(argument.NamedChildCount()) - 1),
		})
	}
	return args, len(args) != 0
}

func csharpEFTableAttribute(decl *sitter.Node, src []byte) (table, schema string, ok bool) {
	if decl == nil {
		return "", "", false
	}
	for i, count := 0, int(decl.NamedChildCount()); i < count; i++ {
		list := decl.NamedChild(i)
		if list == nil || list.Type() != "attribute_list" {
			continue
		}
		for j, attrCount := 0, int(list.NamedChildCount()); j < attrCount; j++ {
			attribute := list.NamedChild(j)
			if attribute == nil || attribute.Type() != "attribute" {
				continue
			}
			nameNode := attribute.ChildByFieldName("name")
			if nameNode == nil || !csharpIsTableAttr(nameNode.Content(src)) {
				continue
			}
			arguments, valid := csharpEFAttributeArguments(
				csharpDirectChildOfType(attribute, "attribute_argument_list"),
				src,
			)
			if !valid {
				continue
			}
			tableValue := ""
			schemaValue := ""
			tableSet := false
			schemaSet := false
			for index, argument := range arguments {
				key := argument.name
				if key == "" && index == 0 {
					key = "name"
				}
				value, literal := csharpDecodeStringLiteral(argument.value.Content(src))
				switch key {
				case "name":
					if tableSet || !literal || value == "" {
						valid = false
					} else {
						tableValue, tableSet = value, true
					}
				case "Schema":
					if schemaSet || !literal {
						valid = false
					} else {
						schemaValue, schemaSet = value, true
					}
				default:
					valid = false
				}
				if !valid {
					break
				}
			}
			if valid && tableSet {
				return tableValue, schemaValue, true
			}
		}
	}
	return "", "", false
}

func detectCSharpORMModel(classNode *sitter.Node, src []byte, classID, filePath string) []*graph.Edge {
	tableName, schema, ok := csharpEFTableAttribute(classNode, src)
	if !ok {
		return nil
	}
	qualified := tableName
	if schema != "" {
		qualified = schema + "." + tableName
	}
	tableID := ormTableNodeID(qualified)
	meta := map[string]any{
		"orm":        "efcore",
		"binding":    "attribute",
		"table_name": tableName,
		"derivation": "override",
	}
	if schema != "" {
		meta["schema"] = schema
	}
	return []*graph.Edge{
		{
			From:     classID,
			To:       tableID,
			Kind:     graph.EdgeModelsTable,
			FilePath: filePath,
			Line:     int(classNode.StartPoint().Row) + 1,
			Origin:   graph.OriginASTResolved,
			Meta:     meta,
		},
	}
}

func stampCSharpEFAttribute(decl *sitter.Node, src []byte, meta map[string]any) {
	table, schema, ok := csharpEFTableAttribute(decl, src)
	if !ok {
		return
	}
	meta["ef_attribute_table"] = table
	if schema != "" {
		meta["ef_attribute_schema"] = schema
	}
}

// csharpIsTableAttr reports whether an attribute name denotes [Table].
// C# lets the attribute appear qualified (Schema.Table, or the full
// namespace path) and with the explicit Attribute suffix, so compare
// the final dotted segment against both spellings.
func csharpIsTableAttr(name string) bool {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name == "Table" || name == "TableAttribute"
}

// csharpDecodeStringLiteral decodes one complete C# regular or verbatim
// string literal. It intentionally rejects newer raw/interpolated/UTF-8
// spellings: accepting a prefix of one of those would manufacture a table
// name that the source does not state as a runtime System.String value.
func csharpDecodeStringLiteral(text string) (string, bool) {
	text = strings.TrimSpace(text)
	value, consumed, ok := csharpDecodeStringLiteralPrefix(text)
	if !ok || strings.TrimSpace(text[consumed:]) != "" {
		return "", false
	}
	return value, true
}

func csharpDecodeStringLiteralPrefix(text string) (string, int, bool) {
	if strings.HasPrefix(text, `@"`) {
		return csharpDecodeVerbatimStringPrefix(text)
	}
	if len(text) < 2 || text[0] != '"' || strings.HasPrefix(text, `"""`) {
		return "", 0, false
	}

	decoded := make([]rune, 0, len(text))
	for i := 1; i < len(text); {
		switch text[i] {
		case '"':
			consumed := i + 1
			if strings.HasPrefix(text[consumed:], "u8") {
				return "", 0, false
			}
			value, ok := csharpNormalizeStringRunes(decoded)
			return value, consumed, ok
		case '\r', '\n':
			return "", 0, false
		case '\\':
			i++
			if i >= len(text) {
				return "", 0, false
			}
			switch text[i] {
			case '\'', '"', '\\':
				decoded = append(decoded, rune(text[i]))
				i++
			case '0':
				decoded = append(decoded, 0)
				i++
			case 'a':
				decoded = append(decoded, '\a')
				i++
			case 'b':
				decoded = append(decoded, '\b')
				i++
			case 'e':
				decoded = append(decoded, 0x1b)
				i++
			case 'f':
				decoded = append(decoded, '\f')
				i++
			case 'n':
				decoded = append(decoded, '\n')
				i++
			case 'r':
				decoded = append(decoded, '\r')
				i++
			case 't':
				decoded = append(decoded, '\t')
				i++
			case 'v':
				decoded = append(decoded, '\v')
				i++
			case 'x':
				value, next, ok := csharpReadHexEscape(text, i+1, 1, 4)
				if !ok {
					return "", 0, false
				}
				decoded = append(decoded, rune(value))
				i = next
			case 'u':
				value, next, ok := csharpReadHexEscape(text, i+1, 4, 4)
				if !ok {
					return "", 0, false
				}
				decoded = append(decoded, rune(value))
				i = next
			case 'U':
				value, next, ok := csharpReadHexEscape(text, i+1, 8, 8)
				if !ok || value > utf8.MaxRune || value >= 0xd800 && value <= 0xdfff {
					return "", 0, false
				}
				decoded = append(decoded, rune(value))
				i = next
			default:
				return "", 0, false
			}
		default:
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && size == 1 || csharpIsRegularStringNewline(r) {
				return "", 0, false
			}
			decoded = append(decoded, r)
			i += size
		}
	}
	return "", 0, false
}

func csharpDecodeVerbatimStringPrefix(text string) (string, int, bool) {
	decoded := make([]rune, 0, len(text))
	for i := 2; i < len(text); {
		if text[i] == '"' {
			if i+1 < len(text) && text[i+1] == '"' {
				decoded = append(decoded, '"')
				i += 2
				continue
			}
			consumed := i + 1
			if strings.HasPrefix(text[consumed:], "u8") {
				return "", 0, false
			}
			value, ok := csharpNormalizeStringRunes(decoded)
			return value, consumed, ok
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			return "", 0, false
		}
		decoded = append(decoded, r)
		i += size
	}
	return "", 0, false
}

func csharpReadHexEscape(text string, start, minDigits, maxDigits int) (uint32, int, bool) {
	var value uint32
	i := start
	for i < len(text) && i-start < maxDigits {
		digit, ok := csharpHexDigit(text[i])
		if !ok {
			break
		}
		value = value*16 + uint32(digit)
		i++
	}
	if i-start < minDigits {
		return 0, start, false
	}
	return value, i, true
}

func csharpHexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func csharpNormalizeStringRunes(decoded []rune) (string, bool) {
	out := make([]rune, 0, len(decoded))
	for i := 0; i < len(decoded); i++ {
		r := decoded[i]
		switch {
		case r >= 0xd800 && r <= 0xdbff:
			if i+1 >= len(decoded) || decoded[i+1] < 0xdc00 || decoded[i+1] > 0xdfff {
				return "", false
			}
			out = append(out, utf16.DecodeRune(r, decoded[i+1]))
			i++
		case r >= 0xdc00 && r <= 0xdfff, !utf8.ValidRune(r):
			return "", false
		default:
			out = append(out, r)
		}
	}
	return string(out), true
}

func csharpIsRegularStringNewline(r rune) bool {
	return r == '\r' || r == '\n' || r == '\u0085' || r == '\u2028' || r == '\u2029'
}

// csharpEFConfigEntity validates a direct base-list entry whose terminal
// type is exactly IEntityTypeConfiguration<T>. Nested occurrences such as
// Wrapper<IEntityTypeConfiguration<T>> are deliberately not configuration
// facts. Multiple distinct entity arguments are ambiguous and fail open.
func csharpEFConfigEntity(decl *sitter.Node, src []byte) (entity, identity string, ok bool) {
	baseList := csharpDirectChildOfType(decl, "base_list")
	if baseList == nil {
		return "", "", false
	}
	for i, count := 0, int(baseList.NamedChildCount()); i < count; i++ {
		arg, matched := csharpEFGenericTypeArgument(baseList.NamedChild(i), src, "IEntityTypeConfiguration")
		if !matched {
			continue
		}
		candidateIdentity, candidateEntity, named := csharpEFNamedType(arg, src)
		if !named {
			return "", "", false
		}
		if identity != "" && candidateIdentity != identity {
			return "", "", false
		}
		identity, entity = candidateIdentity, candidateEntity
	}
	return entity, identity, identity != ""
}

func csharpEFGenericTypeArgument(typeNode *sitter.Node, src []byte, expected string) (*sitter.Node, bool) {
	terminal := csharpEFTerminalTypeNode(typeNode)
	if terminal == nil || terminal.Type() != "generic_name" {
		return nil, false
	}
	name := terminal.ChildByFieldName("name")
	if name == nil && terminal.NamedChildCount() != 0 {
		name = terminal.NamedChild(0)
	}
	if name == nil || strings.TrimPrefix(name.Content(src), "@") != expected {
		return nil, false
	}
	typeArgs := terminal.ChildByFieldName("type_arguments")
	if typeArgs == nil {
		typeArgs = csharpDirectChildOfType(terminal, "type_argument_list")
	}
	if typeArgs == nil || typeArgs.NamedChildCount() != 1 {
		return nil, false
	}
	return typeArgs.NamedChild(0), true
}

func csharpEFTerminalTypeNode(typeNode *sitter.Node) *sitter.Node {
	for typeNode != nil {
		switch typeNode.Type() {
		case "simple_base_type":
			if typeNode.NamedChildCount() != 1 {
				return typeNode
			}
			typeNode = typeNode.NamedChild(0)
		case "qualified_name", "alias_qualified_name":
			next := typeNode.ChildByFieldName("name")
			if next == nil && typeNode.NamedChildCount() != 0 {
				next = typeNode.NamedChild(int(typeNode.NamedChildCount()) - 1)
			}
			typeNode = next
		default:
			return typeNode
		}
	}
	return nil
}

func csharpEFNamedType(typeNode *sitter.Node, src []byte) (identity, terminal string, ok bool) {
	last := csharpEFTerminalTypeNode(typeNode)
	if last == nil || last.Type() != "identifier" {
		return "", "", false
	}
	terminal = strings.TrimPrefix(last.Content(src), "@")
	if terminal == "" {
		return "", "", false
	}
	identity = csharpCompactTypeIdentity(typeNode.Content(src))
	return identity, terminal, identity != ""
}

func csharpCompactTypeIdentity(text string) string {
	text = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(text)
	parts := strings.Split(text, ".")
	for i := range parts {
		parts[i] = strings.TrimPrefix(parts[i], "@")
	}
	return strings.Join(parts, ".")
}

func csharpDirectChildOfType(node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i, count := 0, int(node.NamedChildCount()); i < count; i++ {
		child := node.NamedChild(i)
		if child != nil && child.Type() == kind {
			return child
		}
	}
	return nil
}

// csharpEFConfigureMethod accepts only the direct, unambiguous Configure
// implementation for the same T named by IEntityTypeConfiguration<T>:
// void Configure(EntityTypeBuilder<T> receiver) with a block body.
func csharpEFConfigureMethod(decl *sitter.Node, src []byte, entityIdentity string) (*sitter.Node, string, bool) {
	body := decl.ChildByFieldName("body")
	if body == nil {
		body = csharpDirectChildOfType(decl, "declaration_list")
	}
	if body == nil {
		return nil, "", false
	}
	var method *sitter.Node
	receiver := ""
	for i, count := 0, int(body.NamedChildCount()); i < count; i++ {
		candidate := body.NamedChild(i)
		if candidate == nil || candidate.Type() != "method_declaration" {
			continue
		}
		candidateIdentity, candidateReceiver, valid := csharpEFConfigureSignature(candidate, src)
		if !valid || candidateIdentity != entityIdentity {
			continue
		}
		if method != nil {
			return nil, "", false
		}
		method, receiver = candidate, candidateReceiver
	}
	return method, receiver, method != nil
}

func csharpEFConfigureSignature(method *sitter.Node, src []byte) (entityIdentity, receiver string, ok bool) {
	name := method.ChildByFieldName("name")
	if name == nil || name.Content(src) != "Configure" {
		return "", "", false
	}
	returns := method.ChildByFieldName("returns")
	if returns == nil {
		returns = method.ChildByFieldName("type")
	}
	if returns == nil || returns.Content(src) != "void" {
		return "", "", false
	}
	parameters := method.ChildByFieldName("parameters")
	if parameters == nil || parameters.NamedChildCount() != 1 {
		return "", "", false
	}
	parameter := parameters.NamedChild(0)
	if parameter == nil || parameter.Type() != "parameter" {
		return "", "", false
	}
	parameterType := parameter.ChildByFieldName("type")
	arg, matched := csharpEFGenericTypeArgument(parameterType, src, "EntityTypeBuilder")
	if !matched {
		return "", "", false
	}
	identity, _, named := csharpEFNamedType(arg, src)
	if !named {
		return "", "", false
	}
	parameterName := parameter.ChildByFieldName("name")
	if parameterName == nil || parameterName.Type() != "identifier" {
		return "", "", false
	}
	block := method.ChildByFieldName("body")
	if block == nil || block.Type() != "block" {
		return "", "", false
	}
	return identity, strings.TrimPrefix(parameterName.Content(src), "@"), true
}

// stampCSharpEFConfig records only a mapping proved by an exact
// IEntityTypeConfiguration<T> implementation and its matching Configure
// method. Scalar keys remain for the resolver compatibility contract.
func stampCSharpEFConfig(decl *sitter.Node, src []byte, meta map[string]any) {
	entity, identity, ok := csharpEFConfigEntity(decl, src)
	if !ok {
		return
	}
	configure, _, ok := csharpEFConfigureMethod(decl, src, identity)
	if !ok {
		return
	}
	meta["ef_config_entity"] = entity
	table, schema, relation := csharpEFConfigTableCall(configure, src)
	if table == "" {
		return
	}
	meta["ef_config_table"] = table
	meta["ef_config_relation"] = relation
	if schema != "" {
		meta["ef_config_schema"] = schema
	}
}

// csharpEFConfigTableCall scans only the validated Configure body and
// credits direct calls on its EntityTypeBuilder<T> parameter. Calls are
// visited in source order and the last valid mapping wins, matching EF.
func csharpEFConfigTableCall(decl *sitter.Node, src []byte) (table, schema, relation string) {
	method := decl
	if method == nil {
		return "", "", ""
	}
	if method.Type() != "method_declaration" {
		_, identity, valid := csharpEFConfigEntity(method, src)
		if !valid {
			return "", "", ""
		}
		method, _, valid = csharpEFConfigureMethod(method, src, identity)
		if !valid {
			return "", "", ""
		}
	}
	_, receiver, valid := csharpEFConfigureSignature(method, src)
	if !valid {
		return "", "", ""
	}
	body := method.ChildByFieldName("body")
	csharpWalkEFInvocations(body, func(inv *sitter.Node) {
		t, s, r, matched := csharpEFTableViewArgs(inv, src)
		if !matched {
			return
		}
		fn := inv.ChildByFieldName("function")
		if !csharpEFDirectReceiver(fn, src, receiver) {
			return
		}
		table, schema, relation = t, s, r
	})
	return table, schema, relation
}

type csharpEFArgument struct {
	name  string
	value *sitter.Node
}

func csharpEFArguments(inv *sitter.Node, src []byte) ([]csharpEFArgument, bool) {
	if inv == nil {
		return nil, false
	}
	list := inv.ChildByFieldName("arguments")
	if list == nil {
		return nil, false
	}
	args := make([]csharpEFArgument, 0, list.NamedChildCount())
	for i, count := 0, int(list.NamedChildCount()); i < count; i++ {
		node := list.NamedChild(i)
		if node == nil {
			return nil, false
		}
		if node.Type() != "argument" {
			args = append(args, csharpEFArgument{value: node})
			continue
		}
		name := ""
		nameNode := node.ChildByFieldName("name")
		for j, children := 0, int(node.NamedChildCount()); j < children; j++ {
			child := node.NamedChild(j)
			if child != nil && child.Type() == "name_colon" {
				nameNode = child
			}
		}
		if nameNode != nil {
			name = csharpEFNameColon(nameNode, src)
			if name == "" {
				return nil, false
			}
		}
		value := node.ChildByFieldName("value")
		if value == nil {
			value = node.ChildByFieldName("expression")
		}
		if value == nil && node.NamedChildCount() != 0 {
			value = node.NamedChild(int(node.NamedChildCount()) - 1)
		}
		if value == nil || nameNode != nil && value.Equal(nameNode) {
			return nil, false
		}
		args = append(args, csharpEFArgument{name: name, value: value})
	}
	return args, true
}

func csharpEFNameColon(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	if name := node.ChildByFieldName("name"); name != nil {
		return strings.TrimPrefix(name.Content(src), "@")
	}
	if node.NamedChildCount() != 0 {
		return strings.TrimPrefix(node.NamedChild(0).Content(src), "@")
	}
	return strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(node.Content(src)), ":"), "@")
}

// csharpEFTableViewArgs recognises literal ToTable/ToView name and schema
// arguments, including C# name:/schema: argument syntax. A dynamic schema
// is refused because treating it as the default schema would be wrong.
func csharpEFTableViewArgs(inv *sitter.Node, src []byte) (table, schema, relation string, ok bool) {
	fn, name, valid := csharpEFMemberCall(inv, src)
	if !valid || name != "ToTable" && name != "ToView" {
		return "", "", "", false
	}
	_ = fn
	args, valid := csharpEFArguments(inv, src)
	if !valid {
		return "", "", "", false
	}
	var tableNode, schemaNode *sitter.Node
	tableSet, schemaSet := false, false
	position := 0
	for _, arg := range args {
		switch arg.name {
		case "":
			switch position {
			case 0:
				tableNode, tableSet = arg.value, true
			case 1:
				schemaNode, schemaSet = arg.value, true
			default:
				return "", "", "", false
			}
			position++
		case "name":
			if tableSet {
				return "", "", "", false
			}
			tableNode, tableSet = arg.value, true
		case "schema":
			if schemaSet {
				return "", "", "", false
			}
			schemaNode, schemaSet = arg.value, true
		default:
			return "", "", "", false
		}
	}
	if !tableSet || tableNode == nil {
		return "", "", "", false
	}
	table, valid = csharpDecodeStringLiteral(tableNode.Content(src))
	if !valid || table == "" {
		return "", "", "", false
	}
	if schemaSet {
		if !csharpEFStaticNull(schemaNode, src) {
			schema, valid = csharpDecodeStringLiteral(schemaNode.Content(src))
			if !valid {
				return "", "", "", false
			}
		}
	}
	relation = "table"
	if name == "ToView" {
		relation = "view"
	}
	return table, schema, relation, true
}

var csharpEFSubjectChangers = map[string]bool{
	"OwnsOne": true, "OwnsMany": true,
	"HasOne": true, "HasMany": true,
	"WithOne": true, "WithMany": true,
	"Navigation": true, "ComplexProperty": true,
}

func csharpEFEntityFromChainRoot(fn *sitter.Node, src []byte, root string) string {
	if fn == nil || fn.Type() != "member_access_expression" {
		return ""
	}
	expr := fn.ChildByFieldName("expression")
	for expr != nil && expr.Type() == "invocation_expression" {
		callFn := expr.ChildByFieldName("function")
		if callFn == nil || callFn.Type() != "member_access_expression" {
			return ""
		}
		nameNode := callFn.ChildByFieldName("name")
		if nameNode == nil {
			return ""
		}
		name := csharpEFMemberName(nameNode, src)
		if arg, entityCall := csharpEFGenericTypeArgument(nameNode, src, "Entity"); entityCall {
			arguments := expr.ChildByFieldName("arguments")
			if arguments == nil || arguments.NamedChildCount() != 0 {
				return ""
			}
			base := callFn.ChildByFieldName("expression")
			if base == nil || base.Type() != "identifier" {
				return ""
			}
			baseName := strings.TrimPrefix(base.Content(src), "@")
			if root != "" && baseName != root {
				return ""
			}
			_, entity, named := csharpEFNamedType(arg, src)
			if !named {
				return ""
			}
			return entity
		}
		if csharpEFSubjectChangers[name] {
			return ""
		}
		expr = callFn.ChildByFieldName("expression")
	}
	return ""
}

func csharpEFMemberName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	if node.Type() == "generic_name" {
		name := node.ChildByFieldName("name")
		if name == nil && node.NamedChildCount() != 0 {
			name = node.NamedChild(0)
		}
		if name != nil && name.Type() == "identifier" {
			return strings.TrimPrefix(name.Content(src), "@")
		}
		return ""
	}
	if node.Type() != "identifier" {
		return ""
	}
	return strings.TrimPrefix(node.Content(src), "@")
}

func csharpEFMemberCall(inv *sitter.Node, src []byte) (*sitter.Node, string, bool) {
	if inv == nil || inv.Type() != "invocation_expression" {
		return nil, "", false
	}
	fn := inv.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_access_expression" {
		return nil, "", false
	}
	name := csharpEFMemberName(fn.ChildByFieldName("name"), src)
	return fn, name, name != ""
}

func csharpEFDirectReceiver(fn *sitter.Node, src []byte, expected string) bool {
	if fn == nil || fn.Type() != "member_access_expression" {
		return false
	}
	receiver := fn.ChildByFieldName("expression")
	return receiver != nil && receiver.Type() == "identifier" &&
		strings.TrimPrefix(receiver.Content(src), "@") == expected
}

func csharpWalkEFInvocations(root *sitter.Node, visit func(*sitter.Node)) {
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil {
			return
		}
		if node != root {
			switch node.Type() {
			case "lambda_expression", "anonymous_method_expression", "local_function_statement", "method_declaration":
				return
			}
		}
		if node.Type() == "invocation_expression" {
			visit(node)
		}
		for i, count := 0, int(node.NamedChildCount()); i < count; i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)
}

func csharpEFDbContextClass(decl *sitter.Node, src []byte) (string, bool) {
	if decl == nil || decl.Type() != "class_declaration" {
		return "", false
	}
	baseList := csharpDirectChildOfType(decl, "base_list")
	if baseList == nil {
		return "", false
	}
	isContext := false
	for i, count := 0, int(baseList.NamedChildCount()); i < count; i++ {
		identity, terminal, named := csharpEFNamedType(baseList.NamedChild(i), src)
		if !named || terminal != "DbContext" {
			continue
		}
		if identity == "DbContext" || identity == "Microsoft.EntityFrameworkCore.DbContext" ||
			identity == "global::Microsoft.EntityFrameworkCore.DbContext" {
			isContext = true
		}
	}
	name := decl.ChildByFieldName("name")
	if !isContext || name == nil || name.Type() != "identifier" {
		return "", false
	}
	return strings.TrimPrefix(name.Content(src), "@"), true
}

func csharpEFOnModelCreating(decl *sitter.Node, src []byte) (*sitter.Node, string, bool) {
	body := decl.ChildByFieldName("body")
	if body == nil {
		body = csharpDirectChildOfType(decl, "declaration_list")
	}
	if body == nil {
		return nil, "", false
	}
	var found *sitter.Node
	receiver := ""
	for i, count := 0, int(body.NamedChildCount()); i < count; i++ {
		method := body.NamedChild(i)
		candidateReceiver, valid := csharpEFOnModelCreatingSignature(method, src)
		if !valid {
			continue
		}
		if found != nil {
			return nil, "", false
		}
		found, receiver = method, candidateReceiver
	}
	return found, receiver, found != nil
}

func csharpEFOnModelCreatingSignature(method *sitter.Node, src []byte) (string, bool) {
	if method == nil || method.Type() != "method_declaration" {
		return "", false
	}
	name := method.ChildByFieldName("name")
	if name == nil || name.Type() != "identifier" || name.Content(src) != "OnModelCreating" {
		return "", false
	}
	returns := method.ChildByFieldName("returns")
	if returns == nil {
		returns = method.ChildByFieldName("type")
	}
	if returns == nil || returns.Content(src) != "void" || !csharpEFHasDirectToken(method, src, "override") {
		return "", false
	}
	parameters := method.ChildByFieldName("parameters")
	if parameters == nil || parameters.NamedChildCount() != 1 {
		return "", false
	}
	parameter := parameters.NamedChild(0)
	if parameter == nil || parameter.Type() != "parameter" {
		return "", false
	}
	identity, terminal, named := csharpEFNamedType(parameter.ChildByFieldName("type"), src)
	if !named || terminal != "ModelBuilder" || identity != "ModelBuilder" && identity != "Microsoft.EntityFrameworkCore.ModelBuilder" && identity != "global::Microsoft.EntityFrameworkCore.ModelBuilder" {
		return "", false
	}
	parameterName := parameter.ChildByFieldName("name")
	body := method.ChildByFieldName("body")
	if parameterName == nil || parameterName.Type() != "identifier" || body == nil || body.Type() != "block" {
		return "", false
	}
	return strings.TrimPrefix(parameterName.Content(src), "@"), true
}

func csharpEFHasDirectToken(node *sitter.Node, src []byte, token string) bool {
	for i, count := 0, int(node.ChildCount()); i < count; i++ {
		child := node.Child(i)
		if child != nil && child.Content(src) == token {
			return true
		}
	}
	return false
}

func csharpEFApplyConfiguration(inv *sitter.Node, src []byte, receiver string) (string, bool) {
	fn, name, valid := csharpEFMemberCall(inv, src)
	if !valid || name != "ApplyConfiguration" || !csharpEFDirectReceiver(fn, src, receiver) {
		return "", false
	}
	args, valid := csharpEFArguments(inv, src)
	if !valid || len(args) != 1 || args[0].name != "" && args[0].name != "configuration" {
		return "", false
	}
	creation := args[0].value
	if creation == nil || creation.Type() != "object_creation_expression" {
		return "", false
	}
	_, config, named := csharpEFNamedType(creation.ChildByFieldName("type"), src)
	return config, named
}

func csharpEFApplyAssembly(inv *sitter.Node, src []byte, receiver, context string) bool {
	fn, name, valid := csharpEFMemberCall(inv, src)
	if !valid || name != "ApplyConfigurationsFromAssembly" || !csharpEFDirectReceiver(fn, src, receiver) {
		return false
	}
	args, valid := csharpEFArguments(inv, src)
	if !valid {
		return false
	}
	var assembly, predicate *sitter.Node
	assemblySet, predicateSet, position := false, false, 0
	for _, arg := range args {
		switch arg.name {
		case "":
			switch position {
			case 0:
				assembly, assemblySet = arg.value, true
			case 1:
				predicate, predicateSet = arg.value, true
			default:
				return false
			}
			position++
		case "assembly":
			if assemblySet {
				return false
			}
			assembly, assemblySet = arg.value, true
		case "predicate":
			if predicateSet {
				return false
			}
			predicate, predicateSet = arg.value, true
		default:
			return false
		}
	}
	if !assemblySet || predicateSet && !csharpEFStaticNull(predicate, src) {
		return false
	}
	return csharpEFCurrentAssembly(assembly, src, context)
}

func csharpEFCurrentAssembly(expr *sitter.Node, src []byte, context string) bool {
	if expr == nil || expr.Type() != "member_access_expression" {
		return false
	}
	name := expr.ChildByFieldName("name")
	typeof := expr.ChildByFieldName("expression")
	if name == nil || name.Type() != "identifier" || name.Content(src) != "Assembly" || typeof == nil || typeof.Type() != "typeof_expression" {
		return false
	}
	typeNode := typeof.ChildByFieldName("type")
	if typeNode == nil && typeof.NamedChildCount() == 1 {
		typeNode = typeof.NamedChild(0)
	}
	identity, _, named := csharpEFNamedType(typeNode, src)
	return named && identity == context
}

func csharpEFStaticNull(node *sitter.Node, src []byte) bool {
	return node != nil && node.Type() == "null_literal" && strings.TrimSpace(node.Content(src)) == "null"
}

// stampCSharpEFFluent records lexical EF actions in source order. Each
// map uses the persisted structured contract: context/kind/line/ordinal,
// plus mapping or configuration fields appropriate to the action kind.
func stampCSharpEFFluent(root *sitter.Node, src []byte, fileNode *graph.Node) {
	actions := make([]map[string]any, 0)
	walkNodes(root, func(decl *sitter.Node) {
		context, valid := csharpEFDbContextClass(decl, src)
		if !valid {
			return
		}
		method, receiver, valid := csharpEFOnModelCreating(decl, src)
		if !valid {
			return
		}
		body := method.ChildByFieldName("body")
		csharpWalkEFInvocations(body, func(inv *sitter.Node) {
			if table, schema, relation, matched := csharpEFTableViewArgs(inv, src); matched {
				entity := csharpEFEntityFromChainRoot(inv.ChildByFieldName("function"), src, receiver)
				if entity != "" {
					actions = append(actions, map[string]any{
						"context":  context,
						"kind":     "mapping",
						"line":     int(inv.StartPoint().Row) + 1,
						"ordinal":  len(actions),
						"entity":   entity,
						"table":    table,
						"schema":   schema,
						"relation": relation,
					})
					return
				}
			}
			if config, matched := csharpEFApplyConfiguration(inv, src, receiver); matched {
				actions = append(actions, map[string]any{
					"context": context,
					"kind":    "apply_configuration",
					"line":    int(inv.StartPoint().Row) + 1,
					"ordinal": len(actions),
					"config":  config,
				})
				return
			}
			if csharpEFApplyAssembly(inv, src, receiver, context) {
				actions = append(actions, map[string]any{
					"context": context,
					"kind":    "apply_assembly",
					"line":    int(inv.StartPoint().Row) + 1,
					"ordinal": len(actions),
				})
			}
		})
	})
	if len(actions) == 0 {
		return
	}
	if fileNode.Meta == nil {
		fileNode.Meta = map[string]any{}
	}
	fileNode.Meta["ef_fluent"] = actions
}

// emitCSharpORMEdges materialises the KindTable node + EdgeModelsTable
// edges for a C# type, mirroring emitJavaORMEdges: the per-file table
// node dedup happens in one place.
func emitCSharpORMEdges(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult) {
	for _, e := range detectCSharpORMModel(classNode, src, classID, filePath) {
		if e == nil {
			continue
		}
		if !ormTableNodeAlreadyEmitted(result, e.To) {
			schema, _ := e.Meta["schema"].(string)
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       e.To,
				Kind:     graph.KindTable,
				Name:     e.Meta["table_name"].(string),
				FilePath: filePath,
				Language: "csharp",
				Meta: map[string]any{
					"dialect": "orm",
					"schema":  schema,
					"source":  "csharp-orm",
				},
			})
		}
		result.Edges = append(result.Edges, e)
	}
}
