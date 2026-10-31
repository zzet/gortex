package citest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDocumentationMixedChangeRetainsGoClosure(t *testing.T) {
	f := newFixture(t)
	goOnly := f.plan(f.change("leaf/value.go"))
	requireAffected(t, goOnly)
	f.write("docs/cli.md", "# CLI\n\nOrdinary prose.\n")
	mixed := f.plan(f.commit())
	requireAffected(t, mixed)
	if mixed.EmptyOK || !reflect.DeepEqual(mixed.Packages, goOnly.Packages) || !reflect.DeepEqual(mixed.Matrix, goOnly.Matrix) {
		t.Fatalf("documentation changed the Go closure: %+v", mixed)
	}
	mixed.EmptyOK = true
	mixed.SHA256 = mixed.Seal()
	target := nativeTarget(t)
	if _, err := f.planner.Validate(t.Context(), mixed, target, mixed.Census[target]); err == nil {
		t.Fatal("nonempty package selection accepted an empty-selection flag")
	}
}

func TestDocumentationOnlySelectionIsExplicitSealedAndValidated(t *testing.T) {
	f := newFixture(t)
	f.write("docs/cli.md", "# CLI\n\nOrdinary prose.\n")
	m := f.plan(f.commit())
	requireAffected(t, m)
	if !m.EmptyOK || len(m.Matrix.Include) != 0 || m.SHA256 != m.Seal() {
		t.Fatalf("documentation was not an explicit sealed empty selection: %+v", m)
	}
	for target, names := range m.Packages {
		if names == nil || len(names) != 0 {
			t.Fatalf("%s not explicitly empty: %v", target, names)
		}
	}
	target := nativeTarget(t)
	if selected, err := f.planner.Validate(t.Context(), m, target, m.Census[target]); err != nil || len(selected) != 0 {
		t.Fatalf("verified empty selection refused: %v %v", selected, err)
	}
	m.EmptyOK = false
	m.SHA256 = m.Seal()
	if _, err := f.planner.Validate(t.Context(), m, target, m.Census[target]); err == nil {
		t.Fatal("resealed empty selection without explicit approval accepted")
	}
}

func TestDocumentationUnsafeOwnershipAndContentStayFull(t *testing.T) {
	for _, name := range []string{"embed", "containing_package", "unsupported_package", "nested_module", "invalid_utf8", "nul", "unknown_non_go"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := "docs/cli.md"
			switch name {
			case "embed":
				f.write("root.go", "package root\nimport _ \"embed\"\n//go:embed docs/cli.md\nvar Guide string\n")
			case "containing_package":
				f.pkg("docs", "func Value()int{return 42}", "TestDocs")
			case "unsupported_package":
				f.write("docs/ignored.go", "//go:build unsupported_ci_fixture\n\npackage docs\n")
			case "nested_module":
				path = "docs/manual/cli.md"
				f.write("docs/manual/go.mod", "module example.org/manual\n\ngo 1.22\n")
			}
			// The unsafe ownership exists at the base: only the Markdown changes.
			f.write(path, "original prose\n")
			f.base = f.commit()
			text := "updated prose\n"
			switch name {
			case "invalid_utf8":
				text = "invalid\xff\n"
			case "nul":
				text = "binary\x00input\n"
			}
			f.write(path, text)
			if name == "unknown_non_go" {
				f.write("unknown.data", "unclassified input\n")
			}
			m := f.plan(f.commit())
			if m.Mode != "full" || m.EmptyOK || len(m.Matrix.Include) != 12 {
				t.Fatalf("unsafe documentation omitted: %+v", m)
			}
		})
	}
}

func TestDocumentationTrackedSymlinkModeIsNotOrdinaryEvenWithRegularCheckout(t *testing.T) {
	f := newFixture(t)
	f.git("config", "core.symlinks", "false")
	f.git("config", "core.autocrlf", "true")
	f.git("config", "core.safecrlf", "warn")
	f.write("docs/cli.md", "../README.md\n")
	f.git("add", "docs/cli.md")
	// Symlink payloads are raw blobs, not text-converted checkout files.
	// Forced CRLF policy reproduces the warning that polluted CombinedOutput
	// when hash-object applied ordinary Markdown filters on Windows.
	blob := f.git("hash-object", "-w", "--no-filters", "docs/cli.md")
	if !commitSHA.MatchString(blob) {
		t.Fatalf("raw symlink blob hash includes non-identity output: %q", blob)
	}
	f.git("update-index", "--cacheinfo", "120000,"+blob+",docs/cli.md")
	f.git("commit", "-qm", "symlink-mode fixture")
	head := f.git("rev-parse", "HEAD")
	info, err := os.Lstat(filepath.Join(f.root, "docs", "cli.md"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("control must use a regular checkout file: %v", err)
	}
	m := f.plan(head)
	if m.Mode != "full" || !strings.Contains(m.Reason, "tracked blob") {
		t.Fatalf("Git symlink mode was ignored: %+v", m)
	}
}

func TestDocumentationOverridesAndUnsafeStatusesRemainFull(t *testing.T) {
	f := newFixture(t)
	f.write("docs/cli.md", "ordinary prose\n")
	event := f.pullRequestEvent(f.commit())
	for _, name := range []string{"main", "manual", "label", "override", "dependabot"} {
		e := event
		switch name {
		case "main":
			e.Name = "push"
		case "manual":
			e.Name = "workflow_dispatch"
		case "label":
			e.Labels = []string{"ci:full"}
		case "override":
			e.Full = true
		case "dependabot":
			e.Author = "dependabot[bot]"
		}
		m, err := f.planner.Plan(t.Context(), e)
		if err != nil || m.Mode != "full" || m.EmptyOK || len(m.Matrix.Include) != 12 {
			t.Fatalf("%s override ignored: %+v %v", name, m, err)
		}
	}
	for _, change := range []Change{{"D", []string{"docs/cli.md"}}, {"R100", []string{"docs/cli.md", "docs/renamed.md"}}, {"A", []string{"docs/../README.md"}}, {"M", []string{"docs\\cli.md"}}} {
		if ordinaryChange(change) == nil {
			t.Fatalf("unsupported documentation status/path accepted: %+v", change)
		}
	}
}

func TestDocumentationCLIEmitsExplicitEmptyResultGateInputs(t *testing.T) {
	f := newFixture(t)
	f.write("docs/cli.md", "ordinary prose\n")
	event := f.pullRequestEvent(f.commit())
	body, err := json.Marshal(eventPayload(event))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "outputs")
	env := withEnv(eventEnvironment(t, event, body), map[string]string{"GITHUB_OUTPUT": output})
	var stdout, stderr bytes.Buffer
	if err := RunSelect(t.Context(), f.root, []string{"--output", filepath.Join(t.TempDir(), "selection.json")}, env, &stdout, &stderr); err != nil {
		t.Fatalf("selection CLI: %v: %s", err, &stderr)
	}
	data, err := os.ReadFile(output)
	if err != nil || !strings.Contains(string(data), "mode=affected\nhas_tests=false\nempty_ok=true\n") {
		t.Fatalf("result gate did not receive explicit validated empty outputs: %q %v", data, err)
	}
}
