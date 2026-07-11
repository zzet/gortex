package hooks

import "testing"

func TestClassifyBashCommand(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		wantAction BashAction
		wantExtra  string // pattern for Grep/Find, path for ReadSource, "" otherwise
	}{
		// --- grep family, primary ---
		{"grep absolute path", `grep -n foo /repo/x.go`, BashActionGrepLike, "foo"},
		{"grep -rn with .", `grep -rn "handleFoo" .`, BashActionGrepLike, "handleFoo"},
		{"grep --include=", `grep -rn --include=*.go "handleFoo" .`, BashActionGrepLike, "handleFoo"},
		{"grep -e", `grep -e handleFoo -rn .`, BashActionGrepLike, "handleFoo"},
		{"rg bare", `rg handleFoo`, BashActionGrepLike, "handleFoo"},
		{"rg with path", `rg -n handleFoo src/`, BashActionGrepLike, "handleFoo"},
		{"egrep", `egrep -rn "Handler" .`, BashActionGrepLike, "Handler"},
		{"sudo grep", `sudo grep foo /etc/x`, BashActionGrepLike, "foo"},

		// --- grep after pipe: NOT primary ---
		{"go test | grep FAIL", `go test ./... | grep FAIL`, BashActionPassthrough, ""},
		{"echo | grep", `echo hi | grep hi`, BashActionPassthrough, ""},
		{"git log | grep", `git log --oneline | grep fix`, BashActionPassthrough, ""},

		// --- grep after ; && || : primary ---
		{"semicolon then grep", `cd /tmp; grep -rn foo .`, BashActionGrepLike, "foo"},
		{"&& grep", `cd /tmp && grep -rn foo .`, BashActionGrepLike, "foo"},
		{"|| grep", `false || grep foo x`, BashActionGrepLike, "foo"},

		// --- find -name ---
		{"find . -name Handler*", `find . -name "Handler*"`, BashActionFindName, "Handler"},
		{"find -iname prefix*", `find /repo -iname "Handler*"`, BashActionFindName, "Handler"},
		{"find -name *.go (stripped)", `find . -name "*.go"`, BashActionFindName, ".go"},
		{"find -type d (no name)", `find . -maxdepth 3 -type d`, BashActionPassthrough, ""},
		{"find without -name", `find . -newer foo`, BashActionPassthrough, ""},

		// --- cat / head / tail of source files ---
		{"cat .go", `cat /repo/x.go`, BashActionReadSource, "/repo/x.go"},
		{"head -20 .ts", `head -20 src/app.ts`, BashActionReadSource, "src/app.ts"},
		{"tail -n 50 .py", `tail -n 50 mod/foo.py`, BashActionReadSource, "mod/foo.py"},
		{"cat .log (not source)", `cat /tmp/app.log`, BashActionPassthrough, ""},
		{"cat .json", `cat package.json`, BashActionPassthrough, ""},
		{"cat .go | grep", `cat /repo/x.go | grep foo`, BashActionReadSource, "/repo/x.go"},

		// --- quoting ---
		{"single-quoted pattern", `grep -rn 'foo bar' .`, BashActionGrepLike, "foo bar"},
		{"double-quoted pattern", `grep -rn "foo bar" .`, BashActionGrepLike, "foo bar"},
		{"quoted separator inside", `grep -rn 'a;b' .`, BashActionGrepLike, "a;b"},

		// --- passthroughs ---
		{"empty", ``, BashActionPassthrough, ""},
		{"whitespace only", `   `, BashActionPassthrough, ""},
		{"ls file list", `ls /repo`, BashActionFileList, ""},
		{"fd file list", `fd '\\.go$' internal`, BashActionFileList, ""},
		{"tree file list", `tree internal`, BashActionFileList, ""},
		{"git ls-files", `git ls-files '*.go'`, BashActionFileList, ""},
		{"sed source", `sed -n '1,20p' internal/a.go`, BashActionReadSource, "internal/a.go"},
		{"awk source", `awk '{print}' internal/a.go`, BashActionReadSource, "internal/a.go"},
		{"go build", `go build ./...`, BashActionPassthrough, ""},
		{"echo", `echo hello`, BashActionPassthrough, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyBashCommand(tt.cmd)
			if got.Action != tt.wantAction {
				t.Fatalf("action = %v, want %v (got=%+v)", got.Action, tt.wantAction, got)
			}
			switch tt.wantAction {
			case BashActionGrepLike, BashActionFindName:
				if got.Pattern != tt.wantExtra {
					t.Errorf("pattern = %q, want %q", got.Pattern, tt.wantExtra)
				}
			case BashActionReadSource:
				if got.Path != tt.wantExtra {
					t.Errorf("path = %q, want %q", got.Path, tt.wantExtra)
				}
			}
		})
	}
}

func TestPrimarySegments(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{"simple", `grep foo bar`, []string{"grep foo bar"}},
		{"pipe drops tail", `grep foo bar | wc -l`, []string{"grep foo bar"}},
		{"semicolon keeps both", `a; b`, []string{"a", "b"}},
		{"&& keeps both", `a && b`, []string{"a", "b"}},
		{"|| keeps both", `a || b`, []string{"a", "b"}},
		{"pipe then ; resumes primary", `a | b; c`, []string{"a", "c"}},
		{"pipe then && resumes primary", `a | b && c`, []string{"a", "c"}},
		{"quoted pipe char", `grep 'a|b' .`, []string{"grep 'a|b' ."}},
		{"quoted semicolon", `grep 'a;b' .`, []string{"grep 'a;b' ."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := primarySegments(tt.cmd)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d segments %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("segment %d: %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`grep foo bar`, []string{"grep", "foo", "bar"}},
		{`grep "foo bar" .`, []string{"grep", "foo bar", "."}},
		{`grep 'foo bar' .`, []string{"grep", "foo bar", "."}},
		{`  multiple   spaces  `, []string{"multiple", "spaces"}},
		{`grep --include=*.go foo`, []string{"grep", "--include=*.go", "foo"}},
	}
	for _, tt := range tests {
		got := tokenize(tt.in)
		if len(got) != len(tt.want) {
			t.Fatalf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}
