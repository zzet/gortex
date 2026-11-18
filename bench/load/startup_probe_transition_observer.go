package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// One boot owns this bounded observer. It retains native records, never an
// inferred member set or a registry. close joins the notification reader before
// the boot exposes the evidence to report serialization.
type startupTransitionObserver struct {
	mu     sync.Mutex
	e      *startupTransitionEvidence
	cancel context.CancelFunc
	done   chan struct{}
	calls  int
	bytes  int
}

func startStartupTransitionObserver(ctx context.Context, p *privateRun, primary privateRepo, started time.Time, timeout time.Duration) *startupTransitionObserver {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	e := &startupTransitionEvidence{PrimaryRoot: primary.Path, Prefix: primary.Prefix, BootStarted: started, PID: p.process.Process.Pid}
	if e.Prefix == "" {
		e.Prefix = primary.Corpus.Label
	}
	if owner, err := platform.CaptureProcessIdentity(e.PID); err == nil {
		e.ProcessBirthToken, _ = owner.Token()
	}
	o := &startupTransitionObserver{e: e, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(o.done)
		for ctx.Err() == nil {
			if o.registrationSession(ctx, p.Socket, primary.Path) {
				return
			}
			timer := time.NewTimer(startupWatchInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return o
}

func (o *startupTransitionObserver) close() { o.cancel(); <-o.done }

func (o *startupTransitionObserver) registrationSession(ctx context.Context, socket, cwd string) bool {
	callCtx, cancel := context.WithTimeout(ctx, startupCallTimeout)
	conn, reader, _, err := dialSocketSurface(callCtx, socket, cwd, "mcp", "core", "defer")
	cancel()
	if err != nil {
		return false
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c := &probeConn{conn: conn, reader: reader, notification: o.registrationFrame}
	callCtx, cancel = context.WithTimeout(ctx, startupCallTimeout)
	_, err = c.roundTrip(callCtx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "startup-registration-observer", "version": "1"}})
	cancel()
	if err != nil || c.notify("notifications/initialized") != nil {
		return false
	}
	callCtx, cancel = context.WithTimeout(ctx, startupCallTimeout)
	frame, err := c.roundTrip(callCtx, "tools/call", map[string]any{"name": "subscribe_workspace_readiness", "arguments": map[string]any{}})
	cancel()
	if err != nil || !decodeProbeFrame(frame).OK {
		return false
	}
	// The subscription outlives individual per-tick probe sessions. Repo-state
	// announcements are not replayed, so a missed event remains unknown.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return false
	}
	for ctx.Err() == nil {
		frame, err := readWireLine(reader)
		if err != nil {
			return ctx.Err() != nil
		}
		o.registrationFrame(frame)
	}
	return true
}

func (o *startupTransitionObserver) registrationFrame(frame []byte) {
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(frame, &envelope) != nil || envelope.Method != "notifications/workspace_readiness" {
		return
	}
	var event struct {
		Event string    `json:"event"`
		Root  string    `json:"repo_path"`
		Repo  string    `json:"repo"`
		State string    `json:"state"`
		Since time.Time `json:"repo_state_since"`
	}
	if json.Unmarshal(envelope.Params, &event) != nil || event.Event != "repo_state" || event.Root != o.e.PrimaryRoot || event.Repo != o.e.Prefix || event.State != "ready" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(envelope.Params) > 64<<10 || event.Since.IsZero() {
		o.e.InvalidReason = "native registration origin missing or oversized"
		return
	}
	if len(o.e.Registration) != 0 {
		var prior struct {
			Since time.Time `json:"repo_state_since"`
		}
		if json.Unmarshal(o.e.Registration, &prior) != nil || !prior.Since.Equal(event.Since) {
			o.e.InvalidReason = "native registration origin changed within boot"
		}
		return
	}
	o.e.Registration = append(json.RawMessage(nil), envelope.Params...)
	o.e.RegistrationPID, o.e.RegistrationBirth = o.e.PID, o.e.ProcessBirthToken
}

func (o *startupTransitionObserver) observeCatalog(ctx context.Context, p *privateRun) {
	o.mu.Lock()
	if o.calls >= 64 {
		o.e.InvalidReason = "transition observation limit exceeded"
	}
	if o.e.InvalidReason != "" {
		o.mu.Unlock()
		return
	}
	if len(o.e.Registration) > 0 {
		var event struct {
			Since time.Time `json:"repo_state_since"`
		}
		if json.Unmarshal(o.e.Registration, &event) == nil && !event.Since.IsZero() && time.Now().After(event.Since.Add(startupCoordinatorWindow)) {
			o.mu.Unlock()
			return
		}
	}
	o.calls++
	o.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	raw, err := control(callCtx, p.Socket, "list_checkouts", map[string]any{"family": o.e.PrimaryRoot})
	cancel()
	at := time.Now()
	if err != nil {
		o.mu.Lock()
		if len(o.e.Registration) > 0 {
			o.e.InvalidReason = "native catalog observation failed: " + clip(err.Error())
		}
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bytes += len(raw)
	if o.bytes > 8<<20 {
		o.e.InvalidReason = "transition evidence budget exceeded"
		return
	}
	if len(o.e.BaselineCatalog) == 0 {
		var catalog startupTransitionCatalog
		if json.Unmarshal(raw, &catalog) != nil {
			return
		}
		for _, family := range catalog.Families {
			primary, transitions := false, 0
			for _, row := range family.Checkouts {
				primary = primary || row.Root == o.e.PrimaryRoot
				if row.Transition != "" {
					transitions++
				}
			}
			if primary && transitions == 24 {
				o.e.FamilyID, o.e.BaselineAt = family.ID, at
				o.e.BaselineCatalog = append(json.RawMessage(nil), raw...)
				break
			}
		}
	}
	if len(o.e.Registration) > 0 {
		o.e.Observations = append(o.e.Observations, startupTransitionObservation{At: at, PID: o.e.PID, ProcessBirthToken: o.e.ProcessBirthToken, Catalog: append(json.RawMessage(nil), raw...)})
	}
}
