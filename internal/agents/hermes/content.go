package hermes

import (
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
	yaml "gopkg.in/yaml.v3"
)

// gortexServerName is the key the gortex stanza lives under in the
// Hermes `mcp_servers` map. Stable across releases so re-installs
// upsert in place rather than duplicating.
const gortexServerName = "gortex"

// connectTimeoutSecs / requestTimeoutSecs match a real-world working
// Hermes ↔ gortex setup: the daemon-backed MCP server can take a
// moment to hand off on first connect and graph-heavy tools (smart_
// context, analyze) occasionally run longer than Hermes' tight
// defaults, so we give both a generous ceiling out of the box.
const (
	connectTimeoutSecs = 60
	requestTimeoutSecs = 120
)

// gortexMCPEntry builds the stdio MCP stanza Hermes expects under
// `mcp_servers.gortex`. It mirrors the shape Hermes uses for every
// other stdio server (command + args + the two timeout knobs):
//
//	gortex:
//	  command: /abs/path/to/gortex
//	  args: [mcp]
//	  connect_timeout: 60
//	  timeout: 120
//
// `gortex mcp` (no flags) connects to a running daemon and resolves
// the active workspace per MCP session, so one global stanza serves
// every repo Hermes is pointed at — no cwd-relative state to trip on.
func gortexMCPEntry(command string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	agents.YAMLSetMapValue(entry, "command", agents.YAMLScalar(command))

	// Flow style — `args: [mcp]` — to match the canonical Hermes
	// example and keep the inserted block as compact as the rest of
	// a hand-written config.
	args := &yaml.Node{
		Kind:    yaml.SequenceNode,
		Tag:     "!!seq",
		Style:   yaml.FlowStyle,
		Content: []*yaml.Node{agents.YAMLScalar("mcp")},
	}
	agents.YAMLSetMapValue(entry, "args", args)
	agents.YAMLSetMapValue(entry, "connect_timeout", yamlInt(connectTimeoutSecs))
	agents.YAMLSetMapValue(entry, "timeout", yamlInt(requestTimeoutSecs))
	return entry
}

// yamlInt builds an integer scalar node. Kept here rather than in the
// agents package because the generic YAMLScalar helper only covers
// strings and Hermes is the only adapter that needs typed YAML
// scalars today.
func yamlInt(n int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
}

// SkillName is the directory under ~/.hermes/skills/ that holds the
// gortex skill. Hermes discovers SKILL.md files recursively under the
// skills root, so a single `gortex/SKILL.md` is picked up regardless
// of the per-version layout.
const SkillName = "gortex"

// masterSkillCategory is the Hermes skills category the master gortex
// guide is filed under (skills/<category>/gortex/SKILL.md). Shared with
// skillCategory so the on-disk folder and the frontmatter agree.
const masterSkillCategory = "code-intelligence"

// masterSkillRaw is the lean, operation-oriented master guide emitted for
// every new Hermes integration.
const masterSkillRaw = `---
name: gortex
description: "Use Gortex for indexed-code exploration, reads, relationships, impact checks, edits, and refactors."
version: 1.0.0
metadata:
  hermes:
    tags: [code-intelligence, code-search, navigation, refactoring, mcp]
    category: code-intelligence
---

# Gortex code intelligence

Use Gortex MCP tools for indexed code. This is mandatory.

1. Start every coding task with ` + "`explore`" + ` using ` + "`operation: \"task\"`" + ` and the task text.
2. Use ` + "`search`" + ` for symbols, text, files, or AST shapes. Use ` + "`read`" + ` for source, summaries, files, or editing context.
3. Use ` + "`relations`" + ` for usages, callers, dependencies, dependents, and implementations. Use ` + "`trace`" + ` for call chains and dataflow.
4. Before mutation, call ` + "`change`" + ` with ` + "`operation: \"impact\"`" + `; for a signature change, also call operation ` + "`verify`" + ` with the proposed signature.
5. Mutate only with ` + "`edit`" + ` or ` + "`refactor`" + `. After mutation, call ` + "`change`" + ` operations ` + "`detect`" + `, ` + "`tests`" + `, ` + "`guards`" + `, and ` + "`contract`" + `.
6. Call ` + "`capabilities`" + ` with ` + "`domain`" + `, ` + "`operation`" + `, and ` + "`detail: \"schema\"`" + ` when exact arguments are not visible.

Do not replace graph reads or searches with terminal commands. If the configured Gortex tools are missing from the callable MCP tools, report a Gortex MCP integration failure and stop; do not start a daemon or use a CLI/shell fallback.

Use ` + "`workspace`" + ` for local index, repository, and project state. Use ` + "`workspace_admin`" + ` only when the user asks to change that state. Use ` + "`recall`" + ` before editing known code and ` + "`remember`" + ` for durable decisions or invariants.
`

// SkillBody renders the master gortex skill: the static guide
// (masterSkillRaw) with the dynamic frontmatter fields (platforms,
// related_skills) injected and the slash-command index appended. The
// dynamic parts derive from RoutingSkillNames() so they stay in sync
// with the routing skills actually installed.
func SkillBody() string {
	const fmClose = "    category: " + masterSkillCategory + "\n---\n"
	inject := "    category: " + masterSkillCategory + "\n" +
		"    platforms: [linux, macos, windows]\n" +
		"    related_skills: [" + strings.Join(RoutingSkillNames(), ", ") + "]\n---\n"
	return strings.Replace(masterSkillRaw, fmClose, inject, 1) + masterSkillCommands()
}

// masterSkillCommands renders a discoverability section listing the
// /gortex-* slash commands that `gortex install` registers (Hermes turns
// every installed skill into a /<name> command). Derived from
// RoutingSkillNames() so the list can't drift from what is installed.
func masterSkillCommands() string {
	names := RoutingSkillNames()
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Task playbooks (slash commands)\n\n")
	b.WriteString("`gortex install` also registers these per-task playbooks as Hermes slash commands. Reach for the one that matches your task:\n\n")
	for _, n := range names {
		b.WriteString("- `/" + n + "`\n")
	}
	return b.String()
}

// RoutingSkills returns the per-task routing skills, keyed by the
// directory under ~/.hermes/skills/. They mirror Claude Code's curated
// user-level skill set so a Hermes user gets the same task-routing
// surface — explore / impact / debug / refactor / rename / safe-edit /
// add-test / pr-review / … — that `gortex install` gives Claude Code.
//
// The bodies are the single source of truth in the agents package
// (reused verbatim so no two hosts drift); internal/agents/skillpack
// strips the shared frontmatter off them and Hermes' own envelope goes
// back on. Hermes also turns every installed skill into a `/skill-name`
// slash command, so the `/gortex-explore`-style cross-references in the
// bodies resolve to the sibling skills installed here.
//
// gortex-guide is excluded: the native master `gortex` skill
// (SkillBody) already fills the guide role, and shipping both would be
// a redundant entry in Hermes' skill picker.
func RoutingSkills() map[string]string {
	out := make(map[string]string, len(agents.GlobalSkills))
	for name, sharedBody := range agents.GlobalSkills {
		if name == "gortex-guide" {
			continue
		}
		skill, err := skillpack.Parse(name, sharedBody)
		if err != nil {
			// The shared bodies are compile-time constants, so this can
			// only fire if the agents package is edited into a shape
			// skillpack rejects (a non-kebab id, an unclosed frontmatter
			// fence). Skip rather than install a skill whose frontmatter we
			// could not rewrite — skillpack's test over agents.GlobalSkills
			// is what turns the mistake into a red build.
			continue
		}
		out[name] = hermesSkillFromShared(skill)
	}
	return out
}

// RoutingSkillNames returns the routing skill directory names, sorted,
// for a stable Plan / install report and deterministic tests.
func RoutingSkillNames() []string {
	skills := RoutingSkills()
	names := make([]string, 0, len(skills))
	for name := range skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hermesSkillFromShared re-frames one parsed skill as a Hermes skill:
// it keeps the body verbatim and puts Hermes frontmatter (name +
// description + version + metadata.hermes.{tags,category}) in front of
// it.
//
// The frontmatter is built here rather than with
// skillpack.RenderFrontmatter because Hermes' block is a nested mapping
// with flow sequences, which the flat ordered-pair renderer cannot
// express. Only the description scalar goes through skillpack, so the
// prose quoting rule stays shared with every other host.
func hermesSkillFromShared(skill skillpack.Skill) string {
	tags, category := routingSkillTaxonomy(skill.ID)

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + skill.ID + "\n")
	if skill.Description != "" {
		b.WriteString("description: " + skillpack.QuoteYAMLValue(skill.Description) + "\n")
	}
	b.WriteString("version: 1.0.0\n")
	b.WriteString("metadata:\n")
	b.WriteString("  hermes:\n")
	b.WriteString("    tags: [" + strings.Join(tags, ", ") + "]\n")
	b.WriteString("    category: " + category + "\n")
	b.WriteString("    platforms: [linux, macos, windows]\n")
	b.WriteString("    related_skills: [" + SkillName + "]\n")
	b.WriteString("---\n")
	b.WriteString(skill.Body)
	return b.String()
}

// routingSkillTaxonomy assigns Hermes discovery tags + a category to a
// routing skill from its name. The per-skill topic tag (the name minus
// the `gortex-` prefix) plus a broad category make the skill findable
// in Hermes' skills_list without hand-maintaining a table per skill.
func routingSkillTaxonomy(name string) (tags []string, category string) {
	topic := strings.TrimPrefix(name, "gortex-")
	category = "code-intelligence"
	switch topic {
	case "explore", "onboarding", "cross-repo-usage", "dataflow-trace":
		category = "navigation"
	case "impact", "co-change", "architecture-review", "quality-audit", "pr-review", "pr-review-agent", "episode-replay":
		category = "analysis"
	case "debug", "incident-investigation":
		category = "debugging"
	case "refactor", "rename", "extract-function", "safe-edit", "fix-all":
		category = "refactoring"
	case "add-test":
		category = "testing"
	}
	return []string{"code-intelligence", "mcp", topic}, category
}
