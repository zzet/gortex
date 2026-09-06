// Package skillpack_test is deliberately an external test package: it
// imports internal/agents to assert the real skill corpus parses, and
// skillpack itself must never import its parent. Keeping the assertion
// outside the package means a later refactor can move the corpus without
// turning this file into an import cycle.
package skillpack_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// TestParseAllClaudeSkills is the corpus test: every shipped skill must
// survive the neutral round trip, because a skill that loses its name
// or description is one a host silently drops from its picker, and a
// body that still carries frontmatter renders two stacked blocks.
func TestParseAllSharedSkills(t *testing.T) {
	skills, err := skillpack.ParseAll(agents.GlobalSkills)
	if err != nil {
		t.Fatalf("ParseAll(agents.GlobalSkills): %v", err)
	}
	if len(skills) != len(agents.GlobalSkills) {
		t.Fatalf("parsed %d skills, want %d", len(skills), len(agents.GlobalSkills))
	}

	for _, skill := range skills {
		if skill.Name == "" {
			t.Errorf("%s: empty frontmatter name", skill.ID)
		}
		if skill.Description == "" {
			t.Errorf("%s: empty frontmatter description", skill.ID)
		}
		if strings.HasPrefix(skill.Body, "---") {
			t.Errorf("%s: body still starts with frontmatter:\n%.80s", skill.ID, skill.Body)
		}
		if skill.Body == "" {
			t.Errorf("%s: empty body", skill.ID)
		}
	}
}

// TestParseAllIsIDSorted pins the ordering guarantee ParseAll exists
// for: callers render install plans and golden artifacts from this
// slice, and Go's randomised map iteration would otherwise reorder
// them on every run.
func TestParseAllIsIDSorted(t *testing.T) {
	skills, err := skillpack.ParseAll(agents.GlobalSkills)
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	for i := 1; i < len(skills); i++ {
		if skills[i-1].ID >= skills[i].ID {
			t.Fatalf("not ID-sorted at %d: %q then %q", i, skills[i-1].ID, skills[i].ID)
		}
	}
}

// TestSkillIDsAreKebabCase guards a real host constraint, not a style
// preference: OpenCode and GitHub Copilot both refuse to load a skill
// whose name is not kebab-case, and they refuse it silently.
func TestSkillIDsAreKebabCase(t *testing.T) {
	kebab := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	for id := range agents.GlobalSkills {
		if !kebab.MatchString(id) {
			t.Errorf("skill id %q is not kebab-case", id)
		}
		if !skillpack.ValidID(id) {
			t.Errorf("skill id %q rejected by skillpack.ValidID", id)
		}
	}
}

// TestRoundTripReproducesSource proves Parse loses nothing a host needs
// to rebuild the original file. Doing it over the whole corpus (not one
// representative) is what lets an adapter re-render descriptions
// through QuoteYAMLValue without shifting a single installed byte.
func TestRoundTripReproducesSource(t *testing.T) {
	skills, err := skillpack.ParseAll(agents.GlobalSkills)
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	for _, skill := range skills {
		got := skillpack.RenderFrontmatter([][2]string{
			{"name", skill.Name},
			{"description", skill.Description},
		}) + skill.Body
		if want := agents.GlobalSkills[skill.ID]; got != want {
			t.Errorf("%s: round trip changed the file\n--- got ---\n%s\n--- want ---\n%s", skill.ID, firstLines(got, 6), firstLines(want, 6))
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		raw     string
		want    skillpack.Skill
		wantErr bool
	}{
		{
			name: "claude style",
			id:   "gortex-explore",
			raw:  "---\nname: gortex-explore\ndescription: \"Understand how code works.\"\n---\n# Explore\n\nbody\n",
			want: skillpack.Skill{
				ID:          "gortex-explore",
				Name:        "gortex-explore",
				Description: "Understand how code works.",
				Body:        "# Explore\n\nbody\n",
			},
		},
		{
			// Not an error: a host may carry name/description out of band
			// (a manifest, a directory name) and ship only markdown.
			name: "no frontmatter",
			id:   "plain-skill",
			raw:  "# Just markdown\n\nno frontmatter here\n",
			want: skillpack.Skill{ID: "plain-skill", Body: "# Just markdown\n\nno frontmatter here\n"},
		},
		{
			name: "crlf line endings",
			id:   "crlf-skill",
			raw:  "---\r\nname: crlf-skill\r\ndescription: \"Windows authored.\"\r\n---\r\n# Body\r\n",
			want: skillpack.Skill{
				ID:          "crlf-skill",
				Name:        "crlf-skill",
				Description: "Windows authored.",
				Body:        "# Body\r\n",
			},
		},
		{
			// The colon is why descriptions are quoted at all: unquoted,
			// YAML reads "Gortex reference: tools" as a nested mapping.
			name: "description containing a colon",
			id:   "gortex-guide",
			raw:  "---\nname: gortex-guide\ndescription: \"Gortex reference: available tools, graph schema.\"\n---\nbody\n",
			want: skillpack.Skill{
				ID:          "gortex-guide",
				Name:        "gortex-guide",
				Description: "Gortex reference: available tools, graph schema.",
				Body:        "body\n",
			},
		},
		{
			name: "missing description",
			id:   "bare-skill",
			raw:  "---\nname: bare-skill\n---\nbody\n",
			want: skillpack.Skill{ID: "bare-skill", Name: "bare-skill", Body: "body\n"},
		},
		{
			// An indented key belongs to a nested mapping (Hermes nests
			// its discovery metadata) and is not the skill's own field.
			name: "nested key is not the top-level field",
			id:   "nested-skill",
			raw:  "---\nname: nested-skill\nmetadata:\n  hermes:\n    description: \"nested\"\n---\nbody\n",
			want: skillpack.Skill{ID: "nested-skill", Name: "nested-skill", Body: "body\n"},
		},
		{
			name: "single quoted description",
			id:   "quoted-skill",
			raw:  "---\nname: quoted-skill\ndescription: 'it''s quoted'\n---\nbody\n",
			want: skillpack.Skill{ID: "quoted-skill", Name: "quoted-skill", Description: "it's quoted", Body: "body\n"},
		},
		{
			name: "plain unquoted description",
			id:   "plain-desc",
			raw:  "---\nname: plain-desc\ndescription: no quotes here\n---\nbody\n",
			want: skillpack.Skill{ID: "plain-desc", Name: "plain-desc", Description: "no quotes here", Body: "body\n"},
		},
		{
			name:    "unterminated frontmatter",
			id:      "broken-skill",
			raw:     "---\nname: broken-skill\ndescription: \"never closed\"\n# body\n",
			wantErr: true,
		},
		{
			name:    "non kebab id",
			id:      "Gortex_Explore",
			raw:     "---\nname: x\n---\nbody\n",
			wantErr: true,
		},
		{
			name:    "empty id",
			id:      "",
			raw:     "body\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := skillpack.Parse(tt.id, tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error", tt.id, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.id, err)
			}
			if got != tt.want {
				t.Errorf("Parse(%q)\n got %#v\nwant %#v", tt.id, got, tt.want)
			}
		})
	}
}

func TestRenderFrontmatter(t *testing.T) {
	tests := []struct {
		name  string
		pairs [][2]string
		want  string
	}{
		{
			name:  "quotes prose, leaves bare tokens alone",
			pairs: [][2]string{{"name", "gortex-explore"}, {"description", "Understand how code works."}},
			want:  "---\nname: gortex-explore\ndescription: \"Understand how code works.\"\n---\n",
		},
		{
			name:  "colon forces quoting",
			pairs: [][2]string{{"description", "Gortex reference: tools"}},
			want:  "---\ndescription: \"Gortex reference: tools\"\n---\n",
		},
		{
			name:  "caller order is preserved",
			pairs: [][2]string{{"version", "1.0.0"}, {"name", "x-y"}},
			want:  "---\nversion: 1.0.0\nname: x-y\n---\n",
		},
		{
			name:  "escapes quotes and backslashes",
			pairs: [][2]string{{"description", `say "hi" \ bye`}},
			want:  "---\ndescription: \"say \\\"hi\\\" \\\\ bye\"\n---\n",
		},
		{
			// An empty or bool-ish value must never go out bare: YAML
			// would read it as null / true and the host would drop it.
			name:  "empty and bool-ish values are quoted",
			pairs: [][2]string{{"description", ""}, {"enabled", "true"}},
			want:  "---\ndescription: \"\"\nenabled: \"true\"\n---\n",
		},
		{
			name:  "no pairs renders nothing",
			pairs: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillpack.RenderFrontmatter(tt.pairs); got != tt.want {
				t.Errorf("RenderFrontmatter()\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestQuoteYAMLValueRoundTrip checks the two halves of the frontmatter
// codec agree: whatever QuoteYAMLValue emits, Parse must read back
// unchanged. Hosts rely on that to re-render a description without
// mutating it.
func TestQuoteYAMLValueRoundTrip(t *testing.T) {
	values := []string{
		"Understand how code works.",
		"Gortex reference: available tools, graph schema, and workflow.",
		"Clear LSP diagnostics — one error, a file, or source.fixAll.",
		"Preview an edit's blast radius on the shadow graph before writing.",
		`a "quoted" word`,
		`a \ backslash`,
		"gortex-explore",
		"",
	}
	for _, want := range values {
		raw := "---\nname: round-trip\ndescription: " + skillpack.QuoteYAMLValue(want) + "\n---\nbody\n"
		got, err := skillpack.Parse("round-trip", raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got.Description != want {
			t.Errorf("round trip of %q gave %q (rendered %s)", want, got.Description, skillpack.QuoteYAMLValue(want))
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
