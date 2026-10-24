package config

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
)

func newTestConfigManager(t *testing.T) *ConfigManager {
	t.Helper()
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-cache-test/config.yaml")
	require.NoError(t, err)
	return cm
}

func writeGitignore(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(content), 0o644))
}

func writeWorkspace(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(content), 0o644))
}

// bumpMtime forces a stat-visible change so invalidation fires regardless
// of the filesystem's mtime resolution.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
}

// countingReader wires a call counter into the cache's file reader so a
// test can assert that a cache hit never re-opens the `.gitignore`.
func countingReader(cm *ConfigManager) *int64 {
	var reads int64
	cm.excludeCache.readFn = func(p string) []string {
		atomic.AddInt64(&reads, 1)
		return loadRepoGitignore(p)
	}
	return &reads
}

func TestExcludeCache_CacheHitDoesNotReReadGitignore(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	writeGitignore(t, repoDir, "node_modules/\n*.log\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	reads := countingReader(cm)

	first := cm.EffectiveExclude("r")
	second := cm.EffectiveExclude("r")

	assert.Contains(t, first, "node_modules/")
	assert.Contains(t, second, "node_modules/")
	assert.Contains(t, second, "*.log")
	assert.Equal(t, first, second, "cache hit must return the same layered list")
	assert.Equal(t, int64(1), atomic.LoadInt64(reads),
		"the second call must be a cache hit, not a re-read")
}

func TestExcludeCache_IdenticalMatchingColdVsWarm(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	writeGitignore(t, repoDir, "build/\n*.tmp\ndata/raw/\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	cases := []struct {
		path    string
		exclude bool
	}{
		{"build/output.o", true},
		{"src/main.go", false},
		{"scratch.tmp", true},
		{"data/raw/big.bin", true},
		{"data/clean/ok.csv", false},
		{".git/config", true}, // builtin baseline
		{"README.md", false},
	}

	assertMatches := func(list []string, label string) {
		m := excludes.New(list)
		for _, c := range cases {
			assert.Equalf(t, c.exclude, m.MatchRel(c.path),
				"%s: path %q", label, c.path)
		}
	}

	assertMatches(cm.EffectiveExclude("r"), "cold") // populates the cache
	assertMatches(cm.EffectiveExclude("r"), "warm") // cache hit
}

func TestExcludeCache_InvalidatesOnGitignoreChange(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	writeGitignore(t, repoDir, "old-only/\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	reads := countingReader(cm)

	first := cm.EffectiveExclude("r")
	assert.Contains(t, first, "old-only/")

	// Different content (also a different byte size) plus a bumped mtime so
	// the change is detected even on a coarse-resolution filesystem.
	writeGitignore(t, repoDir, "new-only/\nsecond-new/\n")
	bumpMtime(t, filepath.Join(repoDir, ".gitignore"))

	second := cm.EffectiveExclude("r")
	assert.Contains(t, second, "new-only/", "new patterns must take effect after the edit")
	assert.Contains(t, second, "second-new/")
	assert.NotContains(t, second, "old-only/", "stale patterns must be dropped")
	assert.Equal(t, int64(2), atomic.LoadInt64(reads),
		"the changed file must be re-read exactly once")
}

func TestExcludeCache_InvalidatesOnWorkspaceReload(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	writeWorkspace(t, repoDir, "exclude:\n  - \"first/**\"\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	first := cm.EffectiveExclude("r")
	assert.Contains(t, first, "first/**")
	assert.NotContains(t, first, "second/**")

	// Reloading swaps the cached *Config pointer, which must invalidate the
	// merged entry even though the `.gitignore` (absent) is unchanged.
	writeWorkspace(t, repoDir, "exclude:\n  - \"second/**\"\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	second := cm.EffectiveExclude("r")
	assert.Contains(t, second, "second/**", "workspace edit must be reflected")
	assert.NotContains(t, second, "first/**", "stale workspace patterns must be dropped")
}

func TestExcludeCache_NoGitignoreNeverReads(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	cm.LoadWorkspaceConfig("r", repoDir)

	reads := countingReader(cm)

	got := cm.EffectiveExclude("r")
	_ = cm.EffectiveExclude("r")

	assert.Equal(t, excludes.Builtin, got, "absent .gitignore yields the builtin baseline")
	assert.Equal(t, int64(0), atomic.LoadInt64(reads),
		"an absent .gitignore must not trigger a file read")
}

func TestExcludeCache_ReturnedSliceIsClippedAndAppendSafe(t *testing.T) {
	cm := newTestConfigManager(t)
	repoDir := t.TempDir()
	writeGitignore(t, repoDir, "z-marker/\n")
	cm.LoadWorkspaceConfig("r", repoDir)

	first := cm.EffectiveExclude("r")
	require.Equal(t, len(first), cap(first),
		"returned slice must be clipped so append reallocates")

	// Appending to the returned slice must not disturb the shared cached
	// value handed to every other reader.
	_ = append(first, "mutant/**") //nolint:staticcheck // intentional: exercise append safety

	second := cm.EffectiveExclude("r")
	assert.NotContains(t, second, "mutant/**",
		"appending to the returned slice must not corrupt the cache")
	assert.Equal(t, first, second)
}

// uncachedEffectiveExclude reproduces the pre-cache EffectiveExclude body —
// a fresh `.gitignore` read plus a fresh merge on every call — so the
// benchmark can quote the baseline the cache replaces.
func uncachedEffectiveExclude(gc *GlobalConfig, ws *Config, repoPrefix, repoPath string) []string {
	out := make([]string, 0, 32)
	out = append(out, excludes.Builtin...)
	if shouldRespectGitignore(ws) && repoPath != "" {
		out = append(out, loadRepoGitignore(repoPath)...)
	}
	if gc != nil {
		out = append(out, gc.Exclude...)
		if entry := gc.FindRepoByPrefix(repoPrefix); entry != nil {
			out = append(out, entry.Exclude...)
		}
	}
	if ws != nil {
		out = append(out, ws.Exclude...)
		if len(ws.Exclude) == 0 {
			out = append(out, ws.Index.Exclude...)
			out = append(out, ws.Watch.Exclude...)
		}
	}
	return out
}

const benchGitignore = `# build output
build/
dist/
*.log
*.tmp
node_modules/
vendor/
coverage/
.cache/
target/
__pycache__/
*.pyc
.venv/
data/raw/
data/interim/
testdata/large/
*.bin
*.gz
.DS_Store
`

func benchSetup(b *testing.B) *ConfigManager {
	b.Helper()
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-cache-bench/config.yaml")
	require.NoError(b, err)
	repoDir := b.TempDir()
	require.NoError(b, os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(benchGitignore), 0o644))
	cm.LoadWorkspaceConfig("r", repoDir)
	return cm
}

func BenchmarkEffectiveExclude(b *testing.B) {
	cm := benchSetup(b)
	cm.EffectiveExclude("r") // warm the cache
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cm.EffectiveExclude("r")
	}
}

// benchSetupLayout builds a repo `depth` directories below its git root and
// tracks the deepest one, so the benchmark quotes the steady-state cost of
// the chain walk for each real-world layout. depth 0 is the common case
// (tracked root == git root); depth 2 is the monorepo the chain exists for.
func benchSetupLayout(b *testing.B, depth int) *ConfigManager {
	b.Helper()
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-cache-bench/config.yaml")
	require.NoError(b, err)
	root := b.TempDir()
	require.NoError(b, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(b, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(benchGitignore), 0o644))
	tracked := root
	for i := 0; i < depth; i++ {
		tracked = filepath.Join(tracked, "nested")
	}
	require.NoError(b, os.MkdirAll(tracked, 0o755))
	cm.LoadWorkspaceConfig("r", tracked)
	return cm
}

func BenchmarkEffectiveExcludeAtGitRoot(b *testing.B) {
	cm := benchSetupLayout(b, 0)
	cm.EffectiveExclude("r")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cm.EffectiveExclude("r")
	}
}

func BenchmarkEffectiveExcludeBelowGitRoot(b *testing.B) {
	cm := benchSetupLayout(b, 2)
	cm.EffectiveExclude("r")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cm.EffectiveExclude("r")
	}
}

func BenchmarkEffectiveExcludeUncached(b *testing.B) {
	cm := benchSetup(b)
	cm.mu.RLock()
	gc := cm.global
	ws := cm.workspace["r"]
	repoPath := cm.workspacePaths["r"]
	cm.mu.RUnlock()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = uncachedEffectiveExclude(gc, ws, "r", repoPath)
	}
}
