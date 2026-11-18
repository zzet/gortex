package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestIncludeDiagnosticsUseOriginalLines(t *testing.T) {
	for _, tc := range []struct {
		include    string
		warn, info int
	}{
		{"gen/", 0, 1}, {"Pods/", 0, 1}, {"generated/", 0, 1}, {"gen*/", 0, 1}, {"*.proto", 0, 1},
		{"build/**", 1, 0}, {"**/generated/", 0, 1},
		{"docs/api.md", 0, 1}, {"gen/out/", 0, 1},
		{".git/", 1, 0}, {".git/hooks/", 1, 0}, {".claude/skills/", 1, 0},
	} {
		t.Run(tc.include, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			idx := &Indexer{rootPath: t.TempDir(), config: config.IndexConfig{Exclude: []string{"gen/", "build/", "ios/", "node_modules/"}, Include: []string{tc.include}}, logger: zap.New(core)}
			matcher := idx.excludeMatcher()
			if tc.include == "Pods/" {
				require.True(t, matcher.MatchRel("ios/Pods/x.go"))
			}
			if tc.include == "generated/" {
				require.True(t, matcher.MatchRel("build/generated/a.go"))
			}
			for range 6 {
				idx.SetIgnorePatterns(idx.config.Exclude, idx.config.Include)
			}
			require.Equal(t, tc.warn, logs.FilterLevelExact(zap.WarnLevel).Len())
			require.Equal(t, tc.info, logs.FilterLevelExact(zap.InfoLevel).Len())
			for _, entry := range logs.All() {
				require.Equal(t, tc.include, entry.ContextMap()["pattern"])
				if tc.include == ".git/" || tc.include == ".git/hooks/" || tc.include == ".claude/skills/" {
					require.NotContains(t, entry.Message, "anchored literal")
					require.Contains(t, entry.Message, "never indexed")
				}
				if tc.include == "Pods/" || tc.include == "generated/" || tc.include == "gen/" {
					require.Contains(t, entry.Message, "only when their parents are open")
				}
				if tc.include == "docs/api.md" || tc.include == "gen/out/" {
					require.Contains(t, entry.Message, "root-anchored")
				}
			}
		})
	}
}

func TestNegationDiagnosticsDistinctIgnoreDirectories(t *testing.T) {
	root := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	idx := &Indexer{rootPath: root, logger: zap.New(core)}
	m := excludes.New([]string{"gen/", "!gen/keep.go"})
	for range 2 {
		for _, dir := range []string{"one", "two"} {
			idx.warnExcludedNegationsAt(m, filepath.Join(root, dir))
		}
	}
	require.Equal(t, 2, logs.Len())
	require.Equal(t, filepath.Join(root, "one"), logs.All()[0].ContextMap()["ignore_dir"])
	require.Equal(t, filepath.Join(root, "two"), logs.All()[1].ContextMap()["ignore_dir"])
}

func TestReincludeWarningCacheIsBounded(t *testing.T) {
	// The cache is process-wide to cover indexer rebuilds, but must not keep
	// every repository and pattern the daemon has ever seen.
	cache := reincludeWarningCache{}
	for i := 0; i < maxReincludeWarnings+3; i++ {
		require.True(t, cache.first(reincludeWarningKey{repo: fmt.Sprint(i), pattern: "!*.proto"}))
	}
	require.Len(t, cache.seen, maxReincludeWarnings)
	require.False(t, cache.first(reincludeWarningKey{repo: fmt.Sprint(maxReincludeWarnings + 2), pattern: "!*.proto"}))
	require.True(t, cache.first(reincludeWarningKey{repo: "0", pattern: "!*.proto"}), "old entries are evicted")
}

func TestIncludeDiagnosticsShareCanonicalRoot(t *testing.T) {
	root := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	for _, checkout := range []string{root, t.TempDir(), t.TempDir()} {
		idx := &Indexer{rootPath: checkout, warningRoot: root, config: config.IndexConfig{Include: []string{".git/hooks/"}}, logger: zap.New(core)}
		idx.excludeMatcher()
	}
	require.Equal(t, 1, logs.Len())
}

func TestWatchCheckoutIncludesShareOwnerWarningRoot(t *testing.T) {
	t.Setenv(checkoutWatchDisableEnv, "1")
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("include: [\".git/hooks/\"]\n"), 0o644))
	cm, err := config.NewConfigManager(filepath.Join(root, "global.yaml"))
	require.NoError(t, err)
	cm.LoadWorkspaceConfig("repo", root)
	require.Equal(t, []string{".git/hooks/"}, cm.GetRepoConfig("repo").Index.Include)
	core, logs := observer.New(zap.InfoLevel)
	stopped := make(chan struct{}, 2)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "checkout watch: not started; the checkout is refreshed by its poll" {
			stopped <- struct{}{}
		}
		return nil
	}))
	mi := &MultiIndexer{indexers: map[string]*Indexer{"repo": {rootPath: root}}}
	lifecycle := &CheckoutLifecycle{cfgWatchCheckouts: true, cfgMgr: cm, mi: mi, logger: logger}
	for _, name := range []string{"one", "two"} {
		// Files are rejected by the backend before it starts any native streams.
		checkout := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(checkout, nil, 0o644))
		lifecycle.watchCheckout(&CheckoutCoordinator{checkoutID: name}, "repo", checkout)
	}
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("checkout watch startup did not finish")
		}
	}
	require.Equal(t, 1, logs.FilterLevelExact(zap.WarnLevel).Len())
	require.Contains(t, logs.FilterLevelExact(zap.WarnLevel).All()[0].Message, "never indexed")
}
