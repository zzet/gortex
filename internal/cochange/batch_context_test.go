package cochange

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A real held SQLite writer gate distinguishes context-capable dispatch from
// the legacy checked API, which cannot return until that gate is released.
func TestAddEdgesContextCancelsActualSQLiteAdmission(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cochange.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go", Kind: graph.KindFile, FilePath: "repo/b.go", RepoPrefix: "repo"},
	}, nil))
	release, err := s.HoldWriteGate(t.Context())
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	t.Cleanup(unlock)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	type outcome struct {
		added int
		err   error
	}
	done := make(chan outcome, 1)
	pairs := []Pair{{FileA: "a.go", FileB: "b.go", Count: 2, Score: 1}}
	go func() { n, err := AddEdgesContext(ctx, s, pairs, "repo"); done <- outcome{n, err} }()
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.Zero(t, got.added, "unadmitted edges counted as completed")
	case <-time.After(3 * time.Second):
		unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("legacy writer fixture did not join after releasing its owned gate")
		}
		t.Fatal("cochange called the uninterruptible checked writer instead of the context-capable API")
	}
	require.Zero(t, s.EdgeCount())
	unlock()
	n, err := AddEdgesContext(t.Context(), s, pairs, "repo")
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, 2, s.EdgeCount())
}
