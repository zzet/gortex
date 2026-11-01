package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// A CLI probe is one owned `gortex call` process per oracle request. Its
// cadence is bounded by process start, daemon dial and exit, not the 25 ms
// probe period, and the process start stands in for the probe's wire start.
const editProbeCLIStderrLimit = 64 << 10

type editProbeCappedBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (b *editProbeCappedBuffer) Write(data []byte) (int, error) {
	if keep := b.limit - len(b.data); len(data) > keep {
		b.overflow = true
		b.data = append(b.data, data[:max(keep, 0)]...)
		return len(data), nil
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

// The edit ACK, readbacks and receipt status stay MCP JSON; only oracle
// requests go through the CLI, with the same lowered facade arguments.
func (r *editProbeRuntime) performCLI(ctx context.Context, s *editProbeState, req request, purpose string, stream int) (json.RawMessage, wireCallIdentity, error) {
	budget := min(r.options.Deadline, 20*time.Second)
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	args, err := json.Marshal(dispatchArgs(req, r.p.Repos[0]))
	if err != nil {
		return nil, wireCallIdentity{}, err
	}
	argv := []string{"--config", r.p.Config, "call", req.Tool, "--json", string(args), "--format", "json", "--index", r.p.Repos[0].Path, "--quiet"}
	cmd := exec.CommandContext(callCtx, r.p.Binary, argv...)
	configureOwnedProcess(cmd)
	cmd.Cancel = func() error { return killOwnedProcess(cmd) }
	cmd.WaitDelay = time.Second
	stdout := &editProbeCappedBuffer{limit: editProbeRawLimit}
	stderr := &editProbeCappedBuffer{limit: editProbeCLIStderrLimit}
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = r.p.Root, r.p.Env, stdout, stderr
	identity := wireCallIdentity{Started: time.Now()}
	identity.WriteStarted = identity.Started
	err = cmd.Start()
	if err == nil {
		s.mu.Lock()
		if s.firstProbeWrite.IsZero() || identity.WriteStarted.Before(s.firstProbeWrite) {
			s.firstProbeWrite = identity.WriteStarted
		}
		s.mu.Unlock()
		err = cmd.Wait()
		err = errors.Join(err, cleanupExitedOwnedProcess(cmd))
	}
	identity.Finished = time.Now()
	outcome := "success"
	switch {
	case ctx.Err() != nil:
		outcome, err = "phase_censored", errors.Join(ctx.Err(), err)
	case callCtx.Err() != nil:
		outcome, err = "product_deadline", errors.Join(callCtx.Err(), err)
	case stdout.overflow:
		outcome, err = "error", errors.Join(fmt.Errorf("%w: CLI stdout exceeded the per-edit evidence limit", errEditProbeEvidenceIncomplete), err)
	case err != nil:
		outcome, err = "error", fmt.Errorf("gortex call: %w: %s", err, stderr.data)
	}
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	hash, _ := hashJSON(req)
	attempt := Attempt{Tool: "cli:" + req.name(), Role: "probe", Request: hash, Step: s.step.Ordinal, Slot: stream, Started: identity.Started, DurationMS: float64(identity.Finished.Sub(identity.Started)) / float64(time.Millisecond), Outcome: outcome}
	attempt.LogicalMS = attempt.DurationMS
	if err != nil {
		attempt.Error = err.Error()
	}
	r.mu.Lock()
	r.phase.Attempts = append(r.phase.Attempts, attempt)
	r.mu.Unlock()
	row := map[string]any{"event": "cli", "purpose": purpose, "tool": req.name(), "argv": argv, "exit": exit, "stdout": string(stdout.data), "stderr": string(stderr.data), "stderr_truncated": stderr.overflow, "start_ns": editProbeOffset(identity.Started, r.origin), "end_ns": editProbeOffset(identity.Finished, r.origin), "output_mode": "cli"}
	if err != nil {
		row["error"] = err.Error()
	}
	if evidenceErr := s.emit(row); evidenceErr != nil {
		err = errors.Join(err, evidenceErr)
	}
	if err != nil {
		// A failed process gave no answer; its stdout stays in evidence only.
		return nil, identity, err
	}
	return stdout.data, identity, nil
}
