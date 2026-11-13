package citest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type fixture struct {
	t          *testing.T
	root, base string
	planner    *Planner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir()}
	f.planner = NewPlanner(f.root)
	f.write("go.mod", "module example.org/fixture\n\ngo 1.22\nrequire example.org/helper v0.0.0\nreplace example.org/helper => ./internal/thirdparty/helper\n")
	f.pkg("leaf", "func Value() int {return 42}", "TestLeaf")
	f.pkg("consumer", "import \"example.org/fixture/leaf\"\nfunc Value()int{return leaf.Value()}", "TestConsumer")
	f.pkg("indirect", "import \"example.org/fixture/consumer\"\nfunc Value()int{return consumer.Value()}", "TestIndirect")
	f.pkg("testconsumer", "func Value()int{return 42}", "TestTestConsumer")
	f.write("testconsumer/external_test.go", "package testconsumer_test\nimport(\"testing\";\"example.org/fixture/leaf\")\nfunc TestExternalImport(t *testing.T){if leaf.Value()!=42{t.Fatal(\"bad\")}}\n")
	f.pkg("raceconsumer", "func Value()int{return 42}", "TestRaceConsumer")
	f.write("raceconsumer/race_test.go", "//go:build race\n\npackage raceconsumer\nimport(\"testing\";\"example.org/fixture/leaf\")\nfunc TestRaceImport(t *testing.T){if leaf.Value()!=42{t.Fatal(\"bad\")}}\n")
	f.pkg("platform", "func Value()int{return 42}", "TestPlatform")
	f.write("platform/value_windows.go", "package platform\nimport \"example.org/fixture/leaf\"\nvar WindowsValue=leaf.Value()\n")
	f.write("internal/thirdparty/helper/go.mod", "module example.org/helper\n\ngo 1.22\n")
	f.write("internal/thirdparty/helper/helper.go", "package helper\nimport \"example.org/fixture/leaf\"\nfunc Value()int{return leaf.Value()}\n")
	f.pkg("helperconsumer", "func Value()int{return 42}", "TestHelperConsumer")
	f.write("helperconsumer/helper_test.go", "package helperconsumer\nimport(\"testing\";\"example.org/helper\")\nfunc TestHelperImport(t *testing.T){if helper.Value()!=42{t.Fatal(\"bad\")}}\n")
	f.pkg("internal/mcp", "import \"example.org/fixture/leaf\"\nfunc Value()int{return leaf.Value()}", "TestMCP")
	f.pkg("internal/indexer", "func Value()int{return 42}", "TestIndexer")
	f.write("internal/indexer/other_test.go", "package indexer\nimport \"testing\"\nfunc TestOtherIndexer(t *testing.T){}\nfunc TestWatchFixture(t *testing.T){}\n")
	f.pkg("internal/graph/store_sqlite", "func Value()int{return 42}", "TestStore")
	f.git("init", "-q", "-b", "main")
	f.base = f.commit()
	return f
}
func (f *fixture) write(path, text string) {
	f.t.Helper()
	name := filepath.Join(f.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}
func (f *fixture) pkg(path, source, test string) {
	name := filepath.Base(path)
	f.write(path+"/value.go", "package "+name+"\n"+source+"\n")
	f.write(path+"/value_test.go", fmt.Sprintf("package %s\nimport \"testing\"\nfunc %s(t *testing.T){if Value()!=42{t.Fatal(\"bad\")}}\n", name, test))
}
func (f *fixture) git(args ...string) string {
	f.t.Helper()
	// Waiting for Git does not wait for detached automatic maintenance. Keep
	// these disposable repos quiescent regardless of host/local configuration.
	all := append([]string{
		"-c", "maintenance.auto=false", "-c", "gc.auto=0",
		"-c", "user.name=CI Fixture", "-c", "user.email=ci@example.org",
	}, args...)
	cmd := exec.CommandContext(f.t.Context(), "git", all...)
	cmd.Dir = f.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func (f *fixture) commit() string {
	f.t.Helper()
	f.git("add", "--all")
	f.git("commit", "-qm", "fixture change")
	return f.git("rev-parse", "HEAD")
}
func (f *fixture) change(path string) string {
	f.t.Helper()
	f.write(path, f.read(path)+"\n// changed\n")
	return f.commit()
}
func (f *fixture) plan(head string) Manifest {
	f.t.Helper()
	m, err := f.planner.Plan(f.t.Context(), f.pullRequestEvent(head))
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// pullRequestEvent models the same two-parent checkout Actions tests. The
// existing fixtures have no new main changes, so the merged tree is head's.
func (f *fixture) pullRequestEvent(head string) Event {
	f.t.Helper()
	if head == f.base {
		head = f.git("commit-tree", head+"^{tree}", "-p", head, "-m", "empty pull request")
	}
	tested := f.git("commit-tree", head+"^{tree}", "-p", f.base, "-p", head, "-m", "tested pull request merge")
	f.git("reset", "--hard", tested)
	return Event{Name: "pull_request", Base: f.base, Head: tested, Ref: "refs/pull/17/merge", Author: "fixture-author", PullRequest: &PullRequest{Number: 17, Head: head, Base: f.base, BaseRef: "main"}}
}
func nativeTarget(t *testing.T) string {
	t.Helper()
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range p.Targets {
		if target.GOOS == runtime.GOOS && target.GOARCH == runtime.GOARCH {
			return target.Runner
		}
	}
	t.Fatalf("unsupported native target %s/%s", runtime.GOOS, runtime.GOARCH)
	return ""
}
func requireAffected(t *testing.T, m Manifest) {
	t.Helper()
	if m.Mode != "affected" {
		t.Fatalf("expected affected mode: %s", m.Reason)
	}
}

func TestSelectionReverseImportsTestsRacePlatformsAndReplaceBridge(t *testing.T) {
	f := newFixture(t)
	head := f.change("leaf/value.go")
	original := f.read("go.mod")
	m := f.plan(head)
	requireAffected(t, m)
	want := []string{"consumer", "helperconsumer", "indirect", "internal/mcp", "leaf", "platform", "raceconsumer", "testconsumer"}
	for i := range want {
		want[i] = "example.org/fixture/" + want[i]
	}
	slices.Sort(want)
	for target, names := range m.Packages {
		if !slices.Equal(names, want) {
			t.Errorf("%s selected %v, want %v", target, names, want)
		}
	}
	if len(m.Matrix.Include) != 6 {
		t.Fatalf("matrix = %v", m.Matrix)
	}
	if f.read("go.mod") != original {
		t.Fatal("metadata rewrote go.mod")
	}
	if _, err := os.Stat(filepath.Join(f.root, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("metadata created go.sum: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(f.root, ".ci-selection-*"))
	if len(files) != 0 {
		t.Fatalf("module scratch leaked: %v", files)
	}
}
func TestSelectionTestOnlyAndAddedPackage(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("consumer/value_test.go"))
	requireAffected(t, m)
	want := []string{"example.org/fixture/consumer", "example.org/fixture/indirect"}
	if !slices.Equal(m.Packages[nativeTarget(t)], want) {
		t.Fatalf("test-only selection %v", m.Packages)
	}
	f.git("reset", "--hard", f.base)
	f.pkg("newpkg", "import \"example.org/fixture/leaf\"\nfunc Value()int{return leaf.Value()}", "TestNew")
	m = f.plan(f.commit())
	requireAffected(t, m)
	if !slices.Equal(m.Packages[nativeTarget(t)], []string{"example.org/fixture/newpkg"}) {
		t.Fatal(m.Packages)
	}
}
func TestSelectionResourcesDependenciesAndCIAlwaysFull(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"leaf/testdata/input.txt", "go.mod", "go.sum", "go.work", "go.work.sum", "internal/thirdparty/helper/go.mod", "internal/thirdparty/helper/go.sum", ".github/workflows/ci.yml", "scripts/ci_policy_test.go", ".github/check.go", "internal/citest/policy.go", "README.md", "unknown.data"} {
		t.Run(path, func(t *testing.T) {
			f.git("reset", "--hard", f.base)
			text := "new input\n"
			if strings.HasSuffix(path, ".go") {
				text = "package cipolicy\n"
			} else if data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path))); err == nil {
				text = string(data) + "\n"
			}
			f.write(path, text)
			m := f.plan(f.commit())
			if m.Mode != "full" || !strings.Contains(m.Reason, path) || len(m.Matrix.Include) != 15 {
				t.Fatalf("fallback %s %s", m.Mode, m.Reason)
			}
		})
	}
}
func TestSelectionDeletesRenamesAndNULPaths(t *testing.T) {
	f := newFixture(t)
	renamed := "leaf/a name\nwith newline.go"
	if runtime.GOOS == "windows" {
		renamed = "leaf/a name with spaces.go"
	}
	f.git("mv", "leaf/value.go", renamed)
	head := f.commit()
	changes, err := f.planner.Changes(t.Context(), f.base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !slices.Equal(changes[0].Paths, []string{"leaf/value.go", renamed}) {
		t.Fatalf("rename paths %v", changes)
	}
	if f.plan(head).Mode != "full" {
		t.Fatal("rename selected affected mode")
	}
	f.git("reset", "--hard", f.base)
	if err := os.Remove(filepath.Join(f.root, "leaf/value_test.go")); err != nil {
		t.Fatal(err)
	}
	if f.plan(f.commit()).Mode != "full" {
		t.Fatal("delete selected affected mode")
	}
}
func TestSelectionNestedCgoGeneratedAndUnsupportedInputs(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{"internal/thirdparty/helper/helper.go": "package helper\n// changed\n", "leaf/ignored.go": "//go:build ci_unsupported\n\npackage leaf\n", "leaf/cgo.go": "package leaf\nimport \"C\"\n", "leaf/generated.go": "// Code generated by fixture. DO NOT EDIT.\npackage leaf\n", "leaf/generate.go": "package leaf\n//go:generate echo fixture\n"}
	for path, text := range cases {
		t.Run(path, func(t *testing.T) {
			f.git("reset", "--hard", f.base)
			f.write(path, text)
			m := f.plan(f.commit())
			if m.Mode != "full" || len(m.Matrix.Include) != 15 {
				t.Fatalf("unsafe input optimized: %s", m.Reason)
			}
		})
	}
}
func TestSelectionEmbeddedGoModificationAndNewGlobMatchFull(t *testing.T) {
	f := newFixture(t)
	f.pkg("embedding/child", "func Value()int{return 42}", "TestEmbeddedChild")
	f.pkg("embedding", "import _ \"embed\"\n//go:embed child/*.go\nvar source string\nfunc Value()int{return 42}", "TestEmbedding")
	f.base = f.commit()
	m := f.plan(f.change("embedding/child/value.go"))
	if m.Mode != "full" || !strings.Contains(m.Reason, "embedded") {
		t.Fatal(m.Reason)
	}
	f.git("reset", "--hard", f.base)
	f.write("embedding/child/new.go", "package child\nfunc Added()int{return 42}\n")
	m = f.plan(f.commit())
	if m.Mode != "full" || !strings.Contains(m.Reason, "embedded") {
		t.Fatal(m.Reason)
	}
}
func TestSelectionMissingBrokenOrUnsupportedMetadataFailsFull(t *testing.T) {
	f := newFixture(t)
	head := f.change("leaf/value.go")
	for _, base := range []string{"", strings.Repeat("0", 40)} {
		m, err := f.planner.Plan(t.Context(), Event{Name: "pull_request", Base: base, Head: head})
		if err != nil || m.Mode != "full" || m.EmptyOK {
			t.Fatalf("invalid comparison: %+v %v", m, err)
		}
	}
	f.write("consumer/value.go", "package consumer\nimport \"example.org/missing\"\n")
	if m := f.plan(f.commit()); m.Mode != "full" {
		t.Fatal(m)
	}
	f.git("reset", "--hard", head)
	f.planner.Env = withEnv(os.Environ(), map[string]string{"GOFLAGS": "-tags=unknown"})
	if f.plan(head).Mode != "full" {
		t.Fatal("unsupported flags selected partial")
	}
	files, _ := filepath.Glob(filepath.Join(f.root, ".ci-selection-*"))
	if len(files) > 0 {
		t.Fatal("module scratch leaked after failure")
	}
}
func TestSelectionMainManualOverridesDependabotAndExplicitEmpty(t *testing.T) {
	f := newFixture(t)
	head := f.change("leaf/value.go")
	for _, event := range []Event{{Name: "push"}, {Name: "workflow_dispatch"}, {Name: "pull_request", Full: true}, {Name: "pull_request", Labels: []string{"ci:full"}}, {Name: "pull_request", Author: "dependabot[bot]"}} {
		event.Base = f.base
		event.Head = head
		m, err := f.planner.Plan(t.Context(), event)
		if err != nil || m.Mode != "full" {
			t.Fatalf("override %+v: %+v %v", event, m, err)
		}
	}
	f.base = head
	m, err := f.planner.Plan(t.Context(), f.pullRequestEvent(head))
	if err != nil {
		t.Fatal(err)
	}
	requireAffected(t, m)
	if !m.EmptyOK || len(m.Matrix.Include) != 0 {
		t.Fatal(m)
	}
}
func TestSelectionCLIUsesStablePRAuthorAndOutputsFullPlan(t *testing.T) {
	f := newFixture(t)
	head := f.change("leaf/value.go")
	event := filepath.Join(f.root, "event.json")
	f.write("event.json", `{"pull_request":{"user":{"login":"dependabot[bot]"},"labels":[]}}`)
	mfile := filepath.Join(f.root, "selection.json")
	output := filepath.Join(f.root, "outputs")
	summary := filepath.Join(f.root, "summary")
	env := withEnv(os.Environ(), map[string]string{"GITHUB_EVENT_NAME": "pull_request", "GITHUB_EVENT_PATH": event, "GITHUB_SHA": head, "CI_BASE_SHA": f.base, "GITHUB_ACTOR": "human-rerunner", "CI_FULL_TESTS": "false", "GITHUB_OUTPUT": output, "GITHUB_STEP_SUMMARY": summary})
	var stdout, stderr bytes.Buffer
	if err := RunSelect(t.Context(), f.root, []string{"--output", mfile}, env, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mfile)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "full" || !strings.Contains(m.Reason, "Dependabot") {
		t.Fatal(m)
	}
	if !strings.Contains(f.read("outputs"), "has_tests=true") || !strings.Contains(f.read("summary"), "full") {
		t.Fatal("missing explicit outputs/summary")
	}
}
func TestManifestRejectsStaleCorruptIncompleteAndContextMismatch(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("leaf/value.go"))
	requireAffected(t, m)
	target := nativeTarget(t)
	census := m.Census[target]
	if _, err := f.planner.Validate(t.Context(), m, target, census); err != nil {
		t.Fatal(err)
	}
	clone := func() Manifest {
		data, _ := json.Marshal(m)
		var out Manifest
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, edit := range []func(*Manifest){func(v *Manifest) { v.Head = strings.Repeat("0", 40) }, func(v *Manifest) { delete(v.Census, target) }, func(v *Manifest) { v.Matrix = Matrix{Include: []MatrixRow{}} }} {
		bad := clone()
		edit(&bad)
		bad.SHA256 = bad.Seal()
		if _, err := f.planner.Validate(t.Context(), bad, target, census); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	bad := clone()
	bad.Packages[target] = []string{}
	if _, err := f.planner.Validate(t.Context(), bad, target, census); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	if _, err := f.planner.Validate(t.Context(), m, target, census[:len(census)-1]); err == nil {
		t.Fatal("changed census accepted")
	}
	f.planner.Env = withEnv(os.Environ(), map[string]string{"GOARCH": "unsupported"})
	if _, err := f.planner.Validate(t.Context(), m, target, census); err == nil {
		t.Fatal("changed context accepted")
	}
	data, _ := json.Marshal(m)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "empty_ok")
	data, _ = json.Marshal(fields)
	if _, err := DecodeManifest(data); err == nil {
		t.Fatal("incomplete manifest decoded")
	}
}
func TestSelectionDarwinIndexerKeepsWatcherRaceOwner(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("internal/indexer/value.go"))
	requireAffected(t, m)
	var mac []string
	for _, row := range m.Matrix.Include {
		if row.OS == "macos-latest" {
			mac = append(mac, row.Partition)
		}
	}
	if !slices.Equal(mac, []string{"indexer:1/2", "indexer:2/2", "rest"}) {
		t.Fatal(mac)
	}
}
func TestPlannerCancellationAndInvalidMetadataDoNotBecomeEmptySuccess(t *testing.T) {
	if _, err := decodePackages(`{"ImportPath":"x","Dir":"x","Incomplete":true}`); err == nil {
		t.Fatal("incomplete graph accepted")
	}
	if changes, err := parseChanges([]byte("R100\x00old name.go\x00new name\nwith newline.go\x00")); err != nil || len(changes) != 1 || changes[0].Paths[1] != "new name\nwith newline.go" {
		t.Fatalf("NUL path parsing: %v %v", changes, err)
	}
	if _, err := parseChanges([]byte("M\x00missing-terminator")); err == nil {
		t.Fatal("invalid diff accepted")
	}
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.planner.Plan(ctx, Event{Name: "pull_request", Base: f.base, Head: f.base})
	if err == nil {
		t.Fatal("cancellation became a successful plan")
	}
}

func TestManifestAffectedEmptyNeverMeansFull(t *testing.T) {
	f := newFixture(t)
	m, err := f.planner.Plan(t.Context(), f.pullRequestEvent(f.base))
	if err != nil {
		t.Fatal(err)
	}
	requireAffected(t, m)
	target := nativeTarget(t)
	names, err := f.planner.Validate(t.Context(), m, target, m.Census[target])
	if err != nil || names == nil || len(names) != 0 {
		t.Fatalf("empty affected selection lost its distinction: %v %v", names, err)
	}
	m.Packages[target] = nil
	m.SHA256 = m.Seal()
	if _, err := f.planner.Validate(t.Context(), m, target, m.Census[target]); err == nil {
		t.Fatal("null affected package list accepted")
	}
}
