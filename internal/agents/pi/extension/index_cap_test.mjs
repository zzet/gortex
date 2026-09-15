// READY_WAIT_MS expiry: the barrier gives up once the cap passes, and the
// turn it lets through says so.
//
// Separate file because it needs its own subject — READY_WAIT_MS is rewritten
// at build time, so this suite cannot share the module the others use.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, firstTurn, startGatedSession } from "./testsupport/fixtures.mjs";

const CAP_MS = 20;

describe("the cap expires before registration finishes", () => {
  let pi, toolsAtRelease, turn1, laterTurn;

  before(async () => {
    const subject = await buildSubject({ readyWaitMs: CAP_MS });

    resetInitializeAttempts();
    control.reset({ tools: TOOLS, hookDecision: { orientation: ORIENTATION } });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);

    // The child answers nothing until released, so registration cannot finish
    // while the turn is in flight however slow or fast the machine is.
    const { settled, child } = startGatedSession(pi);

    await firstTurn(pi);
    toolsAtRelease = pi.registered.size;

    turn1 = await pi.pushContext();

    child.releaseReplies();
    await settled;
    laterTurn = await pi.pushContext();
  });

  it("releases the turn before the tools are live", () => {
    // The gate holds registration open, so this is the bounded-wait claim:
    // the cap, and nothing else, let the turn through.
    assert.equal(toolsAtRelease, 0);
  });

  it("still carries the orientation", () => {
    assert.equal(turn1.length, 1);
    assert.ok(turn1[0].content.includes(ORIENTATION));
  });

  it("warns that the tools are not callable yet", () => {
    assert.match(turn1[0].content, /still registering/i);
  });

  it("names /reload as the recovery", () => {
    assert.ok(turn1[0].content.includes("/reload"));
  });

  it("lands the registration after the cap anyway", () => {
    assert.equal(pi.registered.size, 3);
  });

  it("still explains the rename on a late-registered tool", () => {
    assert.ok((pi.registered.get("gortex_read")?.description ?? "").includes("`gortex_read`"));
  });

  it("injects nothing on later turns", () => {
    assert.equal(laterTurn.length, 0);
  });
});
