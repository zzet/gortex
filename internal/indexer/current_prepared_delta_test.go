package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const refreshSourceA = "package main\nfunc Alpha(){}\nfunc Beta(){}\nfunc Caller(){Alpha()}\n"
const refreshSourceB = "package main\nfunc Alpha(){}\nfunc Beta(){}\nfunc Caller(){Beta()}\n"

func TestCurrentPreparedDeltaRestagesNewBytesWithTheirOwnProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, refreshSourceA)
	idx := newParseTreeReleaseIndexer(t, dir)
	t.Cleanup(idx.Close)
	prior, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	oldTree := preparedTree(t, idx, path)
	writeTestFile(t, path, refreshSourceB)
	fresh, prepared, reprepared, ok := idx.currentPreparedDelta(path, prior, false, nil)
	require.Nil(t, oldTree.Tree(), "the stale parse must be physically released")
	require.True(t, reprepared)
	require.True(t, ok, "a single newer save should remain eligible for staged apply instead of legacy fallback")
	require.NotNil(t, prepared)
	defer prepared.release()
	require.Equal(t, refreshSourceB, string(prepared.src))
	fingerprints, _, valid := extractionFingerprints(prepared.result)
	require.True(t, valid)
	require.Equal(t, fingerprints, fresh.fingerprints, "fresh B must never be paired with probe A")
	require.NotEqual(t, prior.fingerprints.semantic, fresh.fingerprints.semantic)
	current, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, sameFileVersion(prepared.readVersion.info, current), "receipt must describe the actual newer read")
}

func TestCurrentPreparedDeltaStableReadReusesParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, refreshSourceA)
	idx := newParseTreeReleaseIndexer(t, dir)
	t.Cleanup(idx.Close)
	prior, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	oldTree := preparedTree(t, idx, path)
	fresh, prepared, reprepared, ok := idx.currentPreparedDelta(path, prior, false, nil)
	require.True(t, ok)
	defer prepared.release()
	require.False(t, reprepared, "unchanged bytes must reuse the original parse")
	require.Same(t, oldTree, prepared.result.Tree)
	require.Equal(t, prior.fingerprints, fresh.fingerprints)
}

func TestCurrentPreparedDeltaSecondMovementFallsBackWithoutStaleParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, refreshSourceA)
	idx := newParseTreeReleaseIndexer(t, dir)
	t.Cleanup(idx.Close)
	prior, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	writeTestFile(t, path, refreshSourceB)
	extractor, found := idx.registry.GetByLanguage("go")
	require.True(t, found)
	mover := &savingExtractor{Extractor: extractor, trigger: "main.go", path: path, line: "\nfunc Newer(){}\n"}
	mover.armed.Store(true)
	idx.registry.Register(mover)
	_, prepared, reprepared, ok := idx.currentPreparedDelta(path, prior, false, nil)
	if prepared != nil {
		prepared.release()
	}
	require.True(t, reprepared)
	require.False(t, mover.armed.Load(), "the bounded fresh parse must actually consume B before its read moves again")
	_, saveErr := mover.saved()
	require.NoError(t, saveErr)
	require.False(t, ok, "a second movement must retain the conservative fallback, never publish stale B")
	require.Nil(t, prepared)
	idx.preparedMu.Lock()
	remaining := len(idx.prepared)
	idx.preparedMu.Unlock()
	require.Zero(t, remaining, "no rejected parse or admission may remain retained")
}
