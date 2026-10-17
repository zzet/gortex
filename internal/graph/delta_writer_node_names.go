package graph

// NodeNamesByIDs is GetNodesByIDs reduced to names, composed through the
// stack: an identity some layer speaks for (it covers the identity's file,
// carries it or removed it) is answered by the topmost such layer — absent
// when that layer hid it — and every other identity by the store at the
// bottom in one name-only read, without decoding a node row. A bottom store
// with no name-only read is answered from its full rows.
func (dw *DeltaWriter) NodeNamesByIDs(ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	s := dw.stack()
	seen := make(map[string]struct{}, len(ids))
	var below []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if n, owned := s.topNode(id); owned {
			if n != nil {
				out[id] = n.Name
			}
			continue
		}
		below = append(below, id)
	}
	if len(below) == 0 || s.base == nil {
		return out
	}
	if names, ok := s.base.(interface {
		NodeNamesByIDs([]string) map[string]string
	}); ok {
		for id, name := range names.NodeNamesByIDs(below) {
			out[id] = name
		}
		return out
	}
	for id, n := range s.base.GetNodesByIDs(below) {
		if n != nil {
			out[id] = n.Name
		}
	}
	return out
}
