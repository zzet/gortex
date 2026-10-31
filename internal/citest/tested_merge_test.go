package citest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func advancedMergeFixture(t *testing.T) (*fixture, Event, string) {
	t.Helper()
	f := newFixture(t)
	f.git("checkout", "-b", "pull-request")
	prHead := f.change("leaf/value.go")
	f.git("checkout", "main")
	f.write("build-input.json", "main-only non-Go build input\n")
	f.pkg("mainconsumer", "import \"example.org/fixture/leaf\"\nfunc Value()int{return leaf.Value()}", "TestMainConsumer")
	f.write("internal/indexer/main_dependency_test.go", "package indexer\nimport(\"testing\";\"example.org/fixture/leaf\")\nfunc TestMainAddedDependency(t *testing.T){if leaf.Value()!=42{t.Fatal(\"bad\")}}\n")
	mainBase := f.commit()
	f.git("merge", "--no-ff", "--no-edit", "pull-request")
	tested := f.git("rev-parse", "HEAD")
	event := Event{Name: "pull_request", Base: f.base, Head: tested, Ref: "refs/pull/17/merge", Author: "fixture-author", PullRequest: &PullRequest{Number: 17, Head: prHead, Base: f.base, BaseRef: "main"}}
	return f, event, mainBase
}

func eventPayload(event Event) map[string]any {
	return map[string]any{"number": event.PullRequest.Number, "pull_request": map[string]any{
		"number": event.PullRequest.Number, "user": map[string]any{"login": event.Author}, "labels": []any{},
		"head": map[string]any{"sha": event.PullRequest.Head}, "base": map[string]any{"sha": event.PullRequest.Base, "ref": event.PullRequest.BaseRef},
	}}
}
func eventEnvironment(t *testing.T, event Event, body []byte) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return withEnv(os.Environ(), map[string]string{"GITHUB_EVENT_NAME": event.Name, "GITHUB_SHA": event.Head, "GITHUB_REF": event.Ref, "CI_BASE_SHA": event.Base, "GITHUB_EVENT_PATH": path, "CI_FULL_TESTS": "false", "GITHUB_OUTPUT": "", "GITHUB_STEP_SUMMARY": ""})
}

func TestSelectionUsesVerifiedTestedMergeBaseAndMergedDependencies(t *testing.T) {
	f, event, mainBase := advancedMergeFixture(t)
	// The old formula includes the unrelated unsafe input and would force full.
	changes, err := f.planner.Changes(t.Context(), event.Base, event.Head)
	if err != nil {
		t.Fatal(err)
	}
	oldFallback := false
	for _, change := range changes {
		if slices.Contains(change.Paths, "build-input.json") && ordinaryChange(change) != nil {
			oldFallback = true
		}
	}
	if !oldFallback {
		t.Fatal("real DAG did not reproduce the old main-only unsafe-input fallback")
	}
	body, err := json.Marshal(eventPayload(event))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := EventFromEnvironment(eventEnvironment(t, event, body))
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.planner.Plan(t.Context(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	requireAffected(t, m)
	if len(m.Packages) != len(m.Targets) {
		t.Fatalf("missing supported-platform selections: %+v", m.Packages)
	}
	if m.Base != mainBase || m.Head != event.Head || m.SHA256 != m.Seal() {
		t.Fatalf("manifest lost tested integration identity: %+v", m)
	}
	changes, err = f.planner.Changes(t.Context(), m.Base, m.Head)
	if err != nil || len(changes) != 1 || !slices.Equal(changes[0].Paths, []string{"leaf/value.go"}) {
		t.Fatalf("PR-only change set = %v, %v", changes, err)
	}
	want := []string{"consumer", "helperconsumer", "indirect", "internal/indexer", "internal/mcp", "leaf", "mainconsumer", "platform", "raceconsumer", "testconsumer"}
	for i := range want {
		want[i] = "example.org/fixture/" + want[i]
	}
	slices.Sort(want)
	for target, names := range m.Packages {
		if !slices.Equal(names, want) {
			t.Errorf("%s selected %v, want %v", target, names, want)
		}
		if !slices.Contains(m.Census[target], "example.org/fixture/mainconsumer") {
			t.Errorf("%s census omitted main's new dependency", target)
		}
	}
	target := nativeTarget(t)
	if _, err := f.planner.Validate(t.Context(), m, target, m.Census[target]); err != nil {
		t.Fatal(err)
	}
	f.git("reset", "--hard", event.PullRequest.Head)
	if _, err := f.planner.Validate(t.Context(), m, target, m.Census[target]); err == nil || !strings.Contains(err.Error(), "stale test selection revision") {
		t.Fatalf("manifest accepted PR head instead of tested merge: %v", err)
	}
}

func TestSelectionUnverifiedMergeMetadataFallsBackBeforeGoDiscovery(t *testing.T) {
	f, event, _ := advancedMergeFixture(t)
	original := f.planner.command
	goCalls := 0
	f.planner.command = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		if args[0] == "go" {
			goCalls++
		}
		return original(ctx, dir, env, args...)
	}
	cases := map[string]func(*Event){
		"missing PR":          func(e *Event) { e.PullRequest = nil },
		"missing ref":         func(e *Event) { e.Ref = "" },
		"head ref":            func(e *Event) { e.Ref = "refs/pull/17/head" },
		"different PR ref":    func(e *Event) { e.Ref = "refs/pull/18/merge" },
		"branch ref":          func(e *Event) { e.Ref = "refs/heads/main" },
		"missing number":      func(e *Event) { e.PullRequest.Number = 0 },
		"unknown target":      func(e *Event) { e.PullRequest.BaseRef = "other" },
		"unknown author":      func(e *Event) { e.Author = "" },
		"missing PR head":     func(e *Event) { e.PullRequest.Head = "" },
		"wrong PR head":       func(e *Event) { e.PullRequest.Head = f.base },
		"base mismatch":       func(e *Event) { e.PullRequest.Base = event.Head },
		"missing base":        func(e *Event) { e.Base = ""; e.PullRequest.Base = "" },
		"missing base object": func(e *Event) { e.Base = strings.Repeat("0", 40); e.PullRequest.Base = e.Base },
		"wrong checkout":      func(e *Event) { e.Head = e.PullRequest.Head },
		"metadata error":      func(e *Event) { e.MetadataError = "invalid event JSON" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			e := event
			pr := *event.PullRequest
			e.PullRequest = &pr
			edit(&e)
			m, err := f.planner.Plan(t.Context(), e)
			if err != nil || m.Mode != "full" || m.EmptyOK || len(m.Matrix.Include) != 12 || len(m.Packages) != 0 {
				t.Fatalf("unsafe event selected partial tests: %+v %v", m, err)
			}
		})
	}
	if goCalls != 0 {
		t.Fatalf("unverified metadata reached Go discovery %d times", goCalls)
	}
}

func TestSelectionRejectsNonSyntheticMergeShapes(t *testing.T) {
	f, event, mainBase := advancedMergeFixture(t)
	tree := event.Head + "^{tree}"
	shapes := map[string]string{
		"single parent":    event.PullRequest.Head,
		"reversed parents": f.git("commit-tree", tree, "-p", event.PullRequest.Head, "-p", mainBase, "-m", "reversed merge"),
		"octopus":          f.git("commit-tree", tree, "-p", mainBase, "-p", event.PullRequest.Head, "-p", f.base, "-m", "octopus merge"),
	}
	for name, head := range shapes {
		t.Run(name, func(t *testing.T) {
			f.git("reset", "--hard", head)
			e := event
			e.Head = head
			m, err := f.planner.Plan(t.Context(), e)
			if err != nil || m.Mode != "full" || m.EmptyOK {
				t.Fatalf("unverified shape selected partial: %+v %v", m, err)
			}
		})
	}
	f.git("reset", "--hard", event.Head)
	orphan := f.git("commit-tree", tree, "-m", "unrelated event base")
	e := event
	pr := *event.PullRequest
	e.PullRequest = &pr
	e.Base = orphan
	e.PullRequest.Base = orphan
	m, err := f.planner.Plan(t.Context(), e)
	if err != nil || m.Mode != "full" || !strings.Contains(m.Reason, "not descended") {
		t.Fatalf("unrelated event base trusted: %+v %v", m, err)
	}
}

func TestSelectionCLIInvalidEventMetadataProducesFullManifest(t *testing.T) {
	f, event, _ := advancedMergeFixture(t)
	bodies := map[string][]byte{"invalid JSON": []byte(`{`), "missing PR": []byte(`{}`), "null PR": []byte(`{"pull_request":null}`)}
	for name, edit := range map[string]func(map[string]any){
		"different number": func(p map[string]any) { p["number"] = 18 },
		"missing head":     func(p map[string]any) { delete(p["pull_request"].(map[string]any), "head") },
		"missing labels":   func(p map[string]any) { delete(p["pull_request"].(map[string]any), "labels") },
		"missing author":   func(p map[string]any) { delete(p["pull_request"].(map[string]any), "user") },
	} {
		p := eventPayload(event)
		edit(p)
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		bodies[name] = body
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			env := eventEnvironment(t, event, body)
			output := filepath.Join(t.TempDir(), "selection.json")
			var stdout, stderr bytes.Buffer
			if err := RunSelect(t.Context(), f.root, []string{"--output", output}, env, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			m, err := DecodeManifest(data)
			if err != nil || m.Mode != "full" || m.EmptyOK || !strings.Contains(m.Reason, "metadata") {
				t.Fatalf("malformed metadata did not fail full: %+v %v", m, err)
			}
		})
	}
}
