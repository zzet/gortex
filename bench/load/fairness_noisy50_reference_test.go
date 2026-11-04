//go:build acceptance

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

const fairnessNoisy50Protocol = "fair-solo-noisy50-v1"

// These are observations from the existing socket SDK, not generated session
// names. Client RPC spans and write-to-reply spans are retained separately.
type fairnessNoisy50Wire struct {
	Role            string `json:"role"`
	SessionID       string `json:"session_id"`
	DaemonInstance  string `json:"daemon_instance"`
	Slot            int    `json:"slot"`
	Step            int    `json:"step"`
	Retry           int    `json:"retry"`
	RPCID           int64  `json:"rpc_id"`
	StartedNS       int64  `json:"started_ns"`
	WriteStartedNS  int64  `json:"write_started_ns"`
	WriteFinishedNS int64  `json:"write_finished_ns"`
	ReplyNS         int64  `json:"reply_ns"`
	FinishedNS      int64  `json:"finished_ns"`
}

// Only answered SDK calls prove RPC identity and observed invocation overlap.
// This is client outstanding-call concurrency, not 50 server execution slots.
func fairnessNoisy50Topology(rows []fairnessNoisy50Wire, readerSession, noisySession, instance string) error {
	if readerSession == "" || noisySession == "" || readerSession == noisySession || instance == "" {
		return errors.New("missing or shared actual session identity")
	}
	slots := map[int]bool{}
	perSlot := map[int][]fairnessNoisy50Wire{}
	ids := map[string]bool{}
	type event struct {
		at    int64
		delta int
	}
	var events []event
	readers := 0
	for _, row := range rows {
		if row.DaemonInstance != instance || row.RPCID <= 0 || row.StartedNS <= 0 || row.WriteStartedNS < row.StartedNS || row.WriteFinishedNS < row.WriteStartedNS || row.ReplyNS < row.WriteStartedNS || row.FinishedNS < row.ReplyNS || row.FinishedNS < row.WriteFinishedNS {
			return errors.New("incomplete or inconsistent actual SDK/RPC observation")
		}
		key := fmt.Sprintf("%s/%d", row.SessionID, row.RPCID)
		if ids[key] {
			return errors.New("duplicate actual session/RPC identity")
		}
		ids[key] = true
		switch row.Role {
		case "noisy":
			if row.SessionID != noisySession || row.Slot < 0 || row.Slot >= 50 {
				return errors.New("noisy worker did not use the one real noisy session")
			}
			slots[row.Slot] = true
			perSlot[row.Slot] = append(perSlot[row.Slot], row)
			events = append(events, event{row.StartedNS, 1}, event{row.FinishedNS, -1})
		case "reader":
			if row.SessionID != readerSession {
				return errors.New("reader borrowed another connection")
			}
			readers++
		default:
			return errors.New("unknown measured role")
		}
	}
	for _, calls := range perSlot {
		sort.Slice(calls, func(i, j int) bool { return calls[i].StartedNS < calls[j].StartedNS })
		for i := 1; i < len(calls); i++ {
			if calls[i].StartedNS < calls[i-1].FinishedNS {
				return errors.New("one noisy worker issued concurrent calls")
			}
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at != events[j].at {
			return events[i].at < events[j].at
		}
		return events[i].delta < events[j].delta // Half-open spans: ends first.
	})
	active, peak := 0, 0
	for _, e := range events {
		active += e.delta
		peak = max(peak, active)
	}
	if len(slots) != 50 || peak != 50 || readers == 0 {
		return errors.New("literal noisy50 treatment not demonstrated by actual RPC spans")
	}
	return nil
}

// No default test run may start a daemon. Native/host/source bindings and a
// reviewed one-shot outer admission remain separate prerequisites.
func TestFairnessNoisy50ReferenceCollector(t *testing.T) {
	path := os.Getenv("GORTEX_FAIRNESS_REFERENCE_PLAN")
	if path == "" {
		t.Skip("explicit pinned fairness plan and A admission required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	// This opt-in prevents accidental test execution; it is NOT admission
	// authority. The reviewed outer must hold A's exact one-shot grant/lease.
	if os.Getenv("GORTEX_FAIRNESS_EXECUTION_PLAN_SHA256") != hex.EncodeToString(sum[:]) {
		t.Fatal("exact admitted plan opt-in missing")
	}
	var plan fairnessNoisy50Plan
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&plan); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		t.Fatal("one plan object required")
	}
	if err := plan.validate(); err != nil {
		t.Fatal(err)
	}
	if err := fairnessNoisy50Collect(t.Context(), plan, hex.EncodeToString(sum[:]), t.Logf); err != nil {
		t.Fatal(err)
	}
}

type fairnessNoisy50Plan struct {
	Protocol           string            `json:"protocol"`
	Ready              bool              `json:"ready"`
	Repetition         int               `json:"repetition"`
	Seed               uint64            `json:"seed"`
	Binary             string            `json:"binary"`
	Manifest           string            `json:"manifest"`
	HostReceipt        string            `json:"host_receipt"`
	Scratch            string            `json:"scratch"`
	Output             string            `json:"output"`
	MeasurementSeconds int               `json:"measurement_seconds"`
	NoisyWorkers       int               `json:"noisy_workers"`
	Bindings           map[string]string `json:"bindings"`
}

func (p fairnessNoisy50Plan) validate() error {
	if !p.Ready || p.Protocol != fairnessNoisy50Protocol || p.Repetition < 1 || p.Repetition > 2 || p.Seed == 0 || p.MeasurementSeconds != 120 || p.NoisyWorkers != 50 {
		return errors.New("unbound or changed literal fairness cell")
	}
	for _, key := range []string{"product_sha256", "collector_sha256", "fixture_sha256", "host_receipt_sha256"} {
		v := p.Bindings[key]
		if len(v) != 64 {
			return fmt.Errorf("unbound %s", key)
		}
		if _, err := hex.DecodeString(v); err != nil {
			return err
		}
	}
	for _, path := range []string{p.Binary, p.Manifest, p.HostReceipt, p.Scratch, p.Output} {
		if !filepath.IsAbs(path) {
			return errors.New("absolute owned artifact/input paths required")
		}
	}
	return nil
}

type fairnessNoisy50Connection struct {
	client    *socketClient
	Role      string          `json:"role"`
	SessionID string          `json:"session_id"`
	Instance  string          `json:"daemon_instance"`
	Ack       json.RawMessage `json:"handshake_ack"`
}

// Retain the real ACK while using the existing SDK reader, multiplexing,
// cancellation and per-call identity methods unchanged. One socket per role.
func openFairnessNoisy50Connection(ctx context.Context, socket, cwd, role string) (*fairnessNoisy50Connection, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	hello := daemon.Handshake{Version: daemon.ProtocolVersion, Mode: "mcp", CWD: cwd, ClientName: "gortex-load", PID: os.Getpid(), Tools: "facade-v1", ToolsMode: "hide"}
	if err := json.NewEncoder(conn).Encode(hello); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	raw, err := readWireLine(reader)
	var ack daemon.HandshakeAck
	if err == nil {
		err = json.Unmarshal(raw, &ack)
	}
	if err == nil && (!ack.OK || ack.SessionID == "" || ack.DaemonInstance == "") {
		err = errors.New("real daemon ACK session/incarnation unavailable")
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := &socketClient{conn: conn, reader: reader, pending: make(map[int64]chan wireReply), done: make(chan struct{}), version: ack.DaemonVersion, cwd: cwd, writeGate: make(chan struct{}, 1)}
	go c.readLoop()
	if _, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "gortex-load", "version": "1"}}); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, false); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &fairnessNoisy50Connection{client: c, Role: role, SessionID: ack.SessionID, Instance: ack.DaemonInstance, Ack: raw}, nil
}

type fairnessNoisy50ObservedCall struct {
	data  json.RawMessage
	err   error
	clock wireCallIdentity
}

type fairnessNoisy50Caller struct {
	connection *fairnessNoisy50Connection
	calls      []fairnessNoisy50ObservedCall
}

func (c *fairnessNoisy50Caller) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, clock, err := c.connection.client.callWithIdentity(ctx, method, params)
	// No hashing, JSON serialization, file I/O or shared writer lock occurs
	// inside the existing logical-call clock. Preserve the actual SDK spans.
	c.calls = append(c.calls, fairnessNoisy50ObservedCall{data, err, clock})
	return data, err
}

type fairnessNoisy50Sink struct {
	mu               sync.Mutex
	attempts, status *os.File
	err              error
}

func (s *fairnessNoisy50Sink) write(file *os.File, row any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = json.NewEncoder(file).Encode(row)
	}
	return s.err
}

func fairnessNoisy50Offset(origin, value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Sub(origin).Nanoseconds()
}

func fairnessNoisy50Call(ctx context.Context, connection *fairnessNoisy50Connection, repo privateRepo, req request, phase string, slot, step int, origin time.Time, sink *fairnessNoisy50Sink) error {
	caller := &fairnessNoisy50Caller{connection: connection}
	logical := time.Now()
	// Timer delivery can lag the deadline. Do not invent a new offered call
	// beyond the exact measured window, even while ctx.Err is still nil.
	if ctx.Err() != nil {
		return nil
	}
	if end, ok := ctx.Deadline(); ok && !logical.Before(end) {
		return nil
	}
	_, attempts, callErr := callTool(ctx, caller, req, dispatchArgs(req, repo), connection.Role, 0, step, slot, 120*time.Second, waitContext)
	if len(attempts) == 0 {
		return errors.Join(errors.New("logical call has no terminal evidence"), callErr)
	}
	for i, attempt := range attempts {
		attempt.CWD = repo.Path
		row := map[string]any{"phase": phase, "session_id": connection.SessionID, "daemon_instance": connection.Instance, "logical_started_ns": fairnessNoisy50Offset(origin, logical), "attempt": attempt, "wire": nil}
		if i < len(caller.calls) {
			call := caller.calls[i]
			clock := call.clock
			row["wire"] = fairnessNoisy50Wire{Role: connection.Role, SessionID: connection.SessionID, DaemonInstance: connection.Instance, Slot: slot, Step: step, Retry: i, RPCID: clock.ID, StartedNS: fairnessNoisy50Offset(origin, clock.Started), WriteStartedNS: fairnessNoisy50Offset(origin, clock.WriteStarted), WriteFinishedNS: fairnessNoisy50Offset(origin, clock.WriteFinished), ReplyNS: fairnessNoisy50Offset(origin, clock.ReplyDelivered), FinishedNS: fairnessNoisy50Offset(origin, clock.Finished)}
			sum := sha256.Sum256(call.data)
			row["result_sha256"], row["result_bytes"] = hex.EncodeToString(sum[:]), len(call.data)
			var remote *rpcError
			if errors.As(call.err, &remote) {
				row["rpc_error"] = remote
			}
		}
		if err := sink.write(sink.attempts, row); err != nil {
			return err
		}
	}
	// Do not turn a dead shared transport into a hot loop of invented offers.
	// Every attempted call above remains in the denominator before we stop.
	connection.client.mu.Lock()
	closed := connection.client.closed
	connection.client.mu.Unlock()
	if closed != nil && ctx.Err() == nil {
		return closed
	}
	return nil
}

func fairnessNoisy50Status(ctx context.Context, socket string, origin time.Time, phase string) map[string]any {
	started := time.Now()
	sampleCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	raw, err := control(sampleCtx, socket, "status", nil)
	message := ""
	if err != nil {
		message = err.Error()
	}
	return map[string]any{"phase": phase, "started_ns": fairnessNoisy50Offset(origin, started), "finished_ns": fairnessNoisy50Offset(origin, time.Now()), "raw": raw, "error": message}
}

func fairnessNoisy50Idle(ctx context.Context, socket string) error {
	idle, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	for {
		raw, err := control(idle, socket, "status", nil)
		var status daemon.StatusResponse
		if err == nil {
			err = json.Unmarshal(raw, &status)
		}
		if err != nil {
			return err
		}
		if status.MCPDispatch == nil {
			return errors.New("dispatch status unavailable")
		}
		n := status.MCPDispatch.DrainingTotal + status.MCPDispatch.ProtocolWaiters
		for _, lane := range status.MCPDispatch.Lanes {
			n += lane.InFlight + lane.QueueDepth + lane.Draining + lane.CancelledHeld
		}
		if n == 0 {
			return nil
		}
		if err := waitContext(idle, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func fairnessNoisy50Phase(ctx context.Context, run *privateRun, reader, noisy *fairnessNoisy50Connection, requests []request, name string, seed uint64, origin time.Time, sink *fairnessNoisy50Sink) (map[string]any, error) {
	if err := fairnessNoisy50Idle(ctx, run.Socket); err != nil {
		return nil, err
	}
	before := fairnessNoisy50Status(ctx, run.Socket, origin, name)
	started := time.Now()
	end := started.Add(120 * time.Second)
	phase, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	var wg sync.WaitGroup
	var errorMu sync.Mutex
	var firstError error
	fail := func(err error) {
		if err != nil {
			errorMu.Lock()
			if firstError == nil {
				firstError = err
			}
			errorMu.Unlock()
			cancel()
		}
	}
	start := make(chan struct{})
	if name == "noisy50" {
		for worker := range 50 {
			wg.Go(func() {
				<-start
				for step := 0; phase.Err() == nil; step++ {
					req := requests[(worker+step)%len(requests)]
					fail(fairnessNoisy50Call(phase, noisy, run.Repos[0], req, name, worker, step, origin, sink))
				}
			})
		}
	}
	wg.Go(func() {
		<-start
		pace := newPacing(agentProfile, seed)
		for step := 0; phase.Err() == nil; step++ {
			if waitContext(phase, pace.think()) != nil {
				return
			}
			var burst sync.WaitGroup
			for slot := range pace.burst() {
				burst.Go(func() {
					req := requests[(step*5+slot)%len(requests)]
					fail(fairnessNoisy50Call(phase, reader, run.Repos[0], req, name, slot, step, origin, sink))
				})
			}
			burst.Wait()
		}
	})
	wg.Go(func() {
		<-start
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			row := fairnessNoisy50Status(phase, run.Socket, origin, name)
			fail(sink.write(sink.status, row))
			select {
			case <-phase.Done():
				return
			case <-ticker.C:
			}
		}
	})
	close(start)
	<-phase.Done()
	observedEnd := time.Now()
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	drain, drainCancel := context.WithTimeout(ctx, 120*time.Second)
	defer drainCancel()
	select {
	case <-joined:
	case <-drain.Done():
		_ = reader.client.Close()
		_ = noisy.client.Close()
		<-joined
		return nil, errors.New("fairness drain bound exceeded")
	}
	if err := fairnessNoisy50Idle(drain, run.Socket); err != nil {
		return nil, err
	}
	after := fairnessNoisy50Status(ctx, run.Socket, origin, name)
	return map[string]any{"name": name, "measurement_seconds": 120, "started_ns": fairnessNoisy50Offset(origin, started), "scheduled_end_ns": fairnessNoisy50Offset(origin, end), "observed_end_ns": fairnessNoisy50Offset(origin, observedEnd), "status_before": before, "status_after": after, "complete": firstError == nil && !observedEnd.Before(end)}, firstError
}

func fairnessNoisy50SHA(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fairnessNoisy50Collect(parent context.Context, plan fairnessNoisy50Plan, planSHA string, progress func(string, ...any)) (resultErr error) {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	collectorSHA, err := fairnessNoisy50SHA(executable)
	if err != nil {
		return err
	}
	if collectorSHA != plan.Bindings["collector_sha256"] {
		return errors.New("pinned collector executable changed")
	}
	for path, key := range map[string]string{plan.Binary: "product_sha256", plan.Manifest: "fixture_sha256", plan.HostReceipt: "host_receipt_sha256"} {
		sum, err := fairnessNoisy50SHA(path)
		if err != nil {
			return err
		}
		if sum != plan.Bindings[key] {
			return errors.New("pinned input changed: " + key)
		}
	}
	corpora, err := loadManifest(plan.Manifest)
	if err != nil {
		return err
	}
	if len(corpora) != 1 {
		return errors.New("fairness requires the one admitted corpus")
	}
	if err := validateOutputRoots(corpora, plan.Scratch, plan.Output); err != nil {
		return err
	}
	if err := os.Mkdir(plan.Output, 0o700); err != nil {
		return err
	}
	attempts, err := os.OpenFile(filepath.Join(plan.Output, "attempts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	statuses, err := os.OpenFile(filepath.Join(plan.Output, "status.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = attempts.Close()
		return err
	}
	sink := &fairnessNoisy50Sink{attempts: attempts, status: statuses}
	origin := time.Now()
	report := map[string]any{"protocol": fairnessNoisy50Protocol, "repetition": plan.Repetition, "seed": plan.Seed, "bindings": plan.Bindings, "plan_sha256": planSHA, "origin_utc": origin.UTC(), "measurement_seconds": 120, "noisy_workers": 50, "reader_profile": "agent", "reader_think_seconds": []int{2, 5}, "reader_burst_calls": []int{3, 5}, "noisy_nested_burst": false, "product_deadline_seconds": 120, "complete": false, "attempts_file": "attempts.jsonl", "status_file": "status.jsonl", "phases": []any{}}
	var run *privateRun
	var reader, noisy *fairnessNoisy50Connection
	defer func() {
		if reader != nil {
			resultErr = errors.Join(resultErr, reader.client.Close())
		}
		if noisy != nil {
			resultErr = errors.Join(resultErr, noisy.client.Close())
		}
		stopReport := map[string]any{"requested": false, "forced": false, "error": ""}
		if run != nil && run.process != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			elapsed, forced, stopErr := run.stop(stopCtx)
			cancel()
			stopReport = map[string]any{"requested": true, "forced": forced, "elapsed_ms": float64(elapsed) / float64(time.Millisecond), "error": ""}
			if stopErr != nil {
				stopReport["error"] = stopErr.Error()
			}
			resultErr = errors.Join(resultErr, stopErr)
		}
		cleanup := map[string]any{"forced": stopReport["forced"], "stop_error": stopReport["error"], "socket_absent": false, "owned_store_removed": false, "native_handle_cleared": run == nil || run.process == nil, "native_leader_joined": stopReport["requested"] == true && run != nil && run.process == nil}
		if run != nil {
			report["owned_root"] = run.Root
			report["owned_socket_dir"] = run.SocketDir
			if run.process == nil {
				resultErr = errors.Join(resultErr, run.removeState(), removeOwnedState(run.SocketDir))
				_, socketErr := os.Lstat(run.SocketDir)
				_, storeErr := os.Lstat(run.Store)
				cleanup["socket_absent"] = errors.Is(socketErr, os.ErrNotExist)
				cleanup["owned_store_removed"] = errors.Is(storeErr, os.ErrNotExist)
			} else {
				resultErr = errors.Join(resultErr, errors.New("native lifetime unresolved; retain owned state and outer lease"))
			}
		}
		report["cleanup"] = cleanup
		report["normal_stop"] = stopReport
		resultErr = errors.Join(resultErr, sink.attempts.Sync(), sink.status.Sync(), sink.attempts.Close(), sink.status.Close(), sink.err)
		report["complete"] = resultErr == nil
		report["error"] = ""
		if resultErr != nil {
			report["error"] = resultErr.Error()
		}
		data, encodeErr := json.MarshalIndent(report, "", "  ")
		resultErr = errors.Join(resultErr, encodeErr)
		if encodeErr == nil {
			resultErr = errors.Join(resultErr, os.WriteFile(filepath.Join(plan.Output, "report.json"), append(data, '\n'), 0o600))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 2520*time.Second)
	defer cancel()
	warm, warmCancel := context.WithTimeout(ctx, 1800*time.Second)
	defer warmCancel()
	run, err = newPrivateRun(warm, plan.Scratch, plan.Binary, corpora)
	if err != nil {
		return err
	}
	configured, err := run.writeBootRepos(false)
	if err != nil {
		return err
	}
	progress("fairness setup: wrote %d configured repositories", len(configured))
	if err = run.start(); err != nil {
		return err
	}
	report["daemon_pid"] = run.process.Process.Pid
	if err = run.awaitSocket(warm); err != nil {
		return err
	}
	progress("fairness setup: daemon/socket ready (pid %d)", run.process.Process.Pid)
	gate, _, err := run.awaitReady(warm, origin, configured)
	if err != nil {
		return err
	}
	progress("fairness setup: repository readiness reached (%d configured)", len(configured))
	report["readiness_gate"] = gate
	var state readiness
	if err = json.Unmarshal(gate["readiness"], &state); err != nil {
		return err
	}
	for _, repo := range state.TrackedRepos {
		if filepath.Clean(repo.Path) == filepath.Clean(run.Repos[0].Path) {
			run.Repos[0].Prefix = repo.Prefix
		}
	}
	if run.Repos[0].Prefix == "" {
		return errors.New("actual repository prefix unavailable")
	}
	reader, err = openFairnessNoisy50Connection(warm, run.Socket, run.Repos[0].Path, "reader")
	if err != nil {
		return err
	}
	noisy, err = openFairnessNoisy50Connection(warm, run.Socket, run.Repos[0].Path, "noisy")
	if err != nil {
		return err
	}
	if reader.SessionID == noisy.SessionID || reader.Instance != noisy.Instance {
		return errors.New("actual reader/noisy connection identity mismatch")
	}
	report["connections"] = []*fairnessNoisy50Connection{reader, noisy}
	requests, err := readingPlan(corpora[0])
	if err != nil {
		return err
	}
	if len(requests) != 10 {
		return errors.New("frozen ten-operation reading plan unavailable")
	}
	requestHash, err := hashJSON(requests)
	if err != nil {
		return err
	}
	report["request_plan_sha256"] = requestHash
	report["request_plan"] = requests
	for _, c := range []*fairnessNoisy50Connection{reader, noisy} {
		for _, req := range requests {
			_, rows, callErr := callTool(warm, c.client, req, dispatchArgs(req, run.Repos[0]), "warm", 0, 0, 0, 120*time.Second, waitContext)
			if callErr != nil {
				return callErr
			}
			for _, row := range rows {
				if err = sink.write(sink.attempts, map[string]any{"phase": "warm", "session_id": c.SessionID, "daemon_instance": c.Instance, "attempt": row, "wire": nil}); err != nil {
					return err
				}
			}
		}
	}
	warmCancel()
	order := []string{"control", "noisy50"}
	if plan.Repetition == 2 {
		order = []string{"noisy50", "control"}
	}
	report["phase_order"] = order
	var phases []map[string]any
	for _, name := range order {
		phase, phaseErr := fairnessNoisy50Phase(ctx, run, reader, noisy, requests, name, plan.Seed, origin, sink)
		if phase != nil {
			phases = append(phases, phase)
		}
		report["phases"] = phases
		if phaseErr != nil {
			return phaseErr
		}
	}
	return nil
}
