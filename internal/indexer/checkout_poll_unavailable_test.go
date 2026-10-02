package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/config"
)

func TestCheckoutPollMissingRootBacksOffAndCaps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vanished")
	const interval = 15 * time.Second
	const quiet = 10 * time.Millisecond
	const id = "missing-checkout"

	synctest.Test(t, func(t *testing.T) {
		cycles := make(chan time.Time, 128)
		c := scheduleOnlyCoordinator(id, root, interval, quiet, func() { cycles <- time.Now() })
		defer func() { _ = c.Close() }()
		started := time.Now()
		phase := initialCheckoutPollDelay(id, interval)

		// Missing probes occur at phase + 0, 30, 90, 210, 450, 750 seconds.
		time.Sleep(phase + 750*time.Second + time.Nanosecond)
		synctest.Wait()
		if len(cycles) != 0 {
			t.Fatalf("missing checkout ran %d reconcile cycles", len(cycles))
		}
		if !c.Running() {
			t.Fatal("one missing path closed the coordinator before lifecycle reconciliation")
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		// The capped next probe is at 1050 seconds, not 930 (uncapped) or never.
		time.Sleep(300*time.Second + quiet)
		synctest.Wait()
		select {
		case at := <-cycles:
			if want := phase + 1050*time.Second + quiet; at.Sub(started) != want {
				t.Fatalf("returned checkout resumed at %s, want %s", at.Sub(started), want)
			}
		default:
			t.Fatal("returned checkout did not resume at the backoff cap")
		}
	})
}

func TestCheckoutPollTransientDisappearanceResumesNormalInterval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const interval = 15 * time.Second
	const quiet = 10 * time.Millisecond
	const id = "transient-checkout"

	synctest.Test(t, func(t *testing.T) {
		cycles := make(chan time.Time, 16)
		c := scheduleOnlyCoordinator(id, root, interval, quiet, func() { cycles <- time.Now() })
		defer func() { _ = c.Close() }()
		phase := initialCheckoutPollDelay(id, interval)
		time.Sleep(phase + quiet + time.Nanosecond)
		synctest.Wait()
		if len(cycles) != 1 {
			t.Fatalf("present checkout ran %d cycles, want 1", len(cycles))
		}
		<-cycles
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		time.Sleep(interval)
		synctest.Wait()
		if len(cycles) != 0 {
			t.Fatal("vanished checkout still reconciles on the normal poll interval")
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * interval)
		synctest.Wait()
		if len(cycles) != 1 {
			t.Fatalf("returned checkout ran %d cycles, want 1", len(cycles))
		}
		resumed := <-cycles
		time.Sleep(interval)
		synctest.Wait()
		select {
		case at := <-cycles:
			if at.Sub(resumed) != interval {
				t.Fatalf("healthy poll interval = %s, want %s", at.Sub(resumed), interval)
			}
		default:
			t.Fatal("backoff was not reset after the checkout returned")
		}
	})
}

func TestCheckoutPollMissingRootStillAcceptsExplicitDemand(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	synctest.Test(t, func(t *testing.T) {
		cycles := make(chan struct{}, 128)
		c := scheduleOnlyCoordinator("explicit-missing", root, 15*time.Second, time.Millisecond, func() { cycles <- struct{}{} })
		defer func() { _ = c.Close() }()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if len(cycles) != 0 {
			t.Fatal("missing checkout was still polling reconcile")
		}
		c.Signal("interactive refresh")
		time.Sleep(time.Millisecond + time.Nanosecond)
		synctest.Wait()
		if len(cycles) != 1 {
			t.Fatalf("explicit demand ran %d cycles, want 1 despite poll backoff", len(cycles))
		}
	})
}

func TestCheckoutLifecycleRetiresMissingBackedOffCoordinator(t *testing.T) {
	f := newLifecycleFixture(t)
	// The lifecycle is closed inside the bubble below, which started its
	// coordinator. Closing it again outside the bubble would touch channels
	// the bubble owns, so the outer teardown closes it only when the bubble
	// did not run.
	lifecycleClosed := false
	defer func() {
		if !lifecycleClosed {
			_ = f.lc.Close()
		}
		_ = f.mi.Close(context.Background())
		_ = f.store.Close()
	}()
	ctx := context.Background()
	main := f.gitRepo("backoff-main")
	root := f.worktreeOf(main, "backoff-wt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: main, Name: "backoff-main"}, TrackSourceCLI)
	if err != nil || tracked.CatalogErr != nil {
		t.Fatalf("register primary: %v / %v", err, tracked.CatalogErr)
	}
	checkouts, err := f.catalog.ListCheckouts(ctx, tracked.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutID := ""
	for _, checkout := range checkouts {
		if checkout.CheckoutID != tracked.CheckoutID {
			checkoutID = checkout.CheckoutID
		}
	}
	if checkoutID == "" {
		t.Fatal("worktree has no catalog identity")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	// Git's omission is portable authoritative removal evidence.
	runGit(t, main, "worktree", "prune")

	synctest.Test(t, func(t *testing.T) {
		defer func() {
			_ = f.lc.Close()
			lifecycleClosed = true
		}()
		cycles := make(chan struct{}, 16)
		c := scheduleOnlyCoordinator(checkoutID, root, 15*time.Second, time.Millisecond, func() { cycles <- struct{}{} })
		defer func() { _ = c.Close() }()
		if !f.lc.installCoordinator(checkoutID, c) {
			t.Fatal("could not install coordinator")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if len(cycles) != 0 || !c.Running() {
			t.Fatal("missing coordinator must remain live without running reconcile cycles")
		}
		if _, err := f.lc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if c.Running() {
			t.Fatal("lifecycle could not close a backed-off coordinator")
		}
		if _, found, err := f.catalog.GetCheckout(ctx, checkoutID); err != nil || !found {
			t.Fatalf("identity removed before removal grace: found=%v err=%v", found, err)
		}
		f.clock.advance(lifecycleGrace.RemovalGrace + time.Second)
		if _, err := f.lc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if _, found, err := f.catalog.GetCheckout(ctx, checkoutID); err != nil || found {
			t.Fatalf("identity survived removal grace: found=%v err=%v", found, err)
		}
	})
}
