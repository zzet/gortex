package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type sourceReadinessEvidence struct {
	Attempts     int
	Refusals     int
	FirstOutcome string
	FirstRefusal string
}

// This is a fixture client, not a product retry or first-response oracle. Its
// caller supplies ONE existing absolute fixture deadline shared across values.
// Only the documented data-free read-only source deferral permits another call.
func sourceReadinessPending(result *mcplib.CallToolResult) bool {
	if !metadataTransientRefusal(result) || result.StructuredContent != nil || len(result.Content) != 1 {
		return false
	}
	text, ok := singleTextContent(result)
	if !ok {
		return false
	}
	var body StructuredError
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &body) != nil || json.Unmarshal([]byte(text), &fields) != nil {
		return false
	}
	for name := range fields {
		if name != "error_code" && name != "message" && name != "retriable" && name != "data" {
			return false
		}
	}
	if len(body.Data) != 3 {
		return false
	}
	return body.ErrorCode == ErrCodeFileViewPending && body.Data["reason"] == "projection_deferred"
}

func sourceWhenReady(ctx context.Context, invoke func(context.Context) (*mcplib.CallToolResult, error)) (*mcplib.CallToolResult, sourceReadinessEvidence, error) {
	evidence := sourceReadinessEvidence{FirstOutcome: "UNOBSERVED"}
	for {
		if err := ctx.Err(); err != nil {
			return nil, evidence, err
		}
		result, err := invoke(ctx)
		evidence.Attempts++
		if err != nil {
			if evidence.Attempts == 1 {
				evidence.FirstOutcome = "ERROR"
			}
			return nil, evidence, err
		}
		if result == nil {
			if evidence.Attempts == 1 {
				evidence.FirstOutcome = "NO_RESPONSE"
			}
			return nil, evidence, fmt.Errorf("source readiness returned no response")
		}
		pending := sourceReadinessPending(result)
		if evidence.Attempts == 1 {
			evidence.FirstOutcome = "NONERROR_REPLY"
			if result.IsError {
				evidence.FirstOutcome = "REFUSAL"
			}
		}
		if pending {
			evidence.Refusals++
			if evidence.FirstRefusal == "" {
				raw, marshalErr := json.Marshal(result)
				if marshalErr != nil {
					return nil, evidence, marshalErr
				}
				evidence.FirstRefusal = string(raw)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, evidence, err
		}
		if !result.IsError {
			return result, evidence, nil
		}
		if !pending {
			return nil, evidence, fmt.Errorf("source readiness refused: %s", resultText(result))
		}
		// Existing test polling cadence; this yields between public client
		// attempts and never creates or resets a request/fixture deadline.
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, evidence, ctx.Err()
		case <-timer.C:
		}
	}
}

func TestSourceReadinessRetriesOnlyExactDocumentedPending(t *testing.T) {
	for _, name := range []string{"documented", "other_code", "other_reason", "not_retriable", "unknown_delivery", "other_scope", "body", "extra_content", "structured_body", "go_error", "cancelled", "cancelled_positive", "deadline"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Unix(1, 0))
				defer stop()
			}
			pending := metadataTransientResult(ErrCodeFileViewPending, "source file view is pending for this read", "projection_deferred")
			if name != "documented" && name != "extra_content" && name != "structured_body" && name != "go_error" && name != "cancelled" && name != "cancelled_positive" && name != "deadline" {
				var body StructuredError
				require.NoError(t, json.Unmarshal([]byte(resultText(pending)), &body))
				switch name {
				case "other_code":
					body.ErrorCode = ErrCodeProjectionSuperseded
				case "other_reason":
					body.Data["reason"] = "store_busy"
				case "not_retriable":
					body.Retriable = false
				case "unknown_delivery":
					body.Data["delivery_unknown"] = true
				case "other_scope":
					body.Data["retry_scope"] = "write"
				case "body":
					body.Data["source"] = "unproved"
				}
				pending = NewStructuredErrorResult(body)
			}
			if name == "extra_content" {
				pending.Content = append(pending.Content, mcplib.NewTextContent("extra"))
			}
			if name == "structured_body" {
				pending.StructuredContent = map[string]any{"source": "unproved"}
			}
			calls := 0
			result, evidence, err := sourceWhenReady(ctx, func(context.Context) (*mcplib.CallToolResult, error) {
				calls++
				if name == "go_error" {
					return nil, fmt.Errorf("actual transport error")
				}
				if name == "cancelled" {
					cancel()
				}
				if name == "cancelled_positive" {
					cancel()
					return mcplib.NewToolResultText("late"), nil
				}
				if calls == 1 {
					return pending, nil
				}
				return mcplib.NewToolResultText(`{"source":"control-positive-not-a-real-source-proof"}`), nil
			})
			if name == "documented" {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, calls)
				require.Equal(t, 1, evidence.Refusals)
				require.Equal(t, "REFUSAL", evidence.FirstOutcome)
				require.NotEmpty(t, evidence.FirstRefusal)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				want := 1
				if name == "deadline" {
					want = 0
				}
				require.Equal(t, want, calls)
			}
		})
	}
}

func TestSourceReadinessNeverResetsSharedCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second) // existing source fixture allowance
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	for _, value := range []string{"return 1", "return 2"} {
		calls := 0
		_, evidence, err := sourceWhenReady(ctx, func(actual context.Context) (*mcplib.CallToolResult, error) {
			require.Equal(t, ctx, actual)
			got, known := actual.Deadline()
			require.True(t, known)
			require.Equal(t, deadline, got)
			calls++
			if calls == 1 {
				return metadataTransientResult(ErrCodeFileViewPending, "pending", "projection_deferred"), nil
			}
			return mcplib.NewToolResultText(value), nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, evidence.Attempts)
		require.Equal(t, "REFUSAL", evidence.FirstOutcome)
	}
}
