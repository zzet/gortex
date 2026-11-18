package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func TestDispatcherUntrackedRecoveryNamesWorkingDirectoryRemedy(t *testing.T) {
	tracked, untracked := t.TempDir(), t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)
	sess := &daemon.Session{ID: "untracked_recovery", CWD: untracked}

	for _, tool := range []string{"explore", "search_symbols"} {
		t.Run(tool, func(t *testing.T) {
			// Repository arguments cannot bypass session admission. The recovery
			// must tell the client to change its MCP working directory instead.
			frame := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":%q,"arguments":{"path":%q,"repo":"intended"}}}`, tool, tracked)
			reply, err := d.Dispatch(context.Background(), sess, frame)
			require.NoError(t, err)
			var response struct {
				ID    int `json:"id"`
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Data    struct {
						ErrorCode  string `json:"error_code"`
						Path       string `json:"path"`
						Suggestion string `json:"suggestion"`
					} `json:"data"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(reply, &response))
			assert.Equal(t, 19, response.ID)
			assert.Equal(t, -32000, response.Error.Code)
			assert.Equal(t, "repo_not_tracked", response.Error.Data.ErrorCode)
			assert.Equal(t, untracked, response.Error.Data.Path, "retain the existing machine-readable attempted path")
			for _, text := range []string{response.Error.Message, response.Error.Data.Suggestion} {
				assert.Contains(t, text, "gortex repos")
				assert.Contains(t, text, "working directory")
				assert.NotContains(t, text, tracked)
				assert.Contains(t, text, untracked, "retain the attempted-path diagnostic")
				assert.NotContains(t, text, "path=")
				assert.NotContains(t, text, "repo=")
			}
			assert.Contains(t, response.Error.Data.Suggestion, "reconnect")
			assert.Contains(t, response.Error.Data.Suggestion, "gortex track "+untracked)
		})
	}
}
