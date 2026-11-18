package indexer

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var walkNegationPatterns = []string{"!node_modules/", "node_modules/**", "!node_modules/*/", "node_modules/*/*", "!*.proto"}

// BenchmarkWalkDiskNegation times one full walk of a repository with an
// excluded dependency tree, with and without reopened parents and a wildcard
// negation that matches protocol files inside it.
func BenchmarkWalkDiskNegation(b *testing.B) {
	root := b.TempDir()
	for i := range 100 {
		writeBenchFile(b, filepath.Join(root, "src", fmt.Sprintf("f%03d.go", i)), []byte("package src\n"))
	}
	for p := range 50 {
		writeBenchFile(b, filepath.Join(root, "node_modules", fmt.Sprintf("pkg%02d", p), "schema.proto"), []byte("syntax = \"proto3\";\n"))
		for f := range 40 {
			writeBenchFile(b, filepath.Join(root, "node_modules", fmt.Sprintf("pkg%02d", p), "lib", fmt.Sprintf("m%02d.js", f)), []byte("module.exports = {}\n"))
		}
	}
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"without_negation", nil},
		{"with_wildcard_negation", []string{"!*.proto"}},
		{"with_reopened_parents", walkNegationPatterns},
	} {
		b.Run(tc.name, func(b *testing.B) {
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			cfg := config.Default().Index
			cfg.Exclude = append(append([]string{}, excludes.Builtin...), tc.extra...)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			var entries int
			b.ResetTimer()
			for b.Loop() {
				entries = 0
				err := idx.walkDisk(root, root, func(string, fs.DirEntry, error) error {
					entries++
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(entries), "entries/op")
		})
	}
}

func TestWalkNegationBenchmarkFixtures(t *testing.T) {
	root := t.TempDir()
	writeBenchFile(t, filepath.Join(root, "src", "a.go"), []byte("package src\n"))
	writeBenchFile(t, filepath.Join(root, "node_modules", "pkg", "schema.proto"), []byte("syntax = \"proto3\";\n"))
	writeBenchFile(t, filepath.Join(root, "node_modules", "pkg", "lib", "a.js"), []byte("module.exports = {}\n"))
	for _, tc := range []struct {
		extra   []string
		entries int
	}{
		{nil, 3}, {[]string{"!*.proto"}, 3}, {walkNegationPatterns, 6},
	} {
		reg := parser.NewRegistry()
		cfg := config.Default().Index
		cfg.Exclude = append(append([]string{}, excludes.Builtin...), tc.extra...)
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		entries := 0
		if err := idx.walkDisk(root, root, func(string, fs.DirEntry, error) error { entries++; return nil }); err != nil {
			t.Fatal(err)
		}
		if entries != tc.entries {
			t.Errorf("%q: visited %d entries, want %d", tc.extra, entries, tc.entries)
		}
	}
}
