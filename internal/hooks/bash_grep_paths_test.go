package hooks

import (
	"path/filepath"
	"testing"
)

// A Bash grep from a tracked cwd is judged by the paths it searches, the way
// `cat` of a file and the Grep tool with `path:` already are (#893). Every row
// runs from inside the tracked repo with a probe that matches the pattern, so
// the historical answer for each one is a deny.
func TestBashGrepJudgedByPathOperands(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubProbe(t, []grepSymbolHit{{Name: "panel", FilePath: "pkg/a.go", Line: 1}}, nil)
	stubIndexedFile(t, true, 9)
	home := fixtureAbs("/home/agent")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	notes := filepath.Join(elsewhereFixtureRoot, "notes")
	trackedPkg := filepath.Join(trackedFixtureRoot, "pkg")
	cases := []struct {
		name    string
		command string
		deny    bool
	}{
		// The issue's table.
		{"cat untracked file", "cat ~/.gortex/config.yaml", false},
		{"head untracked file", "head -5 ~/.gortex/config.yaml", false},
		{"grep untracked file", "grep -n hook ~/.gortex/config.yaml", false},
		{"rg untracked file", "rg -n hook ~/.gortex/config.yaml", false},
		{"grep untracked dir", "grep -rn panel " + notes, false},
		{"cd untracked dir then grep", "cd " + notes + " && grep -rn panel .", false},

		// Option values are not searched paths.
		{"pattern from -e", "grep -rn -e panel " + notes, false},
		{"pattern from cluster -rne", "grep -rne panel " + notes, false},
		{"pattern file from -f", "grep -r -f " + filepath.Join(trackedPkg, "patterns.txt") + " " + notes, false},
		{"include glob and --", "grep --include '*.go' -rn panel -- " + notes, false},
		{"rg glob and type", "rg -g '*.md' -t md panel " + notes, false},
		{"rg replacement", "rg -r Replaced panel " + notes, false},
		{"glob under untracked dir", "grep panel " + filepath.Join(notes, "*.md"), false},
		{"cd chain into untracked", "cd " + elsewhereFixtureRoot + " && cd notes && grep -rn panel .", false},
		{"cd tracked, absolute untracked operand", "cd " + trackedPkg + " && grep -rn panel " + notes, false},

		// Docs and config inside the tracked repo, as the Grep tool sees them.
		{"tracked README", "grep -n panel README.md", false},
		{"tracked config files", "grep -c panel go.mod CHANGELOG.md", false},
		{"glob (its expansion depends on shell options)", "grep -i panel docs/*.md", true},
		{"tracked directory", "grep -rn panel docs", true},
		{"README and a source file", "grep -n panel README.md internal/x.go", true},
		{"escape the shell turns into .go", `grep -n panel internal/x.g\o`, true},
		{"rg --ignore takes no value", "rg panel --ignore internal/x.go README.md", true},
		{"piped grep naming source", "grep -n panel README.md | grep panel internal/x.go", true},
		{"piped grep over stdin", "grep -n panel README.md | grep -v test", false},
		{"long switch", "grep --line-number panel README.md", false},
		{"long option taking the file as its value", "grep -rn panel --group-separator README.md", true},
		{"-X takes the next word", "grep -rnX grep panel.md", true},

		// Anything tracked, or anything unproven, keeps the policy.
		{"cwd recursion", "grep -rn panel .", true},
		{"no path operand", "grep -rn panel", true},
		{"mixed untracked and relative tracked", "grep -rn panel " + notes + " internal", true},
		{"mixed untracked and absolute tracked", "rg panel " + notes + " " + trackedPkg, true},
		{"parent of a tracked repo", "grep -rn panel " + filepath.Dir(trackedFixtureRoot), true},
		{"glob above a tracked repo", "grep -rn panel " + filepath.Join(filepath.Dir(trackedFixtureRoot), "*"), true},
		{"cd untracked then back into tracked", "cd " + notes + " && grep -rn panel " + filepath.Join("..", "..", filepath.Base(trackedFixtureRoot)), true},
		{"cd chained with ;", "cd " + notes + "; grep -rn panel .", true},
		{"cd chained with ||", "cd " + notes + " || grep -rn panel .", true},
		{"variable operand", "grep -rn panel $HOME/notes", true},
		{"stdin operand", "grep panel - " + notes, true},
		{"other user's home", "grep panel ~root/notes", true},
		{"source read chained after untracked grep", "grep -n panel " + notes + " && cat internal/x.go", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := enrich(HookInput{
				ToolName:  "Bash",
				ToolInput: map[string]any{"command": tc.command},
				CWD:       trackedFixtureRoot,
			}, 0)
			if result.deny != tc.deny {
				t.Fatalf("%q: deny=%v, want %v (%#v)", tc.command, result.deny, tc.deny, result)
			}
			if !tc.deny && result.context != "" {
				t.Fatalf("%q: an all-untracked search must stay silent, got context %q", tc.command, result.context)
			}
		})
	}
}

// An unknown tracked-repo list proves nothing, so an absolute operand outside
// the cwd must not be read as "untracked" and silence the policy.
func TestBashGrepUnknownTrackedReposKeepsPolicy(t *testing.T) {
	stubTrackedRepos(t)
	stubProbe(t, []grepSymbolHit{{Name: "panel", FilePath: "pkg/a.go", Line: 1}}, nil)

	result := enrichBash(map[string]any{
		"command": "grep -rn panel " + filepath.Join(elsewhereFixtureRoot, "notes"),
	}, trackedFixtureRoot)
	if !result.deny {
		t.Fatalf("an unknown tracked-repo list must not disable the policy: %#v", result)
	}
}

func TestGrepSearchPaths(t *testing.T) {
	abs := filepath.ToSlash(fixtureAbs("/abs"))
	if !filepath.IsAbs(abs) {
		t.Fatalf("absolute search operand fixture is relative: %q", abs)
	}
	cases := []struct {
		command string
		dir     string
		want    []string
	}{
		{"grep -rn panel a b", "", []string{"a", "b"}},
		{"grep -e panel a", "", []string{"a"}},
		{"grep --regexp=panel a", "", []string{"a"}},
		{"grep -A 3 -m1 panel a", "", []string{"a"}},
		{"grep -C3 panel a", "", []string{"a"}},
		{"grep -- -panel a", "", []string{"a"}},
		{"rg -j 4 --type go panel a", "", []string{"a"}},
		{"ag -f panel a", "", []string{"a"}}, // ag -f follows symlinks
		{"grep -rn panel . " + abs, "sub", []string{filepath.Join("sub", "."), abs}},
		{"grep -rn panel", "", nil},
		{"grep panel -", "", nil},
	}
	for _, tc := range cases {
		got := grepSearchPaths(tokenize(tc.command), tc.dir)
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %q, want %q", tc.command, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: got %q, want %q", tc.command, got, tc.want)
			}
		}
	}
}
