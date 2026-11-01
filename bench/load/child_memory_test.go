package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixtureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The daemon's language servers and helpers are its descendants, forked from
// any of its threads, and may have children of their own. Their proportional
// sets add up once each; the daemon's own set does not count, nor does an
// unrelated process, and a child that exits before its set is read adds
// nothing.
func TestDescendantPSSWalksEveryThreadsChildren(t *testing.T) {
	proc := t.TempDir()
	writeFixtureFiles(t, proc, map[string]string{
		"100/smaps_rollup":      "Rss:  9000 kB\nPss:  8000 kB\n",
		"100/task/100/children": "200 ",
		"100/task/101/children": "300 500\n",
		"200/smaps_rollup":      "Rss:  40 kB\nPss:  30 kB\nPss_Anon:  20 kB\nPss_File:  10 kB\n",
		"200/task/200/children": "400",
		"300/smaps_rollup":      "Pss:  20 kB\n",
		"300/task/300/children": "200\n",
		"400/smaps_rollup":      "Pss:  5 kB\n",
		"400/task/400/children": "",
		"900/smaps_rollup":      "Pss:  7000 kB\n",
		"900/task/900/children": "",
	})
	got := descendantPSS(proc, 100)
	if want := float64(30+20+5) * 1024; got.Value == nil || *got.Value != want || got.Unit != "bytes" {
		t.Fatalf("descendant PSS = %+v, want %g bytes", got, want)
	}
	if got := descendantPSS(proc, 400); got.Value == nil || *got.Value != 0 {
		t.Fatalf("a process without children has no descendant PSS: %+v", got)
	}
	if got := descendantPSS(t.TempDir(), 100); got.Value != nil {
		t.Fatalf("no children lists fabricated a figure: %+v", got)
	}
}

// memory.stat reports the cgroup's anonymous and file-backed charge in bytes;
// the file_* counters break file down and must not be mistaken for it.
func TestCgroupAnonFileReadsMemoryStat(t *testing.T) {
	root := t.TempDir()
	writeFixtureFiles(t, root, map[string]string{
		"memory.stat": "anon 1048576\nfile 2097152\nkernel 65536\nfile_mapped 4096\nfile_dirty 0\nanon_thp 0\n",
	})
	got := cgroupAnonFile(root)
	for key, want := range map[string]float64{"cgroup_anon_bytes": 1048576, "cgroup_file_bytes": 2097152} {
		if m := got[key]; m.Value == nil || *m.Value != want || m.Unit != "bytes" {
			t.Fatalf("%s = %+v, want %g", key, m, want)
		}
	}
	for key, m := range cgroupAnonFile(t.TempDir()) {
		if m.Value != nil {
			t.Fatalf("%s fabricated without memory.stat: %+v", key, m)
		}
	}
}
