package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// handleAnalyzeConstructorsMissingFields surfaces struct/class
// literal sites that don't populate every field defined on the type.
// Catches the classic "added a field, forgot to populate at 3 of 5
// instantiation sites" bug class.
//
// Heuristic, not a full literal-site analyzer:
//
//  1. For every type node T with member fields (via inbound
//     EdgeMemberOf from field nodes), collect T_fields.
//  2. For every EdgeInstantiates whose target is T, take the
//     origin function F.
//  3. Look at every outbound EdgeReferences from F whose target
//     is a member field of T. The set of referenced field names
//     is F_referenced.
//  4. missing = T_fields - F_referenced. When non-empty, emit a
//     row (F, T, missing fields).
//
// False positives:
//   - F populates the field via shorthand the extractor doesn't
//     emit a Reference edge for (rare; we know which extractors emit
//     references, but the field-write case isn't always tagged).
//   - The field carries a meaningful zero-value default (Go zero,
//     struct embedding, JSON omitempty). The analyzer flags these
//     conservatively — agents can suppress by tagging the field
//     with meta["nullable"]=true.
//
// False negatives:
//   - F populates the field outside the literal (e.g. `c := Foo{};
//     c.X = 1`) — we count that as "referenced," which is what we
//     want for the "did you forget about this field at all" question.
//
// The accuracy is good enough for "is this an underpopulated
// literal site?" agent prompts; not good enough for compiler-grade
// claims. The wire response carries `accuracy: "heuristic"` so
// agents don't oversell.
func (s *Server) handleAnalyzeConstructorsMissingFields(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	minMissing := max(req.GetInt("min_missing", 1), 1)
	typeFilter := strings.TrimSpace(req.GetString("type_id", ""))
	limit := max(req.GetInt("limit", 100), 1)

	scoped := s.scopedNodes(ctx)
	scopedSet := make(map[string]*graph.Node, len(scoped))
	for _, n := range scoped {
		scopedSet[n.ID] = n
	}

	// Step 1: index types → their member fields.
	typeFields := map[string]map[string]*graph.Node{} // typeID → {fieldName: fieldNode}
	for _, n := range scoped {
		if n.Kind != graph.KindField {
			continue
		}
		if isUnsettableMember(n) {
			continue
		}
		for _, e := range s.graph.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeMemberOf {
				continue
			}
			if scopedSet[e.To] == nil {
				continue
			}
			if typeFilter != "" && e.To != typeFilter {
				continue
			}
			if !graphpath.HasPrefix(scopedSet[e.To].FilePath, pathPrefix) {
				continue
			}
			if typeFields[e.To] == nil {
				typeFields[e.To] = map[string]*graph.Node{}
			}
			typeFields[e.To][n.Name] = n
		}
	}

	type missingRow struct {
		Function      string   `json:"function_id"`
		FunctionName  string   `json:"function_name"`
		File          string   `json:"file"`
		Line          int      `json:"line"`
		Type          string   `json:"type_id"`
		TypeName      string   `json:"type_name"`
		MissingFields []string `json:"missing_fields"`
		TotalFields   int      `json:"total_fields"`
	}

	rows := []missingRow{}
	for typeID, fields := range typeFields {
		typeNode := scopedSet[typeID]
		if typeNode == nil || len(fields) == 0 {
			continue
		}

		// Step 2: every function that instantiates this type.
		for _, e := range s.graph.GetInEdges(typeID) {
			if e.Kind != graph.EdgeInstantiates {
				continue
			}
			f := scopedSet[e.From]
			if f == nil {
				continue
			}
			if f.Kind != graph.KindFunction && f.Kind != graph.KindMethod {
				continue
			}

			// Step 3: which member fields does F reference?
			referenced := map[string]bool{}
			for _, ref := range s.graph.GetOutEdges(f.ID) {
				if ref.Kind != graph.EdgeReferences {
					continue
				}
				target := s.graph.GetNode(ref.To)
				if target == nil || target.Kind != graph.KindField {
					continue
				}
				if _, isFieldOfT := fields[target.Name]; !isFieldOfT {
					continue
				}
				referenced[target.Name] = true
			}

			// Step 4: missing = T_fields - referenced.
			missing := []string{}
			for name, fnode := range fields {
				if referenced[name] {
					continue
				}
				if isNullableField(fnode) {
					continue
				}
				missing = append(missing, name)
			}
			if len(missing) < minMissing {
				continue
			}
			sort.Strings(missing)

			rows = append(rows, missingRow{
				Function:      f.ID,
				FunctionName:  f.Name,
				File:          f.FilePath,
				Line:          f.StartLine,
				Type:          typeID,
				TypeName:      typeNode.Name,
				MissingFields: missing,
				TotalFields:   len(fields),
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if len(rows[i].MissingFields) != len(rows[j].MissingFields) {
			return len(rows[i].MissingFields) > len(rows[j].MissingFields)
		}
		if rows[i].Function != rows[j].Function {
			return rows[i].Function < rows[j].Function
		}
		return rows[i].Type < rows[j].Type
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"sites":       rows,
		"total":       len(rows),
		"truncated":   truncated,
		"accuracy":    "heuristic",
		"min_missing": minMissing,
	})
}

// isNullableField returns true when the field carries an explicit
// nullable / optional / omitempty marker the agent can use to
// suppress false positives. Reads three meta keys:
//   - meta["nullable"]   bool — explicit opt-out
//   - meta["optional"]   bool — same intent, different convention
//   - meta["json_tag"]   string containing "omitempty" — Go convention
// isUnsettableMember reports whether a field-kind member cannot be
// assigned at construction, which makes it a guaranteed false positive
// here: this heuristic asks which of a type's members an instantiation
// site left unset, and a member that no instantiation CAN set is
// missing at every site forever. C# indexers and events are both
// field-kind member nodes and both qualify - an indexer needs an index
// argument, so it can never appear in an object initializer, and an
// event is reachable only through += and -=.
func isUnsettableMember(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	switch k, _ := n.Meta["kind"].(string); k {
	case "indexer", "event_accessor":
		return true
	}
	return false
}

func isNullableField(n *graph.Node) bool {
	if n.Meta == nil {
		return false
	}
	if b, ok := n.Meta["nullable"].(bool); ok && b {
		return true
	}
	if b, ok := n.Meta["optional"].(bool); ok && b {
		return true
	}
	if tag, ok := n.Meta["json_tag"].(string); ok && strings.Contains(tag, "omitempty") {
		return true
	}
	return false
}
