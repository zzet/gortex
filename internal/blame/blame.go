// Package blame populates per-node authorship metadata by shelling
// out to `git blame -p`. The output is parsed line-by-line and
// projected onto the existing graph: each function/method/type/
// interface node receives meta.last_authored = {commit, email,
// timestamp} from the most recent author touching any line in the
// node's range.
//
// Design choices:
//
//   - We invoke git directly rather than depending on go-git. The
//     CLI is universally available alongside any indexed repo, the
//     porcelain output format is stable and documented, and avoiding
//     the go-git dependency keeps the binary small and the dataset
//     parser fast.
//
//   - Per-file rather than per-symbol blame. `git blame -p <file>`
//     is dominated by repository walk cost; running it per symbol
//     would multiply that cost by ~30x on a typical Go file. One
//     pass per file with line-to-node projection is the right
//     amortised shape.
//
//   - "Most recent author" wins — when a node spans multiple commits,
//     we pick the latest by timestamp. The alternative (most-common
//     author) is more useful for ownership analytics but doesn't
//     answer the agent question we care about ("who touched this
//     last").
package blame

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
)

// Author is one author record extracted from blame.
type Author struct {
	Commit    string    // 40-char SHA, or shortened by caller via meta
	Email     string    // committer email — "<>" stripped
	Timestamp time.Time // author-time
}

// Run executes `git blame -p` on the file at the current worktree
// (HEAD) and returns a map from 1-based line number to Author. errors
// include both git invocation failures (file not in repo, repo not
// initialised) and parse failures. Callers may treat any error as
// "skip this file" — the enrichment pass is best-effort.
func Run(repoRoot, relPath string) (map[int]Author, error) {
	return RunAt(repoRoot, "", relPath)
}

// RunAt is Run with an explicit revision (branch / tag / SHA). Pass
// "" for HEAD. Used by enrichments that must blame the default branch
// regardless of the user's current checkout — e.g. the churn enricher
// pinning to `origin/main` so feature-branch work-in-progress doesn't
// pollute the persisted data.
func RunAt(repoRoot, rev, relPath string) (map[int]Author, error) {
	if err := gitcmd.ValidateRef(rev); err != nil {
		return nil, err
	}
	args := []string{"blame", "-p"}
	if rev != "" {
		args = append(args, rev)
	}
	args = append(args, "--", relPath)
	out, err := gitcmd.Run(context.Background(), repoRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("git blame %s: %w", relPath, err)
	}
	return Parse(out)
}

// Parse converts `git blame -p` porcelain output into a per-line
// Author map. Exposed for tests; production callers go through Run.
//
// Porcelain structure (one block per line, blocks separated by a
// header beginning with the commit hash):
//
//	<commit-sha> <orig-line> <final-line> [<num-lines>]
//	author <name>
//	author-mail <email>
//	author-time <unix-secs>
//	... other header lines ...
//	<TAB><source-line>
//
// The initial block carries the full header set; subsequent blocks
// for the same commit reuse the cached entries via the bare commit
// SHA on the header line. Both forms are handled.
func Parse(out []byte) (map[int]Author, error) {
	commits := make(map[string]Author)
	result := make(map[int]Author)

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var currentCommit, pendingMail string
	var pendingTime int64
	var currentLine int

	for scanner.Scan() {
		line := scanner.Text()

		// A header line looks like "<sha> <orig> <final> [count]"
		// — 40 hex chars + spaces + decimals. Tab-prefixed lines
		// are the actual source. Other lines are header k/v pairs.
		if isHeaderLine(line) {
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			currentCommit = parts[0]
			finalLine, err := strconv.Atoi(parts[2])
			if err != nil {
				continue
			}
			currentLine = finalLine
			pendingMail = ""
			pendingTime = 0
			continue
		}

		switch {
		case strings.HasPrefix(line, "\t"):
			// Source line — emit the current commit's Author for
			// currentLine. If we have header data buffered for this
			// block, finalise the cached entry first.
			if pendingMail != "" || pendingTime != 0 {
				commits[currentCommit] = Author{
					Commit:    currentCommit,
					Email:     pendingMail,
					Timestamp: time.Unix(pendingTime, 0),
				}
			}
			if author, ok := commits[currentCommit]; ok {
				result[currentLine] = author
			}
			pendingMail = ""
			pendingTime = 0
		case strings.HasPrefix(line, "author-mail "):
			pendingMail = strings.TrimSpace(strings.TrimPrefix(line, "author-mail "))
			pendingMail = strings.Trim(pendingMail, "<>")
		case strings.HasPrefix(line, "author-time "):
			ts, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "author-time ")), 10, 64)
			if err == nil {
				pendingTime = ts
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// isHeaderLine reports whether a porcelain line begins with a 40-
// character hex SHA followed by a space. Subsequent header lines
// (author-mail, author-time, etc.) are explicit prefixes; the SHA
// header is the only one we need to detect by shape.
func isHeaderLine(line string) bool {
	if len(line) < 41 {
		return false
	}
	if line[40] != ' ' {
		return false
	}
	for i := 0; i < 40; i++ {
		c := line[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// EnrichGraph walks every node with a known file/range and stamps
// meta.last_authored from the most recent author touching the
// node's line range. Returns the number of nodes enriched. Errors
// from individual files are ignored — best-effort enrichment is
// preferable to all-or-nothing.
//
// repoRoot is the absolute path to the git repository root; the
// caller is responsible for resolving multi-repo node paths back
// to per-repo roots before invoking this function.
//
// Already-enriched nodes are re-evaluated — repeated runs converge
// on the latest blame data. The kind filter excludes file/import/
// package nodes (no symbol-level authorship signal there) and the
// synthetic kinds (todo, license, team, …) which by construction
// have no per-line author.

// PersonNodeID returns the canonical KindTeam node ID for a blame
// author email. Mirrors codeowners.TeamNodeID but keys on email so a
// person who is not yet a CODEOWNERS owner still gets a stable ID.
// Repo-scoping (the leading "<prefix>/" in multi-repo mode) is
// applied by the caller, since blame.EnrichGraph reads RepoPrefix
// off existing graph nodes.
func PersonNodeID(email string) string {
	return "team::" + strings.ToLower(strings.TrimSpace(email))
}

func EnrichGraph(g graph.Store, repoRoot string) (int, error) {
	if g == nil || repoRoot == "" {
		return 0, nil
	}
	// Group nodes by relative file path so we shell out to git
	// once per file — repo walk dominates blame cost. Path normalization
	// is likewise done once per distinct raw path: symbol-dense files can
	// contribute thousands of nodes, but their on-disk path is identical.
	byPath := make(map[string][]*graph.Node)
	normalizedPaths := make(map[string]string)
	_, gitErr := exec.LookPath("git")
	gitAvailable := gitErr == nil
	for _, n := range g.AllNodes() {
		if !Eligible(n) {
			continue
		}
		path, ok := normalizedPaths[n.FilePath]
		if !ok {
			path = n.FilePath
			if gitAvailable {
				// Strip multi-repo prefix when present — the prefix is the
				// repo name, not part of the on-disk path. The caller
				// passes a per-repo root, so the file path we pass to git
				// must be repo-relative.
				path = stripRepoPrefix(n.FilePath, repoRoot)
			}
			normalizedPaths[n.FilePath] = path
		}
		byPath[path] = append(byPath[path], n)
	}

	enriched := 0
	// Symbol nodes we stamp meta.last_authored on. They must be
	// round-tripped back through the store at the end: on the in-memory
	// backend the in-place mutation already persists (n is canonical),
	// but on disk backends (SQLite) n is a per-call AllNodes
	// reconstruction, so without the write-back the last_authored stamp
	// is silently discarded — leaving stale_code / ownership /
	// health_score's recency axis empty on the disk backend even after
	// a successful `gortex enrich blame`. (The person nodes and
	// EdgeAuthored edges below already persist via AddNode/AddEdge; only
	// the symbol-node Meta was being dropped.) Mirrors the reach index,
	// coverage, and releases enrichers.
	var stamped []*graph.Node
	blameWriter, useBlameSidecar := g.(graph.BlameEnrichmentWriter)
	var blameRows []graph.BlameEnrichment
	// Person nodes are deduplicated within this enrichment pass.
	// IDs are repo-scoped: in multi-repo mode the same email touching
	// two repos becomes two distinct KindTeam nodes so per-repo
	// queries stay scoped. The dedup key matches the final node ID.
	personNodes := make(map[string]*graph.Node)
	for path, nodes := range byPath {
		lines, err := Run(repoRoot, path)
		if err != nil || len(lines) == 0 {
			continue
		}
		for _, n := range nodes {
			latest := pickLatest(lines, n.StartLine, n.EndLine)
			if latest == nil {
				continue
			}
			if useBlameSidecar {
				blameRows = append(blameRows, graph.BlameEnrichment{
					NodeID: n.ID, RepoPrefix: n.RepoPrefix,
					Commit: latest.Commit, Email: latest.Email,
					Timestamp: latest.Timestamp.Unix(),
				})
			} else {
				if n.Meta == nil {
					n.Meta = map[string]any{}
				}
				n.Meta["last_authored"] = map[string]any{
					"commit":    latest.Commit,
					"email":     latest.Email,
					"timestamp": latest.Timestamp.Unix(),
				}
				stamped = append(stamped, n)
			}
			enriched++

			if latest.Email == "" {
				continue
			}
			personID := PersonNodeID(latest.Email)
			if n.RepoPrefix != "" {
				personID = n.RepoPrefix + "/" + personID
			}
			if _, ok := personNodes[personID]; !ok {
				node := &graph.Node{
					ID:          personID,
					Kind:        graph.KindTeam,
					Name:        latest.Email,
					FilePath:    n.FilePath, // first sighting; not authoritative
					Language:    n.Language,
					RepoPrefix:  n.RepoPrefix,
					WorkspaceID: n.WorkspaceID,
					ProjectID:   n.ProjectID,
					Meta: map[string]any{
						"kind":  "person",
						"email": latest.Email,
					},
				}
				g.AddNode(node)
				personNodes[personID] = node
			}
			edge := &graph.Edge{
				From:     personID,
				To:       n.ID,
				Kind:     graph.EdgeAuthored,
				FilePath: n.FilePath,
				Line:     n.StartLine,
				Origin:   graph.OriginASTResolved,
				Meta: map[string]any{
					"commit":    latest.Commit,
					"timestamp": latest.Timestamp.Unix(),
				},
			}
			g.AddEdge(edge)
		}
	}
	// Persist the symbol-node last_authored stamps in one batch (the
	// durable write on disk backends; an idempotent re-insert on the
	// in-memory backend).
	if useBlameSidecar && len(blameRows) > 0 {
		byPrefix := map[string][]graph.BlameEnrichment{}
		for _, r := range blameRows {
			byPrefix[r.RepoPrefix] = append(byPrefix[r.RepoPrefix], r)
		}
		for prefix, rr := range byPrefix {
			if err := blameWriter.BulkSetBlame(prefix, rr); err != nil {
				return enriched, fmt.Errorf("blame: persist sidecar: %w", err)
			}
		}
	} else if len(stamped) > 0 {
		g.AddBatch(stamped, nil)
	}
	return enriched, nil
}

// pickLatest returns the most-recent Author touching any line in
// [startLine, endLine] inclusive. Nil when the range has no blame
// coverage (e.g. uncommitted lines or the file isn't tracked).
func pickLatest(lines map[int]Author, startLine, endLine int) *Author {
	if endLine < startLine {
		endLine = startLine
	}
	var best *Author
	for line := startLine; line <= endLine; line++ {
		a, ok := lines[line]
		if !ok {
			continue
		}
		if best == nil || a.Timestamp.After(best.Timestamp) {
			ac := a
			best = &ac
		}
	}
	return best
}

// shouldEnrichBlame keeps the enrichment focused on symbol-level
// nodes that benefit from authorship signal. File and import nodes
// are excluded — file-level blame is recoverable through any of
// the file's symbols. The coverage-introduced synthetic kinds
// (todo / license / team / flag / event / module / config_key /
// fixture) are also excluded since their "authorship" is the
// authorship of the underlying source line, which the agent can
// look up via the file/function the synthetic node attaches to.
// Eligible reports whether this pass will consider n at all: the right kind,
// and enough position information to blame. It is exported because a caller
// that reports blame COVERAGE has to count the same population this pass
// admits — counting symbols the pass never looks at would report a permanent
// shortfall that no enrichment could ever close.
//
// Eligibility is not a promise of a stamp. A file git cannot blame is skipped
// below, and a symbol whose lines carry no blame data is skipped too; both
// leave an eligible symbol unstamped, which is a real coverage hole rather
// than an ineligible symbol.
func Eligible(n *graph.Node) bool {
	if n == nil || n.FilePath == "" || n.StartLine == 0 {
		return false
	}
	return shouldEnrichBlame(n.Kind)
}

func shouldEnrichBlame(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindField, graph.KindVariable,
		graph.KindConstant, graph.KindEnumMember:
		return true
	}
	return false
}

// stripRepoPrefix removes a leading repo name segment from file
// paths produced by the multi-repo indexer. In single-repo mode
// the indexer emits paths like "internal/foo.go" directly; in
// multi-repo mode they look like "<reponame>/internal/foo.go".
// We strip the leading segment when the path doesn't exist on
// disk relative to repoRoot — the absence of the file under the
// untrimmed path is the signal that we're looking at a prefixed
// path.
func stripRepoPrefix(filePath, repoRoot string) string {
	if !strings.Contains(filePath, "/") {
		return filePath
	}
	// Try untrimmed first.
	abs := filepath.Join(repoRoot, filePath)
	if fileExists(abs) {
		return filePath
	}
	// Strip leading segment and retry.
	if idx := strings.Index(filePath, "/"); idx >= 0 {
		trimmed := filePath[idx+1:]
		if fileExists(filepath.Join(repoRoot, trimmed)) {
			return trimmed
		}
	}
	return filePath
}

// fileExists is split out so tests can stub it. os.Stat follows symlinks,
// and Mode().IsRegular matches the previous `test -f` semantics without
// spawning a process for every lookup.
var fileExists = func(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
