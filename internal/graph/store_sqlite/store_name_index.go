package store_sqlite

// sealedGeneration returns the shared seal of generation when it is a
// published, immutable derived generation.
func (s *Store) sealedGeneration(generation int64) (*payloadSeal, bool) {
	if s.coreless() || generation <= baseViewGeneration {
		return nil, false
	}
	seal := s.seal
	if seal == nil || s.viewGen != generation {
		seal = s.payloadSealIfPresent(generation)
	}
	if seal != nil {
		switch seal.state.Load() {
		case payloadSealSealed:
			return seal, true
		case payloadSealUnknown:
		default:
			return nil, false
		}
	}
	// Read-only probe: the write gate owns the cached verdict, so this asks
	// the catalog without storing anything. The seal is minted only for a
	// generation the catalog says is published, so an id from outside the
	// lifecycle never grows the seal map.
	probe := *s
	probe.viewGen = generation
	verdict, _, found, err := probe.catalogSealVerdict()
	if err != nil || !found || verdict != payloadSealSealed {
		return nil, false
	}
	if seal == nil {
		seal = s.payloadSealFor(generation)
	}
	return seal, seal != nil
}
