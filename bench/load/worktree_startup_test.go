package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorktreeStartupReadyRequiresExactCurrentSymbols(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		ready         bool
	}{
		{"current", `{"view":{"kind":"worktree","exact":true,"checkout_id":"c"},"hits":[{"name":"StartupFinalMarker"}]}`, true},
		{"base fallback", `{"view":{"kind":"base","exact":false},"hits":[{"name":"StartupFinalMarker"}]}`, false},
		{"old exact tree", `{"view":{"kind":"worktree","exact":true,"checkout_id":"c"},"hits":[{"name":"StartupInitialMarker"}]}`, false},
		{"leaked predecessor", `{"view":{"kind":"worktree","exact":true,"checkout_id":"c"},"hits":[{"name":"StartupFinalMarker"},{"name":"StartupBaseMarker"}]}`, false},
		{"leaked seed", `{"view":{"kind":"worktree","exact":true,"checkout_id":"c"},"hits":[{"name":"StartupFinalMarker"},{"name":"StartupSeedMarker"}]}`, false},
		{"substring only", `{"view":{"kind":"worktree","exact":true,"checkout_id":"c"},"hits":[{"name":"StartupFinalMarkerExtra"}]}`, false},
		{"no receipt", `{"hits":[{"name":"StartupFinalMarker"}]}`, false},
		{"no identity", `{"view":{"kind":"worktree","exact":true},"hits":[{"name":"StartupFinalMarker"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready, _, err := worktreeStartupReady(json.RawMessage(tc.payload), "StartupFinalMarker", true)
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%t err=%v, want %t", ready, err, tc.ready)
			}
		})
	}
}

func TestWorktreeStartupProbeRetainsTimeoutWithoutReadinessCredit(t *testing.T) {
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "checkout_id": "old"}, "hits": []map[string]string{{"name": "StartupInitialMarker"}}}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	result := probeWorktreeStartup(ctx, &privateRun{Socket: socket}, "/owned/linked", "StartupFinalMarker", true, time.Millisecond, time.Now())
	if !errors.Is(result.err, context.DeadlineExceeded) || !result.readyAt.IsZero() || len(result.attempts) == 0 {
		t.Fatalf("timeout earned readiness or lost attempts: %+v", result)
	}
	if result.checkout != "old" {
		t.Fatalf("timeout lost the selected worktree identity: %q", result.checkout)
	}
	row := worktreeStartupCase{CheckoutID: result.checkout, Outcome: "timeout"}
	attachStartupPhases(&row, json.RawMessage(`{"publication_phases":[{"checkout_id":"old","phases":[{"phase":"admitted"}]}]}`))
	if !strings.Contains(string(row.PublicationPhases), "admitted") || !strings.HasPrefix(row.PhaseEvidence, "observed:") {
		t.Fatalf("timeout lost matching phase attribution: %+v", row)
	}
}

func TestWorktreeStartupIdentityDoesNotBorrowBase(t *testing.T) {
	for _, tc := range []struct{ kind, checkout string }{{"base", ""}, {"unrouted", "c"}, {"worktree", "c"}} {
		raw := json.RawMessage(`{"view":{"kind":"` + tc.kind + `","exact":false,"checkout_id":"c"}}`)
		ready, checkout, err := worktreeStartupReady(raw, "StartupFinalMarker", true)
		if err != nil || ready || checkout != tc.checkout {
			t.Fatalf("%s: ready=%t checkout=%q err=%v", tc.kind, ready, checkout, err)
		}
	}
}

func TestWorktreeStartupRejectsConflictingIdentities(t *testing.T) {
	var calls atomic.Int32
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		return map[string]any{"view": map[string]any{"kind": "worktree", "exact": true, "checkout_id": fmt.Sprint(calls.Add(1))}}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := probeWorktreeStartup(ctx, &privateRun{Socket: socket}, "/owned/linked", "StartupFinalMarker", true, time.Millisecond, time.Now())
	if result.err == nil || errors.Is(result.err, context.DeadlineExceeded) || result.checkout != "" || !result.readyAt.IsZero() {
		t.Fatalf("conflicting identities earned attribution/readiness: %+v", result)
	}
}

func TestWorktreeStartupBuildObservationExcludesBootAndEarlierRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	earlier := "{\"msg\":\"indexer: full index started\",\"origin\":\"checkout_coordinator\"}\n"
	boot := "{\"msg\":\"indexer: full index started\",\"origin\":\"warmup\"}\n"
	if err := os.WriteFile(path, []byte(earlier+boot), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := startupBuildInLog(path, int64(len(earlier))); err != nil || found {
		t.Fatalf("boot counted as initial build: %t %v", found, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(earlier)
	err = errors.Join(err, file.Close())
	if err != nil {
		t.Fatal(err)
	}
	if found, err := startupBuildInLog(path, int64(len(earlier))); err != nil || !found {
		t.Fatalf("new build absent: %t %v", found, err)
	}
}

func TestWorktreeStartupPrivateDaemon(t *testing.T) {
	binary := os.Getenv("GORTEX_STARTUP_BENCH_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_STARTUP_BENCH_BINARY for an isolated real-daemon smoke test")
	}
	root := t.TempDir()
	fixture, err := newWorktreeStartupFixture(t.Context(), filepath.Join(root, "fixture"), 16, 4, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	o := worktreeStartupOptions{Binary: binary, Files: 16, Changes: 4, Functions: 2, Calls: 2, Repeats: 1, SeedWorktrees: 1, Timeout: 90 * time.Second, Poll: 10 * time.Millisecond}
	for _, arm := range []string{"equal_head", "divergent_head", "head_switch"} {
		t.Run(arm, func(t *testing.T) {
			row, err := runWorktreeStartupCase(t.Context(), o, root, fixture, []Corpus{fixture.Corpus}, arm, 0)
			if err != nil || row.Outcome != "ready" || row.ReadyMS == nil || len(row.Attempts) == 0 {
				t.Fatalf("private startup %s: outcome=%s complete=%t err=%v; artifacts %s", arm, row.Outcome, row.Complete, err, row.Root)
			}
			t.Logf("%s current indexed view: %.3f ms, artifacts %s", arm, *row.ReadyMS, row.Root)
		})
	}
}

func TestWorktreeStartupFixtureHasPinnedDistinctTrees(t *testing.T) {
	fixture, err := newWorktreeStartupFixture(t.Context(), t.TempDir(), 12, 4, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Root: fixture.Corpus.Source, Env: fixture.Env}
	for _, target := range []string{fixture.Initial, fixture.Final, fixture.Seed} {
		raw, err := p.git(t.Context(), fixture.Corpus.Source, "diff", "--name-only", fixture.Corpus.Commit, target)
		if err != nil || len(strings.Fields(string(raw))) != 4 {
			t.Fatalf("diff to %s: %s %v", target, raw, err)
		}
	}
	if fixture.Initial == fixture.Final || fixture.Initial == fixture.Corpus.Commit || fixture.Seed == fixture.Initial || fixture.Seed == fixture.Final || fixture.Seed == fixture.Corpus.Commit {
		t.Fatal("fixture must contain four distinct commits")
	}
	data, err := os.ReadFile(filepath.Join(fixture.Corpus.Source, "file0000.go"))
	if err != nil || !strings.Contains(string(data), "StartupBaseMarker") || strings.Contains(string(data), "StartupInitialMarker") {
		t.Fatalf("source must be returned to pinned primary: %s %v", data, err)
	}
	if _, err := newWorktreeStartupFixture(t.Context(), t.TempDir(), 2, 3, 1, 1); err == nil {
		t.Fatal("accepted a change set larger than the corpus")
	}
}

func TestWorktreeStartupRaceRequiresSwitchInsideInitialCycle(t *testing.T) {
	origin := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	status := json.RawMessage(`{"publication_phases":[{"checkout_id":"c","origin_wall":"2026-11-17T08:00:00Z","clock":"monotonic","phases":[{"phase":"admitted","offset_ns":100000000},{"phase":"planned","offset_ns":200000000},{"phase":"published","offset_ns":2000000000},{"phase":"ticket_failed","offset_ns":2100000000}]}]}`)
	for _, tc := range []struct {
		name    string
		start   time.Duration
		end     time.Duration
		id      string
		wantErr bool
	}{
		{"in flight", 500 * time.Millisecond, time.Second, "c", false},
		{"before admission", 0, time.Second, "c", true},
		{"before planning", 150 * time.Millisecond, time.Second, "c", true},
		{"already published", time.Second, 3 * time.Second, "c", true},
		{"other checkout", 500 * time.Millisecond, time.Second, "other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := worktreeStartupRace(status, tc.id, origin.Add(-time.Second), origin.Add(tc.start), origin.Add(tc.end))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}
