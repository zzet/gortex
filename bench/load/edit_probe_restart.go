package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The restart cell kills the owned daemon while the edit's ACK and its own
// receipt are pending and restarts it on the same store. Answers before the
// kill are kept apart; first-correct times come only from probes of the
// restarted daemon.
type editProbeRestartRecord struct {
	PendingBeforeKill  bool              `json:"receipt_pending_before_kill"`
	StatusBeforeKill   json.RawMessage   `json:"status_before_kill"`
	KillNS             *int64            `json:"kill_ns"`
	ReadyNS            *int64            `json:"ready_ns"`
	Downtime           Metric            `json:"downtime_ms"`
	StatusAfterRestart json.RawMessage   `json:"status_after_restart"`
	StatusAfterError   string            `json:"status_after_restart_error,omitempty"`
	BeforeKill         map[string]Metric `json:"first_correct_before_kill_ms"`
	FromReady          map[string]Metric `json:"first_correct_from_ready_ms"`
	DiskHasMarker      bool              `json:"disk_has_marker"`
	Qualification      string            `json:"qualification,omitempty"`
}

type editProbeRestarter interface {
	kill() error
	start(ctx context.Context) error // returns once the restarted daemon is ready
}

type editProbeDaemonRestarter struct{ p *privateRun }

func (d editProbeDaemonRestarter) kill() error {
	if d.p.process == nil {
		return errors.New("private daemon is not running")
	}
	err := killOwnedProcess(d.p.process)
	<-d.p.done
	d.p.process = nil
	return errors.Join(err, d.p.stdout.Close(), d.p.stderr.Close())
}

func (d editProbeDaemonRestarter) start(ctx context.Context) error {
	begin := time.Now()
	if err := d.p.start(); err != nil {
		return err
	}
	if err := d.p.awaitSocket(ctx); err != nil {
		return err
	}
	_, _, err := d.p.awaitReady(ctx, begin, d.p.Repos)
	return err
}

// The window between the edit's disk commit and its graph publication is
// short, so the cell polls its edit's status faster than the probe period.
const editProbeRestartPoll = 5 * time.Millisecond

// doRestartEdit sends the cell's one edit and kills the owned daemon while
// that edit's ACK is still outstanding: once a status read by its mutation_id
// shows the bytes committed and the graph receipt not yet terminal. The ACK
// itself waits for the receipt, so after the ACK the receipt is rarely still
// pending. The harness stops waiting for that ACK just before the kill.
func (r *editProbeRuntime) doRestartEdit(ctx context.Context, step editProbeStep) {
	s, callCtx, cancel, started := r.beginEdit(ctx, step)
	r.markPending(s)
	defer r.settlePending(s)
	defer s.signalAck()
	defer s.stopProbes()
	defer cancel()
	type reply struct {
		ack      json.RawMessage
		identity wireCallIdentity
		err      error
	}
	editCtx, abandon := context.WithCancel(callCtx)
	defer abandon()
	replies := make(chan reply, 1)
	go func() {
		ack, identity, err := r.perform(editCtx, s, r.editorFor(step), step.Request, "edit", "workload", 0)
		replies <- reply{ack, identity, err}
	}()
	restart := &editProbeRestartRecord{Downtime: unavailable("ms", "daemon not restarted"), BeforeKill: map[string]Metric{}, FromReady: map[string]Metric{}}
	statusReq := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"mutation_id": step.MutationID}}}
	readStatus := func(stream int) (json.RawMessage, map[string]any) {
		raw, _, err := r.perform(callCtx, s, s.client, statusReq, "status", "probe", stream)
		status, decodeErr := decodeEditProbeResult(raw)
		if err != nil || decodeErr != nil {
			return raw, nil
		}
		return raw, status
	}
	var edit reply
	answered := false
	for !answered && callCtx.Err() == nil {
		raw, status := readStatus(200)
		if status["disk_status"] == "committed" {
			restart.StatusBeforeKill = raw
			restart.PendingBeforeKill = status["graph_status_terminal"] == false
			break
		}
		select {
		case edit = <-replies:
			answered = true
		case <-time.After(editProbeRestartPoll):
		case <-callCtx.Done():
		}
	}
	if !answered {
		abandon()
		edit = <-replies
	}
	killedOutstanding := !answered && edit.err != nil && callCtx.Err() == nil
	if edit.err != nil && !killedOutstanding {
		s.mu.Lock()
		switch {
		case ctx.Err() != nil:
			s.record.Outcome = "phase_censored"
		case callCtx.Err() != nil:
			s.record.Outcome = "product_deadline"
		default:
			s.record.Outcome = "edit_error"
		}
		s.record.Errors = append(s.record.Errors, edit.err.Error())
		s.mu.Unlock()
		return
	}
	receipt := ""
	if edit.err == nil {
		body, err := decodeEditProbeOutput(edit.ack, editProbeRequestOutput(step.Request), r.plan.Tool)
		if err != nil {
			s.mu.Lock()
			s.record.Outcome = "invalid_ack"
			s.record.Errors = append(s.record.Errors, err.Error())
			s.mu.Unlock()
			return
		}
		receipt, _ = body["mutation_receipt"].(string)
		s.mu.Lock()
		s.record.Ack = measured(float64(edit.identity.Finished.Sub(started))/float64(time.Millisecond), "ms")
		s.acknowledged = edit.identity.Finished
		s.mu.Unlock()
		// The ACK came back first, so its receipt may already be terminal: the
		// status read just before the kill decides.
		raw, status := readStatus(201)
		restart.StatusBeforeKill = raw
		restart.PendingBeforeKill = status["graph_status_terminal"] == false
	} else {
		status, _ := decodeEditProbeResult(restart.StatusBeforeKill)
		receipt, _ = status["receipt"].(string)
		s.mu.Lock()
		s.record.Ack = unavailable("ms", "the daemon was killed while this edit's ACK was outstanding")
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.record.Receipt = receipt
	s.mu.Unlock()
	r.finishRestart(ctx, callCtx, s, step, started, restart, statusReq)
}

// finishRestart kills and restarts the daemon, reopens the sessions, then
// runs the immediate readbacks and resumed probes against the restarted
// daemon.
func (r *editProbeRuntime) finishRestart(ctx, callCtx context.Context, s *editProbeState, step editProbeStep, started time.Time, restart *editProbeRestartRecord, statusReq request) {
	var failure error
	s.mu.Lock()
	if s.record.Receipt == "" {
		failure = errors.New("restart cell observed no receipt for its committed edit")
	}
	s.mu.Unlock()
	var ready time.Time
	if failure == nil {
		if !restart.PendingBeforeKill {
			restart.Qualification = "receipt terminal before kill"
		}
		// No probe is sent to a dead socket; the loops resume once ready.
		s.stopProbes()
		s.mu.Lock()
		for name, value := range s.record.FirstCorrect {
			restart.BeforeKill[name] = value
			s.record.FirstCorrect[name] = unavailable("ms", "not observed after restart")
		}
		s.mu.Unlock()
		kill := time.Now()
		failure = r.restarter.kill()
		if failure == nil {
			failure = r.restarter.start(callCtx)
		}
		ready = time.Now()
		killNS := kill.Sub(r.origin).Nanoseconds()
		restart.KillNS = &killNS
		if failure == nil {
			readyNS := ready.Sub(r.origin).Nanoseconds()
			restart.ReadyNS = &readyNS
			restart.Downtime = measured(float64(ready.Sub(kill))/float64(time.Millisecond), "ms")
			failure = r.reopenSessions(callCtx, s)
		}
		if failure != nil {
			failure = errors.Join(errors.New("daemon restart failed"), failure)
		}
	}
	if failure == nil {
		raw, _, err := r.perform(callCtx, s, s.client, statusReq, "status", "probe", 202)
		restart.StatusAfterRestart = raw
		if err != nil {
			restart.StatusAfterError = err.Error()
		}
		probeCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.cancel = cancel
		s.mu.Unlock()
		r.probe(probeCtx, cancel, s)
		end, correct := r.immediateReadbacks(callCtx, s, step, []string{"file", "source"}, nil)
		if correct {
			s.mu.Lock()
			s.record.Readback = measured(float64(end.Sub(started))/float64(time.Millisecond), "ms")
			s.mu.Unlock()
		}
		for callCtx.Err() == nil && !s.firstCorrectComplete() {
			if waitContext(callCtx, editProbePeriod) != nil {
				break
			}
		}
		disk, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.plan.File)))
		restart.DiskHasMarker = err == nil && editProbeFixtureSource(r.plan.File, string(disk), step.Marker, r.plan.Callee)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	readyOffset := float64(ready.Sub(started)) / float64(time.Millisecond)
	for name, value := range s.record.FirstCorrect {
		restart.FromReady[name] = unavailable("ms", "daemon not restarted")
		if failure == nil {
			restart.FromReady[name] = value
		}
		if failure == nil && value.Value != nil {
			restart.FromReady[name] = measured(*value.Value-readyOffset, "ms")
		}
	}
	s.record.Restart = restart
	s.record.Accounting = editProbeAccounting{State: "not_applicable_restart"}
	s.record.PhasesMS = editProbePhases(nil, "the daemon was killed while this receipt was pending")
	s.record.Derived = unavailable("ms", "the daemon was killed while this receipt was pending")
	if failure != nil {
		s.record.Errors = append(s.record.Errors, failure.Error())
	}
	if restart.Qualification != "" {
		s.record.Errors = append(s.record.Errors, restart.Qualification)
	}
	if failure == nil && !restart.DiskHasMarker {
		s.record.Errors = append(s.record.Errors, "committed marker missing from disk after restart")
	}
	if s.firstProbeWrite.IsZero() || s.editWrite.IsZero() || !s.firstProbeWrite.Before(s.editWrite) {
		s.record.Errors = append(s.record.Errors, "no actual prober wire start before edit wire")
	}
	for name, value := range s.record.FirstCorrect {
		if value.Value == nil {
			s.record.Errors = append(s.record.Errors, "first-correct oracle unavailable: "+name)
		}
	}
	if s.record.StaleClaimedFresh {
		s.record.Errors = append(s.record.Errors, "stale answer explicitly claimed fresh after acknowledgement")
	}
	switch {
	case ctx.Err() != nil:
		s.record.Outcome = "phase_censored"
	case callCtx.Err() != nil:
		s.record.Outcome = "product_deadline"
	case len(s.record.Errors) == 0:
		s.record.Outcome = "complete"
	default:
		s.record.Outcome = "diagnostic_failure"
	}
}

// The killed daemon's sessions are dead. The edit's fixed editor and its
// prober reconnect to the restarted daemon; the runtime closes them.
func (r *editProbeRuntime) reopenSessions(ctx context.Context, s *editProbeState) error {
	editor, err := openMCP(ctx, r.p.Socket, r.p.Repos[0].Path)
	if err != nil {
		return err
	}
	prober, err := openMCP(ctx, r.p.Socket, r.p.Repos[0].Path)
	if err != nil {
		return errors.Join(err, editor.Close())
	}
	r.restarted = append(r.restarted, editor, prober)
	r.editors = []*socketClient{editor}
	s.mu.Lock()
	s.client = prober
	s.mu.Unlock()
	return nil
}
