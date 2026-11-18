package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

type readbackReferenceCaller interface {
	callWithIdentity(context.Context, string, any) (json.RawMessage, wireCallIdentity, error)
}
type readbackReferenceRPC struct {
	Request        json.RawMessage `json:"request_params"`
	WriteStarted   time.Time       `json:"write_started"`
	WriteFinished  time.Time       `json:"write_finished"`
	ReplyDelivered time.Time       `json:"reply_delivered"`
	ID             int64           `json:"rpc_id"`
	Started        time.Time       `json:"started"`
	Finished       time.Time       `json:"finished"`
	Raw            json.RawMessage `json:"raw"`
	Error          string          `json:"error,omitempty"`
	Latency        Metric          `json:"latency_ms"`
	Correct        bool            `json:"correct"`
}
type readbackReferenceRecord struct {
	Ordinal              int                             `json:"ordinal"`
	Editor               int                             `json:"editor"`
	Session              string                          `json:"connection_identity"`
	ToolSurface          string                          `json:"requested_connection_surface"`
	CWD                  string                          `json:"cwd"`
	DueNS                int64                           `json:"due_ns"`
	StartLatenessNS      int64                           `json:"start_lateness_ns"`
	ProbeOrigin          time.Time                       `json:"probe_origin"`
	ACK                  readbackReferenceRPC            `json:"ack"`
	ObservedACKMode      string                          `json:"observed_ack_mode,omitempty"`
	ACKModeEvidence      string                          `json:"ack_mode_evidence"`
	Probes               map[string]readbackReferenceRPC `json:"probes"`
	PhysicalBytesCorrect bool                            `json:"physical_bytes_correct"`
	Correct              bool                            `json:"correct"`
	Errors               []string                        `json:"errors"`
}

func readbackReferenceRPCObservation(raw json.RawMessage, id wireCallIdentity, err error) readbackReferenceRPC {
	out := readbackReferenceRPC{ID: id.ID, Started: id.Started, Finished: id.Finished, WriteStarted: id.WriteStarted, WriteFinished: id.WriteFinished, ReplyDelivered: id.ReplyDelivered, Raw: append(json.RawMessage(nil), raw...), Latency: unavailable("ms", "no complete per-call wire identity")}
	if id.ID != 0 && !id.Started.IsZero() && !id.Finished.Before(id.Started) {
		out.Latency = measured(float64(id.Finished.Sub(id.Started))/float64(time.Millisecond), "ms")
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}
func readbackReferenceQuartetCorrect(probes map[string]readbackReferenceRPC) bool {
	for _, name := range []string{"source", "summary", "editing_context", "file"} {
		p, ok := probes[name]
		if !ok || !p.Correct || p.Error != "" || p.Latency.Value == nil {
			return false
		}
	}
	return true
}

// One client argument owns the mutation and the required four reads. No second prober,
// delay, retry, receipt query or disk read exists before the immediate burst.
func observeReadbackReference(ctx context.Context, client readbackReferenceCaller, repo privateRepo, trial readbackReferenceTrial, role string, deadline time.Duration) readbackReferenceRecord {
	return observeReadbackReferenceSurface(ctx, client, repo, trial, role, deadline, false)
}

type readbackReferenceACKDecoder func(map[string]any, privateRepo, readbackReferenceTrial) (string, string, error)

func observeReadbackReferenceSurface(ctx context.Context, client readbackReferenceCaller, repo privateRepo, trial readbackReferenceTrial, role string, deadline time.Duration, compatibility bool) readbackReferenceRecord {
	return observeReadbackReferenceSurfaceWithACKDecoder(ctx, client, repo, trial, role, deadline, compatibility, nil)
}

func observeReadbackReferenceSurfaceWithACKDecoder(ctx context.Context, client readbackReferenceCaller, repo privateRepo, trial readbackReferenceTrial, role string, deadline time.Duration, compatibility bool, decoder readbackReferenceACKDecoder) readbackReferenceRecord {
	out := readbackReferenceRecord{Ordinal: trial.Ordinal, Editor: trial.Editor, CWD: repo.Path, DueNS: trial.DueNS, Session: fmt.Sprintf("fixed-editor-%d", trial.Editor), Probes: map[string]readbackReferenceRPC{}, Errors: []string{}}
	out.ToolSurface = readbackReferenceFacadeSurface
	parameters := func(req request) (map[string]any, error) {
		return map[string]any{"name": req.Tool, "arguments": dispatchArgs(req, repo)}, nil
	}
	if compatibility {
		out.ToolSurface = readbackReferenceCompatibilitySurface
		parameters = func(req request) (map[string]any, error) { return readbackReferenceCompatibilityParams(req, repo) }
	}
	if role != "no_edit" && role != "under_edit" {
		out.Errors = append(out.Errors, "unknown readback role")
		return out
	}
	if role == "under_edit" {
		callCtx, cancel := context.WithTimeout(ctx, deadline)
		params, err := parameters(trial.Request)
		if err != nil {
			cancel()
			out.Errors = append(out.Errors, err.Error())
			return out
		}
		raw, id, err := client.callWithIdentity(callCtx, "tools/call", params)
		cancel()
		out.ACK = readbackReferenceRPCObservation(raw, id, err)
		out.ACK.Request, _ = json.Marshal(params)
		out.ProbeOrigin = time.Now()
	} else {
		out.ACK.Latency = unavailable("ms", "not applicable: no-edit baseline performs no mutation")
		out.ProbeOrigin = time.Now()
	}
	image := trial.After[trial.ProbeFile]
	image.Prefix = repo.Prefix
	requests := readbackReferenceProbeRequests(trial)
	type result struct {
		name        string
		observation readbackReferenceRPC
	}
	probeNames := []string{"source", "summary", "editing_context", "file"}
	if compatibility {
		probeNames = append(probeNames, "symbol")
	}
	done := make(chan result, len(probeNames))
	var workers sync.WaitGroup
	for _, name := range probeNames {
		name := name
		workers.Go(func() {
			req := requests[name]
			callCtx, cancel := context.WithTimeout(ctx, deadline)
			defer cancel()
			params, paramErr := parameters(req)
			if paramErr != nil {
				done <- result{name, readbackReferenceRPC{Error: paramErr.Error(), Latency: unavailable("ms", "request adapter refused before transport")}}
				return
			}
			raw, id, err := client.callWithIdentity(callCtx, "tools/call", params)
			observed := readbackReferenceRPCObservation(raw, id, err)
			observed.Request, _ = json.Marshal(params)
			// A late response or incomplete identity is not a fast successful read.
			if callCtx.Err() != nil {
				observed.Error = callCtx.Err().Error()
			}
			if observed.Error == "" && (id.Started.Before(out.ProbeOrigin) || id.ID == out.ACK.ID || observed.Latency.Value == nil) {
				observed.Error = "read has no distinct post-ACK wire identity"
			}
			observed.Correct = observed.Error == "" && readbackReferenceCorrect(name, raw, image, trial.Target, trial.Absent)
			done <- result{name, observed}
		})
	}
	workers.Wait()
	close(done)
	for row := range done {
		out.Probes[row.name] = row.observation
	}
	// The facade-v1 connection blocks the legacy get_symbol handler. Preserve
	// that required latency row as unavailable, never a source-call substitute.
	if !compatibility {
		out.Probes["symbol"] = readbackReferenceRPC{Error: "full get_symbol metadata requires a declared compatibility-surface cell", Latency: unavailable("ms", "not observed on facade-v1; source is not full metadata")}
	}
	seenIDs := map[int64]bool{}
	if out.ACK.ID != 0 {
		seenIDs[out.ACK.ID] = true
	}
	for name, probe := range out.Probes {
		if name == "symbol" && !compatibility {
			continue
		}
		if probe.ID == 0 || seenIDs[probe.ID] {
			probe.Error = "missing or repeated per-call wire identity"
			probe.Correct = false
			out.Probes[name] = probe
		}
		seenIDs[probe.ID] = true
	}
	if compatibility {
		metadata := out.Probes["symbol"]
		if !metadata.Correct || metadata.Error != "" || metadata.Latency.Value == nil {
			out.Errors = append(out.Errors, "full metadata endpoint did not return the exact symbol on this compatibility connection")
		}
	}
	if role == "under_edit" {
		ackErrorsBefore := len(out.Errors)
		body, isError, ok := readbackReferenceBody(out.ACK.Raw)
		if !ok || isError || out.ACK.Error != "" || out.ACK.ID == 0 || out.ACK.Latency.Value == nil {
			out.Errors = append(out.Errors, "mutation acknowledgement refused or unavailable")
		} else if decoder != nil {
			mode, evidence, decodeErr := decoder(body, repo, trial)
			out.ObservedACKMode, out.ACKModeEvidence = mode, evidence
			if decodeErr != nil {
				out.Errors = append(out.Errors, decodeErr.Error())
			}
		} else {
			mode, evidence, policyErr := readbackReferenceACKPolicy(body, trial)
			out.ObservedACKMode, out.ACKModeEvidence = mode, evidence
			if policyErr != nil {
				out.Errors = append(out.Errors, policyErr.Error())
			}
			if err := readbackReferenceACKSource(body, repo, trial); err != nil {
				out.Errors = append(out.Errors, err.Error())
			}
		}
		out.ACK.Correct = len(out.Errors) == ackErrorsBefore
	}
	out.Correct = readbackReferenceQuartetCorrect(out.Probes) && len(out.Errors) == 0
	return out
}

func readbackReferenceACKPolicy(body map[string]any, trial readbackReferenceTrial) (string, string, error) {
	if trial.Request.name() == "refactor.rename" {
		rows, ok := body["files"].([]any)
		if !ok || len(rows) != 1 {
			return "", "unavailable", errors.New("rename has no single original physical ACK policy")
		}
		var rowOK bool
		body, rowOK = rows[0].(map[string]any)
		if !rowOK {
			return "", "unavailable", errors.New("rename physical ACK policy row missing")
		}
	}
	mode, ok := body["ack_mode"].(string)
	if ok && (mode == "full" || mode == "local") {
		return mode, "direct original ACK", nil
	}
	if trial.Request.name() == "refactor.move" && body["ack_mode"] == nil {
		return "", "unavailable on original move ACK; configured policy retained separately", nil
	}
	return "", "unavailable", errors.New("original ACK policy missing or invalid")
}
func readbackReferenceACKSource(body map[string]any, repo privateRepo, trial readbackReferenceTrial) error {
	if trial.Request.name() == "edit.batch" {
		params, err := readbackReferenceCompatibilityParams(trial.Request, repo)
		if err != nil {
			return err
		}
		// This adapter validates and emits exactly one fixture edit. Bind the
		// original ACK to that exact qualified wire identity, never a suffix.
		args := params["arguments"].(map[string]any)
		edit := args["edits"].([]any)[0].(map[string]any)
		requestedPath := edit["path"].(string)
		image := trial.After[trial.ProbeFile]
		refs, err := decodeEditProbeBatchAcknowledgement(body, trial.TransactionID, filepath.Join(repo.Path, trial.ProbeFile), requestedPath, []byte(image.Bytes))
		if err == nil && refs.TransactionError != "" {
			return errors.New("durable batch reports an actual transaction/publication error")
		}
		return err
	}
	if trial.Request.name() == "refactor.move" {
		if body["status"] != "applied" || body["dry_run"] != false || body["same_package"] != true || body["target_id"] != repo.Prefix+"/"+trial.ProbeFile+"::"+trial.Target {
			return errors.New("move ACK does not identify the actual requested applied destination")
		}
		return nil
	}
	if trial.Request.name() == "refactor.rename" {
		rows, ok := body["files"].([]any)
		if body["status"] != "applied" || !ok || len(rows) != 1 {
			return errors.New("rename ACK is partial/provisional or not one actual physical file")
		}
		row, ok := rows[0].(map[string]any)
		if !ok {
			return errors.New("rename physical row missing")
		}
		body = row
	}
	if body["graph_status"] == "failed" || body["reindex_error"] != nil && body["reindex_error"] != "" {
		return errors.New("ACK reports actual background publication failure")
	}
	if body["ack_mode"] == "full" && body["graph_status"] != "fresh" {
		return errors.New("ordinary full ACK lacks actual completed graph publication")
	}
	image := trial.After[trial.ProbeFile]
	if body["disk_status"] != "committed" || body["content_sha256"] != readbackReferenceSHA(image.Bytes) {
		return errors.New("ACK does not prove the exact offered committed after-image")
	}
	return nil
}

// Offer times are fixed independently of service completion. A slow logical
// operation does not postpone the next offer, even on the same MCP connection.
func runReadbackReferenceOffers(ctx context.Context, origin time.Time, trials []readbackReferenceTrial, visit func(int, readbackReferenceTrial)) {
	var workers sync.WaitGroup
	for i, trial := range trials {
		i, trial := i, trial
		workers.Go(func() {
			if waitContext(ctx, time.Until(origin.Add(time.Duration(trial.DueNS)))) == nil {
				visit(i, trial)
			}
		})
	}
	workers.Wait()
}
