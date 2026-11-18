package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingProjectionRetirementDoesNotCrossPhysicalCheckouts(t *testing.T) {
	a, b := newProjectionFixture(t), newProjectionFixture(t)
	p := newPendingProjections()
	const current = "package p\n\nfunc New() {}\n"
	ea, eb := a.edit("a.go", current), b.edit("a.go", current)
	require.Equal(t, ea.graphPath, eb.graphPath)
	require.Equal(t, ea.repoPrefix, eb.repoPrefix)
	require.Equal(t, ea.hash, eb.hash, "equal graph paths and byte identities cannot select a physical checkout")
	require.NotEqual(t, ea.key, eb.key)
	require.NoError(t, p.publish(ea))
	require.NoError(t, p.publish(eb))
	require.NoError(t, a.idx.IndexFile(filepath.Join(a.dir, "a.go")))
	require.NotNil(t, b.g.GetNode("a.go::Old"), "checkout B's backend has not caught up")
	assert.Equal(t, 1, p.retirePaths(a.g, []string{ea.absPath}), "A publication may verify only A's physical frontier")
	require.Nil(t, liveEntry(p, ea.absPath))
	require.Same(t, eb, liveEntry(p, eb.absPath), "A's matching store hash must not retire B's still-pending graph")
	require.NoError(t, b.idx.IndexFile(filepath.Join(b.dir, "a.go")))
	require.Equal(t, 1, p.retirePaths(b.g, []string{eb.absPath}))
	require.Nil(t, liveEntry(p, eb.absPath))
}

func TestPendingProjectionScopedRetirementEmptySetDoesNoStoreWork(t *testing.T) {
	f := newProjectionFixture(t)
	p := newPendingProjections()
	e := f.edit("a.go", "package p\n\nfunc New() {}\n")
	require.NoError(t, p.publish(e))
	store := &projectionRetirementSpy{Graph: f.g}
	require.Zero(t, p.retirePaths(store, nil))
	require.Zero(t, store.metadataReads)
	require.Zero(t, store.nodeReads)
	require.Same(t, e, liveEntry(p, e.absPath))
}
