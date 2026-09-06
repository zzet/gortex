// Package skillpack is the host-neutral representation of a Gortex
// skill: an ID, a name, a description, and a markdown body with no
// frontmatter attached.
//
// Every agent host (Claude Code, Hermes, Codex, OpenCode, GitHub
// Copilot, …) ships the same 21 playbooks but demands its own
// frontmatter dialect — different keys, different nesting, different
// discovery metadata. Parsing one host's SKILL.md into this neutral
// shape and re-rendering it per host keeps the *content* single-sourced
// while each adapter owns only its own envelope.
//
// # Import direction: adapters -> skillpack, never the reverse
//
// This package MUST NOT import any adapter package
// (internal/agents/claudecode, internal/agents/hermes, …). The skill
// bodies stay where they are; callers pass the raw map in. The
// tempting inversion — skillpack reaching up for the bodies itself —
// deadlocks the moment an adapter wants to call back into skillpack
// (Sync, in sync.go, is exactly that), because Go rejects the import
// cycle. Keeping the arrow one-way means any number of adapters can
// depend on skillpack without ever constraining each other.
//
// This file imports only the stdlib. sync.go additionally imports
// internal/agents and internal/agents/internalutil for the shared file
// actions, writers and log prefixes; see the note there for why those
// two — and only those two — cannot close a cycle.
package skillpack

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Skill is one playbook, stripped of whatever host dialect it arrived
// in. Body carries the markdown only — a host renders its own
// frontmatter in front of it, so a Body that still began with "---"
// would produce two stacked frontmatter blocks and a skill most hosts
// silently ignore.
type Skill struct {
	ID          string // kebab-case identifier, e.g. "gortex-explore" — the map key and on-disk directory name
	Name        string // frontmatter `name` field, usually equal to ID
	Description string // frontmatter `description` field, unquoted and unescaped
	Body        string // markdown body, with no frontmatter and no leading "---"
}

// idPattern is the identifier shape every host accepts. OpenCode and
// GitHub Copilot both reject a skill whose name is not kebab-case
// (uppercase, spaces and underscores are refused at load time), and a
// rejected skill fails silently — it simply never appears in the
// picker. Validating at parse time turns that into a loud error in our
// own tests instead of a missing skill on a user's machine.
var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidID reports whether id is the kebab-case shape every supported
// host accepts as a skill name.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ErrUnterminatedFrontmatter is returned when a document opens a
// "---" frontmatter fence that is never closed. Guessing (treating the
// whole file as either frontmatter or body) would silently install a
// broken skill, so this is an error rather than a fallback.
var ErrUnterminatedFrontmatter = errors.New("unterminated frontmatter")

// Parse turns one SKILL.md into a Skill: it splits the leading YAML
// frontmatter off the markdown, and reads the `name` and `description`
// scalars out of it.
//
// A document with no frontmatter at all is not an error — the whole
// text becomes Body and the caller supplies name/description itself.
// Only a fence that opens and never closes is rejected.
func Parse(id, raw string) (Skill, error) {
	if !ValidID(id) {
		return Skill{}, fmt.Errorf("skillpack: invalid skill id %q: want kebab-case (%s)", id, idPattern)
	}
	front, body, err := splitFrontmatter(raw)
	if err != nil {
		return Skill{}, fmt.Errorf("skillpack: skill %q: %w", id, err)
	}
	return Skill{
		ID:          id,
		Name:        frontmatterField(front, "name"),
		Description: frontmatterField(front, "description"),
		Body:        body,
	}, nil
}

// ParseAll parses a whole id -> SKILL.md map into an ID-sorted slice.
// Sorting is what makes an install plan, a rendered artifact set and a
// golden test reproducible: Go map iteration order is randomised, so a
// caller ranging over the map directly would emit a different byte
// order on every run.
func ParseAll(raw map[string]string) ([]Skill, error) {
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]Skill, 0, len(ids))
	for _, id := range ids {
		skill, err := Parse(id, raw[id])
		if err != nil {
			return nil, err
		}
		out = append(out, skill)
	}
	return out, nil
}

// RenderFrontmatter emits a minimal YAML frontmatter block from ordered
// key/value pairs, quoting values that need it.
//
// Ordered pairs rather than a fixed struct because the hosts disagree
// about the envelope: Codex, OpenCode and Copilot all need `name` +
// `description`, but each adds its own keys (and expects them in its
// own order), so the shape belongs to the caller. Values are emitted as
// plain YAML scalars — a host needing structured YAML (nested maps,
// flow sequences) builds that block itself and uses QuoteYAMLValue for
// the scalar leaves.
//
// Returns "" for no pairs: an empty "---\n---\n" block is not
// something any host wants written to disk.
func RenderFrontmatter(pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fence + "\n")
	for _, kv := range pairs {
		b.WriteString(kv[0])
		b.WriteString(": ")
		b.WriteString(QuoteYAMLValue(kv[1]))
		b.WriteString("\n")
	}
	b.WriteString(fence + "\n")
	return b.String()
}

// QuoteYAMLValue renders s as a YAML scalar, double-quoting it unless
// it is a bare token that is unambiguous in every YAML context.
//
// The rule is deliberately conservative — anything with a space, a
// colon, a comment marker or any other punctuation gets quoted. Skill
// descriptions are prose ("Gortex reference: available tools, …") and
// an unquoted colon-space turns the line into a nested mapping, which
// makes the host drop the skill.
func QuoteYAMLValue(s string) string {
	if isBareToken(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// isBareToken reports whether s can be emitted unquoted. Only ASCII
// identifier-ish tokens qualify (`gortex-explore`, `1.0.0`), and never
// a token YAML would read as a bool/null — those are the values a host
// would silently reinterpret.
func isBareToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '/':
		default:
			return false
		}
	}
	if s[0] == '-' {
		return false // a leading dash reads as a sequence entry
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "y", "n", "~":
		return false
	}
	return true
}

// fence is the YAML frontmatter delimiter.
const fence = "---"

// splitFrontmatter separates a leading "---"-fenced YAML block from the
// markdown that follows. The body is returned byte-for-byte as it
// appeared after the closing fence — re-joining parsed lines would
// normalise line endings and silently rewrite every skill we install.
func splitFrontmatter(raw string) (front, body string, err error) {
	first, rest, ok := cutLine(raw)
	if !ok || !isFence(first) {
		return "", raw, nil
	}
	// consumed tracks the start of the line under inspection, so the
	// frontmatter slice can be cut without rebuilding it.
	consumed := 0
	for {
		line, tail, ok := cutLine(rest[consumed:])
		if !ok {
			return "", "", ErrUnterminatedFrontmatter
		}
		if isFence(line) {
			return rest[:consumed], tail, nil
		}
		consumed = len(rest) - len(tail)
	}
}

// isFence matches a delimiter line, tolerating the CR of a CRLF file.
// Skill sources authored on Windows (or round-tripped through a Windows
// editor) otherwise parse as "no frontmatter" and ship their own YAML
// as visible markdown.
func isFence(line string) bool { return strings.TrimRight(line, "\r") == fence }

// cutLine splits off the first line of s. ok is false only when s is
// empty, so a final line without a trailing newline is still returned.
func cutLine(s string) (line, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", true
}

// frontmatterField reads a single-line scalar out of a frontmatter
// block. Only top-level keys match: an indented `description:` belongs
// to a nested mapping (Hermes nests discovery metadata) and must not be
// mistaken for the skill's own description.
func frontmatterField(front, key string) string {
	prefix := key + ":"
	rest := front
	for {
		line, tail, ok := cutLine(rest)
		if !ok {
			return ""
		}
		if value, found := strings.CutPrefix(line, prefix); found {
			return unquoteYAMLValue(strings.TrimSpace(strings.TrimRight(value, "\r")))
		}
		rest = tail
	}
}

// unquoteYAMLValue decodes the quoted-scalar forms the skill sources
// actually use. It is not a YAML parser: block scalars, anchors and
// flow collections are returned verbatim, because no skill frontmatter
// uses them and a half-implemented parser is worse than none.
func unquoteYAMLValue(v string) string {
	if len(v) < 2 {
		return v
	}
	switch {
	case v[0] == '"' && v[len(v)-1] == '"':
		return unescapeDoubleQuoted(v[1 : len(v)-1])
	case v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

// unescapeDoubleQuoted reverses the escapes QuoteYAMLValue emits. An
// escape it does not recognise is kept verbatim (backslash included)
// rather than dropped, so an unexpected sequence survives a
// parse/render round trip instead of quietly losing a character.
func unescapeDoubleQuoted(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
