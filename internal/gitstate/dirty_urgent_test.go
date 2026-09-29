package gitstate

import (
	"context"
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
	backgroundInStatus := make(chan struct{})
	releaseBackground := make(chan struct{})
	var mu sync.Mutex
	var order []string
	calls := 0
	s := dirtyContentFake(t, root, func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			// The background sample's first status: hold it until the urgent
			// sample is queued for the lease.
			close(backgroundInStatus)
			<-releaseBackground
		}
		return status, nil
	})

	var wg sync.WaitGroup
	wg.Add(2)
	var backgroundErr, urgentErr error
	go func() {
		defer wg.Done()
		_, backgroundErr = s.Sample(context.Background())
		mu.Lock()
		order = append(order, "background")
		mu.Unlock()
	}()
	<-backgroundInStatus
	go func() {
		defer wg.Done()
		_, urgentErr = s.Sample(WithUrgentSample(context.Background()))
		mu.Lock()
		order = append(order, "urgent")
		mu.Unlock()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.urgentWaiting.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(releaseBackground)
	wg.Wait()
	if backgroundErr != nil || urgentErr != nil {
		t.Fatalf("background=%v urgent=%v", backgroundErr, urgentErr)
	}
	if len(order) != 2 || order[0] != "urgent" {
		t.Fatalf("finish order %v: the urgent sample waited for the whole background sample", order)
	}
	if s.UrgentYields() == 0 {
		t.Fatal("the background sample never yielded")
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
