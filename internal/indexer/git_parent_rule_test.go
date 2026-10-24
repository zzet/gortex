package indexer

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// Check every entry, not only the entries a pruned walk happens to visit.
// The same fixtures cover flat excludes and per-folder ignore files.
func TestPruneDir_GitParentRuleAgreement(t *testing.T) {
	for _, tc := range []struct {
		name, rules string
		keep        bool
		scope       string
	}{
		{"closed_parent", "dir/\n!dir/keep.go\n", false, ""},
		{"basename_negation", "dir/\n!*.go\n!.gitkeep\n", false, ""},
		{"globstar_negation", "**/dir/\n!**/keep.go\n", false, ""},
		{"anchored_negation", "/dir/\n!/dir/keep.go\n", false, ""},
		{"open_child", "dir/*\n!dir/keep/\n", true, ""},
		{"open_folder", "dir/\n!dir/\n", true, ""},
		{"backend_reopened_child", "gen/*\n!gen/keep/\n", true, "gen/keep"},
		{"backend_reopened_ancestors", "/*\n!/agent/\n/agent/*\n!/agent/skills/\n", true, "agent/skills"},
	} {
		for _, perFolder := range []bool{false, true} {
			mode := "flat"
			if perFolder {
				mode = "per_folder"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				for _, rel := range []string{"dir/keep.go", "dir/keep/deep.go", "dir/drop/drop.go", "open/a.go", "gen/keep/deep.go", "agent/skills/deep.go", "node_modules/pkg/deep.go"} {
					writeExcludeFixture(t, filepath.Join(root, rel), "package fixture\n")
				}
				// This file cannot reopen dir/ when the parent itself is excluded.
				writeExcludeFixture(t, filepath.Join(root, "dir/.gortexignore"), "!keep.go\n!keep/\n")
				patterns := []string{}
				if perFolder {
					writeExcludeFixture(t, filepath.Join(root, ".gortexignore"), tc.rules)
				} else {
					// Keep the watch and index lists identical; NewWatcher uses its own list.
					patterns = strings.Split(tc.rules, "\n")
				}
				idx := newAdmissionTestIndexer(t, root, patterns...)
				watchPatterns := patterns
				if !perFolder {
					watchPatterns = idx.config.Exclude
				}
				w, err := NewWatcher(idx, config.WatchConfig{Exclude: watchPatterns}, zap.NewNop())
				require.NoError(t, err)
				filter := &watcherExcludeFilter{w: w}
				expectedFiles := map[string]bool{}
				require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err)
					excluded := idx.shouldExcludeRules(path, root, d.IsDir())
					require.Equal(t, excluded, w.isExcluded(path), path)
					if d.IsDir() {
						require.Equal(t, excluded, idx.shouldPruneDir(path, root), path)
						if !excluded {
							require.True(t, filter.ShouldInclude(path), "backend must pass open directory: %s", path)
						}
					} else if filepath.Ext(path) == ".go" {
						if !excluded {
							expectedFiles[path] = true
						}
						// The backend knows neither the entry type nor per-folder files.
						// It may pass events to the full check, but must not drop admitted files.
						if !excluded {
							require.True(t, filter.ShouldInclude(path), path)
						}
						if !perFolder {
							require.Equal(t, !excluded, filter.ShouldInclude(path), path)
						}
					}
					return nil
				}))
				scope := tc.scope
				if scope == "" {
					scope = "dir/keep"
				}
				keep := filepath.Join(root, scope, "deep.go")
				require.Equal(t, tc.keep, !idx.shouldExcludeRules(keep, root, false))
				visitedFiles := map[string]bool{}
				require.NoError(t, idx.walkDisk(root, root, func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err)
					if !d.IsDir() && filepath.Ext(path) == ".go" {
						full := idx.admitWalkFileKnownType(root, path, -1, d.Type())
						walk := idx.admitFileInOpenDir(root, path, -1, d.Type())
						require.Equal(t, full, walk, "walk/full agreement: %s", path)
						if walk.admit {
							visitedFiles[path] = true
						}
					}
					return nil
				}))
				require.Equal(t, expectedFiles, visitedFiles)
				// Scoped walks and the scope probe start inside an excluded tree too.
				require.Equal(t, tc.keep, mustScope(t, idx, scope))
				var scopedFiles []string
				require.NoError(t, idx.walkDisk(root, filepath.Join(root, scope), func(path string, d fs.DirEntry, err error) error {
					require.NoError(t, err)
					if !d.IsDir() && idx.admitScopedWalkFile(root, path) {
						scopedFiles = append(scopedFiles, path)
					}
					return nil
				}))
				if tc.keep {
					require.Equal(t, []string{keep}, scopedFiles)
				} else {
					require.Empty(t, scopedFiles)
				}
			})
		}
	}
}

func TestIndex_IncludeOverridesExcludedParents(t *testing.T) {
	for _, tc := range []struct {
		name, include, target string
		want                  bool
	}{
		{"basename", "Pods/", "ios/Pods/keep.go", true},
		{"basename_closed_ancestor", "Pods/", "ios/Pods/keep.go", false},
		{"basename_per_folder", "Pods/", "ios/Pods/keep.go", false},
		{"vendor", "vendor/github.com/acme/lib/", "vendor/github.com/acme/lib/keep.go", true},
		{"vendor_per_folder", "vendor/github.com/acme/lib/", "vendor/github.com/acme/lib/keep.go", false},
		{"vendor_no_slash", "vendor/github.com/acme/lib", "vendor/github.com/acme/lib/keep.go", true},
		{"node_modules", "node_modules/@org/pkg/", "node_modules/@org/pkg/keep.go", true},
		{"literal_file", "vendor/acme/keep.go", "vendor/acme/keep.go", true},
		{"wildcard_closed", "*.go", "node_modules/dep/keep.go", false},
		{"wildcard_open", "*.go", "src/keep.go", true},
		{"git_hard_rule", ".git/", ".git/keep.go", false},
		{"agent_hard_rule", ".claude/skills/", ".claude/skills/keep.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeExcludeFixture(t, filepath.Join(root, tc.target), "package fixture\nfunc Kept() {}\n")
			writeExcludeFixture(t, filepath.Join(root, "vendor/other/drop.go"), "package fixture\nfunc Dropped() {}\n")
			writeExcludeFixture(t, filepath.Join(root, ".gortex.yaml"), "include:\n  - '"+tc.include+"'\n")
			if strings.HasPrefix(tc.name, "basename") {
				rules := "Pods/\n"
				if tc.name == "basename_closed_ancestor" {
					rules += "ios/\n"
				}
				writeExcludeFixture(t, filepath.Join(root, ".gitignore"), rules)
				if tc.name == "basename_per_folder" {
					writeExcludeFixture(t, filepath.Join(root, "ios/.gortexignore"), "Pods/\n")
				}
			}
			if tc.name == "vendor_per_folder" {
				writeExcludeFixture(t, filepath.Join(root, ".gortexignore"), "vendor/\n")
			}
			cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
			require.NoError(t, err)
			cm.LoadWorkspaceConfig("fixture", root)
			cfg := cm.GetRepoConfig("fixture")
			idx := newAdmissionTestIndexer(t, root)
			// GetRepoConfig is the production config boundary.
			idx.config = cfg.Index
			_, err = idx.Index(root)
			require.NoError(t, err)
			require.Equal(t, tc.want, len(idx.graph.FindNodesByName("Kept")) > 0)
			require.Empty(t, idx.graph.FindNodesByName("Dropped"))
			require.Equal(t, tc.want, !mustIndexability(t, idx, tc.target).Skipped)
			w, err := NewWatcher(idx, cfg.Watch, zap.NewNop())
			require.NoError(t, err)
			require.Equal(t, tc.want, !w.isExcluded(filepath.Join(root, tc.target)))
			if tc.want {
				require.True(t, (&watcherExcludeFilter{w: w}).ShouldInclude(filepath.Join(root, tc.target)))
			}
		})
	}
}
