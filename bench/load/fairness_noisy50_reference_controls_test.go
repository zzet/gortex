//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

// Synthetic source controls only. These are never native/host acceptance
// observations; the real collector must obtain session IDs from handshake ACKs.
func fairnessNoisy50SyntheticWire() []fairnessNoisy50Wire {
	var rows []fairnessNoisy50Wire
	for slot := range 50 {
		rows = append(rows, fairnessNoisy50Wire{Role: "noisy", SessionID: "server-noisy", DaemonInstance: "server-incarnation", Slot: slot, RPCID: int64(slot + 2), StartedNS: int64(slot + 1), WriteStartedNS: int64(slot + 51), WriteFinishedNS: int64(slot + 101), ReplyNS: 1000, FinishedNS: 1001})
	}
	return append(rows, fairnessNoisy50Wire{Role: "reader", SessionID: "server-reader", DaemonInstance: "server-incarnation", RPCID: 2, StartedNS: 200, WriteStartedNS: 201, WriteFinishedNS: 202, ReplyNS: 800, FinishedNS: 801})
}

func TestFairnessNoisy50TopologyRequiresActualSessionAndRPCOverlap(t *testing.T) {
	require.NoError(t, fairnessNoisy50Topology(fairnessNoisy50SyntheticWire(), "server-reader", "server-noisy", "server-incarnation"))
	for _, fault := range []string{"fifty_sessions", "borrowed_reader", "missing_rpc", "duplicate_rpc", "different_instance", "serial_calls", "missing_worker", "fifty_one_workers", "nested_worker_burst", "unwritten", "no_reply"} {
		t.Run(fault, func(t *testing.T) {
			rows := fairnessNoisy50SyntheticWire()
			switch fault {
			case "fifty_sessions":
				for i := range 50 {
					rows[i].SessionID = fmt.Sprintf("actual-other-session-%d", i)
				}
			case "borrowed_reader":
				rows[50].SessionID = "server-noisy"
			case "missing_rpc":
				rows[0].RPCID = 0
			case "duplicate_rpc":
				rows[1].RPCID = rows[0].RPCID
			case "different_instance":
				rows[0].DaemonInstance = "another-process"
			case "serial_calls":
				for i := range 50 {
					start := int64(i*100 + 1)
					rows[i].StartedNS, rows[i].WriteStartedNS = start, start+1
					rows[i].WriteFinishedNS, rows[i].ReplyNS, rows[i].FinishedNS = start+2, start+3, start+4
				}
			case "missing_worker":
				rows = rows[1:]
			case "fifty_one_workers":
				extra := rows[0]
				extra.Slot, extra.RPCID = 50, 1000
				rows = append(rows, extra)
			case "nested_worker_burst":
				extra := rows[0]
				extra.RPCID = 1000
				rows = append(rows, extra)
			case "unwritten":
				rows[0].WriteStartedNS = 0
			case "no_reply":
				rows[0].ReplyNS = 0
			}
			require.Error(t, fairnessNoisy50Topology(rows, "server-reader", "server-noisy", "server-incarnation"), "configured labels/slots cannot substitute for actual treatment")
		})
	}
}

func TestFairnessNoisy50PlanKeepsLiteralCellAndBinding(t *testing.T) {
	p := fairnessNoisy50Plan{Protocol: fairnessNoisy50Protocol, Ready: true, Repetition: 1, Seed: 1, Binary: "/owned/native", Manifest: "/owned/manifest", HostReceipt: "/owned/host", Scratch: "/owned/scratch", Output: "/owned/output", MeasurementSeconds: 120, NoisyWorkers: 50, Bindings: map[string]string{}}
	for _, key := range []string{"product_sha256", "collector_sha256", "fixture_sha256", "host_receipt_sha256"} {
		p.Bindings[key] = strings.Repeat("a", 64)
	}
	require.NoError(t, p.validate())
	for _, fault := range []string{"duration", "workers", "repetition", "unbound", "path", "opt_in"} {
		t.Run(fault, func(t *testing.T) {
			q := p
			q.Bindings = map[string]string{}
			for k, v := range p.Bindings {
				q.Bindings[k] = v
			}
			switch fault {
			case "duration":
				q.MeasurementSeconds = 119
			case "workers":
				q.NoisyWorkers = 49
			case "repetition":
				q.Repetition = 3
			case "unbound":
				delete(q.Bindings, "collector_sha256")
			case "path":
				q.Output = "relative"
			case "opt_in":
				q.Ready = false
			}
			require.Error(t, q.validate())
		})
	}
}

// A local framing stub, not a product/host acceptance case. This exercises the
// additive constructor and the unchanged real socket SDK with two actual
// sockets and 50 simultaneously outstanding calls on the noisy one.
func TestFairnessNoisy50ACKAndMultiplexedWireIdentity(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "fair-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var peersMu sync.Mutex
	var peers []net.Conn
	var serving sync.WaitGroup
	accepted := make(chan struct{})
	arrived := make(chan struct{}, 50)
	release := make(chan struct{})
	go func() {
		defer close(accepted)
		for id := 1; ; id++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			peersMu.Lock()
			peers = append(peers, conn)
			peersMu.Unlock()
			serving.Go(func() {
				defer conn.Close()
				decoder := json.NewDecoder(conn)
				var hello daemon.Handshake
				if decoder.Decode(&hello) != nil {
					return
				}
				var writeMu sync.Mutex
				encode := func(value any) { writeMu.Lock(); defer writeMu.Unlock(); _ = json.NewEncoder(conn).Encode(value) }
				encode(daemon.HandshakeAck{OK: true, SessionID: fmt.Sprintf("actual-stub-session-%d", id), DaemonInstance: "actual-stub-process", DaemonVersion: "source-control"})
				var replies sync.WaitGroup
				defer replies.Wait()
				for {
					var frame stubFrame
					if decoder.Decode(&frame) != nil {
						return
					}
					if frame.ID == 0 {
						continue
					}
					replies.Go(func() {
						if frame.Method != "initialize" && id == 2 {
							select {
							case arrived <- struct{}{}:
							case <-ctx.Done():
								return
							}
							select {
							case <-release:
							case <-ctx.Done():
								return
							}
						}
						encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"content": []any{}}})
					})
				}
			})
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-accepted
		peersMu.Lock()
		for _, conn := range peers {
			_ = conn.Close()
		}
		peersMu.Unlock()
		serving.Wait()
	})
	reader, err := openFairnessNoisy50Connection(ctx, socket, dir, "reader")
	require.NoError(t, err)
	defer reader.client.Close()
	noisy, err := openFairnessNoisy50Connection(ctx, socket, dir, "noisy")
	require.NoError(t, err)
	defer noisy.client.Close()
	require.NotEqual(t, reader.SessionID, noisy.SessionID)
	require.Equal(t, reader.Instance, noisy.Instance)
	origin := time.Now().Add(-time.Millisecond)
	type result struct {
		row fairnessNoisy50Wire
		err error
	}
	results := make(chan result, 51)
	call := func(c *fairnessNoisy50Connection, slot int) {
		_, clock, callErr := c.client.callWithIdentity(ctx, "tools/call", map[string]any{"name": "source-control"})
		results <- result{fairnessNoisy50Wire{Role: c.Role, SessionID: c.SessionID, DaemonInstance: c.Instance, Slot: slot, RPCID: clock.ID, StartedNS: fairnessNoisy50Offset(origin, clock.Started), WriteStartedNS: fairnessNoisy50Offset(origin, clock.WriteStarted), WriteFinishedNS: fairnessNoisy50Offset(origin, clock.WriteFinished), ReplyNS: fairnessNoisy50Offset(origin, clock.ReplyDelivered), FinishedNS: fairnessNoisy50Offset(origin, clock.Finished)}, callErr}
	}
	for slot := range 50 {
		go call(noisy, slot)
	}
	for range 50 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("50 real noisy RPCs did not overlap")
		}
	}
	go call(reader, 0)
	close(release)
	var rows []fairnessNoisy50Wire
	for range 51 {
		select {
		case got := <-results:
			require.NoError(t, got.err)
			rows = append(rows, got.row)
		case <-ctx.Done():
			t.Fatal("SDK replies did not join")
		}
	}
	require.NoError(t, fairnessNoisy50Topology(rows, reader.SessionID, noisy.SessionID, reader.Instance))
}

func TestFairnessNoisy50SinkFailureIsSticky(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "sink-")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	sink := fairnessNoisy50Sink{attempts: f, status: f}
	first := sink.write(f, map[string]any{"attempt": 1})
	require.Error(t, first)
	require.True(t, errors.Is(sink.write(f, map[string]any{"attempt": 2}), first))
}

// Exercise the actual collector bootstrap with the existing owned child daemon.
// Its intentionally minimal ACK stops before warm calls or either measured arm.
func TestFairnessNoisy50ConfiguresOwnedRepoBeforeDaemonReadiness(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
	corpus := gitFixture(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	root := t.TempDir()
	manifest := filepath.Join(root, "manifest.json")
	payload, err := json.Marshal([]Corpus{corpus})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, payload, 0o600))
	host := filepath.Join(root, "host.json")
	require.NoError(t, os.WriteFile(host, []byte("{}\n"), 0o600))
	bindings := map[string]string{}
	for path, key := range map[string]string{executable: "collector_sha256", manifest: "fixture_sha256", host: "host_receipt_sha256"} {
		bindings[key], err = fairnessNoisy50SHA(path)
		require.NoError(t, err)
	}
	bindings["product_sha256"] = bindings["collector_sha256"]
	plan := fairnessNoisy50Plan{Protocol: fairnessNoisy50Protocol, Ready: true, Repetition: 1, Seed: 1, Binary: executable, Manifest: manifest, HostReceipt: host, Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "report"), MeasurementSeconds: 120, NoisyWorkers: 50, Bindings: bindings}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = fairnessNoisy50Collect(ctx, plan, "owned-bootstrap-source-control", t.Logf)
	require.Error(t, err, "the fixture must not run a fairness workload")
	data, readErr := os.ReadFile(filepath.Join(plan.Output, "report.json"))
	require.NoError(t, readErr)
	var report struct {
		OwnedRoot string                     `json:"owned_root"`
		Readiness map[string]json.RawMessage `json:"readiness_gate"`
		Phases    []json.RawMessage          `json:"phases"`
		Cleanup   map[string]any             `json:"cleanup"`
	}
	require.NoError(t, json.Unmarshal(data, &report))
	require.NotEmpty(t, report.Readiness, "collector started without registering its private repo: %v", err)
	var state readiness
	require.NoError(t, json.Unmarshal(report.Readiness["readiness"], &state))
	require.Len(t, state.TrackedRepos, 1)
	expected := filepath.Join(report.OwnedRoot, "repos", corpus.Label)
	require.Equal(t, expected, state.TrackedRepos[0].Path)
	require.NotEqual(t, corpus.Source, state.TrackedRepos[0].Path)
	covered, coverageErr := readyCoverage(state, []privateRepo{{Path: expected}})
	require.NoError(t, coverageErr)
	require.True(t, covered, "readiness did not cover the configured private clone")
	require.Contains(t, err.Error(), "real daemon ACK session/incarnation unavailable")
	require.Empty(t, report.Phases)
	for _, file := range []string{"attempts.jsonl", "status.jsonl"} {
		info, statErr := os.Stat(filepath.Join(plan.Output, file))
		require.NoError(t, statErr)
		require.Zero(t, info.Size(), "bootstrap control leaked query attempts or measured samples")
	}
	require.Equal(t, true, report.Cleanup["socket_absent"])
	require.Equal(t, true, report.Cleanup["owned_store_removed"])
	require.Equal(t, true, report.Cleanup["native_handle_cleared"])
	require.Equal(t, true, report.Cleanup["native_leader_joined"])
}
