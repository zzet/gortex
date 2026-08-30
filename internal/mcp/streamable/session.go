// Package streamable implements the MCP 2026 Streamable HTTP transport
// on top of the existing in-process MCP server. The transport is
// stateless from the network's perspective — every request carries a
// `Mcp-Session-Id` header that the server uses to replay the matching
// per-session state out of an in-memory store. Any worker can serve
// any request as long as it has access to the same store; the spec's
// "horizontally scalable behind a load balancer" promise reduces to
// "share the SessionStore across workers".
//
// Two store implementations live alongside the transport:
//
//   - MemoryStore: process-local, TTL-evicted, the default. Safe for
//     single-binary deployments and for tests that bring up several
//     handler instances in one process to prove the per-request
//     replay actually works.
//   - StatelessStore: refuses to mint or persist anything. Selected
//     when the operator runs in pure-stateless mode; every request
//     is treated as a fresh session and the `Mcp-Session-Id` header
//     never round-trips.
//
// External stores (Redis, DynamoDB, …) can implement SessionStore
// without touching this package.
package streamable

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// DefaultSessionIdleTTL is the idle horizon after which the sweeper
// evicts an MCP session. Thirty minutes matches the value the daemon
// has always run with (previously borrowed from the overlay
// subsystem's DefaultOverlayIdleTTL); owning it here makes session
// lifetime an MCP policy with its own configuration.
const DefaultSessionIdleTTL = 30 * time.Minute

// SessionIdleTTLFromEnv resolves the MCP session idle TTL: an explicit
// positive override wins, then GORTEX_MCP_SESSION_IDLE_TTL (any
// time.ParseDuration string), then DefaultSessionIdleTTL. A negative
// value clamps to 0, which disables expiry entirely; garbage env
// values fall back to the default — we deliberately don't fail
// startup over a typo'd duration.
func SessionIdleTTLFromEnv(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if raw := os.Getenv("GORTEX_MCP_SESSION_IDLE_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			return d
		}
	}
	return DefaultSessionIdleTTL
}

// SessionState is everything the transport needs to replay an MCP
// session on a fresh worker. It is captured at `initialize` time and
// updated by subsequent frames that mutate session-scoped state
// (`notifications/initialized`, client-info refreshes, cwd binding).
// The fields are deliberately small — the per-tool savings / feedback
// state lives on the in-process MCP Server and is keyed by SessionID
// there, so replicating the store across nodes does not require
// shipping query state with it.
type SessionState struct {
	ID              string          `json:"id"`
	ProtocolVersion string          `json:"protocol_version"`
	ClientName      string          `json:"client_name,omitempty"`
	ClientVersion   string          `json:"client_version,omitempty"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	InitParams      json.RawMessage `json:"init_params,omitempty"`
	CWD             string          `json:"cwd,omitempty"`
	Workspace       string          `json:"workspace,omitempty"`
	Initialized     bool            `json:"initialized"`
	CreatedAt       time.Time       `json:"created_at"`
	LastUsed        time.Time       `json:"last_used"`
}

// SessionStore is the persistence boundary the transport sees. Any
// implementation that is goroutine-safe and survives long enough for a
// client to come back with the session id will work. The store is
// responsible for eviction policy — the transport never garbage-
// collects on its own.
type SessionStore interface {
	// Create mints a fresh session id, persists the state under it,
	// and returns the id. The state's ID/CreatedAt/LastUsed fields
	// are overwritten with authoritative values before persistence.
	// A stateless store may return ("", nil) to signal "do not
	// expose a session id"; the transport then runs the request in
	// pure-stateless mode (no `Mcp-Session-Id` response header).
	Create(state SessionState) (string, error)

	// Get returns the state stored under id and refreshes its
	// LastUsed timestamp. ok is false when id is unknown or has
	// expired. A stateless store always returns (zero, false).
	Get(id string) (state SessionState, ok bool)

	// Update writes the state back under its existing id. Used when
	// `notifications/initialized` flips the Initialized flag, or
	// when the server learns clientInfo from the initialize frame.
	Update(state SessionState) error

	// Delete removes the session. Used for explicit DELETE /mcp.
	// Idempotent — deleting an unknown id is not an error.
	Delete(id string)

	// Len returns the number of currently live sessions. Exposed
	// for metrics and tests; never used as a correctness signal.
	Len() int
}

// MemoryStore is the default in-process store. Goroutine-safe,
// TTL-evicted via a background sweeper that wakes once per minute.
// The sweeper is started on construction and stopped via Close.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]SessionState
	ttl      time.Duration
	done     chan struct{}
	doneOnce sync.Once
}

// NewMemoryStore returns a MemoryStore with the supplied idle TTL.
// A TTL of zero disables eviction entirely (test-only — production
// callers should always supply a finite TTL). The background sweeper
// is started in a goroutine and runs until Close is invoked.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	s := &MemoryStore{
		sessions: make(map[string]SessionState),
		ttl:      ttl,
		done:     make(chan struct{}),
	}
	if ttl > 0 {
		go s.sweepLoop()
	}
	return s
}

// Close stops the background sweeper. Subsequent calls are no-ops.
// Idempotent so tests that bring up several stores can defer Close
// without worrying about double-close panics.
func (s *MemoryStore) Close() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *MemoryStore) sweepLoop() {
	t := time.NewTicker(s.sweepInterval())
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.sweepOnce(time.Now())
		}
	}
}

// sweepInterval picks a sane wake-up cadence: never more than one
// minute, never less than a tenth of the TTL.
func (s *MemoryStore) sweepInterval() time.Duration {
	step := s.ttl / 10
	if step <= 0 {
		step = time.Minute
	}
	if step > time.Minute {
		step = time.Minute
	}
	return step
}

// SweepNow forces an eviction pass. Exposed for tests that want
// deterministic timing without waiting for the background ticker.
func (s *MemoryStore) SweepNow(now time.Time) int { return s.sweepOnce(now) }

func (s *MemoryStore) sweepOnce(now time.Time) int {
	if s.ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := 0
	for id, st := range s.sessions {
		if st.LastUsed.Before(cutoff) {
			delete(s.sessions, id)
			evicted++
		}
	}
	return evicted
}

// Create implements SessionStore.
func (s *MemoryStore) Create(state SessionState) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	state.ID = id
	state.CreatedAt = now
	state.LastUsed = now
	s.mu.Lock()
	s.sessions[id] = state
	s.mu.Unlock()
	return id, nil
}

// Get implements SessionStore.
func (s *MemoryStore) Get(id string) (SessionState, bool) {
	if id == "" {
		return SessionState{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[id]
	if !ok {
		return SessionState{}, false
	}
	if s.ttl > 0 && now.Sub(st.LastUsed) > s.ttl {
		delete(s.sessions, id)
		return SessionState{}, false
	}
	st.LastUsed = now
	s.sessions[id] = st
	return st, true
}

// Update implements SessionStore. The state's ID must already exist;
// Update is a no-op if it does not (the caller raced eviction).
func (s *MemoryStore) Update(state SessionState) error {
	if state.ID == "" {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[state.ID]; !ok {
		return nil
	}
	state.LastUsed = now
	s.sessions[state.ID] = state
	return nil
}

// Delete implements SessionStore.
func (s *MemoryStore) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// Len implements SessionStore.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// StatelessStore is the null store. Selected when the operator turns
// session persistence off entirely — every request runs as if it were
// the first one the server has ever seen. Useful for serverless /
// function-as-a-service deployments that can't share memory.
type StatelessStore struct{}

func (StatelessStore) Create(SessionState) (string, error) { return "", nil }
func (StatelessStore) Get(string) (SessionState, bool)     { return SessionState{}, false }
func (StatelessStore) Update(SessionState) error           { return nil }
func (StatelessStore) Delete(string)                       {}
func (StatelessStore) Len() int                            { return 0 }

// newSessionID mints a 32-hex-char (128-bit) random id. Cryptographic
// randomness — predictable ids would let one client guess another's
// session id and replay its state.
func newSessionID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
