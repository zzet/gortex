package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestForeignImpactRetainsRefusalAndOwnedRPC(t *testing.T) {
	root := t.TempDir()
	target := privateRepo{Path: filepath.Join(root, "target"), Prefix: "target", Corpus: Corpus{Label: "target", Symbol: "file.go::Target"}}
	edited := privateRepo{Path: filepath.Join(root, "edited"), Prefix: "edited", Corpus: Corpus{Label: "edited"}}
	socket := startStub(t, func(f stubFrame) (any, *rpcError) {
		if f.Method != "tools/call" {
			return map[string]any{}, nil
		}
		args, _ := f.Params["arguments"].(map[string]any)
		got, _ := args["target"].(map[string]any)
		if f.Cwd != target.Path || f.Params["name"] != "change" || args["operation"] != "impact" || got["symbol"] != "target/file.go::Target" {
			t.Errorf("foreign impact dispatched to wrong route: %+v", f)
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "graph freshness unavailable: relations pending"}}}, nil
	})
	client, err := openMCP(t.Context(), socket, target.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	window := foreignImpactWindow{ID: "active-edits", EditedRepo: "edited", Start: time.Now().Add(-time.Second), End: time.Now().Add(time.Minute)}
	row := observeForeignImpact(t.Context(), client, target, edited, window)
	if !row.WithinWindow || !row.Refused || row.Error == "" || row.RPCID == 0 || len(row.Raw) == 0 || row.Latency.Value == nil {
		t.Fatalf("refusal or exact RPC evidence was lost: %+v", row)
	}
	if row.TargetRepo != "target" || row.EditedRepo != "edited" || row.TargetSymbol != "target/file.go::Target" {
		t.Fatalf("lost repository attribution: %+v", row)
	}
	if row.GateWait.Value != nil || row.ResolveMutexWait.Value != nil {
		t.Fatalf("missing daemon waits were invented: %+v", row)
	}
	summary := summarizeForeignImpact([]foreignImpactSample{row})
	if summary.WithinWindow != 1 || summary.Refusals != 1 || summary.Errors != 1 || summary.Latency.Count != 1 {
		t.Fatalf("refusal was excluded from denominator: %+v", summary)
	}
}

func TestForeignImpactRejectsBorrowedSessionAndSameRepository(t *testing.T) {
	root := t.TempDir()
	target := privateRepo{Path: filepath.Join(root, "target"), Prefix: "target", Corpus: Corpus{Label: "target", Symbol: "a.go::Target"}}
	edited := privateRepo{Path: filepath.Join(root, "edited"), Prefix: "edited", Corpus: Corpus{Label: "edited"}}
	window := foreignImpactWindow{ID: "active", EditedRepo: "edited", Start: time.Now().Add(-time.Second), End: time.Now().Add(time.Second)}
	for _, tc := range []struct {
		name   string
		client *socketClient
		edited privateRepo
	}{
		{"borrowed session", &socketClient{cwd: edited.Path}, edited},
		{"same repository", &socketClient{cwd: target.Path}, target},
		{"nil session", nil, edited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := observeForeignImpact(t.Context(), tc.client, target, tc.edited, window)
			if row.Error == "" || row.RPCID != 0 || row.Latency.Value != nil || row.WithinWindow {
				t.Fatalf("invalid route supplied evidence: %+v", row)
			}
		})
	}
}

func TestForeignImpactWindowKeepsFailuresAndBoundaryCalls(t *testing.T) {
	start := time.Now()
	window := foreignImpactWindow{ID: "active", EditedRepo: "edited", Start: start, End: start.Add(time.Minute)}
	rows := []foreignImpactSample{
		foreignImpactObservation(window, "target", "edited", "target/a.go::A", json.RawMessage(`{"isError":false,"content":[{"text":"{}"}]}`), wireCallIdentity{ID: 1, Started: start, Finished: start.Add(time.Millisecond)}, nil),
		foreignImpactObservation(window, "target", "edited", "target/a.go::A", nil, wireCallIdentity{ID: 2, Started: start.Add(time.Second), Finished: start.Add(2 * time.Second)}, context.DeadlineExceeded),
		foreignImpactObservation(window, "target", "edited", "target/a.go::A", nil, wireCallIdentity{ID: 3, Started: start.Add(-time.Second), Finished: start.Add(time.Second)}, errors.New("connection lost")),
		foreignImpactObservation(window, "target", "edited", "target/a.go::A", nil, wireCallIdentity{ID: 4, Started: start.Add(59 * time.Second), Finished: start.Add(61 * time.Second)}, errors.New("late error")),
	}
	summary := summarizeForeignImpact(rows)
	if summary.Offered != 4 || summary.WithinWindow != 2 || summary.OutsideWindow != 2 || summary.Errors != 3 || summary.Latency.Count != 2 {
		t.Fatalf("hidden exclusion/boundary: %+v", summary)
	}
	missing := summarizeForeignImpact([]foreignImpactSample{{Error: "no connection", Latency: unavailable("ms", "no RPC")}})
	if missing.Latency.P95.Value != nil {
		t.Fatalf("missing observation became zero: %+v", missing)
	}

}

func TestForeignImpactWaitDecoderUsesOnlyObservedCompleteProducer(t *testing.T) {
	for _, tc := range []struct {
		state               string
		attempted, acquired float64
		want                bool
	}{
		{"observed", 1, 1, true}, {"not_applicable_detached", 0, 0, false}, {"not_observed", 0, 0, false}, {"censored", 1, 0, false}, {"observed", 0, 0, false}, {"observed", 2, 1, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			meta := map[string]any{"reach_coordination": map[string]any{"resolve_mutex_wait": map[string]any{"state": tc.state, "attempted": tc.attempted, "acquired": tc.acquired, "total_ms": float64(4)}}}
			got := foreignImpactWait(meta, "resolve_mutex_wait")
			if (got.Value != nil) != tc.want {
				t.Fatalf("producer state %s became %+v", tc.state, got)
			}
			if got.Value != nil && *got.Value != 4 {
				t.Fatalf("producer duration altered: %+v", got)
			}
		})
	}
}

func TestForeignImpactPreservesStructuredRPCRefusalData(t *testing.T) {
	start := time.Now()
	data := map[string]any{"error_code": "graph_freshness_unavailable", "details": map[string]any{"pending_files": []any{"target/a.go"}}}
	remote := &rpcError{Code: -32000, Message: "stale graph", Data: data}
	row := foreignImpactObservation(foreignImpactWindow{ID: "active", EditedRepo: "edited", Start: start, End: start.Add(time.Minute)}, "target", "edited", "target/a.go::A", nil, wireCallIdentity{ID: 1, Started: start, Finished: start.Add(time.Millisecond)}, remote)
	if !row.Refused || row.ErrorCode == nil || *row.ErrorCode != -32000 || row.ErrorData["error_code"] != "graph_freshness_unavailable" {
		t.Fatalf("typed refusal lost: %+v", row)
	}
	data["error_code"] = "mutated"
	data["details"].(map[string]any)["pending_files"].([]any)[0] = "other.go"
	if row.ErrorData["error_code"] != "graph_freshness_unavailable" || row.ErrorData["details"].(map[string]any)["pending_files"].([]any)[0] != "target/a.go" {
		t.Fatal("retained refusal borrowed mutable RPC data")
	}
}

func TestForeignImpactStreamFailsClosedWhenRawEvidenceCannotBeRetained(t *testing.T) {
	root := t.TempDir()
	target := privateRepo{Path: filepath.Join(root, "target"), Prefix: "target", Corpus: Corpus{Label: "target", Symbol: "a.go::A"}}
	edited := privateRepo{Path: filepath.Join(root, "edited"), Prefix: "edited", Corpus: Corpus{Label: "edited"}}
	socket := startStub(t, func(f stubFrame) (any, *rpcError) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "{}"}}}, nil
	})
	client, err := openMCP(t.Context(), socket, target.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	window := foreignImpactWindow{ID: "active", EditedRepo: "edited", Start: time.Now(), End: time.Now().Add(time.Minute)}
	emitted := 0
	diskErr := errors.New("raw evidence writer failed")
	summary, err := runForeignImpactStream(t.Context(), []foreignImpactTarget{{Client: client, Repo: target}}, edited, window, func(row foreignImpactSample) error {
		emitted++
		if row.RPCID == 0 || len(row.Raw) == 0 {
			t.Error("writer received no actual RPC evidence")
		}
		return diskErr
	})
	if !errors.Is(err, diskErr) || emitted != 1 || summary.Offered != 1 {
		t.Fatalf("evidence failure was hidden: emitted=%d err=%v", emitted, err)
	}
}

func TestForeignImpactStreamRotatesDistinctOwnedTargets(t *testing.T) {
	root := t.TempDir()
	edited := privateRepo{Path: filepath.Join(root, "edited"), Prefix: "edited", Corpus: Corpus{Label: "edited"}}
	socket := startStub(t, func(f stubFrame) (any, *rpcError) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "{}"}}}, nil
	})
	var targets []foreignImpactTarget
	for _, name := range []string{"target-one", "target-two"} {
		repo := privateRepo{Path: filepath.Join(root, name), Prefix: name, Corpus: Corpus{Label: name, Symbol: "a.go::A"}}
		client, err := openMCP(t.Context(), socket, repo.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		targets = append(targets, foreignImpactTarget{Client: client, Repo: repo})
	}
	stop := errors.New("three-row diagnostic complete")
	var order []string
	window := foreignImpactWindow{ID: "active", EditedRepo: "edited", Start: time.Now(), End: time.Now().Add(time.Minute)}
	summary, err := runForeignImpactStream(t.Context(), targets, edited, window, func(row foreignImpactSample) error {
		order = append(order, row.TargetRepo)
		if len(order) == 3 {
			return stop
		}
		return nil
	})
	if len(summary.Pairs) != 2 || summary.Pairs[0].Summary.Offered != 2 || summary.Pairs[1].Summary.Offered != 1 {
		t.Fatalf("pooled queries hid individual pair denominators: %+v", summary.Pairs)
	}
	if !errors.Is(err, stop) || summary.Offered != 3 || len(order) != 3 || order[0] != "target-one" || order[1] != "target-two" || order[2] != "target-one" {
		t.Fatalf("query targets were not independently rotated: order=%v summary=%+v err=%v", order, summary, err)
	}
}
