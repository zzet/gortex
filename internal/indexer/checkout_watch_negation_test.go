package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
)

func TestCheckoutWatchPreservesDirectoryNegation(t *testing.T) {
	root := t.TempDir()
	filter := &checkoutWatchFilter{
		roots:    []string{root},
		excludes: excludes.New([]string{"gen/*", "!gen/keep/"}),
	}
	require.True(t, filter.ShouldInclude(filepath.Join(root, "gen/keep")), "backend must admit a reopened directory without knowing its type")
	require.True(t, filter.ShouldInclude(filepath.Join(root, "gen/keep/deep.go")))
	require.False(t, filter.ShouldInclude(filepath.Join(root, "gen/drop/deep.go")))
}

func TestCheckoutWatchPreservesIncludes(t *testing.T) {
	root := t.TempDir()
	filter := &checkoutWatchFilter{
		roots:    []string{root},
		excludes: excludes.NewWithIncludes(excludes.Builtin, []string{"vendor/acme/keep.go"}),
	}
	for rel, want := range map[string]bool{
		"vendor": true, "vendor/acme": true, "vendor/acme/keep.go": true,
		"vendor/other/drop.go": false, ".git/objects/data": false, "main.go": true,
	} {
		require.Equal(t, want, filter.ShouldInclude(filepath.Join(root, rel)), rel)
	}
}
