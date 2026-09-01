package fswatcher

import (
	"testing"
	"time"
)

func TestEventAggregatorCloseRejectsInFlightAdd(t *testing.T) {
	events := make(chan WatchEvent, 1)
	dropped := make(chan WatchEvent, 1)
	ea := newEventAggregator(&watcher{events: events, dropped: dropped}, time.Hour)

	addReachedAdmission := make(chan struct{})
	releaseAdd := make(chan struct{})
	ea.beforeAddLock = func() {
		close(addReachedAdmission)
		<-releaseAdd
	}
	addDone := make(chan struct{})
	go func() {
		defer close(addDone)
		ea.addEvent(WatchEvent{Path: "/in-flight", Types: []EventType{EventMod}})
	}()

	<-addReachedAdmission
	ea.close()
	close(releaseAdd)
	select {
	case <-addDone:
	case <-time.After(time.Second):
		t.Fatal("in-flight add did not return after close")
	}

	ea.mu.Lock()
	remaining := len(ea.events)
	ea.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("event admitted after close: remaining=%d", remaining)
	}
	if ea.started.Load() {
		t.Fatal("in-flight event started the aggregator after close")
	}
	if len(events) != 0 || len(dropped) != 0 {
		t.Fatalf("event emitted after close: events=%d dropped=%d", len(events), len(dropped))
	}
}

func TestEventAggregatorCloseFlushesAndJoinsRun(t *testing.T) {
	events := make(chan WatchEvent, 1)
	dropped := make(chan WatchEvent, 1)
	ea := newEventAggregator(&watcher{events: events, dropped: dropped}, time.Hour)
	ea.addEvent(WatchEvent{Path: "/queued", Types: []EventType{EventCreate}})

	ea.close()

	select {
	case <-ea.runDone:
	default:
		t.Fatal("close returned before the run goroutine exited")
	}
	if len(events) != 1 {
		t.Fatalf("close did not flush queued event: got=%d want=1", len(events))
	}

	secondCloseDone := make(chan struct{})
	go func() {
		ea.close()
		close(secondCloseDone)
	}()
	select {
	case <-secondCloseDone:
	case <-time.After(time.Second):
		t.Fatal("second close did not observe completed shutdown")
	}
}

func BenchmarkEventAggregatorAddAndClose(b *testing.B) {
	for b.Loop() {
		events := make(chan WatchEvent, 1)
		dropped := make(chan WatchEvent, 1)
		ea := newEventAggregator(&watcher{events: events, dropped: dropped}, time.Hour)
		ea.addEvent(WatchEvent{Path: "/bench", Types: []EventType{EventMod}})
		ea.close()
	}
}
