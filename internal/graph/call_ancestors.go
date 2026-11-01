package graph

import "context"

// CallAncestorRow is one selected calls/references edge in a candidate's
// inverse closure. Repeated edge rows are significant for fan-in. Source
// classification needs only its kind and file, never retrieval metadata.
type CallAncestorRow struct {
	From, To string
	FromKind NodeKind
	FromFile string
}

type CallAncestorReader interface {
	CallAncestorsContext(context.Context, []string) ([]CallAncestorRow, error)
}

type FilteredCallAncestorReader interface {
	CallAncestorsFilteredContext(context.Context, []string, OutlineEdgeFilter) ([]CallAncestorRow, error)
}

func CallAncestorsContext(ctx context.Context, reader Reader, candidates []string) ([]CallAncestorRow, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if capable, ok := reader.(CallAncestorReader); ok {
		rows, err := capable.CallAncestorsContext(ctx, candidates)
		if err != nil {
			return nil, err
		}
		return rows, ctx.Err()
	}
	return CallAncestorsOf(ctx, reader, candidates)
}

// CallAncestorsOf is the selected-reader Go oracle and composed-view fallback.
// Missing intermediate nodes remain traversable identities, as in the legacy
// forward test walk; only existing function/method nodes can later seed tests.
func CallAncestorsOf(ctx context.Context, reader Reader, candidates []string) ([]CallAncestorRow, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	frontier := uniqueNonEmpty(candidates)
	seen := make(map[string]bool, len(frontier))
	for _, id := range frontier {
		seen[id] = true
	}
	nodes := make(map[string]*Node)
	loaded := make(map[string]bool)
	var out []CallAncestorRow
	for len(frontier) > 0 {
		var edges []*Edge
		var next, sourceIDs []string
		for _, id := range frontier {
			rows, err := ReadKindAdjacency(ctx, reader, id, DirectionBackward, []EdgeKind{EdgeCalls, EdgeReferences})
			if err != nil {
				return nil, err
			}
			edges = append(edges, rows.Edges...)
			for _, edge := range rows.Edges {
				if !loaded[edge.From] {
					loaded[edge.From] = true
					sourceIDs = append(sourceIDs, edge.From)
				}
				if edge.From != "" && !seen[edge.From] {
					seen[edge.From] = true
					next = append(next, edge.From)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for id, node := range reader.GetNodesByIDs(sourceIDs) {
			nodes[id] = node
		}
		for _, edge := range edges {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			row := CallAncestorRow{From: edge.From, To: edge.To}
			if node := nodes[edge.From]; node != nil {
				row.FromKind, row.FromFile = node.Kind, node.FilePath
			}
			out = append(out, row)
		}
		frontier = next
	}
	return out, ctx.Err()
}
