package codex

// subagents.go installs Gortex's two sub-agents as Codex custom agents —
// the CLI's delegation surface, its analogue of Claude Code's sub-agents.
// Same bodies, same role: a fresh context that answers one delegated
// question with the graph tools instead of burning the main session's
// window.
//
// The envelope is deliberately four keys wide. Codex 0.147.0 parses an
// agent file into a `#[serde(deny_unknown_fields)]` struct, so ONE
// unrecognised key — a typo, a hopeful future field — rejects the whole
// file, silently. There is no partial load and no warning: the agent
// simply never appears. Everything written below is a key we can name
// from the vendor docs, and nothing else is written at all.
//
// # `mcp_servers` is omitted on purpose
//
// The intuitive `mcp_servers = ["gortex"]` is not a thing. The key is a
// TABLE of full server definitions (the same shape as a top-level
// `[mcp_servers.<id>]` stanza), so the array form fails deserialization
// with `invalid type: sequence, expected a map` — and, per the rule
// above, takes the entire agent file down with it. Omitting the key
// inherits the parent session's MCP servers, which is exactly what these
// two agents need. Codex also has no allowlist semantics, so there is
// nothing to gain by naming servers even in the correct shape: an agent
// cannot be restricted to a subset. Same reasoning retires Claude's
// `tools:` frontmatter, which is spelled in the `mcp__gortex__*`
// vocabulary and has no Codex equivalent.
//
// # User-level only
//
// Personal agents live in `$CODEX_HOME/agents/`. A project-scoped
// `<repo>/.codex/agents/` exists too, but it is gated on the project
// being TRUSTED — an untrusted project's entire config layer is skipped,
// so the directory is never even scanned. Writing there would be a
// coin-flip on a state we cannot see from the installer, and it would put
// Gortex files in the user's working tree, which `gortex init` never
// does. ModeGlobal only, matching every other curated artifact here.
//
// # Discovery details worth not re-deriving
//
// The `.toml` extension is required and matched case-sensitively
// lowercase — a `.TOML` file is ignored. The scan recurses into
// subdirectories, but these are written flat. The agent's NAME comes from
// the `name` key, not the filename; the file is still named after the
// agent because that is the documented convention and it keeps
// inspection and removal obvious.
//
// No feature flag is needed: `features.multi_agent` and `agents.enabled`
// are both stable and default to true. (`multi_agent_v2` is a separate,
// off-by-default surface and is not touched.)
//
// Docs: https://learn.chatgpt.com/codex (custom agents)

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

const (
	// subAgentsDirName is the leaf under ~/.codex that Codex scans.
	subAgentsDirName = "agents"
	// subAgentFileExt is discovery-critical: required, and matched
	// case-sensitively lowercase.
	subAgentFileExt = ".toml"
	// subAgentSandboxMode turns a written instruction into a guarantee.
	// Both agents are read-only by contract — gortex-search only
	// navigates, and gortex-impact's body says "Do not mutate files" —
	// so the sandbox should not be able to grant more than the prompt
	// asks for. `model` and `model_reasoning_effort` are equally legal
	// here and are deliberately NOT written: those are the user's
	// preference, not ours to pin.
	subAgentSandboxMode = "read-only"
)

// SubAgentNames returns the sub-agent IDs, sorted, so Plan, Apply and the
// uninstall preview stay byte-stable across runs.
func SubAgentNames() []string {
	names := make([]string, 0, len(agents.SubAgents))
	for file := range agents.SubAgents {
		names = append(names, subAgentID(file))
	}
	sort.Strings(names)
	return names
}

// SubAgents renders agents.SubAgents into Codex agent TOML, keyed by
// agent ID (the filename without any extension).
func SubAgents() map[string]string {
	out := make(map[string]string, len(agents.SubAgents))
	for file, sharedBody := range agents.SubAgents {
		id := subAgentID(file)
		skill, err := skillpack.Parse(id, sharedBody)
		if err != nil {
			// Compile-time constants: only reachable if the agents package is
			// edited into a shape skillpack rejects. Skip rather than
			// install an agent whose frontmatter we could not rewrite.
			continue
		}
		out[id] = renderSubAgent(skill)
	}
	return out
}

// renderSubAgent emits the four-key agent file.
//
// `developer_instructions` is a multi-line LITERAL string — the form
// delimited by three apostrophes, which processes no escape sequences at
// all. The bodies are Go raw-string constants full of backticks and
// double quotes, and a future edit could easily introduce a backslash;
// inside the basic multi-line form (three double quotes) that backslash
// would be read as an escape and either mangle the text or reject the
// file. A literal string has no such failure mode.
//
// The two things a literal string cannot hold are a run of three
// apostrophes and a trailing apostrophe adjacent to the closing
// delimiter. The renderer puts its own newline before the delimiter so
// the second case cannot arise here, and subagents_test.go fails loudly
// on either — a mangled file is worth a build break, not a silent skip.
func renderSubAgent(skill skillpack.Skill) string {
	var b strings.Builder
	b.WriteString("name = " + quoteTOMLBasicString(skill.ID) + "\n")
	b.WriteString("description = " + quoteTOMLBasicString(skill.Description) + "\n")
	b.WriteString("sandbox_mode = " + quoteTOMLBasicString(subAgentSandboxMode) + "\n")
	b.WriteString("developer_instructions = '''\n")
	b.WriteString(subAgentInstructions(skill))
	b.WriteString("\n'''\n")
	return b.String()
}

// subAgentInstructions is the exact text that lands between the
// delimiters. Split out so the safety test can check the embedded string
// itself rather than re-deriving it from the rendered file.
func subAgentInstructions(skill skillpack.Skill) string {
	return strings.Trim(skill.Body, "\n")
}

// quoteTOMLBasicString renders s as a TOML basic string.
//
// skillpack.QuoteYAMLValue is the wrong tool despite the superficial
// resemblance: it returns identifier-ish values UNQUOTED (`gortex-search`
// comes back bare), which is a YAML convenience and a TOML syntax error —
// TOML has no bare values. Its escape set is YAML's, too. So this is a
// small TOML-correct quoter rather than a reuse.
func quoteTOMLBasicString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			// Every other control character needs the \uXXXX form; TOML
			// forbids raw control characters inside a basic string.
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// subAgentID strips the ".md" the agents package uses as its map key, leaving the
// bare name the Codex filename and the `name` key both carry.
func subAgentID(file string) string {
	return strings.TrimSuffix(file, ".md")
}

// subAgentsDir is ~/.codex/agents. An empty home yields "" rather than a
// relative path — see installSubAgents.
func subAgentsDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", subAgentsDirName)
}

// subAgentPath is ~/.codex/agents/<id>.toml, flat.
func subAgentPath(home, id string) string {
	dir := subAgentsDir(home)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, id+subAgentFileExt)
}

// installSubAgents writes the agent files, ModeGlobal only.
//
// An empty Home is refused rather than defaulted, the same guard
// installCuratedSkills carries: filepath.Join onto an empty home yields a
// RELATIVE ".codex/agents/…", so the artifacts would land in whatever
// directory the process happens to be in — most often the user's repo.
//
// WriteIfNotExists, not WriteOwnedFile: a user who tuned a description or
// pinned a `model` keeps their copy across re-installs.
func installSubAgents(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	if env.Mode != agents.ModeGlobal || env.Home == "" {
		return nil
	}
	defs := SubAgents()
	out := make([]agents.FileAction, 0, len(defs))
	for _, id := range SubAgentNames() {
		body, ok := defs[id]
		if !ok {
			continue
		}
		path := subAgentPath(env.Home, id)
		if path == "" {
			continue
		}
		action, err := agents.WriteIfNotExists(env.Stderr, path, body, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "codex sub-agent %s: %v", id, err)
			continue
		}
		out = append(out, action)
	}
	return out
}

// planSubAgents enumerates what installSubAgents would write. `gortex
// doctor` and `--print-config` read Plan(), never Apply(), so a path Apply
// writes but Plan omits is invisible to both.
func planSubAgents(env agents.Env) []agents.FileAction {
	if env.Mode != agents.ModeGlobal || env.Home == "" {
		return nil
	}
	out := make([]agents.FileAction, 0, len(agents.SubAgents))
	for _, id := range SubAgentNames() {
		path := subAgentPath(env.Home, id)
		if path == "" {
			continue
		}
		out = append(out, agents.FileAction{
			Path:   path,
			Action: agents.ActionWouldCreate,
			Keys:   []string{"sub-agent"},
		})
	}
	return out
}
