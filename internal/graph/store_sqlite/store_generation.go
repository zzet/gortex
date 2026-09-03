package store_sqlite

import "sync"

// baseViewGeneration is the generation every store starts on: the single
// corpus written by a plain index. The catalog mints new generations from 1
// upwards, so 0 always names the base payload rows.
const baseViewGeneration int64 = 0

// AtGeneration returns a handle over the same database pinned to payload view
// generation g. The returned handle shares the core — pools, prepared
// statements, caches, write gate — with the receiver, so it sees every write
// any other handle makes; only the pinned generation differs.
//
// The derived handle never owns the core: closing it is a no-op, and the
// handle Open returned stays responsible for teardown.
//
// A negative generation is a programming error and returns nil rather than
// silently falling back to the base corpus.
func (s *Store) AtGeneration(g int64) *Store {
	if g < baseViewGeneration {
		return nil
	}
	if g == baseViewGeneration {
		return s.atBase()
	}
	return &Store{
		storeCore:   s.storeCore,
		viewGen:     g,
		seal:        s.payloadSealFor(g),
		resolveLane: s.resolveLaneFor(g),
	}
}

// resolveLaneFor returns the resolver-coordination mutex shared by every
// handle on generation g. Sharing it by generation rather than by handle is
// what keeps two passes over one payload excluding each other; see
// ResolveMutex for why generations do not exclude one another.
func (s *Store) resolveLaneFor(g int64) *sync.Mutex {
	if s.coreless() || g == baseViewGeneration {
		return nil
	}
	if cached, ok := s.resolveLanes.Load(g); ok {
		return cached.(*sync.Mutex)
	}
	shared, _ := s.resolveLanes.LoadOrStore(g, &sync.Mutex{})
	return shared.(*sync.Mutex)
}

// atBase returns a handle over the same core pinned to the base corpus. The
// control plane uses it: nothing in the catalog is payload, so a catalog write
// must not be refused because the caller happened to hold a published
// generation's handle. It always allocates rather than returning the receiver,
// so the derived handle can never inherit the owning handle's teardown duty.
func (s *Store) atBase() *Store {
	return &Store{storeCore: s.storeCore}
}

// ViewGeneration reports the payload view generation this handle is pinned to.
// The handle Open returned reads the base corpus, generation 0.
func (s *Store) ViewGeneration() int64 { return s.viewGen }
