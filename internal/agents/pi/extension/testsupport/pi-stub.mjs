// Minimal stand-in for the `pi` object an extension factory receives, plus an
// emit() that mirrors ExtensionRunner.emit / emitBeforeAgentStart in Pi 0.85.1:
// handlers run sequentially and each one is awaited.
//
// Fidelity note: Pi's real registerTool (core/extensions/loader.js:238) is a
// Map.set and never throws on a duplicate name — it only throws via
// assertActive() once the instance has been invalidated. This stub matches
// that, and exposes invalidate() so a scenario can exercise the throwing path.

export function createPi(cwd = "/tmp/harness-cwd") {
  const handlers = new Map();
  const tools = new Map();
  const messages = [];
  let active = true;

  return {
    cwd,

    on(event, handler) {
      if (!handlers.has(event)) handlers.set(event, []);
      handlers.get(event).push(handler);
    },

    registerTool(def) {
      if (!active) throw new Error("extension has been invalidated");
      tools.set(def.name, def);
    },

    sendMessage(msg) {
      messages.push(msg);
    },

    // --- harness surface (not part of Pi's API) ---

    invalidate() {
      active = false;
    },

    get registered() {
      return tools;
    },

    get sentMessages() {
      return messages;
    },

    hasHandlers(event) {
      return (handlers.get(event) ?? []).length > 0;
    },

    // Sequential, awaited — the property the readiness barrier depends on.
    async emit(event, payload = {}, ctx = {}) {
      const results = [];
      for (const handler of handlers.get(event) ?? []) {
        results.push(await handler(payload, ctx));
      }
      return results;
    },

    // Convenience: run the `context` hook the way Pi does and return the
    // messages the extension appended.
    async pushContext() {
      const messagesIn = [];
      await this.emit("context", { messages: messagesIn });
      return messagesIn;
    },
  };
}
