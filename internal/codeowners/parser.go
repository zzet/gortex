// Package codeowners parses GitHub-style CODEOWNERS files into a
// rule list, applies last-match-wins matching, and emits team and
// person nodes plus EdgeOwns edges so blast-radius queries can
// surface "who needs to know" alongside the changed files.
//
// CODEOWNERS file syntax follows GitHub's documented format:
// gitignore-style patterns, one rule per non-comment line, owners
// listed after the pattern as @-prefixed handles. The last matching
// rule wins.
package codeowners

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
)

// Rule is one parsed CODEOWNERS line. Pattern is the gitignore-style
// glob; Owners is the list of @-handles or email addresses.
type Rule struct {
	Pattern string
	Owners  []string
	matcher *excludes.Matcher
}

// matchPattern returns the rule's gitignore matcher. Parse precompiles
// it, so for any Parse-built Rule the field is non-nil and MatchFile's
// concurrent hot path only reads it — no data race on a shared rule list
// (applyCoverageDomains matches files across goroutines against one
// list). For a Rule hand-constructed outside Parse the field is nil;
// compile a throwaway matcher rather than caching into r.matcher, so
// concurrent callers still can't race on the field.
//
// Compilation goes through excludes.New rather than go-gitignore
// directly: the raw library splices pattern text into a regexp with
// almost nothing escaped, so an owner rule as ordinary as "*$" would
// claim every file in the repository (the same defect that emptied a
// whole index in #624).
func (r *Rule) matchPattern() *excludes.Matcher {
	if r.matcher != nil {
		return r.matcher
	}
	return excludes.New([]string{r.Pattern})
}

// Parse reads a CODEOWNERS file's bytes and returns the rule list in
// document order. Comment lines (#…) and blank lines are skipped.
// Lines without owners are kept as Rules with an empty Owners list —
// they are valid CODEOWNERS syntax (a way to "blank out" an earlier
// rule for matched paths) and downstream consumers can decide
// whether to ignore them.
func Parse(source []byte) []Rule {
	if len(source) == 0 {
		return nil
	}
	var rules []Rule
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Drop trailing inline comments. CODEOWNERS supports them
		// (GitHub treats anything after a "#" the same as gitignore
		// does), so honour the comment delimiter even mid-line.
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		rule := Rule{Pattern: fields[0]}
		// Precompile the matcher in this single-goroutine parse so the
		// concurrent MatchFile hot path only reads rule.matcher.
		rule.matcher = excludes.New([]string{rule.Pattern})
		if len(fields) > 1 {
			rule.Owners = append(rule.Owners, fields[1:]...)
		}
		rules = append(rules, rule)
	}
	return rules
}

// MatchFile applies the last-match-wins rule against path and
// returns the matching owner list. Returns nil if no rule matched
// or if the matching rule has no owners. path should be relative
// to the repo root with forward-slash separators.
func MatchFile(path string, rules []Rule) []string {
	path = filepath.ToSlash(path)
	for i := len(rules) - 1; i >= 0; i-- {
		r := &rules[i]
		if r.matchPattern().MatchRel(path) {
			if len(r.Owners) == 0 {
				return nil
			}
			out := make([]string, len(r.Owners))
			copy(out, r.Owners)
			return out
		}
	}
	return nil
}

// LoadFromRepo locates and parses the first CODEOWNERS file found in
// the standard locations relative to repoRoot. Returns nil rules
// and ok=false when no file exists. Locations checked in order:
// .github/CODEOWNERS, CODEOWNERS, docs/CODEOWNERS — matching
// GitHub's resolution.
func LoadFromRepo(repoRoot string) (rules []Rule, sourcePath string, ok bool) {
	for _, rel := range []string{
		".github/CODEOWNERS",
		"CODEOWNERS",
		"docs/CODEOWNERS",
	} {
		full := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		return Parse(data), rel, true
	}
	return nil, "", false
}

// BuildGraphArtifacts produces a team node and EdgeOwns edge for
// each owner in owners. The team node ID convention is `team::<name>`
// (the leading "@" or trailing email-domain is preserved verbatim
// inside the node name and meta — the prefix exists only to keep
// IDs unique against future "person::" or "org::" namespaces).
//
// Team nodes are shared across files in the repo; graph.AddNode is
// idempotent on ID so re-emitting per file is cheap. The Meta.kind
// disambiguates teams from individuals: an owner containing "/" or
// matching "@org/team" is a team; everything else (a bare @user or
// an email) is a person.
//
// filePath is the unprefixed path; applyRepoPrefix downstream
// handles multi-repo namespacing. Its spelling is preserved verbatim —
// the edge endpoint must match the extractor's file-node ID spelling
// (OS-native separators on Windows) or the edge dangles.
func BuildGraphArtifacts(filePath string, owners []string, language string) ([]*graph.Node, []*graph.Edge) {
	if len(owners) == 0 {
		return nil, nil
	}
	nodes := make([]*graph.Node, 0, len(owners))
	edges := make([]*graph.Edge, 0, len(owners))
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		nodes = append(nodes, &graph.Node{
			ID:       TeamNodeID(owner),
			Kind:     graph.KindTeam,
			Name:     owner,
			FilePath: filePath, // first sighting; not authoritative
			Language: language,
			Meta: map[string]any{
				"owner": owner,
				"kind":  classifyOwner(owner),
			},
		})
		edges = append(edges, &graph.Edge{
			From:     TeamNodeID(owner),
			To:       filePath,
			Kind:     graph.EdgeOwns,
			FilePath: filePath,
			Origin:   graph.OriginASTResolved,
		})
	}
	return nodes, edges
}

// TeamNodeID returns the canonical ID for an owner node. We strip
// the leading "@" so `@core` and `core` produce the same ID — the
// "@" is purely CODEOWNERS syntax, not part of the team identity.
// Repo-scoped via applyRepoPrefix in multi-repo mode (same as
// annotation:: nodes — see scanner.go in internal/licenses for
// notes on cross-repo de-dup as a v2 concern).
func TeamNodeID(owner string) string {
	owner = strings.TrimPrefix(strings.TrimSpace(owner), "@")
	return "team::" + owner
}

// classifyOwner returns "team" or "person". GitHub teams take the
// form "@org/team" (one slash); GitLab uses "@group/subgroup/team".
// Either form contains a slash; bare @users do not. Email addresses
// are people. Everything else defaults to person — false positives
// are recoverable by the user via .gortex.yaml override (out of
// scope for v1).
func classifyOwner(owner string) string {
	owner = strings.TrimPrefix(owner, "@")
	switch {
	case strings.Contains(owner, "/"):
		return "team"
	case strings.Contains(owner, "@"):
		return "person"
	default:
		return "person"
	}
}
