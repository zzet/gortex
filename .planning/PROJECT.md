# Gortex Mainframe Engine

## What This Is

A private fork of [zzet/gortex](https://github.com/zzet/gortex) — the graph-based, multi-language code-intelligence engine (Go, tree-sitter, daemon + MCP/CLI/API) — being evolved into a fully-fledged **graph-based mainframe processing engine**. It ingests a mainframe estate into a queryable graph, layers deterministic analysis on top, then LLM enrichment, and ultimately grows into a digital twin of the estate. Built for one user's modernization work, not for upstream.

## Core Value

A trustworthy graph representation of a mainframe estate that modernization cutover decisions can be made against — deterministic and reproducible first, enriched and simulated later.

## Requirements

### Validated

<!-- Inherited from upstream gortex — shipped, working, relied upon. -->

- ✓ Graph indexing engine over 250+ languages (tree-sitter), incremental reindex via daemon — existing
- ✓ Access surfaces: MCP server, CLI, API — existing
- ✓ Graph queries: symbol search, usages, call chains, dataflow edges (`value_flow`/`arg_of`/`returns_to`), AST pattern search, clone detection — existing
- ✓ Analyzer framework (dead code, hotspots, cycles, coverage, routes/models/components, k8s, dbt, cross-repo) — existing
- ✓ Session notes + durable workspace memory layers — existing
- ✓ Token-economy wire formats (GCX1, TOON) and body compression — existing

### Active

<!-- Big-picture staged vision. Detailed per-stage scoping is deliberately deferred to milestone/phase planning. -->

- [ ] **Stage 1 — Deterministic mainframe processing:** ingest mainframe artifacts into the graph (COBOL + copybooks first-class; aspirationally "everything" — JCL, DB2, CICS, etc.), consuming pre-processed input from `cobol-repo-architect`
- [ ] **Stage 1 — Deterministic analyses** over the mainframe graph (impact analysis, lineage, batch flow — exact capability set TBD at phase planning)
- [ ] **Stage 2 — LLM enrichment layer** on top of the deterministic graph (interpretation, summarization, business-rule extraction — scoped later)
- [ ] **Stage 3 — Digital twin, staged:** static structural twin → behavioral simulation → live-synced twin (feasibility-gated), with data integration
- [ ] Twin outputs that directly support **modernization cutover** (the end goal all stages serve)

### Out of Scope

- Upstream contributions / PR parity with zzet/gortex — this fork exists for custom private features
- Reusing `cobol-ingestor` or `mainframe-viewer` internals — explicit fresh start; those tools stay separate
- Committing to a v1 artifact list or deterministic feature set now — user chose to keep planning big-picture; details land in phase planning
- Live-synced twin as a hard commitment — pursued only "if possible and feasible" after static + behavioral stages prove out

## Context

- Upstream base: gortex v0.61.x — Go, tree-sitter grammars, sqlite-fts5 search, long-running daemon, MCP/CLI/API surfaces. Full codebase map in `.planning/codebase/`.
- `cobol-repo-architect` (sibling project in GoApps) is the **preprocessing front door**: it sets up / pre-processes mainframe source for this engine to ingest.
- Related prior work (`cobol-ingestor`, `mainframe-viewer`) informs the domain but is intentionally not coupled.
- Fork remotes: `origin` = MuiGoku123432/gortex (personal, via `github-personal` SSH), `upstream` = zzet/gortex.
- The engine's existing multi-language graph, dataflow, and analyzer machinery is the substrate the mainframe layers extend.

## Constraints

- **Tech stack**: Go + tree-sitter ecosystem, extending gortex's existing architecture — evolve the engine, don't rewrite it
- **Fork hygiene**: keep the ability to pull upstream improvements — prefer additive packages/analyzers over invasive edits to upstream internals where practical
- **Sequencing**: deterministic layer before LLM enrichment before twin — trust and reproducibility are prerequisites for the later stages
- **Ownership**: personal project under the MuiGoku123432 GitHub account

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Fork zzet/gortex rather than build from scratch | Mature graph engine, daemon, MCP/CLI surfaces, and analyzer framework already exist | — Pending |
| Fresh start vs. existing mainframe tools | Avoid legacy coupling; `cobol-repo-architect` becomes the preprocessing front door instead | — Pending |
| Staged twin: static → behavioral → live-synced | De-risks the vision; each stage has standalone modernization value | — Pending |
| Deterministic before LLM enrichment | LLM interpretation belongs on top of a reproducible substrate, not in place of one | — Pending |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition** (via `/gsd-transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd-complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-07-24 after initialization*
