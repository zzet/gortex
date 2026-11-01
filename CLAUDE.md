@AGENTS.md

## Claude Code

The shared guidance for every coding agent lives in `AGENTS.md`, imported above. This section holds only what is specific to Claude Code.

- PreToolUse hooks deny `Read` / `Grep` / `Glob` against indexed source; the deny message names the right Gortex tool.
- The cross-project rule tables live in `~/.claude/CLAUDE.md` and are not restated here.
