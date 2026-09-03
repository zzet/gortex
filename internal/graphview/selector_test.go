package graphview

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSelectorAccepts(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		graphID    string
		checkoutID string
		value      string
		want       Selector
	}{
		{"auto", "auto", "", "", "", Selector{Kind: SelectorAuto}},
		{"empty kind means auto", "", "", "", "", Selector{Kind: SelectorAuto}},
		{"kind is trimmed", "  auto  ", "", "", "", Selector{Kind: SelectorAuto}},
		{"base", "base", "graph-1", "", "", Selector{Kind: SelectorBase, GraphID: "graph-1"}},
		{"worktree", "worktree", "", "wt-7", "", Selector{Kind: SelectorWorktree, CheckoutID: "wt-7"}},
		{"branch", "git_ref", "", "", "refs/heads/main", Selector{Kind: SelectorGitRef, Value: "refs/heads/main"}},
		{"nested branch", "git_ref", "", "", "refs/heads/feature/foo", Selector{Kind: SelectorGitRef, Value: "refs/heads/feature/foo"}},
		{"deeply nested branch", "git_ref", "", "", "refs/heads/team/feature/foo-bar_2", Selector{Kind: SelectorGitRef, Value: "refs/heads/team/feature/foo-bar_2"}},
		{"tag", "git_ref", "", "", "refs/tags/v1.0.0", Selector{Kind: SelectorGitRef, Value: "refs/tags/v1.0.0"}},
		{"remote branch", "git_ref", "", "", "refs/remotes/origin/main", Selector{Kind: SelectorGitRef, Value: "refs/remotes/origin/main"}},
		{"branch with a dot inside a component", "git_ref", "", "", "refs/heads/release-1.2.x", Selector{Kind: SelectorGitRef, Value: "refs/heads/release-1.2.x"}},
		{"branch named lock", "git_ref", "", "", "refs/heads/lock", Selector{Kind: SelectorGitRef, Value: "refs/heads/lock"}},
		{"branch containing at", "git_ref", "", "", "refs/heads/user@host", Selector{Kind: SelectorGitRef, Value: "refs/heads/user@host"}},
		{"sha1 commit", "commit", "", "", strings.Repeat("a", 40), Selector{Kind: SelectorCommit, Value: strings.Repeat("a", 40)}},
		{"sha256 commit", "commit", "", "", strings.Repeat("0", 64), Selector{Kind: SelectorCommit, Value: strings.Repeat("0", 64)}},
		{"mixed hex commit", "commit", "", "", "0123456789abcdef0123456789abcdef01234567", Selector{Kind: SelectorCommit, Value: "0123456789abcdef0123456789abcdef01234567"}},
		{"branch in a named graph", "git_ref", "graph-1", "", "refs/heads/main", Selector{Kind: SelectorGitRef, GraphID: "graph-1", Value: "refs/heads/main"}},
		{"commit in a named graph", "commit", "graph-1", "", strings.Repeat("a", 40), Selector{Kind: SelectorCommit, GraphID: "graph-1", Value: strings.Repeat("a", 40)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSelector(tc.kind, tc.graphID, tc.checkoutID, tc.value)
			if err != nil {
				t.Fatalf("ParseSelector() = %v, want %+v", err, tc.want)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("ParseSelector() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSelectorRejects(t *testing.T) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name       string
		kind       string
		graphID    string
		checkoutID string
		value      string
		wantCode   string
	}{
		{"unknown kind", "branch", "", "", "", CodeInvalidViewSelector},
		{"kind is case sensitive", "AUTO", "", "", "", CodeInvalidViewSelector},
		{"auto with a graph id", "auto", "graph-1", "", "", CodeSelectorConflict},
		{"auto with a checkout id", "auto", "", "wt-1", "", CodeSelectorConflict},
		{"auto with a value", "auto", "", "", "refs/heads/main", CodeSelectorConflict},
		{"base without a graph id", "base", "", "", "", CodeInvalidViewSelector},
		{"base with a checkout id", "base", "graph-1", "wt-1", "", CodeSelectorConflict},
		{"base with a value", "base", "graph-1", "", "refs/heads/main", CodeSelectorConflict},
		{"worktree without a checkout id", "worktree", "", "", "", CodeInvalidViewSelector},
		{"worktree with a graph id", "worktree", "graph-1", "wt-1", "", CodeSelectorConflict},
		{"git_ref with a checkout id", "git_ref", "", "wt-1", "refs/heads/main", CodeSelectorConflict},
		{"commit with a checkout id", "commit", "", "wt-1", oid, CodeSelectorConflict},

		{"empty ref", "git_ref", "", "", "", CodeInvalidViewSelector},
		{"short branch name", "git_ref", "", "", "main", CodeInvalidViewSelector},
		{"HEAD", "git_ref", "", "", "HEAD", CodeInvalidViewSelector},
		{"FETCH_HEAD", "git_ref", "", "", "FETCH_HEAD", CodeInvalidViewSelector},
		{"heads without refs", "git_ref", "", "", "heads/main", CodeInvalidViewSelector},
		{"unsupported namespace", "git_ref", "", "", "refs/notes/commits", CodeInvalidViewSelector},
		{"namespace with nothing after it", "git_ref", "", "", "refs/heads/", CodeInvalidViewSelector},
		{"leading slash", "git_ref", "", "", "/refs/heads/main", CodeInvalidViewSelector},
		{"trailing slash", "git_ref", "", "", "refs/heads/main/", CodeInvalidViewSelector},
		{"empty component", "git_ref", "", "", "refs/heads/foo//bar", CodeInvalidViewSelector},
		{"range expression", "git_ref", "", "", "refs/heads/feature..bar", CodeInvalidViewSelector},
		{"double dot at the end", "git_ref", "", "", "refs/heads/feature..", CodeInvalidViewSelector},
		{"tilde revision", "git_ref", "", "", "refs/heads/main~1", CodeInvalidViewSelector},
		{"caret revision", "git_ref", "", "", "refs/heads/main^", CodeInvalidViewSelector},
		{"reflog revision", "git_ref", "", "", "refs/heads/main@{1}", CodeInvalidViewSelector},
		{"colon", "git_ref", "", "", "refs/heads/main:file.go", CodeInvalidViewSelector},
		{"question mark", "git_ref", "", "", "refs/heads/main?", CodeInvalidViewSelector},
		{"asterisk", "git_ref", "", "", "refs/heads/*", CodeInvalidViewSelector},
		{"open bracket", "git_ref", "", "", "refs/heads/ma[in", CodeInvalidViewSelector},
		{"backslash", "git_ref", "", "", "refs/heads/ma\\in", CodeInvalidViewSelector},
		{"space", "git_ref", "", "", "refs/heads/ma in", CodeInvalidViewSelector},
		{"leading space", "git_ref", "", "", " refs/heads/main", CodeInvalidViewSelector},
		{"trailing newline", "git_ref", "", "", "refs/heads/main\n", CodeInvalidViewSelector},
		{"control character", "git_ref", "", "", "refs/heads/ma\x01in", CodeInvalidViewSelector},
		{"delete character", "git_ref", "", "", "refs/heads/ma\x7fin", CodeInvalidViewSelector},
		{"lock suffix", "git_ref", "", "", "refs/heads/main.lock", CodeInvalidViewSelector},
		{"lock suffix on a tag", "git_ref", "", "", "refs/tags/v1.0.0.lock", CodeInvalidViewSelector},
		{"lock suffix mid path", "git_ref", "", "", "refs/heads/feature.lock/foo", CodeInvalidViewSelector},
		{"component starting with a dot", "git_ref", "", "", "refs/heads/.hidden", CodeInvalidViewSelector},
		{"component ending with a dot", "git_ref", "", "", "refs/heads/foo.", CodeInvalidViewSelector},
		{"bare at component", "git_ref", "", "", "refs/heads/@", CodeInvalidViewSelector},

		{"empty commit id", "commit", "", "", "", CodeInvalidViewSelector},
		{"uppercase commit id", "commit", "", "", strings.ToUpper(oid), CodeInvalidViewSelector},
		{"39 hex digits", "commit", "", "", strings.Repeat("a", 39), CodeInvalidViewSelector},
		{"41 hex digits", "commit", "", "", strings.Repeat("a", 41), CodeInvalidViewSelector},
		{"63 hex digits", "commit", "", "", strings.Repeat("a", 63), CodeInvalidViewSelector},
		{"non hex digit", "commit", "", "", strings.Repeat("a", 39) + "g", CodeInvalidViewSelector},
		{"ref name as a commit", "commit", "", "", "refs/heads/main", CodeInvalidViewSelector},
		{"padded commit id", "commit", "", "", " " + oid, CodeInvalidViewSelector},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSelector(tc.kind, tc.graphID, tc.checkoutID, tc.value)
			if err == nil {
				t.Fatalf("ParseSelector() = %+v, want %s", got, tc.wantCode)
			}
			if code := CodeOf(err); code != tc.wantCode {
				t.Fatalf("CodeOf() = %q, want %q (err=%v)", code, tc.wantCode, err)
			}
			if !got.Equal(Selector{}) {
				t.Errorf("failed parse returned %+v, want the zero selector", got)
			}
		})
	}
}

func TestParseSelectorErrorsMatchSentinels(t *testing.T) {
	_, err := ParseSelector("git_ref", "", "", "main")
	if !errors.Is(err, ErrInvalidViewSelector) {
		t.Errorf("%v does not match the invalid_view_selector sentinel", err)
	}
	if errors.Is(err, ErrSelectorConflict) {
		t.Error("a malformed ref matched the selector_conflict sentinel")
	}
	_, err = ParseSelector("auto", "graph-1", "", "")
	if !errors.Is(err, ErrSelectorConflict) {
		t.Errorf("%v does not match the selector_conflict sentinel", err)
	}
}

func TestSelectorString(t *testing.T) {
	tests := []struct {
		sel  Selector
		want string
	}{
		{Selector{Kind: SelectorAuto}, "auto"},
		{Selector{Kind: SelectorBase, GraphID: "graph-1"}, "base:graph-1"},
		{Selector{Kind: SelectorWorktree, CheckoutID: "wt-7"}, "worktree:wt-7"},
		{Selector{Kind: SelectorGitRef, Value: "refs/heads/main"}, "git_ref:refs/heads/main"},
		{Selector{Kind: SelectorCommit, Value: "abc"}, "commit:abc"},
		{Selector{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.sel.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectorEqual(t *testing.T) {
	a := Selector{Kind: SelectorGitRef, Value: "refs/heads/main"}
	if !a.Equal(Selector{Kind: SelectorGitRef, Value: "refs/heads/main"}) {
		t.Error("identical selectors are not Equal")
	}
	if a.Equal(Selector{Kind: SelectorGitRef, Value: "refs/heads/other"}) {
		t.Error("selectors with different values are Equal")
	}
	if a.Equal(Selector{Kind: SelectorCommit, Value: "refs/heads/main"}) {
		t.Error("selectors with different kinds are Equal")
	}
	if a.Equal(Selector{Kind: SelectorGitRef, GraphID: "g", Value: "refs/heads/main"}) {
		t.Error("selectors with different graph ids are Equal")
	}
	if a.Equal(Selector{Kind: SelectorGitRef, CheckoutID: "c", Value: "refs/heads/main"}) {
		t.Error("selectors with different checkout ids are Equal")
	}
}
