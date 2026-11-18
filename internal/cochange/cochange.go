// Package cochange mines git history for files that change together
// and projects the result onto the graph as EdgeCoChange edges.
//
// Co-change ("logical coupling") is the relationship git history
// reveals but the import graph cannot: a handler and its test, a
// struct and the serializer that mirrors it, a schema and its
// migration. Two files repeatedly committed together are coupled even
// when neither imports the other.
//
// Design mirrors internal/blame: shell out to `git log` (universally
// available, stable porcelain), best-effort (any git error yields an
// empty result rather than failing), file-level granularity (git
// tracks files, not symbols).
package cochange

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/platform"
)

// Tuning defaults for a history scan.
const (
	DefaultMaxCommits        = 1500
	DefaultMaxFilesPerCommit = 40
	DefaultMinCount          = 2
)

// Options tunes the history scan. The zero value is valid — every
// field falls back to its Default* constant.
type Options struct {
	// MaxCommits caps how far back the scan walks.
	MaxCommits int
	// MaxFilesPerCommit drops a commit from pair counting when it
	// touches more files than this. Sweeping refactors, generated-file
	// regens, and vendor bumps couple everything with everything and
	// only add noise.
	MaxFilesPerCommit int
	// MinCount drops file pairs that co-changed fewer than this many
	// times — a single shared commit is not evidence of coupling.
	MinCount int
}

func (o Options) withDefaults() Options {
	if o.MaxCommits <= 0 {
		o.MaxCommits = DefaultMaxCommits
	}
	if o.MaxFilesPerCommit <= 0 {
		o.MaxFilesPerCommit = DefaultMaxFilesPerCommit
	}
	if o.MinCount <= 0 {
		o.MinCount = DefaultMinCount
	}
	return o
}

// Pair is one co-change relationship between two files. FileA and
// FileB are repo-relative paths, ordered lexically so the pair has a
// canonical form.
type Pair struct {
	FileA string  `json:"file_a"`
	FileB string  `json:"file_b"`
	Count int     `json:"count"` // commits touching both files
	Score float64 `json:"score"` // association strength, 0..1
}

// Result bundles a single Mine pass.
type Result struct {
	// Pairs is sorted by Score descending, then by file path.
	Pairs []Pair
	// FileChanges is the per-file commit-touch count over the scan.
	FileChanges map[string]int
	// CommitsScanned counts non-empty commits the scan walked.
	CommitsScanned int
}

// Mine runs `git log` in root and computes file-level co-change
// associations. Returns an empty Result (never an error) when root is
// not a git repository or git is unavailable — co-change is a
// best-effort enrichment.
func Mine(ctx context.Context, root string, opts Options) Result {
	res, _ := MineContext(ctx, root, opts)
	return res
}

// MineContext retains cancellation and Git failures instead of making an
// interrupted mine look like an empty successful history. No partial result
// is returned on error. Mine keeps the historical best-effort contract.
func MineContext(ctx context.Context, root string, opts Options) (Result, error) {
	opts = opts.withDefaults()
	empty := Result{FileChanges: map[string]int{}}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if root == "" {
		return empty, nil
	}
	// %x00 emits a NUL before each commit record; --name-only appends
	// the changed file list. The format body is empty — we only need
	// the file lists, grouped per commit by the NUL separator.
	cmd := exec.CommandContext(ctx, "git", "-C", root, "log", "--no-merges", //nolint:gosec // root is daemon-internal
		"-n", strconv.Itoa(opts.MaxCommits), "--name-only", "--format=%x00")
	platform.ConfigureBackgroundCommand(cmd)
	out, err := mineCommandOutput(ctx, cmd)
	if err != nil {
		return empty, fmt.Errorf("cochange git log: %w", err)
	}

	pairCounts := map[[2]string]int{}
	fileChanges := map[string]int{}
	commitsScanned := 0

	for _, chunk := range strings.Split(string(out), "\x00") {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		files := parseCommitFiles(chunk)
		if len(files) == 0 {
			continue
		}
		commitsScanned++
		// Per-file totals count every commit touching the file —
		// including solo and sweeping commits — so the association
		// denominator honestly reflects how often the file moves.
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return empty, err
			}
			fileChanges[f]++
		}
		// Pair counting skips solo commits (no pair) and sweeping
		// commits (noise). The remaining commits are genuine
		// co-change evidence.
		if len(files) < 2 || len(files) > opts.MaxFilesPerCommit {
			continue
		}
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				if err := ctx.Err(); err != nil {
					return empty, err
				}
				pairCounts[orderedPair(files[i], files[j])]++
			}
		}
	}

	pairs := make([]Pair, 0, len(pairCounts))
	for key, cnt := range pairCounts {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if cnt < opts.MinCount {
			continue
		}
		a, b := key[0], key[1]
		// Cosine association: count / sqrt(touchesA * touchesB).
		// Symmetric, in (0,1]; reaches 1 only when the two files
		// never move apart.
		score := 0.0
		if ca, cb := fileChanges[a], fileChanges[b]; ca > 0 && cb > 0 {
			score = float64(cnt) / math.Sqrt(float64(ca)*float64(cb))
		}
		if score > 1 {
			score = 1
		}
		pairs = append(pairs, Pair{FileA: a, FileB: b, Count: cnt, Score: score})
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	// The loaded sort is not interruptible; ownership includes its real return.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score > pairs[j].Score
		}
		if pairs[i].FileA != pairs[j].FileA {
			return pairs[i].FileA < pairs[j].FileA
		}
		return pairs[i].FileB < pairs[j].FileB
	})

	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return Result{Pairs: pairs, FileChanges: fileChanges, CommitsScanned: commitsScanned}, nil
}

// mineCommandOutput waits for the exact exec-owned direct child, including cancellation
// and pipe-copy completion. Keep both context and exit/wait errors; a kill is
// not successful mining. This does not claim arbitrary descendant containment.
func mineCommandOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(ctxErr, err)
	}
	return out, err
}

// parseCommitFiles extracts the changed-file list from one NUL-
// delimited `git log --name-only` chunk. Every non-empty line in the
// chunk is a file path (the commit format body is empty).
func parseCommitFiles(chunk string) []string {
	chunk = strings.Trim(chunk, "\n")
	if chunk == "" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(chunk, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// orderedPair returns the two paths as a lexically-ordered array so a
// pair has one canonical map key regardless of commit order.
func orderedPair(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// EnrichGraph mines co-change for the repository rooted at root and
// adds symmetric EdgeCoChange edges between the matching KindFile
// nodes. Returns the number of directed edges added.
//
// repoPrefix scopes the file-node match in multi-repo graphs: when
// set, only file nodes carrying that RepoPrefix are considered and
// the git-relative paths are matched against the prefix-stripped node
// path. Pass "" for a single-repo graph.
//
// Best-effort: returns (0, nil) when root is not a git repository.
// Idempotent — stores dedupe exact edge identities, so repeated runs converge.
func EnrichGraph(g graph.Store, root, repoPrefix string) (int, error) {
	return EnrichGraphWith(g, root, repoPrefix, Options{})
}

// EnrichGraphWith is EnrichGraph with explicit scan tuning.
func EnrichGraphWith(g graph.Store, root, repoPrefix string, opts Options) (int, error) {
	if g == nil || root == "" {
		return 0, nil
	}
	res := Mine(context.Background(), root, opts)
	return AddEdges(g, res.Pairs, repoPrefix), nil
}

// EnrichGraphContext is the cancellation/error-aware sibling of EnrichGraph.
// Completed earlier batches remain durable on cancellation; no subsequent
// batch is admitted, and the error must not be reported as completed enrichment.
func EnrichGraphContext(ctx context.Context, g graph.Store, root, repoPrefix string) (int, error) {
	return EnrichGraphWithContext(ctx, g, root, repoPrefix, Options{})
}

// EnrichGraphWithContext is EnrichGraphContext with explicit scan tuning.
func EnrichGraphWithContext(ctx context.Context, g graph.Store, root, repoPrefix string, opts Options) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if g == nil || root == "" {
		return 0, nil
	}
	res, err := MineContext(ctx, root, opts)
	if err != nil {
		return 0, err
	}
	return AddEdgesContext(ctx, g, res.Pairs, repoPrefix)
}

// coChangeEdgeBatchSize bounds the temporary edge set while filling one large
// SQLite JSONB ingest payload per store call. Keeping this package-owned avoids
// coupling co-change semantics to a specific backend; Graph.AddBatch uses the
// same bounded slices without special handling.
const coChangeEdgeBatchSize = 8 * 1024

// AddEdges projects already-mined co-change pairs onto the graph as
// symmetric EdgeCoChange edges between the matching KindFile nodes,
// returning the number of directed edges added.
//
// repoPrefix scopes the file-node match: when set, only file nodes
// carrying that RepoPrefix are matched, against the prefix-stripped
// node path (the pairs hold git-relative paths). Pass "" for a
// single-repo graph. Idempotent — graph.AddBatch dedupes.
func AddEdges(g graph.Store, pairs []Pair, repoPrefix string) int {
	added, _ := addEdges(context.Background(), g, pairs, repoPrefix, func(edges []*graph.Edge) error {
		g.AddBatch(nil, edges)
		return nil
	})
	return added
}

// AddEdgesContext returns directed edges submitted by completed batches (the
// same count as AddEdges on success, including deduplicated repeats). Errors
// retain already completed batches; pending rows are never counted as written.
// Legacy store calls themselves remain joined, not physically interruptible.
func AddEdgesContext(ctx context.Context, g graph.Store, pairs []Pair, repoPrefix string) (int, error) {
	return addEdges(ctx, g, pairs, repoPrefix, func(edges []*graph.Edge) error {
		if checked, ok := g.(interface {
			AddBatchCheckedContext(context.Context, []*graph.Node, []*graph.Edge) error
		}); ok {
			return checked.AddBatchCheckedContext(ctx, nil, edges)
		}
		if checked, ok := g.(interface {
			AddBatchChecked([]*graph.Node, []*graph.Edge) error
		}); ok {
			return checked.AddBatchChecked(nil, edges)
		}
		g.AddBatch(nil, edges)
		return nil
	})
}

func addEdges(ctx context.Context, g graph.Store, pairs []Pair, repoPrefix string, write func([]*graph.Edge) error) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if g == nil || len(pairs) == 0 {
		return 0, nil
	}
	// Map repo-relative path -> file node ID. Ask the store for file nodes
	// directly: the SQLite implementation pushes KindFile into its indexed
	// query and closes the read cursor before this function starts writing
	// co-change edges. Pulling AllNodes here decoded every symbol and its
	// metadata once per repository during lazy co-change mining.
	//
	// A file node's FilePath carries the multi-repo prefix; key on the
	// prefix-stripped path so a git-relative path matches.
	idByPath := make(map[string]string)
	for n := range graph.BindReadContext(g, ctx).NodesByKind(graph.KindFile) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if n == nil || n.Kind != graph.KindFile || n.FilePath == "" {
			continue
		}
		if repoPrefix != "" {
			if n.RepoPrefix != repoPrefix {
				continue
			}
			idByPath[strings.TrimPrefix(n.FilePath, repoPrefix+"/")] = n.ID
		} else {
			idByPath[n.FilePath] = n.ID
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	edgeBatch := make([]*graph.Edge, 0, coChangeEdgeBatchSize)
	added := 0
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(edgeBatch) == 0 {
			return nil
		}
		if err := write(edgeBatch); err != nil {
			return errors.Join(ctx.Err(), err)
		}
		added += len(edgeBatch)
		edgeBatch = edgeBatch[:0]
		return ctx.Err()
	}

	for _, p := range pairs {
		if err := ctx.Err(); err != nil {
			return added, err
		}
		idA, okA := idByPath[p.FileA]
		idB, okB := idByPath[p.FileB]
		if !okA || !okB || idA == idB {
			continue
		}
		edgeBatch = append(edgeBatch,
			coChangeEdge(idA, idB, p.FileA, p),
			coChangeEdge(idB, idA, p.FileB, p),
		)
		if len(edgeBatch) >= coChangeEdgeBatchSize {
			if err := flush(); err != nil {
				return added, err
			}
		}
	}
	err := flush()
	return added, err
}

// coChangeEdge builds one directed EdgeCoChange edge. Meta is cloned
// per edge so the two symmetric copies don't share a map.
func coChangeEdge(from, to, filePath string, p Pair) *graph.Edge {
	return &graph.Edge{
		From:       from,
		To:         to,
		Kind:       graph.EdgeCoChange,
		FilePath:   filePath,
		Origin:     graph.OriginASTInferred,
		Confidence: p.Score,
		Meta: map[string]any{
			"count": p.Count,
			"score": p.Score,
		},
	}
}
