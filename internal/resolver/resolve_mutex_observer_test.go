package resolver

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestMutationResolveMutexObserverEndsBeforeModuleRefresh(t *testing.T) {
	g := graph.New()
	r := New(g)
	enteringLock, refreshing, permitRefresh, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var signal, unlock, allowRefresh sync.Once
	core, _ := observer.New(zap.InfoLevel)
	r.SetLogger(zap.New(zapcore.RegisterHooks(core, func(entry zapcore.Entry) error {
		if entry.Message == "resolver: incremental phase starting" {
			signal.Do(func() { close(enteringLock) })
		}
		return nil
	})))
	r.SetGoModuleLookup(func() map[string]string { close(refreshing); <-permitRefresh; return nil })
	observed := make(chan [2]time.Time, 1)
	g.ResolveMutex().Lock()
	release := func() { unlock.Do(g.ResolveMutex().Unlock) }
	resume := func() { allowRefresh.Do(func() { close(permitRefresh) }) }
	t.Cleanup(func() {
		release()
		resume()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("resolver did not drain")
		}
	})
	go func() {
		defer close(done)
		r.ResolveFilesAndIncomingObserved([]string{"missing.go"}, func(started, acquired time.Time) { observed <- [2]time.Time{started, acquired} })
	}()
	select {
	case <-enteringLock:
	case <-time.After(2 * time.Second):
		t.Fatal("resolve never reached mutex admission")
	}
	select {
	case <-observed:
		t.Fatal("held mutation mutex reported acquired before release")
	default:
	}
	timer := time.NewTimer(20 * time.Millisecond)
	<-timer.C
	release()
	select {
	case <-refreshing:
	case <-time.After(2 * time.Second):
		t.Fatal("resolve did not reach module refresh")
	}
	select {
	case interval := <-observed:
		if interval[0].IsZero() || !interval[1].After(interval[0]) {
			t.Fatalf("held mutex acquisition has no positive observed interval: %v", interval)
		}
	default:
		t.Fatal("actual mutation acquisition was not observed before module refresh")
	}
	select {
	case <-done:
		t.Fatal("module refresh unexpectedly completed without its permit")
	default:
	}
	resume()
	<-done
	select {
	case <-observed:
		t.Fatal("one execution reported multiple acquisition waits")
	default:
	}
}

func TestMutationResolveMutexObserverLegacyNilAndEmptyCompatibility(t *testing.T) {
	legacy := New(graph.New()).ResolveFilesAndIncoming([]string{"missing.go"})
	unobserved := New(graph.New()).ResolveFilesAndIncomingObserved([]string{"missing.go"}, nil)
	if !reflect.DeepEqual(legacy, unobserved) {
		t.Fatalf("nil observer changed resolution: legacy=%+v observed=%+v", legacy, unobserved)
	}
	calls := 0
	empty := New(graph.New()).ResolveFilesAndIncomingObserved(nil, func(time.Time, time.Time) { calls++ })
	if calls != 0 || !reflect.DeepEqual(empty, &ResolveStats{}) {
		t.Fatal("empty execution fabricated an observed zero acquisition")
	}
}

func TestMutationResolveMutexObserverCrossRepoCompatibility(t *testing.T) {
	files := []string{"missing.go"}
	legacy := NewCrossRepo(graph.New()).ResolveMutationFrontiers(files, files, files)
	unobserved := NewCrossRepo(graph.New()).ResolveMutationFrontiersObserved(files, files, files, nil)
	if !reflect.DeepEqual(legacy, unobserved) {
		t.Fatal("nil observer changed cross-repo resolution")
	}
	g := graph.New()
	calls := 0
	NewCrossRepo(g).ResolveMutationFrontiersObserved(files, files, files, func(started, acquired time.Time) {
		calls++
		if started.IsZero() || acquired.Before(started) {
			t.Fatal("invalid cross-repo acquisition interval")
		}
		if g.ResolveMutex().TryLock() {
			g.ResolveMutex().Unlock()
			t.Fatal("cross-repo observer did not follow mutex acquisition")
		}
	})
	if calls != 1 {
		t.Fatal("cross-repo execution did not report one actual acquisition")
	}
	NewCrossRepo(g).ResolveMutationFrontiersObserved(nil, nil, nil, func(time.Time, time.Time) { calls++ })
	if calls != 1 {
		t.Fatal("empty cross-repo pass fabricated an acquisition")
	}
}
