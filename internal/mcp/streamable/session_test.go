package streamable

import (
	"sync"
	"testing"
	"time"
)

func TestMemoryStore_CreateAndGet(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer s.Close()

	id, err := s.Create(SessionState{ClientName: "claude"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	got, ok := s.Get(id)
	if !ok {
		t.Fatalf("Get(%q): not found", id)
	}
	if got.ClientName != "claude" {
		t.Errorf("ClientName = %q, want claude", got.ClientName)
	}
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.CreatedAt.IsZero() || got.LastUsed.IsZero() {
		t.Error("timestamps not set on create")
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
}

func TestMemoryStore_GetUnknownReturnsNotOK(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer s.Close()
	if _, ok := s.Get("nonexistent"); ok {
		t.Error("Get of unknown id returned ok=true")
	}
	if _, ok := s.Get(""); ok {
		t.Error("Get(\"\") returned ok=true")
	}
}

func TestMemoryStore_UpdatePersists(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer s.Close()
	id, _ := s.Create(SessionState{})
	got, _ := s.Get(id)
	got.ClientName = "cursor"
	got.Initialized = true
	if err := s.Update(got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	back, ok := s.Get(id)
	if !ok || back.ClientName != "cursor" || !back.Initialized {
		t.Errorf("Update didn't persist: %+v", back)
	}
}

func TestMemoryStore_UpdateUnknownIsNoop(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer s.Close()
	if err := s.Update(SessionState{ID: "never_seen"}); err != nil {
		t.Errorf("Update of unknown id returned err: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Update of unknown id added entry: Len() = %d", s.Len())
	}
}

func TestMemoryStore_DeleteIsIdempotent(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer s.Close()
	id, _ := s.Create(SessionState{})
	s.Delete(id)
	s.Delete(id)
	s.Delete("never_seen")
	s.Delete("")
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

func TestMemoryStore_TTLEviction(t *testing.T) {
	s := NewMemoryStore(50 * time.Millisecond)
	defer s.Close()
	id, _ := s.Create(SessionState{})
	// Sleep past the TTL so the next Get falls into the eviction path.
	time.Sleep(80 * time.Millisecond)
	if _, ok := s.Get(id); ok {
		t.Error("Get returned an expired session")
	}
}

func TestMemoryStore_SweepNowEvictsStale(t *testing.T) {
	s := NewMemoryStore(50 * time.Millisecond)
	defer s.Close()
	id1, _ := s.Create(SessionState{})
	id2, _ := s.Create(SessionState{})
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	// Force every session to look stale by sweeping with a far-future "now".
	evicted := s.SweepNow(time.Now().Add(1 * time.Hour))
	if evicted != 2 {
		t.Errorf("SweepNow evicted = %d, want 2", evicted)
	}
	if _, ok := s.Get(id1); ok {
		t.Error("id1 should have been evicted")
	}
	if _, ok := s.Get(id2); ok {
		t.Error("id2 should have been evicted")
	}
}

func TestMemoryStore_GetRefreshesLastUsed(t *testing.T) {
	s := NewMemoryStore(time.Hour)
	defer s.Close()
	id, _ := s.Create(SessionState{})
	first, _ := s.Get(id)
	time.Sleep(10 * time.Millisecond)
	second, _ := s.Get(id)
	if !second.LastUsed.After(first.LastUsed) {
		t.Errorf("LastUsed didn't advance: first=%v second=%v", first.LastUsed, second.LastUsed)
	}
}

func TestMemoryStore_ConcurrentCreate(t *testing.T) {
	s := NewMemoryStore(time.Hour)
	defer s.Close()
	const N = 100
	var wg sync.WaitGroup
	ids := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Create(SessionState{})
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, N)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate id: %s", id)
		}
		seen[id] = true
	}
	if s.Len() != N {
		t.Errorf("Len() = %d, want %d", s.Len(), N)
	}
}

func TestMemoryStore_CloseIsIdempotent(t *testing.T) {
	s := NewMemoryStore(time.Hour)
	s.Close()
	s.Close() // would panic on a double-close channel without the once gate.
}

func TestStatelessStore_AllOpsAreNoops(t *testing.T) {
	s := StatelessStore{}
	id, err := s.Create(SessionState{ClientName: "ignored"})
	if err != nil || id != "" {
		t.Errorf("Create returned (%q, %v), want (\"\", nil)", id, err)
	}
	if _, ok := s.Get("anything"); ok {
		t.Error("Get returned ok=true")
	}
	if err := s.Update(SessionState{ID: "x"}); err != nil {
		t.Errorf("Update: %v", err)
	}
	s.Delete("y")
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

func TestNewSessionID_IsRandomAndHex(t *testing.T) {
	a, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	b, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	if a == b {
		t.Errorf("got two identical ids: %q", a)
	}
	if len(a) != 32 {
		t.Errorf("len(id) = %d, want 32", len(a))
	}
	for _, c := range a {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("id contains non-hex byte %q", c)
		}
	}
}

func TestSessionIdleTTLFromEnv(t *testing.T) {
	t.Run("default when nothing is set", func(t *testing.T) {
		t.Setenv("GORTEX_MCP_SESSION_IDLE_TTL", "")
		if got := SessionIdleTTLFromEnv(0); got != DefaultSessionIdleTTL {
			t.Errorf("got %v, want %v", got, DefaultSessionIdleTTL)
		}
	})
	t.Run("explicit override wins over env", func(t *testing.T) {
		t.Setenv("GORTEX_MCP_SESSION_IDLE_TTL", "5m")
		if got := SessionIdleTTLFromEnv(time.Hour); got != time.Hour {
			t.Errorf("got %v, want %v", got, time.Hour)
		}
	})
	t.Run("env is honored", func(t *testing.T) {
		t.Setenv("GORTEX_MCP_SESSION_IDLE_TTL", "45m")
		if got := SessionIdleTTLFromEnv(0); got != 45*time.Minute {
			t.Errorf("got %v, want %v", got, 45*time.Minute)
		}
	})
	t.Run("negative env clamps to no-expiry", func(t *testing.T) {
		t.Setenv("GORTEX_MCP_SESSION_IDLE_TTL", "-1m")
		if got := SessionIdleTTLFromEnv(0); got != 0 {
			t.Errorf("got %v, want 0 (no expiry)", got)
		}
	})
	t.Run("garbage env falls back to the default", func(t *testing.T) {
		t.Setenv("GORTEX_MCP_SESSION_IDLE_TTL", "not-a-duration")
		if got := SessionIdleTTLFromEnv(0); got != DefaultSessionIdleTTL {
			t.Errorf("got %v, want %v", got, DefaultSessionIdleTTL)
		}
	})
}
