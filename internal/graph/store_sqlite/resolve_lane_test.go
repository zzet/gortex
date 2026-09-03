package store_sqlite

import "testing"

// The resolver-coordination lane's scope.
//
// ResolveMutex is held across whole O(graph) passes — the resolver's inference
// passes, clone detection, capability and test-edge synthesis, external-call
// synthesis. Those passes describe the graph the handle reads, and a handle
// pinned to a derived payload generation reads only that generation. One lane
// per generation is therefore the exact grain: passes over the same payload
// still exclude each other, and a build of one checkout's layer no longer
// queues behind another checkout's.

func TestResolveLaneIsSharedAcrossBaseHandles(t *testing.T) {
	store := openPayloadStore(t)

	if store.ResolveMutex() != store.AtGeneration(baseViewGeneration).ResolveMutex() {
		t.Fatal("two handles on the base corpus took different resolve lanes")
	}
}

func TestResolveLaneIsSharedAcrossHandlesOnOneGeneration(t *testing.T) {
	store := openPayloadStore(t)

	first := store.AtGeneration(7).ResolveMutex()
	if second := store.AtGeneration(7).ResolveMutex(); first != second {
		t.Fatal("two handles on one payload generation took different resolve lanes")
	}
}

func TestResolveLaneIsPerPayloadGeneration(t *testing.T) {
	store := openPayloadStore(t)

	seven := store.AtGeneration(7).ResolveMutex()
	eight := store.AtGeneration(8).ResolveMutex()
	switch {
	case seven == eight:
		t.Error("two payload generations share one resolve lane")
	case seven == store.ResolveMutex():
		t.Error("a payload generation resolves on the base corpus lane")
	}
}

// TestGenerationResolveLaneLeavesTheBaseLaneFree is the work bound. A pass
// over one generation must leave both the base corpus and every other
// generation free to resolve.
func TestGenerationResolveLaneLeavesTheBaseLaneFree(t *testing.T) {
	store := openPayloadStore(t)

	held := store.AtGeneration(7).ResolveMutex()
	held.Lock()
	defer held.Unlock()

	if store.AtGeneration(7).ResolveMutex().TryLock() {
		t.Error("a second pass over the same generation crossed a held resolve lane")
	}
	if !store.ResolveMutex().TryLock() {
		t.Error("a generation's resolve pass held the base corpus lane")
	} else {
		store.ResolveMutex().Unlock()
	}
	if !store.AtGeneration(8).ResolveMutex().TryLock() {
		t.Error("a generation's resolve pass held another generation's lane")
	} else {
		store.AtGeneration(8).ResolveMutex().Unlock()
	}
}

// TestBaseResolveLaneStillExcludesEveryBaseMutation is the other half of the
// pin: the base corpus keeps one lane, so a watcher reindex and a resolver
// pass over the corpus still serialise exactly as before.
func TestBaseResolveLaneStillExcludesEveryBaseMutation(t *testing.T) {
	store := openPayloadStore(t)

	store.ResolveMutex().Lock()
	defer store.ResolveMutex().Unlock()

	if store.AtGeneration(baseViewGeneration).ResolveMutex().TryLock() {
		t.Fatal("a second base-corpus pass crossed a held base resolve lane")
	}
}
