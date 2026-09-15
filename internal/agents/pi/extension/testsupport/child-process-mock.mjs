// Stand-in for node:child_process, substituted into the subject module so the
// extension's spawn()/execFileSync() calls land here. No real `gortex` binary
// is involved. The control object below is the only steering surface the
// scenarios need.

import { EventEmitter } from "node:events";

export const control = {
  // Delay before the child answers `initialize`.
  handshakeDelayMs: 0,
  // Delay before the child answers `tools/list`.
  toolsListDelayMs: 0,
  // Payload for tools/list.
  tools: [],
  // What the Go hook "writes back" for every callHook() envelope.
  hookDecision: {},
  // Make the first initialize fail, to exercise start()'s single retry.
  failFirstInitialize: false,
  // Withhold the next spawned child's replies until releaseReplies(). A gate
  // lets a suite hold registration open for as long as it needs without
  // asserting on wall-clock latency.
  holdNextChild: false,

  // Observations.
  spawns: [],
  hookCalls: [],
  children: [],

  reset(overrides = {}) {
    this.handshakeDelayMs = 0;
    this.toolsListDelayMs = 0;
    this.tools = [];
    this.hookDecision = {};
    this.failFirstInitialize = false;
    this.holdNextChild = false;
    this.spawns = [];
    this.hookCalls = [];
    this.children = [];
    Object.assign(this, overrides);
  },
};

let initializeAttempts = 0;

class FakeChild extends EventEmitter {
  constructor() {
    super();
    this.stdout = new EventEmitter();
    this.stderr = new EventEmitter();
    this.stdin = new EventEmitter();
    this.stdin.write = (line) => this.#onWrite(line);
    this.killed = false;
    this.exited = false;
    this.held = false;
    this.pending = [];
  }

  #reply(obj, delayMs) {
    const send = () => {
      if (this.killed || this.exited) return;
      this.stdout.emit("data", Buffer.from(JSON.stringify(obj) + "\n", "utf8"));
    };
    if (this.held) {
      this.pending.push({ send, delayMs });
      return;
    }
    if (delayMs > 0) setTimeout(send, delayMs).unref?.();
    else queueMicrotask(send);
  }

  // Flush everything withheld and answer normally from here on.
  releaseReplies() {
    this.held = false;
    const queued = this.pending;
    this.pending = [];
    for (const { send, delayMs } of queued) {
      if (delayMs > 0) setTimeout(send, delayMs).unref?.();
      else queueMicrotask(send);
    }
  }

  #onWrite(line) {
    let msg;
    try {
      msg = JSON.parse(String(line).trim());
    } catch {
      return true;
    }
    // Notifications carry no id and get no reply.
    if (typeof msg.id !== "number") return true;

    if (msg.method === "initialize") {
      initializeAttempts += 1;
      if (control.failFirstInitialize && initializeAttempts === 1) {
        this.#reply(
          { jsonrpc: "2.0", id: msg.id, error: { code: -32603, message: "daemon warming up" } },
          control.handshakeDelayMs,
        );
        return true;
      }
      this.#reply(
        { jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-06-18", capabilities: {} } },
        control.handshakeDelayMs,
      );
      return true;
    }

    if (msg.method === "tools/list") {
      this.#reply(
        { jsonrpc: "2.0", id: msg.id, result: { tools: control.tools } },
        control.toolsListDelayMs,
      );
      return true;
    }

    if (msg.method === "tools/call") {
      this.#reply(
        { jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: "ok" }] } },
        0,
      );
      return true;
    }

    this.#reply({ jsonrpc: "2.0", id: msg.id, result: {} }, 0);
    return true;
  }

  // Push an unsolicited server notification (used for list_changed re-sync).
  notify(method, params = {}) {
    this.stdout.emit("data", Buffer.from(JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n", "utf8"));
  }

  kill() {
    if (this.killed) return;
    this.killed = true;
    this.exited = true;
    this.emit("exit", 0, null);
  }

  unref() {}
}

export function spawn(bin, args = [], opts = {}) {
  control.spawns.push({ bin, args: [...args], opts });

  // `gortex daemon start --detach` is fire-and-forget; the extension only
  // attaches an error handler and unrefs it.
  if (args[0] === "daemon") {
    const stub = new EventEmitter();
    stub.unref = () => {};
    stub.kill = () => {};
    return stub;
  }

  const child = new FakeChild();
  child.held = control.holdNextChild;
  control.children.push(child);
  return child;
}

export function execFileSync(bin, args = [], opts = {}) {
  control.hookCalls.push({ bin, args: [...args], input: opts?.input });
  return JSON.stringify(control.hookDecision);
}

export function resetInitializeAttempts() {
  initializeAttempts = 0;
}

export default { spawn, execFileSync };
