// Shared fixtures for the extension suites. The per-suite choreography stays
// in the test files: for the barrier, when each event fires relative to the
// others IS the thing under test.

import { control } from "./child-process-mock.mjs";

export const ORIENTATION = "GORTEX-ORIENTATION: prefer graph tools over native reads.";

// `read` and `edit` collide with Pi's builtins; `search` does not.
export const TOOLS = [
  { name: "read", description: "Read indexed source.", inputSchema: { type: "object", properties: {} } },
  { name: "edit", description: "Apply guarded edits.", inputSchema: { type: "object", properties: {} } },
  { name: "search", description: "Search the graph.", inputSchema: { type: "object", properties: {} } },
];

export const delay = (ms) => new Promise((r) => setTimeout(r, ms));

export function freshSession(pi, reason = "startup") {
  return pi.emit("session_start", { type: "session_start", reason });
}

export function firstTurn(pi) {
  return pi.emit("before_agent_start", { type: "before_agent_start", prompt: "go" });
}

/**
 * Emits session_start without awaiting it, and returns the child that
 * invocation spawned with its replies withheld. The suite decides when
 * registration completes by calling `child.releaseReplies()`, so nothing has
 * to assert on how long the handshake took.
 *
 * DRIFT FENCE: this targets the new child by capturing it on the line after
 * the emit, which holds only while index.ts spawns before its first await
 * inside the session_start handler. Insert an await ahead of the spawn and
 * this throws instead of silently gating nothing.
 */
export function startGatedSession(pi, reason = "startup") {
  const before = control.children.length;
  control.holdNextChild = true;
  try {
    const settled = freshSession(pi, reason);
    if (control.children.length === before) {
      throw new Error(
        "session_start spawned no child synchronously — index.ts now awaits before spawning, so this gate targets nothing; rework startGatedSession",
      );
    }
    return { settled, child: control.children[control.children.length - 1] };
  } finally {
    control.holdNextChild = false;
  }
}
