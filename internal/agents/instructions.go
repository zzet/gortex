// Instructions shared across every doc-aware adapter. Centralising the
// body here avoids per-adapter drift: Cursor's .cursor/rules file,
// Copilot's .github/copilot-instructions.md, Codex's AGENTS.md, and
// Claude Code's CLAUDE.md all read from the same constant, so when the
// "prefer Gortex over Read/Grep" story evolves we update it once and
// every agent sees the change on the next `gortex init`.
//
// The claudecode adapter extends this body with its own slash-commands
// appendix — that part is Claude-Code-specific and lives in
// claudecode/content.go, keyed off the same sentinel so idempotency
// checks line up across adapters.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/profiles"
)

// InstructionsSentinel is the substring every doc-aware adapter checks
// for when deciding whether to append the instructions block. If it's
// already present (wherever it came from — a prior `gortex init`, a
// user-copied block, another adapter writing to a shared rules file
// like AGENTS.md) we skip to stay idempotent.
const InstructionsSentinel = "## MANDATORY: Use Gortex MCP tools"

// BashInstructionsSentinel identifies the separate policy installed only by
// harness adapters that have no native MCP transport.
const BashInstructionsSentinel = "## MANDATORY: Use the Gortex public CLI mirror"

// CommunitiesStartMarker / CommunitiesEndMarker fence the generated
// community-routing block that `gortex init` writes into per-repo
// instructions files. Fenced (not just start-only) because this block
// is regenerated on every `init` re-run as the codebase evolves, so
// we need to identify and overwrite it precisely without clobbering
// user edits around it.
const (
	CommunitiesStartMarker = "<!-- gortex:communities:start -->"
	CommunitiesEndMarker   = "<!-- gortex:communities:end -->"
)

// GlobalRulesStartMarker / GlobalRulesEndMarker fence the rule block
// that `gortex install` merges into ~/.claude/CLAUDE.md. The block is
// idempotent (re-running install replaces it in place) and removable
// (user can delete the marked region by hand without other side
// effects). Distinct from the communities markers above because this
// block lives at user level and survives every project init.
const (
	GlobalRulesStartMarker = "<!-- gortex:rules:start -->"
	GlobalRulesEndMarker   = "<!-- gortex:rules:end -->"
)

// GlobalPointerBody renders the thin machine-level rule block
// `gortex install` merges into ~/.claude/CLAUDE.md. The rule content
// itself lives in <instructionsDir>/active.md — an atomic byte copy of
// the selected instruction profile (internal/profiles) — and is pulled
// in through an @-include, so switching guidance depth never rewrites
// CLAUDE.md. The heading stays here as the idempotency sentinel and as
// a functional minimum for readers that do not expand @-includes.
func GlobalPointerBody(instructionsDir string) string {
	// The @-include is document content, not a filesystem call: it is
	// written into ~/.claude/CLAUDE.md and read back as markdown. Native
	// separators would leak a `@C:\Users\me\.gortex\...` line into a file
	// whose every other path is '/'-spelled, and the same rendered body
	// is compared against a golden that cannot be platform-specific. This
	// mirrors shellSafeHookBinary, which normalises for the same reason.
	active := filepath.ToSlash(filepath.Join(instructionsDir, profiles.ActiveFileName))
	return "## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob\n\n" +
		"The machine-wide Gortex rules load from the active instruction profile, imported below:\n\n" +
		"@" + active + "\n\n" +
		switchDepthLine
}

// switchDepthLine is the profile-discovery footer the pointer body
// carries, so a machine that switched guidance depth can always find its
// way back. The generated profile bodies end with their own switch-back
// bullet; this is for the surfaces that do not embed one.
const switchDepthLine = "Switch guidance depth with `gortex instructions switch <core|localization|full>` (`list` shows all) — applies to NEW sessions only.\n"

// GlobalInlineBody renders the machine-level rule block for agents whose
// instructions file is consumed as literal markdown, with no @-include
// mechanism to follow. Claude Code gets GlobalPointerBody — a thin
// pointer at <instructionsDir>/active.md, so `gortex instructions
// switch` never has to rewrite CLAUDE.md. Codex reads its AGENTS.md
// verbatim (an @path line is prose to it), so it gets a copy of the
// active profile body inlined instead. The block is marker-fenced, so
// `gortex install` and `gortex instructions switch` both refresh the
// copy in place rather than appending a second one.
//
// No switch-back footer is appended: every generated profile body
// already ends with one, and the file Codex loads on every session
// should not pay for the same line twice.
func GlobalInlineBody(instructionsDir string) string {
	body := strings.TrimRight(profiles.Active(instructionsDir).Body(), "\n")
	if body == "" {
		// No profile row resolved (unknown name, empty table row): fall
		// back to the shared agent-neutral block plus the switch footer,
		// so the agent still gets the rule rather than an empty fence.
		return strings.TrimRight(InstructionsBody, "\n") + "\n\n" + switchDepthLine
	}
	return body + "\n"
}

// InstructionsDir resolves where the generated instruction profiles live
// for an install run: the Env override (tests pin a temp dir) or the
// machine default shared with the daemon and the `gortex instructions`
// verb.
func InstructionsDir(env Env) string {
	if env.InstructionsDir != "" {
		return env.InstructionsDir
	}
	return profiles.DefaultDir()
}

// InstructionsBody is the shared, agent-neutral rule block every doc-aware
// adapter writes. It names only the compact public tools and treats a missing
// callable handle as an integration failure; transport versions,
// implementation aliases, and cross-transport fallbacks do not belong in an
// MCP-capable agent's working context.
const InstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

For every coding task:

1. Call ` + "`" + `explore` + "`" + ` first with the complete task. Work from the returned source and call paths; do not reopen them with file or shell tools.
2. Inspect indexed code only with ` + "`" + `search` + "`" + `, ` + "`" + `read` + "`" + `, ` + "`" + `relations` + "`" + `, and ` + "`" + `trace` + "`" + `. Never use Read/Grep/Glob or shell equivalents for indexed source.
3. Before mutation, call ` + "`" + `change(operation:"impact")` + "`" + `; for a signature change, also call ` + "`" + `change(operation:"verify")` + "`" + ` with the proposed signature. Mutate only with ` + "`" + `edit` + "`" + ` or ` + "`" + `refactor` + "`" + `. After mutation, call ` + "`" + `change(operation:"detect")` + "`" + `, then use its symbol IDs with ` + "`" + `change(operation:"tests")` + "`" + `, ` + "`" + `change(operation:"guards")` + "`" + `, and ` + "`" + `change(operation:"contract")` + "`" + `.
4. Call ` + "`" + `capabilities` + "`" + ` only when you need the exact fields for an operation.

Common calls:

- ` + "`" + `search({operation:"symbols", query:"<name or concept>"})` + "`" + `
- ` + "`" + `read({target:{symbol:"<file>::<Name|Recv.Name>"}})` + "`" + `
- ` + "`" + `relations({operation:"usages", target:{symbol:"<id>"}})` + "`" + `
- ` + "`" + `edit({target:{file:"<path>"}, match:"<old>", replacement:"<new>"})` + "`" + `

If the Gortex server is configured but these tools are missing from the callable MCP tools, report a Gortex MCP integration failure and stop. Do not start a daemon or switch to a CLI/shell fallback.

Use ` + "`" + `recall` + "`" + ` before revisiting prior work and ` + "`" + `remember` + "`" + ` immediately for durable decisions, invariants, or gotchas.
`

// BashInstructionsBody is used only by harnesses that genuinely have no MCP
// transport. It mirrors the same compact public surface through the CLI rather
// than teaching MCP-capable agents to silently change transports.
const BashInstructionsBody = BashInstructionsSentinel + ` instead of raw source reads/searches

This harness has no native MCP transport. Invoke public Gortex tools only as ` + "`" + `gortex call <tool>` + "`" + `; never invent a bare ` + "`" + `gortex <tool>` + "`" + ` command.

1. Start every coding task with ` + "`" + `gortex call explore --arg task="<task>"` + "`" + `.
2. Inspect with ` + "`" + `gortex call search` + "`" + `, ` + "`" + `gortex call read` + "`" + `, ` + "`" + `gortex call relations` + "`" + `, or ` + "`" + `gortex call trace` + "`" + ` instead of Read/Grep/Glob or shell equivalents.
3. Before mutation call ` + "`" + `gortex call change --arg operation=impact` + "`" + `. Mutate only through ` + "`" + `gortex call edit` + "`" + ` or ` + "`" + `gortex call refactor` + "`" + `; afterward run change operations ` + "`" + `detect` + "`" + `, ` + "`" + `tests` + "`" + `, ` + "`" + `guards` + "`" + `, and ` + "`" + `contract` + "`" + `.
4. Use ` + "`" + `gortex call capabilities` + "`" + ` only when exact operation fields are unknown.
`

// UpsertMarkedBlock is the only supported way to put a Gortex block into
// a human-edited instructions file. The older sentinel-guarded append it
// replaced could not update a block in place — it detected the sentinel
// and skipped, so a changed body never reached a file that already had
// one — and it wrote non-atomically. Adapters must not reintroduce that
// pattern; see UpsertMarkedBlock below.

// CursorMDCFrontmatter wraps the instructions body in the YAML
// frontmatter Cursor expects for MDC rules files. Cursor reads
// `alwaysApply: true` rules on every chat turn — which is what we
// want for the MANDATORY-prefer-Gortex block.
//
// Kept separate from AppendInstructions because MDC files are
// one-rule-per-file (Cursor owns the filename, not the content), so
// they use WriteIfNotExists semantics, not append.
func CursorMDCFrontmatter(body string) string {
	return `---
description: Gortex code intelligence — prefer graph tools over file reads
alwaysApply: true
---

` + body
}

// UpsertMarkedBlock writes `body` into `path` between `startMarker`
// and `endMarker`. Unlike AppendInstructions, this is idempotent AND
// regeneratable: if the markers already exist the block between them
// is replaced; otherwise the block is appended with a blank-line gap
// to existing content. If `body` is empty and the markers exist, the
// block is removed (migration use case). Creates the file if missing.
//
// Designed for the per-repo community-routing block which regenerates
// on every `gortex init` run as the graph evolves.
func UpsertMarkedBlock(w io.Writer, path, body, startMarker, endMarker string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	text := ""
	if existed {
		text = string(existing)
	}

	hasBlock := existed && strings.Contains(text, startMarker) && strings.Contains(text, endMarker)
	empty := strings.TrimSpace(body) == ""

	// Nothing to do: empty body and no existing block.
	if empty && !hasBlock {
		return FileAction{Path: path, Action: ActionSkip, Reason: "no-communities"}, nil
	}

	fenced := startMarker + "\n" + body + "\n" + endMarker + "\n"

	var next string
	switch {
	case hasBlock:
		start := strings.Index(text, startMarker)
		end := strings.Index(text, endMarker) + len(endMarker)
		// Trim trailing newline after the end marker so we don't
		// accumulate blank lines on repeated re-runs.
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if empty {
			next = text[:start] + text[end:]
		} else {
			next = text[:start] + fenced + text[end:]
		}
	case !existed:
		next = fenced
	default:
		prefix := ""
		if len(text) > 0 {
			if !strings.HasSuffix(text, "\n") {
				prefix = "\n\n"
			} else if !strings.HasSuffix(text, "\n\n") {
				prefix = "\n"
			}
		}
		next = text + prefix + fenced
	}

	// Skip when the file would end up byte-identical to what's
	// already there — important for AssertIdempotent semantics and
	// for avoiding spurious mtime bumps on `gortex init` re-runs
	// when the graph hasn't changed.
	if existed && next == text {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		switch {
		case !existed:
			return FileAction{Path: path, Action: ActionWouldCreate, Keys: []string{"communities-block"}}, nil
		case hasBlock:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		default:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		verb := "updated"
		if !existed {
			verb = "wrote"
		}
		fmt.Fprintf(w, "[gortex init] %s %s (communities block)\n", verb, path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"communities-block"}}, nil
}
