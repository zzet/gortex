package main

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/platform"
)

// embeddedIndexPlan is what the embedded MCP server should serve when no
// daemon is available.
type embeddedIndexPlan struct {
	// Index is the repository root to index, or "" to serve an empty
	// graph. Empty is a real answer, not a failure: an ungated index of
	// a directory the user never asked us to index is worse than no
	// graph, because it silently answers with the wrong corpus.
	Index string
	// Notebook is the notebook store root. Repo-local (so entries travel
	// with the repo and surface in PR reviews) only when the index root
	// was the user's explicit choice; otherwise the machine-global cache,
	// the same location the daemon uses.
	Notebook string
	// Inferred is true when Index was guessed from the launch cwd rather
	// than passed as --index.
	Inferred bool
	// Refusal, when non-empty, explains why no index was chosen. Surfaced
	// to the operator on stderr and to the MCP client in the initialize
	// instructions — stderr alone is invisible, since MCP clients
	// routinely discard it.
	Refusal string
}

// resolveEmbeddedIndex decides what the embedded fallback serves, and
// where it is allowed to write.
//
// The embedded server has no tracked-repo gate: it indexes one tree and
// serves the whole graph. That is right for `--index <repo>`, where the
// user named the tree. It is wrong for a guessed cwd, which is how the
// fallback ran in practice — a daemon blip is enough to reach it, and the
// cwd is whatever directory the MCP client happened to launch from. Two
// consequences made that guess costly rather than merely unhelpful: the
// server indexed the entire tree under that directory with none of the
// per-repo excludes the workspace would have applied, and it created a
// `.gortex/` directory there for the repo-local notebook — which showed
// up as an untracked directory in the user's `git status`.
//
// Resolution order:
//
//  1. An explicit --index always wins, verbatim. The user named it.
//  2. A cwd that is unsafe to index at all (filesystem root, drive root,
//     home directory) yields no index. This replaces the ad-hoc
//     ambiguous-cwd check with the shared safety gate every other index
//     entry point uses, which also covers Windows drive roots.
//  3. A cwd that CONTAINS tracked repos but is not itself one is the
//     workspace-root shape: the user's repos are the corpus, not the
//     folder above them. Indexing the parent would build a second,
//     unscoped copy of every child. Yields no index.
//  4. Otherwise the cwd is the index — the case the fallback exists for,
//     where a user with no daemon opens their project directly.
//
// The notebook goes repo-local only under (1). An inferred root is a
// guess, and a guess must not leave files behind in the user's tree.
func resolveEmbeddedIndex(flagIndex, cwd string, global *config.GlobalConfig) embeddedIndexPlan {
	cacheNotebook := filepath.Join(platform.DataDir(), "notebook-cache")

	if flagIndex != "" {
		root := pathkey.CanonicalExistingRoot(flagIndex)
		return embeddedIndexPlan{Index: root, Notebook: root}
	}
	if cwd == "" {
		return embeddedIndexPlan{
			Notebook: cacheNotebook,
			Refusal:  "no working directory available to index",
		}
	}
	if reason := indexer.UnsafeIndexRootReason(cwd); reason != "" {
		return embeddedIndexPlan{Notebook: cacheNotebook, Refusal: reason}
	}
	if contained := trackedReposUnder(cwd, global); len(contained) > 0 {
		return embeddedIndexPlan{
			Notebook: cacheNotebook,
			Refusal: "refusing to index " + cwd + " — it contains tracked repositories (" +
				joinShort(contained) + "); start the daemon to query them instead of indexing their parent",
		}
	}
	return embeddedIndexPlan{Index: cwd, Notebook: cacheNotebook, Inferred: true}
}

// embeddedModeNote renders the operating-mode warning the embedded
// server delivers in its initialize instructions. It states the two
// things an agent cannot otherwise infer from the results: that no
// daemon is backing this session, and what corpus it is actually
// answering from.
func embeddedModeNote(plan embeddedIndexPlan) string {
	const lead = "DEGRADED: no gortex daemon is reachable, so this session is served by an embedded, " +
		"single-tree server. Results are NOT workspace-scoped and cover only the tree named below. " +
		"Start the daemon (`gortex daemon start`) for the full multi-repo graph."
	switch {
	case plan.Refusal != "":
		return lead + "\nIndexed tree: none — " + plan.Refusal + ". Every graph query will return empty."
	case plan.Index == "":
		return lead + "\nIndexed tree: none. Every graph query will return empty."
	default:
		return lead + "\nIndexed tree: " + plan.Index + "."
	}
}

// trackedReposUnder returns the tracked repo paths rooted under dir,
// excluding dir itself — the reverse-containment test the daemon's
// session gate applies, evaluated here against the on-disk config
// because the embedded path has no daemon to ask.
func trackedReposUnder(dir string, global *config.GlobalConfig) []string {
	if global == nil {
		return nil
	}
	var out []string
	for _, entry := range global.Repos {
		root := entry.Path
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if pathkey.SamePathIdentity(abs, dir) {
			// dir IS a tracked repo — index it, don't refuse it.
			return nil
		}
		if pathkey.HasPathPrefix(abs, dir) {
			out = append(out, abs)
		}
	}
	return out
}

// joinShort renders repo paths for a one-line diagnostic, naming at most
// three so a workspace with dozens does not produce an unreadable error.
func joinShort(paths []string) string {
	const max = 3
	out := ""
	for i, p := range paths {
		if i == max {
			out += ", …"
			break
		}
		if i > 0 {
			out += ", "
		}
		out += filepath.Base(p)
	}
	return out
}
