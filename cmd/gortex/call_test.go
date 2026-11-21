package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// newCallTestCmd builds a fresh call command bound to an out/err buffer and
// resets the package-level flag state to defaults so tests don't leak.
func newCallTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	callIndex = "."
	callJSON = ""
	callJSONFile = ""
	callArgs = nil
	callFormat = "json"
	callDry = false
	callQuiet = false
	callLegacy = false

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "call", RunE: runCall}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

func TestCall_ReportsRepoLoadingRefusal(t *testing.T) {
	original := callDaemonTool
	t.Cleanup(func() { callDaemonTool = original })

	const loading = `{"error_code":"repo_loading","retryable":true,"repo":"app","phase":"parallel_parse","percent":15}`
	const identityPending = `{"error_code":"repo_loading","retryable":true,"reason":"identity_pending","phase":"identity_resolution","percent":0}`
	tests := []struct {
		name    string
		data    string
		json    string
		message []string
	}{
		{name: "default", data: loading, message: []string{"still loading app", "parallel_parse, 15%", "error_code=repo_loading", "retryable=true", "retry shortly"}},
		{name: "identity pending", data: identityPending, message: []string{"resolving graph admission", "identity_pending", "identity_resolution, 0%", "error_code=repo_loading", "retryable=true", "retry shortly"}},
		{name: "explicit JSON", data: loading, json: `{"error_code":"repo_loading","retryable":true,"phase":"parallel_parse","percent":15}`},
		{name: "explicit JSON with zero progress", data: identityPending, json: `{"error_code":"repo_loading","retryable":true,"phase":"identity_resolution","percent":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
				if tool == "tool_profile" {
					return json.RawMessage(cannedToolProfileJSON), nil
				}
				frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"repository loading","data":%s}}`, tt.data)
				return extractToolResult([]byte(frame))
			}
			cmd, stdout := newCallTestCmd(t)
			cmd.SilenceUsage = true
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			cmd.Flags().StringVar(&callFormat, "format", "json", "")
			args := []string{"search_symbols"}
			if tt.json != "" {
				args = append(args, "--format", "json")
			}
			cmd.SetArgs(args)
			var coded interface{ ExitCode() int }
			require.ErrorAs(t, cmd.Execute(), &coded)
			require.Equal(t, exitTempFail, coded.ExitCode())
			require.Empty(t, stdout.String())
			require.NotContains(t, stderr.String(), "gortex track")
			require.NotContains(t, stderr.String(), "gortex repos reconcile")
			if tt.json != "" {
				require.JSONEq(t, tt.json, stderr.String())
			}
			for _, fragment := range tt.message {
				require.Contains(t, stderr.String(), fragment)
			}
		})
	}
}

// TestCoerceScalar exhaustively covers the deterministic --arg value coercion.
func TestCoerceScalar(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"3.14", 3.14},
		{"", ""},
		{"hello", "hello"},
		{"1.0.0", "1.0.0"}, // not a number — stays a string
		{`[1,2,3]`, []any{float64(1), float64(2), float64(3)}},
		{`{"k":"v"}`, map[string]any{"k": "v"}},
		{`[broken`, `[broken`}, // invalid JSON array — falls back to literal string
	}
	for _, c := range cases {
		got := coerceScalar(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("coerceScalar(%q) = %#v (%T), want %#v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}

// TestCoerceArg covers the key/value split, the := walrus raw-JSON form, and
// error cases.
func TestCoerceArg(t *testing.T) {
	t.Run("plain string", func(t *testing.T) {
		k, v, err := coerceArg("name=foo")
		require.NoError(t, err)
		require.Equal(t, "name", k)
		require.Equal(t, "foo", v)
	})
	t.Run("bool", func(t *testing.T) {
		k, v, err := coerceArg("flag=true")
		require.NoError(t, err)
		require.Equal(t, "flag", k)
		require.Equal(t, true, v)
	})
	t.Run("int", func(t *testing.T) {
		_, v, err := coerceArg("limit=50")
		require.NoError(t, err)
		require.Equal(t, int64(50), v)
	})
	t.Run("empty value", func(t *testing.T) {
		k, v, err := coerceArg("note=")
		require.NoError(t, err)
		require.Equal(t, "note", k)
		require.Equal(t, "", v)
	})
	t.Run("walrus forces raw string", func(t *testing.T) {
		// version:="1.0" must stay the string "1.0", not the number 1.0.
		k, v, err := coerceArg(`version:="1.0"`)
		require.NoError(t, err)
		require.Equal(t, "version", k)
		require.Equal(t, "1.0", v)
	})
	t.Run("walrus parses raw JSON object", func(t *testing.T) {
		k, v, err := coerceArg(`opts:={"a":1}`)
		require.NoError(t, err)
		require.Equal(t, "opts", k)
		require.Equal(t, map[string]any{"a": float64(1)}, v)
	})
	t.Run("walrus invalid JSON errors", func(t *testing.T) {
		_, _, err := coerceArg(`x:=not-json`)
		require.Error(t, err)
	})
	t.Run("missing equals errors", func(t *testing.T) {
		_, _, err := coerceArg("noequals")
		require.Error(t, err)
	})
	t.Run("empty key errors", func(t *testing.T) {
		_, _, err := coerceArg("=value")
		require.Error(t, err)
	})
}

// TestCoerceArg_SeparatorChosenByKeyPosition pins the rule that decides which
// separator splits a token: the walrus applies only when ":=" opens the token's
// key, so a VALUE that quotes ":=" (a pasted VB/Go snippet, a Pascal
// assignment) is carried through verbatim instead of being split mid-value and
// rejected as invalid JSON.
func TestCoerceArg_SeparatorChosenByKeyPosition(t *testing.T) {
	t.Run("value containing walrus stays a string", func(t *testing.T) {
		k, v, err := coerceArg("key=a:=b")
		require.NoError(t, err)
		require.Equal(t, "key", k)
		require.Equal(t, "a:=b", v)
	})
	t.Run("walrus in key position parses JSON", func(t *testing.T) {
		k, v, err := coerceArg(`key:={"x":1}`)
		require.NoError(t, err)
		require.Equal(t, "key", k)
		require.Equal(t, map[string]any{"x": float64(1)}, v)
	})
	t.Run("plain key=value is unchanged", func(t *testing.T) {
		k, v, err := coerceArg("key=val")
		require.NoError(t, err)
		require.Equal(t, "key", k)
		require.Equal(t, "val", v)
	})
	t.Run("value containing both separators is verbatim", func(t *testing.T) {
		// A pasted issue body: the first "=" ends the key, everything after it
		// — further "=" signs and the ":=" walrus alike — is the value.
		k, v, err := coerceArg("task=Dim x := 5 : y = x + 1")
		require.NoError(t, err)
		require.Equal(t, "task", k)
		require.Equal(t, "Dim x := 5 : y = x + 1", v)
	})
	t.Run("walrus still wins when it opens the token", func(t *testing.T) {
		k, v, err := coerceArg(`note:="a=b:=c"`)
		require.NoError(t, err)
		require.Equal(t, "note", k)
		require.Equal(t, "a=b:=c", v)
	})
}

// TestLowerCallArgs_Precedence asserts the three precedence layers: base JSON,
// inline JSON merged over it, and --arg merged on top (last wins per key).
func TestLowerCallArgs_Precedence(t *testing.T) {
	cmd, _ := newCallTestCmd(t)

	t.Run("inline json plus arg override", func(t *testing.T) {
		obj, err := lowerCallArgs(cmd, `{"a":1,"b":2}`, "", []string{"b=3"})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"a": float64(1), "b": int64(3)}, obj)
	})

	t.Run("later arg replaces earlier same key", func(t *testing.T) {
		obj, err := lowerCallArgs(cmd, "", "", []string{"k=first", "k=second"})
		require.NoError(t, err)
		require.Equal(t, "second", obj["k"])
	})

	t.Run("arg over inline json over file base", func(t *testing.T) {
		// Write a base file, then merge inline JSON and --arg over it.
		base := writeTempJSON(t, `{"a":"file","b":"file","c":"file"}`)
		obj, err := lowerCallArgs(cmd, `{"b":"inline","c":"inline"}`, base, []string{"c=arg"})
		require.NoError(t, err)
		require.Equal(t, "file", obj["a"])   // only in the file base
		require.Equal(t, "inline", obj["b"]) // inline beats file
		require.Equal(t, "arg", obj["c"])    // arg beats inline + file
	})
}

// TestLowerCallArgs_StdinBase asserts --json - reads the base object from stdin.
func TestLowerCallArgs_StdinBase(t *testing.T) {
	cmd, _ := newCallTestCmd(t)
	cmd.SetIn(strings.NewReader(`{"from":"stdin","n":1}`))
	obj, err := lowerCallArgs(cmd, "-", "", []string{"n=2"})
	require.NoError(t, err)
	require.Equal(t, "stdin", obj["from"])
	require.Equal(t, int64(2), obj["n"])
}

// TestLowerCallArgs_NonObjectRejected asserts a non-object base JSON is refused.
func TestLowerCallArgs_NonObjectRejected(t *testing.T) {
	cmd, _ := newCallTestCmd(t)
	_, err := lowerCallArgs(cmd, `[1,2,3]`, "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "JSON object")
}

// TestCallDry_NoDaemon asserts --dry prints the lowered object and the target
// tool WITHOUT invoking the daemon (the stub records any call).
func TestCallDry_NoDaemon(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })

	called := false
	callDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		called = true
		return nil, nil
	}

	cmd, buf := newCallTestCmd(t)
	callDry = true
	callJSON = `{"id":"pkg/foo.go::Bar"}`
	callArgs = []string{"depth=2", "verbose=true"}

	require.NoError(t, runCall(cmd, []string{"get_callers"}))
	require.False(t, called, "--dry must NOT invoke the daemon (not even tool_profile)")

	out := buf.String()
	require.Contains(t, out, "tool: get_callers")
	// The lowered object is printed as indented JSON.
	require.Contains(t, out, `"id": "pkg/foo.go::Bar"`)
	require.Contains(t, out, `"depth": 2`)
	require.Contains(t, out, `"verbose": true`)
}

func TestCall_AllCompactNamesUseFacadeRelay(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() {
		callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade
	})

	facadeNames := []string{
		"analyze", "ask", "capabilities", "change", "edit", "explore",
		"overlay", "pr", "publish_review", "read", "recall", "refactor",
		"relations", "remember", "response", "review", "search", "session",
		"trace", "workspace", "workspace_admin",
	}
	for _, name := range facadeNames {
		require.True(t, gortexmcp.IsFacadeToolName(name), "%q must remain a registered facade name", name)
		t.Run(name, func(t *testing.T) {
			var facadeTool string
			callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
				t.Fatalf("compact call %q touched legacy relay with %q", name, tool)
				return nil, nil
			}
			callFacadeDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
				facadeTool = tool
				return json.RawMessage(`{"ok":true}`), nil
			}

			cmd, _ := newCallTestCmd(t)
			require.NoError(t, runCall(cmd, []string{name}))
			require.Equal(t, name, facadeTool)
		})
	}
}

func TestCall_HelpUsesOneClientNeutralShellContract(t *testing.T) {
	help := strings.ToLower(callCmd.Long)
	require.Contains(t, help, "gortex call capabilities")
	require.NotContains(t, help, "facade-v1")
	for _, client := range []string{"codex", "claude", "cursor", "vscode"} {
		require.NotContains(t, help, client)
	}
	require.False(t, isKnownRootCommand("facade"), "compact calls must not create a second top-level CLI contract")
}

func TestCall_CompactRequestObjectIsForwardedUnchanged(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() {
		callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade
	})
	callDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		t.Fatal("compact call must not use legacy relay")
		return nil, nil
	}
	var got map[string]any
	callFacadeDaemonTool = func(_ string, tool string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		require.Equal(t, "read", tool)
		got = args
		return json.RawMessage(`{"ok":true}`), nil
	}

	cmd, _ := newCallTestCmd(t)
	callArgs = []string{"operation=file", `target:={"file":"internal/mcp/server.go"}`}
	require.NoError(t, runCall(cmd, []string{"read"}))
	require.Equal(t, "file", got["operation"])
	require.Equal(t, map[string]any{"file": "internal/mcp/server.go"}, got["target"])
	require.NotContains(t, got, "format")
	require.NotContains(t, got, "output", "the default CLI format must not rewrite an exact compact MCP request")
}

func TestCall_CompactDefaultPreservesRequestOutputFormat(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() { callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade })
	var got map[string]any
	callFacadeDaemonTool = func(_ string, _ string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		got = args
		return json.RawMessage("TOON|results|...\n"), nil
	}

	cmd, buf := newCallTestCmd(t)
	callJSON = `{"operation":"symbols","query":"Server","output":{"format":"toon","limit":7}}`
	require.NoError(t, runCall(cmd, []string{"search"}))
	output := got["output"].(map[string]any)
	require.Equal(t, "toon", output["format"])
	require.Equal(t, float64(7), output["limit"])
	require.Equal(t, "TOON|results|...\n", buf.String())
}

// exploreTaskMarkdown is the shape explore(operation:"task") returns: the
// human-oriented localization page, which the tool renders as markdown and
// which carries no structured payload of its own.
const exploreTaskMarkdown = "# Task: retry backoff\n\n## Candidates\n\n1. `Client.do` — internal/http/client.go:42\n"

// TestCall_JSONFormatAlwaysEmitsJSON pins the --format json contract at the
// only layer that promises it: whatever the tool rendered, stdout parses as
// JSON. explore(operation:"task") is the case in the core surface that renders
// markdown, so a JSON client used to get a parse error instead of a response.
func TestCall_JSONFormatAlwaysEmitsJSON(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() { callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade })

	t.Run("explore task markdown is wrapped in an envelope", func(t *testing.T) {
		callFacadeDaemonTool = func(_ string, _ string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
			return json.RawMessage(exploreTaskMarkdown), nil
		}
		cmd, buf := newCallTestCmd(t)
		callArgs = []string{"operation=task", "task=why does the retry backoff never fire"}
		require.NoError(t, runCall(cmd, []string{"explore"}))

		var envelope map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &envelope),
			"--format json must emit parseable JSON, got: %s", buf.String())
		require.Equal(t, "explore", envelope["tool"])
		require.Equal(t, "text", envelope["format"])
		require.Equal(t, exploreTaskMarkdown, envelope["text"])
	})

	t.Run("structured payloads pass through unwrapped", func(t *testing.T) {
		callFacadeDaemonTool = func(_ string, _ string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
			return json.RawMessage(`{"completion":{"state":"localized"},"symbols":["a"]}`), nil
		}
		cmd, buf := newCallTestCmd(t)
		callArgs = []string{"operation=localize", "task=where is the retry backoff"}
		require.NoError(t, runCall(cmd, []string{"explore"}))

		var payload map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &payload))
		require.Contains(t, payload, "completion", "a JSON tool response must not be re-wrapped")
		require.NotContains(t, payload, "text")
	})

	t.Run("legacy relay text is wrapped too", func(t *testing.T) {
		callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
			if tool == "tool_profile" {
				return json.RawMessage(cannedToolProfileJSON), nil
			}
			return json.RawMessage("plain prose, not JSON\n"), nil
		}
		cmd, buf := newCallTestCmd(t)
		require.NoError(t, runCall(cmd, []string{"search_symbols"}))

		var envelope map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &envelope),
			"--format json must emit parseable JSON, got: %s", buf.String())
		require.Equal(t, "search_symbols", envelope["tool"])
		require.Equal(t, "plain prose, not JSON\n", envelope["text"])
	})

	t.Run("format text keeps the rendered page verbatim", func(t *testing.T) {
		callFacadeDaemonTool = func(_ string, _ string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
			return json.RawMessage(exploreTaskMarkdown), nil
		}
		cmd, buf := newCallTestCmd(t)
		callFormat = "text"
		callArgs = []string{"operation=task", "task=why does the retry backoff never fire"}
		require.NoError(t, runCall(cmd, []string{"explore"}))
		require.Equal(t, exploreTaskMarkdown+"\n", buf.String())
	})
}

func TestCall_CompactDryRunShowsFinalExplicitFormat(t *testing.T) {
	cmd, buf := newCallTestCmd(t)
	callDry = true
	callFormat = "gcx"
	callJSON = `{"operation":"symbols","query":"Server","output":{"format":"toon"}}`
	require.NoError(t, runCall(cmd, []string{"search"}))
	require.Contains(t, buf.String(), `"format": "gcx"`)
	require.NotContains(t, buf.String(), `"format": "toon"`)
}

func TestCall_CompactFormatMergesIntoExistingOutput(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() {
		callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade
	})
	callDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		t.Fatal("compact call must not use legacy relay")
		return nil, nil
	}
	var got map[string]any
	callFacadeDaemonTool = func(_ string, _ string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		got = args
		return json.RawMessage(`{"ok":true}`), nil
	}

	cmd, _ := newCallTestCmd(t)
	callFormat = "gcx"
	callJSON = `{"operation":"symbols","query":"Server","output":{"limit":7,"fields":"id,name","format":"toon"}}`
	require.NoError(t, runCall(cmd, []string{"search"}))
	output, ok := got["output"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gcx", output["format"], "CLI --format must win")
	require.Equal(t, float64(7), output["limit"])
	require.Equal(t, "id,name", output["fields"])
	require.NotContains(t, got, "format")
}

func TestCall_EffectfulCompactToolWarnsWithoutCatalog(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() {
		callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade
	})
	callDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		t.Fatal("compact call must not use legacy relay")
		return nil, nil
	}
	callFacadeDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}

	cmd, buf := newCallTestCmd(t)
	require.NoError(t, runCall(cmd, []string{"session"}))
	require.Contains(t, buf.String(), "can change session state")

	cmd, buf = newCallTestCmd(t)
	callQuiet = true
	require.NoError(t, runCall(cmd, []string{"session"}))
	require.NotContains(t, buf.String(), "note:")
}

func TestCall_LegacyNameUsesLegacyRelay(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() {
		callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade
	})
	var called []string
	callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
		called = append(called, tool)
		if tool == "tool_profile" {
			return json.RawMessage(cannedToolProfileJSON), nil
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	callFacadeDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		t.Fatal("legacy name must not use the compact relay")
		return nil, nil
	}

	cmd, _ := newCallTestCmd(t)
	require.NoError(t, runCall(cmd, []string{"search_symbols"}))
	require.Equal(t, []string{"tool_profile", "search_symbols"}, called)
}

func TestCall_LegacyEscapePreservesSharedAnalyzeContract(t *testing.T) {
	origLegacy, origFacade := callDaemonTool, callFacadeDaemonTool
	t.Cleanup(func() { callDaemonTool, callFacadeDaemonTool = origLegacy, origFacade })
	var called []string
	callDaemonTool = func(_ string, tool string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		called = append(called, tool)
		if tool == "tool_profile" {
			return json.RawMessage(`{"live":["analyze"],"descriptors":[{"name":"analyze","mutating":false}]}`), nil
		}
		require.Equal(t, "analyze", tool)
		require.Equal(t, "temporal_verify", args["kind"])
		return json.RawMessage(`{"ok":true}`), nil
	}
	callFacadeDaemonTool = func(string, string, map[string]any, io.Writer) (json.RawMessage, error) {
		t.Fatal("--legacy must not negotiate the compact surface")
		return nil, nil
	}

	cmd, _ := newCallTestCmd(t)
	callLegacy = true
	callArgs = []string{"kind=temporal_verify"}
	require.NoError(t, runCall(cmd, []string{"analyze"}))
	require.Equal(t, []string{"tool_profile", "analyze"}, called)
}

// cannedToolProfileJSON is a minimal tool_profile response with a descriptors
// array covering a read tool and a mutating tool.
const cannedToolProfileJSON = `{
  "live": ["search_symbols", "get_callers"],
  "deferred": ["edit_file", "rename_symbol"],
  "descriptors": [
    {"name":"search_symbols","category":"nav","mutating":false,"presets":["core","nav","readonly"],"summary":"BM25 symbol search."},
    {"name":"get_callers","category":"nav","mutating":false,"presets":["core","nav","readonly"],"summary":"Who calls a function."},
    {"name":"edit_file","category":"edit","mutating":true,"presets":["core","edit"],"summary":"Edit a file."},
    {"name":"rename_symbol","category":"edit","mutating":true,"presets":["edit"],"summary":"Rename across files."}
  ]
}`

// TestCall_UnknownToolSuggestsNearest asserts that, when a daemon is reachable
// and returns a catalog, an unknown tool name yields an error listing the
// nearest matches plus the `gortex tools search` hint.
func TestCall_UnknownToolSuggestsNearest(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })

	var calledTools []string
	callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
		calledTools = append(calledTools, tool)
		if tool == "tool_profile" {
			return json.RawMessage(cannedToolProfileJSON), nil
		}
		t.Fatalf("must not call %q after a failed name validation", tool)
		return nil, nil
	}

	cmd, _ := newCallTestCmd(t)
	err := runCall(cmd, []string{"get_caller"}) // typo of get_callers
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown tool")
	require.Contains(t, err.Error(), "get_callers", "should suggest the nearest match")
	require.Contains(t, err.Error(), "gortex tools search")
	require.Equal(t, []string{"tool_profile"}, calledTools, "only the catalog fetch should happen")
}

// TestCall_KnownMutatingToolWarns asserts a known mutating tool both forwards
// the call and prints the stderr write note (unless --quiet).
func TestCall_KnownMutatingToolWarns(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })

	callDaemonTool = func(_ string, tool string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		if tool == "tool_profile" {
			return json.RawMessage(cannedToolProfileJSON), nil
		}
		require.Equal(t, "edit_file", tool)
		require.Equal(t, "json", args["format"])
		return json.RawMessage(`{"status":"ok"}`), nil
	}

	cmd, buf := newCallTestCmd(t)
	require.NoError(t, runCall(cmd, []string{"edit_file"}))
	out := buf.String()
	require.Contains(t, out, "note: edit_file writes")
	require.Contains(t, out, `"status": "ok"`)
}

// TestCall_QuietSuppressesMutatingNote asserts --quiet hides the write note.
func TestCall_QuietSuppressesMutatingNote(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })
	callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
		if tool == "tool_profile" {
			return json.RawMessage(cannedToolProfileJSON), nil
		}
		return json.RawMessage(`{}`), nil
	}

	cmd, buf := newCallTestCmd(t)
	callQuiet = true
	require.NoError(t, runCall(cmd, []string{"edit_file"}))
	require.NotContains(t, buf.String(), "note:")
}

// TestCall_NoDaemonSkipsValidation asserts that when the catalog fetch fails
// (no daemon), validation is skipped and the normal call path runs — surfacing
// whatever the call returns rather than an "unknown tool" error.
func TestCall_NoDaemonSkipsValidation(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })

	callDaemonTool = func(_ string, tool string, _ map[string]any, _ io.Writer) (json.RawMessage, error) {
		if tool == "tool_profile" {
			return nil, daemonRequiredErr(".") // no catalog available
		}
		require.Equal(t, "some_tool", tool, "the call must proceed despite the catalog miss")
		return json.RawMessage(`{"ok":true}`), nil
	}

	cmd, buf := newCallTestCmd(t)
	require.NoError(t, runCall(cmd, []string{"some_tool"}))
	require.Contains(t, buf.String(), `"ok": true`)
}

// TestCall_FormatGCXVerbatim asserts gcx/toon output is printed verbatim, not
// re-indented through emitDaemonJSON.
func TestCall_FormatGCXVerbatim(t *testing.T) {
	orig := callDaemonTool
	t.Cleanup(func() { callDaemonTool = orig })

	callDaemonTool = func(_ string, tool string, args map[string]any, _ io.Writer) (json.RawMessage, error) {
		if tool == "tool_profile" {
			return json.RawMessage(cannedToolProfileJSON), nil
		}
		require.Equal(t, "gcx", args["format"], "the chosen format must be forwarded")
		return json.RawMessage("GCX1|results|...\n"), nil
	}

	cmd, buf := newCallTestCmd(t)
	callFormat = "gcx"
	require.NoError(t, runCall(cmd, []string{"search_symbols"}))
	require.Equal(t, "GCX1|results|...\n", buf.String())
}

// TestCall_DaemonRequired asserts that, with no daemon running and no stub, the
// real call path returns the actionable daemon-required error (mirrors
// query_daemon_required_test.go). The catalog fetch fails first (no daemon), so
// validation is skipped and the call itself surfaces the error.
func TestCall_DaemonRequired(t *testing.T) {
	// Point at a temp dir no daemon could possibly track so resolveExecutor
	// returns ErrNoExecutor -> daemonRequiredErr.
	cmd, _ := newCallTestCmd(t)
	callIndex = t.TempDir()
	err := runCall(cmd, []string{"search_symbols"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "gortex track")
}

// writeTempJSON writes content to a temp file and returns its path.
func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/base.json"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
