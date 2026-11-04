package main

import (
	"encoding/json"
	"errors"
	"time"
)

type readFreshnessKey struct {
	cwd, request, tool string
	agent, step, slot  int
}

func scopedReadTool(tool string) bool {
	return tool == "read.summary" || tool == "read.editing_context"
}

func readFreshnessKeyOf(a attempt) (readFreshnessKey, bool) {
	if a.Role != "workload" || !scopedReadTool(a.Tool) || a.CWD == "" || !planPattern.MatchString(a.Request) || a.Agent == nil || a.Step == nil || a.Slot == nil || a.Retry == nil {
		return readFreshnessKey{}, false
	}
	return readFreshnessKey{a.CWD, a.Request, a.Tool, *a.Agent, *a.Step, *a.Slot}, true
}

func explicitReadFreshness(a attempt) bool {
	code, _ := a.ErrorData["error_code"].(string)
	unknown, known := a.ErrorData["delivery_unknown"].(bool)
	return a.Role == "workload" && scopedReadTool(a.Tool) && a.Outcome == "retryable" && a.Code != nil && *a.Code == -32000 && a.Error != "" && (code == "projection_superseded" || code == "file_view_pending") && a.ErrorData["retriable"] == true && a.ErrorData["retry_scope"] == "read_only" && known && !unknown
}

// A refused first response remains refused. Only the separately retained final
// success completes its logical call; all refusals and their total cost remain.
func validateReadFreshnessGroups(r report) (map[int]bool, error) {
	allowed := make(map[int]bool)
	attempts := r.Phases[0].Attempts
	keys := make(map[readFreshnessKey]bool)
	for _, a := range attempts {
		if a.Outcome != "retryable" && (a.Role != "workload" || !scopedReadTool(a.Tool) || a.Retry == nil || *a.Retry == 0) {
			continue
		}
		key, valid := readFreshnessKeyOf(a)
		if !valid {
			return nil, errors.New("retry lacks complete workload read identity; probes and writes stay first-response strict")
		}
		keys[key] = true
	}
	refusals := make(map[string]int)
	for tool, metrics := range r.Phases[0].Tools {
		if scopedReadTool(tool) && metrics.RetryableRefusals != nil && *metrics.RetryableRefusals != 0 {
			refusals[tool] = 0
		}
	}
	if len(keys) == 0 {
		if len(refusals) != 0 {
			return nil, errors.New("declared read refusal has no retained original attempt")
		}
		return allowed, nil
	}
	var workload struct {
		Deadline float64 `json:"product_deadline_seconds"`
	}
	if json.Unmarshal(r.Workload, &workload) != nil || !finiteNonnegative(workload.Deadline) || workload.Deadline <= 0 {
		return nil, errors.New("retry lacks original logical deadline")
	}
	for key := range keys {
		var group []int
		for index, a := range attempts {
			candidate, ok := readFreshnessKeyOf(a)
			if ok && candidate == key {
				group = append(group, index)
			}
		}
		if len(group) < 2 {
			return nil, errors.New("retry missing its original refusal or terminal success")
		}
		fresh := 0
		previous := float64(0)
		firstStarted := attempts[group[0]].Started
		var previousStarted time.Time
		var previousDuration float64
		for retry, index := range group {
			a := attempts[index]
			if *a.Retry != retry || a.DurationMS == nil || !finiteNonnegative(*a.DurationMS) || a.LogicalMS == nil || !finiteNonnegative(*a.LogicalMS) || *a.LogicalMS < previous+*a.DurationMS || *a.LogicalMS > workload.Deadline*1000 {
				return nil, errors.New("retry sequence/cumulative cost exceeds or resets original logical budget")
			}
			// Serialized Started retains nanosecond wall-clock precision. A
			// missing/backwards/overlapping clock cannot certify sequential
			// retry work, so refuse it rather than estimate or reset cost.
			// LogicalMS remains the producer's original monotonic elapsed time.
			if firstStarted.IsZero() || a.Started.IsZero() || a.Started.Before(firstStarted) ||
				retry > 0 && float64(a.Started.Sub(previousStarted))/float64(time.Millisecond) < previousDuration ||
				*a.LogicalMS < float64(a.Started.Sub(firstStarted))/float64(time.Millisecond)+*a.DurationMS {
				return nil, errors.New("retry timestamps omit original attempt/backoff cost or are not comparable")
			}
			previousStarted, previousDuration = a.Started, *a.DurationMS
			previous = *a.LogicalMS
			if retry == len(group)-1 {
				if a.Outcome != "success" || a.Code == nil || *a.Code != 0 || a.Error != "" {
					return nil, errors.New("retry has no correct terminal-success status; censor/error/refusal remains failure")
				}
				continue
			}
			if a.Outcome == "busy" && a.Code != nil && *a.Code == -32002 {
				continue
			}
			if !explicitReadFreshness(a) {
				return nil, errors.New("unknown/generic or unscoped read refusal cannot be retried")
			}
			fresh++
			if fresh > 2 {
				return nil, errors.New("read freshness retry cap exceeded")
			}
			refusals[a.Tool]++
			allowed[index] = true
		}
	}
	for tool, count := range refusals {
		metrics, ok := r.Phases[0].Tools[tool]
		successes := 0
		for _, a := range attempts {
			if a.Tool == tool && a.Role == "workload" && a.Outcome == "success" {
				successes++
			}
		}
		if !ok || metrics.RetryableRefusals == nil || *metrics.RetryableRefusals != count || metrics.Successes == nil || *metrics.Successes != successes {
			return nil, errors.New("read refusal or logical-success counters differ from retained original attempts")
		}
	}
	return allowed, nil
}
