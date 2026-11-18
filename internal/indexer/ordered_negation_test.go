package indexer

import (
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

func TestPruneDir_OrderedNegations(t *testing.T) {
	// The second * closes agent/ again; descendant negations cannot reopen it.
	patterns := []string{"*", "!/agent/", "!/research/", "*", "!/agent/extensions/**", "!/agent/skills/**", "!/agent/AGENTS.md", ".venv/", "node_modules/"}
	for _, tc := range []struct {
		dir   string
		prune bool
	}{
		{"agent/session-artifacts", true}, {"research/x", true},
		{"agent/skills/x/.venv", true}, {"agent/extensions/x/node_modules", true},
		{"agent/skills/x", true}, {"agent/extensions/x", true},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			root := t.TempDir()
			idx := &Indexer{config: config.IndexConfig{Exclude: patterns}, logger: zap.NewNop()}
			if got := idx.shouldPruneDir(filepath.Join(root, tc.dir), root); got != tc.prune {
				t.Errorf("prune=%v, want %v", got, tc.prune)
			}
		})
	}
}
