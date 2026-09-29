package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// A save that shifts every later line of the file keeps the file's unchanged
// references bound without re-resolving them, and the file's rows equal a
// whole index of the edited tree. The case the shape-keyed reuse
// (incremental_resolve.go) refuses is the one exercised: one declaration
// calling two different functions of the same name (q.Helper, r.Helper) is an
// ambiguous shape there, and the carry tells them apart by their place in the
// declaration. A rename of a called function in the same save is not
// carried: its callers re-resolve.
func TestPriorBindingsCarryUnchangedReferencesAcrossAReparse(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "prior-bindings")
	builderGit(t, root, "init", "--initial-branch=main")
	builderWriteTree(t, root, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.23\n",
		"p/a.go": "package p\n\nimport (\n\t\"example.test/m/q\"\n\t\"example.test/m/r\"\n)\n\nfunc First() int {\n\ta := q.Helper()\n\tb := r.Helper()\n\treturn a + b + Local()\n}\n\nfunc Local() int { return 1 }\n\nfunc Second() int {\n\treturn q.Helper() + Local() + Gone()\n}\n\nfunc Gone() int { return 2 }\n",
		"q/q.go": "package q\n\nfunc Helper() int { return 3 }\n",
		"r/r.go": "package r\n\nfunc Helper() int { return 4 }\n",
	})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "fixture")
	index := func(logger *zap.Logger) (*graph.Graph, *Indexer) {
		g := graph.New()
		cfg := config.Default().Index
		cfg.Workers = 1
		idx := New(g, builderRegistry(), cfg, zap.NewNop())
		idx.SetRepoPrefix(builderRepoPrefix)
		if logger != nil {
			idx.resolver.SetLogger(logger)
		}
		_, err := idx.Index(root)
		require.NoError(t, err)
		return g, idx
	}
	rows := func(g *graph.Graph) []string {
		var out []string
		for _, e := range g.AllEdges() {
			if e.FilePath == builderRepoPrefix+"/p/a.go" {
				out = append(out, fmt.Sprintf("%s -%s-> %s @%d o=%s c=%.2f", e.From, e.Kind, e.To, e.Line, e.Origin, e.Confidence))
			}
		}
		sort.Strings(out)
		return out
	}
	core, logs := observer.New(zap.InfoLevel)
	g, idx := index(zap.New(core))
	logs.TakeAll()

	// A declaration inserted above First shifts every later line; Gone is
	// renamed, so Second's call to it must not carry.
	aPath := filepath.Join(root, "p", "a.go")
	bumpMtime(t, aPath, "package p\n\nimport (\n\t\"example.test/m/q\"\n\t\"example.test/m/r\"\n)\n\nfunc Zero() int { return 0 }\n\nfunc First() int {\n\ta := q.Helper()\n\tb := r.Helper()\n\treturn a + b + Local()\n}\n\nfunc Local() int { return 1 }\n\nfunc Second() int {\n\treturn q.Helper() + Local() + Gone2()\n}\n\nfunc Gone2() int { return 2 }\n")
	_, err := idx.IncrementalReindexPaths(root, []string{aPath})
	require.NoError(t, err)

	carried := 0
	for _, entry := range logs.FilterMessage("resolver: incremental phase complete").All() {
		if entry.ContextMap()["phase"] == "carry_prior_bindings" {
			carried += int(entry.ContextMap()["carried"].(int64))
		}
	}
	require.Positive(t, carried, "no reference carried its prior binding across the reparse")
	fresh, _ := index(nil)
	require.Equal(t, rows(fresh), rows(g), "the carried rows must equal a whole index of the edited tree")
}
