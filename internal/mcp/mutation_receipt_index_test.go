package mcp

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommitLedgerReverseIndexEviction(t *testing.T) {
	newRecord := func(s *Server, id string) *mutationCommitRecord {
		return &mutationCommitRecord{owner: s, id: id, startedAt: time.Now()}
	}
	bind := func(r *mutationCommitRecord, receipt string) {
		r.recordGraph(mutationReindexOutcome{Pending: true, Receipt: receipt})
	}
	assertConsistent := func(t *testing.T, s *Server) {
		t.Helper()
		l := &s.mutationCommits
		l.mu.Lock()
		defer l.mu.Unlock()
		for receipt, record := range l.byReindexReceipt {
			require.Same(t, record, l.byID[record.id])
			record.mu.RLock()
			bound := record.reindexReceipt
			record.mu.RUnlock()
			require.Equal(t, receipt, bound)
		}
		for _, record := range l.byID {
			record.mu.RLock()
			receipt := record.reindexReceipt
			record.mu.RUnlock()
			if receipt != "" {
				require.Same(t, record, l.byReindexReceipt[receipt])
			}
		}
	}

	t.Run("put races binding and rebind drops old key", func(t *testing.T) {
		s := &Server{}
		var wg sync.WaitGroup
		for i := range 100 {
			record := newRecord(s, fmt.Sprintf("commit-%d", i))
			wg.Add(2)
			go func() { defer wg.Done(); s.mutationCommits.put(record) }()
			go func() {
				defer wg.Done()
				bind(record, "old-"+record.id)
				bind(record, "new-"+record.id)
			}()
		}
		wg.Wait()
		assertConsistent(t, s)
		require.Len(t, s.mutationCommits.byReindexReceipt, 100)
		for i := range 100 {
			_, ok := s.mutationCommits.byFreshnessReceipt(fmt.Sprintf("old-commit-%d", i))
			require.False(t, ok)
		}
	})

	t.Run("expiry races rebind without resurrecting entries", func(t *testing.T) {
		s := &Server{}
		record := newRecord(s, "commit-expired")
		record.startedAt = time.Now().Add(-mutationCommitRetention - time.Second)
		s.mutationCommits.put(record)
		bind(record, "mutation-old")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); bind(record, "mutation-new") }()
		go func() {
			defer wg.Done()
			s.mutationCommits.mu.Lock()
			defer s.mutationCommits.mu.Unlock()
			s.mutationCommits.evictLocked(time.Now())
		}()
		wg.Wait()
		require.Empty(t, s.mutationCommits.byID)
		require.Empty(t, s.mutationCommits.byReindexReceipt)
		assertConsistent(t, s)
	})

	t.Run("cap races rebind without retaining evicted record", func(t *testing.T) {
		s := &Server{}
		var oldest *mutationCommitRecord
		for i := range maxMutationCommits {
			record := newRecord(s, fmt.Sprintf("commit-%d", i))
			s.mutationCommits.put(record)
			bind(record, fmt.Sprintf("mutation-%d", i))
			if i == 0 {
				oldest = record
			}
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); bind(oldest, "mutation-rebound") }()
		go func() {
			defer wg.Done()
			record := newRecord(s, "commit-over-cap")
			bind(record, "mutation-over-cap") // binding may also precede put
			s.mutationCommits.put(record)
		}()
		wg.Wait()
		require.Len(t, s.mutationCommits.byReindexReceipt, maxMutationCommits)
		for _, id := range []string{"mutation-0", "mutation-rebound"} {
			_, ok := s.mutationCommits.byFreshnessReceipt(id)
			require.False(t, ok)
		}
		assertConsistent(t, s)
	})

	t.Run("receipt collision survives rebind and eviction of old owner", func(t *testing.T) {
		for _, removal := range []string{"rebind", "expiry", "cap"} {
			s := &Server{}
			old := newRecord(s, "commit-old")
			s.mutationCommits.put(old)
			bind(old, "mutation-shared")
			newer := newRecord(s, "commit-new")
			bind(newer, "mutation-shared")
			s.mutationCommits.put(newer)
			switch removal {
			case "expiry":
				old.mu.Lock()
				old.startedAt = time.Now().Add(-mutationCommitRetention - time.Second)
				old.mu.Unlock()
				s.mutationCommits.mu.Lock()
				s.mutationCommits.evictLocked(time.Now())
				s.mutationCommits.mu.Unlock()
			case "cap":
				for i := range maxMutationCommits - 1 {
					s.mutationCommits.put(newRecord(s, fmt.Sprintf("commit-fill-%d", i)))
				}
			}
			// A late completion/rebind must not remove the new owner's key.
			bind(old, "mutation-old-moved")
			got, ok := s.mutationCommits.byFreshnessReceipt("mutation-shared")
			require.True(t, ok)
			require.Same(t, newer, got)
			if removal != "rebind" {
				require.Len(t, s.mutationCommits.byReindexReceipt, 1)
			}
		}
	})
}
