package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestGlobalGortexMCPEntryUsesProxy verifies that the user-level MCP
// entry is daemon-only — no cwd-relative `--index .` arg. This is the
// load-bearing property for gortexhq/gortex#19: Cursor launches the
// global config with cwd=$HOME, so any cwd-relative indexing would
// fail the entry-point handshake.
func TestGlobalGortexMCPEntryUsesProxy(t *testing.T) {
	entry := GlobalGortexMCPEntry()

	cmd, _ := entry["command"].(string)
	if cmd != "gortex" {
		t.Fatalf("command = %q, want %q", cmd, "gortex")
	}

	args, _ := entry["args"].([]string)
	if len(args) == 0 || args[0] != "mcp" {
		t.Fatalf("args[0] = %v, want mcp; full args=%v", args, args)
	}
	// The canonical shape is bare ["mcp"]: no cwd-relative --index/--watch
	// (the daemon resolves the workspace per session) and no --proxy
	// (gortex mcp proxies to / auto-starts the daemon on its own).
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("global entry must emit canonical [mcp], got %v", args)
	}
}

// TestIsGortexAuthoredMCPEntry checks the heuristic used by
// UpsertMCPServerWithMigration to decide which existing stanzas are
// safe to overwrite during a global-install migration.
func TestIsGortexAuthoredMCPEntry(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "legacy --index . shape (the bug from issue 19)",
			raw:  `{"command":"gortex","args":["mcp","--index",".","--watch"]}`,
			want: true,
		},
		{
			name: "current --proxy shape",
			raw:  `{"command":"gortex","args":["mcp","--proxy"]}`,
			want: true,
		},
		{
			name: "user wrapper that shells gortex through a script",
			raw:  `{"command":"/usr/local/bin/wrap-gortex.sh","args":["mcp"]}`,
			want: false,
		},
		{
			name: "user's own MCP server, not gortex at all",
			raw:  `{"command":"other-tool","args":["serve"]}`,
			want: false,
		},
		{
			name: "gortex command but a different subcommand",
			raw:  `{"command":"gortex","args":["daemon","start"]}`,
			want: false,
		},
		{
			name: "empty args list",
			raw:  `{"command":"gortex","args":[]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := IsGortexAuthoredMCPEntry(v)
			if got != tc.want {
				t.Errorf("IsGortexAuthoredMCPEntry(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestUpsertMCPServerWithMigrationReplacesLegacy exercises the
// migration path: a user with an old global config containing
// `--index . --watch` should get rewritten to the proxy entry on next
// `gortex install`, even though opts.Force is off.
func TestUpsertMCPServerWithMigrationReplacesLegacy(t *testing.T) {
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "gortex",
				"args":    []any{"mcp", "--index", ".", "--watch"},
				"env":     map[string]any{"GORTEX_INDEX_WORKERS": "8"},
			},
		},
	}
	want := GlobalGortexMCPEntry()

	changed := UpsertMCPServerWithMigration(root, "gortex", want, ApplyOpts{})
	if !changed {
		t.Fatalf("expected migration to rewrite legacy entry, got changed=false")
	}
	servers, _ := root["mcpServers"].(map[string]any)
	got, _ := servers["gortex"].(map[string]any)
	if got["command"] != "gortex" {
		t.Fatalf("command lost during migration: %v", got)
	}
	args, _ := got["args"].([]string)
	for _, a := range args {
		if a == "--index" {
			t.Fatalf("migration left legacy --index in args: %v", got)
		}
	}
	if _, hasEnv := got["env"]; hasEnv {
		t.Fatalf("migration kept the legacy GORTEX_INDEX_WORKERS env: %v", got)
	}
}

// TestUpsertMCPServerWithMigrationIdempotent verifies the second run
// is a no-op once the config is already in the desired shape — no
// spurious rewrite, no noisy "modified" log.
func TestUpsertMCPServerWithMigrationIdempotent(t *testing.T) {
	// Use the on-disk shape (args as []any) to mirror what comes back
	// out of json.Unmarshal on an existing mcp.json.
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "gortex",
				"args":    []any{"mcp"},
			},
		},
	}
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{}); changed {
		t.Errorf("expected no rewrite when entry already matches; got changed=true")
	}
}

// TestUpsertMCPServerWithMigrationPreservesUserCustomization confirms
// the migration is fingerprint-gated: a user who hand-rolled their
// own gortex wrapper (non-`gortex` command, or a different subcommand)
// should not have their config silently overwritten.
func TestUpsertMCPServerWithMigrationPreservesUserCustomization(t *testing.T) {
	root := map[string]any{
		"mcpServers": map[string]any{
			"gortex": map[string]any{
				"command": "/opt/scripts/my-gortex.sh",
				"args":    []any{"mcp"},
			},
		},
	}
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{}); changed {
		t.Errorf("user-customized entry was overwritten without --force")
	}
	// And with --force the same call should overwrite — that's the
	// escape hatch users have when they want Gortex to take over.
	if changed := UpsertMCPServerWithMigration(root, "gortex", GlobalGortexMCPEntry(), ApplyOpts{Force: true}); !changed {
		t.Errorf("opts.Force must overwrite user-customized entry")
	}
}

// TestIsGortexAuthoredMCPEntryAbsolutePath verifies the broadened
// detection: the legacy absolute-path command form a pre-fix `gortex
// install` baked into ~/.claude.json (via os.Executable()) is now
// recognized as Gortex-authored, so the next install can migrate it to
// the canonical bare-`gortex` shape. A user's own wrapper script is
// still left alone. Both / and \ separators are handled regardless of
// the host OS.
func TestIsGortexAuthoredMCPEntryAbsolutePath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "unix absolute path to gortex (legacy os.Executable form)",
			raw:  `{"command":"/usr/local/bin/gortex","args":["mcp"]}`,
			want: true,
		},
		{
			name: "windows absolute path to gortex.exe (the issue #201 form)",
			raw:  `{"command":"C:\\Users\\daoti\\AppData\\Local\\Programs\\gortex\\gortex.exe","args":["mcp"]}`,
			want: true,
		},
		{
			name: "absolute path to a non-gortex binary",
			raw:  `{"command":"/usr/local/bin/something-else","args":["mcp"]}`,
			want: false,
		},
		{
			name: "user wrapper whose basename merely contains gortex",
			raw:  `{"command":"/opt/scripts/my-gortex.sh","args":["mcp"]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := IsGortexAuthoredMCPEntry(v); got != tc.want {
				t.Errorf("IsGortexAuthoredMCPEntry(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestResolveGortexCommandFrom pins the command-resolution policy that
// keeps the user-scope ~/.claude.json entry in agreement with the
// project .mcp.json template (and so avoids Claude Code's "conflicting
// scopes" diagnostic) while staying robust when gortex isn't on PATH.
func TestResolveGortexCommandFrom(t *testing.T) {
	sameTrue := func(a, b string) bool { return true }
	sameFalse := func(a, b string) bool { return false }
	notFound := errors.New("executable file not found in $PATH")

	cases := []struct {
		name     string
		exe      string
		exeErr   error
		lookPath string
		lookErr  error
		same     func(a, b string) bool
		want     string
	}{
		{
			name:     "on PATH and same binary -> bare gortex (matches project template)",
			exe:      "/usr/local/bin/gortex",
			lookPath: "/usr/local/bin/gortex",
			same:     sameTrue,
			want:     "gortex",
		},
		{
			name:     "installed but not on PATH -> pin absolute path",
			exe:      `C:\Users\daoti\AppData\Local\Programs\gortex\gortex.exe`,
			lookPath: "",
			lookErr:  notFound,
			same:     sameFalse,
			want:     `C:\Users\daoti\AppData\Local\Programs\gortex\gortex.exe`,
		},
		{
			name:     "on PATH but a different gortex -> pin the running binary, not the PATH one",
			exe:      "/opt/build/gortex",
			lookPath: "/usr/local/bin/gortex",
			same:     sameFalse,
			want:     "/opt/build/gortex",
		},
		{
			name:     "go run transient temp build + gortex on PATH -> bare gortex",
			exe:      filepath.Join(os.TempDir(), "go-build1234", "agents.test"),
			lookPath: "/usr/local/bin/gortex",
			same:     sameFalse,
			want:     "gortex",
		},
		{
			name:    "nothing resolvable -> bare gortex last resort",
			exe:     "",
			exeErr:  errors.New("os.Executable failed"),
			lookErr: notFound,
			same:    sameFalse,
			want:    "gortex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGortexCommandFrom(tc.exe, tc.exeErr, tc.lookPath, tc.lookErr, tc.same)
			if got != tc.want {
				t.Errorf("resolveGortexCommandFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveGortexHookBinaryFrom mirrors TestResolveGortexCommandFrom's
// table for the hook resolver, and additionally asserts the load-bearing
// invariant: the hook binary and the MCP stanza launch the SAME on-disk
// file in every case. The hook only prefers an absolute path over the
// bare name (it fires in a shell whose PATH may differ from install
// time); it must never point at a different gortex than the MCP server.
func TestResolveGortexHookBinaryFrom(t *testing.T) {
	sameTrue := func(a, b string) bool { return true }
	sameFalse := func(a, b string) bool { return false }
	notFound := errors.New("executable file not found in $PATH")

	cases := []struct {
		name     string
		exe      string
		exeErr   error
		lookPath string
		lookErr  error
		same     func(a, b string) bool
		want     string
	}{
		{
			name:     "on PATH and same binary -> pin absolute (hook avoids PATH at fire time)",
			exe:      "/usr/local/bin/gortex",
			lookPath: "/usr/local/bin/gortex",
			same:     sameTrue,
			want:     "/usr/local/bin/gortex",
		},
		{
			name:     "installed but not on PATH -> pin absolute path",
			exe:      `C:\Users\daoti\AppData\Local\Programs\gortex\gortex.exe`,
			lookPath: "",
			lookErr:  notFound,
			same:     sameFalse,
			want:     `C:\Users\daoti\AppData\Local\Programs\gortex\gortex.exe`,
		},
		{
			name:     "on PATH but a different gortex -> pin the running binary, not the PATH one",
			exe:      "/opt/build/gortex",
			lookPath: "/usr/local/bin/gortex",
			same:     sameFalse,
			want:     "/opt/build/gortex",
		},
		{
			name:     "go run transient temp build + gortex on PATH -> PATH gortex, never the temp build",
			exe:      filepath.Join(os.TempDir(), "go-build1234", "agents.test"),
			lookPath: "/usr/local/bin/gortex",
			same:     sameFalse,
			want:     "/usr/local/bin/gortex",
		},
		{
			name:    "nothing resolvable -> bare gortex last resort",
			exe:     "",
			exeErr:  errors.New("os.Executable failed"),
			lookErr: notFound,
			same:    sameFalse,
			want:    "gortex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGortexHookBinaryFrom(tc.exe, tc.exeErr, tc.lookPath, tc.lookErr, tc.same)
			if got != tc.want {
				t.Errorf("resolveGortexHookBinaryFrom() = %q, want %q", got, tc.want)
			}
			// Map each resolver output to the concrete file it launches:
			// an absolute path is itself; the bare name resolves via PATH
			// to lookPath. The two must never disagree.
			launched := func(resolved string) string {
				if resolved == "gortex" {
					return tc.lookPath
				}
				return resolved
			}
			mcp := resolveGortexCommandFrom(tc.exe, tc.exeErr, tc.lookPath, tc.lookErr, tc.same)
			if hf, mf := launched(got), launched(mcp); hf != mf {
				t.Errorf("hook/MCP disagree on binary file: hook %q -> %q, mcp %q -> %q", got, hf, mcp, mf)
			}
		})
	}
}
