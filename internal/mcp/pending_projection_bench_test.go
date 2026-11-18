package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// repoGoSources lists this repository's Go files under the projection size
// limit, largest first.
func repoGoSources(b *testing.B) (root string, files []string, lines map[string]int) {
	b.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		b.Fatal(err)
	}
	sizes := map[string]int64{}
	lines = map[string]int{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || len(src) > 1<<20 {
			return nil
		}
		sizes[path] = int64(len(src))
		lines[path] = bytes.Count(src, []byte("\n"))
		files = append(files, path)
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	sort.Slice(files, func(i, j int) bool { return sizes[files[i]] > sizes[files[j]] })
	return root, files, lines
}

func reportP95(b *testing.B, samples []time.Duration, unit string, scale time.Duration) {
	b.Helper()
	slices.Sort(samples)
	b.ReportMetric(float64(samples[len(samples)*95/100])/float64(scale), "p95-"+unit)
}

// BenchmarkProjectionPublish publishes into a registry already at its file
// cap, the worst case for the copy-on-write swap.
func BenchmarkProjectionPublish(b *testing.B) {
	dir := b.TempDir()
	p := newPendingProjections()
	paths := make([]string, projectionMaxFiles)
	for i := range projectionMaxFiles {
		name := fmt.Sprintf("f%03d.go", i)
		paths[i] = filepath.Join(dir, name)
		if err := p.publish(syntheticProjectionEntry(paths[i], name, "h", 1, diskStamp{}, nil)); err != nil {
			b.Fatal(err)
		}
	}
	entries := make([]*projectionEntry, b.N)
	for i := range entries {
		path := paths[i%len(paths)]
		entries[i] = syntheticProjectionEntry(path, filepath.Base(path), "h", uint64(i+2), diskStamp{}, nil)
	}
	samples := make([]time.Duration, b.N)
	b.ResetTimer()
	for i, e := range entries {
		started := time.Now()
		if err := p.publish(e); err != nil {
			b.Fatal(err)
		}
		samples[i] = time.Since(started)
	}
	b.StopTimer()
	reportP95(b, samples, "µs", time.Microsecond)
}

// BenchmarkProjectionBuild projects the largest file of at most 3k lines and
// the largest file in this repository. A file the projection refuses, such as
// one whose build runs past the deadline, is reported as skipped.
func BenchmarkProjectionBuild(b *testing.B) {
	root, files, lines := repoGoSources(b)
	idx := newProjectionTestIndexer(b, graph.New())
	idx.SetRootPath(root)
	largest := files[0]
	var upTo3k string
	for _, f := range files {
		if lines[f] <= 3000 {
			upTo3k = f
			break
		}
	}
	for _, tc := range []struct{ name, path string }{{"3k-lines", upTo3k}, {"largest", largest}} {
		src, err := os.ReadFile(tc.path)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(tc.name, func(b *testing.B) {
			samples := make([]time.Duration, b.N)
			for i := range b.N {
				started := time.Now()
				if _, err := projectFile(context.Background(), idx, tc.path, src, uint64(i+1), diskStamp{}); err != nil {
					b.Skipf("%s (%d lines): %v", tc.path, lines[tc.path], err)
				}
				samples[i] = time.Since(started)
			}
			b.ReportMetric(float64(lines[tc.path]), "lines")
			reportP95(b, samples, "ms", time.Millisecond)
		})
	}
}

// BenchmarkProjectionRetainedHeap fills the registry with projections of this
// repository's Go files until it holds its file cap or refuses a publish, and
// reports the heap the registry alone retains next to its own size estimate.
// Files the projection refuses are left out. Largest-first is the worst case
// for the byte cap; path order is a mix of file sizes.
func BenchmarkProjectionRetainedHeap(b *testing.B) {
	root, largestFirst, _ := repoGoSources(b)
	idx := newProjectionTestIndexer(b, graph.New())
	idx.SetRootPath(root)
	pathOrder := slices.Clone(largestFirst)
	slices.Sort(pathOrder)
	for _, tc := range []struct {
		name  string
		files []string
	}{{"largest-first", largestFirst}, {"path-order", pathOrder}} {
		b.Run(tc.name, func(b *testing.B) {
			for range b.N {
				var live, released runtime.MemStats
				entries, estimated, notProjected := fillProjections(b, idx, tc.files, &live)
				runtime.GC()
				runtime.ReadMemStats(&released)
				b.ReportMetric(float64(entries), "entries")
				b.ReportMetric(float64(notProjected), "not-projected")
				b.ReportMetric(float64(int64(live.HeapAlloc)-int64(released.HeapAlloc))/(1<<20), "retained-MiB")
				b.ReportMetric(float64(estimated)/(1<<20), "estimated-MiB")
			}
		})
	}
}

// fillProjections publishes projections of files into a new registry until it
// is full, and reads the heap into live while the registry is still reachable.
func fillProjections(b *testing.B, idx *indexer.Indexer, files []string, live *runtime.MemStats) (entries int, estimated int64, notProjected int) {
	b.Helper()
	p := newPendingProjections()
	for _, path := range files {
		if len(p.snap.Load().entries) == projectionMaxFiles {
			break
		}
		src, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		e, err := projectFile(context.Background(), idx, path, src, 1, diskStamp{})
		if err != nil {
			notProjected++
			continue
		}
		if err := p.publish(e); err != nil {
			break
		}
	}
	runtime.GC()
	runtime.ReadMemStats(live)
	snap := p.snap.Load()
	return len(snap.entries), snap.bytes, notProjected
}
