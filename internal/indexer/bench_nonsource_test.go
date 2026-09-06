package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// BenchmarkIndex_NonSourceTree cold-indexes a generated repository whose files
// are mostly NOT source: an asset tree the extractors disown and a vendored
// tree the exclude rules drop, with a minority of real source beside them.
//
// BenchmarkIndex_Self cannot stand in for it. Gortex is 94% Go (4157 of 4446
// tracked files), so the walk there is dominated by files the registry claims
// from their extension — precisely the population on which the walk-entry
// gate's ordering does not matter.
//
// The mix is tunable so a reviewer can push it toward whatever shape their own
// repositories have:
//
//	GORTEX_BENCH_TREE_FILES   total files      (default 4000)
//	GORTEX_BENCH_TREE_SOURCE  percent source   (default 5)
//	GORTEX_BENCH_TREE_VENDOR  percent vendored (default 35)
func BenchmarkIndex_NonSourceTree(b *testing.B) {
	files := benchEnvInt(b, "GORTEX_BENCH_TREE_FILES", 4000)
	sourcePct := benchEnvInt(b, "GORTEX_BENCH_TREE_SOURCE", 5)
	vendorPct := benchEnvInt(b, "GORTEX_BENCH_TREE_VENDOR", 35)

	root := b.TempDir()
	writeNonSourceTree(b, root, files, sourcePct, vendorPct)
	benchmarkColdIndex(b, root)
}

// BenchmarkIndex_Repo cold-indexes the repository named by GORTEX_BENCH_REPO,
// for corroborating the synthetic numbers against a real working copy. It
// skips when unset, because there is no repository every machine has that is
// also a fair sample.
func BenchmarkIndex_Repo(b *testing.B) {
	root := os.Getenv("GORTEX_BENCH_REPO")
	if root == "" {
		b.Skip("set GORTEX_BENCH_REPO to a repository with a large non-source population")
	}
	benchmarkColdIndex(b, root)
}

// benchmarkColdIndex times repeated cold indexes of root, building a fresh
// graph and indexer per pass so no pass sees the previous one's mtime ledger.
func benchmarkColdIndex(b *testing.B, root string) {
	b.Helper()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	// Mirror production's LAYERED exclude list: config.Default().Index.Exclude
	// is empty on purpose, and a benchmark that left it empty would measure a
	// walk with no vendored tree to skip.
	cfg := config.Default().Index
	cfg.Exclude = append([]string{}, excludes.Builtin...)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		if _, err := idx.Index(root); err != nil {
			b.Fatal(err)
		}
	}
}

// writeNonSourceTree lays down `files` entries split between real source, an
// asset tree nothing claims, and a vendored tree the exclude rules drop.
func writeNonSourceTree(tb testing.TB, root string, files, sourcePct, vendorPct int) {
	tb.Helper()
	source := files * sourcePct / 100
	vendored := files * vendorPct / 100
	assets := files - source - vendored
	if assets < 0 {
		tb.Fatalf("source%%+vendor%% exceed 100: %d + %d", sourcePct, vendorPct)
	}

	// Real source, so the walk has something to actually parse.
	for i := range source {
		writeBenchFile(tb, filepath.Join(root, "internal", "app",
			"mod"+strconv.Itoa(i)+".go"), []byte("package app\n"))
	}
	// Assets: unclaimed extensions outside any exclude rule. Both orderings
	// sniff these; only the new one also runs the exclude matchers.
	for i := range assets {
		name := benchUnclaimedNames[i%len(benchUnclaimedNames)]
		writeBenchFile(tb, filepath.Join(root, "assets",
			fmt.Sprintf(name, i)), benchFileBody)
	}
	// A vendored tree of both kinds. The new order refuses these before
	// effectiveLanguage can open them.
	for i := range vendored {
		names := benchUnclaimedNames
		if i%3 == 0 {
			names = benchClaimedNames
		}
		writeBenchFile(tb, filepath.Join(root, "node_modules", "dpack", "lib",
			fmt.Sprintf(names[i%len(names)], i)), benchFileBody)
	}
}

func writeBenchFile(tb testing.TB, path string, body []byte) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		tb.Fatal(err)
	}
}

func benchEnvInt(tb testing.TB, key string, fallback int) int {
	tb.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("%s=%q is not a positive integer", key, raw)
	}
	return n
}
