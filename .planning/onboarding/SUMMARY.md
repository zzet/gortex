# Onboarding Summary

## Project State
- PROJECT.md: present
- REQUIREMENTS.md: missing — deliberately deferred (owner wants initialization only; scoping happens when building starts)
- ROADMAP.md: missing — deliberately deferred (same reason)
- STATE.md: missing — created by the roadmapper when the roadmap is made
- config.json: present (yolo, standard granularity, parallel, research/plan-check/verifier/drift-guard on, adaptive models)

## Codebase Context
- Brownfield repo: yes (fork of zzet/gortex, ~2,700 commits, Go)
- Map readiness: complete
- Codebase map: `.planning/codebase/` (complete codebase map — STACK, ARCHITECTURE, STRUCTURE, CONVENTIONS, TESTING, INTEGRATIONS, CONCERNS)
- Fast map available: yes

## Docs Context
- Existing ADR/PRD/SPEC/RFC candidates: 0

## Project Vision (see PROJECT.md)
Evolve this gortex fork into a graph-based mainframe processing engine:
deterministic processing → LLM enrichment → digital twin (static → behavioral → live-synced) in support of modernization cutover. `cobol-repo-architect` is the preprocessing front door.

## Recommended Next Step
When ready to start building: `/gsd-next` (routes to requirements + roadmap creation, then phase planning).
