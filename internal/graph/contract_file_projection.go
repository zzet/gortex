package graph

import (
	"context"
	"errors"
	"sort"
)

// ContractProjectionRowLimit bounds an observed file or ID group. Exceeding
// the bound refuses the optimization rather than returning a partial snapshot.
const ContractProjectionRowLimit = 65536

var (
	ErrContractProjectionUnsupported = errors.New("complete contract projection unsupported")
	ErrContractProjectionLimit       = errors.New("contract projection row limit exceeded")
	ErrContractProjectionIncomplete  = errors.New("contract projection owner source missing")
	ErrContractProjectionStale       = errors.New("contract layer ownership revision changed")
)

// ContractFileProjection contains complete selected rows, not a full registry.
// OwnerRows includes same-ID siblings across repositories and handles_route
// evidence needed to determine legacy scalar liveness. OffFileOwnerRows are
// ownership rows originating in selected files but recorded elsewhere.
// Physical reads can span transactions. Callers must capture a selected-view
// witness before reading and validate it afterward and before publication.
// In-memory node/edge rows follow ordinary Reader ownership: callers must not
// mutate them. This projection does not itself certify ancestor freshness.
type ContractFileProjection struct {
	FileNodes        map[string][]*Node
	ScalarNodes      []*Node
	Targets          map[string]*Node
	OwnerRows        []RepoEdgeRow
	OffFileOwnerRows []RepoEdgeRow
	// SourceNodes and OutgoingOwnerRows carry checked composition evidence;
	// Targets contains only canonical contract nodes.
	SourceNodes       map[string]*Node
	OutgoingOwnerRows []RepoEdgeRow
}

// ContractFileProjectionReader exposes checked, bounded file and canonical-ID
// projections. Errors never return usable partial rows. A generic/errorless
// reader cannot provide evidence that a file's prior contracts were empty.
type ContractFileProjectionReader interface {
	LoadContractFileProjectionContext(context.Context, string, []string) (ContractFileProjection, error)
	LoadContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error)
}

type OverlayLayerContractProjectionReader interface {
	LayerContractFileProjectionContext(context.Context, string, []string) (ContractFileProjection, error)
	LayerContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error)
}

// FilterContractFileProjection applies a generation's existing context/mask
// serving rules without routing through errorless point readers.
func FilterContractFileProjection(p ContractFileProjection, nodeVisible func(*Node) bool, edgeVisible func(*Edge) bool) ContractFileProjection {
	out := ContractFileProjection{FileNodes: make(map[string][]*Node), Targets: make(map[string]*Node), SourceNodes: make(map[string]*Node)}
	for path, nodes := range p.FileNodes {
		for _, node := range nodes {
			if nodeVisible(node) {
				out.FileNodes[path] = append(out.FileNodes[path], node)
			}
		}
	}
	for _, node := range p.ScalarNodes {
		if nodeVisible(node) {
			out.ScalarNodes = append(out.ScalarNodes, node)
		}
	}
	for id, node := range p.Targets {
		if nodeVisible(node) {
			out.Targets[id] = node
		}
	}
	for id, node := range p.SourceNodes {
		if nodeVisible(node) {
			out.SourceNodes[id] = node
		}
	}
	for _, row := range p.OutgoingOwnerRows {
		if edgeVisible(row.Edge) {
			out.OutgoingOwnerRows = append(out.OutgoingOwnerRows, row)
		}
	}
	for _, row := range p.OwnerRows {
		if edgeVisible(row.Edge) {
			out.OwnerRows = append(out.OwnerRows, row)
		}
	}
	for _, row := range p.OffFileOwnerRows {
		if edgeVisible(row.Edge) {
			out.OffFileOwnerRows = append(out.OffFileOwnerRows, row)
		}
	}
	return out
}

func mergeContractProjection(base, layer ContractFileProjection) ContractFileProjection {
	if base.FileNodes == nil {
		base.FileNodes = make(map[string][]*Node)
	}
	if base.Targets == nil {
		base.Targets = make(map[string]*Node)
	}
	if base.SourceNodes == nil {
		base.SourceNodes = make(map[string]*Node)
	}
	for id, node := range layer.SourceNodes {
		base.SourceNodes[id] = node
	}
	base.OutgoingOwnerRows = append(base.OutgoingOwnerRows, layer.OutgoingOwnerRows...)
	for path, nodes := range layer.FileNodes {
		base.FileNodes[path] = append(base.FileNodes[path], nodes...)
	}
	for id, node := range layer.Targets {
		base.Targets[id] = node
	}
	base.ScalarNodes = append(base.ScalarNodes, layer.ScalarNodes...)
	base.OwnerRows = append(base.OwnerRows, layer.OwnerRows...)
	base.OffFileOwnerRows = append(base.OffFileOwnerRows, layer.OffFileOwnerRows...)
	return base
}

func contractProjectionIDs(p ContractFileProjection) []string {
	seen := make(map[string]bool)
	for _, node := range p.ScalarNodes {
		seen[node.ID] = true
	}
	for _, row := range p.OwnerRows {
		if row.Edge != nil && row.Edge.To != "" {
			seen[row.Edge.To] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (v *OverlaidView) contractProjection(ctx context.Context, repo string, paths, ids []string, byFile bool) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	var base, own ContractFileProjection
	var err error
	if v.base != nil {
		base, err = rawContractProjection(ctx, v.base, repo, paths, ids, byFile)
		if err != nil {
			return ContractFileProjection{}, err
		}
	}
	if v.layer != nil {
		reader, ok := v.layer.(OverlayLayerContractProjectionReader)
		if !ok {
			return ContractFileProjection{}, ErrContractProjectionUnsupported
		}
		if byFile {
			own, err = reader.LayerContractFileProjectionContext(ctx, repo, paths)
		} else {
			own, err = reader.LayerContractIDProjectionContext(ctx, ids)
		}
		if err != nil {
			return ContractFileProjection{}, err
		}
	}
	// Ownership probes use mask/identity claims; endpoint presence must come
	// from checked layer rows, never its errorless NodeByID reader.
	checked := own.SourceNodes
	var endpointIDs []string
	for _, rows := range [][]RepoEdgeRow{base.OwnerRows, base.OffFileOwnerRows, base.OutgoingOwnerRows} {
		for _, row := range rows {
			for _, id := range []string{row.Edge.From, row.Edge.To} {
				if v.overlayOwnsIdentity(id) {
					endpointIDs = append(endpointIDs, id)
				}
			}
		}
	}
	if len(endpointIDs) > 0 {
		evidence, e := v.layer.(OverlayLayerContractProjectionReader).LayerContractIDProjectionContext(ctx, endpointIDs)
		if e != nil {
			return ContractFileProjection{}, e
		}
		checked = evidence.SourceNodes
	}
	edgeVisible := func(e *Edge) bool {
		if e == nil || v.overlayOwnsBaseEdge(e.From, e.FilePath) || v.overlayClaimsBaseEdge(e) {
			return false
		}
		for _, id := range []string{e.From, e.To} {
			if v.overlayOwnsIdentity(id) && checked[id] == nil {
				return false
			}
		}
		return true
	}
	base = FilterContractFileProjection(base, v.baseNodeVisible, edgeVisible)
	out := mergeContractProjection(base, own)
	if !contractProjectionWithinLimit(out) {
		return ContractFileProjection{}, ErrContractProjectionLimit
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	return out, nil
}

// rawContractProjection deliberately avoids public group hydration at each
// recursive level: every physical layer is read once per bounded pass.
func rawContractProjection(ctx context.Context, r Reader, repo string, paths, ids []string, byFile bool) (ContractFileProjection, error) {
	switch v := r.(type) {
	case *OverlaidView:
		return v.contractProjection(ctx, repo, paths, ids, byFile)
	case *DeltaWriter:
		return v.view.contractProjection(ctx, repo, paths, ids, byFile)
	}
	reader, ok := r.(OverlayLayerContractProjectionReader)
	if !ok {
		if wrapper, yes := r.(Unwrapper); yes {
			if next := wrapper.Unwrap(); next != nil {
				return rawContractProjection(ctx, next, repo, paths, ids, byFile)
			}
		}
		return ContractFileProjection{}, ErrContractProjectionUnsupported
	}
	if byFile {
		return reader.LayerContractFileProjectionContext(ctx, repo, paths)
	}
	return reader.LayerContractIDProjectionContext(ctx, ids)
}

// CompleteContractFileProjection hydrates canonical ID groups only after the
// selected file rows have been composed, then checks their source identities.
func CompleteContractFileProjection(ctx context.Context, r Reader, repo string, paths []string) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	local, err := rawContractProjection(ctx, r, repo, paths, nil, true)
	if err != nil {
		return ContractFileProjection{}, err
	}
	groups, err := CompleteContractIDProjection(ctx, r, contractProjectionIDs(local))
	if err != nil {
		return ContractFileProjection{}, err
	}
	local.OwnerRows, local.Targets, local.SourceNodes = groups.OwnerRows, groups.Targets, groups.SourceNodes
	// Recorded ownership can live above a source node without re-emitting it.
	// Read outgoing ownership from every physical layer using composed file IDs.
	var sources []string
	pathSet := make(map[string]bool, len(paths))
	for _, path := range paths {
		pathSet[path] = true
	}
	for _, nodes := range local.FileNodes {
		for _, n := range nodes {
			sources = append(sources, n.ID)
		}
	}
	outgoing, err := rawContractProjection(ctx, r, "", nil, sources, false)
	if err != nil {
		return ContractFileProjection{}, err
	}
	for _, row := range outgoing.OutgoingOwnerRows {
		if source := outgoing.SourceNodes[row.Edge.From]; source != nil && !pathSet[row.Edge.FilePath] {
			row.RepoPrefix = source.RepoPrefix
			local.OffFileOwnerRows = append(local.OffFileOwnerRows, row)
		}
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	if !contractProjectionWithinLimit(local) {
		return ContractFileProjection{}, ErrContractProjectionLimit
	}
	return local, nil
}

func CompleteContractIDProjection(ctx context.Context, r Reader, ids []string) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := rawContractProjection(ctx, r, "", nil, ids, false)
	if err != nil {
		return ContractFileProjection{}, err
	}
	var sourceIDs []string
	for _, row := range p.OwnerRows {
		sourceIDs = append(sourceIDs, row.Edge.From)
	}
	sources, err := rawContractProjection(ctx, r, "", nil, sourceIDs, false)
	if err != nil {
		return ContractFileProjection{}, err
	}
	p.SourceNodes = sources.SourceNodes
	visible := p.OwnerRows[:0]
	for _, row := range p.OwnerRows {
		source := p.SourceNodes[row.Edge.From]
		if source == nil {
			return ContractFileProjection{}, ErrContractProjectionIncomplete
		}
		row.RepoPrefix = source.RepoPrefix
		visible = append(visible, row)
	}
	p.OwnerRows = visible
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	if !contractProjectionWithinLimit(p) {
		return ContractFileProjection{}, ErrContractProjectionLimit
	}
	return p, nil
}

func (v *OverlaidView) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (ContractFileProjection, error) {
	return CompleteContractFileProjection(ctx, v, repo, paths)
}
func (v *OverlaidView) LoadContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return CompleteContractIDProjection(ctx, v, ids)
}

func (dw *DeltaWriter) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (ContractFileProjection, error) {
	return dw.view.LoadContractFileProjectionContext(ctx, repo, paths)
}

func (dw *DeltaWriter) LoadContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return dw.view.LoadContractIDProjectionContext(ctx, ids)
}

func (l *deltaLayer) LayerContractFileProjectionContext(ctx context.Context, repo string, paths []string) (ContractFileProjection, error) {
	return l.work.LayerContractFileProjectionContext(ctx, repo, paths)
}

func (l *deltaLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return l.work.LayerContractIDProjectionContext(ctx, ids)
}

func contractProjectionWithinLimit(p ContractFileProjection) bool {
	count := len(p.ScalarNodes) + len(p.Targets) + len(p.SourceNodes) + len(p.OwnerRows) + len(p.OffFileOwnerRows) + len(p.OutgoingOwnerRows)
	for _, nodes := range p.FileNodes {
		count += len(nodes)
	}
	return count <= ContractProjectionRowLimit
}
