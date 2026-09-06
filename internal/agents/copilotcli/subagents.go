package copilotcli

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// subagents.go installs Copilot CLI custom agents — the CLI's delegation
// surface, its analogue of Claude Code's sub-agents. Same bodies, same
// role: a fresh context that answers one delegated question with the
// graph tools instead of burning the main session's window.
//
// Two vendor facts shape what is written:
//
//   - The file is `NAME.agent.md`, not `NAME.md`. A plain `.md` in the
//     agents directory is not picked up.
//   - A home-level agent SHADOWS a repo agent of the same name. That is
//     why these are user-level only: writing `gortex-search` into both
//     ~/.copilot/agents/ and .github/agents/ would make the repo copy
//     permanently dead, and committing it would push it at every
//     teammate who clones.
//
// Docs: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/create-custom-agents

const (
	// subAgentsDirName is the leaf under the CLI config home.
	subAgentsDirName = "agents"
	// subAgentFileSuffix is the discovery-critical double extension.
	subAgentFileSuffix = ".agent.md"
)

// SubAgentNames returns the sub-agent IDs, sorted, so Plan and the
// install report stay byte-stable across runs.
func SubAgentNames() []string {
	names := make([]string, 0, len(agents.SubAgents))
	for file := range agents.SubAgents {
		names = append(names, subAgentID(file))
	}
	sort.Strings(names)
	return names
}

// SubAgents renders agents.SubAgents into the Copilot envelope,
// keyed by agent ID (the filename without any extension).
//
// The envelope carries `name` + `description` only. The CLI also accepts
// `tools`, and the Claude source declares one — but in Claude's own
// `mcp__gortex__*` vocabulary, which is a Claude Code naming convention
// rather than anything the Copilot CLI resolves. An unrecognised entry in
// a `tools` allowlist does not fall back to "allow everything"; it grants
// nothing, so a faithless mapping would hand the agent an empty toolbox
// and no error message. Omitting the key inherits the session's tools,
// which is the safe half of that trade.
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
		out[id] = skillpack.RenderFrontmatter([][2]string{
			{"name", skill.ID},
			{"description", skill.Description},
		}) + skill.Body
	}
	return out
}

// subAgentID strips the ".md" the agents package uses as its map key, leaving the
// bare name the Copilot filename and frontmatter both need.
func subAgentID(file string) string {
	return strings.TrimSuffix(file, ".md")
}

// subAgentPath is ~/.copilot/agents/<name>.agent.md.
func subAgentPath(env agents.Env, id string) string {
	home := copilotConfigHome(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, subAgentsDirName, id+subAgentFileSuffix)
}

// installSubAgents writes the sub-agent definitions, ModeGlobal only.
// Existing files are left alone so a user's tuned description or added
// `tools` allowlist survives re-installs.
func installSubAgents(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	defs := SubAgents()
	out := make([]agents.FileAction, 0, len(defs))
	for _, id := range SubAgentNames() {
		body, ok := defs[id]
		if !ok {
			continue
		}
		path := subAgentPath(env, id)
		if path == "" {
			continue
		}
		action, err := agents.WriteIfNotExists(env.Stderr, path, body, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "copilot-cli sub-agent %s: %v", id, err)
			continue
		}
		out = append(out, action)
	}
	return out
}
