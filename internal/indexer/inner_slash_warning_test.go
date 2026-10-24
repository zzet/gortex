package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
)

func TestExcludeMatcher_InnerSlashNegationWarnings(t *testing.T) {
	for _, filename := range []string{".gitignore", ".gortexignore"} {
		for _, tc := range []struct {
			name, rules string
			warn        bool
		}{
			{"rooted_parent", "/gen/\n!gen/keep.go\n", true},
			{"allowlist", "/*\n!/main.go\n!gen/keep.go\n", true},
			{"open_parents", "/*\n!/gen/\n!gen/keep.go\n", false},
		} {
			t.Run(filename+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				writeExcludeFixture(t, filepath.Join(root, filename), tc.rules)
				writeExcludeFixture(t, filepath.Join(root, "gen/keep.go"), "package fixture\n")
				cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
				require.NoError(t, err)
				cm.LoadWorkspaceConfig("fixture", root)
				cfg := cm.GetRepoConfig("fixture")
				core, logs := observer.New(zap.WarnLevel)
				for range 2 {
					idx := &Indexer{rootPath: root, config: cfg.Index, logger: zap.New(core)}
					for range 2 {
						require.Equal(t, tc.warn, idx.shouldExcludeRules(filepath.Join(root, "gen/keep.go"), root, false))
						idx.SetExcludePatterns(cfg.Index.Exclude)
						if idx.dirIgnore != nil {
							idx.dirIgnore.Forget(root)
						}
					}
				}
				want := 0
				if tc.warn {
					want = 1
				}
				require.Equal(t, want, logs.Len(), "one warning per repo and pattern across reloads and rebuilds")
				for _, entry := range logs.All() {
					require.Equal(t, "!gen/keep.go", entry.ContextMap()["pattern"])
					require.Equal(t, root, entry.ContextMap()["repo"])
					require.Contains(t, entry.Message, "negations no longer reach inside excluded folders")
					if filename == ".gortexignore" {
						require.Equal(t, root, entry.ContextMap()["ignore_dir"])
					}
				}
			})
		}
	}
}
