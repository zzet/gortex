package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInvalidEditAckConfigurationRefusesLoading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("edit:\n  ack: immediate\n"), 0o600))
	t.Run("workspace", func(t *testing.T) {
		_, err := Load(path)
		require.ErrorContains(t, err, "edit.ack")
	})
	t.Run("global", func(t *testing.T) {
		_, err := LoadGlobal(path)
		require.ErrorContains(t, err, "edit.ack")
	})
}
