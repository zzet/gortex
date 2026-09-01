package persistence

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFeedbackStore() *FeedbackStore {
	return &FeedbackStore{
		Version:  "0.1.0-test",
		RepoPath: "/tmp/test-repo",
		Entries: []FeedbackEntry{
			{
				Timestamp: time.Now().Truncate(time.Second),
				Task:      "add new MCP tool",
				Useful:    []string{"server.go::Server", "tools_core.go::registerCoreTools"},
				NotNeeded: []string{"types.go::NodeKind"},
				Missing:   []string{"tools_enhancements.go::registerEnhancementTools"},
				Source:    "smart_context",
			},
			{
				Timestamp: time.Now().Add(-time.Hour).Truncate(time.Second),
				Task:      "fix bug in search",
				Useful:    []string{"search.go::Search"},
				Source:    "prefetch_context",
			},
		},
	}
}

func TestFeedback_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := testFeedbackStore()

	require.NoError(t, SaveFeedback(dir, store))

	loaded, err := LoadFeedback(dir)
	require.NoError(t, err)

	assert.Equal(t, store.Version, loaded.Version)
	assert.Equal(t, store.RepoPath, loaded.RepoPath)
	require.Len(t, loaded.Entries, 2)

	assert.Equal(t, "add new MCP tool", loaded.Entries[0].Task)
	assert.Equal(t, []string{"server.go::Server", "tools_core.go::registerCoreTools"}, loaded.Entries[0].Useful)
	assert.Equal(t, []string{"types.go::NodeKind"}, loaded.Entries[0].NotNeeded)
	assert.Equal(t, []string{"tools_enhancements.go::registerEnhancementTools"}, loaded.Entries[0].Missing)
	assert.Equal(t, "smart_context", loaded.Entries[0].Source)

	assert.Equal(t, "fix bug in search", loaded.Entries[1].Task)
	assert.Equal(t, "prefetch_context", loaded.Entries[1].Source)
}

func TestFeedback_ColdStart(t *testing.T) {
	dir := t.TempDir()
	loaded, err := LoadFeedback(dir)
	require.NoError(t, err)
	assert.NotNil(t, loaded)
	assert.Empty(t, loaded.Entries)
}

func TestFeedback_TrimOldEntries(t *testing.T) {
	dir := t.TempDir()
	store := &FeedbackStore{
		Version:  "0.1.0",
		RepoPath: "/tmp/test",
	}

	// Create 510 entries.
	for range 510 {
		store.Entries = append(store.Entries, FeedbackEntry{
			Timestamp: time.Now().Truncate(time.Second),
			Task:      "task",
			Useful:    []string{"sym"},
			Source:    "smart_context",
		})
	}

	require.NoError(t, SaveFeedback(dir, store))

	loaded, err := LoadFeedback(dir)
	require.NoError(t, err)
	assert.Len(t, loaded.Entries, 500)
}

func TestRepoCacheKey_Stable(t *testing.T) {
	key1 := RepoCacheKey("/tmp/my-repo")
	key2 := RepoCacheKey("/tmp/my-repo")
	assert.Equal(t, key1, key2)
	assert.Contains(t, key1, "_latest")
}

func TestRepoCacheKey_DifferentRepos(t *testing.T) {
	key1 := RepoCacheKey("/tmp/repo-a")
	key2 := RepoCacheKey("/tmp/repo-b")
	assert.NotEqual(t, key1, key2)
}

func TestFeedbackDir(t *testing.T) {
	dir := FeedbackDir("/home/user/.cache/gortex", "/tmp/my-repo")
	assert.Contains(t, dir, "_latest")
	// FeedbackDir filepath.Joins, so the result carries native separators and
	// feeds os.Open directly. The '/' spelling is the caller's input, not the
	// result's: on Windows this reads `\home\user\.cache\gortex\<key>`.
	assert.Contains(t, dir, filepath.Join(".cache", "gortex"))
}
