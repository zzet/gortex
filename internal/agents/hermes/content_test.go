package hermes

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// TestRoutingSkillsMatchLegacyRendering is the zero-diff guard for the
// move onto internal/agents/skillpack. legacyHermesSkill below is the
// frontmatter-swapping code Hermes carried before the extraction,
// verbatim; every installed routing skill must still come out
// byte-identical to what it produced. A shared parser is only worth
// having if it cannot shift a single byte of an installed file.
func TestRoutingSkillsMatchLegacyRendering(t *testing.T) {
	routing := RoutingSkills()
	if len(routing) == 0 {
		t.Fatal("no routing skills derived")
	}
	for name, got := range routing {
		want := legacyHermesSkill(name, agents.GlobalSkills[name])
		if got != want {
			t.Errorf("%s: rendering changed\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
		}
	}
}

// TestRoutingSkillFrontmatterIsFrozen pins one full frontmatter block
// as a literal, so a future change to the shared renderer (quoting
// rules, key order) fails here rather than silently reshaping every
// SKILL.md Hermes has on disk.
func TestRoutingSkillFrontmatterIsFrozen(t *testing.T) {
	const want = `---
name: gortex-explore
description: "Understand how code works or trace an execution flow."
version: 1.0.0
metadata:
  hermes:
    tags: [code-intelligence, mcp, explore]
    category: navigation
    platforms: [linux, macos, windows]
    related_skills: [gortex]
---
`
	got, ok := RoutingSkills()["gortex-explore"]
	if !ok {
		t.Fatal("gortex-explore routing skill missing")
	}
	if !strings.HasPrefix(got, want) {
		t.Errorf("frontmatter drifted\n--- got ---\n%s\n--- want ---\n%s", firstN(got, len(want)), want)
	}
	// The shared body must follow the Hermes frontmatter untouched —
	// with no second `---` block stacked on top of it.
	body := strings.TrimPrefix(got, want)
	if strings.HasPrefix(body, "---") {
		t.Errorf("body still carries the shared frontmatter:\n%s", firstN(body, 120))
	}
	if !strings.HasSuffix(agents.GlobalSkills["gortex-explore"], body) {
		t.Errorf("body is not the shared body verbatim:\n%s", firstN(body, 200))
	}
}

// legacyHermesSkill is the pre-skillpack implementation, kept here as
// the reference oracle for the test above. Do not "simplify" it to call
// the production code — a guard that shares the code under test proves
// nothing.
func legacyHermesSkill(name, sharedContent string) string {
	desc := legacyFrontmatterField(sharedContent, "description")
	body := legacyStripFrontmatter(sharedContent)
	tags, category := routingSkillTaxonomy(name)

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	if desc != "" {
		b.WriteString("description: " + desc + "\n")
	}
	b.WriteString("version: 1.0.0\n")
	b.WriteString("metadata:\n")
	b.WriteString("  hermes:\n")
	b.WriteString("    tags: [" + strings.Join(tags, ", ") + "]\n")
	b.WriteString("    category: " + category + "\n")
	b.WriteString("    platforms: [linux, macos, windows]\n")
	b.WriteString("    related_skills: [" + SkillName + "]\n")
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

func legacyStripFrontmatter(s string) string {
	const open = "---\n"
	if !strings.HasPrefix(s, open) {
		return s
	}
	rest := s[len(open):]
	if _, after, found := strings.Cut(rest, "\n---\n"); found {
		return after
	}
	return s
}

func legacyFrontmatterField(s, field string) string {
	const open = "---\n"
	if !strings.HasPrefix(s, open) {
		return ""
	}
	block := s[len(open):]
	if before, _, found := strings.Cut(block, "\n---\n"); found {
		block = before
	}
	prefix := field + ":"
	for line := range strings.SplitSeq(block, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
