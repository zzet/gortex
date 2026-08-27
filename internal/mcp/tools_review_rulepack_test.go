package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// reviewRulepackFixture is a Go source the `go-unchecked-type-assertion`
// review detector fires on. It is deliberately not an N+1 / check-then-act
// shape so the graph-grounding post-pass keeps the match unconditionally.
const reviewRulepackFixture = `package pkg

func Widen(v any) string {
	s := v.(string)
	return s
}
`

// prefixedServerOver indexes an existing directory through the MultiIndexer
// under the given repo name, so every graph node carries a repo prefix — the
// daemon's shape, where file nodes are keyed "<prefix>/<rel>" while git emits
// repo-relative paths. Returns the server and the graph repo prefix.
func prefixedServerOver(t *testing.T, dir, name string) (*Server, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: name}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	srv.RunAnalysis()

	// Precondition: the graph really is prefixed, otherwise these tests would
	// pass for the wrong reason — the pre-fix join worked in unprefixed mode,
	// which is exactly why the bug reached a release.
	var prefix string
	for n := range g.NodesByKind(graph.KindFile) {
		require.NotEmpty(t, n.RepoPrefix, "file node %q must carry a repo prefix", n.ID)
		prefix = n.RepoPrefix
	}
	require.NotEmpty(t, prefix, "fixture repo produced no indexed file nodes")
	return srv, prefix
}

// setupPrefixedReviewServer writes the given repo-relative sources into a fresh
// directory and indexes it prefixed.
func setupPrefixedReviewServer(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo-a")
	for rel, src := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}
	return prefixedServerOver(t, dir, "repo-a")
}

// TestReviewRulepackMatches_JoinsRepoRelativeChangedFiles pins the join the
// review tool depends on: `git diff` names changed files repo-relative while
// the graph keys them "<prefix>/<rel>", so intersecting the two vocabularies
// raw matched nothing and `review` reported zero findings on code its own
// detector bundle flags.
func TestReviewRulepackMatches_JoinsRepoRelativeChangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
	})

	matches := srv.reviewRulepackMatches(context.Background(), []string{"pkg/widget.go"}, analysis.RepoRelativePath, prefix, nil)
	require.NotEmpty(t, matches,
		"repo-relative changed file must join the prefixed graph path %q/pkg/widget.go", prefix)

	// Findings travel onward to rule resolution, the risk ranking, and the
	// forge comment API — all of which speak repo-relative paths.
	for _, m := range matches {
		require.NotEmpty(t, m.SymbolID,
			"review grounding must receive post-match enclosing-symbol enrichment")
		require.Equal(t, "pkg/widget.go", m.File,
			"match paths must be repo-relative, not graph-prefixed")
	}
}

// TestReviewRulepackMatches_IgnoresUnchangedFiles keeps the narrowing honest:
// the rulepack must scan the changeset, not the whole repository.
func TestReviewRulepackMatches_IgnoresUnchangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
		"pkg/other.go":  "package pkg\n\nfunc Other() {}\n",
	})

	matches := srv.reviewRulepackMatches(context.Background(), []string{"pkg/other.go"}, analysis.RepoRelativePath, prefix, nil)
	require.Empty(t, matches, "a file outside the changeset must not be scanned")
}

// TestReviewRulepackMatches_PrefixShadowedPathScansOnlyTheChangedTarget pins
// the case that makes inferring the path domain unsafe. The repo's own tree
// carries a top-level directory named like the repo prefix, so the changed
// git-relative path `repo-a/pkg/widget.go` is *also* a well-formed graph key
// for the different, unchanged file `pkg/widget.go`.
//
// Guessing "this already looks prefixed" skips the real key
// `repo-a/repo-a/pkg/widget.go` — the changed file is never scanned, and the
// unchanged shadow is scanned in its place. Both files carry the detector
// fixture, so a wrong-target scan still returns matches and only the reported
// path distinguishes the two outcomes.
func TestReviewRulepackMatches_PrefixShadowedPathScansOnlyTheChangedTarget(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go":        reviewRulepackFixture,
		"repo-a/pkg/widget.go": reviewRulepackFixture,
	})
	require.Equal(t, "repo-a", prefix, "the fixture's shadow directory must equal the repo prefix")

	matches := srv.reviewRulepackMatches(context.Background(),
		[]string{"repo-a/pkg/widget.go"}, analysis.RepoRelativePath, prefix, nil)

	require.NotEmpty(t, matches, "the changed nested file must be scanned")
	for _, m := range matches {
		require.Equal(t, "repo-a/pkg/widget.go", m.File,
			"only the changed target may be scanned; %q is the unchanged shadow", m.File)
	}
}

// TestReviewRulepackMatches_AcceptsAlreadyPrefixedChangedFiles covers the
// callers that hand in graph-keyed paths (a changed symbol's FilePath) rather
// than git's repo-relative spelling.
func TestReviewRulepackMatches_AcceptsAlreadyPrefixedChangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
	})

	matches := srv.reviewRulepackMatches(context.Background(),
		[]string{prefix + "/pkg/widget.go"}, analysis.GraphKeyedPath, prefix, nil)
	require.NotEmpty(t, matches, "an already-prefixed changed file must still join")
	require.Equal(t, "pkg/widget.go", matches[0].File)
}

// TestReview_PrefixedGraphReportsRulepackFinding is the end-to-end regression
// for the reported failure: against a prefixed graph — the only shape a daemon
// produces — `review` returned `total: 0` on a changeset whose code the review
// detector bundle flags under `analyze --kind review`. The BLOCK verdict it
// paired that with carried nothing to act on.
func TestReview_PrefixedGraphReportsRulepackFinding(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv, _ := prefixedServerOver(t, dir, "svc-repo")

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"repo": dir,
		"base": "base-ref",
	}))

	require.GreaterOrEqual(t, out.Total, 1,
		"the planted inverted-err-check must surface; got %+v", out)
	require.Equal(t, "BLOCK", out.Verdict, "an error-severity finding must block")

	var found bool
	for _, c := range out.Comments {
		if c.Rule != "go-inverted-err-check" {
			continue
		}
		found = true
		require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(c.File),
			"the comment path must be repo-relative — a forge rejects a prefixed path")
		require.Greater(t, c.Line, 0, "the finding must be anchored to a real line")
		require.Equal(t, "rulepack", c.Source)
	}
	require.True(t, found, "expected a go-inverted-err-check finding; got %+v", out.Comments)

	for _, fr := range out.FileRisk {
		require.NotContains(t, fr.File, "svc-repo/", "risk rows stay repo-relative")
	}
}

// reviewShadowGitRepo builds the prefix-shadow fixture end to end: a git repo
// whose own tree carries a top-level directory named like the repo prefix, so
// the changed git-relative path `repo-a/pkg/widget.go` is simultaneously a
// well-formed graph key for the different, unchanged `pkg/widget.go`.
//
// Both files carry the flagged source at the base commit and only the nested
// one is modified. A wrong-target join therefore still produces findings — only
// the path they are attributed to separates a correct run from a broken one.
func reviewShadowGitRepo(t *testing.T) (root, changed, shadow string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, src string) {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}

	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.noprefix", "false")

	// Flat paths on purpose. The raw-lookup defect only manifests when the
	// changed git path is byte-identical to the shadow's graph key, and a
	// nested spelling would differ by separator on Windows ("repo-a/pkg\widget.go"
	// stored vs "repo-a/pkg/widget.go" from git) — masking the defect on this
	// platform. One segment makes the two coincide everywhere.
	shadow = "widget.go"
	changed = "repo-a/widget.go"

	flagged := "package pkg\n\nimport \"errors\"\n\n" +
		"func Load() error {\n" +
		"\terr := errors.New(\"boom\")\n" +
		"\tif err == nil {\n" +
		"\t\treturn err\n" +
		"\t}\n" +
		"\treturn nil\n" +
		"}\n"

	write(shadow, flagged)
	write(changed, flagged)
	run("add", ".")
	run("commit", "-m", "base")
	run("tag", "base-ref")

	// Only the nested file moves, and the edit lands INSIDE Load so the
	// hunk overlaps the function's line range. A top-of-file edit would
	// change the file without touching any symbol, and ChangedSymbols would
	// come back empty for a reason unrelated to the join under test.
	write(changed, strings.Replace(flagged, `"boom"`, `"boom-changed"`, 1))
	run("add", ".")
	run("commit", "-m", "change")
	return dir, changed, shadow
}

// TestReview_PrefixShadowAttributesOnlyTheChangedFile is the end-to-end half of
// the prefix-shadow regression: it drives `review` through MapGitDiff,
// JoinFileNodes and rankFileRisk rather than calling the narrowing directly, so
// a domain collision anywhere along that path surfaces here.
func TestReview_PrefixShadowAttributesOnlyTheChangedFile(t *testing.T) {
	dir, changed, shadow := reviewShadowGitRepo(t)
	srv, prefix := prefixedServerOver(t, dir, "repo-a")
	require.Equal(t, "repo-a", prefix)

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"repo": dir,
		"base": "base-ref",
	}))

	require.GreaterOrEqual(t, out.Total, 1, "the changed file's planted finding must surface: %+v", out)
	require.Equal(t, "BLOCK", out.Verdict)

	for _, c := range out.Comments {
		require.Equal(t, changed, filepath.ToSlash(c.File),
			"a finding was attributed to %q; only %q changed", c.File, changed)
	}
	// Exactly one row: NotEqual(shadow) would still pass if the run
	// produced BOTH a graph-prefixed row and a repo-relative one for the
	// same file, which is what a domain collision in rankFileRisk does.
	require.Len(t, out.FileRisk, 1,
		"exactly one risk row for the one changed file: %+v", out.FileRisk)
	require.Equal(t, changed, filepath.ToSlash(out.FileRisk[0].File),
		"the risk row must name the changed file, not the unchanged shadow %q", shadow)
}

// TestReviewPack_PrefixShadowAttributesOnlyTheChangedFile covers the packaged
// envelope: changed symbols, per-file risk and findings must all name the
// changed nested file, never the unchanged same-named shadow.
func TestReviewPack_PrefixShadowAttributesOnlyTheChangedFile(t *testing.T) {
	dir, changed, shadow := reviewShadowGitRepo(t)
	srv, prefix := prefixedServerOver(t, dir, "repo-a")
	require.Equal(t, "repo-a", prefix)

	out := decodeReviewPack(t, callReviewPack(t, srv, map[string]any{
		"repo": dir,
		"base": "base-ref",
	}))

	require.GreaterOrEqual(t, out.Total, 1, "the changed file's planted finding must surface: %+v", out)

	// ChangedSymbols are graph-keyed: the real key nests the prefix twice.
	require.NotEmpty(t, out.ChangedSymbols, "the changed nested file must contribute symbols")
	// The key is the graph's own spelling, not a '/'-joined guess: the
	// remainder after the prefix carries native separators.
	wantPrefix := analysis.GraphKey(prefix, changed, analysis.RepoRelativePath) + "::"
	for _, cs := range out.ChangedSymbols {
		require.Truef(t, strings.HasPrefix(cs.ID, wantPrefix),
			"changed symbol %q is not under the changed file %q", cs.ID, wantPrefix)
	}
	for _, f := range out.Findings {
		require.Equal(t, changed, filepath.ToSlash(f.File),
			"a finding was attributed to %q; only %q changed", f.File, changed)
	}
	// Exactly one row: NotEqual(shadow) would still pass if the run
	// produced BOTH a graph-prefixed row and a repo-relative one for the
	// same file, which is what a domain collision in rankFileRisk does.
	require.Len(t, out.FileRisk, 1,
		"exactly one risk row for the one changed file: %+v", out.FileRisk)
	require.Equal(t, changed, filepath.ToSlash(out.FileRisk[0].File),
		"the risk row must name the changed file, not the unchanged shadow %q", shadow)
}
