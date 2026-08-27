package lsp

import (
	"container/list"
	"os"
	"sync"
)

// docSession shares one bounded set of open documents across every phase
// of a single enrichment pass. It builds ONLY on the bare
// enrichOpenDoc / enrichCloseDoc lifecycle (never the shared-state
// openDocument / sourceCache tables) and owns its own per-entry content
// cache, so a file read from disk once serves every phase that touches it
// and no phase writes the provider's unsynchronised sourceCache.
//
// A file is didOpen'd at most once per (client, path) while its entry
// lives; release unpins it and leaves it warm in an LRU tail so a later
// phase reuses the already-open document instead of reopening it. When the
// documents open on a client would exceed cap, acquire first didCloses the
// oldest unpinned entries. Pinned entries are never in the LRU and so never
// close, which means a fully-pinned working set may briefly exceed cap —
// pin volume is bounded externally by fileSem, so the overshoot is small.
type docSession struct {
	p   *Provider
	cap int // simultaneously-open ceiling per client
	// sendOpens gates the server-side lifecycle. When false (the provider's
	// opensDocs resolved off — see ServerSpec.NoDidOpen) the session keeps
	// its entry / LRU machinery purely as a bounded content cache: acquire
	// still reads and caches file bytes, but no didOpen / didClose is ever
	// sent, and the open-lifecycle telemetry (didOpens, curOpen, peakOpen)
	// honestly reports zero. Evictions still count — they track the cache
	// churn either way.
	sendOpens bool

	mu        sync.Mutex
	perClient map[*Client]*clientDocs

	// Telemetry, all guarded by mu.
	didOpens   int
	evictions  int
	curOpen    int
	peakOpen   int
	openCounts map[string]int // absPath → number of didOpens across the pass
}

// clientDocs tracks the documents open on one client. lru holds the
// refcount-0 (unpinned) entries with the oldest at the front, so eviction
// pops the least-recently-released document first. The list element Value
// is the entry's absPath.
type clientDocs struct {
	open map[string]*docEntry
	lru  *list.List
}

// docEntry is one open document. refs counts the live acquire pins; when it
// falls to zero the entry moves onto its client's lru and becomes
// evictable. content is the file's bytes, read from disk once per entry.
type docEntry struct {
	refs    int
	elem    *list.Element
	content []byte
}

// newDocSession builds a session sized to hold 2*maxParallel documents
// open per client (floor 4). The pinned working set is bounded by fileSem
// at maxParallel, so the extra headroom is the warm LRU tail that carries
// recently-released documents across phases.
func newDocSession(p *Provider) *docSession {
	cp := 2 * p.maxParallel
	if cp < 4 {
		cp = 4
	}
	return &docSession{
		p:          p,
		cap:        cp,
		sendOpens:  p.opensDocs,
		perClient:  map[*Client]*clientDocs{},
		openCounts: map[string]int{},
	}
}

// acquire pins absPath open on c, sending its didOpen once per entry
// lifetime and reading its bytes from disk once. The returned release
// unpins the entry; a refcount-0 entry stays open on the server (warm in
// the lru) until it is evicted or closeAll runs. err is non-nil only when
// the disk read or the didOpen notify failed — release is a no-op then.
func (s *docSession) acquire(c *Client, absPath string) ([]byte, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cd := s.perClient[c]
	if cd == nil {
		cd = &clientDocs{open: map[string]*docEntry{}, lru: list.New()}
		s.perClient[c] = cd
	}

	if e, ok := cd.open[absPath]; ok {
		// Already open on this client — pin it, pulling it back off the
		// lru if it was sitting there unpinned.
		if e.refs == 0 && e.elem != nil {
			cd.lru.Remove(e.elem)
			e.elem = nil
		}
		e.refs++
		return e.content, s.releaseFunc(c, absPath), nil
	}

	// Read the file before touching the server so a read error leaves no
	// half-open entry behind.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, func() {}, err
	}

	// Make room: didClose the oldest unpinned entries so this open keeps
	// the client at or under cap. Evicting before the new didOpen keeps the
	// server's open count from spiking above cap. A fully-pinned client has
	// an empty lru and simply overshoots.
	for len(cd.open) >= s.cap {
		front := cd.lru.Front()
		if front == nil {
			break
		}
		evPath := front.Value.(string)
		cd.lru.Remove(front)
		delete(cd.open, evPath)
		// Evictions count the cache churn, not the didClose traffic — a
		// lifecycle-off session evicts entries all the same and hiding that
		// would blind the content-cache telemetry.
		s.evictions++
		if s.sendOpens {
			_ = s.p.enrichCloseDoc(c, evPath)
			s.curOpen--
		}
	}

	if s.sendOpens {
		if err := s.p.enrichOpenDoc(c, absPath, content); err != nil {
			return nil, func() {}, err
		}
		s.didOpens++
		s.openCounts[absPath]++
		s.curOpen++
		if s.curOpen > s.peakOpen {
			s.peakOpen = s.curOpen
		}
	}
	cd.open[absPath] = &docEntry{refs: 1, content: content}
	return content, s.releaseFunc(c, absPath), nil
}

// releaseFunc returns a release closure for one pin on (c, absPath).
// Callers defer it exactly once per successful acquire; a refcount-0 entry
// re-joins the lru so a later acquire can reuse (or evict) it.
func (s *docSession) releaseFunc(c *Client, absPath string) func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		cd := s.perClient[c]
		if cd == nil {
			return
		}
		e := cd.open[absPath]
		if e == nil || e.refs == 0 {
			return
		}
		e.refs--
		if e.refs == 0 {
			e.elem = cd.lru.PushBack(absPath)
		}
	}
}

// closeAll didCloses every still-open document on every client,
// best-effort (a doc opened on a client that later died still gets its
// close attempt, error ignored). Deferred once at the end of the pass.
func (s *docSession) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, cd := range s.perClient {
		if s.sendOpens {
			for path := range cd.open {
				_ = s.p.enrichCloseDoc(c, path)
				s.curOpen--
			}
		}
		cd.open = map[string]*docEntry{}
		cd.lru.Init()
	}
}

// stats returns the pass telemetry: total didOpens, the number of distinct
// paths opened more than once, LRU evictions, and the peak simultaneously
// open documents.
func (s *docSession) stats() (didOpens, reopenedFiles, evictions, peakOpen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reopened := 0
	for _, cnt := range s.openCounts {
		if cnt > 1 {
			reopened++
		}
	}
	return s.didOpens, reopened, s.evictions, s.peakOpen
}
