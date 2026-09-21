package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The private-daemon fixture this test drives lives in
// issue767_fixture_shared_test.go; only the idle measurement itself is here.

// TestIssue767IdleIOIntegration launches only explicitly supplied binaries.
// Every process owns private XDG directories, SQLite storage and a Git fixture.
// It never addresses the default daemon.
//
// GORTEX_ISSUE767_TEST_BINARY is required; BASELINE_BINARY optionally runs first.
// GORTEX_ISSUE767_ARTIFACT_DIR preserves logs/reports when supplied.
// GORTEX_ISSUE767_IDLE_DURATION defaults to 45s, bounded 15s–1h; cold idle caps 60s.
// GORTEX_ISSUE767_WRITE_BUDGET_BYTES is optional; absent/zero means no assertion.
// The 5s janitor interval accelerates this regression scenario; measurements are
// not default-configuration idle performance or physical NAND writes.
func TestIssue767IdleIOIntegration(t *testing.T) {
	fixed := os.Getenv("GORTEX_ISSUE767_TEST_BINARY")
	if fixed == "" {
		t.Skip("set GORTEX_ISSUE767_TEST_BINARY to opt into isolated daemon validation")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process I/O sampler supports Darwin and Linux")
	}
	idle := 45 * time.Second
	if value := os.Getenv("GORTEX_ISSUE767_IDLE_DURATION"); value != "" {
		var err error
		idle, err = time.ParseDuration(value)
		if err != nil || idle < 15*time.Second || idle > time.Hour {
			t.Fatal("GORTEX_ISSUE767_IDLE_DURATION must be between 15s and 1h")
		}
	}
	var budget uint64
	if value := os.Getenv("GORTEX_ISSUE767_WRITE_BUDGET_BYTES"); value != "" {
		var err error
		budget, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	required := issue767RequiredTimeout(idle, os.Getenv("GORTEX_ISSUE767_BASELINE_BINARY") != "")
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < required {
		t.Fatalf("isolated scenario needs at least %s remaining; increase go test -timeout before launching daemon children", required)
	}
	for _, variant := range []struct{ name, binary string }{{"baseline", os.Getenv("GORTEX_ISSUE767_BASELINE_BINARY")}, {"fixed", fixed}} {
		if variant.binary == "" {
			continue
		}
		t.Run(variant.name, func(t *testing.T) {
			binary, err := filepath.Abs(variant.binary)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(binary); err != nil {
				t.Fatal(err)
			}
			f := newIssue767Fixture(t, binary)
			coldStart := time.Now()
			f.start()
			f.command(10*time.Second, f.primary, "daemon", "status", "--no-progress")
			f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
			t.Logf("configured cold startup to selected primary query: %s", time.Since(coldStart))
			f.write(filepath.Join(f.primary, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker", "Issue767DirtyMarker"))
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			f.git(f.primary, "worktree", "add", "-b", "issue767-linked", f.linked)
			f.write(filepath.Join(f.linked, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker", "Issue767LinkedMarker"))
			// The linked checkout is intentionally never explicitly tracked.
			f.awaitSymbol(f.linked, "Issue767LinkedMarker")
			leaked, err := f.trySearchSymbol(f.primary, "Issue767LinkedMarker")
			if err != nil {
				t.Fatalf("primary isolation query failed: %v", err)
			}
			if leaked {
				t.Fatal("linked-checkout symbol leaked into primary view")
			}
			f.git(f.primary, "worktree", "remove", "--force", f.linked)
			f.awaitRemoved()
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			f.settle()
			cold := f.measureIdle("cold_idle", min(idle, time.Minute))
			f.stop()
			warmStart := time.Now()
			f.start()
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			t.Logf("warm startup to selected primary query: %s", time.Since(warmStart))
			f.awaitRemoved()
			f.settle()
			warm := f.measureIdle("warm_idle", idle)
			if warm.Before.Sequence != cold.After.Sequence {
				t.Errorf("unchanged warm restart allocated new generations: cold=%d warm=%d", cold.After.Sequence, warm.Before.Sequence)
			}
			if variant.name == "fixed" && budget > 0 {
				for _, report := range []issue767IdleReport{cold, warm} {
					if report.ProcessBytesWritten > budget {
						t.Errorf("%s wrote %d process-accounted bytes, budget %d", report.Phase, report.ProcessBytesWritten, budget)
					}
				}
			}
		})
	}
}

func issue767RequiredTimeout(idle time.Duration, baseline bool) time.Duration {
	variants := 1
	if baseline {
		variants++
	}
	return time.Duration(variants) * (min(idle, time.Minute) + idle + 3*time.Minute)
}

type issue767IdleReport struct {
	Phase                         string
	ElapsedSeconds                float64
	ProcessBytesWritten           uint64
	ProcessLogicalBytesWritten    *uint64 `json:",omitempty"`
	WALBeforeBytes, WALAfterBytes int64
	Before, After                 issue767GenerationSnapshot
}

func (f *issue767Fixture) measureIdle(phase string, duration time.Duration) issue767IdleReport {
	f.t.Helper()
	db := f.openReadOnly()
	defer db.Close()
	ctx, cancel := context.WithTimeout(f.t.Context(), duration+30*time.Second)
	defer cancel()
	before, err := issue767ReadGenerations(ctx, db)
	if err != nil {
		f.t.Fatal(err)
	}
	first, err := issue767ReadProcessIO(ctx, f.pid())
	if err != nil {
		f.t.Fatal(err)
	}
	if before.PrimaryRefFacts == 0 {
		f.t.Fatal("selected primary did not persist reference facts; idle I/O would not exercise the repaired projection")
	}
	report := issue767IdleReport{Phase: phase, Before: before, WALBeforeBytes: issue767FileSize(f.store + "-wal")}
	start := time.Now()
	for time.Since(start) < duration {
		if !f.searchHasSymbol(f.primary, "Issue767DirtyMarker") {
			f.t.Fatal("read-only query lost the selected ready symbol")
		}
		select {
		case <-ctx.Done():
			f.t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	last, err := issue767ReadProcessIO(ctx, f.pid())
	if err != nil {
		f.t.Fatal(err)
	}
	if first.StartTicks != last.StartTicks || last.BytesWritten < first.BytesWritten {
		f.t.Fatal("process identity/counter changed during sample")
	}
	report.ProcessBytesWritten = last.BytesWritten - first.BytesWritten
	if first.LogicalBytesWritten != nil && last.LogicalBytesWritten != nil {
		if *last.LogicalBytesWritten < *first.LogicalBytesWritten {
			f.t.Fatal("logical write counter regressed during sample")
		}
		delta := *last.LogicalBytesWritten - *first.LogicalBytesWritten
		report.ProcessLogicalBytesWritten = &delta
	}
	report.ElapsedSeconds = time.Since(start).Seconds()
	report.WALAfterBytes = issue767FileSize(f.store + "-wal")
	report.After, err = issue767ReadGenerations(ctx, db)
	if err != nil {
		f.t.Fatal(err)
	}
	if report.After.PrimaryRefFacts == 0 {
		f.t.Error("selected primary lost its reference facts during the idle phase")
	}
	if report.Before.Sequence != report.After.Sequence {
		f.t.Errorf("settled unchanged fixture allocated new generations: before=%+v after=%+v", report.Before, report.After)
	}
	// Retired payload may legitimately be collected during a long sample.
	// Counts are evidence, not proof that no existing rows were rewritten.
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.root, phase+".json"), string(data)+"\n")
	f.t.Logf("%s: %s", f.root, data)
	return report
}

func TestIssue767JSONEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		value                  any
		wantFound, wantRefusal bool
	}{
		{"exact", map[string]any{"results": []any{map[string]any{"name": "marker"}}, "freshness": map[string]any{"exact": true}}, true, false},
		{"sibling_fallback", map[string]any{"results": []any{map[string]any{"name": "marker"}}, "freshness": map[string]any{"exact": false}}, true, true},
		{"wrapped", map[string]any{"text": "{\"name\":\"marker\",\"freshness\":{\"exact\":false}}"}, true, true},
		{"tool_error", map[string]any{"error_code": "view_building"}, false, true},
		{"missing", map[string]any{"results": []any{}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, refusal := issue767JSONEvidence(tc.value, "marker")
			if found != tc.wantFound || refusal != tc.wantRefusal {
				t.Fatalf("got found=%v refusal=%v, want found=%v refusal=%v", found, refusal, tc.wantFound, tc.wantRefusal)
			}
		})
	}
}

// TestIssue767JSONSourceMatchesTheNamedFile pins the file-scoped source check
// the sustained harness depends on: a hit only counts when it comes from the
// exact file the caller named, so an edit probe cannot be satisfied by the same
// name in the checkout's marker file or in a sibling checkout.
func TestIssue767JSONSourceMatchesTheNamedFile(t *testing.T) {
	root := filepath.Join("/tmp", "gx767-source")
	hit := func(file string) map[string]any {
		return map[string]any{"results": []any{map[string]any{
			"name": "Probe", "repo_prefix": "issue767", "absolute_file_path": file,
		}}}
	}
	marker := filepath.Join(root, "marker.go")
	nested := filepath.Join(root, "p001", "file00042.go")
	for _, tc := range []struct {
		name  string
		value any
		file  string
		want  bool
	}{
		{"marker_file", hit(marker), marker, true},
		{"nested_file", hit(nested), nested, true},
		{"nested_hit_for_marker_probe", hit(nested), marker, false},
		{"marker_hit_for_nested_probe", hit(marker), nested, false},
		{"sibling_checkout", hit(filepath.Join("/tmp", "gx767-other", "p001", "file00042.go")), nested, false},
		{"foreign_repo", map[string]any{"results": []any{map[string]any{
			"name": "Probe", "repo_prefix": "other", "absolute_file_path": nested,
		}}}, nested, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := issue767JSONSource(tc.value, "Probe", tc.file); got != tc.want {
				t.Fatalf("issue767JSONSource = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIssue767PrefixMatchesTheFixtureFamily pins which repo prefixes count as
// this fixture: the family prefix and the worktree-instance prefixes an
// explicit `track --as-worktree` creates, and nothing else.
func TestIssue767PrefixMatchesTheFixtureFamily(t *testing.T) {
	for prefix, want := range map[string]bool{
		"issue767":        true,
		"issue767@wt01":   true,
		"issue767@linked": true,
		"issue767x":       false,
		"other":           false,
		"":                false,
		"@issue767":       false,
	} {
		if got := issue767PrefixMatches(prefix); got != want {
			t.Errorf("issue767PrefixMatches(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func TestIssue767RequiredTimeout(t *testing.T) {
	for _, tc := range []struct {
		idle     time.Duration
		baseline bool
		want     time.Duration
	}{
		{45 * time.Second, false, 270 * time.Second},
		{time.Minute, true, 10 * time.Minute},
		{30 * time.Minute, false, 34 * time.Minute},
		{time.Hour, true, 128 * time.Minute},
	} {
		if got := issue767RequiredTimeout(tc.idle, tc.baseline); got != tc.want {
			t.Errorf("idle=%s baseline=%v: got %s, want %s", tc.idle, tc.baseline, got, tc.want)
		}
	}
}

// TestIssue767DefaultCorpusIsTheOriginalFixture pins the extraction: the shared
// fixture must still write the exact 34-file corpus the idle and readiness
// harnesses were validated against — one go.mod, one marker.go carrying the
// primary probe, and file00..file31 with their intra-package call edges.
func TestIssue767DefaultCorpusIsTheOriginalFixture(t *testing.T) {
	f := &issue767Fixture{t: t, primary: t.TempDir()}
	issue767DefaultCorpus(f)
	entries, err := os.ReadDir(f.primary)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 34 {
		t.Fatalf("default corpus wrote %d entries, want 34", len(entries))
	}
	for _, tc := range []struct{ path, want string }{
		{"go.mod", "module example.invalid/issue767\n\ngo 1.24\n"},
		{"marker.go", "package fixture\nfunc Issue767PrimaryMarker() int { return Issue767Target00() }\n"},
		{"file00.go", "package fixture\nfunc Issue767Target00() int { return 0 }\nfunc Issue767Caller00() int { return Issue767Target00() }\n"},
		{"file31.go", "package fixture\nfunc Issue767Target31() int { return 31 }\nfunc Issue767Caller31() int { return Issue767Target00() }\n"},
	} {
		got, err := os.ReadFile(filepath.Join(f.primary, tc.path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestIssue767SpellingCarriesAViewSelectorOnlyForAnAutomaticCheckout pins the
// request shape per checkout mode.
//
// The worktree view selector is the automatic lane's spelling. A dedicated
// (explicitly tracked) checkout owns no checkout_routes row, so the same
// selector reaches materializeRequestView with strict=true and is refused —
// "checkout %q is not fully routed yet" — for as long as it stays dedicated
// (internal/mcp/view_request.go:1995-2001). It is served from its own indexed
// corpus instead (internal/mcp/view_request.go:1038-1041), which is a request
// with no view selector at all.
func TestIssue767SpellingCarriesAViewSelectorOnlyForAnAutomaticCheckout(t *testing.T) {
	root := "/private/tmp/gx767/wt01"
	for _, tc := range []struct {
		spelling issue767Spelling
		wantView bool
		label    string
	}{
		{issue767AsPrimary, false, "primary"},
		{issue767AsAutomaticWorktree, true, "worktree_view_selector"},
		{issue767AsOwnCorpus, false, "own_corpus_no_view_selector"},
	} {
		t.Run(tc.spelling.String(), func(t *testing.T) {
			request := issue767SearchRequest(root, "Marker", tc.spelling)
			view, hasView := request["view"]
			if hasView != tc.wantView {
				t.Fatalf("view selector present=%v, want %v (request %v)", hasView, tc.wantView, request)
			}
			if tc.wantView {
				selector, _ := view.(map[string]any)
				if selector["kind"] != "worktree" || selector["path"] != root {
					t.Fatalf("automatic spelling must select the worktree by path, got %v", view)
				}
			}
			if request["query"] != "Marker" || request["operation"] != "symbols" {
				t.Fatalf("request lost its query: %v", request)
			}
			if tc.spelling.String() != tc.label {
				t.Fatalf("spelling %d prints as %q, want %q", tc.spelling, tc.spelling, tc.label)
			}
		})
	}
}

// TestIssue767VerdictKeepsExactnessWhereTheProductOffersIt is the other half:
// relaxing the exactness demand for the dedicated lane must not relax anything
// else. A fallback answer stays a failure in every spelling, and the
// own-corpus spelling still has to be answered out of the asked checkout's own
// file — that physical check is what stops a base-corpus answer from passing
// for a dedicated one.
func TestIssue767VerdictKeepsExactnessWhereTheProductOffersIt(t *testing.T) {
	exact := issue767Answer{Found: true, Exact: true, FromExpectedFile: true}
	unlabelled := issue767Answer{Found: true, FromExpectedFile: true}
	wrongFile := issue767Answer{Found: true, FromExpectedFile: false}
	fallback := issue767Answer{Found: true, Fallback: true, FromExpectedFile: true}

	if found, err := issue767Verdict(exact, issue767AsAutomaticWorktree); !found || err != nil {
		t.Errorf("an exact automatic answer must pass: %v %v", found, err)
	}
	if _, err := issue767Verdict(unlabelled, issue767AsAutomaticWorktree); err == nil {
		t.Error("an automatic checkout answer without the exact label must be refused")
	}
	if found, err := issue767Verdict(unlabelled, issue767AsOwnCorpus); !found || err != nil {
		t.Errorf("a dedicated checkout answers without a freshness label; that spelling must accept it: %v %v", found, err)
	}
	if found, err := issue767Verdict(unlabelled, issue767AsPrimary); !found || err != nil {
		t.Errorf("the primary corpus answer must pass: %v %v", found, err)
	}
	for _, spelling := range []issue767Spelling{issue767AsPrimary, issue767AsAutomaticWorktree, issue767AsOwnCorpus} {
		if _, err := issue767Verdict(fallback, spelling); err == nil {
			t.Errorf("%s accepted a fallback answer", spelling)
		}
		if _, err := issue767Verdict(wrongFile, spelling); err == nil {
			t.Errorf("%s accepted an answer from another file", spelling)
		}
	}
	if found, err := issue767Verdict(issue767Answer{}, issue767AsOwnCorpus); found || err != nil {
		t.Errorf("a not-found answer is not an error, it is a retry: %v %v", found, err)
	}
}

// TestIssue767JSONPrefixNamesTheCorpusThatAnswered records which corpus served
// a request, which is the measurement that distinguishes the family base from
// a checkout's own graph.
//
// Both shapes are real and were observed with the same question against the
// same worktree: served automatically the answer is
// repo_prefix "issue767" with the checkout's absolute path; tracked as an
// independent instance it is repo_prefix "issue767@wt01" with the same path.
func TestIssue767JSONPrefixNamesTheCorpusThatAnswered(t *testing.T) {
	file := filepath.Join("/private/tmp", "gx767", "wt01", "marker.go")
	answer := func(prefix, path string) any {
		return map[string]any{"results": []any{map[string]any{
			"name": "Probe", "repo_prefix": prefix, "absolute_file_path": path,
		}}}
	}
	if got := issue767JSONPrefix(answer("issue767", file), "Probe", file); got != "issue767" {
		t.Errorf("automatic answer attributed to %q, want the family prefix", got)
	}
	if got := issue767JSONPrefix(answer("issue767@wt01", file), "Probe", file); got != "issue767@wt01" {
		t.Errorf("dedicated answer attributed to %q, want the instance prefix", got)
	}
	other := filepath.Join("/private/tmp", "gx767", "repo", "marker.go")
	if got := issue767JSONPrefix(answer("issue767", other), "Probe", file); got != "" {
		t.Errorf("an answer from another file was attributed as %q", got)
	}
	if got := issue767JSONPrefix(answer("issue767", file), "Other", file); got != "" {
		t.Errorf("an answer for another symbol was attributed as %q", got)
	}
}
