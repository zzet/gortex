package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// plugin_node_test.go drives the rendered bridge inside a real JavaScript
// runtime against a stub `gortex hook`.
//
// The string assertions in plugin_test.go can only prove the file *says*
// it fails open. This proves it: a stub that exits non-zero, prints
// garbage, or prints nothing has to leave the tool call untouched, and
// only running the code shows that. The alternative — shipping a bridge
// whose failure mode is "the user's session breaks" — is the one outcome
// that must never reach a machine.
//
// The stub is a POSIX shell script, so this is Unix-only; the rendered
// JavaScript is platform-independent and plugin_test.go's `node --check`
// covers syntax everywhere.

// stubHook is the fake `gortex hook`: it records the envelope it was
// given and answers according to $GORTEX_STUB_MODE.
const stubHook = `#!/bin/sh
cat >> "$GORTEX_STUB_CAPTURE"
printf '\n' >> "$GORTEX_STUB_CAPTURE"
case "$GORTEX_STUB_MODE" in
  block)   printf '%s' '{"block":true,"reason":"[Gortex] call search_symbols instead"}' ;;
  tip)     printf '%s' '{"additional_context":"GRAPH TIP"}' ;;
  orient)  printf '%s' '{"orientation":"BRIEFING","additional_context":"PROMPT CONTEXT"}' ;;
  garbage) printf '%s' 'this is not json' ;;
  crash)   exit 3 ;;
  *)       : ;;
esac
`

// driverV1 exercises every hook the OpenCode 1.18 entrypoint returns and
// reports what happened, so one node run covers the whole surface.
const driverV1 = `import plugin from "./gortex.mjs";

const hooks = await plugin.server({ directory: process.cwd(), worktree: process.cwd() });
const result = { threw: null, toolOutput: null, promptText: null, permission: null };

try {
  await hooks["tool.execute.before"](
    { tool: "read", sessionID: "s1", callID: "c1" },
    { args: { filePath: "/repo/main.go" } },
  );
} catch (err) {
  result.threw = err.message;
}

const toolResult = { title: "read", output: "FILE CONTENTS", metadata: {} };
await hooks["tool.execute.after"]({ tool: "read", sessionID: "s1", callID: "c1", args: {} }, toolResult);
result.toolOutput = toolResult.output;

const parts = [{ type: "text", text: "why is login failing" }];
await hooks["chat.message"]({}, { message: { role: "user", sessionID: "s1" }, parts });
result.promptText = parts[0].text;

const permission = { status: "ask" };
await hooks["permission.ask"](
  { type: "bash", sessionID: "s1", callID: "c2", metadata: { command: "rm -rf /" } },
  permission,
);
result.permission = permission.status;

console.log(JSON.stringify(result));
`

// driverV2 drives the OpenCode 2 entrypoint the way the host does: setup
// receives a ctx whose domains collect hook registrations, and each hook
// is then called with an event shaped like @opencode/plugin's types
// (tool execute.before/after, permission evaluate, session prompt). The
// result is reported in driverV1's shape so the same checks apply.
const driverV2 = `import plugin from "./gortex.mjs";

const hooks = {};
const domain = (name) => ({
  hook: async (hook, callback) => {
    hooks[name + ":" + hook] = callback;
    return { dispose: async () => {} };
  },
});
const cwd = process.cwd();
await plugin.setup({
  location: { directory: cwd, project: { id: "p", directory: cwd, canonical: cwd } },
  tool: domain("tool"),
  permission: domain("permission"),
  session: domain("session"),
});
const result = { threw: null, toolOutput: null, promptText: null, permission: null, structured: null };

const base = { tool: "read", sessionID: "s1", agent: "build", messageID: "m1" };
try {
  await hooks["tool:execute.before"]({ ...base, id: "c1", input: { path: "/repo/main.go" } });
} catch (err) {
  result.threw = err.message;
}

const after = { ...base, id: "c1", input: {}, status: "completed", result: { content: "FILE CONTENTS" } };
await hooks["tool:execute.after"](after);
result.toolOutput = after.result.content;

// A result with structured output and no content: the host would render
// the output, so a tip must arrive after that rendering, not instead of it.
try {
  await hooks["tool:execute.before"]({ ...base, id: "c3", input: { path: "/repo/other.go" } });
} catch {}
const structured = { ...base, id: "c3", input: {}, status: "completed", result: { output: { lines: 3 } } };
await hooks["tool:execute.after"](structured);
result.structured = structured.result.content ?? null;

const prompt = { sessionID: "s1", messageID: "m2", prompt: { text: "why is login failing" }, delivery: "steer" };
await hooks["session:prompt"](prompt);
result.promptText = prompt.prompt.text;

const evaluation = {
  sessionID: "s1",
  action: "shell",
  resources: ["rm -rf /"],
  metadata: {},
  source: { type: "tool", messageID: "m1", id: "c2" },
  effect: "ask",
};
await hooks["permission:evaluate"](evaluation);
result.permission = evaluation.effect;
result.permissionMessage = evaluation.message ?? null;

console.log(JSON.stringify(result));
`

type driverResult struct {
	Threw             *string `json:"threw"`
	ToolOutput        string  `json:"toolOutput"`
	PromptText        string  `json:"promptText"`
	Permission        string  `json:"permission"`
	PermissionMessage *string `json:"permissionMessage"`
	Structured        any     `json:"structured"`
}

// generations pairs each OpenCode plugin generation with its driver.
var generations = []struct {
	name   string
	driver string
}{
	{name: "v1", driver: driverV1},
	{name: "v2", driver: driverV2},
}

// runDriver renders the plugin against the stub hook in a fresh dir, runs
// driver under node with the stub in the given mode, and returns the
// decoded result plus the capture file the stub appended envelopes to.
func runDriver(t *testing.T, node, driver, mode string) (driverResult, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.jsonl")

	stub := filepath.Join(dir, "gortex")
	if err := os.WriteFile(stub, []byte(stubHook), 0o755); err != nil {
		t.Fatal(err)
	}
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.HookCommand = stub + " hook"
	if err := os.WriteFile(filepath.Join(dir, "gortex.mjs"), []byte(renderPlugin(env)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GORTEX_STUB_MODE="+mode,
		"GORTEX_STUB_CAPTURE="+capture,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("driver failed: %v\n%s", err, out)
	}
	var got driverResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("driver output %q: %v", out, err)
	}
	return got, capture
}

func requireNode(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook stub is a POSIX shell script")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	return node
}

func TestPluginBehaviourUnderNode(t *testing.T) {
	node := requireNode(t)

	cases := []struct {
		mode  string
		check func(t *testing.T, got driverResult)
	}{
		{
			mode: "block",
			check: func(t *testing.T, got driverResult) {
				if got.Threw == nil || !strings.Contains(*got.Threw, "call search_symbols instead") {
					t.Fatalf("a block decision must throw the Go reason verbatim, got %v", got.Threw)
				}
				if got.Permission != "deny" {
					t.Fatalf("the permission hook should have denied, got %q", got.Permission)
				}
			},
		},
		{
			mode: "tip",
			check: func(t *testing.T, got driverResult) {
				if got.Threw != nil {
					t.Fatalf("soft guidance must not block: %v", *got.Threw)
				}
				if !strings.Contains(got.ToolOutput, "FILE CONTENTS") || !strings.Contains(got.ToolOutput, "GRAPH TIP") {
					t.Fatalf("the after-hook must append the parked tip to the result, got %q", got.ToolOutput)
				}
				if got.Permission != "ask" {
					t.Fatalf("a non-blocking decision must leave the permission status alone, got %q", got.Permission)
				}
			},
		},
		{
			mode: "orient",
			check: func(t *testing.T, got driverResult) {
				if !strings.HasPrefix(got.PromptText, "why is login failing") {
					t.Fatalf("injection must extend the user's text, not replace it: %q", got.PromptText)
				}
				if !strings.Contains(got.PromptText, "BRIEFING") || !strings.Contains(got.PromptText, "PROMPT CONTEXT") {
					t.Fatalf("the prompt hook must carry both the briefing and the turn context: %q", got.PromptText)
				}
			},
		},
		// The three fail-open modes: a broken bridge is indistinguishable
		// from no bridge at all.
		{mode: "crash", check: assertNoInterference},
		{mode: "garbage", check: assertNoInterference},
		{mode: "empty", check: assertNoInterference},
	}

	for _, gen := range generations {
		for _, tc := range cases {
			t.Run(gen.name+"/"+tc.mode, func(t *testing.T) {
				got, _ := runDriver(t, node, gen.driver, tc.mode)
				tc.check(t, got)
			})
		}
	}
}

// TestPluginV2SpecificSemantics covers what only the OpenCode 2 entrypoint
// does: deny text travels in the permission event's message, and a tip on
// a content-less result keeps the host's rendering of the output.
func TestPluginV2SpecificSemantics(t *testing.T) {
	node := requireNode(t)

	t.Run("block carries the reason on the permission event", func(t *testing.T) {
		got, _ := runDriver(t, node, driverV2, "block")
		if got.PermissionMessage == nil || !strings.Contains(*got.PermissionMessage, "call search_symbols instead") {
			t.Fatalf("a V2 permission deny must carry the Go reason as its message, got %v", got.PermissionMessage)
		}
	})

	t.Run("tip on structured output keeps the output", func(t *testing.T) {
		got, _ := runDriver(t, node, driverV2, "tip")
		parts, ok := got.Structured.([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("want the rendered output followed by the tip, got %#v", got.Structured)
		}
		first, _ := parts[0].(map[string]any)
		second, _ := parts[1].(map[string]any)
		if first["text"] != `{"lines":3}` || second["text"] != "GRAPH TIP" {
			t.Fatalf("want [output, tip], got %#v", parts)
		}
	})

	t.Run("broken bridge leaves structured output alone", func(t *testing.T) {
		got, _ := runDriver(t, node, driverV2, "crash")
		if got.Structured != nil {
			t.Fatalf("a broken bridge must not synthesise content, got %#v", got.Structured)
		}
	})
}

// TestPluginDefaultExportLoadsInBothGenerations checks the module shape
// each host validates before it runs anything: OpenCode 2 decodes the
// default export as `{ id: string, setup: function }` (anything else is
// "Plugin must export a default definition with an id and an effect or
// setup function", the failure that silently disabled every hook), and
// OpenCode 1.18 takes a default export with `server` and requires an id
// on a file plugin. A lone default export keeps 1.18 off its legacy path,
// which would reject any non-function export.
func TestPluginDefaultExportLoadsInBothGenerations(t *testing.T) {
	node := requireNode(t)
	dir := t.TempDir()
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.HookCommand = filepath.Join(dir, "gortex") + " hook"
	if err := os.WriteFile(filepath.Join(dir, "gortex.mjs"), []byte(renderPlugin(env)), 0o644); err != nil {
		t.Fatal(err)
	}
	const probe = `import * as mod from "./gortex.mjs";
const d = mod.default;
console.log(JSON.stringify({
  exports: Object.keys(mod),
  id: d && d.id,
  setup: typeof (d && d.setup),
  server: typeof (d && d.server),
}));
`
	if err := os.WriteFile(filepath.Join(dir, "probe.mjs"), []byte(probe), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "probe.mjs")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	var got struct {
		Exports []string `json:"exports"`
		ID      string   `json:"id"`
		Setup   string   `json:"setup"`
		Server  string   `json:"server"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	if len(got.Exports) != 1 || got.Exports[0] != "default" {
		t.Fatalf("the plugin must export exactly a default definition, got %v", got.Exports)
	}
	if got.ID == "" || got.Setup != "function" || got.Server != "function" {
		t.Fatalf("default export must carry id, setup() and server(), got %+v", got)
	}
}

func assertNoInterference(t *testing.T, got driverResult) {
	t.Helper()
	if got.Threw != nil {
		t.Fatalf("a broken bridge must not block a tool call, but it threw %q", *got.Threw)
	}
	if got.ToolOutput != "FILE CONTENTS" {
		t.Fatalf("a broken bridge must not touch the tool result, got %q", got.ToolOutput)
	}
	if got.PromptText != "why is login failing" {
		t.Fatalf("a broken bridge must not touch the user's message, got %q", got.PromptText)
	}
	if got.Permission != "ask" {
		t.Fatalf("a broken bridge must not change a permission decision, got %q", got.Permission)
	}
}

// TestPluginEnvelopeMatchesTheBridgeContract reads back what the plugin
// actually put on the hook's stdin and checks it against the BridgeEvent
// fields in internal/hooks/pi.go — including the `filePath` / `path` ->
// `file_path` mapping, without which the Go classifier sees no path on any
// read and enforces nothing while looking perfectly healthy. Both plugin
// generations must emit the same envelopes.
func TestPluginEnvelopeMatchesTheBridgeContract(t *testing.T) {
	node := requireNode(t)

	for _, gen := range generations {
		t.Run(gen.name, func(t *testing.T) {
			_, capture := runDriver(t, node, gen.driver, "empty")

			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("no envelope was ever written: %v", err)
			}
			envelopes := map[string]map[string]any{}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var ev map[string]any
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					t.Fatalf("envelope %q is not JSON: %v", line, err)
				}
				event, _ := ev["event"].(string)
				if _, seen := envelopes[event]; !seen {
					envelopes[event] = ev
				}
			}

			for _, event := range []string{"tool.execute.before", "session", "chat.message", "permission.ask"} {
				if _, ok := envelopes[event]; !ok {
					t.Fatalf("no %q envelope was sent; got %v", event, keysOf(envelopes))
				}
			}

			before := envelopes["tool.execute.before"]
			if before["tool_name"] != "Read" {
				t.Fatalf("tool name must arrive in the Claude vocabulary, got %v", before["tool_name"])
			}
			input, _ := before["tool_input"].(map[string]any)
			if input["file_path"] != "/repo/main.go" {
				t.Fatalf("OpenCode's path key was not mapped onto file_path: %v", input)
			}
			if before["session_id"] != "s1" {
				t.Fatalf("session_id missing: %v", before)
			}
			if before["cwd"] == "" || before["cwd"] == nil {
				t.Fatalf("cwd missing: %v", before)
			}

			perm := envelopes["permission.ask"]
			if perm["tool_name"] != "Bash" {
				t.Fatalf("permission type must map onto the Claude vocabulary, got %v", perm["tool_name"])
			}
			permInput, _ := perm["tool_input"].(map[string]any)
			if permInput["command"] != "rm -rf /" {
				t.Fatalf("permission subject did not become tool_input: %v", permInput)
			}

			if prompt, _ := envelopes["chat.message"]["prompt"].(string); prompt != "why is login failing" {
				t.Fatalf("the prompt envelope must carry the turn text, got %q", prompt)
			}
		})
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
