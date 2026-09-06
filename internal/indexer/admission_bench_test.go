package indexer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The exclude-before-language reorder is a trade, not a flat cost, and which
// way it lands depends entirely on the file population:
//
//   - unclaimed extension, not excluded — both orders open the file to sniff
//     it (effectiveLanguage falls through to readSniffPrefix), and the new
//     order runs the exclude and hierarchical-gitignore matchers plus a
//     SymlinkEscapes lstat on top. This is the population the reorder costs.
//   - unclaimed extension inside an excluded tree — the old order sniffed
//     first and only then excluded; the new one excludes without touching the
//     file at all.
//   - claimed extension — the registry answers from the path, nothing is
//     opened either way, and only the order of two cheap checks changes.
//
// A repository that is 94% source has almost none of the first two, which is
// why this builds its own tree rather than measuring gortex.
//
// To compare orderings, build the baseline by flipping admitWalkEntry back to
// language-first in a worktree at HEAD, rather than by checking out the base
// revision — newAdmissionTestIndexer and these two bench files do not exist
// there, so the base side would not compile:
//
//	git worktree add -f /tmp/gx-langfirst HEAD
//	cp internal/indexer/{admission_bench_test.go,bench_nonsource_test.go} /tmp/gx-langfirst/internal/indexer/
//	# then move the shouldExclude block in /tmp/gx-langfirst back below effectiveLanguage
//	go test -run '^$' -bench AdmitWalkEntry -benchmem -count=10 ./internal/indexer/ > new.txt
//	(cd /tmp/gx-langfirst && go test -run '^$' -bench AdmitWalkEntry -benchmem -count=10 ./internal/indexer/) > old.txt
//	benchstat old.txt new.txt
//
// BenchmarkIndex_NonSourceTree in bench_nonsource_test.go is the end-to-end
// half; its GORTEX_BENCH_TREE_* variables set the file mix.
func BenchmarkAdmitWalkEntry(b *testing.B) {
	for _, pop := range benchAdmitPopulations {
		b.Run(pop.name, func(b *testing.B) {
			root := b.TempDir()
			paths := writeBenchPopulation(b, root, pop.dir, pop.names)
			idx := newAdmissionTestIndexer(b, root)

			// One untimed call warms the exclude matcher and checks the
			// population is the one the sub-benchmark is named for: without it
			// a new extractor claiming .sketch would silently turn the costly
			// case into the cheap one and the numbers would read as a win.
			//
			// Only `admit` is asserted, because the rejection FIELDS differ by
			// ordering — language-first leaves `excluded` unset for a vendored
			// file it disowns before reaching the rule — and this file has to
			// compile and pass on both sides of the comparison.
			if adm := idx.admitWalkEntry(root, paths[0], benchFileSize, false); adm.admit != pop.wantAdmit {
				b.Fatalf("population %q admitted=%v, want %v (%+v)",
					pop.name, adm.admit, pop.wantAdmit, adm)
			}

			b.ReportAllocs()
			b.ResetTimer()
			i := 0
			for b.Loop() {
				idx.admitWalkEntry(root, paths[i%len(paths)], benchFileSize, false)
				i++
			}
		})
	}
}

// benchAdmitFiles is large enough that the OS path cache is not answering
// every call from one hot entry, small enough to lay down quickly.
const benchAdmitFiles = 512

// benchFileSize is the size handed to the gate. It stays under any configured
// cap so the size branch never short-circuits the measurement.
const benchFileSize = 4096

// benchUnclaimedNames carry extensions no extractor claims, so
// effectiveLanguage disowns them only after the sniff read.
var benchUnclaimedNames = []string{
	"logo-%d.sketch", "blob-%d.bin", "shard-%d.dat", "pack-%d.pak",
	"NOTICE-%d", "index-%d.idx",
}

// benchClaimedNames carry extensions the registry claims from the path alone.
var benchClaimedNames = []string{
	"mod-%d.go", "app-%d.ts", "util-%d.py", "lib-%d.rs",
}

var benchAdmitPopulations = []struct {
	name      string
	dir       string
	names     []string
	wantAdmit bool
}{
	{name: "unclaimed_included", dir: "assets", names: benchUnclaimedNames},
	{name: "unclaimed_vendored", dir: "node_modules/dpack/assets", names: benchUnclaimedNames},
	{name: "claimed_included", dir: "internal/app", names: benchClaimedNames, wantAdmit: true},
	{name: "claimed_vendored", dir: "node_modules/dpack/lib", names: benchClaimedNames},
}

// benchFileBody is binary filler: long enough to fill a sniff read, and
// shaped so no content probe can claim it as a language.
var benchFileBody = bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, benchFileSize/4)

// writeBenchPopulation lays benchAdmitFiles files down under root/dir, cycling
// through names, and returns their absolute paths.
func writeBenchPopulation(tb testing.TB, root, dir string, names []string) []string {
	tb.Helper()
	abs := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		tb.Fatal(err)
	}
	paths := make([]string, 0, benchAdmitFiles)
	for i := range benchAdmitFiles {
		p := filepath.Join(abs, fmt.Sprintf(names[i%len(names)], i))
		if err := os.WriteFile(p, benchFileBody, 0o644); err != nil {
			tb.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}
