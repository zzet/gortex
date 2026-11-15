package contracts

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GraphQLExtractor detects GraphQL schema definitions (providers) and query usage (consumers).
type GraphQLExtractor struct{}

var (
	// Schema providers: type Query { field: Type } or type Mutation { ... }
	gqlTypeBlockRe = regexp.MustCompile(`(?m)type\s+(Query|Mutation|Subscription)\s*\{([^}]*)\}`)
	gqlFieldRe     = regexp.MustCompile(`(?m)^\s*(\w+)`)

	// Consumer query strings: query { users { ... } } or mutation { createUser(...) { ... } }
	gqlQueryOpRe  = regexp.MustCompile(`(?m)(query|mutation|subscription)\s*(?:\w+\s*)?\{([^}]*)\}`)
	gqlGqlTagRe   = regexp.MustCompile("(?s)gql`([^`]*)`")
	gqlTopFieldRe = regexp.MustCompile(`(?m)^\s*(\w+)`)
)

func (e *GraphQLExtractor) SupportedLanguages() []string {
	return []string{"graphql", "go", "typescript", "javascript", "python"}
}

// gqlConsumerMarkers is the substring prefilter for the consumer
// scan. Every consumer pattern needs one of:
//   - a gql“ tagged template literal (marker: "gql`")
//   - a bare `query` / `mutation` / `subscription` operation block
//
// Reject any file that contains none. Schema files (.graphql/.gql)
// are already routed to extractSchemaProviders before this check.
var gqlConsumerMarkers = [][]byte{
	[]byte("gql`"),
	[]byte("query"),
	[]byte("mutation"),
	[]byte("subscription"),
}

func (e *GraphQLExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract

	isSchemaFile := strings.HasSuffix(filePath, ".graphql") || strings.HasSuffix(filePath, ".gql")
	if isSchemaFile {
		contracts = append(contracts, e.extractSchemaProviders(filePath, src)...)
	}

	// NestJS code-first resolvers (@Query/@Mutation/@Subscription) live in
	// source files and must run before the consumer-marker prefilter below,
	// which would otherwise short-circuit a resolver file with no gql tag.
	contracts = append(contracts, e.extractNestResolvers(filePath, src, nodes)...)

	// Consumer scan: skip entirely when the file has no marker
	// substring that any consumer regex could possibly match. A
	// schema-only .graphql file lacks lowercase `query`/`mutation`/
	// `subscription` operation blocks and no gql-tagged templates,
	// so it short-circuits here — the provider pass ran above.
	if !srcHasAnyMarker(src, gqlConsumerMarkers) {
		return contracts
	}

	// Consumer queries can live in any source file. Thread file nodes
	// through so findEnclosingSymbol can anchor each consumer contract
	// on its calling function — required for EdgeMatches bridges.
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})
	contracts = append(contracts, e.extractConsumers(filePath, src, fileNodes)...)

	return contracts
}

func (e *GraphQLExtractor) extractSchemaProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	for _, blockMatch := range gqlTypeBlockRe.FindAllStringSubmatchIndex(text, -1) {
		typeName := text[blockMatch[2]:blockMatch[3]] // Query, Mutation, Subscription
		body := text[blockMatch[4]:blockMatch[5]]
		bodyOffset := blockMatch[4]

		for _, fLine := range strings.Split(body, "\n") {
			fLine = strings.TrimSpace(fLine)
			if fLine == "" || strings.HasPrefix(fLine, "#") {
				continue
			}
			fm := gqlFieldRe.FindStringSubmatch(fLine)
			if fm == nil {
				continue
			}
			fieldName := fm[1]
			// Approximate offset for line number.
			idx := strings.Index(text[bodyOffset:], fLine)
			line := 1
			if idx >= 0 {
				line = lineNumber(lines, bodyOffset+idx)
			}
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("graphql::%s::%s", typeName, fieldName),
				Type:       ContractGraphQL,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       line,
				Meta:       map[string]any{"operation": typeName, "field": fieldName},
				Confidence: 0.95,
			})
		}
	}

	return contracts
}

// gqlNestOpRe matches a NestJS code-first operation decorator.
var gqlNestOpRe = regexp.MustCompile(`@(Query|Mutation|Subscription)\s*\(`)

// gqlNestNameRe pulls an explicit `name: 'foo'` option out of the decorator.
var gqlNestNameRe = regexp.MustCompile(`name:\s*["']([^"']+)["']`)

// extractNestResolvers detects NestJS code-first resolver operations —
// @Query/@Mutation/@Subscription on a resolver method — and records each as a
// GraphQL provider. The operation field is the explicit `name:` option when
// present, otherwise the decorated method's name.
func (e *GraphQLExtractor) extractNestResolvers(filePath string, src []byte, nodes []*graph.Node) []Contract {
	text := string(src)
	if !strings.Contains(text, "@Query(") && !strings.Contains(text, "@Mutation(") && !strings.Contains(text, "@Subscription(") {
		return nil
	}
	lines := strings.Split(text, "\n")
	fileNodes := filterFileNodes(filePath, nodes)

	var out []Contract
	for i, line := range lines {
		m := gqlNestOpRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		opType := m[1] // Query | Mutation | Subscription
		handlerName, _ := nestHandlerAfter(lines, i)
		field := ""
		if nm := gqlNestNameRe.FindStringSubmatch(line); nm != nil {
			field = nm[1]
		} else {
			field = handlerName
		}
		if field == "" {
			continue
		}
		symbolID := ""
		if handlerName != "" {
			symbolID = findFunctionByName(fileNodes, handlerName)
		}
		out = append(out, Contract{
			ID:         fmt.Sprintf("graphql::%s::%s", opType, field),
			Type:       ContractGraphQL,
			Role:       RoleProvider,
			SymbolID:   symbolID,
			FilePath:   filePath,
			Line:       i + 1,
			Meta:       map[string]any{"operation": opType, "field": field, "framework": "nestjs"},
			Confidence: 0.9,
		})
	}
	return out
}

func (e *GraphQLExtractor) extractConsumers(filePath string, src []byte, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Ordinary Go syntax, such as type query struct, can look like an
	// operation to the regex. In Go a query document lives in a string.
	isGo := strings.HasSuffix(filePath, ".go")
	var operations [][]int
	if isGo {
		if gqlQueryOpRe.Match(src) {
			operations = goGraphQLStringOperations(filePath, src)
		}
	} else {
		operations = gqlQueryOpRe.FindAllStringSubmatchIndex(text, -1)
	}
	for _, m := range operations {
		opType := text[m[2]:m[3]]
		body := text[m[4]:m[5]]
		contracts = append(contracts, e.fieldsFromBody(filePath, opType, body, lines, m[0], fileNodes)...)
	}

	if isGo {
		return contracts
	}

	// gql`` tagged template literals.
	for _, m := range gqlGqlTagRe.FindAllStringSubmatchIndex(text, -1) {
		inner := text[m[2]:m[3]]
		// Try to find the operation keyword inside the template.
		for _, opM := range gqlQueryOpRe.FindAllStringSubmatch(inner, -1) {
			contracts = append(contracts, e.fieldsFromBody(filePath, opM[1], opM[2], lines, m[0], fileNodes)...)
		}
	}

	return contracts
}

// goGraphQLStringOperations keeps capture offsets relative to the file so
// the caller can still attribute each operation to its line and symbol.
func goGraphQLStringOperations(filePath string, src []byte) [][]int {
	var matches [][]int
	file := token.NewFileSet().AddFile(filePath, -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	for pos, tok, lit := s.Scan(); tok != token.EOF; pos, tok, lit = s.Scan() {
		if tok != token.STRING {
			continue
		}
		start := file.Offset(pos)
		end := start + len(lit)
		if src[start] == '`' {
			// The Go scanner strips carriage returns from raw literals.
			// Look for the closing quote in the source to retain offsets.
			if close := bytes.IndexByte(src[start+1:], '`'); close >= 0 {
				end = start + close + 2
			}
		}
		for _, m := range gqlQueryOpRe.FindAllSubmatchIndex(src[start:end], -1) {
			for i := range m {
				m[i] += start
			}
			matches = append(matches, m)
		}
	}
	return matches
}

func (e *GraphQLExtractor) fieldsFromBody(filePath, opType, body string, lines []string, baseOffset int, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	opTypeCap := strings.ToUpper(opType[:1]) + opType[1:]
	ln := lineNumber(lines, baseOffset)
	sym := findEnclosingSymbol(fileNodes, ln)

	for _, fLine := range strings.Split(body, "\n") {
		fLine = strings.TrimSpace(fLine)
		if fLine == "" {
			continue
		}
		fm := gqlTopFieldRe.FindStringSubmatch(fLine)
		if fm == nil {
			continue
		}
		fieldName := fm[1]
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("graphql::%s::%s", opTypeCap, fieldName),
			Type:       ContractGraphQL,
			Role:       RoleConsumer,
			SymbolID:   sym,
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"operation": opTypeCap, "field": fieldName},
			Confidence: 0.8,
		})
	}

	return contracts
}
