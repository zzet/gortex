package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The restart cell kills the owned daemon while the edit's own receipt is
// pending and restarts it on the same store. Answers before the kill are kept
// apart; first-correct times come only from probes of the restarted daemon.
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

// finishRestart runs after the edit's own ACK: one status read of its receipt,
// the kill, the restart, reopened sessions, then the immediate readbacks and
// resumed probes against the restarted daemon.
func (r *editProbeRuntime) finishRestart(ctx, callCtx context.Context, s *editProbeState, step editProbeStep, started time.Time, receipt string) {
	restart := &editProbeRestartRecord{Downtime: unavailable("ms", "daemon not restarted"), BeforeKill: map[string]Metric{}, FromReady: map[string]Metric{}}
	var failure error
	if receipt == "" {
		failure = errors.New("restart cell needs the edit's own receipt before the kill")
	}
	statusReq := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": receipt}}}
	var ready time.Time
	if failure == nil {
		raw, _, err := r.perform(callCtx, s, s.client, statusReq, "status", "probe", 200)
		restart.StatusBeforeKill = raw
		if status, decodeErr := decodeEditProbeResult(raw); err == nil && decodeErr == nil {
			restart.PendingBeforeKill = status["graph_status_terminal"] == false
		}
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
		raw, _, err := r.perform(callCtx, s, s.client, statusReq, "status", "probe", 201)
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
		s.record.Errors = append(s.record.Errors, "acknowledged marker missing from disk after restart")
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
