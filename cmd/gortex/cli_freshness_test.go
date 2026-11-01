package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestCLIRequireFreshPrintsLineAndExits(t *testing.T) {
	prior := cliRequireFresh
	cliRequireFresh = true
	t.Cleanup(func() { cliRequireFresh = prior })
	for _, nested := range []bool{false, true} {
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetErr(&stderr)
		body := []byte(`{"freshness_scope":"relations","graph_freshness":"pending","pending_file_count":2}`)
		if nested {
			body = append(append([]byte(`{"freshness":`), body...), '}')
		}
		err := checkCLIFreshness(cmd, body, nil)
		var exit *exitCodeError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 4, exit.ExitCode())
		require.Equal(t, "relations: pending (2 files)\n", stderr.String())
	}
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	err := checkCLIFreshness(cmd, nil, errors.New("require_fresh: relations: failed (1 files) after waiting 25 ms"))
	var exit *exitCodeError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 4, exit.ExitCode())
	require.Equal(t, "relations: failed (1 files)\n", stderr.String())
	require.Equal(t, true, applyCLIRequireFresh(nil)["require_fresh"])
}
