// Tool registration: names that collide with Pi's builtins are registered
// under a `gortex_` alias, and each aliased tool explains its own rename in
// its description — the model never sees the bare name it was told to call.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";

import { buildSubject, loadFactory } from "./testsupport/subject.mjs";
import { control, resetInitializeAttempts } from "./testsupport/child-process-mock.mjs";
import { createPi } from "./testsupport/pi-stub.mjs";
import { ORIENTATION, TOOLS, freshSession } from "./testsupport/fixtures.mjs";

describe("a tool whose name collides with a Pi builtin", () => {
  let pi;

  before(async () => {
    const subject = await buildSubject();

    resetInitializeAttempts();
    control.reset({ tools: TOOLS, hookDecision: { orientation: ORIENTATION } });

    const factory = await loadFactory(subject, 0);
    pi = createPi();
    factory(pi);
    await freshSession(pi);
  });

  it("is registered under the alias", () => {
    assert.ok(pi.registered.has("gortex_read"));
    assert.ok(pi.registered.has("gortex_edit"));
  });

  it("does not register the bare colliding name", () => {
    assert.ok(!pi.registered.has("read"));
  });

  it("names both the bare and the aliased name in its description", () => {
    const d = pi.registered.get("gortex_read").description;
    assert.ok(d.includes("`read`"), d);
    assert.ok(d.includes("`gortex_read`"), d);
  });

  it("preserves the original description", () => {
    assert.ok(pi.registered.get("gortex_read").description.includes("Read indexed source."));
  });
});

describe("a tool whose name does not collide", () => {
  let pi;

  before(async () => {
    const subject = await buildSubject();

    resetInitializeAttempts();
    control.reset({ tools: TOOLS, hookDecision: { orientation: ORIENTATION } });

    const factory = await loadFactory(subject, 1);
    pi = createPi();
    factory(pi);
    await freshSession(pi);
  });

  it("keeps its bare name", () => {
    assert.ok(pi.registered.has("search"));
  });

  it("keeps its description untouched", () => {
    assert.equal(pi.registered.get("search").description, "Search the graph.");
  });
});
