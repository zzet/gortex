package graph

import "context"

// IncomingSourceCandidate retains the source and actual edge-file provenance.
// It is not a node-row projection and does not imply whole-source ownership.
type IncomingSourceCandidate struct {
	From     string
	FilePath string
}

// BoundedIncomingSourceCandidateReader returns complete, checked raw candidate
// batches, never partial success. Storage cursors must be closed before return.
// Rows are not deduplicated before an overlay applies edge ownership.
type BoundedIncomingSourceCandidateReader interface {
	ReadIncomingSourceCandidates(context.Context, []string, EdgeKind) (map[string][]IncomingSourceCandidate, error)
}

const MaxIncomingSourceCandidateRows = MaxBoundedAdjacencyInspectedEdges

var (
	_ BoundedIncomingSourceCandidateReader = (*Graph)(nil)
	_ BoundedIncomingSourceCandidateReader = (*OverlaidView)(nil)
	_ BoundedIncomingSourceCandidateReader = (*OverlayLayer)(nil)
)

func validateIncomingSourceCandidateKeys(ids []string) error {
	if len(ids) > MaxBoundedAdjacencyKeys {
		return &BoundedLocalizationLimitError{Resource: "incoming-source candidate targets", Limit: MaxBoundedAdjacencyKeys}
	}
	return nil
}

func checkIncomingSourceCandidateInspection(inspected int) error {
	if inspected > MaxIncomingSourceCandidateRows {
		return &BoundedLocalizationLimitError{Resource: "incoming-source candidate inspections", Limit: MaxIncomingSourceCandidateRows}
	}
	return nil
}

func (g *Graph) ReadIncomingSourceCandidates(ctx context.Context, targetIDs []string, kind EdgeKind) (map[string][]IncomingSourceCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := boundedIncomingTargetIDs(targetIDs)
	if err := validateIncomingSourceCandidateKeys(ids); err != nil {
		return nil, err
	}
	out := make(map[string][]IncomingSourceCandidate)
	if g == nil {
		return out, nil
	}
	inspected := 0
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		shard := g.shardFor(target)
		shard.mu.RLock()
		for _, edge := range shard.inEdges[target] {
			inspected++
			if err := checkIncomingSourceCandidateInspection(inspected); err != nil {
				shard.mu.RUnlock()
				return nil, err
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return nil, err
				}
			}
			if edge == nil || edge.Kind != kind || edge.From == "" {
				continue
			}
			out[target] = append(out[target], IncomingSourceCandidate{From: edge.From, FilePath: edge.FilePath})
		}
		shard.mu.RUnlock()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *OverlaidView) ReadIncomingSourceCandidates(ctx context.Context, targetIDs []string, kind EdgeKind) (map[string][]IncomingSourceCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := boundedIncomingTargetIDs(targetIDs)
	if err := validateIncomingSourceCandidateKeys(ids); err != nil {
		return nil, err
	}
	out := make(map[string][]IncomingSourceCandidate)
	if v == nil {
		return out, nil
	}
	var lower map[string][]IncomingSourceCandidate
	if v.base != nil {
		reader, ok := v.base.(BoundedIncomingSourceCandidateReader)
		if !ok {
			return nil, ErrBoundedLocalizationUnavailable
		}
		var err error
		lower, err = reader.ReadIncomingSourceCandidates(ctx, ids, kind)
		if err != nil {
			return nil, err
		}
	}
	if v.layer == nil {
		return lower, nil
	}
	localReader, ok := v.layer.(BoundedIncomingSourceCandidateReader)
	if !ok {
		return nil, ErrBoundedLocalizationUnavailable
	}
	local, err := localReader.ReadIncomingSourceCandidates(ctx, ids, kind)
	if err != nil {
		return nil, err
	}
	// Lower cursors and Graph locks are closed before these ownership probes.
	// Some layer predicates may lazily read node existence from their Store.
	inspected := 0
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !v.overlayIdentityVisible(target) {
			continue
		}
		for _, row := range lower[target] {
			inspected++
			if err := checkIncomingSourceCandidateInspection(inspected); err != nil {
				return nil, err
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if row.From == "" || v.overlayOwnsBaseEdge(row.From, row.FilePath) || !v.overlayIdentityVisible(row.From) {
				continue
			}
			out[target] = append(out[target], row)
		}
		// Each raw physical input was bounded and its cursor closed above.
		// Recharge candidate merging without hydrating adjacency payloads.
		for _, row := range local[target] {
			inspected++
			if err := checkIncomingSourceCandidateInspection(inspected); err != nil {
				return nil, err
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if row.From == "" || !v.overlayIdentityVisible(row.From) {
				continue
			}
			out[target] = append(out[target], row)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// OverlayLayer already owns its in-memory adjacency. Persisted layers must
// implement the checked interface and may not fall back to full InEdges rows.
func (l *OverlayLayer) ReadIncomingSourceCandidates(ctx context.Context, targetIDs []string, kind EdgeKind) (map[string][]IncomingSourceCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := boundedIncomingTargetIDs(targetIDs)
	if err := validateIncomingSourceCandidateKeys(ids); err != nil {
		return nil, err
	}
	out := make(map[string][]IncomingSourceCandidate)
	if l == nil {
		return out, nil
	}
	inspected := 0
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, edge := range l.InEdges(target) {
			inspected++
			if err := checkIncomingSourceCandidateInspection(inspected); err != nil {
				return nil, err
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if edge == nil || edge.Kind != kind || edge.From == "" {
				continue
			}
			out[target] = append(out[target], IncomingSourceCandidate{From: edge.From, FilePath: edge.FilePath})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
