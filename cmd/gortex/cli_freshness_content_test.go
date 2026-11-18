package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestCLIRequireFreshIgnoresRelationsLabelInsideSourceContent(t *testing.T) {
	prior := cliRequireFresh
	cliRequireFresh = true
	t.Cleanup(func() { cliRequireFresh = prior })
	body, err := json.Marshal(map[string]any{
		"path":    "example.go",
		"content": "package example\n// relations: pending (2 files)\nfunc Fresh() {}\n",
	})
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	require.NoError(t, checkCLIFreshness(cmd, body, nil), "source text is not freshness metadata")
	require.Empty(t, stderr.String(), "fresh source content must not produce a stale diagnostic")
	require.False(t, cmd.SilenceErrors)
}
