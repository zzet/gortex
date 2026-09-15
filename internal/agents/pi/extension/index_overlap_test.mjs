// Overlapping session_start invocations on ONE extension instance: the turn
// parked on the newest invocation must not be released by an older one
// finishing. This is the review finding on PR #787 — a single mutable
// `settleSessionReady` slot, reassigned by each arm, settles whichever promise
// is current when the OLD handler's finally runs.
//
// Not reachable through Pi 0.85.1's own modes (every path that emits
// session_start constructs a fresh instance first), so it drives the closure
// directly: bindExtensions() emits with no idempotence guard, and
// core/sdk.js:69 lets an embedder share one resourceLoader across sessions.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, delay, freshSession, firstTurn, startGatedSession } from "./testsupport/fixtures.mjs";

describe("a stale session_start settling mid-turn", () => {
  let pi, releasedWhenAFinished, released, aFinishedAt;

  before(async () => {
    const subject = await buildSubject();

    resetInitializeAttempts();
    control.reset({ tools: TOOLS, hookDecision: { orientation: ORIENTATION } });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);

    // A: superseded by B, so its start() rejects, backs off and retries. It
    // settles on its own schedule; nothing here depends on when.
    const a = freshSession(pi, "startup");

    // B: fast, and overlaps A.
    await freshSession(pi, "new");

    // C: gated, so it is guaranteed to still be running when A settles.
    // Ordering is structural — A is awaited before C is ever released.
    const { settled: c, child: cChild } = startGatedSession(pi, "reload");

    const t0 = Date.now();
    released = 0;
    const turn = firstTurn(pi).then(() => {
      released = Date.now() - t0;
    });

    await a;
    aFinishedAt = Date.now() - t0;

    // A settling releases the turn through a then(), so give that a full tick
    // to land before reading. C stays gated throughout, so correct code has
    // nothing that could release the turn no matter how long this waits.
    await delay(20);
    releasedWhenAFinished = released;

    cChild.releaseReplies();
    await turn;
    await c;
  });

  it("does not release the parked turn when the stale invocation settles", () => {
    assert.equal(
      releasedWhenAFinished,
      0,
      `released at ${releasedWhenAFinished}ms, while A settled at ${aFinishedAt}ms and C was still gated`,
    );
  });

  it("releases the turn once the invocation it parked on finishes", () => {
    assert.ok(released > 0, "the turn never resumed");
  });

  it("leaves the newest session's tools live", () => {
    assert.equal(pi.registered.size, 3);
  });
});
