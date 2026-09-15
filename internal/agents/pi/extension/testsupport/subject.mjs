// Derives the module under test from the LIVE extension source, resolving the
// installer's sentinels exactly as internal/agents/pi/adapter.go does and
// redirecting node:child_process to the mock.
//
// Every transform asserts it matched, and buildSubject asserts no `{{...}}`
// survives: a sentinel removed from index.ts fails on its own transform, and
// one newly added to adapter.go fails on the sweep.

import { readFile, writeFile, mkdir } from "node:fs/promises";
import { pathToFileURL, fileURLToPath } from "node:url";
import path from "node:path";
import os from "node:os";

const HERE = path.dirname(fileURLToPath(import.meta.url));

// <repo>/internal/agents/pi/extension/index.ts, resolved from this file so the
// suite runs from any working directory. GORTEX_PI_INDEX_TS points it at
// another checkout (e.g. to confirm a scenario fails before the fix).
export const DEFAULT_INDEX_TS =
  process.env.GORTEX_PI_INDEX_TS || path.join(HERE, "..", "index.ts");

const MOCK_PATH = path.join(HERE, "child-process-mock.mjs");

// Mirrors adapter.go's substituteSentinel: tolerant of inner whitespace
// (Prettier reflows `{{NAME}}` to `{{ NAME }}`), literal replacement.
const SENTINELS = {
  GORTEX_BIN: "/nonexistent/gortex",
  GORTEX_HOOK_ARGV: ["/nonexistent/gortex", "hook", "--agent=pi"],
  GORTEX_ENFORCE: true,
  GORTEX_TOOLS_PRESET: "core",
};

function substituteSentinel(src, name, value) {
  const re = new RegExp("\\{\\{\\s*" + name + "\\s*\\}\\}", "g");
  let hits = 0;
  const out = src.replace(re, () => {
    hits += 1;
    return JSON.stringify(value);
  });
  if (hits === 0) {
    throw new Error(
      `sentinel {{${name}}} not found in the extension source — the installer contract changed, update SENTINELS in testsupport/subject.mjs`,
    );
  }
  return out;
}

let buildCounter = 0;

/**
 * @param {object} [opts]
 * @param {string} [opts.indexPath]   path to index.ts
 * @param {number} [opts.readyWaitMs] override READY_WAIT_MS (the cap suite)
 * @returns {Promise<string>} path to the generated subject module
 */
export async function buildSubject(opts = {}) {
  const indexPath = opts.indexPath ?? DEFAULT_INDEX_TS;
  let src = await readFile(indexPath, "utf8");

  for (const [name, value] of Object.entries(SENTINELS)) {
    src = substituteSentinel(src, name, value);
  }

  const leftover = src.match(/\{\{\s*[A-Z0-9_]+\s*\}\}/g);
  if (leftover) {
    throw new Error(
      `unsubstituted sentinel(s) ${[...new Set(leftover)].join(", ")} — adapter.go grew a sentinel the harness does not know, add it to SENTINELS in testsupport/subject.mjs`,
    );
  }

  const importRe = /from\s+"node:child_process"/g;
  if (!importRe.test(src)) {
    throw new Error("node:child_process import not found — the mock seam moved, update testsupport/subject.mjs");
  }
  src = src.replace(importRe, () => `from ${JSON.stringify(pathToFileURL(MOCK_PATH).href)}`);

  if (opts.readyWaitMs !== undefined) {
    const capRe = /const READY_WAIT_MS\s*=\s*[0-9_]+;/;
    if (!capRe.test(src)) {
      throw new Error("READY_WAIT_MS declaration not found — update testsupport/subject.mjs");
    }
    src = src.replace(capRe, `const READY_WAIT_MS = ${opts.readyWaitMs};`);
  }

  const dir = path.join(os.tmpdir(), "pi-extension-harness");
  await mkdir(dir, { recursive: true });
  const out = path.join(dir, `subject-${process.pid}-${buildCounter++}.ts`);
  await writeFile(out, src, "utf8");
  return out;
}

/**
 * Imports the subject module fresh. `generation` stands in for what jiti's
 * moduleCache:false does on /reload: a new generation re-evaluates the module
 * (module-level state resets); the same generation reuses it (module-level
 * state is shared, factory re-invocation only).
 */
export async function loadFactory(subjectPath, generation = 0) {
  const url = pathToFileURL(subjectPath).href + `?generation=${generation}`;
  const mod = await import(url);
  if (typeof mod.default !== "function") {
    throw new Error("extension source does not default-export a factory function");
  }
  return mod.default;
}
