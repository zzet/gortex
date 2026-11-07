// Gortex plugin for OpenCode (1.18.18).
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
// down with it.

import { execFileSync } from "node:child_process";

const GORTEX_BIN = {{GORTEX_BIN}};
const HOOK_ARGV = {{GORTEX_HOOK_ARGV}};

// The adapter does not write this file at all under --no-hooks, so a
// rendered `false` only ever reaches disk via a hand-copied or downgraded
// install. Honouring it anyway costs one branch and means the documented
// off switch still works on a file that outlived its installer.
const ENFORCE = {{GORTEX_ENFORCE}};

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

// OpenCode's built-in tool vocabulary, mapped onto the Claude names the
// shared Go enrichment switches on. Deliberately the same seven entries
// as openCodeToolNames in internal/hooks/opencode.go, in the same order,
// so the two tables can be diffed by eye. Anything unrecognised is passed
// through untouched; the Go classifier ignores names it does not know,
// which is the fail-open default this whole file is built around.
const TOOL_NAMES = {
  read: "Read",
  write: "Write",
  edit: "Edit",
  bash: "Bash",
  grep: "Grep",
  glob: "Glob",
  task: "Task",
};

// A tool call that IS a Gortex graph query. OpenCode exposes MCP tools as
// `<server>_<tool>`, and our server is registered as `gortex`, so the
// prefix identifies them; the `mcp__gortex__` spelling is accepted too in
// case a future OpenCode adopts Claude's naming. The flag matters to the
// Go side: consult-unlock uses a graph query as the handshake that
// unlocks fallback reads, and adaptive-nudge resets its streak on one.
const GORTEX_TOOL_PATTERN = /^(gortex[_.]|mcp__gortex__)/;

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
// OpenCode spells the first one `filePath`; without this the classifier
// would see no path on any read or edit and quietly classify nothing,
// which looks exactly like a healthy install that enforces nothing.
// Original keys are kept alongside the canonical ones — the envelope is
// ours, and dropping them would lose context for future rules.
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

export const GortexPlugin = async ({ directory, worktree }) => {
  // The repo the Go side resolves indexed-source coverage against.
  // `worktree` is the git root and `directory` the CWD OpenCode started
  // in; prefer the former so a session opened in a subdirectory is still
  // recognised as covered.
  const cwd = worktree || directory || process.cwd();

  // callID -> soft guidance parked by tool.execute.before for
  // tool.execute.after to deliver. Presence of a key also records "this
  // call was already decided", which is what keeps permission.ask from
  // asking the same question twice for one tool call.
  const decided = new Map();

  // The session briefing is a once-per-session injection; OpenCode has no
  // session-start hook in the stable set, so the first chat message is
  // where it goes.
  let oriented = false;

  function remember(callID, tip) {
    if (!callID) return;
    if (decided.size >= DECISION_CACHE_MAX) decided.clear();
    decided.set(callID, tip || "");
  }

  return {
    // Throwing from tool.execute.before is OpenCode's documented way to
    // refuse a tool call: the throw becomes the tool's error and the
    // model reads the message. That makes the thrown reason the deny
    // text, which is why it is the Go decision's Reason verbatim.
    "tool.execute.before": async (input, output) => {
      if (!ENFORCE) return;
      const tool = String(input?.tool ?? "");
      const gortexTool = isGortexTool(tool);
      const callID = String(input?.callID ?? "");

      const decision = callHook({
        event: "tool.execute.before",
        tool_name: gortexTool ? tool : normalizeToolName(tool),
        tool_input: normalizeToolInput(output?.args),
        cwd,
        session_id: String(input?.sessionID ?? ""),
        is_gortex_tool: gortexTool,
      });

      remember(callID, decision.additional_context);
      if (decision.block) {
        throw new Error(
          decision.reason || "[Gortex] blocked — use the Gortex graph tools instead of raw file reads.",
        );
      }
    },

    // This hook deliberately does NOT shell the bridge. The Go router
    // maps no "after" phase, so every call would spend a subprocess to
    // receive a guaranteed-empty decision. What it is for is delivery:
    // it is the only stable hook that can put text in front of the model
    // without blocking anything, so it hands over the soft guidance the
    // before-hook parked (the enrich and nudge postures' whole output).
    "tool.execute.after": async (input, output) => {
      const callID = String(input?.callID ?? "");
      if (!callID || !decided.has(callID)) return;
      const tip = decided.get(callID);
      decided.delete(callID);
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
    // tool.execute.before has already run for that callID. Deferring to
    // the earlier decision keeps one tool call from being scored twice —
    // double-counting would inflate doctor's run tallies and, under
    // consult-unlock / adaptive-nudge, move per-session state twice for
    // one action. A permission with no matching before-hook is still
    // asked about.
    "permission.ask": async (input, output) => {
      if (!ENFORCE) return;
      const callID = String(input?.callID ?? "");
      if (callID && decided.has(callID)) return;
      const type = String(input?.type ?? "");
      if (isGortexTool(type)) return;

      const decision = callHook({
        event: "permission.ask",
        tool_name: normalizeToolName(type),
        tool_input: normalizeToolInput(input?.metadata),
        cwd,
        session_id: String(input?.sessionID ?? ""),
      });

      remember(callID, "");
      if (decision.block) {
        output.status = "deny";
      }
    },

    // chat.message carries the user's turn with a mutable parts array —
    // the one place a bridged host can inject context the way Claude's
    // UserPromptSubmit hook does. Two envelopes go out on the first turn:
    // the session briefing (once) and the per-turn symbol context.
    "chat.message": async (input, output) => {
      const role = output?.message?.role;
      if (role && role !== "user") return;
      const sessionID = String(output?.message?.sessionID ?? input?.sessionID ?? "");
      const injected = [];

      if (!oriented) {
        oriented = true;
        const briefing = callHook({ event: "session", cwd, session_id: sessionID });
        if (briefing.orientation) injected.push(briefing.orientation);
      }

      const decision = callHook({
        event: "chat.message",
        prompt: messageText(output?.parts),
        cwd,
        session_id: sessionID,
      });
      if (decision.additional_context) injected.push(decision.additional_context);

      if (injected.length > 0) {
        try {
          appendToLastTextPart(output?.parts, injected.join("\n\n"));
        } catch {
          // Best effort — never break message assembly.
        }
      }
    },
  };
};
