package gitstate

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

// An urgent sample (an edit's) does not wait for a background sample holding
// the lease to finish: the background sample gives the lease up at its next
// git boundary, the urgent one runs to its end, and the background one then
// completes too.
func TestUrgentSampleOvertakesABackgroundSampleAtItsNextGitBoundary(t *testing.T) {
	root := t.TempDir()
	dirtyContentWrite(t, root, "note.txt", "bytes\n")
	status := dirtyContentStatus("? note.txt")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	backgroundInStatus := make(chan struct{})
	releaseBackground := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBackground) }) }
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		release()
		joined := make(chan struct{})
		go func() { workers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("sampling workers did not join after fixture cancellation")
		}
	})
	var mu sync.Mutex
	var commands []string
	calls := make(map[string]int)
	s := dirtyContentFake(t, root, func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if !slices.Contains(args, "status") {
			return nil, fmt.Errorf("unexpected fake Git command: %v", args)
		}
		class := "background"
		if urgentSample(ctx) {
			class = "urgent"
		}
		mu.Lock()
		calls[class]++
		call := calls[class]
		commands = append(commands, fmt.Sprintf("%s/status%d", class, call))
		mu.Unlock()
		if class == "background" && call == 1 {
			// Hold the background's actual first command until the urgent
			// sample is queued for the lease, then observe command order.
			close(backgroundInStatus)
			select {
			case <-releaseBackground:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return status, nil
	})

	var background, urgent DirtySnapshot
	var backgroundErr, urgentErr error
	backgroundDone := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(backgroundDone)
		background, backgroundErr = s.Sample(ctx)
	}()
	select {
	case <-backgroundInStatus:
	case <-ctx.Done():
		t.Fatal("background sample did not reach its first Git command")
	}
	urgentDone := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(urgentDone)
		urgent, urgentErr = s.Sample(WithUrgentSample(ctx))
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for s.urgentWaiting.Load() == 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("urgent sample did not queue behind the blocked Git command")
		}
	}
	release()
	for _, done := range []<-chan struct{}{backgroundDone, urgentDone} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("samples did not complete after releasing the Git command")
		}
	}
	if backgroundErr != nil || urgentErr != nil {
		t.Fatalf("background=%v urgent=%v", backgroundErr, urgentErr)
	}
	// Sample releases its lease before its caller resumes. Caller finish
	// order can invert after correct yielding; actual Git command order
	// proves the urgent sample's complete status fence ran before resumption.
	want := []string{"background/status1", "urgent/status1", "urgent/status2", "background/status2"}
	if !slices.Equal(commands, want) {
		t.Fatalf("Git command order %v, want %v", commands, want)
	}
	if s.UrgentYields() == 0 {
		t.Fatal("the background sample never yielded")
	}
	if background.Fingerprint == "" || !reflect.DeepEqual(background, urgent) {
		t.Fatalf("successful samples disagree: background=%+v urgent=%+v", background, urgent)
	}
}

// A background caller waiting for the lease does not take it ahead of an
// urgent one queued behind the same holder.
func TestUrgentSampleIsServedBeforeWaitingBackgroundSamples(t *testing.T) {
	root := t.TempDir()
	s := dirtyContentFake(t, root, func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		return dirtyContentStatus(), nil
	})
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	take := func(name string, ctx context.Context) {
		defer wg.Done()
		r, err := s.acquire(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		r()
	}
	wg.Add(1)
	go take("background", context.Background())
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go take("urgent", WithUrgentSample(context.Background()))
	for s.urgentWaiting.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	release()
	wg.Wait()
	if len(order) != 2 || order[0] != "urgent" {
		t.Fatalf("lease order %v, want the urgent caller first", order)
	}
}
