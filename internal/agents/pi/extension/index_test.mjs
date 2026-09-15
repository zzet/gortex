// The readiness barrier: a turn that arrives before the gortex tools are
// registered is held until they are, and the orientation is injected once.
//
// Run with `node --test` from this directory, or through the Go wrapper
// (TestPiExtensionHarness in internal/agents/pi/extension_test.go).

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, delay, freshSession, firstTurn } from "./testsupport/fixtures.mjs";

const subject = await buildSubject();

describe("a turn arriving mid-handshake", () => {
  let pi, held, turn1, turn2, turn3;

  before(async () => {
    resetInitializeAttempts();
    control.reset({
      handshakeDelayMs: 300,
      toolsListDelayMs: 300,
      tools: TOOLS,
      hookDecision: { orientation: ORIENTATION },
    });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);

    // Nothing awaits this — exactly how Pi reaches before_agent_start at startup.
    const started = freshSession(pi);

    const t0 = Date.now();
    await firstTurn(pi);
    held = Date.now() - t0;
    await started;

    turn1 = await pi.pushContext();
    turn2 = await pi.pushContext();
    await firstTurn(pi);
    turn3 = await pi.pushContext();
  });

  it("is held for the whole handshake", () => {
    assert.ok(held >= 500, `released after ${held}ms of a 300+300ms handshake`);
  });

  it("finds the tools live at the first LLM call", () => {
    assert.equal(pi.registered.size, 3);
  });

  it("carries the orientation on the first context push", () => {
    assert.equal(turn1.length, 1);
    assert.ok(turn1[0].content.includes(ORIENTATION));
  });

  it("injects nothing on a later push", () => {
    assert.equal(turn2.length, 0);
  });

  it("injects nothing on a second before_agent_start", () => {
    assert.equal(turn3.length, 0);
  });
});

describe("a prompt submitted in the /reload gap", () => {
  let pi1, pi2, injected1, injected2, parkedBeforeSessionStart, released;

  before(async () => {
    resetInitializeAttempts();
    control.reset({
      handshakeDelayMs: 150,
      toolsListDelayMs: 0,
      tools: TOOLS,
      hookDecision: { orientation: ORIENTATION },
    });

    const factory1 = await loadFactory(subject, 1);
    pi1 = createPi();
    factory1(pi1);
    await freshSession(pi1);
    await firstTurn(pi1);
    injected1 = await pi1.pushContext();

    // /reload: the module is re-imported and the new instance exists before
    // its own session_start fires.
    const factory2 = await loadFactory(subject, 2);
    pi2 = createPi();
    factory2(pi2);

    released = false;
    const turn = firstTurn(pi2).then(() => {
      released = true;
    });

    // Nothing has emitted session_start on pi2 yet, so the turn parks
    // regardless of the handshake latency.
    await delay(100);
    parkedBeforeSessionStart = !released;

    const started2 = freshSession(pi2, "reload");
    await turn;
    await started2;

    injected2 = await pi2.pushContext();
  });

  it("injected the orientation for session 1", () => {
    assert.equal(injected1.length, 1);
  });

  it("parks the turn in the gap before session_start", () => {
    assert.ok(parkedBeforeSessionStart, "the turn proceeded before session_start fired");
  });

  it("resumes the turn once session_start lands", () => {
    assert.ok(released);
  });

  it("gives session 2 its own tools", () => {
    assert.equal(pi2.registered.size, 3);
  });

  it("re-injects the orientation for session 2", () => {
    assert.equal(injected2.length, 1);
    assert.ok(injected2[0].content.includes(ORIENTATION));
  });
});
