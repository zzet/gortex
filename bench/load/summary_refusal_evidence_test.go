package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFirstSummaryRefusalEvidenceFollowsOriginalWire(t *testing.T) {
	r := request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": map[string]any{"file": "reader.go"}}}
	planHash, err := hashJSON(r)
	require.NoError(t, err)
	args := dispatchArgs(r, privateRepo{Prefix: "selected"})
	root := t.TempDir()
	selectedCWD := filepath.Join(root, "repos", "selected")
	routeHash, err := summaryRefusalRouteHash(root, selectedCWD, args)
	require.NoError(t, err)
	selection := planHash + ":" + routeHash
	valid := json.RawMessage(`{"first_refusal":{"site":"retain","decision":"owner_before","attempt":1,"selected_source_seq":12,"current_source_seq":13,"unused_wire_note":"must not be retained"},"recovery_samples":[{"site":"recovery.entry_budget","decision":"expired","attempt":1,"original_remaining_ns":-5}],"overflow":false}`)
	for _, tc := range []struct {
		name      string
		selection string
		meta      json.RawMessage
		want      string
		wrongArgs bool
		wrongCWD  bool
		stopRetry bool
	}{
		{name: "default_off", meta: valid},
		{name: "different_request", selection: strings.Repeat("e", 64) + ":" + routeHash, meta: valid},
		{name: "first_exact_wire_only", selection: selection, meta: valid, want: "observed"},
		{name: "same_plan_wrong_prefix", selection: selection, meta: valid, want: "observed", wrongArgs: true},
		{name: "same_arguments_wrong_cwd", selection: selection, meta: valid, want: "observed", wrongCWD: true},
		{name: "canceled_backoff", selection: selection, meta: valid, want: "observed", stopRetry: true},
		{name: "missing_meta", selection: selection, want: "unavailable"},
		{name: "invalid_meta", selection: selection, meta: json.RawMessage(`{"first_refusal":{"site":"/unexpected/path","decision":"refused","attempt":1},"recovery_samples":[],"overflow":false}`), want: "invalid"},
		{name: "oversize_meta", selection: selection, meta: json.RawMessage(`{"unexpected":"` + strings.Repeat("x", 2200) + `"}`), want: "oversize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := withFirstSummaryRefusalEvidence(t.Context(), tc.selection)
			require.NoError(t, err)
			if selected := firstSummaryRefusalFrom(ctx); selected != nil {
				selected.root = root
			}
			var calls atomic.Int32
			client := searchSequenceClient(t, func(stubFrame) any {
				call := calls.Add(1)
				if call%2 == 0 {
					return stubContent(map[string]any{"nodes": []string{"actual retry response"}})
				}
				response := stubContent(map[string]any{"error_code": "projection_superseded", "message": "projection not published: superseded", "retriable": true, "data": map[string]any{"retry_scope": "read_only", "delivery_unknown": false}}).(map[string]any)
				response["isError"] = true
				metadata := tc.meta
				if call > 1 {
					metadata = valid // A later witness may not replace the first refusal.
				}
				if metadata != nil {
					response["_meta"] = map[string]any{"gortex_projection_predicate": metadata}
				}
				return response
			})
			client.cwd = selectedCWD
			wait := func(context.Context, time.Duration) error { return nil }
			if tc.wrongArgs || tc.wrongCWD {
				otherArgs := args
				if tc.wrongArgs {
					otherArgs = dispatchArgs(r, privateRepo{Prefix: "other"})
				} else {
					client.cwd = filepath.Join(root, "repos", "other")
				}
				_, attempts, err := callTool(ctx, client, r, otherArgs, "workload", 0, 0, 0, time.Second, wait)
				require.NoError(t, err)
				require.Len(t, attempts, 2)
				require.Equal(t, planHash, attempts[0].Request)
				require.Equal(t, "projection_superseded", attempts[0].ErrorData["error_code"])
				require.NotContains(t, attempts[0].ErrorData, "first_summary_projection_refusal")
				client.cwd = selectedCWD
			}
			if tc.stopRetry {
				wait = func(context.Context, time.Duration) error { return context.Canceled }
			}
			for call := 0; call < 2; call++ {
				_, attempts, err := callTool(ctx, client, r, args, "workload", 0, call, 0, time.Second, wait)
				require.Len(t, attempts, 2, "only the existing scoped retry is allowed")
				require.Equal(t, "retryable", attempts[0].Outcome)
				require.Equal(t, -32000, attempts[0].Code)
				require.Equal(t, "projection_superseded", attempts[0].ErrorData["error_code"])
				if tc.stopRetry {
					require.ErrorIs(t, err, context.Canceled)
					require.Zero(t, attempts[1].Code)
					require.ErrorContains(t, err, "canceled")
					require.Contains(t, attempts[1].Error, "busy backoff ended")
					want := copyRPCErrorData(attempts[0].ErrorData)
					delete(want, "first_summary_projection_refusal")
					require.Equal(t, want, attempts[1].ErrorData)
				} else {
					require.NoError(t, err)
					require.Equal(t, "success", attempts[1].Outcome)
					require.Empty(t, attempts[1].ErrorData)
				}
				evidence, present := attempts[0].ErrorData["first_summary_projection_refusal"]
				if call != 0 || tc.want == "" {
					require.False(t, present)
					continue
				}
				require.True(t, present)
				encoded, err := json.Marshal(evidence)
				require.NoError(t, err)
				if tc.want == "observed" {
					require.JSONEq(t, `{"state":"observed","route_sha256":"`+routeHash+`","first_refusal":{"site":"retain","decision":"owner_before","attempt":1,"selected_source_seq":12,"current_source_seq":13},"recovery_samples":[{"site":"recovery.entry_budget","decision":"expired","attempt":1,"original_remaining_ns":-5}]}`, string(encoded))
				} else {
					require.JSONEq(t, `{"state":"`+tc.want+`","route_sha256":"`+routeHash+`"}`, string(encoded))
				}
				if tc.stopRetry {
					break
				}
			}
			if tc.stopRetry {
				require.EqualValues(t, 1, calls.Load())
			} else if tc.wrongArgs || tc.wrongCWD {
				require.EqualValues(t, 6, calls.Load())
			} else {
				require.EqualValues(t, 4, calls.Load())
			}
		})
	}
}
