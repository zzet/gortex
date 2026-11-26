package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The setup is an opt-in for the six-repository, six-editor cell only. It
// does not depend on the ACK policy or host, and the choice is part of the
// workload identity, so a run with the setup never pairs with one without it.
func TestEditCoChangeSetupSelection(t *testing.T) {
	six := []string{
		"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(),
		"--scenario", editProbeScenario, "--edit-repos", "6", "--edit-agents", "6",
		"--agents", "0", "--worktrees", "0", "--edit-count", "100", "--seed", "42",
	}
	type selection struct {
		setup     bool
		ack, plan string
	}
	selected := func(t *testing.T, args ...string) selection {
		t.Helper()
		o, err := parseRun(append(append([]string(nil), six...), args...))
		if err != nil {
			t.Fatalf("six-repository cell %v refused: %v", args, err)
		}
		report, err := newRunReport(o, editProbeRoutingCorpora(), strings.Repeat("a", 40), Host{}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if o.EditWaitCoChange != report.Workload.EditWaitCoChange {
			t.Fatalf("workload identity lost the setup selection: option=%v workload=%v", o.EditWaitCoChange, report.Workload.EditWaitCoChange)
		}
		return selection{setup: report.Workload.EditWaitCoChange, ack: report.Workload.EditACKMode, plan: report.RequestPlan}
	}
	for _, cell := range []struct {
		name string
		args []string
	}{
		{"default ACK", nil},
		{"full ACK", []string{"--edit-ack", "full"}},
		{"local ACK", []string{"--edit-ack", "local"}},
		{"reference full ACK", []string{"--host", "reference", "--edit-ack", "full"}},
		{"reference local ACK", []string{"--host", "reference", "--edit-ack", "local"}},
	} {
		t.Run(cell.name, func(t *testing.T) {
			without := selected(t, cell.args...)
			with := selected(t, append(append([]string(nil), cell.args...), "--edit-wait-cochange")...)
			if without.setup || !with.setup || without.ack != with.ack {
				t.Fatalf("setup selection changed the ACK policy or was not recorded: without=%+v with=%+v", without, with)
			}
			if without.plan == with.plan {
				t.Fatal("request plan identity does not separate the co-change setup")
			}
		})
	}
	for _, cell := range []struct {
		name string
		args []string
	}{
		{"other scenario", []string{"--scenario", "many"}},
		{"single repository", []string{"--edit-repos", "1"}},
		{"shared repositories", []string{"--edit-agents", "8"}},
		{"gate modes", []string{"--edit-gate-modes"}},
	} {
		t.Run("refuses "+cell.name, func(t *testing.T) {
			args := append(append(append([]string(nil), six...), "--edit-wait-cochange"), cell.args...)
			if _, err := parseRun(args); err == nil || !strings.Contains(err.Error(), "--edit-wait-cochange requires") {
				t.Fatalf("co-change setup outside the six-repository cell: %v", err)
			}
		})
	}
}

type coChangeSetupClient struct {
	t        *testing.T
	payloads []string
	used     int
}

func (c *coChangeSetupClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.t.Helper()
	if ctx.Err() != nil || c.used >= len(c.payloads) {
		c.t.Fatalf("unexpected call %d: %v", c.used+1, ctx.Err())
	}
	wire, err := json.Marshal(params)
	if err != nil {
		c.t.Fatal(err)
	}
	want := `{"arguments":{"kind":"co_change","options":{"limit":1},"output":{"format":"json","max_bytes":0},"target":{"file":"primary/pkg/file.go"}},"name":"analyze"}`
	if method != "tools/call" || string(wire) != want {
		c.t.Fatalf("setup path = %s %s, want tools/call %s", method, wire, want)
	}
	payload := c.payloads[c.used]
	c.used++
	return json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": payload}}})
}

func TestEditCoChangeSetupReceipt(t *testing.T) {
	const prefix = `{"target_file":"primary/pkg/file.go","co_changing":[]`
	const pending = prefix + `,"mining_in_progress":true}`
	const ready = prefix + `}`
	const failed = prefix + `,"mining_in_progress":false,"mining_failed":true,"mining_error":"mine failed"}`
	oversized := prefix + `,"note":"` + strings.Repeat("x", coChangeSetupReplyLimit) + `"}`
	repo := privateRepo{Prefix: "primary", Corpus: Corpus{File: "pkg/file.go"}}
	tests := []struct {
		name    string
		wire    []string
		waits   int
		waitErr error
		wantErr string
		// retained is the expected receipt when it is not the last reply.
		retained string
	}{
		{name: "valid empty", wire: []string{ready}},
		{name: "valid bounded row", wire: []string{`{"target_file":"primary/pkg/file.go","co_changing":[{"file":"primary/pkg/other.go"}]}`}},
		{name: "pending then settled", wire: []string{pending, ready}, waits: 1},
		{name: "pending expires", wire: []string{pending}, waits: 1, waitErr: context.DeadlineExceeded, wantErr: "deadline exceeded"},
		{name: "failed", wire: []string{failed}, wantErr: "mining failed"},
		{name: "other repo same relative target", wire: []string{`{"target_file":"foreign/pkg/file.go","co_changing":[]}`}, wantErr: "target"},
		{name: "no collection", wire: []string{`{"target_file":"primary/pkg/file.go"}`}, wantErr: "collection"},
		{name: "unbounded collection", wire: []string{`{"target_file":"primary/pkg/file.go","co_changing":[{},{}]}`}, wantErr: "collection"},
		{name: "untyped status", wire: []string{prefix + `,"mining_in_progress":"true"}`}, wantErr: "status"},
		{name: "explicit null status", wire: []string{prefix + `,"mining_in_progress":null}`}, wantErr: "status"},
		{name: "inconsistent status", wire: []string{prefix + `,"mining_in_progress":false}`}, wantErr: "status"},
		{name: "oversized reply keeps prior receipt", wire: []string{pending, oversized}, waits: 1, wantErr: "evidence bound", retained: pending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			c := &coChangeSetupClient{t: t, payloads: tt.wire}
			waits := 0
			body, rows, err := warmEditCoChange(ctx, c, repo, time.Second, func(_ context.Context, delay time.Duration) error {
				waits++
				if delay != 5*time.Second {
					t.Errorf("pending delay = %v, want 5s", delay)
				}
				return tt.waitErr
			})
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("setup error = %v, want %q", err, tt.wantErr)
			}
			if tt.waitErr != nil && !errors.Is(err, tt.waitErr) {
				t.Errorf("cancellation cause lost: %v", err)
			}
			if waits != tt.waits || c.used != len(tt.wire) || len(rows) != len(tt.wire) {
				t.Fatalf("receipts = %d, calls = %d, waits = %d", len(rows), c.used, waits)
			}
			want := tt.wire[len(tt.wire)-1]
			if tt.retained != "" {
				want = tt.retained
			}
			if string(body) != want {
				t.Errorf("setup receipt = %.80s, want %.80s", body, want)
			}
		})
	}
}
