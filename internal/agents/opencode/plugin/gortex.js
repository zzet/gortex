// Gortex plugin for OpenCode (1.18.18+ and 2.x).
//
// OpenCode has no lifecycle-hook configuration at all — there is no
// settings key that runs a command on session start or before a tool
// call. A JS/TS plugin is its only enforcement surface, so this file is
// the OpenCode half of the Gortex bridge: on each relevant plugin hook it
// shells `gortex hook --agent=opencode`, writes a BridgeEvent envelope to
// stdin, and applies the BridgeDecision it reads back.
//
// Every policy decision — the deny / enrich / consult-unlock / nudge
// postures, indexed-source classification, telemetry — stays in Go
// (internal/hooks/opencode.go + pi.go). Re-implementing any of it here
// would give OpenCode a second, drifting copy of rules every other host
// reads from one place.
//
// One file serves both plugin generations through a single default
// export carrying `id`, `setup` and `server`:
//
//   - OpenCode 2 validates the default export as `{ id, setup }` and runs
//     `setup(ctx)`, where hooks register on their owning domain
//     (ctx.tool / ctx.permission / ctx.session). It never calls server().
//   - OpenCode 1.18.x sees a default export with `server` and calls
//     `server(input)` for the classic hook-object form. It ignores setup.
//
// A file without a default export loads under V1 only — OpenCode 2
// rejects it with "Plugin must export a default definition" — and a
// named export next to the default is unnecessary on both, so there is
// exactly one export.
//
// This file is written verbatim by the `opencode` adapter except for
// three sentinels, each replaced with a JSON literal at install time:
//
//   GORTEX_BIN        -> string:  resolved `gortex` binary path
//   GORTEX_HOOK_ARGV  -> array:   full hook argv, argv[0] included
//   GORTEX_ENFORCE    -> boolean: false when installed with --no-hooks
//
// Zero package dependencies, deliberately: OpenCode installs plugin
// dependencies from `.opencode/package.json` with a bun install at
// startup. Anything beyond a `node:` built-in would need a package.json
// this installer does not own, and a failed install would take the plugin
// down with it. That is also why the V2 definition is a plain object
// rather than a `Plugin.define(...)` call: `Plugin.define` from
// `@opencode/plugin` is an identity function, and the host validates the
// shape, not the helper.

import { execFileSync } from "node:child_process";

const GORTEX_BIN = {{GORTEX_BIN}};
const HOOK_ARGV = {{GORTEX_HOOK_ARGV}};

// The adapter does not write this file at all under --no-hooks, so a
// rendered `false` only ever reaches disk via a hand-copied or downgraded
// install. Honouring it anyway costs one branch and means the documented
// off switch still works on a file that outlived its installer.
const ENFORCE = {{GORTEX_ENFORCE}};

// The plugin id OpenCode keys this plugin by. OpenCode 2 requires one on
// every plugin, and OpenCode 1.18 requires one on a file plugin that
// default-exports an object.
const PLUGIN_ID = "gortex";

// How long a single bridge call may take before we give up and let the
// tool through. The Go side answers from a local daemon in single-digit
// milliseconds; this ceiling exists for the pathological case (daemon
// mid-restart, machine swapping), where waiting longer would be felt as
// the agent hanging.
const HOOK_TIMEOUT_MS = 5000;

// Cap on the per-call decision map. A session is thousands of tool calls
// long and OpenCode gives no "call finished" signal we can trust for
// cleanup, so the map is cleared wholesale once it grows past this. The
// only cost of a clear is that an in-flight call loses its parked tip.
const DECISION_CACHE_MAX = 256;

// Same wholesale-clear cap for the set of sessions that already received
// the briefing. Clearing it only means a long-lived host re-briefs a
// session it has seen before.
const ORIENTED_CACHE_MAX = 256;

// OpenCode's built-in tool vocabulary, mapped onto the Claude names the
// shared Go enrichment switches on. Deliberately the same entries as
// openCodeToolNames in internal/hooks/opencode.go, in the same order, so
// the two tables can be diffed by eye. The first seven are OpenCode 1's
// names; OpenCode 2 renamed bash, task and the multi-file edit to shell,
// subagent and patch. Anything unrecognised is passed through untouched;
// the Go classifier ignores names it does not know, which is the
// fail-open default this whole file is built around.
const TOOL_NAMES = {
  read: "Read",
  write: "Write",
  edit: "Edit",
  bash: "Bash",
  grep: "Grep",
  glob: "Glob",
  task: "Task",
  shell: "Bash",
  patch: "Edit",
  subagent: "Task",
};

// A tool call that IS a Gortex graph query. OpenCode exposes MCP tools as
// `<server>_<tool>`, and our server is registered as `gortex`, so the
// prefix identifies them; the `mcp__gortex__` spelling is accepted too in
// case a future OpenCode adopts Claude's naming. The flag matters to the
// Go side: consult-unlock uses a graph query as the handshake that
// unlocks fallback reads, and adaptive-nudge resets its streak on one.
const GORTEX_TOOL_PATTERN = /^(gortex[_.]|mcp__gortex__)/;

const DEFAULT_BLOCK_REASON =
  "[Gortex] blocked — use the Gortex graph tools instead of raw file reads.";

function isGortexTool(name) {
  return GORTEX_TOOL_PATTERN.test(String(name || ""));
}

function normalizeToolName(name) {
  const raw = String(name || "").trim();
  return TOOL_NAMES[raw.toLowerCase()] || raw;
}

function firstString(obj, keys) {
  if (!obj || typeof obj !== "object") return undefined;
  for (const key of keys) {
    const value = obj[key];
    if (typeof value === "string" && value !== "") return value;
  }
  return undefined;
}

// normalizeToolInput maps OpenCode's argument keys onto the canonical
// `file_path` / `pattern` / `command` shape the Go enrichment reads.
// OpenCode 1 spells the first one `filePath` and OpenCode 2 `path`;
// without this the classifier would see no path on any read or edit and
// quietly classify nothing, which looks exactly like a healthy install
// that enforces nothing. Original keys are kept alongside the canonical
// ones — the envelope is ours, and dropping them would lose context for
// future rules.
function normalizeToolInput(args) {
  const out = args && typeof args === "object" ? { ...args } : {};
  const path = firstString(out, ["file_path", "filePath", "path", "file", "filepath"]);
  if (path !== undefined) out.file_path = path;
  const pattern = firstString(out, ["pattern", "glob", "query"]);
  if (pattern !== undefined) out.pattern = pattern;
  const command = firstString(out, ["command", "cmd", "script"]);
  if (command !== undefined) out.command = command;
  return out;
}

// permissionInput builds tool_input for an OpenCode 2 permission
// evaluation. Its metadata is tool-specific and often lacks the subject,
// which travels in `resources` instead (the shell command, the grep
// pattern, the edited path), so the first resource fills whichever
// canonical key the tool's metadata left empty.
function permissionInput(toolName, metadata, resources) {
  const out = normalizeToolInput(metadata);
  const first = Array.isArray(resources) ? resources.find((r) => typeof r === "string" && r !== "") : undefined;
  if (first === undefined) return out;
  const key = { Bash: "command", Grep: "pattern", Glob: "pattern" }[toolName] || "file_path";
  if (out[key] === undefined) out[key] = first;
  return out;
}

// callHook sends one envelope to `gortex hook --agent=opencode` and
// parses the decision it writes back.
//
// Fail-open and silent by construction: a missing binary, a non-zero
// exit, a timeout, or unparsable stdout all land in the catch and return
// an empty decision, which every caller below treats as "do nothing". A
// broken bridge must never be able to break the user's session. The
// child's stderr is discarded rather than inherited so a Go-side warning
// cannot scribble over OpenCode's TUI.
function callHook(envelope) {
  try {
    const out = execFileSync(GORTEX_BIN, HOOK_ARGV.slice(1), {
      input: JSON.stringify(envelope),
      encoding: "utf8",
      maxBuffer: 16 * 1024 * 1024,
      timeout: HOOK_TIMEOUT_MS,
      stdio: ["pipe", "pipe", "ignore"],
    });
    const trimmed = String(out || "").trim();
    if (!trimmed) return {};
    const decision = JSON.parse(trimmed);
    return decision && typeof decision === "object" ? decision : {};
  } catch {
    return {};
  }
}

// messageText joins the text parts of a chat message, which is what the
// Go side scores for "indexed symbols relevant to this turn".
function messageText(parts) {
  if (!Array.isArray(parts)) return "";
  return parts
    .filter((p) => p && p.type === "text" && typeof p.text === "string")
    .map((p) => p.text)
    .join("\n");
}

// appendToLastTextPart injects context by extending the last text part in
// place rather than pushing a new one. A part carries identity fields
// (id, sessionID, messageID) whose shape is OpenCode's, not ours;
// fabricating one risks a malformed message where the worst case is a
// broken session, while appending to an existing string cannot be
// structurally wrong.
function appendToLastTextPart(parts, text) {
  if (!Array.isArray(parts) || !text) return;
  for (let i = parts.length - 1; i >= 0; i--) {
    const part = parts[i];
    if (part && part.type === "text" && typeof part.text === "string") {
      part.text = part.text + "\n\n" + text;
      return;
    }
  }
}

// stringifyOutput mirrors how OpenCode 2 renders a tool result that has
// structured `output` but no `content`, so appending a tip to such a
// result does not change what the model sees of the output itself.
function stringifyOutput(value) {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
}

// appendToToolContent returns OpenCode 2 tool-result content with text
// appended. Content is a string or an array of text/file parts; an empty
// or absent one means the host renders `output` instead, so that rendering
// is reproduced ahead of the tip rather than replaced by it.
function appendToToolContent(content, output, text) {
  if (typeof content === "string") return content + "\n\n" + text;
  const tip = { type: "text", text };
  if (Array.isArray(content) && content.length > 0) return [...content, tip];
  return [{ type: "text", text: stringifyOutput(output) }, tip];
}

// createBridge holds the per-plugin-instance state and every decision the
// two host generations share. The entrypoints below only translate each
// host's event shape into these calls, so V1 and V2 cannot drift apart on
// what is sent to Go or what a decision means.
function createBridge(cwd) {
  // callID -> soft guidance parked by the before-hook for the after-hook
  // to deliver. Presence of a key also records "this call was already
  // decided", which is what keeps the permission hook from asking the
  // same question twice for one tool call.
  const decided = new Map();

  // Sessions that already received the once-per-session briefing.
  // OpenCode has no session-start hook in the stable set, so the first
  // user turn of each session is where it goes.
  const oriented = new Set();

  function remember(callID, tip) {
    if (!callID) return;
    if (decided.size >= DECISION_CACHE_MAX) decided.clear();
    decided.set(callID, tip || "");
  }

  return {
    // beforeTool scores one tool call and returns the deny reason, or ""
    // to let it through. Soft guidance is parked for afterTool.
    beforeTool(tool, args, sessionID, callID) {
      const gortexTool = isGortexTool(tool);
      const decision = callHook({
        event: "tool.execute.before",
        tool_name: gortexTool ? tool : normalizeToolName(tool),
        tool_input: normalizeToolInput(args),
        cwd,
        session_id: sessionID,
        is_gortex_tool: gortexTool,
      });
      remember(callID, decision.additional_context);
      return decision.block ? decision.reason || DEFAULT_BLOCK_REASON : "";
    },

    // takeTip hands over (once) the guidance parked for callID.
    takeTip(callID) {
      if (!callID || !decided.has(callID)) return "";
      const tip = decided.get(callID);
      decided.delete(callID);
      return tip || "";
    },

    // permission scores a permission request and returns the deny reason,
    // or "" to leave the host's decision alone. A permission raised from
    // inside a tool whose before-hook already ran is skipped: scoring one
    // tool call twice would inflate doctor's run tallies and, under
    // consult-unlock / adaptive-nudge, move per-session state twice for
    // one action. A permission with no matching before-hook is still
    // scored.
    permission(name, toolInput, sessionID, callID) {
      if (callID && decided.has(callID)) return "";
      if (isGortexTool(name)) return "";
      const decision = callHook({
        event: "permission.ask",
        tool_name: normalizeToolName(name),
        tool_input: toolInput,
        cwd,
        session_id: sessionID,
      });
      remember(callID, "");
      return decision.block ? decision.reason || DEFAULT_BLOCK_REASON : "";
    },

    // promptContext returns the text to inject into a user turn: the
    // session briefing on the session's first turn, plus the per-turn
    // symbol context — the equivalent of Claude's UserPromptSubmit.
    promptContext(sessionID, promptText) {
      const injected = [];
      if (!oriented.has(sessionID)) {
        if (oriented.size >= ORIENTED_CACHE_MAX) oriented.clear();
        oriented.add(sessionID);
        const briefing = callHook({ event: "session", cwd, session_id: sessionID });
        if (briefing.orientation) injected.push(briefing.orientation);
      }
      const decision = callHook({ event: "chat.message", prompt: promptText, cwd, session_id: sessionID });
      if (decision.additional_context) injected.push(decision.additional_context);
      return injected.join("\n\n");
    },
  };
}

// server is the OpenCode 1.18 entrypoint: it returns the classic
// hook-object form.
async function server({ directory, worktree } = {}) {
  // The repo the Go side resolves indexed-source coverage against.
  // `worktree` is the git root and `directory` the CWD OpenCode started
  // in; prefer the former so a session opened in a subdirectory is still
  // recognised as covered.
  const bridge = createBridge(worktree || directory || process.cwd());

  return {
    // Throwing from tool.execute.before is OpenCode's documented way to
    // refuse a tool call: the throw becomes the tool's error and the
    // model reads the message. That makes the thrown reason the deny
    // text, which is why it is the Go decision's Reason verbatim.
    "tool.execute.before": async (input, output) => {
      if (!ENFORCE) return;
      const reason = bridge.beforeTool(
        String(input?.tool ?? ""),
        output?.args,
        String(input?.sessionID ?? ""),
        String(input?.callID ?? ""),
      );
      if (reason) throw new Error(reason);
    },

    // This hook deliberately does NOT shell the bridge. The Go router
    // maps no "after" phase, so every call would spend a subprocess to
    // receive a guaranteed-empty decision. What it is for is delivery:
    // it is the only stable hook that can put text in front of the model
    // without blocking anything, so it hands over the soft guidance the
    // before-hook parked (the enrich and nudge postures' whole output).
    "tool.execute.after": async (input, output) => {
      const tip = bridge.takeTip(String(input?.callID ?? ""));
      if (!tip) return;
      try {
        if (output && typeof output.output === "string") {
          output.output = output.output + "\n\n" + tip;
        }
      } catch {
        // Never let context delivery break a tool result.
      }
    },

    // permission.ask is a second gate, not a duplicate one: OpenCode
    // raises it from inside a tool that needs approval, after
    // tool.execute.before has already run for that callID.
    "permission.ask": async (input, output) => {
      if (!ENFORCE) return;
      const type = String(input?.type ?? "");
      const reason = bridge.permission(
        type,
        normalizeToolInput(input?.metadata),
        String(input?.sessionID ?? ""),
        String(input?.callID ?? ""),
      );
      if (reason) output.status = "deny";
    },

    // chat.message carries the user's turn with a mutable parts array —
    // the one place a bridged host can inject context the way Claude's
    // UserPromptSubmit hook does.
    "chat.message": async (input, output) => {
      const role = output?.message?.role;
      if (role && role !== "user") return;
      const sessionID = String(output?.message?.sessionID ?? input?.sessionID ?? "");
      const text = bridge.promptContext(sessionID, messageText(output?.parts));
      if (!text) return;
      try {
        appendToLastTextPart(output?.parts, text);
      } catch {
        // Best effort — never break message assembly.
      }
    },
  };
}

// register attaches one OpenCode 2 hook, failing open: a host that lacks
// a domain or rejects a hook name loses that one hook, not the plugin.
async function register(domain, name, callback) {
  try {
    if (domain && typeof domain.hook === "function") await domain.hook(name, callback);
  } catch {
    // A missing hook must never take the plugin's load down with it.
  }
}

// setup is the OpenCode 2 entrypoint. Hooks register on the domain that
// owns them instead of being returned as one object.
async function setup(ctx) {
  // `project.directory` is the project root and `directory` where the
  // session runs; prefer the root for the same reason server() prefers
  // the worktree.
  const location = ctx?.location || {};
  const bridge = createBridge(location.project?.directory || location.directory || process.cwd());

  // Throwing from execute.before is OpenCode 2's documented way to refuse
  // a tool call; the thrown reason is what the model reads.
  await register(ctx?.tool, "execute.before", (event) => {
    if (!ENFORCE) return;
    const reason = bridge.beforeTool(
      String(event?.tool ?? ""),
      event?.input,
      String(event?.sessionID ?? ""),
      String(event?.id ?? ""),
    );
    if (reason) throw new Error(reason);
  });

  // Delivery only, as in server(): hands the parked tip to the model by
  // extending the tool result. A failed call has no result to extend, but
  // its tip is still taken so the map does not hold it.
  await register(ctx?.tool, "execute.after", (event) => {
    const tip = bridge.takeTip(String(event?.id ?? ""));
    if (!tip || event?.status !== "completed" || !event.result) return;
    try {
      event.result = {
        ...event.result,
        content: appendToToolContent(event.result.content, event.result.output, tip),
      };
    } catch {
      // Never let context delivery break a tool result.
    }
  });

  // OpenCode 2 evaluates a permission from inside the tool, after
  // execute.before ran for the same call; `source.id` is that call's id,
  // which is what lets the bridge skip a call it already scored. Denying
  // here surfaces `message` to the model as the refusal. A deny the
  // user's own config already made is final and never reaches hooks.
  await register(ctx?.permission, "evaluate", (event) => {
    if (!ENFORCE || !event || event.effect === "deny") return;
    const action = String(event.action ?? "");
    const reason = bridge.permission(
      action,
      permissionInput(normalizeToolName(action), event.metadata, event.resources),
      String(event.sessionID ?? ""),
      String(event.source?.id ?? ""),
    );
    if (!reason) return;
    event.effect = "deny";
    event.message = reason;
  });

  // The prompt hook sees the user's turn before admission, with mutable
  // text — OpenCode 2's replacement for chat.message.
  await register(ctx?.session, "prompt", (event) => {
    const prompt = event?.prompt;
    if (!prompt || typeof prompt.text !== "string") return;
    const text = bridge.promptContext(String(event.sessionID ?? ""), prompt.text);
    if (text) prompt.text = prompt.text + "\n\n" + text;
  });
}

export default { id: PLUGIN_ID, setup, server };
