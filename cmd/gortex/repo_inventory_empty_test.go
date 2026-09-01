package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/daemon"
)

// Issue #624: a repo that finished indexing with zero files rendered as
// an ordinary zero-count row. That is the one state an operator has to
// act on and the one state every view stayed quiet about — so a repo of
// 1,519 Python files reported as tracked and healthy while holding
// nothing, and every query against it answered "not found" with full
// confidence.

func TestRepoIndexIsEmpty(t *testing.T) {
	assert.True(t, repoIndexIsEmpty(daemon.TrackedRepoStatus{LastIndex: 1700000000}),
		"indexed, present, zero files — the reported state")
	assert.False(t, repoIndexIsEmpty(daemon.TrackedRepoStatus{Files: 12, LastIndex: 1700000000}))
	assert.False(t, repoIndexIsEmpty(daemon.TrackedRepoStatus{}),
		"a repo that has never finished an index pass is pending, not empty")
	assert.False(t, repoIndexIsEmpty(daemon.TrackedRepoStatus{Missing: true, LastIndex: 1700000000}),
		"a deleted path is the actionable fact; it outranks an empty index")
	assert.False(t, repoIndexIsEmpty(daemon.TrackedRepoStatus{Unloaded: true, LastIndex: 1700000000}))
}

func TestRepoStateLabel_EmptyIndex(t *testing.T) {
	assert.Equal(t, "EMPTY", repoStateLabel(daemon.TrackedRepoStatus{LastIndex: 1700000000}))
	assert.Equal(t, "MISSING", repoStateLabel(daemon.TrackedRepoStatus{Missing: true, LastIndex: 1700000000}))
	assert.Equal(t, "ok", repoStateLabel(daemon.TrackedRepoStatus{Files: 1, LastIndex: 1700000000}))
}

func TestRepoItemState_EmptyIndex(t *testing.T) {
	assert.Equal(t, "EMPTY — 0 files indexed", repoItemState(daemon.TrackedRepoStatus{LastIndex: 1700000000}))
	assert.Empty(t, repoItemState(daemon.TrackedRepoStatus{Files: 3, LastIndex: 1700000000}))
}

func TestRenderEmptyIndexWarning_PluralisesAndListsEach(t *testing.T) {
	var buf bytes.Buffer
	renderEmptyIndexWarning(&buf, []daemon.TrackedRepoStatus{
		{Prefix: "pandas", Path: "/srv/pandas", LastIndex: 1700000000},
		{Prefix: "django", Path: "/srv/django", Files: 2600, LastIndex: 1700000000},
		{Prefix: "numpy", Path: "/srv/numpy", LastIndex: 1700000000},
	})
	out := buf.String()

	assert.Contains(t, out, "2 tracked repos indexed no files")
	assert.NotContains(t, out, "repos indexed no file\n")
	assert.Contains(t, out, "/srv/pandas")
	assert.Contains(t, out, "/srv/numpy")
	assert.NotContains(t, out, "/srv/django")
	assert.Contains(t, out, "no source files were indexed",
		"the block must point at the daemon log line that names the pattern")
}

func TestRenderEmptyIndexWarning_QuietWhenEveryRepoHasFiles(t *testing.T) {
	var buf bytes.Buffer
	renderEmptyIndexWarning(&buf, []daemon.TrackedRepoStatus{
		{Prefix: "django", Path: "/srv/django", Files: 2600, LastIndex: 1700000000},
		{Prefix: "pending", Path: "/srv/pending"},
	})
	assert.Empty(t, buf.String())
}
