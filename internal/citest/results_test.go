package citest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCensusAndExactPatternsIncludeUnicode(t *testing.T) {
	names, err := censusNames("chatter\nTestΓραφή\nTest世界\nTestA\nTestAB\nExample\nFuzzΣ\n")
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := regexp.Compile(exactPattern(names))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !pattern.MatchString(name) || pattern.MatchString("prefix"+name) || pattern.MatchString(name+"suffix") {
			t.Fatalf("incorrect exact pattern for %s: %s", name, pattern)
		}
	}
	for _, bad := range []string{"no tests\n", "TestA\nTestA\n", "TestA invalid\n", "TestA/sub\n"} {
		if _, err := censusNames(bad); err == nil {
			t.Errorf("bad census accepted: %q", bad)
		}
	}
}
func TestShardBatchesRunEachTestExactlyOnce(t *testing.T) {
	names := []string{"Example", "FuzzRoundTrip"}
	for _, family := range []string{"Chain", "Fold", "Parity"} {
		for i := 0; i < 40; i++ {
			names = append(names, fmt.Sprintf("Test%s%d", family, i))
		}
	}
	slices.Sort(names)
	for _, count := range []int{1, 2, 3} {
		var seen []string
		minSize, maxSize := len(names), 0
		for shard := 0; shard < count; shard++ {
			batches, err := planBatches(names, shard, count, 8)
			if err != nil {
				t.Fatal(err)
			}
			size := 0
			for _, batch := range batches {
				pattern := regexp.MustCompile(exactPattern(batch))
				var matched []string
				for _, name := range names {
					if pattern.MatchString(name) {
						matched = append(matched, name)
					}
				}
				want := append([]string{}, batch...)
				slices.Sort(want)
				if !slices.Equal(matched, want) {
					t.Fatalf("wrong batch pattern %v %v", matched, want)
				}
				seen = append(seen, batch...)
				size += len(batch)
			}
			minSize = min(minSize, size)
			maxSize = max(maxSize, size)
		}
		slices.Sort(seen)
		if !slices.Equal(seen, names) || maxSize-minSize > 1 {
			t.Fatal("unequal/missing/duplicate shard work")
		}
	}
	if _, err := planBatches([]string{"TestA"}, 1, 2, 8); err == nil {
		t.Fatal("empty shard accepted")
	}
}
func TestResultParsingKeepsTopLevelFailuresAndPanicTail(t *testing.T) {
	output := "=== RUN   TestA\n--- PASS: TestA (1.50s)\n=== RUN   TestB\n=== RUN   TestB/sub\n    --- FAIL: TestB/sub (0.00s)\n--- FAIL: TestB (0.20s)\npanic: boom\ngoroutine 1\n=== RUN   TestC\n--- SKIP: TestC (0.00s)\n"
	r := parseResults(output)
	if len(r) != 3 || r["TestA"].seconds != 1.5 || r["TestB"].status != "FAIL" || r["TestC"].status != "SKIP" {
		t.Fatal(r)
	}
	part := testOutput(output, "TestB")
	if !strings.HasSuffix(part, "goroutine 1") || strings.Contains(part, "TestC") {
		t.Fatal(part)
	}
	if parseResults("--- FAIL: TestA (0s)\n--- PASS: TestA (0s)\n")["TestA"].status != "FAIL" {
		t.Fatal("contradictory failure hidden")
	}
	for _, text := range []string{raceReport, "panic: boom", "fatal error: concurrent map writes", "SIGSEGV: bad", "Exception 0xc0000005 at pc", timeoutReport, "signal arrived during cgo"} {
		if !hardFailure(text) {
			t.Errorf("hard failure missed: %s", text)
		}
	}
}
func TestJSONEventsRetainBuildAndTestFailureEvidence(t *testing.T) {
	out := `non-json build chatter
{"Action":"output","Package":"example.org/p","Test":"TestA","Output":"details\n"}
{"Action":"fail","Package":"example.org/p","Test":"TestA","Elapsed":0.2}
{"Action":"pass","Package":"example.org/p"}
{"Action":"build-output","ImportPath":"example.org/broken","Output":"compile error\n"}
`
	e := parseEvents(out)
	if e.tests[testKey{"example.org/p", "TestA"}].status != "fail" || e.packages["example.org/p"].status != "pass" || e.output[testKey{"example.org/p", "TestA"}] != "details\n" || e.packageOutput["example.org/broken"] != "compile error\n" {
		t.Fatal(e)
	}
}
func TestCoverageSumsAttemptsAndPreservesZeroBlocks(t *testing.T) {
	root := t.TempDir()
	a, b, dest := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "merged")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "mode: atomic\nf.go:1.1,2.2 1 3\nf.go:3.1,4.2 1 0\n")
	write(b, "mode: atomic\nf.go:1.1,2.2 1 2\n")
	ok, err := mergeCoverage([]string{a, b, filepath.Join(root, "missing")}, dest)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mode: atomic\nf.go:1.1,2.2 1 5\nf.go:3.1,4.2 1 0\n" {
		t.Fatal(string(data))
	}
	write(b, "mode: set\nf.go:1.1,2.2 1 2\n")
	if _, err := mergeCoverage([]string{a, b}, dest); err == nil {
		t.Fatal("mixed modes accepted")
	}
	absent := filepath.Join(root, "absent")
	if ok, err := mergeCoverage(nil, absent); ok || err != nil {
		t.Fatal("fabricated coverage")
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("coverage created without any profiles")
	}
}
