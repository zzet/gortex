package graph

import "context"

// ContractRepoProjectionReader reads an exact namespace, including the empty
// standalone namespace. The result includes checked same-ID ownership evidence.
// Callers must validate their selected-view witness before accepting the result.
type ContractRepoProjectionReader interface {
	LoadContractRepoProjectionContext(context.Context, string) (ContractFileProjection, error)
}

// OverlayLayerContractRepoProjectionReader reads physical repo owner/scalar seeds.
// It does not certify composed selected-view liveness on its own.
type OverlayLayerContractRepoProjectionReader interface {
	LayerContractRepoProjectionContext(context.Context, string) (ContractFileProjection, error)
}

func (v *OverlaidView) contractRepoProjection(ctx context.Context, repo string) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	var base, own ContractFileProjection
	var err error
	if v.base != nil {
		base, err = rawContractRepoProjection(ctx, v.base, repo)
		if err != nil {
			return ContractFileProjection{}, err
		}
	}
	if v.layer != nil {
		reader, ok := v.layer.(OverlayLayerContractRepoProjectionReader)
		if !ok {
			return ContractFileProjection{}, ErrContractProjectionUnsupported
		}
		own, err = reader.LayerContractRepoProjectionContext(ctx, repo)
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
		reader, ok := v.layer.(OverlayLayerContractProjectionReader)
		if !ok {
			return ContractFileProjection{}, ErrContractProjectionUnsupported
		}
		evidence, e := reader.LayerContractIDProjectionContext(ctx, endpointIDs)
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

func rawContractRepoProjection(ctx context.Context, r Reader, repo string) (ContractFileProjection, error) {
	switch v := r.(type) {
	case *OverlaidView:
		return v.contractRepoProjection(ctx, repo)
	case *DeltaWriter:
		return v.view.contractRepoProjection(ctx, repo)
	case Unwrapper:
		if next := v.Unwrap(); next != nil {
			return rawContractRepoProjection(ctx, next, repo)
		}
	}
	reader, ok := r.(OverlayLayerContractRepoProjectionReader)
	if !ok {
		return ContractFileProjection{}, ErrContractProjectionUnsupported
	}
	return reader.LayerContractRepoProjectionContext(ctx, repo)
}

// CompleteContractRepoProjection uses indexed repo seeds, then completes only
// their canonical groups; it never enumerates other repositories' node sets.
func CompleteContractRepoProjection(ctx context.Context, r Reader, repo string) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	local, err := rawContractRepoProjection(ctx, r, repo)
	if err != nil {
		return ContractFileProjection{}, err
	}
	groups, err := CompleteContractIDProjection(ctx, r, contractProjectionIDs(local))
	if err != nil {
		return ContractFileProjection{}, err
	}
	local.OwnerRows, local.Targets, local.SourceNodes = groups.OwnerRows, groups.Targets, groups.SourceNodes
	if !contractProjectionWithinLimit(local) {
		return ContractFileProjection{}, ErrContractProjectionLimit
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	return local, nil
}
func (v *OverlaidView) LoadContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	return CompleteContractRepoProjection(ctx, v, repo)
}
func (dw *DeltaWriter) LoadContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	return dw.view.LoadContractRepoProjectionContext(ctx, repo)
}
func (l *deltaLayer) LayerContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	return l.work.LayerContractRepoProjectionContext(ctx, repo)
}
