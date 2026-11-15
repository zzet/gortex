package mcp

import (
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestSharedReceiptJSONUsesPrimaryContentBeforeSessionNotes(t *testing.T) {
	result := mcplib.NewToolResultText(`{"receipt":"own","complete":true}`)
	result.Content = append(result.Content, mcplib.NewTextContent(momentumNote(6)))
	body, err := sharedReceiptJSONBody(result)
	require.NoError(t, err)
	require.JSONEq(t, `"own"`, string(body["receipt"]))
	require.Len(t, result.Content, 2, "the original note must remain intact")
	for _, raw := range []string{`null`, `[]`, `{"receipt":`, `{"receipt":"own"} unexpected`} {
		bad := mcplib.NewToolResultText(raw)
		bad.Content = append(bad.Content, mcplib.NewTextContent(`{"receipt":"favorable second block"}`))
		_, err := sharedReceiptJSONBody(bad)
		require.Error(t, err, "never select a favorable later block: %s", raw)
	}
	failed := mcplib.NewToolResultError(`{"receipt":"own"}`)
	_, err = sharedReceiptJSONBody(failed)
	require.Error(t, err)
}

func TestSharedReceiptImpactKeepsExactOwnRecordAndGlobalPending(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"complete", `{"by_depth":{"1":[{"id":"own"}]},"complete":true,"truncated":false}`, true},
		{"later_pending", `{"by_depth":{"1":[{"id":"own"}]},"complete":false,"truncated":false,"graph_freshness":"pending"}`, true},
		{"pending_missing_own", `{"by_depth":{"1":[{"id":"other"}]},"complete":false,"truncated":false,"graph_freshness":"pending"}`, false},
		{"complete_missing_own", `{"by_depth":{"1":[{"id":"other"}]},"complete":true,"truncated":false}`, false},
		{"unknown_incomplete", `{"by_depth":{"1":[{"id":"own"}]},"complete":false,"truncated":false}`, false},
		{"truncated", `{"by_depth":{"1":[{"id":"own"}]},"complete":false,"truncated":true,"graph_freshness":"pending"}`, false},
		{"lower_bound", `{"by_depth":{"1":[{"id":"own"}]},"complete":false,"truncated":false,"lower_bound":true,"graph_freshness":"pending"}`, false},
		{"duplicate_own", `{"by_depth":{"1":[{"id":"own"},{"id":"own"}]},"complete":true,"truncated":false}`, false},
		{"malformed_metadata", `{"by_depth":{"1":[{"id":"own"}]},"complete":"true","truncated":false}`, false},
		{"null_complete", `{"by_depth":{"1":[{"id":"own"}]},"complete":null,"truncated":false,"graph_freshness":"pending"}`, false},
		{"null_truncated", `{"by_depth":{"1":[{"id":"own"}]},"complete":true,"truncated":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &body))
			err := sharedReceiptOwnImpact(body, "own")
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			reencoded, err := json.Marshal(body)
			require.NoError(t, err)
			require.JSONEq(t, tc.raw, string(reencoded), "global flags remain original")
		})
	}
}
