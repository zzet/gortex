package audit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultConfigPaths returns filenames and directories probed by default
// when no explicit file list is passed. Directory entries are expanded
// to any `*.md`/`*.mdc`/`*.txt` files they contain.
func DefaultConfigPaths() []string {
	return []string{
		"CLAUDE.md",
		"CLAUDE.local.md",
		"AGENTS.md",
		"GEMINI.md",
		".cursorrules",
		".cursor/rules",
		".github/copilot-instructions.md",
		// Path-scoped Copilot instructions. The CLI reads every
		// *.instructions.md under here in addition to the single
		// copilot-instructions.md above, so auditing only the latter
		// misses whatever a repo scoped to its TypeScript or Go files.
		".github/instructions",
		".windsurfrules",
		".windsurf/rules",
		".antigravity/rules",
		".agents/rules",
		".aider.conf.yml",
		// Skill trees. These carry the same stale symbol refs and dead
		// paths as a rules file, and Gortex now writes into them, so a
		// drift audit that skips them under-reports its own output.
		// `.agents/skills` is the cross-agent location Codex and
		// OpenCode both read.
		".agents/skills",
		".opencode/skills",
		".opencode/commands",
		".github/skills",
	}
}

// DiscoverConfigFiles walks the default probe locations under root and
// returns the existing config files (relative to root).
func DiscoverConfigFiles(root string) []string {
	var out []string
	for _, entry := range DefaultConfigPaths() {
		abs := entry
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, entry)
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			out = append(out, entry)
			continue
		}
		// Directory: collect supported config extensions.
		_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			switch ext {
			case ".md", ".mdc", ".txt", ".rules":
			default:
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				rel = p
			}
			// Discovered paths are slash-separated: the entries above are
			// slash-separated literals, and callers match them against that
			// list and report them to agents. filepath.Rel hands back the
			// native spelling, so a Windows walk would otherwise emit
			// ".cursor\rules\foo.md" beside ".github/copilot-instructions.md".
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
	}

	sort.Strings(out)
	return uniqueStrings(out)
}

func uniqueStrings(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
