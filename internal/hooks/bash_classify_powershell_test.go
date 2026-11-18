package hooks

import (
	"path/filepath"
	"reflect"
	"testing"
)

// PowerShell read, search and find cmdlets get the verdict their POSIX
// counterparts get (#1096). The rows are the issue's table plus the
// parameter spellings the classifier has to read.
func TestClassifyPowerShellReadCommands(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		wantAction BashAction
		wantExtra  string // pattern for Grep/Find, path for ReadSource
	}{
		// --- Get-Content and its aliases ---
		{"Get-Content", `Get-Content internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"gc", `gc internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"type", `type internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"lowercase get-content", `get-content internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-Path with -TotalCount", `Get-Content -Path internal/hooks/pi.go -TotalCount 20`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-TotalCount before the path", `Get-Content -TotalCount 20 internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-Head", `Get-Content internal/hooks/pi.go -Head 5`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-Tail", `gc -Tail 50 internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-LiteralPath", `Get-Content -LiteralPath 'internal/hooks/pi.go' -Raw`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-Path: attached", `Get-Content -Path:internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"parameter names are case-insensitive", `Get-Content -PATH internal/hooks/pi.go -totalcount 20`, BashActionReadSource, "internal/hooks/pi.go"},
		{"Windows path", `Get-Content C:\repo\internal\x.go`, BashActionReadSource, `C:\repo\internal\x.go`},
		{"Get-Content of a non-source file", `Get-Content notes.txt`, BashActionPassthrough, ""},
		{"Get-Content of a comma list", `Get-Content a.go,b.go`, BashActionPassthrough, ""},
		{"Get-Content with no path", `Get-Content -Raw`, BashActionPassthrough, ""},
		{"-ErrorAction value before -Path", `Get-Content -ErrorAction Stop -Path internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-ea value before the positional", `gc -ea SilentlyContinue internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-ErrorAction: attached", `Get-Content -ErrorAction:Stop internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"-OutVariable value is not the file", `Get-Content -OutVariable out.go -Raw`, BashActionPassthrough, ""},
		{"-Path after a positional wins", `Get-Content notes.txt -Path internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},

		// --- POSIX names matched case-insensitively ---
		{"Cat", `Cat internal/hooks/pi.go`, BashActionReadSource, "internal/hooks/pi.go"},
		{"GREP", `GREP -n bridgeToolCall internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},

		// --- Select-String and sls ---
		{"Select-String -Path -Pattern", `Select-String -Path internal/hooks/pi.go -Pattern bridgeToolCall`, BashActionGrepLike, "bridgeToolCall"},
		{"Select-String -Pattern first", `Select-String -Pattern bridgeToolCall -Path internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},
		{"sls positional", `sls bridgeToolCall internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},
		{"sls switches and -Context", `sls -SimpleMatch -Context 2 bridgeToolCall internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},
		{"-Pattern: attached", `Select-String -Pattern:bridgeToolCall internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},
		{"Select-String without a pattern", `Select-String -CaseSensitive`, BashActionPassthrough, ""},
		{"positional path before -Pattern", `Select-String internal/hooks/pi.go -Pattern bridgeToolCall`, BashActionGrepLike, "bridgeToolCall"},
		{"-ErrorAction value is not the pattern", `sls -ErrorAction SilentlyContinue bridgeToolCall internal/hooks/pi.go`, BashActionGrepLike, "bridgeToolCall"},
		{"-InputObject searches text", `Select-String -InputObject 'hello' -Pattern bridgeToolCall`, BashActionPassthrough, ""},
		{"-InputObject: attached", `sls -InputObject:hello bridgeToolCall`, BashActionPassthrough, ""},
		{"Select-String after a pipe is a filter", `go test ./... | Select-String FAIL`, BashActionPassthrough, ""},

		// --- Get-ChildItem -Recurse -Filter ---
		{"Get-ChildItem -Recurse -Filter", `Get-ChildItem -Recurse -Filter pi.go`, BashActionFindName, "pi.go"},
		{"gci -Filter glob", `gci -Filter 'Handler*' -Recurse`, BashActionFindName, "Handler"},
		{"dir -Recurse -Include", `dir -Recurse -Include *.go`, BashActionFindName, ".go"},
		{"ls -Recurse -Filter", `ls -Recurse -Filter pi.go`, BashActionFindName, "pi.go"},
		{"Get-ChildItem -Path . -Recurse -Filter", `Get-ChildItem -Path . -Recurse -Filter pi.go`, BashActionFindName, "pi.go"},
		{"Get-ChildItem without -Recurse", `Get-ChildItem -Filter pi.go`, BashActionPassthrough, ""},
		{"Get-ChildItem -Recurse without a filter", `Get-ChildItem -Recurse`, BashActionPassthrough, ""},
		{"POSIX ls keeps its classification", `ls -1A /repo`, BashActionFileList, ""},
		{"POSIX ls -la stays passthrough", `ls -la /repo`, BashActionPassthrough, ""},
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

// Select-String reports the paths it searches, so a search outside every
// tracked repo stays silent like grep's does (#893).
func TestSelectStringSearchPaths(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{`Select-String -Path internal/hooks/pi.go -Pattern x`, []string{filepath.Join("/repo", "internal/hooks/pi.go")}},
		{`sls x a.go b.go`, []string{filepath.Join("/repo", "a.go"), filepath.Join("/repo", "b.go")}},
		{`sls x -Path a.go,b.go`, []string{filepath.Join("/repo", "a.go"), filepath.Join("/repo", "b.go")}},
		{`sls x`, nil},
		{`Select-String a.go -Pattern x`, []string{filepath.Join("/repo", "a.go")}},
		{`Select-String a.go b.go -Pattern x`, []string{filepath.Join("/repo", "a.go"), filepath.Join("/repo", "b.go")}},
		{`sls -ea Stop x a.go`, []string{filepath.Join("/repo", "a.go")}},
		{`sls x $env:USERPROFILE/notes`, nil},
	}
	for _, tc := range cases {
		got := classifyBashReadCommand("cd /repo && " + tc.cmd)
		if got.Action != BashActionGrepLike {
			t.Fatalf("%q: action = %v, want GrepLike", tc.cmd, got.Action)
		}
		if !reflect.DeepEqual(got.Paths, tc.want) {
			t.Errorf("%q: paths = %q, want %q", tc.cmd, got.Paths, tc.want)
		}
	}
}

// End to end through enrich: the cmdlets now reach the deny `cat` and `grep`
// of the same indexed file get, and a Select-String of an untracked path
// stays silent.
func TestPowerShellReadsReachBashPolicy(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubProbe(t, []grepSymbolHit{{Name: "bridgeToolCall", FilePath: "internal/hooks/pi.go", Line: 1}}, nil)
	stubIndexedFile(t, true, 9)

	notes := filepath.Join(elsewhereFixtureRoot, "notes")
	cases := []struct {
		command string
		deny    bool
	}{
		{"Get-Content internal/hooks/pi.go", true},
		{"gc internal/hooks/pi.go", true},
		{"type internal/hooks/pi.go", true},
		{"Cat internal/hooks/pi.go", true},
		{"Get-Content -Path internal/hooks/pi.go -TotalCount 20", true},
		{"Select-String -Path internal/hooks/pi.go -Pattern bridgeToolCall", true},
		{"sls bridgeToolCall internal/hooks/pi.go", true},
		{"sls bridgeToolCall " + notes, false},
		{"Select-String " + notes + " -Pattern bridgeToolCall", false},
		{"Select-String -InputObject 'hello' -Pattern bridgeToolCall", false},
		{"Get-Content -ErrorAction Stop -Path internal/hooks/pi.go", true},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			result := enrich(HookInput{
				ToolName:  "Bash",
				ToolInput: map[string]any{"command": tc.command},
				CWD:       trackedFixtureRoot,
			}, 0)
			if result.deny != tc.deny {
				t.Fatalf("%q: deny=%v, want %v (%#v)", tc.command, result.deny, tc.deny, result)
			}
		})
	}
}
