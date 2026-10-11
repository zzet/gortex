package indexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func TestContractAnalysisCoordinatorCoalescesOwnedCohortAndPublishesBothTargets(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "unused-coordinator-a")
	b := f.request(t, "repo-b", "unused-coordinator-b")
	var released atomic.Int64
	var reads atomic.Int64
	var coreReads atomic.Int64
	var readMu sync.Mutex
	uniqueReads := make(map[string]bool)
	started := make(chan struct{})
	resume := make(chan struct{})
	var entered atomic.Bool
	capture := func(ctx context.Context, _ *graphview.RepoView, _ *graphview.SelectedContractInputs, _, _ string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		snapshot := a.Snapshot
		snapshot.Release = func() { released.Add(1) }
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			coreReads.Add(1)
			return a.Snapshot.ReadCoreFile(ctx, file)
		}
		snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
			reads.Add(1)
			readMu.Lock()
			uniqueReads[file.Path] = true
			readMu.Unlock()
			if entered.CompareAndSwap(false, true) {
				close(started)
				select {
				case <-resume:
				case <-ctx.Done():
					return ContractAcceptedSource{}, ctx.Err()
				}
			}
			return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, nil
		}
		return snapshot, []ContractFollowupTarget{{Key: a.Snapshot.Key, Work: a.Snapshot.Work, Catalog: f.store}, {Key: b.Snapshot.Key, Work: b.Snapshot.Work, Catalog: f.store}}, nil
	}
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: capture, Yield: ContractAnalysisYield(func() bool { return true })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	inputs := &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}
	admitted, err := c.Request(context.Background(), nil, inputs, "repo-a", "")
	if err != nil || !admitted {
		t.Fatalf("admit=%v err=%v", admitted, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	admitted, err = c.Request(context.Background(), nil, inputs, "repo-b", "")
	if err != nil || !admitted {
		t.Fatalf("coalesce=%v err=%v", admitted, err)
	}
	if released.Load() != 1 {
		t.Fatalf("duplicate handoff not released: %d", released.Load())
	}
	close(resume)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, key := range []graph.ContractAttachmentKey{a.Snapshot.Key, b.Snapshot.Key} {
		for {
			header, err := f.store.GetContractAttachmentContext(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if header != nil {
				break
			}
			if err := c.WaitChange(ctx, key); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	readMu.Lock()
	uniqueCount := len(uniqueReads)
	readMu.Unlock()
	// Lazy body/signature enrichment may legitimately reread an accepted file;
	// complete core census admission must occur exactly once for the cohort.
	if uniqueCount != len(f.files) || coreReads.Load() != int64(len(f.files)) || reads.Load() < int64(uniqueCount) || released.Load() != 2 || f.leases.Held() != 0 {
		t.Fatalf("shared sources raw=%d unique=%d core census=%d releases=%d leases=%d", reads.Load(), uniqueCount, coreReads.Load(), released.Load(), f.leases.Held())
	}
	// Sustained foreground demand stayed true for every scheduling checkpoint,
	// yet the real shared worker published both targets under this same deadline.
}

func TestContractAnalysisCoordinatorBaselineRaceAndCancellation(t *testing.T) {
	f := newContractWorkerFixture(t)
	var c *ContractAnalysisCoordinator
	baselineCalls := 0
	var err error
	c, err = NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		t.Error("unknown baseline captured")
		return ContractFollowupSnapshot{}, nil, errors.New("unsupported")
	}, ReconcileBaseline: func(ctx context.Context, _ *graphview.RepoView, repo, checkout string) (bool, error) {
		baselineCalls++
		if baselineCalls == 1 {
			c.Published(ctx, repo, checkout)
		}
		return true, nil
	}, Yield: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	admitted, err := c.Request(ctx, nil, nil, "repo-a", "")
	if err != nil || !admitted || baselineCalls != 1 {
		t.Fatalf("baseline=%v calls=%d err=%v", admitted, baselineCalls, err)
	}
	if err := c.WaitChange(ctx, graph.ContractAttachmentKey{RepoPrefix: "repo-a"}); err != nil {
		t.Fatalf("publication before wait lost: %v", err)
	}
	second, cancelSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelSecond()
	admitted, err = c.Request(second, nil, nil, "repo-a", "")
	if err != nil || !admitted {
		t.Fatalf("second baseline admission=%v %v", admitted, err)
	}
	if err := c.WaitChange(second, graph.ContractAttachmentKey{RepoPrefix: "repo-a"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second baseline reused closed observation: %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if admitted, err := c.Request(canceled, nil, nil, "repo-a", ""); admitted || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission=%v err=%v", admitted, err)
	}
}

func TestContractAnalysisCoordinatorCloseJoinsAdmissionRelease(t *testing.T) {
	f := newContractWorkerFixture(t)
	releasing := make(chan struct{})
	resume := make(chan struct{})
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		return ContractFollowupSnapshot{Release: func() { close(releasing); <-resume }}, []ContractFollowupTarget{{Key: graph.ContractAttachmentKey{RepoPrefix: "repo-a", InputVersion: "v", InputFingerprint: "f"}, Catalog: f.store}}, nil
	}, Yield: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := c.Request(context.Background(), nil, &graphview.SelectedContractInputs{State: graph.ContractInputState{Accepted: true}}, "repo-a", "")
		requestDone <- err
	}()
	select {
	case <-releasing:
	case <-time.After(time.Second):
		t.Fatal("release did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close certified admission still owning release: %v", err)
	}
	close(resume)
	if err := <-requestDone; err == nil {
		t.Fatal("incomplete handoff accepted")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContractAnalysisCoordinatorPendingAcceptanceWaitsWithoutCapture(t *testing.T) {
	f := newContractWorkerFixture(t)
	c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg, Capture: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		t.Error("pending input was captured for analysis")
		return ContractFollowupSnapshot{}, nil, errors.New("pending")
	}, Yield: ContractAnalysisYield(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inputs := &graphview.SelectedContractInputs{State: graph.ContractInputState{RepoPrefix: "repo-a", InputVersion: "pending-v", InputFingerprint: "pending-f"}}
	if admitted, err := c.Request(ctx, nil, inputs, "repo-a", ""); err != nil || !admitted {
		t.Fatalf("pending observation=%v %v", admitted, err)
	}
	c.Published(ctx, "repo-a", "") // Acceptance races Wait registration.
	if err := c.WaitChange(ctx, graph.ContractAttachmentKey{RepoPrefix: "repo-a", InputVersion: "pending-v", InputFingerprint: "pending-f"}); err != nil {
		t.Fatal(err)
	}
}
