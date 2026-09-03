package graph

import "context"

// ScanNodeSearchKeys merges compact keys through the same overlay masks as the
// point/name readers: base nodes owned by overlaid files (or explicitly removed
// names) disappear, and the overlay's replacement nodes are appended. Ordering
// is intentionally unspecified; the search consumer performs its own total
// ranking by score, name length, and ID.
func (v *OverlaidView) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]NodeSearchKey) bool) error {
	if v == nil || yield == nil {
		return nil
	}
	ctx = nodeSearchContext(ctx)
	pageSize = nodeSearchPageSize(pageSize)

	page := make([]NodeSearchKey, 0, pageSize)
	stopped := false
	emit := func(key NodeSearchKey) bool {
		page = append(page, key)
		if len(page) < pageSize {
			return true
		}
		if !yield(page) {
			stopped = true
			return false
		}
		clear(page)
		page = page[:0]
		return true
	}

	if v.base != nil {
		err := ScanNodeSearchKeys(ctx, v.base, pageSize, func(keys []NodeSearchKey) bool {
			for _, key := range keys {
				if ctx.Err() != nil {
					return false
				}
				if v.layer != nil {
					if v.nodeBelongsToOverlay(key.ID) {
						continue
					}
					if v.layer.IsNameRemoved(key.Name, key.ID) {
						continue
					}
				}
				if !emit(key) {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}

	if v.layer != nil {
		for node := range v.layer.Nodes() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if node == nil {
				continue
			}
			if !emit(NodeSearchKey{ID: node.ID, Kind: node.Kind, Name: node.Name}) {
				return nil
			}
		}
	}

	if len(page) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		yield(page)
	}
	return nil
}

// SearchMutationRevision forwards the base reader's optional invalidation
// token without pretending that an unsupported base has an authoritative zero
// revision. Overlay layers are immutable after view construction, so only base
// mutations can invalidate a scan/hydration pair.
func (v *OverlaidView) SearchMutationRevision() (uint64, bool) {
	if v == nil || v.base == nil {
		return 0, false
	}
	if revisioner, ok := v.base.(interface{ MutationRevision() uint64 }); ok {
		return revisioner.MutationRevision(), true
	}
	if revisioner, ok := v.base.(interface{ SearchMutationRevision() (uint64, bool) }); ok {
		return revisioner.SearchMutationRevision()
	}
	return 0, false
}

var _ NodeSearchKeyScanner = (*OverlaidView)(nil)
