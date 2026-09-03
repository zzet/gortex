package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func pendingFreshnessReceipt(s *Server, id, repo, path string, generation uint64) *mutationReceipt {
	receipt := &mutationReceipt{
		id:         id,
		repo:       repo,
		path:       path,
		generation: generation,
		done:       make(chan struct{}),
	}
	s.mutationReceipts.Store(id, receipt)
	return receipt
}

func completeFreshnessReceipt(receipt *mutationReceipt, result indexer.MutationResult) {
	receipt.mu.Lock()
	receipt.result = result
	receipt.completed = true
	receipt.mu.Unlock()
	close(receipt.done)
}

func freshnessToolResultText(result *mcpgo.CallToolResult) string {
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range result.Content {
		if item, ok := content.(mcpgo.TextContent); ok {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}

func TestMutationFreshnessRepoScopeAggregatesPending(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	pendingFreshnessReceipt(s, "receipt-a1", "repo-a", "/repo-a/a.go", 3)
	pendingFreshnessReceipt(s, "receipt-a2", "repo-a", "/repo-a/b.go", 8)
	pendingFreshnessReceipt(s, "receipt-b", "repo-b", "/repo-b/c.go", 5)

	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil {
		t.Fatal("repo-scoped freshness unexpectedly succeeded")
	}
	message := err.Error()
	for _, want := range []string{"receipt-a1", "generation=3", "receipt-a2", "generation=8"} {
		if !strings.Contains(message, want) {
			t.Fatalf("freshness error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "receipt-b") {
		t.Fatalf("repo-scoped freshness included unrelated receipt: %s", message)
	}
}

func TestMutationFreshnessScopeIncludesUnknownAndIgnoresUnrelated(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	pendingFreshnessReceipt(s, "receipt-b", "repo-b", "/repo-b/b.go", 2)
	if err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a"); err != nil {
		t.Fatalf("unrelated repository blocked repo-a: %v", err)
	}

	pendingFreshnessReceipt(s, "receipt-unknown", "", "/unknown/u.go", 4)
	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil || !strings.Contains(err.Error(), "receipt-unknown") {
		t.Fatalf("unknown-owner receipt did not fail wide: %v", err)
	}
	if strings.Contains(err.Error(), "receipt-b") {
		t.Fatalf("unknown-owner check also included unrelated known repo: %v", err)
	}
}

func TestMutationFreshnessTerminalFailureFailsClosed(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	failed := pendingFreshnessReceipt(s, "receipt-failed", "repo-a", "/repo-a/broken.go", 9)
	completeFreshnessReceipt(failed, indexer.MutationResult{
		RequestedGeneration: 9,
		AppliedGeneration:   9,
		Err:                 errors.New("syntax patch failed"),
	})

	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil {
		t.Fatal("terminal patch failure did not fail closed")
	}
	for _, want := range []string{"failed", "receipt-failed", "generation=9", "syntax patch failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("terminal failure %q does not contain %q", err, want)
		}
	}
}

func TestMutationFreshnessSuccessResolvesSupersededFailures(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	stale := pendingFreshnessReceipt(s, "receipt-stale-failed", "repo-a", "/repo-a/file.go", 6)
	completeFreshnessReceipt(stale, indexer.MutationResult{
		RequestedGeneration: 6,
		Err:                 errors.New("context deadline exceeded"),
	})
	otherPath := pendingFreshnessReceipt(s, "receipt-other-path", "repo-a", "/repo-a/other.go", 7)
	completeFreshnessReceipt(otherPath, indexer.MutationResult{
		RequestedGeneration: 7,
		Err:                 errors.New("unrelated failure"),
	})
	newer := pendingFreshnessReceipt(s, "receipt-newer-failed", "repo-a", "/repo-a/file.go", 12)
	completeFreshnessReceipt(newer, indexer.MutationResult{
		RequestedGeneration: 12,
		Err:                 errors.New("later failure"),
	})

	succeeded := pendingFreshnessReceipt(s, "receipt-success", "repo-a", "/repo-a/file.go", 9)
	appliedResult := indexer.MutationResult{
		RequestedGeneration: 9,
		AppliedGeneration:   9,
		Reindexed:           true,
	}
	completeFreshnessReceipt(succeeded, appliedResult)
	s.resolveSupersededFailedReceipts("/repo-a/file.go", 9, appliedResult)

	if _, loaded := s.mutationReceipts.Load("receipt-stale-failed"); !loaded {
		t.Fatal("superseded receipt was deleted; it must stay queryable for the mutation-commit ledger")
	}
	resolved := stale.outcome(false)
	if resolved.Err != nil || !resolved.Reindexed || resolved.AppliedGeneration != 9 {
		t.Fatalf("superseded failure was not resolved in place: %+v", resolved)
	}
	if outcome := otherPath.outcome(false); outcome.Err == nil {
		t.Fatal("failure on an unrelated path was resolved")
	}
	if outcome := newer.outcome(false); outcome.Err == nil {
		t.Fatal("failure newer than the succeeded generation was resolved")
	}

	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil {
		t.Fatal("remaining failures did not fail closed")
	}
	message := err.Error()
	if strings.Contains(message, "receipt-stale-failed") {
		t.Fatalf("freshness error still reports the superseded receipt: %s", message)
	}
	for _, want := range []string{
		"receipt-other-path",
		"receipt-newer-failed",
		"do not recover by waiting",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("freshness error %q does not contain %q", message, want)
		}
	}
}

func TestTrackMutationTicketResolvesSupersededFailures(t *testing.T) {
	s := &Server{}
	stale := pendingFreshnessReceipt(s, "receipt-stale-failed", "", "/repo/file.go", 3)
	completeFreshnessReceipt(stale, indexer.MutationResult{
		RequestedGeneration: 3,
		Err:                 errors.New("context deadline exceeded"),
	})

	done := make(chan indexer.MutationResult, 1)
	ticket := &indexer.MutationTicket{Path: "/repo/file.go", Generation: 5, Done: done}
	receipt := s.trackMutationTicket(ticket)
	done <- indexer.MutationResult{
		RequestedGeneration: 5,
		AppliedGeneration:   5,
		Reindexed:           true,
	}
	close(done)
	<-receipt.done

	if _, loaded := s.mutationReceipts.Load("receipt-stale-failed"); !loaded {
		t.Fatal("stale receipt was deleted; it must stay queryable for the mutation-commit ledger")
	}
	resolved := stale.outcome(false)
	if resolved.Err != nil || !resolved.Reindexed || resolved.AppliedGeneration != 5 {
		t.Fatalf("stale failed receipt not resolved after a successful ticket: %+v", resolved)
	}
	if _, loaded := s.mutationReceipts.Load(receipt.id); !loaded {
		t.Fatal("successful receipt itself was dropped before retention")
	}
}

func TestMutationFreshnessSuccessKeepsPendingSamePathReceipts(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	inflight := pendingFreshnessReceipt(s, "receipt-inflight", "repo-a", "/repo-a/file.go", 4)

	appliedResult := indexer.MutationResult{
		RequestedGeneration: 9,
		AppliedGeneration:   9,
		Reindexed:           true,
	}
	succeeded := pendingFreshnessReceipt(s, "receipt-success", "repo-a", "/repo-a/file.go", 9)
	completeFreshnessReceipt(succeeded, appliedResult)
	s.resolveSupersededFailedReceipts("/repo-a/file.go", 9, appliedResult)

	if _, loaded := s.mutationReceipts.Load("receipt-inflight"); !loaded {
		t.Fatal("a still-pending receipt for the same path was dropped by the resolve")
	}
	if outcome := inflight.outcome(true); outcome.Pending != true {
		t.Fatalf("a still-pending receipt was marked completed by the resolve: %+v", outcome)
	}
	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil || !strings.Contains(err.Error(), "receipt-inflight") {
		t.Fatalf("barrier no longer reports the in-flight receipt: %v", err)
	}
}

func TestReindexedPathResolvesFailedReceiptsForThatPathOnly(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	failed := pendingFreshnessReceipt(s, "receipt-failed", "repo-a", "/repo-a/file.go", 6)
	completeFreshnessReceipt(failed, indexer.MutationResult{
		RequestedGeneration: 6,
		Err:                 errors.New("context deadline exceeded"),
	})
	otherPath := pendingFreshnessReceipt(s, "receipt-other-path", "repo-a", "/repo-a/other.go", 7)
	completeFreshnessReceipt(otherPath, indexer.MutationResult{
		RequestedGeneration: 7,
		Err:                 errors.New("unrelated failure"),
	})
	// Snapshot taken where production takes it: before the pass runs. The
	// in-flight receipt below therefore appears mid-pass and is out of scope
	// by construction, not by the completed/pending check alone.
	eligible := s.failedReceiptsBefore([]string{"file.go"}, "/repo-a")
	inflight := pendingFreshnessReceipt(s, "receipt-inflight", "repo-a", "/repo-a/file.go", 9)

	s.resolveReindexedPathReceipts("/repo-a/file.go", eligible)

	if outcome := failed.outcome(false); outcome.Err != nil || !outcome.Reindexed {
		t.Fatalf("a re-parsed path did not resolve its failed receipt: %+v", outcome)
	}
	if outcome := otherPath.outcome(false); outcome.Err == nil {
		t.Fatal("a failure on an unrelated path was resolved")
	}
	if outcome := inflight.outcome(true); !outcome.Pending {
		t.Fatalf("an in-flight receipt was completed by the reindex resolve: %+v", outcome)
	}
	inflight.mu.RLock()
	touched := inflight.result.Reindexed || inflight.result.AppliedGeneration != 0
	inflight.mu.RUnlock()
	if touched {
		t.Fatal("the resolve stamped a result onto a receipt whose ticket is still in flight")
	}

	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil {
		t.Fatal("the remaining receipts did not fail closed")
	}
	if strings.Contains(err.Error(), "receipt-failed") {
		t.Fatalf("the barrier still reports the resolved receipt: %v", err)
	}
	for _, want := range []string{"receipt-other-path", "receipt-inflight"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("barrier %q no longer reports %q", err, want)
		}
	}
}

func TestReindexedReceiptPathOnlyResolvesAProvenSinglePath(t *testing.T) {
	stale := &indexer.IndexResult{StaleFileCount: 1}
	// A platform-real absolute path: on Windows a rooted path without a
	// drive letter is not absolute, so a hand-written "/repo/..." would be
	// joined rather than used as-is (harmlessly — it then matches no
	// receipt — but it would not exercise the branch under test).
	absFixture, absErr := filepath.Abs(filepath.Join("repo", "internal", "a.go"))
	if absErr != nil {
		t.Fatalf("resolving the fixture path: %v", absErr)
	}
	cases := []struct {
		name   string
		paths  []string
		root   string
		result *indexer.IndexResult
		want   string
		ok     bool
	}{
		{"single relative path is joined to the repo root", []string{"internal/a.go"}, "/repo", stale, filepath.Join("/repo", "internal/a.go"), true},
		{"single absolute path is used as-is", []string{absFixture}, "/repo", stale, absFixture, true},
		{"several paths cannot be attributed per path", []string{"a.go", "b.go"}, "/repo", stale, "", false},
		{"a whole-repo pass resolves nothing", nil, "/repo", stale, "", false},
		{"nothing was re-parsed", []string{"a.go"}, "/repo", &indexer.IndexResult{StaleFileCount: 0}, "", false},
		{"the path itself failed to index", []string{"a.go"}, "/repo", &indexer.IndexResult{StaleFileCount: 1, FailedFiles: []string{"a.go"}}, "", false},
		// The spelling production actually produces: FailedFiles is absolute
		// while the caller asked with a relative path. Comparing the raw
		// request alone lets this walk past the guard and stamp a file that
		// failed to index as fresh.
		{
			"an absolute failed file is refused for a relative request",
			[]string{"internal/a.go"}, "/repo",
			&indexer.IndexResult{StaleFileCount: 1, FailedFiles: []string{filepath.Join("/repo", "internal", "a.go")}},
			"", false,
		},
		{
			"an absolute failed file is refused for an absolute request",
			[]string{absFixture}, "/repo",
			&indexer.IndexResult{StaleFileCount: 1, FailedFiles: []string{absFixture}},
			"", false,
		},
		{"a relative path without a known root is not guessed", []string{"a.go"}, "", stale, "", false},
		{"a missing result resolves nothing", []string{"a.go"}, "/repo", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reindexedReceiptPath(tc.paths, tc.root, tc.result)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("reindexedReceiptPath(%v, %q) = (%q, %v), want (%q, %v)",
					tc.paths, tc.root, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestReindexResolveIgnoresReceiptsThatFailedDuringThePass(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	before := pendingFreshnessReceipt(s, "receipt-before", "repo-a", "/repo-a/file.go", 6)
	completeFreshnessReceipt(before, indexer.MutationResult{
		RequestedGeneration: 6,
		Err:                 errors.New("context deadline exceeded"),
	})

	eligible := s.failedReceiptsBefore([]string{"file.go"}, "/repo-a")

	// A write that lands, fails its own ingest, and completes while the
	// indexer pass is still running. The pass read the bytes as they were at
	// its start, so its success is evidence about those bytes and about
	// nothing written afterwards.
	during := pendingFreshnessReceipt(s, "receipt-during", "repo-a", "/repo-a/file.go", 7)
	completeFreshnessReceipt(during, indexer.MutationResult{
		RequestedGeneration: 7,
		Err:                 errors.New("ingest failed"),
	})

	s.resolveReindexedPathReceipts("/repo-a/file.go", eligible)

	if outcome := before.outcome(false); outcome.Err != nil || !outcome.Reindexed {
		t.Fatalf("the receipt the pass actually covers was not resolved: %+v", outcome)
	}
	if outcome := during.outcome(false); outcome.Err == nil {
		t.Fatalf("a write that failed while the pass ran was stamped fresh: %+v", outcome)
	}

	err := s.awaitMutationFreshnessForRepos(context.Background(), "repo-a")
	if err == nil || !strings.Contains(err.Error(), "receipt-during") {
		t.Fatalf("the barrier stopped reporting the mid-pass failure: %v", err)
	}
	if strings.Contains(err.Error(), "receipt-before") {
		t.Fatalf("the barrier still reports the resolved receipt: %v", err)
	}
}

func TestFailedReceiptsBeforeRefusesToSnapshotAnUnattributablePass(t *testing.T) {
	s := &Server{mutationSafetyWait: time.Millisecond}
	failed := pendingFreshnessReceipt(s, "receipt-failed", "repo-a", "/repo-a/file.go", 6)
	completeFreshnessReceipt(failed, indexer.MutationResult{
		RequestedGeneration: 6,
		Err:                 errors.New("context deadline exceeded"),
	})

	// Every shape reindexedReceiptPath also refuses. A nil snapshot has to
	// resolve nothing on its own, so the two guards cannot drift into a state
	// where one of them alone is what stands between a failed ingest and a
	// fresh verdict.
	for _, tc := range []struct {
		name  string
		paths []string
		root  string
	}{
		{"several paths", []string{"a.go", "b.go"}, "/repo-a"},
		{"whole-repo pass", nil, "/repo-a"},
		{"relative path without a root", []string{"file.go"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if eligible := s.failedReceiptsBefore(tc.paths, tc.root); eligible != nil {
				t.Fatalf("failedReceiptsBefore(%v, %q) = %v, want nil", tc.paths, tc.root, eligible)
			}
		})
	}

	s.resolveReindexedPathReceipts("/repo-a/file.go", nil)
	if outcome := failed.outcome(false); outcome.Err == nil {
		t.Fatalf("a nil snapshot resolved a receipt: %+v", outcome)
	}
}

func TestMutationReposForSymbolIDsUnresolvedWidensBarrier(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:         "repo-a::known",
		Kind:       graph.KindFunction,
		Name:       "known",
		FilePath:   "known.go",
		StartLine:  1,
		EndLine:    1,
		Language:   "go",
		RepoPrefix: "repo-a",
	})
	s := &Server{
		graph:              g,
		engine:             query.NewEngine(g),
		session:            newSessionState(),
		mutationSafetyWait: time.Millisecond,
	}
	pendingFreshnessReceipt(s, "receipt-other", "repo-b", "/repo-b/other.go", 6)

	if repos := s.mutationReposForSymbolIDs(context.Background(), []string{"missing"}); repos != nil {
		t.Fatalf("unresolved symbol scope = %v, want nil fail-wide scope", repos)
	}
	err := s.awaitMutationFreshnessForRepos(context.Background(), s.mutationReposForSymbolIDs(context.Background(), []string{"missing"})...)
	if err == nil || !strings.Contains(err.Error(), "receipt-other") {
		t.Fatalf("unresolved symbol did not widen the barrier: %v", err)
	}

	repos := s.mutationReposForSymbolIDs(context.Background(), []string{"repo-a::known"})
	if len(repos) != 1 || repos[0] != "repo-a" {
		t.Fatalf("resolved symbol scope = %v", repos)
	}
	if err := s.awaitMutationFreshnessForRepos(context.Background(), repos...); err != nil {
		t.Fatalf("resolved repo-a scope was blocked by repo-b: %v", err)
	}
}

func TestChangeImpactFreshnessGuardAndRetry(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:         "repo-a::target",
		Kind:       graph.KindFunction,
		Name:       "target",
		FilePath:   "target.go",
		StartLine:  1,
		EndLine:    1,
		Language:   "go",
		RepoPrefix: "repo-a",
	})
	s := &Server{
		graph:              g,
		engine:             query.NewEngine(g),
		session:            newSessionState(),
		mutationSafetyWait: time.Millisecond,
	}
	receipt := pendingFreshnessReceipt(s, "receipt-impact", "repo-a", "/repo-a/target.go", 11)
	req := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "explain_change_impact",
		Arguments: map[string]any{"ids": "repo-a::target", "format": "json"},
	}}

	result, err := s.handleEnhancedChangeImpact(context.Background(), req)
	if err != nil {
		t.Fatalf("pending impact handler error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("pending impact result = %#v, want MCP error", result)
	}
	for _, want := range []string{"change impact refused a stale graph", "receipt-impact", "generation=11"} {
		if !strings.Contains(freshnessToolResultText(result), want) {
			t.Fatalf("pending impact response %q does not contain %q", freshnessToolResultText(result), want)
		}
	}

	completeFreshnessReceipt(receipt, indexer.MutationResult{
		RequestedGeneration: 11,
		AppliedGeneration:   11,
		Reindexed:           true,
	})
	result, err = s.handleEnhancedChangeImpact(context.Background(), req)
	if err != nil {
		t.Fatalf("completed impact handler error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("completed impact remained blocked: %q", freshnessToolResultText(result))
	}
}

func TestDetectChangesFreshnessGuard(t *testing.T) {
	s := &Server{
		graph:              graph.New(),
		session:            newSessionState(),
		mutationSafetyWait: time.Millisecond,
	}
	pendingFreshnessReceipt(s, "receipt-detect", "repo-a", "/repo-a/changed.go", 12)
	req := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
		Name:      "detect_changes",
		Arguments: map[string]any{"format": "json"},
	}}

	result, err := s.handleDetectChanges(context.Background(), req)
	if err != nil {
		t.Fatalf("detect handler error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("pending detect result = %#v, want MCP error", result)
	}
	for _, want := range []string{"change detection refused a stale graph", "receipt-detect", "generation=12"} {
		if !strings.Contains(freshnessToolResultText(result), want) {
			t.Fatalf("pending detect response %q does not contain %q", freshnessToolResultText(result), want)
		}
	}
}
