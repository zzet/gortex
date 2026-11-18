package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func eventProjectionFixture(t *testing.T) (*Server, string, *pendingBudgetExtractor, *projectionEntry) {
	t.Helper()
	selectedProjectionRegistry(t)
	extractor := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, []byte("\nfunc EventA() {}\n")...), 0600))
	entry, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, entry)
	return s, path, extractor, entry
}

func TestObservedSameBytesEventGetsFreshImmutableReadOwner(t *testing.T) {
	s, path, _, old := eventProjectionFixture(t)
	pendingFileProjections.markSuspect(path)
	require.True(t, old.suspect.Load())
	current, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotSame(t, old, current, "same bytes after observed invalidation need a newly checked owner, not an old physical receipt")
	require.Same(t, old.layer, current.layer, "revalidating identical bytes must not parse the file again")
	require.Zero(t, current.commitSeq)
	require.False(t, current.suspect.Load())
	require.True(t, old.suspect.Load(), "never clear an old reader or historical receipt's invalidation")
	require.True(t, liveProjectionForAcknowledgement(current))
	require.False(t, liveProjectionForAcknowledgement(old))
}

func TestSourceEventDuringProjectionCannotBeClearedByOldObservation(t *testing.T) {
	s, path, extractor, old := eventProjectionFixture(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, []byte("\nfunc EventB() {}\n")...), 0600))
	pendingFileProjections.markSuspect(path) // observed before this read
	extractor.hold.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), projectedReadBudget)
	defer cancel()
	var once sync.Once
	release := func() { once.Do(func() { close(extractor.release) }) }
	type reply struct {
		entry *projectionEntry
		err   error
	}
	done := make(chan reply, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		entry, err := s.projectCurrentTrackedFile(ctx, ctx, path, "main.go", "main.go")
		done <- reply{entry, err}
	}()
	t.Cleanup(func() {
		release()
		<-joined
		select {
		case <-extractor.entered:
			<-extractor.finished
		default:
		}
	})
	select {
	case <-extractor.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pendingFileProjections.markSuspect(path) // a second real event during extraction
	release()
	var result reply
	select {
	case result = <-done:
	case <-ctx.Done():
		<-done
		t.Fatal(ctx.Err())
	}
	require.ErrorIs(t, result.err, projectionSuperseded, "an event after bytes were selected cannot be acknowledged by the older observation")
	require.Nil(t, result.entry)
	require.Same(t, old, pendingFileProjections.snap.Load().entries[old.key], "refusal must not publish an unusable replacement")
	require.True(t, old.suspect.Load())
	// A genuinely new bounded read can observe that event and accept current B.
	next, err := s.projectCurrentTrackedFile(t.Context(), t.Context(), path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.False(t, next.suspect.Load())
	require.True(t, liveProjectionForAcknowledgement(next))
}

func TestDeferredOldProjectionInvalidationCannotPoisonNewerPublisher(t *testing.T) {
	s, path, extractor, old := eventProjectionFixture(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(raw, []byte("\nfunc EventB() {}\n")...), 0600))
	extractor.hold.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var once sync.Once
	release := func() { once.Do(func() { close(extractor.release) }) }
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, err := s.projectCurrentTrackedFile(ctx, ctx, path, "main.go", "main.go")
		done <- err
	}()
	t.Cleanup(func() {
		release()
		<-joined
		select {
		case <-extractor.entered:
			<-extractor.finished
		default:
		}
	})
	select {
	case <-extractor.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	newer, err := s.projectCurrentTrackedFile(ctx, ctx, path, "main.go", "main.go")
	require.NoError(t, err)
	require.NotNil(t, newer)
	require.NotSame(t, old, newer)
	release()
	select {
	case err = <-done:
	case <-ctx.Done():
		<-done
		t.Fatal(ctx.Err())
	}
	require.ErrorIs(t, err, projectionSuperseded)
	require.True(t, old.suspect.Load(), "the late failed read may invalidate only its old pointer")
	require.Same(t, newer, pendingFileProjections.snap.Load().entries[newer.key])
	require.False(t, newer.suspect.Load())
	require.True(t, liveProjectionForAcknowledgement(newer))
}
