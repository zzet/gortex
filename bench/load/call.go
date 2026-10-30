package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func checkToolResult(data json.RawMessage) error {
	var envelope struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if !envelope.IsError {
		return nil
	}
	payload, err := textPayload(data, "content")
	if err != nil {
		var plain struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &plain) == nil {
			var messages []string
			for _, content := range plain.Content {
				if content.Text != "" {
					messages = append(messages, content.Text)
				}
			}
			if len(messages) > 0 {
				return &rpcError{Code: -32000, Message: strings.Join(messages, "\n")}
			}
		}
		return errors.New("tool returned isError without error content")
	}
	var body struct {
		Code      string         `json:"error_code"`
		Message   string         `json:"message"`
		Retriable bool           `json:"retriable"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return err
	}
	code := -32000
	if body.Code == "invalid_argument" {
		code = -32602
	}
	return &rpcError{Code: code, Message: body.Code + ": " + body.Message, Data: body.Data, Retriable: body.Code == "repo_loading" && body.Retriable}
}

type toolCaller interface {
	call(context.Context, string, any) (json.RawMessage, error)
}

// Each logical call has its own product budget. The phase context may end
// sooner; that observation is right-censored, never a product deadline.
func callTool(ctx context.Context, client toolCaller, r request, args map[string]any, role string, agent, step, slot int, timeout time.Duration, sleep func(context.Context, time.Duration) error) (json.RawMessage, []Attempt, error) {
	return callToolPolicy(ctx, client, r, args, role, agent, step, slot, timeout, sleep, false)
}

func callEditProbeTool(ctx context.Context, client toolCaller, r request, args map[string]any, role string, agent, step, slot int, timeout time.Duration, sleep func(context.Context, time.Duration) error) (json.RawMessage, []Attempt, error) {
	return callToolPolicy(ctx, client, r, args, role, agent, step, slot, timeout, sleep, true)
}

func callToolPolicy(ctx context.Context, client toolCaller, r request, args map[string]any, role string, agent, step, slot int, timeout time.Duration, sleep func(context.Context, time.Duration) error, retryTyped bool) (json.RawMessage, []Attempt, error) {
	logicalStart := time.Now()
	productEnd := logicalStart.Add(timeout)
	phaseEnd, phaseBounded := ctx.Deadline()
	phaseClipsProduct := phaseBounded && phaseEnd.Before(productEnd)
	terminalOutcome := func() string {
		if phaseClipsProduct || ctx.Err() == context.Canceled {
			return "phase_censored"
		}
		return "product_deadline"
	}
	callCtx, cancel := context.WithDeadline(ctx, productEnd)
	defer cancel()
	hash, err := hashJSON(r)
	if err != nil {
		return nil, nil, err
	}
	var attempts []Attempt
	for retry := 0; ; retry++ {
		start := time.Now()
		data, err := client.call(callCtx, "tools/call", map[string]any{"name": r.Tool, "arguments": args})
		returned := time.Now()
		if retryTyped {
			if clock, ok := client.(interface{ responseReturnedAt() time.Time }); ok {
				if observed := clock.responseReturnedAt(); !observed.IsZero() && !observed.Before(start) {
					returned = observed
				}
			}
		}
		if err == nil && callCtx.Err() != nil {
			err = callCtx.Err()
		}
		if err == nil {
			err = checkToolResult(data)
		}
		a := Attempt{Tool: r.name(), Role: role, Request: hash, Agent: agent, Step: step, Slot: slot, Retry: retry, Started: start, DurationMS: float64(returned.Sub(start)) / float64(time.Millisecond), LogicalMS: float64(returned.Sub(logicalStart)) / float64(time.Millisecond), Outcome: "success"}
		var remote *rpcError
		if err != nil {
			a.Outcome, a.Error = "error", err.Error()
			if errors.As(err, &remote) {
				a.Code = remote.Code
			}
			if remote != nil && remote.Code == -32002 && (!retryTyped || remote.retryEditProbe()) {
				a.Outcome = "busy"
			} else if retryTyped && remote != nil && remote.retryEditProbe() {
				a.Outcome = "retryable"
			}
			if _, expected := err.(*editProbeExpectedAbsence); expected {
				a.Outcome = "expected_refusal"
				a.Code = -32000
				a.ExpectedOutcome = "symbol_absent"
			}
			if callCtx.Err() != nil {
				a.Outcome = terminalOutcome()
				err = errors.Join(err, callCtx.Err())
			}
			if retryTyped && errors.Is(err, errEditProbeEvidenceIncomplete) {
				a.Outcome = "harness_incomplete"
			}
		}
		attempts = append(attempts, a)
		if a.Outcome != "busy" && a.Outcome != "retryable" {
			return data, attempts, err
		}
		if err := sleep(callCtx, busyBackoff(retry)); err != nil {
			terminal := a
			terminal.Retry++
			terminal.Started = time.Now()
			terminal.DurationMS = 0
			terminal.LogicalMS = float64(time.Since(logicalStart)) / float64(time.Millisecond)
			terminal.Error = fmt.Sprintf("busy backoff ended: %v", err)
			terminal.Code = 0
			terminal.Outcome = terminalOutcome()
			attempts = append(attempts, terminal)
			return nil, attempts, err
		}
	}
}
