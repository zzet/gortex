package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"time"
)

const foreignImpactDuration = 10 * time.Minute

// The driver must supply an independently evidenced offered-edit window.
// Timestamp membership here does not certify its traffic or the 600s gate.
type foreignImpactWindow struct {
	ID         string    `json:"id"`
	EditedRepo string    `json:"edited_repo"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
}

type foreignImpactSample struct {
	WindowID         string          `json:"window_id"`
	TargetRepo       string          `json:"target_repo"`
	EditedRepo       string          `json:"edited_repo"`
	TargetSymbol     string          `json:"target_symbol"`
	RPCID            int64           `json:"rpc_id"`
	Started          time.Time       `json:"started"`
	Finished         time.Time       `json:"finished"`
	WithinWindow     bool            `json:"within_window"`
	Latency          Metric          `json:"latency_ms"`
	GateWait         Metric          `json:"gate_wait_ms"`
	ResolveMutexWait Metric          `json:"resolve_mutex_wait_ms"`
	Refused          bool            `json:"refused"`
	Error            string          `json:"error,omitempty"`
	ErrorCode        *int            `json:"error_code,omitempty"`
	ErrorData        map[string]any  `json:"error_data,omitempty"`
	Raw              json.RawMessage `json:"raw"`
}

// observeForeignImpact uses a separate target-owned session and sends exactly
// one public impact RPC. It never retries or hides refusals behind a success.
func observeForeignImpact(ctx context.Context, client *socketClient, target, edited privateRepo, window foreignImpactWindow) foreignImpactSample {
	symbol := target.Prefix + "/" + target.Corpus.Symbol
	invalid := func(message string) foreignImpactSample {
		return foreignImpactObservation(window, target.Corpus.Label, edited.Corpus.Label, symbol, nil, wireCallIdentity{}, errors.New(message))
	}
	if client == nil || client.cwd == "" || target.Path == "" || filepath.Clean(client.cwd) != filepath.Clean(target.Path) {
		return invalid("foreign impact requires its target checkout's own query session")
	}
	if target.Prefix == "" || target.Corpus.Symbol == "" || target.Corpus.Label == "" || edited.Corpus.Label == "" || edited.Path == "" || target.Corpus.Label == edited.Corpus.Label || filepath.Clean(target.Path) == filepath.Clean(edited.Path) {
		return invalid("foreign impact requires distinct target and edited repositories")
	}
	if window.ID == "" || window.EditedRepo != edited.Corpus.Label || window.Start.IsZero() || !window.End.After(window.Start) {
		return invalid("foreign impact has no valid owned edit window")
	}
	args := map[string]any{"operation": "impact", "target": map[string]any{"symbol": symbol}, "output": map[string]any{"format": "json", "max_bytes": 0}}
	raw, identity, err := client.callWithIdentity(ctx, "tools/call", map[string]any{"name": "change", "arguments": args})
	return foreignImpactObservation(window, target.Corpus.Label, edited.Corpus.Label, symbol, raw, identity, err)
}

func foreignImpactObservation(window foreignImpactWindow, targetRepo, editedRepo, symbol string, raw json.RawMessage, identity wireCallIdentity, callErr error) foreignImpactSample {
	row := foreignImpactSample{WindowID: window.ID, TargetRepo: targetRepo, EditedRepo: editedRepo, TargetSymbol: symbol, RPCID: identity.ID, Started: identity.Started, Finished: identity.Finished, Raw: append(json.RawMessage(nil), raw...),
		Latency: unavailable("ms", "RPC timestamps unavailable"), GateWait: unavailable("ms", "ABSENT: no impact topology-gate wait producer"), ResolveMutexWait: unavailable("ms", "ABSENT: no impact resolve-mutex wait producer")}
	if identity.ID != 0 && !identity.Started.IsZero() && !identity.Finished.Before(identity.Started) {
		row.Latency = measured(float64(identity.Finished.Sub(identity.Started))/float64(time.Millisecond), "ms")
		row.WithinWindow = window.ID != "" && window.EditedRepo == editedRepo && targetRepo != "" && targetRepo != editedRepo && !window.Start.IsZero() && window.End.After(window.Start) && !identity.Started.Before(window.Start) && !identity.Finished.After(window.End)
	}
	var remote *rpcError
	row.Refused = errors.As(callErr, &remote)
	if remote != nil {
		code := remote.Code
		row.ErrorCode = &code
		row.ErrorData = copyRPCErrorData(remote.Data)
	}
	if len(raw) > 0 {
		var envelope struct {
			IsError bool           `json:"isError"`
			Meta    map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			callErr = errors.Join(callErr, err)
		} else {
			row.GateWait = foreignImpactWait(envelope.Meta, "gate_wait")
			row.ResolveMutexWait = foreignImpactWait(envelope.Meta, "resolve_mutex_wait")
			if envelope.IsError {
				row.Refused = true
				callErr = errors.Join(callErr, checkToolResult(raw))
			}
		}
	} else if callErr == nil {
		callErr = errors.New("impact RPC returned no raw response")
	}
	if callErr != nil {
		row.Error = callErr.Error()
	}
	return row
}

type foreignImpactPairSummary struct {
	TargetRepo string               `json:"target_repo"`
	EditedRepo string               `json:"edited_repo"`
	Summary    foreignImpactSummary `json:"summary"`
}

type foreignImpactSummary struct {
	Offered                     int                        `json:"offered"`
	Pairs                       []foreignImpactPairSummary `json:"pairs,omitempty"`
	WithinWindow                int                        `json:"within_window"`
	OutsideWindow               int                        `json:"outside_window"`
	Unavailable                 int                        `json:"unavailable"`
	Refusals                    int                        `json:"refusals"`
	Errors                      int                        `json:"errors"`
	Latency                     Distribution               `json:"latency_ms"`
	GateWait                    Distribution               `json:"gate_wait_ms"`
	ResolveMutexWait            Distribution               `json:"resolve_mutex_wait_ms"`
	GateWaitUnavailable         int                        `json:"gate_wait_unavailable"`
	ResolveMutexWaitUnavailable int                        `json:"resolve_mutex_wait_unavailable"`
}

// All offers remain counted. The window distribution includes failed and
// refused RPCs; unavailable timestamps remain explicit rather than zero.
func summarizeForeignImpact(rows []foreignImpactSample) foreignImpactSummary {
	out := summarizeForeignImpactRows(rows)
	grouped := make(map[string][]foreignImpactSample)
	for _, row := range rows {
		key := row.TargetRepo + "\x00" + row.EditedRepo
		grouped[key] = append(grouped[key], row)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		group := grouped[key]
		out.Pairs = append(out.Pairs, foreignImpactPairSummary{TargetRepo: group[0].TargetRepo, EditedRepo: group[0].EditedRepo, Summary: summarizeForeignImpactRows(group)})
	}
	return out
}

func summarizeForeignImpactRows(rows []foreignImpactSample) foreignImpactSummary {
	out := foreignImpactSummary{Offered: len(rows)}
	var values, gateWaits, resolveWaits []float64
	for _, row := range rows {
		if row.Error != "" {
			out.Errors++
		}
		if row.Refused {
			out.Refusals++
		}
		if row.Latency.Value == nil {
			out.Unavailable++
		}
		if row.WithinWindow {
			out.WithinWindow++
			if row.Latency.Value != nil {
				values = append(values, *row.Latency.Value)
			}
			if row.GateWait.Value != nil {
				gateWaits = append(gateWaits, *row.GateWait.Value)
			} else {
				out.GateWaitUnavailable++
			}
			if row.ResolveMutexWait.Value != nil {
				resolveWaits = append(resolveWaits, *row.ResolveMutexWait.Value)
			} else {
				out.ResolveMutexWaitUnavailable++
			}
		} else {
			out.OutsideWindow++
		}
	}
	out.Latency = distribution(values, "ms")
	out.GateWait = distribution(gateWaits, "ms")
	out.ResolveMutexWait = distribution(resolveWaits, "ms")
	return out
}

// Only a complete producer observation can supply a wait sample. Detached,
// omitted, malformed, and interrupted observations never become measured zero.
func foreignImpactWait(meta map[string]any, name string) Metric {
	waits, _ := meta["reach_coordination"].(map[string]any)
	phase, _ := waits[name].(map[string]any)
	if phase == nil {
		return unavailable("ms", "ABSENT: impact coordination producer omits "+name)
	}
	state, _ := phase["state"].(string)
	if state != "observed" {
		return unavailable("ms", "producer wait state "+state)
	}
	attempted, attemptedOK := phase["attempted"].(float64)
	acquired, acquiredOK := phase["acquired"].(float64)
	ms, msOK := phase["total_ms"].(float64)
	if !attemptedOK || !acquiredOK || attempted < 1 || attempted != math.Trunc(attempted) || acquired != attempted || !msOK || math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 {
		return unavailable("ms", "invalid or incomplete producer wait observation")
	}
	return measured(ms, "ms")
}

type foreignImpactTarget struct {
	Client *socketClient
	Repo   privateRepo
}

// runForeignImpactStream reuses the existing relation-prober cadence. It is a
// single query stream, never a new editor schedule. emit must retain every row;
// a persistence failure terminates evidence instead of hiding observations.
func runForeignImpactStream(ctx context.Context, targets []foreignImpactTarget, edited privateRepo, window foreignImpactWindow, emit func(foreignImpactSample) error) (foreignImpactSummary, error) {
	if len(targets) == 0 || emit == nil || window.Start.IsZero() || !window.End.After(window.Start) || window.End.Sub(window.Start) > foreignImpactDuration {
		return summarizeForeignImpact(nil), errors.New("foreign impact requires a bounded window and raw evidence sink")
	}
	if err := waitContext(ctx, time.Until(window.Start)); err != nil {
		return summarizeForeignImpact(nil), err
	}
	probeCtx, cancel := context.WithDeadline(ctx, window.End)
	defer cancel()
	ticker := time.NewTicker(editProbePeriod)
	defer ticker.Stop()
	// Only scalar samples are retained here; raw responses belong to emit.
	var rows []foreignImpactSample
	for time.Now().Before(window.End) && ctx.Err() == nil {
		target := targets[len(rows)%len(targets)]
		row := observeForeignImpact(probeCtx, target.Client, target.Repo, edited, window)
		retained := row
		row.Raw = nil
		row.ErrorData = nil
		row.ErrorCode = nil
		if row.Error != "" {
			row.Error = "observed product error; raw sink carries details"
		}
		row.Latency.Reason, row.GateWait.Reason, row.ResolveMutexWait.Reason = "", "", ""
		rows = append(rows, row)
		if err := emit(retained); err != nil {
			return summarizeForeignImpact(rows), err
		}
		if row.RPCID == 0 {
			return summarizeForeignImpact(rows), errors.New(row.Error)
		}
		select {
		case <-probeCtx.Done():
			if ctx.Err() != nil {
				return summarizeForeignImpact(rows), ctx.Err()
			}
			return summarizeForeignImpact(rows), nil
		case <-ticker.C:
		}
	}
	return summarizeForeignImpact(rows), ctx.Err()
}
