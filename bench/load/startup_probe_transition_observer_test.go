package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func startupRegistrationFrame(t *testing.T, evidence *startupTransitionEvidence, origin time.Time) []byte {
	t.Helper()
	frame, err := json.Marshal(map[string]any{"method": "notifications/workspace_readiness", "params": map[string]any{"event": "repo_state", "repo_path": evidence.PrimaryRoot, "repo": evidence.Prefix, "state": "ready", "repo_state_since": origin, "ts": origin.Add(time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestStartupRegistrationObserverUsesNativeOriginAndRejectsChangedOrigin(t *testing.T) {
	e := startupTransitionFixture(t)
	var event struct {
		Since time.Time `json:"repo_state_since"`
	}
	if err := json.Unmarshal(e.Registration, &event); err != nil {
		t.Fatal(err)
	}
	e.Registration = nil
	o := &startupTransitionObserver{e: e}
	o.registrationFrame(startupRegistrationFrame(t, e, event.Since))
	o.registrationFrame(startupRegistrationFrame(t, e, event.Since))
	if m := analyzeStartupTransitionCensus(e); m.Value == nil || *m.Value != 24 {
		t.Fatalf("native field was replaced by later announcement/poll time: %+v", m)
	}
	o.registrationFrame(startupRegistrationFrame(t, e, event.Since.Add(time.Second)))
	if m := analyzeStartupTransitionCensus(e); m.Value != nil {
		t.Fatalf("changed registration earned credit: %+v", m)
	}
}

func TestProbeRoundTripRetainsNativeReadinessNotificationBeforeResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	e := startupTransitionFixture(t)
	origin := e.BootStarted.Add(time.Second)
	e.Registration = nil
	o := &startupTransitionObserver{e: e}
	done := make(chan error, 1)
	go func() {
		var request map[string]any
		if err := json.NewDecoder(server).Decode(&request); err != nil {
			done <- err
			return
		}
		if _, err := fmt.Fprintln(server, string(startupRegistrationFrame(t, e, origin))); err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(server).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{}})
	}()
	c := &probeConn{conn: client, reader: bufio.NewReader(client), notification: o.registrationFrame}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.roundTrip(ctx, "tools/call", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m := analyzeStartupTransitionCensus(e); m.Value == nil || *m.Value != 24 {
		t.Fatalf("actual notification frame not retained: %+v", m)
	}
}

func TestStartupRegistrationObserverMissingOriginStaysUnknown(t *testing.T) {
	e := startupTransitionFixture(t)
	e.Registration = nil
	o := &startupTransitionObserver{e: e}
	o.registrationFrame(startupRegistrationFrame(t, e, time.Time{}))
	if m := analyzeStartupTransitionCensus(e); m.Value != nil {
		t.Fatalf("zero origin earned credit: %+v", m)
	}
}

func TestStartupTransitionObserverCapturesCatalogBeforeNativeOrigin(t *testing.T) {
	e := startupTransitionFixture(t)
	catalog := append(json.RawMessage(nil), e.BaselineCatalog...)
	e.BootStarted = time.Now()
	e.BaselineCatalog, e.Registration = nil, nil
	e.BaselineAt = time.Time{}
	e.Observations = nil
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Kind != "list_checkouts" {
			t.Errorf("unexpected native observation %s", frame.Kind)
		}
		return json.RawMessage(catalog), nil
	})
	o := &startupTransitionObserver{e: e}
	p := &privateRun{Socket: socket}
	o.observeCatalog(t.Context(), p)
	if len(e.BaselineCatalog) == 0 {
		t.Fatal("actual control response did not bind membership")
	}
	// Compare the wire clock (without Go monotonic metadata), exactly as the
	// analyzer does. A coarse-clock tie cannot establish strictly-before.
	origin := time.Now().UTC()
	o.registrationFrame(startupRegistrationFrame(t, e, origin))
	o.observeCatalog(t.Context(), p)
	m := analyzeStartupTransitionCensus(e)
	if !e.BaselineAt.Before(origin) {
		if m.Value != nil || m.Reason != "no exact membership captured before native registration" {
			t.Fatalf("coarse wire-clock tie must retain the strict ordering refusal: %+v", m)
		}
		return
	}
	if m.Value == nil || *m.Value != 24 {
		t.Fatalf("native control/notification sequence did not preserve proof: %+v", m)
	}
}

func TestStartupTransitionObserverLateCatalogCannotBecomeBaseline(t *testing.T) {
	e := startupTransitionFixture(t)
	catalog := append(json.RawMessage(nil), e.BaselineCatalog...)
	e.BootStarted = time.Now()
	e.BaselineCatalog, e.Registration = nil, nil
	e.BaselineAt = time.Time{}
	e.Observations = nil
	socket := startStub(t, func(stubFrame) (any, *rpcError) { return json.RawMessage(catalog), nil })
	o := &startupTransitionObserver{e: e}
	o.registrationFrame(startupRegistrationFrame(t, e, time.Now()))
	o.observeCatalog(t.Context(), &privateRun{Socket: socket})
	if m := analyzeStartupTransitionCensus(e); m.Value != nil {
		t.Fatalf("post-registration selection of members earned credit: %+v", m)
	}
}

func TestStartupTransitionObserverJoinsPersistentSubscriptionOnClose(t *testing.T) {
	var once sync.Once
	subscribed := make(chan struct{})
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Params["name"] == "subscribe_workspace_readiness" {
			once.Do(func() { close(subscribed) })
		}
		return stubContent(map[string]any{"status": "subscribed"}), nil
	})
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Socket: socket, process: &exec.Cmd{Process: process}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	o := startStartupTransitionObserver(ctx, p, privateRepo{Path: "/owned/primary", Prefix: "app"}, time.Now(), 3*time.Second)
	select {
	case <-subscribed:
	case <-ctx.Done():
		t.Fatal("persistent native subscription not established")
	}
	done := make(chan struct{})
	go func() { o.close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("notification observer did not join on close")
	}
}
