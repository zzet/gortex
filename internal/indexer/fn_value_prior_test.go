package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// fnValueTree registers function values: same-file handlers, a value no
// same-file function binds (ungated, resolved across the repository) and a
// handler another file defines.
func fnValueTree(handlers string) map[string]string {
	return map[string]string{
		"go.mod": "module example.com/fv\n\ngo 1.22\n",
		"pkg/reg.go": "package pkg\n\nimport \"example.com/fv/sub\"\n\nfunc Register(name string, fn func() int) {}\n\n" +
			"func Setup() {\n\tRegister(\"a\", onAlpha)\n\tRegister(\"b\", onBeta)\n\tRegister(\"c\", sub.Shared)\n}\n\n" + handlers,
		"sub/shared.go": "package sub\n\nfunc Shared() int {\n\treturn 3\n}\n",
	}
}

const fnValueHandlers = "func onAlpha() int {\n\treturn 1\n}\n\nfunc onBeta() int {\n\treturn 2\n}\n"

// fnValueRegistrations renders every fn-value registration a reader holds.
func fnValueRegistrations(t *testing.T, r graph.Reader) []string {
	t.Helper()
	var rows []string
	for e := range r.EdgesByKind(graph.EdgeReferences) {
		if e == nil || e.Meta == nil {
			continue
		}
		if via, _ := e.Meta["via"].(string); via != "callback_registration" {
			continue
		}
		rows = append(rows, e.From+" -> "+e.To+" @"+e.FilePath)
	}
	sort.Strings(rows)
	return rows
}

// A working-tree delta re-resolves only the function values whose name the
// save moved; every other value's registration is republished from the
// prior, and the served view equals a whole index of the saved tree.
func TestEditDeltaFnValueResolvesOnlyWhatTheSaveMoved(t *testing.T) {
	cases := []struct {
		name         string
		handlers     string
		wantResolved int64
	}{
		// A body edit: every value is republished.
		{"body", "func onAlpha() int {\n\treturn 10\n}\n\nfunc onBeta() int {\n\treturn 2\n}\n", 0},
		// onBeta renamed away: its value is no longer captured (a bare name
		// no function of the file declares), the others are republished.
		{"rename_target", "func onAlpha() int {\n\treturn 1\n}\n\nfunc onGamma() int {\n\treturn 2\n}\n", 0},
		// A Shared defined here: the value naming sub.Shared resolves again
		// (a whole index binds it to the same-file definition).
		// onAlpha becomes a method under the same name: the value naming it
		// keeps its capture but must bind the new identity.
		{"retarget", "type T struct{}\n\nfunc (T) onAlpha() int {\n\treturn 1\n}\n\nfunc onBeta() int {\n\treturn 2\n}\n", 1},
		// Setup (the registrar) is renamed: its values resolve as before,
		// through the file's same-name captures, without a lookup.
		{"rename_registrar", fnValueHandlers, 0},
		{"shadow_elsewhere", fnValueHandlers + "\nfunc Shared() int {\n\treturn 4\n}\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordinatorFixtureWithTree(t, fnValueTree(fnValueHandlers))
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			c.compaction.quiet = -1
			coordinatorReconcile(t, c)
			path := filepath.Join(f.worktree, "pkg", "reg.go")
			body := fnValueTree(tc.handlers)["pkg/reg.go"]
			if tc.name == "rename_registrar" {
				body = strings.Replace(body, "func Setup()", "func SetupRen()", 1)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			reusedBefore, resolvedBefore := resolver.FnValueGateCounts()
			if out := coordinatorReconcile(t, c); !out.DirtyBuilt {
				t.Fatalf("no dirty build: %+v", out)
			}
			if LastEditDeltaReport() == nil {
				t.Fatal("the save was not built as a delta")
			}
			reused, resolved := resolver.FnValueGateCounts()
			reused, resolved = reused-reusedBefore, resolved-resolvedBefore

			view := chainMaterialize(t, f)
			defer view.Close()
			clean := builderOpenStore(t, "fv-clean-"+tc.name)
			builderIndex(t, clean, f.worktree)
			served, want := fnValueRegistrations(t, view.Reader), fnValueRegistrations(t, clean)
			if len(want) < 2 {
				t.Fatalf("the fixture registers too little: %v", want)
			}
			if strings.Join(served, "\n") != strings.Join(want, "\n") {
				t.Fatalf("registrations after the save differ from a whole index\n served:\n%s\n clean:\n%s",
					strings.Join(served, "\n"), strings.Join(want, "\n"))
			}
			if resolved != tc.wantResolved {
				t.Fatalf("the save resolved %d function values (republished %d), want %d", resolved, reused, tc.wantResolved)
			}
		})
	}
}
