package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnknownWorkspaceKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gortex.yaml")

	// Missing file → no keys, never an error.
	assert.Nil(t, UnknownWorkspaceKeys(filepath.Join(dir, "absent.yaml")))

	// Malformed file → no keys; the parse error is reported through the
	// malformed-config warning channel, not here.
	require.NoError(t, os.WriteFile(path, []byte("exclude: [\"a/\n"), 0644))
	assert.Nil(t, UnknownWorkspaceKeys(path))

	// A fully valid file — including the deprecated legacy nesting —
	// stays silent.
	require.NoError(t, os.WriteFile(path, []byte(
		"exclude:\n  - \"a/**\"\nindex:\n  exclude:\n    - \"legacy/**\"\n  workers: 2\nrespect_gitignore: false\nworkspace: my-ws\n"), 0644))
	assert.Nil(t, UnknownWorkspaceKeys(path))
}

func TestUnknownWorkspaceKeys_FlagsTopLevelAndNestedTypos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gortex.yaml")

	// The exact trap this guards: index.ignore does not exist (the real key
	// is index.exclude) — and a wholly invented top-level block.
	require.NoError(t, os.WriteFile(path, []byte(
		"index:\n  ignore:\n    - \"build/\"\nbogus_section:\n  x: 1\n"), 0644))

	got := UnknownWorkspaceKeys(path)
	require.NotNil(t, got)
	assert.Contains(t, got, "index.ignore")
	assert.Contains(t, got, "bogus_section")
	assert.Len(t, got, 2)
}

func TestUnknownWorkspaceKeys_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gortex.yaml")
	require.NoError(t, os.WriteFile(path, []byte("# nothing here\n"), 0644))
	assert.Empty(t, UnknownWorkspaceKeys(path))
}
