package indexer

import (
	"strings"
	"testing"
)

// An edit over a base built without the type checker's stage, as the
// daemon's committed bases are. The run's edits rename a function, which
// leaves its callers undefined: the type checker's load is degraded
// ("package load degraded"), and the functions it cannot check keep the
// resolver's rows while the rest of the file is at the type checker's level.
// This pins what the edit path gives in both cases:
//   - a body edit: the whole edited file is brought to the type checker's
//     level (the untouched function too);
//   - a rename that breaks a caller in the same file: reported per function.
func TestEditOverABaseWithoutTheTypeChecker(t *testing.T) {
	src := `package p

import (
	"fmt"
	"strings"
)

func Edited(xs []string) error {
	return fmt.Errorf("%s", strings.Join(xs, ","))
}

func Untouched(xs []string) error {
	return fmt.Errorf("%s", strings.Join(xs, ";"))
}

func Caller(xs []string) error {
	if err := Edited(xs); err != nil {
		return err
	}
	return fmt.Errorf("%s", strings.Join(xs, "|"))
}
`
	cases := []struct {
		name, edited string
		// wantTyped names the functions whose standard-library calls must
		// be at the type checker's level after the edit.
		wantTyped []string
	}{
		{"body edit", strings.Replace(src, `"%s", strings.Join(xs, ","))`, `"edited %s", strings.Join(xs, ","))`, 1),
			[]string{"Edited", "Untouched", "Caller"}},
		{"rename breaking a caller", strings.Replace(src, "func Edited(", "func EditedRen(", 1),
			[]string{"EditedRen", "Untouched"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builderIsolateGit(t)
			t.Setenv("GORTEX_ENRICH_MIN_NODES", "1")
			repoDir := builderTempDir(t, "mixed-level")
			builderGit(t, repoDir, "init", "--initial-branch=main")
			builderWriteTree(t, repoDir, map[string]string{"go.mod": "module example.com/p\n\ngo 1.22\n", "p.go": src})
			builderGit(t, repoDir, "add", "-A")
			builderGit(t, repoDir, "commit", "-q", "-m", "base")
			store := builderOpenStore(t, "mixed-level")
			builderIndex(t, store, repoDir) // the base: no type checker
			builder := builderNewBuilder(store)
			builder.Semantic = goTypesManager(t)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			builderWriteFile(t, repoDir, "p.go", tc.edited)
			_, report, chain := h.build()
			view := dirtyChainComposed(t, store, chain)
			level := func(fn string) string {
				var out []string
				for _, e := range view.GetOutEdges(builderGraphPath(builderRepoPrefix, "p.go") + "::" + fn) {
					if e.Kind == "calls" && strings.Contains(e.To, "Errorf") {
						out = append(out, e.To)
					}
				}
				return strings.Join(out, " ")
			}
			t.Logf("enrichment ran %v", report.Enrichment.Ran)
			for _, fn := range []string{"Edited", "EditedRen", "Untouched", "Caller"} {
				if got := level(fn); got != "" {
					t.Logf("%s: %s", fn, got)
				}
			}
			for _, fn := range tc.wantTyped {
				if got := level(fn); !strings.Contains(got, "ext::go:") {
					t.Errorf("%s is not at the type checker's level after the edit: %q", fn, got)
				}
			}
		})
	}
}
